package worker

import (
	"context"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Test constants
const (
	testInstanceStopped = "i-stopped1"
	testInstanceRunning = "i-running1"
)

func TestTryAssignToWarmPool_NoPool(t *testing.T) {
	assigner := &WarmPoolAssigner{}

	job := &queue.JobMessage{
		JobID: 123,
		RunID: 456,
		Pool:  "", // No pool specified
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Assigned {
		t.Error("expected not assigned when no pool specified")
	}
}

func TestTryAssignToWarmPool_NilManagers(t *testing.T) {
	assigner := &WarmPoolAssigner{
		Pool:   nil,
		Runner: nil,
	}

	job := &queue.JobMessage{
		JobID: 123,
		RunID: 456,
		Pool:  "test-pool",
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Assigned {
		t.Error("expected not assigned when managers are nil")
	}
}

// mockEC2API implements pools.EC2API for testing
type mockEC2API struct {
	describeInstancesFunc  func(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	startInstancesFunc     func(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	stopInstancesFunc      func(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	terminateInstancesFunc func(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

func (m *mockEC2API) DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if m.describeInstancesFunc != nil {
		return m.describeInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.DescribeInstancesOutput{}, nil
}

func (m *mockEC2API) StartInstances(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	if m.startInstancesFunc != nil {
		return m.startInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.StartInstancesOutput{}, nil
}

func (m *mockEC2API) StopInstances(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	if m.stopInstancesFunc != nil {
		return m.stopInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.StopInstancesOutput{}, nil
}

func (m *mockEC2API) TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	if m.terminateInstancesFunc != nil {
		return m.terminateInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

// createTestAssigner creates a WarmPoolAssigner with mock EC2 for testing
func createTestAssigner(instances []ec2types.Instance) *WarmPoolAssigner {
	mockEC2 := &mockEC2API{
		describeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{Instances: instances}},
			}, nil
		},
	}

	poolManager := pools.NewManager(nil, nil, &config.Config{})
	poolManager.SetEC2Client(mockEC2)

	return &WarmPoolAssigner{
		Pool:   poolManager,
		Runner: nil,
		DB:     nil,
	}
}

func TestWarmPoolAssigner_NoAvailableInstance(t *testing.T) {
	instances := []ec2types.Instance{
		{
			InstanceId:   aws.String(testInstanceRunning),
			InstanceType: ec2types.InstanceTypeC7gXlarge,
			State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		},
	}
	assigner := createTestAssigner(instances)

	job := &queue.JobMessage{
		JobID: 123,
		RunID: 456,
		Repo:  "owner/repo",
		Pool:  "test-pool",
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Assigned {
		t.Error("expected job to NOT be assigned when no stopped instances")
	}
}

func TestWarmPoolAssigner_NoRunnerManager(t *testing.T) {
	instances := []ec2types.Instance{
		{
			InstanceId:   aws.String(testInstanceStopped),
			InstanceType: ec2types.InstanceTypeC7gXlarge,
			State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
		},
	}
	assigner := createTestAssigner(instances)

	job := &queue.JobMessage{
		JobID: 123,
		RunID: 456,
		Repo:  "owner/repo",
		Pool:  "test-pool",
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With nil Runner, should not assign (early return)
	if result.Assigned {
		t.Error("expected not assigned when Runner is nil")
	}
}

func TestWarmPoolAssigner_EmptyPool(t *testing.T) {
	assigner := createTestAssigner(nil)

	job := &queue.JobMessage{
		JobID: 123,
		RunID: 456,
		Repo:  "owner/repo",
		Pool:  "empty-pool",
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Assigned {
		t.Error("expected job to NOT be assigned from empty pool")
	}
}
