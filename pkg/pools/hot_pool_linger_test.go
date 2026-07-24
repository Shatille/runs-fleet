package pools

import (
	"context"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const lingerPoolName = "portal-api"

func hotPoolConfig(pool string, linger, maxHot int) *config.Config {
	return &config.Config{
		SubnetIDs: []string{"subnet-1"},
		HotPools:  map[string]config.HotPoolSpec{pool: {LingerMinutes: linger, MaxHot: maxHot}},
	}
}

func TestLingerDesiredRunning(t *testing.T) {
	t.Parallel()

	now := time.Now()
	tests := []struct {
		name string
		cfg  *config.Config
		pool *db.PoolConfig
		want int
	}{
		{
			name: "nil HotPools returns 0 (gate off)",
			cfg:  &config.Config{},
			pool: &db.PoolConfig{PoolName: lingerPoolName, LastJobTime: now.Add(-1 * time.Minute)},
			want: 0,
		},
		{
			name: "pool not in allowlist returns 0",
			cfg:  hotPoolConfig("other-pool", 15, 2),
			pool: &db.PoolConfig{PoolName: lingerPoolName, LastJobTime: now.Add(-1 * time.Minute)},
			want: 0,
		},
		{
			name: "within linger window returns MaxHot",
			cfg:  hotPoolConfig(lingerPoolName, 15, 2),
			pool: &db.PoolConfig{PoolName: lingerPoolName, LastJobTime: now.Add(-5 * time.Minute)},
			want: 2,
		},
		{
			name: "past linger window returns 0",
			cfg:  hotPoolConfig(lingerPoolName, 15, 2),
			pool: &db.PoolConfig{PoolName: lingerPoolName, LastJobTime: now.Add(-16 * time.Minute)},
			want: 0,
		},
		{
			name: "exactly at linger boundary returns 0 (decayed)",
			cfg:  hotPoolConfig(lingerPoolName, 15, 2),
			pool: &db.PoolConfig{PoolName: lingerPoolName, LastJobTime: now.Add(-15 * time.Minute)},
			want: 0,
		},
		{
			name: "zero LastJobTime returns 0 (never active)",
			cfg:  hotPoolConfig(lingerPoolName, 15, 2),
			pool: &db.PoolConfig{PoolName: lingerPoolName},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := NewManager(&MockDBClient{}, &MockFleetAPI{}, tt.cfg)
			if got := m.lingerDesiredRunning(tt.pool, now); got != tt.want {
				t.Errorf("lingerDesiredRunning() = %d, want %d", got, tt.want)
			}
		})
	}
}

// When linger is active and the pool has a stopped spare, reconcile starts it
// with reason "linger" (attribution) — an ephemeral pool would otherwise sit at
// desired_running=0 and pay the stopped-boot on the next job.
func TestReconcileLingerStartsSpare(t *testing.T) {
	t.Parallel()

	var startedIDs []string

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) { return []string{lingerPoolName}, nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:     lingerPoolName,
				Ephemeral:    true,
				LastJobTime:  time.Now().Add(-2 * time.Minute), // within a 15-min linger
				InstanceType: "c7g.large",
			}, nil
		},
		GetPoolP90ConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) { return 0, nil },
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
				{
					InstanceId:   aws.String(testInstanceStoppedID),
					InstanceType: ec2types.InstanceTypeC7gLarge,
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			}}}}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			startedIDs = append(startedIDs, params.InstanceIds...)
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	recordingMetrics := &lingerMetrics{}
	m := NewManager(mockDB, &MockFleetAPI{}, hotPoolConfig(lingerPoolName, 15, 1))
	m.SetEC2Client(mockEC2)
	m.SetMetrics(recordingMetrics)

	m.reconcile(context.Background())

	if len(startedIDs) != 1 || startedIDs[0] != testInstanceStoppedID {
		t.Fatalf("started IDs = %v, want [%s] (linger floor should start the stopped spare)", startedIDs, testInstanceStoppedID)
	}
	if !contains(recordingMetrics.startReasons, poolActionReasonLinger) {
		t.Errorf("start reasons = %v, want to contain %q", recordingMetrics.startReasons, poolActionReasonLinger)
	}
}

