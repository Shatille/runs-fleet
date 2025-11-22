package fleet

import (
	"context"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
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
				CreateFleetFunc: func(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					// Verify Instance Type
					if len(params.LaunchTemplateConfigs) > 0 && len(params.LaunchTemplateConfigs[0].Overrides) > 0 {
						gotType := params.LaunchTemplateConfigs[0].Overrides[0].InstanceType
						if string(gotType) != tt.wantInstanceType {
							t.Errorf("InstanceType = %v, want %v", gotType, tt.wantInstanceType)
						}
					}

					// Verify Spot vs On-Demand
					if tt.wantSpot {
						if params.TargetCapacitySpecification.DefaultTargetCapacityType != types.DefaultTargetCapacityTypeSpot {
							t.Errorf("TargetCapacityType = %v, want Spot", params.TargetCapacitySpecification.DefaultTargetCapacityType)
						}
					} else if tt.wantOnDemand {
						if params.TargetCapacitySpecification.DefaultTargetCapacityType != types.DefaultTargetCapacityTypeOnDemand {
							t.Errorf("TargetCapacityType = %v, want OnDemand", params.TargetCapacitySpecification.DefaultTargetCapacityType)
						}
					}

					return &ec2.CreateFleetOutput{}, nil
				},
			}

			manager := &FleetManager{
				ec2Client: mock,
				config:    tt.config,
			}

			err := manager.CreateFleet(context.Background(), tt.spec)
			if err != nil {
				t.Errorf("CreateFleet() error = %v", err)
			}
		})
	}
}
