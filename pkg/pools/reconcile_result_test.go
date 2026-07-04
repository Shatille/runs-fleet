package pools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// TestReconcilePoolRecordsSuccess proves a clean reconcile pass persists a
// "success" outcome via UpdatePoolReconcileResult.
func TestReconcilePoolRecordsSuccess(t *testing.T) {
	t.Parallel()

	var gotPool, gotResult string
	var called bool
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) { return []string{"warm-pool"}, nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{PoolName: "warm-pool", DesiredRunning: 1, InstanceType: "t3.medium"}, nil
		},
		UpdatePoolStateFunc:        func(_ context.Context, _ string, _, _ int) error { return nil },
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) { return nil, nil },
		UpdatePoolReconcileResultFunc: func(_ context.Context, poolName, result string, _ time.Time) error {
			called, gotPool, gotResult = true, poolName, result
			return nil
		},
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{Instances: []ec2types.Instance{{
						InstanceId:   aws.String("i-running1"),
						InstanceType: ec2types.InstanceTypeT3Medium,
						State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
						LaunchTime:   aws.Time(time.Now().Add(-10 * time.Minute)),
					}}},
				},
			}, nil
		},
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{SubnetIDs: []string{"subnet-1"}})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	if !called {
		t.Fatal("UpdatePoolReconcileResult was not called")
	}
	if gotPool != "warm-pool" {
		t.Errorf("pool = %q, want warm-pool", gotPool)
	}
	if gotResult != "success" {
		t.Errorf("result = %q, want success", gotResult)
	}
}

// TestReconcilePoolRecordsFailure proves a failing reconcile pass persists a
// "failed: ..." outcome carrying the error.
func TestReconcilePoolRecordsFailure(t *testing.T) {
	t.Parallel()

	var gotResult string
	var called bool
	mockDB := &MockDBClient{
		ListPoolsFunc:     func(_ context.Context) ([]string, error) { return []string{"warm-pool"}, nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) { return nil, errors.New("boom") },
		UpdatePoolReconcileResultFunc: func(_ context.Context, _, result string, _ time.Time) error {
			called, gotResult = true, result
			return nil
		},
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(&MockEC2API{})

	manager.reconcile(context.Background())

	if !called {
		t.Fatal("UpdatePoolReconcileResult was not called")
	}
	if !strings.HasPrefix(gotResult, "failed:") {
		t.Errorf("result = %q, want prefix %q", gotResult, "failed:")
	}
	if !strings.Contains(gotResult, "boom") {
		t.Errorf("result = %q, want it to carry the error", gotResult)
	}
}
