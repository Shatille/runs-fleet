package pools

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func reconcileStoppedPool(t *testing.T, stoppedIDs []string, desiredRunning, desiredStopped int, startErr error) (started, terminated []string) {
	t.Helper()

	instances := make([]ec2types.Instance, len(stoppedIDs))
	for i, id := range stoppedIDs {
		instances[i] = ec2types.Instance{
			InstanceId:   aws.String(id),
			InstanceType: ec2types.InstanceTypeT3Medium,
			State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
		}
	}

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) { return []string{"test-pool"}, nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{DesiredRunning: desiredRunning, DesiredStopped: desiredStopped, InstanceType: "t3.medium"}, nil
		},
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: instances}}}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			if startErr != nil {
				return nil, startErr
			}
			started = append(started, params.InstanceIds...)
			return &ec2.StartInstancesOutput{}, nil
		},
		TerminateInstancesFunc: func(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			terminated = append(terminated, params.InstanceIds...)
			return &ec2.TerminateInstancesOutput{}, nil
		},
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())
	return started, terminated
}

func TestReconcile_DoesNotTerminateInstancesItJustStarted(t *testing.T) {
	t.Parallel()

	started, terminated := reconcileStoppedPool(t, []string{"i-a", "i-b", "i-c"}, 3, 2, nil)

	if len(started) != 3 {
		t.Fatalf("started %v, want all 3 stopped instances", started)
	}
	if len(terminated) != 0 {
		t.Errorf("terminated %v in the pass that started them, want none", terminated)
	}
}

func TestReconcile_TrimsOnlyInstancesLeftStopped(t *testing.T) {
	t.Parallel()

	started, terminated := reconcileStoppedPool(t, []string{"i-a", "i-b", "i-c", "i-d"}, 1, 1, nil)

	if len(started) != 1 {
		t.Fatalf("started %v, want 1", started)
	}
	if len(terminated) != 2 {
		t.Fatalf("terminated %v, want 2 (3 left stopped, 1 desired)", terminated)
	}
	if slices.Contains(terminated, started[0]) {
		t.Errorf("terminated %v includes %s, which this pass started", terminated, started[0])
	}
}

func TestReconcile_TrimsAllExcessWhenStartFails(t *testing.T) {
	t.Parallel()

	started, terminated := reconcileStoppedPool(t, []string{"i-a", "i-b", "i-c"}, 3, 2, errors.New("start failed"))

	if len(started) != 0 {
		t.Fatalf("started %v, want none", started)
	}
	if len(terminated) != 1 {
		t.Errorf("terminated %v, want 1 (3 still stopped, 2 desired)", terminated)
	}
}
