package fleet

import (
	"context"
	"encoding/base64"
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
	userData, err := m.generateUserData(spec.RunID)
	if err != nil {
		return fmt.Errorf("failed to generate user data: %w", err)
	}

	launchTemplate := &types.FleetLaunchTemplateSpecificationRequest{
		LaunchTemplateName: aws.String("runs-fleet-runner"), // Assumes pre-created LT
		Version:            aws.String("$Latest"),
	}

	overrides := []types.FleetLaunchTemplateOverridesRequest{
		{
			InstanceType: types.InstanceType(spec.InstanceType),
			SubnetId:     aws.String(spec.SubnetID),
		},
	}

	// TODO: Use userData in Launch Template Version
	_ = userData

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
	}

	if !spec.Spot || !m.config.SpotEnabled {
		req.TargetCapacitySpecification.DefaultTargetCapacityType = types.DefaultTargetCapacityTypeOnDemand
	} else {
		req.SpotOptions = &types.SpotOptionsRequest{
			AllocationStrategy: types.SpotAllocationStrategyPriceCapacityOptimized,
		}
	}

	_, err = m.ec2Client.CreateFleet(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to create fleet: %w", err)
	}

	return nil
}

func (m *FleetManager) generateUserData(runID string) (string, error) {
	// Basic user data to start the agent
	// In a real scenario, this would be more complex, possibly fetching a binary from S3
	script := fmt.Sprintf(`#!/bin/bash
echo "Starting runs-fleet agent for run %s"
export RUNS_FLEET_INSTANCE_ID=$(curl -s http://169.254.169.254/latest/meta-data/instance-id)
export RUNS_FLEET_RUN_ID=%s
# /usr/local/bin/agent would be baked into the AMI or downloaded here
`, runID, runID)

	return base64.StdEncoding.EncodeToString([]byte(script)), nil
}
