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
	CheckCircuit(ctx context.Context, instanceType string) (circuit.State, error)
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
	InstanceType  string   // Primary instance type (used if InstanceTypes is empty)
	InstanceTypes []string // Multiple instance types for spot diversification (Phase 10)
	SubnetID      string
	Spot          bool
	Pool          string
	ForceOnDemand bool   // Force on-demand even if spot is preferred (for retries)
	RetryCount    int    // Number of times this job has been retried
	Region        string // Target AWS region (Phase 3: Multi-region)
	Environment   string // Environment tag (Phase 6: Per-stack environments)
	OS            string // Operating system: linux, windows (Phase 4: Windows support)
	Arch          string // Architecture: x64, arm64
}

// CreateFleet launches EC2 instances using spot or on-demand capacity.
// When multiple instance types are specified and spot is enabled, EC2 Fleet
// will select from the pool with the best price-capacity balance.
func (m *Manager) CreateFleet(ctx context.Context, spec *LaunchSpec) ([]string, error) {
	// Select appropriate launch template based on OS
	launchTemplateName := m.selectLaunchTemplate(spec)

	launchTemplate := &types.FleetLaunchTemplateSpecificationRequest{
		LaunchTemplateName: aws.String(launchTemplateName),
		Version:            aws.String("$Latest"),
	}

	// Build overrides for instance type diversification
	overrides := m.buildInstanceOverrides(spec)

	targetCapacity := int32(1)

	tags := m.buildTags(spec)

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
		Type: types.FleetTypeInstant,
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         tags,
			},
		},
	}

	// Determine if we should use spot or on-demand
	useSpot := spec.Spot && m.config.SpotEnabled && !spec.ForceOnDemand

	// Check circuit breaker if using spot (check primary instance type)
	if useSpot && m.circuitBreaker != nil {
		primaryType := spec.InstanceType
		if len(spec.InstanceTypes) > 0 {
			primaryType = spec.InstanceTypes[0]
		}
		state, err := m.circuitBreaker.CheckCircuit(ctx, primaryType)
		if err != nil {
			log.Printf("Warning: failed to check circuit breaker: %v", err)
			// Continue with spot if we can't check
		} else if state == circuit.StateOpen {
			log.Printf("Circuit breaker OPEN for %s, forcing on-demand", primaryType)
			useSpot = false
		}
	}

	if !useSpot {
		req.TargetCapacitySpecification.DefaultTargetCapacityType = types.DefaultTargetCapacityTypeOnDemand
		if spec.ForceOnDemand && spec.RetryCount > 0 {
			log.Printf("Using on-demand for retry #%d of run %s", spec.RetryCount, spec.RunID)
		}
		// For on-demand, use only the primary instance type
		req.LaunchTemplateConfigs[0].Overrides = []types.FleetLaunchTemplateOverridesRequest{
			{
				InstanceType: types.InstanceType(m.getPrimaryInstanceType(spec)),
				SubnetId:     aws.String(spec.SubnetID),
			},
		}
	} else {
		req.SpotOptions = &types.SpotOptionsRequest{
			AllocationStrategy: types.SpotAllocationStrategyPriceCapacityOptimized,
		}
		if len(spec.InstanceTypes) > 1 {
			log.Printf("Using %d instance types for spot diversification: %v", len(overrides), m.getInstanceTypeNames(overrides))
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

// selectLaunchTemplate returns the appropriate launch template based on OS and architecture.
// spec must not be nil; enforced by caller (CreateFleet).
// Supported OS values: "linux", "windows", or "" (defaults to linux).
// Supported Arch values: "arm64", "x64", or "" (arch doesn't matter - uses non-suffixed template).
func (m *Manager) selectLaunchTemplate(spec *LaunchSpec) string {
	baseName := m.config.LaunchTemplateName
	if baseName == "" {
		baseName = "runs-fleet-runner"
	}

	// Windows instances use a separate launch template
	if spec.OS == "windows" {
		return baseName + "-windows"
	}

	// Validate OS - only linux (or empty, defaulting to linux) is supported beyond windows
	if spec.OS != "" && spec.OS != "linux" {
		log.Printf("Warning: unsupported OS %q, defaulting to linux", spec.OS)
	}

	// x64 Linux instances use a separate launch template (different AMI)
	if spec.Arch == "x64" {
		return baseName + "-x64"
	}

	// ARM64 Linux instances
	if spec.Arch == "arm64" {
		return baseName + "-arm64"
	}

	// Empty arch means "arch doesn't matter" - use non-suffixed template
	return baseName
}

// buildTags creates the tag set for the fleet resources.
func (m *Manager) buildTags(spec *LaunchSpec) []types.Tag {
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

	// Add OS tag for Windows instances
	if spec.OS != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:os"),
			Value: aws.String(spec.OS),
		})
	}

	// Add architecture tag
	if spec.Arch != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:arch"),
			Value: aws.String(spec.Arch),
		})
	}

	// Add region tag for multi-region support (Phase 3)
	if spec.Region != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:region"),
			Value: aws.String(spec.Region),
		})
	}

	// Add environment tag for per-stack environments (Phase 6)
	if spec.Environment != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:environment"),
			Value: aws.String(spec.Environment),
		})
		// Also add standard AWS Environment tag for cost tracking
		tags = append(tags, types.Tag{
			Key:   aws.String("Environment"),
			Value: aws.String(spec.Environment),
		})
	}

	return tags
}

// WindowsInstanceTypes returns the list of supported Windows instance types.
var WindowsInstanceTypes = map[string]bool{
	"t3.medium":   true,
	"t3.large":    true,
	"t3.xlarge":   true,
	"m6i.large":   true,
	"m6i.xlarge":  true,
	"m6i.2xlarge": true,
	"c6i.large":   true,
	"c6i.xlarge":  true,
	"c6i.2xlarge": true,
}

// IsValidWindowsInstanceType checks if an instance type supports Windows.
func IsValidWindowsInstanceType(instanceType string) bool {
	return WindowsInstanceTypes[instanceType]
}

// buildInstanceOverrides creates launch template overrides for instance type diversification.
// When multiple instance types are specified, this enables EC2 Fleet to select from a
// larger pool of instances, improving spot availability.
func (m *Manager) buildInstanceOverrides(spec *LaunchSpec) []types.FleetLaunchTemplateOverridesRequest {
	instanceTypes := spec.InstanceTypes
	if len(instanceTypes) == 0 {
		instanceTypes = []string{spec.InstanceType}
	}

	// Limit to 20 instance types (EC2 Fleet maximum per launch template config)
	const maxOverrides = 20
	if len(instanceTypes) > maxOverrides {
		instanceTypes = instanceTypes[:maxOverrides]
	}

	overrides := make([]types.FleetLaunchTemplateOverridesRequest, len(instanceTypes))
	for i, instType := range instanceTypes {
		overrides[i] = types.FleetLaunchTemplateOverridesRequest{
			InstanceType: types.InstanceType(instType),
			SubnetId:     aws.String(spec.SubnetID),
		}
	}

	return overrides
}

// getPrimaryInstanceType returns the primary instance type to use.
func (m *Manager) getPrimaryInstanceType(spec *LaunchSpec) string {
	if len(spec.InstanceTypes) > 0 {
		return spec.InstanceTypes[0]
	}
	return spec.InstanceType
}

// getInstanceTypeNames extracts instance type names from overrides for logging.
func (m *Manager) getInstanceTypeNames(overrides []types.FleetLaunchTemplateOverridesRequest) []string {
	names := make([]string, len(overrides))
	for i, o := range overrides {
		names[i] = string(o.InstanceType)
	}
	return names
}
