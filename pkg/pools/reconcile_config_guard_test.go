package pools

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type mockConfigChecker struct {
	hasConfig map[string]bool
	err       error
	calls     int
}

func (m *mockConfigChecker) HasRunnerConfig(_ context.Context, instanceID string) (bool, error) {
	m.calls++
	if m.err != nil {
		return false, m.err
	}
	return m.hasConfig[instanceID], nil
}

// A running spare whose runner config still exists has a live assignment the
// busy set may not reflect yet (or anymore); it must not be scaled down.
func TestReconcileSkipsSpareWithRunnerConfig(t *testing.T) {
	t.Parallel()

	oldLaunch := time.Now().Add(-1 * time.Hour)
	manager, capture := newHotPoolManager(t, []ec2types.Instance{
		hotPoolInstance("i-assigned", oldLaunch),
		hotPoolInstance("i-clean1", oldLaunch),
		hotPoolInstance("i-clean2", oldLaunch),
	}, nil)
	manager.readyDwellPeriod = 0
	markIdle(manager, "i-assigned", "i-clean1", "i-clean2")
	manager.SetRunnerConfigChecker(&mockConfigChecker{hasConfig: map[string]bool{"i-assigned": true}})

	manager.reconcile(context.Background())

	if slices.Contains(capture.all(), "i-assigned") {
		t.Errorf("instance with live runner config must not be scaled down, got stopped=%v terminated=%v",
			capture.stopped, capture.terminated)
	}
	if len(capture.all()) != 2 {
		t.Errorf("expected the two clean spares scaled down, got stopped=%v terminated=%v",
			capture.stopped, capture.terminated)
	}
}

// A config-check failure means the assignment state is unknown; never scale
// down on uncertainty (worst case is a one-cycle deferral).
func TestReconcileConfigCheckErrorFailsClosed(t *testing.T) {
	t.Parallel()

	oldLaunch := time.Now().Add(-1 * time.Hour)
	manager, capture := newHotPoolManager(t, []ec2types.Instance{
		hotPoolInstance("i-unknown1", oldLaunch),
		hotPoolInstance("i-unknown2", oldLaunch),
	}, nil)
	manager.readyDwellPeriod = 0
	markIdle(manager, "i-unknown1", "i-unknown2")
	manager.SetRunnerConfigChecker(&mockConfigChecker{err: errors.New("ssm unavailable")})

	manager.reconcile(context.Background())

	if len(capture.all()) != 0 {
		t.Errorf("must not scale down when config state is unknown, got stopped=%v terminated=%v",
			capture.stopped, capture.terminated)
	}
}

// Without a configured checker the reconciler behaves exactly as before.
func TestReconcileNilConfigCheckerUnchanged(t *testing.T) {
	t.Parallel()

	oldLaunch := time.Now().Add(-1 * time.Hour)
	manager, capture := newHotPoolManager(t, []ec2types.Instance{
		hotPoolInstance("i-idle1", oldLaunch),
		hotPoolInstance("i-idle2", oldLaunch),
	}, nil)
	manager.readyDwellPeriod = 0
	markIdle(manager, "i-idle1", "i-idle2")

	manager.reconcile(context.Background())

	if len(capture.all()) != 1 {
		t.Errorf("expected one excess spare scaled down without a checker, got stopped=%v terminated=%v",
			capture.stopped, capture.terminated)
	}
}

// The guard stops probing once enough clean candidates cover the excess, so
// SSM reads stay bounded by the scale-down size.
func TestReconcileConfigCheckStopsEarly(t *testing.T) {
	t.Parallel()

	oldLaunch := time.Now().Add(-1 * time.Hour)
	checker := &mockConfigChecker{}
	manager, _ := newHotPoolManager(t, []ec2types.Instance{
		hotPoolInstance("i-idle1", oldLaunch),
		hotPoolInstance("i-idle2", oldLaunch),
	}, nil)
	manager.readyDwellPeriod = 0
	markIdle(manager, "i-idle1", "i-idle2")
	manager.SetRunnerConfigChecker(checker)

	manager.reconcile(context.Background())

	if checker.calls != 1 {
		t.Errorf("expected exactly 1 config check for excess=1, got %d", checker.calls)
	}
}

// Terminating surplus STOPPED instances involves no agent or registration, so
// the config guard must not add SSM reads there.
func TestReconcileStoppedTerminationSkipsConfigCheck(t *testing.T) {
	t.Parallel()

	checker := &mockConfigChecker{}
	terminated := []string{}

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 0,
				DesiredStopped: 1,
				InstanceType:   "t3.medium",
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
					{
						InstanceId:   aws.String("i-stopped1"),
						InstanceType: ec2types.InstanceTypeT3Medium,
						State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
					},
					{
						InstanceId:   aws.String("i-stopped2"),
						InstanceType: ec2types.InstanceTypeT3Medium,
						State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
					},
				}}},
			}, nil
		},
		TerminateInstancesFunc: func(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			terminated = append(terminated, params.InstanceIds...)
			return &ec2.TerminateInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)
	manager.SetRunnerConfigChecker(checker)

	manager.reconcile(context.Background())

	if len(terminated) != 1 {
		t.Fatalf("expected one surplus stopped instance terminated, got %v", terminated)
	}
	if checker.calls != 0 {
		t.Errorf("stopped-instance termination must not probe configs, got %d calls", checker.calls)
	}
}