// After the linger window elapses, the effective running target falls to 0 and
// the existing warm-pool immediate-stop branch banks the running spare — no new
// decay path. Guarded by the bootstrap grace + ready dwell, both disabled here so
// the single pass exercises the decay.
func TestReconcileLingerExpiryBanksSpare(t *testing.T) {
	t.Parallel()

	var stoppedIDs []string

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) { return []string{lingerPoolName}, nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:     lingerPoolName,
				Ephemeral:    true,
				LastJobTime:  time.Now().Add(-30 * time.Minute), // past a 15-min linger
				InstanceType: "c7g.large",
			}, nil
		},
		GetPoolP90ConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) { return 0, nil },
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-hotspare"),
					InstanceType: ec2types.InstanceTypeC7gLarge,
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
					LaunchTime:   aws.Time(time.Now().Add(-1 * time.Hour)), // past bootstrap grace
				},
			}}}}, nil
		},
		StopInstancesFunc: func(_ context.Context, params *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			stoppedIDs = append(stoppedIDs, params.InstanceIds...)
			return &ec2.StopInstancesOutput{}, nil
		},
	}

	m := NewManager(mockDB, &MockFleetAPI{}, hotPoolConfig(lingerPoolName, 15, 1))
	m.SetEC2Client(mockEC2)
	m.readyDwellPeriod = 0 // exercise decay in a single pass

	m.reconcile(context.Background())

	if len(stoppedIDs) != 1 || stoppedIDs[0] != "i-hotspare" {
		t.Fatalf("stopped IDs = %v, want [i-hotspare] (expired linger => desired_running 0 => bank the spare)", stoppedIDs)
	}
}

// lingerMetrics records pool-action reasons so tests can assert attribution.
type lingerMetrics struct {
	startReasons []string
}

func (l *lingerMetrics) PublishPoolAction(_ context.Context, _, action, reason string) error {
	if action == poolActionStart {
		l.startReasons = append(l.startReasons, reason)
	}
	return nil
}
func (l *lingerMetrics) PublishPoolDesired(context.Context, string, string, int) error   { return nil }
func (l *lingerMetrics) PublishPoolInstances(context.Context, string, string, int) error { return nil }
func (l *lingerMetrics) PublishPoolReconcileSeconds(context.Context, float64) error      { return nil }
func (l *lingerMetrics) PublishLockWaitSeconds(context.Context, string, float64) error   { return nil }
func (l *lingerMetrics) PublishInstances(context.Context, string, string, string, int) error {
	return nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Gate-off no-regression suite (production-safety proof)
//
// The overriding invariant of this feature is: with the hot-pool gate off, or
// configured only for OTHER pools, reconciliation behaves byte-identically to a
// build without the feature. This suite proves it by replaying a set of
// representative reconcile scenarios through a call recorder under three configs
// — HotPools nil (gate off), HotPools empty map, HotPools set for a DIFFERENT
// pool — and asserting every EC2/Fleet/DB side-effecting call, in order, is
// identical. A drift in any scenario means the gate leaks into the default path.
// ---------------------------------------------------------------------------

// recordedCall is one side-effecting AWS/DB call captured for sequence equality.
type recordedCall struct {
	op    string
	inst  []string // instance IDs (sorted) for EC2 ops
	extra string   // reason / fleet spec summary
}

// recorder accumulates the ordered call log for one reconcile pass.
type recorder struct {
	calls []recordedCall
}

func (r *recorder) add(op string, inst []string, extra string) {
	sorted := append([]string(nil), inst...)
	sort.Strings(sorted)
	r.calls = append(r.calls, recordedCall{op: op, inst: sorted, extra: extra})
}

// noRegressionScenario is a self-contained reconcile fixture: a pool config and
// the instances DescribeInstances reports, plus the busy set. The same fixture
// is replayed under each config variant.
type noRegressionScenario struct {
	name       string
	poolConfig *db.PoolConfig
	instances  []ec2types.Instance
	busyIDs    []string
}

// runRecorded runs one reconcile pass for the scenario under cfg, returning the
// ordered call log. Every EC2/Fleet mutating call and the DB writes are recorded;
// read calls (Describe, GetPoolConfig, busy IDs) are deterministic inputs and
// need not be logged for behavioral equality.
func runRecorded(scn noRegressionScenario, cfg *config.Config) []recordedCall {
	rec := &recorder{}
	poolName := scn.poolConfig.PoolName

	mockDB := &MockDBClient{
		ListPoolsFunc:     func(context.Context) ([]string, error) { return []string{poolName}, nil },
		GetPoolConfigFunc: func(context.Context, string) (*db.PoolConfig, error) { return scn.poolConfig, nil },
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) {
			return scn.busyIDs, nil
		},
		GetPoolP90ConcurrencyFunc: func(context.Context, string, int) (int, error) { return 0, nil },
		UpdatePoolStateFunc: func(_ context.Context, _ string, running, stopped int) error {
			rec.add("UpdatePoolState", nil, itoa2(running, stopped))
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateOnDemandInstanceFunc: func(_ context.Context, spec *fleet.LaunchSpec) (string, error) {
			rec.add("CreateOnDemand", nil, spec.Reason)
			return testInstanceNewID, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: scn.instances}}}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			rec.add("StartInstances", params.InstanceIds, "")
			return &ec2.StartInstancesOutput{}, nil
		},
		StopInstancesFunc: func(_ context.Context, params *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			rec.add("StopInstances", params.InstanceIds, "")
			return &ec2.StopInstancesOutput{}, nil
		},
		TerminateInstancesFunc: func(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			rec.add("TerminateInstances", params.InstanceIds, "")
			return &ec2.TerminateInstancesOutput{}, nil
		},
		CreateTagsFunc: func(_ context.Context, params *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			rec.add("CreateTags", params.Resources, "")
			return &ec2.CreateTagsOutput{}, nil
		},
	}

	m := NewManager(mockDB, mockFleet, cfg)
	m.SetEC2Client(mockEC2)
	// Disable time-based guards so a single pass is deterministic across variants.
	m.bootstrapGracePeriod = 0
	m.readyDwellPeriod = 0
	m.reconcile(context.Background())
	return rec.calls
}

