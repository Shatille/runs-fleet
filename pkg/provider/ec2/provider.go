// Package ec2 implements the provider interfaces for AWS EC2.
package ec2

import (
	"context"
	"fmt"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Runner state constants.
const (
	StateRunning    = "running"
	StatePending    = "pending"
	StateStopped    = "stopped"
	StateTerminated = "terminated"
	StateUnknown    = "unknown"
)

// API defines EC2 operations for instance management.
type API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// FleetAPI defines fleet management operations.
type FleetAPI interface {
	CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)
}

// Provider implements provider.Provider for EC2.
type Provider struct {
	fleetManager FleetAPI
	ec2Client    API
	config       *config.Config
}

// NewProvider creates an EC2 provider with optional circuit breaker.
func NewProvider(awsCfg aws.Config, appConfig *config.Config, circuitBreaker fleet.CircuitBreaker) *Provider {
	fm := fleet.NewManager(awsCfg, appConfig)
	if circuitBreaker != nil {
		fm.SetCircuitBreaker(circuitBreaker)
	}
	return &Provider{
		fleetManager: fm,
		ec2Client:    ec2.NewFromConfig(awsCfg),
		config:       appConfig,
	}
}

// NewProviderWithClients creates a Provider with injected clients for testing.
func NewProviderWithClients(fleetClient FleetAPI, ec2Client API, appConfig *config.Config) *Provider {
	return &Provider{
		fleetManager: fleetClient,
		ec2Client:    ec2Client,
		config:       appConfig,
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string {
	return "ec2"
}

// CreateRunner creates EC2 instances for a runner.
func (p *Provider) CreateRunner(ctx context.Context, spec *provider.RunnerSpec) (*provider.RunnerResult, error) {
	launchSpec := &fleet.LaunchSpec{
		RunID:         spec.RunID,
		InstanceType:  spec.InstanceType,
		InstanceTypes: spec.InstanceTypes,
		SubnetID:      spec.SubnetID,
		Spot:          spec.Spot,
		Pool:          spec.Pool,
		ForceOnDemand: spec.ForceOnDemand,
		RetryCount:    spec.RetryCount,
		Region:        spec.Region,
		Environment:   spec.Environment,
		OS:            spec.OS,
		Arch:          spec.Arch,
	}

	instanceIDs, err := p.fleetManager.CreateFleet(ctx, launchSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to create fleet: %w", err)
	}

	return &provider.RunnerResult{
		RunnerIDs: instanceIDs,
		ProviderData: map[string]string{
			"instance_type": spec.InstanceType,
			"subnet_id":     spec.SubnetID,
		},
	}, nil
}

// TerminateRunner terminates an EC2 instance.
func (p *Provider) TerminateRunner(ctx context.Context, runnerID string) error {
	_, err := p.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{runnerID},
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instance %s: %w", runnerID, err)
	}
	return nil
}

// DescribeRunner returns the current state of an EC2 instance.
func (p *Provider) DescribeRunner(ctx context.Context, runnerID string) (*provider.RunnerState, error) {
	output, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{runnerID},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe instance %s: %w", runnerID, err)
	}

	if len(output.Reservations) == 0 || len(output.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("instance %s not found", runnerID)
	}

	inst := output.Reservations[0].Instances[0]
	instanceState := StateUnknown
	if inst.State != nil {
		instanceState = mapEC2State(inst.State.Name)
	}
	state := &provider.RunnerState{
		RunnerID:     runnerID,
		State:        instanceState,
		InstanceType: string(inst.InstanceType),
		ProviderData: map[string]string{},
	}

	if inst.LaunchTime != nil {
		state.LaunchTime = *inst.LaunchTime
	}

	return state, nil
}

// mapEC2State maps EC2 instance state to provider state.
func mapEC2State(state types.InstanceStateName) string {
	switch state {
	case types.InstanceStateNamePending:
		return StatePending
	case types.InstanceStateNameRunning:
		return StateRunning
	case types.InstanceStateNameStopping, types.InstanceStateNameStopped:
		return StateStopped
	case types.InstanceStateNameShuttingDown, types.InstanceStateNameTerminated:
		return StateTerminated
	default:
		return string(state)
	}
}

// Ensure Provider implements provider.Provider.
var _ provider.Provider = (*Provider)(nil)
