package ec2

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type mockEC2API struct {
	describeInstancesFunc  func(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	terminateInstancesFunc func(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

type mockFleetAPI struct {
	createFleetFunc func(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)
}

func (m *mockFleetAPI) CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error) {
	if m.createFleetFunc != nil {
		return m.createFleetFunc(ctx, spec)
	}
	return []string{}, nil
}

func (m *mockEC2API) DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if m.describeInstancesFunc != nil {
		return m.describeInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.DescribeInstancesOutput{}, nil
}

func (m *mockEC2API) TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	if m.terminateInstancesFunc != nil {
		return m.terminateInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func TestProvider_Name(t *testing.T) {
	p := &Provider{}
	if got := p.Name(); got != "ec2" {
		t.Errorf("Name() = %v, want ec2", got)
	}
}

func TestProvider_DescribeRunner(t *testing.T) {
	launchTime := time.Now()

	tests := []struct {
		name      string
		runnerID  string
		mockResp  *ec2.DescribeInstancesOutput
		mockErr   error
		wantState string
		wantErr   bool
	}{
		{
			name:     "running instance",
			runnerID: "i-123",
			mockResp: &ec2.DescribeInstancesOutput{
				Reservations: []types.Reservation{
					{
						Instances: []types.Instance{
							{
								InstanceId:   aws.String("i-123"),
								InstanceType: types.InstanceTypeT4gMedium,
								State: &types.InstanceState{
									Name: types.InstanceStateNameRunning,
								},
								LaunchTime: &launchTime,
							},
						},
					},
				},
			},
			wantState: StateRunning,
		},
		{
			name:     "stopped instance",
			runnerID: "i-456",
			mockResp: &ec2.DescribeInstancesOutput{
				Reservations: []types.Reservation{
					{
						Instances: []types.Instance{
							{
								InstanceId:   aws.String("i-456"),
								InstanceType: types.InstanceTypeT4gMedium,
								State: &types.InstanceState{
									Name: types.InstanceStateNameStopped,
								},
							},
						},
					},
				},
			},
			wantState: StateStopped,
		},
		{
			name:     "instance not found",
			runnerID: "i-999",
			mockResp: &ec2.DescribeInstancesOutput{
				Reservations: []types.Reservation{},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockEC2API{
				describeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
					if tt.mockErr != nil {
						return nil, tt.mockErr
					}
					return tt.mockResp, nil
				},
			}

			p := &Provider{
				ec2Client: mock,
				config:    &config.Config{},
			}

			got, err := p.DescribeRunner(context.Background(), tt.runnerID)
			if (err != nil) != tt.wantErr {
				t.Errorf("DescribeRunner() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil && got.State != tt.wantState {
				t.Errorf("DescribeRunner() state = %v, want %v", got.State, tt.wantState)
			}
		})
	}
}

func TestProvider_TerminateRunner(t *testing.T) {
	mock := &mockEC2API{
		terminateInstancesFunc: func(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			if len(params.InstanceIds) != 1 || params.InstanceIds[0] != "i-123" {
				t.Errorf("unexpected instance ID: %v", params.InstanceIds)
			}
			return &ec2.TerminateInstancesOutput{}, nil
		},
	}

	p := &Provider{
		ec2Client: mock,
		config:    &config.Config{},
	}

	err := p.TerminateRunner(context.Background(), "i-123")
	if err != nil {
		t.Errorf("TerminateRunner() error = %v", err)
	}
}

func TestMapEC2State(t *testing.T) {
	tests := []struct {
		input types.InstanceStateName
		want  string
	}{
		{types.InstanceStateNamePending, StatePending},
		{types.InstanceStateNameRunning, StateRunning},
		{types.InstanceStateNameStopping, StateStopped},
		{types.InstanceStateNameStopped, StateStopped},
		{types.InstanceStateNameShuttingDown, StateTerminated},
		{types.InstanceStateNameTerminated, StateTerminated},
	}

	for _, tt := range tests {
		t.Run(string(tt.input), func(t *testing.T) {
			if got := mapEC2State(tt.input); got != tt.want {
				t.Errorf("mapEC2State(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestProvider_CreateRunner(t *testing.T) {
	tests := []struct {
		name        string
		spec        *provider.RunnerSpec
		mockResult  []string
		mockErr     error
		wantErr     bool
		wantIDs     []string
		verifySpec  func(t *testing.T, spec *fleet.LaunchSpec)
	}{
		{
			name: "success",
			spec: &provider.RunnerSpec{
				RunID:         123,
				JobID:         456,
				InstanceType:  "t4g.medium",
				InstanceTypes: []string{"t4g.medium", "t4g.large"},
				SubnetID:      "subnet-abc",
				Spot:          true,
				Pool:          "default",
				Region:        "us-east-1",
				Environment:   "prod",
				OS:            "linux",
				Arch:          "arm64",
			},
			mockResult: []string{"i-123", "i-456"},
			wantIDs:    []string{"i-123", "i-456"},
			verifySpec: func(t *testing.T, spec *fleet.LaunchSpec) {
				if spec.RunID != 123 {
					t.Errorf("RunID = %v, want 123", spec.RunID)
				}
				if spec.InstanceType != "t4g.medium" {
					t.Errorf("InstanceType = %v, want t4g.medium", spec.InstanceType)
				}
				if !spec.Spot {
					t.Error("Spot should be true")
				}
				if spec.Pool != "default" {
					t.Errorf("Pool = %v, want default", spec.Pool)
				}
			},
		},
		{
			name: "fleet error",
			spec: &provider.RunnerSpec{
				RunID:        789,
				InstanceType: "t4g.medium",
			},
			mockErr: errors.New("fleet creation failed"),
			wantErr: true,
		},
		{
			name: "force on-demand",
			spec: &provider.RunnerSpec{
				RunID:         999,
				InstanceType:  "c7g.xlarge",
				ForceOnDemand: true,
				RetryCount:    2,
			},
			mockResult: []string{"i-ondemand"},
			wantIDs:    []string{"i-ondemand"},
			verifySpec: func(t *testing.T, spec *fleet.LaunchSpec) {
				if !spec.ForceOnDemand {
					t.Error("ForceOnDemand should be true")
				}
				if spec.RetryCount != 2 {
					t.Errorf("RetryCount = %v, want 2", spec.RetryCount)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedSpec *fleet.LaunchSpec
			fleetMock := &mockFleetAPI{
				createFleetFunc: func(_ context.Context, spec *fleet.LaunchSpec) ([]string, error) {
					capturedSpec = spec
					return tt.mockResult, tt.mockErr
				},
			}

			p := NewProviderWithClients(fleetMock, &mockEC2API{}, &config.Config{})

			result, err := p.CreateRunner(context.Background(), tt.spec)
			if (err != nil) != tt.wantErr {
				t.Errorf("CreateRunner() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			if len(result.RunnerIDs) != len(tt.wantIDs) {
				t.Errorf("CreateRunner() returned %d IDs, want %d", len(result.RunnerIDs), len(tt.wantIDs))
			}

			for i, id := range result.RunnerIDs {
				if id != tt.wantIDs[i] {
					t.Errorf("RunnerID[%d] = %v, want %v", i, id, tt.wantIDs[i])
				}
			}

			if tt.verifySpec != nil && capturedSpec != nil {
				tt.verifySpec(t, capturedSpec)
			}
		})
	}
}

func TestProvider_DescribeRunner_NilState(t *testing.T) {
	mock := &mockEC2API{
		describeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []types.Reservation{
					{
						Instances: []types.Instance{
							{
								InstanceId:   aws.String("i-nilstate"),
								InstanceType: types.InstanceTypeT4gMedium,
								State:        nil,
							},
						},
					},
				},
			}, nil
		},
	}

	p := &Provider{
		ec2Client: mock,
		config:    &config.Config{},
	}

	got, err := p.DescribeRunner(context.Background(), "i-nilstate")
	if err != nil {
		t.Errorf("DescribeRunner() error = %v", err)
		return
	}
	if got.State != StateUnknown {
		t.Errorf("DescribeRunner() state = %v, want %v", got.State, StateUnknown)
	}
}

var _ provider.Provider = (*Provider)(nil)
