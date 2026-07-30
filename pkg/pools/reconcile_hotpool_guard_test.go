package pools

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type scaleDownCapture struct {
	stopped    []string
	terminated []string
}

func (c *scaleDownCapture) all() []string {
	return append(append([]string{}, c.stopped...), c.terminated...)
}

func hotPoolInstance(id string, launched time.Time) ec2types.Instance {
	return ec2types.Instance{
		InstanceId:   aws.String(id),
		InstanceType: ec2types.InstanceTypeT3Medium,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		LaunchTime:   aws.Time(launched),
	}
}

func newHotPoolManager(t *testing.T, instances []ec2types.Instance, busyIDs []string) (*Manager, *scaleDownCapture) {
	t.Helper()
	capture := &scaleDownCapture{}

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning:     1,
				DesiredStopped:     0,
				InstanceType:       "t3.medium",
				IdleTimeoutMinutes: 1,
			}, nil
		},
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) {
			return busyIDs, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{Instances: instances}},
			}, nil
		},
		StopInstancesFunc: func(_ context.Context, params *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			capture.stopped = append(capture.stopped, params.InstanceIds...)
			return &ec2.StopInstancesOutput{}, nil
		},
		TerminateInstancesFunc: func(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			capture.terminated = append(capture.terminated, params.InstanceIds...)
			return &ec2.TerminateInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)
	return manager, capture
}

func markIdle(manager *Manager, ids ...string) {
	idleTime := time.Now().Add(-10 * time.Minute)
	for _, id := range ids {
		manager.instanceIdle[id] = idleTime
	}
}

// A busy instance must never be a hot-pool scale-down candidate even when its
// per-replica idle timestamp is stale (another replica assigned the job, or
// this replica restarted and re-seeded IdleSince).
func TestReconcileHotPoolNeverStopsBusyInstance(t *testing.T) {
	t.Parallel()

	oldLaunch := time.Now().Add(-1 * time.Hour)
	manager, capture := newHotPoolManager(t, []ec2types.Instance{
		hotPoolInstance("i-busy", oldLaunch),
		hotPoolInstance("i-idle1", oldLaunch),
		hotPoolInstance("i-idle2", oldLaunch),
	}, []string{"i-busy"})
	manager.readyDwellPeriod = 0
	markIdle(manager, "i-busy", "i-idle1", "i-idle2")

	manager.reconcile(context.Background())

	if slices.Contains(capture.all(), "i-busy") {
		t.Errorf("busy instance must not be stopped or terminated, got stopped=%v terminated=%v",
			capture.stopped, capture.terminated)
	}
	if len(capture.all()) == 0 {
		t.Error("expected an idle non-busy instance to be scaled down")
	}
}

// An instance still inside the bootstrap grace window is not a scale-down
// candidate: stopping mid-boot churns the pool.
func TestReconcileHotPoolDefersWithinBootstrapGrace(t *testing.T) {
	t.Parallel()

	manager, capture := newHotPoolManager(t, []ec2types.Instance{
		hotPoolInstance("i-fresh1", time.Now()),
		hotPoolInstance("i-fresh2", time.Now()),
	}, nil)
	manager.readyDwellPeriod = 0
	markIdle(manager, "i-fresh1", "i-fresh2")

	manager.reconcile(context.Background())

	if len(capture.all()) != 0 {
		t.Errorf("instances within bootstrap grace must not be scaled down, got stopped=%v terminated=%v",
			capture.stopped, capture.terminated)
	}
}

// An instance observed not-busy for less than the dwell window is not a
// scale-down candidate: the busy set can momentarily miss a live instance.
func TestReconcileHotPoolDefersWithinReadyDwell(t *testing.T) {
	t.Parallel()

	oldLaunch := time.Now().Add(-1 * time.Hour)
	manager, capture := newHotPoolManager(t, []ec2types.Instance{
		hotPoolInstance("i-recent1", oldLaunch),
		hotPoolInstance("i-recent2", oldLaunch),
	}, nil)
	markIdle(manager, "i-recent1", "i-recent2")

	manager.reconcile(context.Background())

	if len(capture.all()) != 0 {
		t.Errorf("instances within the ready dwell must not be scaled down, got stopped=%v terminated=%v",
			capture.stopped, capture.terminated)
	}
}

// An instance a concurrent local claim has reserved must not be a scale-down
// candidate.
func TestReconcileHotPoolSkipsInFlightClaim(t *testing.T) {
	t.Parallel()

	oldLaunch := time.Now().Add(-1 * time.Hour)
	manager, capture := newHotPoolManager(t, []ec2types.Instance{
		hotPoolInstance("i-inflight", oldLaunch),
		hotPoolInstance("i-idle1", oldLaunch),
	}, nil)
	manager.readyDwellPeriod = 0
	markIdle(manager, "i-inflight", "i-idle1")
	manager.poolLockFor("test-pool").reserve("i-inflight")

	manager.reconcile(context.Background())

	if slices.Contains(capture.all(), "i-inflight") {
		t.Errorf("in-flight instance must not be scaled down, got stopped=%v terminated=%v",
			capture.stopped, capture.terminated)
	}
}

// Regression: a genuinely idle spare that passes every guard is still scaled
// down once it exceeds the pool's idle timeout.
func TestReconcileHotPoolStopsGenuinelyIdleSpare(t *testing.T) {
	t.Parallel()

	oldLaunch := time.Now().Add(-1 * time.Hour)
	manager, capture := newHotPoolManager(t, []ec2types.Instance{
		hotPoolInstance("i-keep", oldLaunch),
		hotPoolInstance("i-idle", oldLaunch),
	}, nil)
	manager.readyDwellPeriod = 0
	markIdle(manager, "i-keep", "i-idle")

	manager.reconcile(context.Background())

	if len(capture.all()) != 1 {
		t.Errorf("expected exactly one idle spare scaled down, got stopped=%v terminated=%v",
			capture.stopped, capture.terminated)
	}
}
