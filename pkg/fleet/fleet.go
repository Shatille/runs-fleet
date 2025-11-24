// Package fleet manages EC2 fleet creation and lifecycle for runner instances.
package fleet

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/circuit"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// EC2API defines EC2 operations for fleet management.
type EC2API interface {
	CreateFleet(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error)
}

// CircuitBreaker defines circuit breaker operations.
type CircuitBreaker interface {
	CheckCircuit(ctx context.Context, instanceType string) (circuit.CircuitState, error)
}

// Manager orchestrates EC2 fleet creation for runner instances.
type Manager struct {
	ec2Client      EC2API
	config         *config.Config
	circuitBreaker CircuitBreaker
}

// NewManager creates fleet manager with EC2 client and configuration.
func NewManager(cfg aws.Config, appConfig *config.Config) *Manager {
	return &Manager{
		ec2Client: ec2.NewFromConfig(cfg),
		config:    appConfig,
	}
}

// SetCircuitBreaker sets the circuit breaker for the manager.
func (m *Manager) SetCircuitBreaker(cb CircuitBreaker) {
	m.circuitBreaker = cb
}

// LaunchSpec defines EC2 instance launch parameters for workflow job.
type LaunchSpec struct {
	RunID         string
	InstanceType  string
	SubnetID      string
	Spot          bool
	Pool          string
	ForceOnDemand bool // Force on-demand even if spot is preferred (for retries)
	RetryCount    int  // Number of times this job has been retried
}

// CreateFleet launches EC2 instances using spot or on-demand capacity.
func (m *Manager) CreateFleet(ctx context.Context, spec *LaunchSpec) ([]string, error) {
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

	if spec.Pool != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:pool"),
			Value: aws.String(spec.Pool),
		})
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

	// Determine if we should use spot or on-demand
	useSpot := spec.Spot && m.config.SpotEnabled && !spec.ForceOnDemand

	// Check circuit breaker if using spot
	if useSpot && m.circuitBreaker != nil {
		state, err := m.circuitBreaker.CheckCircuit(ctx, spec.InstanceType)
		if err != nil {
			log.Printf("Warning: failed to check circuit breaker: %v", err)
			// Continue with spot if we can't check
		} else if state == circuit.StateOpen {
			log.Printf("Circuit breaker OPEN for %s, forcing on-demand", spec.InstanceType)
			useSpot = false
		}
	}

	if !useSpot {
		req.TargetCapacitySpecification.DefaultTargetCapacityType = types.DefaultTargetCapacityTypeOnDemand
		if spec.ForceOnDemand && spec.RetryCount > 0 {
			log.Printf("Using on-demand for retry #%d of run %s", spec.RetryCount, spec.RunID)
		}
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
