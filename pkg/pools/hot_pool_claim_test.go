package pools

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const (
	iRunID  = "i-run"
	iStopID = "i-stop"
)

// mixedReservation returns a reservation with the given running and stopped IDs.
func mixedReservation(running, stopped []string) []ec2types.Reservation {
	insts := make([]ec2types.Instance, 0, len(running)+len(stopped))
	for _, id := range running {
		insts = append(insts, ec2types.Instance{
			InstanceId:   aws.String(id),
			InstanceType: ec2types.InstanceTypeC7gXlarge,
			State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		})
	}
	for _, id := range stopped {
		insts = append(insts, ec2types.Instance{
			InstanceId:   aws.String(id),
			InstanceType: ec2types.InstanceTypeC7gXlarge,
			State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
		})
	}
	return []ec2types.Reservation{{Instances: insts}}
}

func hotGateConfig() *config.Config {
	return &config.Config{
		SubnetIDs: []string{"subnet-1"},
		HotPools:  map[string]config.HotPoolSpec{"hot": {LingerMinutes: 15, MaxHot: 1}},
	}
}

// A hot spare (running, not busy) is preferred over a stopped instance and is
// assigned WITHOUT StartInstances — the whole point of hot pools.
func TestClaim_PrefersRunningSpare_NoStart(t *testing.T) {
	t.Parallel()

	var startCalls, tagCalls, busyCalls int64

	mockDB := &MockDBClient{
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) {
			atomic.AddInt64(&busyCalls, 1)
			return nil, nil
		},
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: mixedReservation([]string{iRunID}, []string{iStopID})}, nil
		},
		StartInstancesFunc: func(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			atomic.AddInt64(&startCalls, 1)
			return &ec2.StartInstancesOutput{}, nil
		},
		CreateTagsFunc: func(context.Context, *ec2.CreateTagsInput, ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			atomic.AddInt64(&tagCalls, 1)
			return &ec2.CreateTagsOutput{}, nil
		},
	}

	m := NewManager(mockDB, &MockFleetAPI{}, hotGateConfig())
	m.SetEC2Client(mockEC2)

	inst, err := m.ClaimAndStartPoolInstance(context.Background(), "hot", 1, "owner/repo", nil)
	if err != nil {
		t.Fatalf("claim err = %v", err)
	}
	if inst.InstanceID != iRunID {
		t.Errorf("claimed %s, want i-run (running spare preferred)", inst.InstanceID)
	}
	if !inst.IsFromRunningSpare() {
		t.Errorf("IsFromRunningSpare() = false, want true for a running claim")
	}
	if n := atomic.LoadInt64(&startCalls); n != 0 {
		t.Errorf("StartInstances called %d times, want 0 (hot spare is already running)", n)
	}
	if n := atomic.LoadInt64(&tagCalls); n != 1 {
		t.Errorf("CreateTags called %d times, want 1 (Role tag still applied)", n)
	}
	if n := atomic.LoadInt64(&busyCalls); n != 1 {
		t.Errorf("GetPoolBusyInstanceIDs called %d times, want 1 (mandatory for hot pools)", n)
	}
}

// A busy running instance (claim still live) is skipped; the claim falls to the
// stopped instance, which IS started.
func TestClaim_BusyRunningSkipped_FallsToStopped(t *testing.T) {
	t.Parallel()

	var startCalls int64

	mockDB := &MockDBClient{
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) {
			return []string{iRunID}, nil // the only running instance is busy
		},
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: mixedReservation([]string{iRunID}, []string{iStopID})}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			atomic.AddInt64(&startCalls, 1)
			if params.InstanceIds[0] != iStopID {
				t.Errorf("started %v, want i-stop", params.InstanceIds)
			}
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	m := NewManager(mockDB, &MockFleetAPI{}, hotGateConfig())
	m.SetEC2Client(mockEC2)

	inst, err := m.ClaimAndStartPoolInstance(context.Background(), "hot", 1, "owner/repo", nil)
	if err != nil {
		t.Fatalf("claim err = %v", err)
	}
	if inst.InstanceID != iStopID {
		t.Errorf("claimed %s, want i-stop (busy running skipped)", inst.InstanceID)
	}
	if inst.IsFromRunningSpare() {
		t.Errorf("IsFromRunningSpare() = true, want false for a stopped claim")
	}
	if n := atomic.LoadInt64(&startCalls); n != 1 {
		t.Errorf("StartInstances called %d times, want 1 (stopped instance started)", n)
	}
}

// A claim conflict on the preferred running spare (another orchestrator won it)
// falls through to the stopped instance.
func TestClaim_RunningConflict_FallsToStopped(t *testing.T) {
	t.Parallel()

	claimAttempts := map[string]int{}
	mockDB := &MockDBClient{
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) { return nil, nil },
		ClaimInstanceForJobFunc: func(_ context.Context, instanceID string, _ int64, _ time.Duration) error {
			claimAttempts[instanceID]++
			if instanceID == iRunID {
				return db.ErrInstanceAlreadyClaimed // lost the race for the hot spare
			}
			return nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: mixedReservation([]string{iRunID}, []string{iStopID})}, nil
		},
	}

	m := NewManager(mockDB, &MockFleetAPI{}, hotGateConfig())
	m.SetEC2Client(mockEC2)

	inst, err := m.ClaimAndStartPoolInstance(context.Background(), "hot", 1, "owner/repo", nil)
	if err != nil {
		t.Fatalf("claim err = %v", err)
	}
	if inst.InstanceID != iStopID {
		t.Errorf("claimed %s, want i-stop (running claim lost the race)", inst.InstanceID)
	}
	if claimAttempts[iRunID] != 1 {
		t.Errorf("i-run claim attempts = %d, want 1", claimAttempts[iRunID])
	}
}

