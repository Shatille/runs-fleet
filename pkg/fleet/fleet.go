package fleet

import (
	"context"
	"fmt"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type EC2API interface {
	CreateFleet(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error)
}

type FleetManager struct {
	ec2Client EC2API
	config    *config.Config
}

func NewManager(cfg aws.Config, appConfig *config.Config) *FleetManager {
	return &FleetManager{
		ec2Client: ec2.NewFromConfig(cfg),
		config:    appConfig,
	}
}

type LaunchSpec struct {
	RunID        string
	InstanceType string
	SubnetID     string
	Spot         bool
}

func (m *FleetManager) CreateFleet(ctx context.Context, spec *LaunchSpec) error {
	launchTemplate := &types.FleetLaunchTemplateSpecificationRequest{
		LaunchTemplateName: aws.String(m.config.LaunchTemplateName),
		Version:            aws.String("$Latest"),
	}

	overrides := []types.FleetLaunchTemplateOverridesRequest{
		{
			InstanceType: types.InstanceType(spec.InstanceType),
			SubnetId:     aws.String(spec.SubnetID),
		},
	}

	targetCapacity := int32(1)

	req := &ec2.CreateFleetInput{
		LaunchTemplateConfigs: []types.FleetLaunchTemplateConfigRequest{
			{
				LaunchTemplateSpecification: launchTemplate,
				Overrides:                   overrides,
			},
		},
		TargetCapacitySpecification: &types.TargetCapacitySpecificationRequest{
			TotalTargetCapacity:       &targetCapacity,
			DefaultTargetCapacityType: types.DefaultTargetCapacityTypeSpot,
		},
		Type: types.FleetTypeRequest,
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags: []types.Tag{
					{
						Key:   aws.String("runs-fleet:run-id"),
						Value: aws.String(spec.RunID),
					},
					{
						Key:   aws.String("runs-fleet:managed"),
						Value: aws.String("true"),
					},
				},
			},
		},
	}

	if !spec.Spot || !m.config.SpotEnabled {
		req.TargetCapacitySpecification.DefaultTargetCapacityType = types.DefaultTargetCapacityTypeOnDemand
	} else {
		req.SpotOptions = &types.SpotOptionsRequest{
			AllocationStrategy: types.SpotAllocationStrategyPriceCapacityOptimized,
		}
	}

	output, err := m.ec2Client.CreateFleet(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to create fleet: %w", err)
	}

	if len(output.Errors) > 0 {
		errMsg := "unknown error"
		if output.Errors[0].ErrorMessage != nil {
			errMsg = *output.Errors[0].ErrorMessage
		}
		return fmt.Errorf("fleet creation had errors: %s", errMsg)
	}

	if len(output.Instances) == 0 {
		return fmt.Errorf("no instances were created")
	}

	return nil
}
