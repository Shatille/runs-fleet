package housekeeping

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type fakeFleetCostStore struct {
	deltas    []db.FleetCostDelta
	days      []string
	lastDay   db.FleetCostDay
	lastErr   error
	busyIDs   []string
	busyErr   error
	addErr    error
	lastCalls int
}

func (f *fakeFleetCostStore) AddFleetCostSample(_ context.Context, day string, d db.FleetCostDelta) error {
	f.days = append(f.days, day)
	f.deltas = append(f.deltas, d)
	return f.addErr
}

func (f *fakeFleetCostStore) GetFleetCostDays(_ context.Context, _, _ string) ([]db.FleetCostDay, error) {
	f.lastCalls++
	if f.lastErr != nil {
		return nil, f.lastErr
	}
	if f.lastDay.Day == "" {
		return nil, nil
	}
	return []db.FleetCostDay{f.lastDay}, nil
}

func (f *fakeFleetCostStore) ListBusyInstanceIDs(_ context.Context) ([]string, error) {
	return f.busyIDs, f.busyErr
}

func instance(id, instType, pool, state string, spot bool) ec2types.Instance {
	inst := ec2types.Instance{
		InstanceId:   aws.String(id),
		InstanceType: ec2types.InstanceType(instType),
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateName(state)},
		Tags: []ec2types.Tag{
			{Key: aws.String("runs-fleet:managed"), Value: aws.String("true")},
		},
	}
	if pool != "" {
		inst.Tags = append(inst.Tags, ec2types.Tag{
			Key: aws.String("runs-fleet:pool"), Value: aws.String(pool),
		})
	}
	if spot {
		inst.InstanceLifecycle = ec2types.InstanceLifecycleTypeSpot
	}
	return inst
}

func fleetCostTasks(t *testing.T, store *fakeFleetCostStore, instances ...ec2types.Instance) *Tasks {
	t.Helper()
	tasks := &Tasks{
		ec2Client: &mockEC2API{instances: []ec2types.Reservation{{Instances: instances}}},
		config:    &config.Config{JobsTableName: "jobs-table"},
	}
	tasks.SetFleetCostStore(store)
	return tasks
}

// The sampler must see the whole managed fleet, not just pool members. Pool
// reconciliation filters on runs-fleet:pool and is structurally blind to
// cold-start instances, which is half the cost the Cost tab misses today.
func TestFleetCostSampleEnumeratesTheWholeManagedFleet(t *testing.T) {
	store := &fakeFleetCostStore{}
	tasks := fleetCostTasks(t, store,
		instance("i-pool", "c7g.xlarge", "default", "running", false),
		instance("i-cold", "c7g.xlarge", "", "running", true),
	)

	if err := tasks.ExecuteFleetCostSample(context.Background()); err != nil {
		t.Fatalf("ExecuteFleetCostSample() error = %v", err)
	}
	if len(store.deltas) != 1 {
		t.Fatalf("wrote %d deltas, want exactly 1 per tick", len(store.deltas))
	}
	if store.deltas[0].TotalCost <= 0 {
		t.Error("cost = 0, want both the pool and the cold-start instance priced")
	}
	// Both instances contribute instance-seconds; neither may be dropped.
	if store.deltas[0].InstanceSeconds <= 0 {
		t.Error("instance seconds = 0, want time recorded for both instances")
	}
}

// A stopped warm-pool instance still bills for its EBS volume. Today it reports
// zero, which is why keeping a warm pool looks free on the Cost tab.
func TestFleetCostSampleChargesStoppedInstancesForStorage(t *testing.T) {
	store := &fakeFleetCostStore{}
	tasks := fleetCostTasks(t, store,
		instance("i-stopped", "c7g.xlarge", "default", "stopped", false),
	)

	if err := tasks.ExecuteFleetCostSample(context.Background()); err != nil {
		t.Fatalf("ExecuteFleetCostSample() error = %v", err)
	}
	got := store.deltas[0]
	if got.EBSCost <= 0 {
		t.Error("EBS cost = 0, want a stopped instance to still cost storage")
	}
	if got.ComputeCost != 0 {
		t.Errorf("compute cost = %v, want 0 for a stopped instance", got.ComputeCost)
	}
}