func itoa2(a, b int) string {
	return fmtInt(a) + "," + fmtInt(b)
}

func fmtInt(n int) string {
	return strconv.Itoa(n)
}

func TestReconcileGateOffNoRegression(t *testing.T) {
	t.Parallel()

	// Scenarios span the reconcile decision tree: scale-up from stopped, create
	// on-demand deficit, warm-pool stop-excess, terminate over-desired-stopped,
	// steady state (no change), and a busy running instance (no scale-down).
	scenarios := []noRegressionScenario{
		{
			name: "scale up: start a stopped spare",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 1, DesiredStopped: 0, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				stoppedInst("i-s1", ec2types.InstanceTypeC7gLarge),
			},
		},
		{
			name: "scale up: create on-demand when no stopped inventory",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 2, DesiredStopped: 0, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{},
		},
		{
			name: "warm pool: stop an idle running spare",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 0, DesiredStopped: 1, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				runningInst("i-r1", ec2types.InstanceTypeC7gLarge, time.Now().Add(-1*time.Hour)),
			},
		},
		{
			name: "terminate over-desired stopped",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 0, DesiredStopped: 0, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				stoppedInst("i-s1", ec2types.InstanceTypeC7gLarge),
				stoppedInst("i-s2", ec2types.InstanceTypeC7gLarge),
			},
		},
		{
			name: "steady state: desired met, no change",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 1, DesiredStopped: 0, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				runningInst("i-r1", ec2types.InstanceTypeC7gLarge, time.Now().Add(-1*time.Hour)),
			},
		},
		{
			name: "busy running instance is never scaled down",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 0, DesiredStopped: 1, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				runningInst("i-busy", ec2types.InstanceTypeC7gLarge, time.Now().Add(-1*time.Hour)),
			},
			busyIDs: []string{"i-busy"},
		},
		{
			name: "ephemeral pool with recent activity (linger would target a DIFFERENT pool)",
			poolConfig: &db.PoolConfig{
				PoolName: "web", Ephemeral: true, LastJobTime: time.Now().Add(-1 * time.Minute),
				InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				stoppedInst("i-s1", ec2types.InstanceTypeC7gLarge),
			},
		},
	}

	// The gate-off baseline (HotPools nil) versus two "configured but not for this
	// pool" variants. All three MUST produce identical call sequences: the feature
	// is inert unless RUNS_FLEET_HOT_POOLS names the pool being reconciled.
	variants := []struct {
		name string
		cfg  func() *config.Config
	}{
		{name: "HotPools nil (gate off)", cfg: func() *config.Config {
			return &config.Config{SubnetIDs: []string{"subnet-1"}}
		}},
		{name: "HotPools empty map", cfg: func() *config.Config {
			return &config.Config{SubnetIDs: []string{"subnet-1"}, HotPools: map[string]config.HotPoolSpec{}}
		}},
		{name: "HotPools for a different pool", cfg: func() *config.Config {
			return &config.Config{
				SubnetIDs: []string{"subnet-1"},
				HotPools:  map[string]config.HotPoolSpec{"portal-api": {LingerMinutes: 15, MaxHot: 3}},
			}
		}},
	}

	for _, scn := range scenarios {
		scn := scn
		t.Run(scn.name, func(t *testing.T) {
			t.Parallel()
			baseline := runRecorded(scn, variants[0].cfg())
			for _, v := range variants[1:] {
				got := runRecorded(scn, v.cfg())
				if !reflect.DeepEqual(baseline, got) {
					t.Errorf("call sequence drifted with %q:\n baseline: %+v\n got:      %+v",
						v.name, baseline, got)
				}
			}
		})
	}
}

func stoppedInst(id string, t ec2types.InstanceType) ec2types.Instance {
	return ec2types.Instance{
		InstanceId:   aws.String(id),
		InstanceType: t,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
	}
}

func runningInst(id string, t ec2types.InstanceType, launch time.Time) ec2types.Instance {
	return ec2types.Instance{
		InstanceId:   aws.String(id),
		InstanceType: t,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		LaunchTime:   aws.Time(launch),
	}
}
