package fleet

import (
	"context"
	"fmt"
	"strings"

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

func (m *FleetManager) CreateFleet(ctx context.Context, spec *LaunchSpec) ([]string, error) {
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

	tags := []types.Tag{
		{
			Key:   aws.String("runs-fleet:run-id"),
			Value: aws.String(spec.RunID),
		},
		{
			Key:   aws.String("runs-fleet:managed"),
			Value: aws.String("true"),
		},
	}

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
				Tags:         tags,
			},
			{
				ResourceType: types.ResourceTypeVolume,
				Tags:         tags,
			},
			{
				ResourceType: types.ResourceTypeNetworkInterface,
				Tags:         tags,
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
		return nil, fmt.Errorf("failed to create fleet: %w", err)
	}

	if len(output.Errors) > 0 {
		var errMsgs []string
		for _, e := range output.Errors {
			if e.ErrorMessage != nil {
				errMsgs = append(errMsgs, *e.ErrorMessage)
			} else {
				errMsgs = append(errMsgs, "unknown error")
			}
		}
		return nil, fmt.Errorf("fleet creation had errors: %s", strings.Join(errMsgs, ", "))
	}

	if len(output.Instances) == 0 {
		return nil, fmt.Errorf("no instances were created")
	}

	instanceIDs := make([]string, 0, len(output.Instances))
	for _, inst := range output.Instances {
		instanceIDs = append(instanceIDs, inst.InstanceIds...)
	}

	return instanceIDs, nil
}