// pending and stopping are billed by AWS as running; only stopped is not.
func TestFleetCostSampleTreatsPendingAndStoppingAsBillable(t *testing.T) {
	for _, state := range []string{"pending", "stopping", "running"} {
		t.Run(state, func(t *testing.T) {
			store := &fakeFleetCostStore{}
			tasks := fleetCostTasks(t, store,
				instance("i-1", "c7g.xlarge", "", state, false),
			)
			if err := tasks.ExecuteFleetCostSample(context.Background()); err != nil {
				t.Fatalf("ExecuteFleetCostSample() error = %v", err)
			}
			if store.deltas[0].ComputeCost <= 0 {
				t.Errorf("state %q: compute cost = 0, want it billed as running", state)
			}
		})
	}
}

// Coverage is the point of the whole feature: it must count time on instances
// that were actually running a job, so the remainder is visibly unattributed.
func TestFleetCostSampleAttributesOnlyBusyInstanceTime(t *testing.T) {
	store := &fakeFleetCostStore{busyIDs: []string{"i-busy"}}
	tasks := fleetCostTasks(t, store,
		instance("i-busy", "c7g.xlarge", "", "running", false),
		instance("i-idle", "c7g.xlarge", "default", "running", false),
	)

	if err := tasks.ExecuteFleetCostSample(context.Background()); err != nil {
		t.Fatalf("ExecuteFleetCostSample() error = %v", err)
	}
	got := store.deltas[0]
	if got.AttributedSeconds <= 0 {
		t.Fatal("attributed seconds = 0, want the busy instance counted")
	}
	if got.AttributedSeconds >= got.InstanceSeconds {
		t.Errorf("attributed %v of %v seconds, want the idle instance to remain unattributed",
			got.AttributedSeconds, got.InstanceSeconds)
	}
}

// The first tick has no previous checkpoint. It must not attribute a wild
// elapsed window, and must not crash.
func TestFleetCostSampleFirstTickUsesTheNominalInterval(t *testing.T) {
	store := &fakeFleetCostStore{}
	tasks := fleetCostTasks(t, store,
		instance("i-1", "c7g.xlarge", "", "running", false),
	)

	if err := tasks.ExecuteFleetCostSample(context.Background()); err != nil {
		t.Fatalf("ExecuteFleetCostSample() error = %v", err)
	}
	got := store.deltas[0]
	if got.InstanceSeconds != fleetCostSampleInterval.Seconds() {
		t.Errorf("instance seconds = %v, want the nominal interval %v on a first tick",
			got.InstanceSeconds, fleetCostSampleInterval.Seconds())
	}
	if got.Partial {
		t.Error("first tick marked partial, want it treated as a normal interval")
	}
}

// A missed tick must self-heal: the next sample covers the gap, so a stalled
// replica undercounts nothing. Fixed-interval attribution would silently lose
// the gap, which is the same class of dishonesty this feature exists to remove.
func TestFleetCostSampleCoversAGapLeftByAMissedTick(t *testing.T) {
	gap := 4 * time.Minute
	store := &fakeFleetCostStore{lastDay: db.FleetCostDay{
		Day:          time.Now().UTC().Format(db.FleetDayFormat),
		LastSampleAt: time.Now().UTC().Add(-gap),
	}}
	tasks := fleetCostTasks(t, store,
		instance("i-1", "c7g.xlarge", "", "running", false),
	)

	if err := tasks.ExecuteFleetCostSample(context.Background()); err != nil {
		t.Fatalf("ExecuteFleetCostSample() error = %v", err)
	}
	got := store.deltas[0].InstanceSeconds
	if got < gap.Seconds()*0.9 {
		t.Errorf("instance seconds = %v, want ~%v so the missed ticks are covered",
			got, gap.Seconds())
	}
	if store.deltas[0].Partial {
		t.Error("a gap within the cap must not mark the day partial")
	}
}