// A running SPOT instance (cold-start overflow that got pool-tagged) is not a
// reliable hot spare — it can be interrupted mid-job — so it is skipped and the
// claim falls to the stopped on-demand instance.
func TestClaim_RunningSpotSkipped_FallsToStopped(t *testing.T) {
	t.Parallel()

	mockDB := &MockDBClient{
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) { return nil, nil },
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
				{
					InstanceId:        aws.String(iRunID),
					InstanceType:      ec2types.InstanceTypeC7gXlarge,
					State:             &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
					InstanceLifecycle: ec2types.InstanceLifecycleTypeSpot,
				},
				{
					InstanceId:   aws.String(iStopID),
					InstanceType: ec2types.InstanceTypeC7gXlarge,
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			}}}}, nil
		},
		StartInstancesFunc: func(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	m := NewManager(mockDB, &MockFleetAPI{}, hotGateConfig())
	m.SetEC2Client(mockEC2)

	inst, err := m.ClaimAndStartPoolInstance(context.Background(), "hot", 1, "owner/repo", nil)
	if err != nil {
		t.Fatalf("claim err = %v", err)
	}
	if inst.InstanceID != iStopID || inst.IsFromRunningSpare() {
		t.Errorf("claimed %+v, want stopped i-stop (running spot is not a hot spare)", inst)
	}
}

// On a busy-query error, hot-pool claiming degrades to stopped-only rather than
// risk handing out a possibly-busy running instance.
func TestClaim_BusyQueryError_DegradesToStopped(t *testing.T) {
	t.Parallel()

	var startCalls int64

	mockDB := &MockDBClient{
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) {
			return nil, errors.New("dynamo throttled")
		},
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: mixedReservation([]string{iRunID}, []string{iStopID})}, nil
		},
		StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			atomic.AddInt64(&startCalls, 1)
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	m := NewManager(mockDB, &MockFleetAPI{}, hotGateConfig())
	m.SetEC2Client(mockEC2)

	inst, err := m.ClaimAndStartPoolInstance(context.Background(), "hot", 1, "owner/repo", nil)
	if err != nil {
		t.Fatalf("claim err = %v", err)
	}
	if inst.InstanceID != iStopID || inst.IsFromRunningSpare() {
		t.Errorf("claimed %+v, want stopped i-stop (degrade on busy-query error)", inst)
	}
	if n := atomic.LoadInt64(&startCalls); n != 1 {
		t.Errorf("StartInstances called %d times, want 1", n)
	}
}

// The gate-off proof for the claim path: a pool NOT in the hot allowlist makes
// zero busy-instance queries and never claims a running instance — exactly the
// prior behavior.
func TestClaim_GateOff_NoBusyQuery_NoRunningClaim(t *testing.T) {
	t.Parallel()

	var busyCalls, startCalls int64
	var claimedIDs []string

	mockDB := &MockDBClient{
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) {
			atomic.AddInt64(&busyCalls, 1)
			return nil, nil
		},
		ClaimInstanceForJobFunc: func(_ context.Context, instanceID string, _ int64, _ time.Duration) error {
			claimedIDs = append(claimedIDs, instanceID)
			return nil
		},
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// A running (idle) instance is present but must be IGNORED with the gate off.
			return &ec2.DescribeInstancesOutput{Reservations: mixedReservation([]string{iRunID}, []string{iStopID})}, nil
		},
		StartInstancesFunc: func(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			atomic.AddInt64(&startCalls, 1)
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	// Two config variants that both leave "cold" pool ungated.
	for _, cfg := range []*config.Config{
		{SubnetIDs: []string{"subnet-1"}}, // HotPools nil
		{SubnetIDs: []string{"subnet-1"}, HotPools: map[string]config.HotPoolSpec{"other": {LingerMinutes: 5, MaxHot: 1}}}, // different pool
	} {
		busyCalls, startCalls, claimedIDs = 0, 0, nil
		m := NewManager(mockDB, &MockFleetAPI{}, cfg)
		m.SetEC2Client(mockEC2)

		inst, err := m.ClaimAndStartPoolInstance(context.Background(), "cold", 1, "owner/repo", nil)
		if err != nil {
			t.Fatalf("claim err = %v", err)
		}
		if inst.InstanceID != iStopID {
			t.Errorf("claimed %s, want i-stop (running ignored with gate off)", inst.InstanceID)
		}
		if inst.IsFromRunningSpare() {
			t.Errorf("IsFromRunningSpare() = true, want false with gate off")
		}
		if n := atomic.LoadInt64(&busyCalls); n != 0 {
			t.Errorf("GetPoolBusyInstanceIDs called %d times with gate off, want 0 (zero extra DB reads)", n)
		}
		if len(claimedIDs) != 1 || claimedIDs[0] != iStopID {
			t.Errorf("claimed IDs = %v, want [i-stop]", claimedIDs)
		}
	}
}
