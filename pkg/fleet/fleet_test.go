package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type mockEC2Client struct {
	CreateFleetFunc func(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error)
}

func (m *mockEC2Client) CreateFleet(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
	if m.CreateFleetFunc != nil {
		return m.CreateFleetFunc(ctx, params, optFns...)
	}
	return &ec2.CreateFleetOutput{}, nil
}

func TestCreateFleet(t *testing.T) {
	tests := []struct {
		name             string
		spec             *LaunchSpec
		config           *config.Config
		wantSpot         bool
		wantOnDemand     bool
		wantInstanceType string
	}{
		{
			name: "Spot Request",
			spec: &LaunchSpec{
				RunID:        "run-1",
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			wantSpot:         true,
			wantInstanceType: "t4g.medium",
		},
		{
			name: "Explicit On-Demand",
			spec: &LaunchSpec{
				RunID:        "run-2",
				InstanceType: "c7g.xlarge",
				SubnetID:     "subnet-1",
				Spot:         false,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			wantOnDemand:     true,
			wantInstanceType: "c7g.xlarge",
		},
		{
			name: "Spot Disabled Globally",
			spec: &LaunchSpec{
				RunID:        "run-3",
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: false,
			},
			wantOnDemand:     true,
			wantInstanceType: "t4g.medium",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, params *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					if len(params.LaunchTemplateConfigs) > 0 && len(params.LaunchTemplateConfigs[0].Overrides) > 0 {
						gotType := params.LaunchTemplateConfigs[0].Overrides[0].InstanceType
						if string(gotType) != tt.wantInstanceType {
							t.Errorf("InstanceType = %v, want %v", gotType, tt.wantInstanceType)
						}
					}

					if tt.wantSpot {
						if params.TargetCapacitySpecification.DefaultTargetCapacityType != types.DefaultTargetCapacityTypeSpot {
							t.Errorf("TargetCapacityType = %v, want Spot", params.TargetCapacitySpecification.DefaultTargetCapacityType)
						}
					} else if tt.wantOnDemand {
						if params.TargetCapacitySpecification.DefaultTargetCapacityType != types.DefaultTargetCapacityTypeOnDemand {
							t.Errorf("TargetCapacityType = %v, want OnDemand", params.TargetCapacitySpecification.DefaultTargetCapacityType)
						}
					}

					return &ec2.CreateFleetOutput{
						Instances: []types.CreateFleetInstance{
							{
								InstanceIds: []string{"i-123456789"},
							},
						},
					}, nil
				},
			}

			manager := &Manager{
				ec2Client: mock,
				config:    tt.config,
			}

			instanceIDs, err := manager.CreateFleet(context.Background(), tt.spec)
			if err != nil {
				t.Errorf("CreateFleet() error = %v", err)
			}
			if len(instanceIDs) != 1 {
				t.Errorf("CreateFleet() returned %d instance IDs, want 1", len(instanceIDs))
			}
			if len(instanceIDs) > 0 && instanceIDs[0] != "i-123456789" {
				t.Errorf("CreateFleet() returned instance ID %s, want i-123456789", instanceIDs[0])
			}
		})
	}
}

func TestCreateFleet_Errors(t *testing.T) {
	tests := []struct {
		name    string
		spec    *LaunchSpec
		config  *config.Config
		mock    *mockEC2Client
		wantErr string
	}{
		{
			name: "API error",
			spec: &LaunchSpec{
				RunID:        "run-1",
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			mock: &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					return nil, errors.New("aws api error")
				},
			},
			wantErr: "failed to create fleet",
		},
		{
			name: "Fleet creation errors",
			spec: &LaunchSpec{
				RunID:        "run-2",
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			mock: &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					return &ec2.CreateFleetOutput{
						Errors: []types.CreateFleetError{
							{
								ErrorMessage: aws.String("insufficient capacity"),
							},
						},
					}, nil
				},
			},
			wantErr: "fleet creation had errors: insufficient capacity",
		},
		{
			name: "Fleet creation errors with nil message",
			spec: &LaunchSpec{
				RunID:        "run-3",
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			mock: &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					return &ec2.CreateFleetOutput{
						Errors: []types.CreateFleetError{
							{
								ErrorMessage: nil,
							},
						},
					}, nil
				},
			},
			wantErr: "fleet creation had errors: unknown error",
		},
		{
			name: "No instances created",
			spec: &LaunchSpec{
				RunID:        "run-4",
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			mock: &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					return &ec2.CreateFleetOutput{
						Instances: []types.CreateFleetInstance{},
					}, nil
				},
			},
			wantErr: "no instances were created",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &Manager{
				ec2Client: tt.mock,
				config:    tt.config,
			}

			instanceIDs, err := manager.CreateFleet(context.Background(), tt.spec)
			if err == nil {
				t.Fatalf("CreateFleet() expected error containing %q, got nil", tt.wantErr)
			}
			if instanceIDs != nil {
				t.Errorf("CreateFleet() returned non-nil instance IDs on error: %v", instanceIDs)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("CreateFleet() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