// A long outage must not attribute one enormous phantom block. The window is
// capped, and the day is flagged so the API can say it understates.
func TestFleetCostSampleClampsALongOutageAndMarksThePartialDay(t *testing.T) {
	store := &fakeFleetCostStore{lastDay: db.FleetCostDay{
		Day:          time.Now().UTC().Format(db.FleetDayFormat),
		LastSampleAt: time.Now().UTC().Add(-6 * time.Hour),
	}}
	tasks := fleetCostTasks(t, store,
		instance("i-1", "c7g.xlarge", "", "running", false),
	)

	if err := tasks.ExecuteFleetCostSample(context.Background()); err != nil {
		t.Fatalf("ExecuteFleetCostSample() error = %v", err)
	}
	got := store.deltas[0]
	if got.InstanceSeconds > fleetCostMaxElapsed.Seconds() {
		t.Errorf("instance seconds = %v, want it clamped to %v",
			got.InstanceSeconds, fleetCostMaxElapsed.Seconds())
	}
	if !got.Partial {
		t.Error("a clamped tick must mark the day partial so it is not read as complete")
	}
}

// A clock that jumps backwards must not produce negative cost.
func TestFleetCostSampleIgnoresACheckpointInTheFuture(t *testing.T) {
	store := &fakeFleetCostStore{lastDay: db.FleetCostDay{
		Day:          time.Now().UTC().Format(db.FleetDayFormat),
		LastSampleAt: time.Now().UTC().Add(time.Hour),
	}}
	tasks := fleetCostTasks(t, store,
		instance("i-1", "c7g.xlarge", "", "running", false),
	)

	if err := tasks.ExecuteFleetCostSample(context.Background()); err != nil {
		t.Fatalf("ExecuteFleetCostSample() error = %v", err)
	}
	if got := store.deltas[0].TotalCost; got < 0 {
		t.Errorf("total cost = %v, want it never negative", got)
	}
}

// With no store wired the task is inert — the same shape every optional
// housekeeping dependency uses.
func TestFleetCostSampleIsANoopWithoutAStore(t *testing.T) {
	ec2Mock := &mockEC2API{}
	tasks := &Tasks{ec2Client: ec2Mock, config: &config.Config{}}

	if err := tasks.ExecuteFleetCostSample(context.Background()); err != nil {
		t.Fatalf("ExecuteFleetCostSample() error = %v", err)
	}
	if ec2Mock.describeCalls != 0 {
		t.Errorf("made %d EC2 calls, want none without a store", ec2Mock.describeCalls)
	}
}

// Coverage is a nice-to-have; fleet cost is not. A busy-lookup failure must
// still record the spend rather than losing the whole tick.
func TestFleetCostSampleStillRecordsCostWhenTheBusyLookupFails(t *testing.T) {
	store := &fakeFleetCostStore{busyErr: errors.New("dynamo down")}
	tasks := fleetCostTasks(t, store,
		instance("i-1", "c7g.xlarge", "", "running", false),
	)

	if err := tasks.ExecuteFleetCostSample(context.Background()); err != nil {
		t.Fatalf("ExecuteFleetCostSample() error = %v", err)
	}
	if len(store.deltas) != 1 {
		t.Fatal("no delta written, want the cost recorded despite the coverage failure")
	}
	if store.deltas[0].AttributedSeconds != 0 {
		t.Error("attributed seconds must be 0 when the busy set is unknown")
	}
}

func TestFleetCostSampleReturnsDescribeErrors(t *testing.T) {
	store := &fakeFleetCostStore{}
	tasks := &Tasks{
		ec2Client: &mockEC2API{describeErr: errors.New("throttled")},
		config:    &config.Config{},
	}
	tasks.SetFleetCostStore(store)

	if err := tasks.ExecuteFleetCostSample(context.Background()); err == nil {
		t.Fatal("ExecuteFleetCostSample() error = nil, want the describe failure surfaced")
	}
}

// An empty fleet is a real observation (nothing running costs nothing), but it
// must still checkpoint so the next tick's elapsed window stays correct.
func TestFleetCostSampleCheckpointsAnEmptyFleet(t *testing.T) {
	store := &fakeFleetCostStore{}
	tasks := fleetCostTasks(t, store)

	if err := tasks.ExecuteFleetCostSample(context.Background()); err != nil {
		t.Fatalf("ExecuteFleetCostSample() error = %v", err)
	}
	if len(store.deltas) != 1 {
		t.Fatal("no delta written, want an empty fleet to still checkpoint")
	}
	if store.deltas[0].SampledAt.IsZero() {
		t.Error("checkpoint not set, want the next tick to measure from here")
	}
}
