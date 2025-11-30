package ec2

import (
	"context"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type mockPoolEC2API struct {
	describeInstancesFunc  func(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	startInstancesFunc     func(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	stopInstancesFunc      func(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	terminateInstancesFunc func(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

func (m *mockPoolEC2API) DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if m.describeInstancesFunc != nil {
		return m.describeInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.DescribeInstancesOutput{}, nil
}

func (m *mockPoolEC2API) StartInstances(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	if m.startInstancesFunc != nil {
		return m.startInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.StartInstancesOutput{}, nil
}

func (m *mockPoolEC2API) StopInstances(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	if m.stopInstancesFunc != nil {
		return m.stopInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.StopInstancesOutput{}, nil
}

func (m *mockPoolEC2API) TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	if m.terminateInstancesFunc != nil {
		return m.terminateInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func TestPoolProvider_ListPoolRunners(t *testing.T) {
	launchTime := time.Now()

	mock := &mockPoolEC2API{
		describeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []types.Reservation{
					{
						Instances: []types.Instance{
							{
								InstanceId:   aws.String("i-running"),
								InstanceType: types.InstanceTypeT4gMedium,
								State: &types.InstanceState{
									Name: types.InstanceStateNameRunning,
								},
								LaunchTime: &launchTime,
							},
							{
								InstanceId:   aws.String("i-stopped"),
								InstanceType: types.InstanceTypeT4gMedium,
								State: &types.InstanceState{
									Name: types.InstanceStateNameStopped,
								},
							},
						},
					},
				},
			}, nil
		},
	}

	p := NewPoolProviderWithClient(mock, &config.Config{})

	runners, err := p.ListPoolRunners(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("ListPoolRunners() error = %v", err)
	}

	if len(runners) != 2 {
		t.Errorf("ListPoolRunners() got %d runners, want 2", len(runners))
	}

	// Verify running instance has idle tracking initialized
	for _, r := range runners {
		if r.RunnerID == "i-running" {
			if r.State != StateRunning {
				t.Errorf("runner i-running state = %v, want %v", r.State, StateRunning)
			}
			if r.IdleSince.IsZero() {
				t.Error("runner i-running should have IdleSince initialized")
			}
		}
		if r.RunnerID == "i-stopped" {
			if r.State != StateStopped {
				t.Errorf("runner i-stopped state = %v, want %v", r.State, StateStopped)
			}
		}
	}
}

func TestPoolProvider_StartRunners(t *testing.T) {
	called := false
	mock := &mockPoolEC2API{
		startInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			called = true
			if len(params.InstanceIds) != 2 {
				t.Errorf("StartInstances got %d instances, want 2", len(params.InstanceIds))
			}
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	p := NewPoolProviderWithClient(mock, &config.Config{})

	err := p.StartRunners(context.Background(), []string{"i-1", "i-2"})
	if err != nil {
		t.Errorf("StartRunners() error = %v", err)
	}
	if !called {
		t.Error("StartInstances was not called")
	}
}

func TestPoolProvider_StartRunners_Empty(t *testing.T) {
	mock := &mockPoolEC2API{
		startInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			t.Error("StartInstances should not be called for empty list")
			return nil, nil
		},
	}

	p := NewPoolProviderWithClient(mock, &config.Config{})

	err := p.StartRunners(context.Background(), []string{})
	if err != nil {
		t.Errorf("StartRunners() error = %v", err)
	}
}

func TestPoolProvider_StopRunners(t *testing.T) {
	mock := &mockPoolEC2API{
		stopInstancesFunc: func(_ context.Context, _ *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			return &ec2.StopInstancesOutput{}, nil
		},
	}

	p := NewPoolProviderWithClient(mock, &config.Config{})
	// Pre-populate idle tracking
	p.instanceIdle["i-1"] = time.Now()
	p.instanceIdle["i-2"] = time.Now()

	err := p.StopRunners(context.Background(), []string{"i-1", "i-2"})
	if err != nil {
		t.Errorf("StopRunners() error = %v", err)
	}

	// Verify idle tracking was cleared
	if _, ok := p.instanceIdle["i-1"]; ok {
		t.Error("i-1 should be removed from idle tracking")
	}
	if _, ok := p.instanceIdle["i-2"]; ok {
		t.Error("i-2 should be removed from idle tracking")
	}
}

func TestPoolProvider_TerminateRunners(t *testing.T) {
	mock := &mockPoolEC2API{
		terminateInstancesFunc: func(_ context.Context, _ *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			return &ec2.TerminateInstancesOutput{}, nil
		},
	}

	p := NewPoolProviderWithClient(mock, &config.Config{})
	p.instanceIdle["i-1"] = time.Now()

	err := p.TerminateRunners(context.Background(), []string{"i-1"})
	if err != nil {
		t.Errorf("TerminateRunners() error = %v", err)
	}

	if _, ok := p.instanceIdle["i-1"]; ok {
		t.Error("i-1 should be removed from idle tracking")
	}
}

func TestPoolProvider_MarkRunnerBusy(t *testing.T) {
	p := &PoolProvider{
		instanceIdle: make(map[string]time.Time),
	}
	p.instanceIdle["i-1"] = time.Now()

	p.MarkRunnerBusy("i-1")

	if _, ok := p.instanceIdle["i-1"]; ok {
		t.Error("i-1 should be removed from idle tracking after MarkRunnerBusy")
	}
}

func TestPoolProvider_MarkRunnerIdle(t *testing.T) {
	p := &PoolProvider{
		instanceIdle: make(map[string]time.Time),
	}

	p.MarkRunnerIdle("i-1")

	if _, ok := p.instanceIdle["i-1"]; !ok {
		t.Error("i-1 should be added to idle tracking after MarkRunnerIdle")
	}
}

var _ provider.PoolProvider = (*PoolProvider)(nil)
