package ec2

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// PoolAPI defines EC2 operations for pool management.
type PoolAPI interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StartInstances(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// PoolProvider implements provider.PoolProvider for EC2.
type PoolProvider struct {
	mu           sync.RWMutex
	ec2Client    PoolAPI
	config       *config.Config
	instanceIdle map[string]time.Time
}

// NewPoolProvider creates an EC2 pool provider.
func NewPoolProvider(awsCfg aws.Config, appConfig *config.Config) *PoolProvider {
	return &PoolProvider{
		ec2Client:    ec2.NewFromConfig(awsCfg),
		config:       appConfig,
		instanceIdle: make(map[string]time.Time),
	}
}

// NewPoolProviderWithClient creates an EC2 pool provider with a custom client (for testing).
func NewPoolProviderWithClient(client PoolAPI, appConfig *config.Config) *PoolProvider {
	return &PoolProvider{
		ec2Client:    client,
		config:       appConfig,
		instanceIdle: make(map[string]time.Time),
	}
}

// ListPoolRunners returns all runners in a pool.
func (p *PoolProvider) ListPoolRunners(ctx context.Context, poolName string) ([]provider.PoolRunner, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:runs-fleet:pool"),
				Values: []string{poolName},
			},
			{
				Name:   aws.String("tag:runs-fleet:managed"),
				Values: []string{"true"},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"pending", "running", "stopping", "stopped"},
			},
		},
	}

	output, err := p.ec2Client.DescribeInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %w", err)
	}

	// Build runners list without lock
	var runners []provider.PoolRunner
	for _, reservation := range output.Reservations {
		for _, inst := range reservation.Instances {
			state := StateUnknown
			if inst.State != nil {
				state = mapEC2PoolState(inst.State.Name)
			}
			runner := provider.PoolRunner{
				RunnerID:     aws.ToString(inst.InstanceId),
				State:        state,
				InstanceType: string(inst.InstanceType),
			}
			if inst.LaunchTime != nil {
				runner.LaunchTime = *inst.LaunchTime
			}
			runners = append(runners, runner)
		}
	}

	// Apply idle tracking with lock held only for map access
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for i := range runners {
		if idleSince, ok := p.instanceIdle[runners[i].RunnerID]; ok {
			runners[i].IdleSince = idleSince
		} else if runners[i].State == StateRunning {
			// Initialize idle tracking for new running instances
			p.instanceIdle[runners[i].RunnerID] = now
			runners[i].IdleSince = now
		}
	}

	return runners, nil
}

// StartRunners starts stopped EC2 instances.
func (p *PoolProvider) StartRunners(ctx context.Context, runnerIDs []string) error {
	if len(runnerIDs) == 0 {
		return nil
	}

	_, err := p.ec2Client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: runnerIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to start instances: %w", err)
	}

	return nil
}

// StopRunners stops running EC2 instances.
func (p *PoolProvider) StopRunners(ctx context.Context, runnerIDs []string) error {
	if len(runnerIDs) == 0 {
		return nil
	}

	_, err := p.ec2Client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: runnerIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to stop instances: %w", err)
	}

	p.mu.Lock()
	for _, id := range runnerIDs {
		delete(p.instanceIdle, id)
	}
	p.mu.Unlock()

	return nil
}

// TerminateRunners terminates EC2 instances.
func (p *PoolProvider) TerminateRunners(ctx context.Context, runnerIDs []string) error {
	if len(runnerIDs) == 0 {
		return nil
	}

	_, err := p.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: runnerIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instances: %w", err)
	}

	p.mu.Lock()
	for _, id := range runnerIDs {
		delete(p.instanceIdle, id)
	}
	p.mu.Unlock()

	return nil
}

// MarkRunnerBusy marks a runner as busy (has an assigned job).
func (p *PoolProvider) MarkRunnerBusy(runnerID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.instanceIdle, runnerID)
}

// MarkRunnerIdle marks a runner as idle (no assigned job).
func (p *PoolProvider) MarkRunnerIdle(runnerID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.instanceIdle[runnerID] = time.Now()
}

// mapEC2PoolState maps EC2 instance state to pool runner state.
func mapEC2PoolState(state types.InstanceStateName) string {
	switch state {
	case types.InstanceStateNamePending:
		return StatePending
	case types.InstanceStateNameRunning:
		return StateRunning
	case types.InstanceStateNameStopping, types.InstanceStateNameStopped:
		return StateStopped
	default:
		return string(state)
	}
}

// Ensure PoolProvider implements provider.PoolProvider.
var _ provider.PoolProvider = (*PoolProvider)(nil)
