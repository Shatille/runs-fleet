// Package fleet manages EC2 fleet creation and lifecycle for runner instances.
package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/circuit"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

var fleetLog = logging.WithComponent(logging.LogTypeFleet, "manager")

//nolint:dupl // Mock struct in test file mirrors this interface - intentional pattern
// EC2API defines EC2 operations for fleet management.
type EC2API interface {
	CreateFleet(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error)
	CreateTags(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DeleteFleets(ctx context.Context, params *ec2.DeleteFleetsInput, optFns ...func(*ec2.Options)) (*ec2.DeleteFleetsOutput, error)
	DescribeFleetInstances(ctx context.Context, params *ec2.DescribeFleetInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeFleetInstancesOutput, error)
	DescribeSpotPriceHistory(ctx context.Context, params *ec2.DescribeSpotPriceHistoryInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error)
	DescribeInstanceTypeOfferings(ctx context.Context, params *ec2.DescribeInstanceTypeOfferingsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error)
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	RunInstances(ctx context.Context, params *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
}

// CircuitBreaker defines circuit breaker operations.
type CircuitBreaker interface {
	CheckCircuit(ctx context.Context, instanceType string) (circuit.State, error)
}

// spotPriceCache caches spot prices with TTL.
type spotPriceCache struct {
	mu      sync.RWMutex
	prices  map[string]float64
	expires time.Time
}

// availabilityCache caches available instance types in the region.
type availabilityCache struct {
	mu        sync.RWMutex
	available map[string]bool // instance type -> available
	expires   time.Time
	loaded    bool
}

const spotPriceCacheTTL = 5 * time.Minute
const availabilityCacheTTL = 24 * time.Hour
const archARM64 = "arm64"

// Manager orchestrates EC2 fleet creation for runner instances.
type Manager struct {
	ec2Client         EC2API
	config            *config.Config
	circuitBreaker    CircuitBreaker
	spotCache         spotPriceCache
	availabilityCache availabilityCache
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
	RunID         int64
	InstanceType  string   // Primary instance type (used if InstanceTypes is empty)
	InstanceTypes []string // Multiple instance types for spot diversification (Phase 10)
	SubnetID      string
	Spot          bool
	Pool          string
	Repo          string // Repository name for cost allocation (Role tag)
	ForceOnDemand bool   // Force on-demand even if spot is preferred (for retries)
	RetryCount    int    // Number of times this job has been retried
	Region        string // Target AWS region (Phase 3: Multi-region)
	Environment   string // Environment tag (Phase 6: Per-stack environments)
	OS            string // Operating system: linux, windows (Phase 4: Windows support)
	Arch          string // Architecture: amd64, arm64
	StorageGiB    int    // Disk storage in GiB (0 = use launch template default)
	Conditions    string // Resource conditions for instance naming (e.g., "arm64-cpu4-ram16")
}

// CreateFleet launches EC2 instances using spot or on-demand capacity via FleetTypeInstant.
// When multiple instance types are specified and spot is enabled, EC2 Fleet
// will select from the pool with the best price-capacity balance.
// When arch is empty and instance types span multiple architectures, multiple
// launch template configs are created to let EC2 Fleet choose the best option.
//
// For warm pool instances that need stop/start capability, use CreateOnDemandInstance instead.
func (m *Manager) CreateFleet(ctx context.Context, spec *LaunchSpec) ([]string, error) {
	useSpot := m.shouldUseSpot(ctx, spec)
	tags := m.buildTags(spec)

	req, err := m.buildFleetRequest(ctx, spec, useSpot, tags)
	if err != nil {
		return nil, err
	}

	output, err := m.ec2Client.CreateFleet(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create fleet: %w", err)
	}

	if err = m.checkFleetErrors(output); err != nil {
		return nil, err
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

// shouldUseSpot determines if spot instances should be used based on spec and circuit breaker.
func (m *Manager) shouldUseSpot(ctx context.Context, spec *LaunchSpec) bool {
	if !spec.Spot || !m.config.SpotEnabled || spec.ForceOnDemand {
		return false
	}

	if m.circuitBreaker == nil {
		return true
	}

	primaryType := m.getPrimaryInstanceType(spec)
	state, err := m.circuitBreaker.CheckCircuit(ctx, primaryType)
	if err != nil {
		fleetLog.Warn("circuit breaker check failed", slog.String("error", err.Error()))
		return true
	}
	if state == circuit.StateOpen {
		fleetLog.Info("circuit breaker open, forcing on-demand",
			slog.String("instance_type", primaryType))
		return false
	}
	return true
}

// buildFleetRequest constructs the CreateFleetInput based on spec and capacity type.
func (m *Manager) buildFleetRequest(ctx context.Context, spec *LaunchSpec, useSpot bool, tags []types.Tag) (*ec2.CreateFleetInput, error) {
	launchTemplateConfigs, err := m.buildLaunchTemplateConfigs(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("failed to build launch template configs: %w", err)
	}

	targetCapacity := int32(1)

	req := &ec2.CreateFleetInput{
		LaunchTemplateConfigs: launchTemplateConfigs,
		TargetCapacitySpecification: &types.TargetCapacitySpecificationRequest{
			TotalTargetCapacity:       &targetCapacity,
			DefaultTargetCapacityType: types.DefaultTargetCapacityTypeSpot,
		},
		Type: types.FleetTypeInstant,
		TagSpecifications: []types.TagSpecification{{
			ResourceType: types.ResourceTypeInstance,
			Tags:         tags,
		}},
	}

	if !useSpot {
		return m.configureOnDemandRequest(req, spec)
	}
	return m.configureSpotRequest(req, spec, launchTemplateConfigs)
}

// configureOnDemandRequest configures the fleet request for on-demand instances.
func (m *Manager) configureOnDemandRequest(req *ec2.CreateFleetInput, spec *LaunchSpec) (*ec2.CreateFleetInput, error) {
	req.TargetCapacitySpecification.DefaultTargetCapacityType = types.DefaultTargetCapacityTypeOnDemand
	if spec.ForceOnDemand && spec.RetryCount > 0 {
		fleetLog.Info("using on-demand for retry",
			slog.Int64(logging.KeyRunID, spec.RunID),
			slog.Int("retry_count", spec.RetryCount))
	}

	primaryType := m.getPrimaryInstanceType(spec)
	arch := spec.Arch
	if arch == "" {
		arch = GetInstanceArch(primaryType)
		if arch == "" {
			return nil, fmt.Errorf("cannot determine architecture for instance type %q: not in catalog", primaryType)
		}
	}

	override := types.FleetLaunchTemplateOverridesRequest{
		InstanceType: types.InstanceType(primaryType),
		SubnetId:     aws.String(spec.SubnetID),
	}
	if spec.StorageGiB > 0 {
		override.BlockDeviceMappings = []types.FleetBlockDeviceMappingRequest{
			{
				DeviceName: aws.String("/dev/xvda"),
				Ebs: &types.FleetEbsBlockDeviceRequest{
					VolumeSize:          aws.Int32(int32(spec.StorageGiB)),
					VolumeType:          types.VolumeTypeGp3,
					DeleteOnTermination: aws.Bool(true),
					Encrypted:           aws.Bool(true),
				},
			},
		}
	}
	req.LaunchTemplateConfigs = []types.FleetLaunchTemplateConfigRequest{
		{
			LaunchTemplateSpecification: &types.FleetLaunchTemplateSpecificationRequest{
				LaunchTemplateName: aws.String(m.getLaunchTemplateForArch(spec.OS, arch)),
				Version:            aws.String("$Latest"),
			},
			Overrides: []types.FleetLaunchTemplateOverridesRequest{override},
		},
	}
	return req, nil
}

// configureSpotRequest configures the fleet request for spot instances with FleetTypeInstant.
func (m *Manager) configureSpotRequest(req *ec2.CreateFleetInput, _ *LaunchSpec, launchTemplateConfigs []types.FleetLaunchTemplateConfigRequest) (*ec2.CreateFleetInput, error) {
	req.SpotOptions = &types.SpotOptionsRequest{
		AllocationStrategy: types.SpotAllocationStrategyPriceCapacityOptimized,
	}

	totalTypes := 0
	for _, cfg := range launchTemplateConfigs {
		totalTypes += len(cfg.Overrides)
	}
	if totalTypes > 1 {
		fleetLog.Debug("spot diversification enabled",
			slog.Int("instance_types", totalTypes),
			slog.Int("launch_configs", len(launchTemplateConfigs)))
	}
	return req, nil
}

// checkFleetErrors checks for errors in the fleet creation output.
func (m *Manager) checkFleetErrors(output *ec2.CreateFleetOutput) error {
	if len(output.Errors) == 0 {
		return nil
	}
	var errMsgs []string
	for _, e := range output.Errors {
		if e.ErrorMessage != nil {
			errMsgs = append(errMsgs, *e.ErrorMessage)
		} else {
			errMsgs = append(errMsgs, "unknown error")
		}
	}
	return fmt.Errorf("fleet creation had errors: %s", strings.Join(errMsgs, ", "))
}

// getLaunchTemplateForArch returns the launch template name for a specific OS and architecture.
// Supported OS values: "linux", "windows", or "" (defaults to linux).
// Supported Arch values: "arm64", "amd64", or "" (defaults to arm64).
func (m *Manager) getLaunchTemplateForArch(os, arch string) string {
	baseName := m.config.LaunchTemplateName
	if baseName == "" {
		baseName = "runs-fleet-runner"
	}

	// Windows instances use a separate launch template
	if os == "windows" {
		return baseName + "-windows"
	}

	// Validate OS - only linux (or empty, defaulting to linux) is supported beyond windows
	if os != "" && os != "linux" {
		fleetLog.Warn("unsupported OS, defaulting to linux", slog.String("os", os))
	}

	// amd64 Linux instances use a separate launch template (different AMI)
	if arch == "amd64" {
		return baseName + "-amd64"
	}

	// ARM64 Linux instances (default)
	return baseName + "-arm64"
}

// buildLaunchTemplateConfigs creates launch template configurations for the fleet.
// When arch is specified, creates a single config. When arch is empty and instance types
// span multiple architectures, queries spot prices and selects the cheapest architecture.
// Returns an error if instance types conflict with specified architecture or no valid types found.
func (m *Manager) buildLaunchTemplateConfigs(ctx context.Context, spec *LaunchSpec) ([]types.FleetLaunchTemplateConfigRequest, error) {
	instanceTypes := spec.InstanceTypes
	if len(instanceTypes) == 0 {
		instanceTypes = []string{spec.InstanceType}
	}

	// Filter out instance types not available in this region
	instanceTypes = m.filterAvailableInstanceTypes(ctx, instanceTypes)
	if len(instanceTypes) == 0 {
		return nil, fmt.Errorf("no instance types available in region after filtering")
	}

	// Limit total instance types (EC2 Fleet maximum per launch template config is 20)
	const maxOverridesPerConfig = 20

	// If arch is specified, validate instance types match and create a single config
	if spec.Arch != "" {
		for _, instType := range instanceTypes {
			arch := GetInstanceArch(instType)
			if arch != "" && arch != spec.Arch {
				return nil, fmt.Errorf("instance type %q (arch=%s) conflicts with specified arch=%s", instType, arch, spec.Arch)
			}
		}
		return []types.FleetLaunchTemplateConfigRequest{
			m.buildSingleArchConfig(spec.OS, spec.Arch, instanceTypes, spec.SubnetID, spec.StorageGiB, maxOverridesPerConfig),
		}, nil
	}

	// Arch is empty - group by architecture
	groupedTypes := GroupInstanceTypesByArch(instanceTypes)

	// If no valid instance types found, return error
	if len(groupedTypes) == 0 {
		return nil, fmt.Errorf("no valid instance types found in catalog: %v", instanceTypes)
	}

	// Select the cheapest architecture based on spot prices
	selectedArch := m.selectCheapestArch(ctx, groupedTypes)
	if selectedArch == "" {
		return nil, fmt.Errorf("failed to select architecture for instance types: %v", instanceTypes)
	}

	return []types.FleetLaunchTemplateConfigRequest{
		m.buildSingleArchConfig(spec.OS, selectedArch, groupedTypes[selectedArch], spec.SubnetID, spec.StorageGiB, maxOverridesPerConfig),
	}, nil
}

// buildSingleArchConfig creates a single launch template config for one architecture.
// If storageGiB > 0, adds block device mappings to override root volume size.
func (m *Manager) buildSingleArchConfig(os, arch string, instanceTypes []string, subnetID string, storageGiB, maxOverrides int) types.FleetLaunchTemplateConfigRequest {
	if len(instanceTypes) > maxOverrides {
		instanceTypes = instanceTypes[:maxOverrides]
	}

	overrides := make([]types.FleetLaunchTemplateOverridesRequest, len(instanceTypes))
	for i, instType := range instanceTypes {
		override := types.FleetLaunchTemplateOverridesRequest{
			InstanceType: types.InstanceType(instType),
			SubnetId:     aws.String(subnetID),
		}

		// Add custom storage if specified (overrides launch template default)
		if storageGiB > 0 {
			override.BlockDeviceMappings = []types.FleetBlockDeviceMappingRequest{
				{
					DeviceName: aws.String("/dev/xvda"),
					Ebs: &types.FleetEbsBlockDeviceRequest{
						VolumeSize:          aws.Int32(int32(storageGiB)),
						VolumeType:          types.VolumeTypeGp3,
						DeleteOnTermination: aws.Bool(true),
						Encrypted:           aws.Bool(true),
					},
				},
			}
		}

		overrides[i] = override
	}

	return types.FleetLaunchTemplateConfigRequest{
		LaunchTemplateSpecification: &types.FleetLaunchTemplateSpecificationRequest{
			LaunchTemplateName: aws.String(m.getLaunchTemplateForArch(os, arch)),
			Version:            aws.String("$Latest"),
		},
		Overrides: overrides,
	}
}

// buildTags creates the tag set for the fleet resources.
func (m *Manager) buildTags(spec *LaunchSpec) []types.Tag {
	name := buildInstanceName(spec.Pool, spec.Repo, spec.Conditions)


	tags := []types.Tag{
		{
			Key:   aws.String("Name"),
			Value: aws.String(name),
		},
		{
			Key:   aws.String("runs-fleet:run-id"),
			Value: aws.String(fmt.Sprintf("%d", spec.RunID)),
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

	// Add Role tag for cost allocation by repository
	if spec.Repo != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("Role"),
			Value: aws.String(spec.Repo),
		})
	}

	// Add container config tags for bootstrap script
	if m.config.RunnerImage != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:runner-image"),
			Value: aws.String(m.config.RunnerImage),
		})
	}
	if m.config.TerminationQueueURL != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:termination-queue-url"),
			Value: aws.String(m.config.TerminationQueueURL),
		})
	}
	if m.config.CacheURL != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:cache-url"),
			Value: aws.String(m.config.CacheURL),
		})
	}

	// Add secrets backend configuration for runner agent
	if m.config.SecretsBackend != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:secrets-backend"),
			Value: aws.String(m.config.SecretsBackend),
		})
	}
	if m.config.SecretsBackend == "vault" {
		if m.config.VaultAddr != "" {
			tags = append(tags, types.Tag{
				Key:   aws.String("runs-fleet:vault-addr"),
				Value: aws.String(m.config.VaultAddr),
			})
		}
		if m.config.VaultKVMount != "" {
			tags = append(tags, types.Tag{
				Key:   aws.String("runs-fleet:vault-kv-mount"),
				Value: aws.String(m.config.VaultKVMount),
			})
		}
		if m.config.VaultKVVersion != 0 {
			tags = append(tags, types.Tag{
				Key:   aws.String("runs-fleet:vault-kv-version"),
				Value: aws.String(fmt.Sprintf("%d", m.config.VaultKVVersion)),
			})
		}
		if m.config.VaultBasePath != "" {
			tags = append(tags, types.Tag{
				Key:   aws.String("runs-fleet:vault-base-path"),
				Value: aws.String(m.config.VaultBasePath),
			})
		}
		// EC2 runners always use AWS IAM auth, not the orchestrator's auth method
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:vault-auth-method"),
			Value: aws.String("aws"),
		})
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:vault-aws-role"),
			Value: aws.String("runs-fleet-runner"),
		})
	}

	// Add custom tags from configuration
	for key, value := range m.config.Tags {
		tags = append(tags, types.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
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

// getPrimaryInstanceType returns the primary instance type to use.
func (m *Manager) getPrimaryInstanceType(spec *LaunchSpec) string {
	if len(spec.InstanceTypes) > 0 {
		return spec.InstanceTypes[0]
	}
	return spec.InstanceType
}

// selectCheapestArch queries spot prices and returns the architecture with lower average price.
// Returns empty string if prices can't be determined (caller should fall back to default behavior).
func (m *Manager) selectCheapestArch(ctx context.Context, groupedTypes map[string][]string) string {
	if len(groupedTypes) <= 1 {
		// Only one architecture, return it
		for arch := range groupedTypes {
			return arch
		}
		return ""
	}

	archPrices := make(map[string]float64)
	for arch, instanceTypes := range groupedTypes {
		avgPrice := m.getAverageSpotPrice(ctx, instanceTypes)
		if avgPrice > 0 {
			archPrices[arch] = avgPrice
		}
	}

	if len(archPrices) == 0 {
		// Couldn't get prices, default to arm64 (generally cheaper)
		if _, ok := groupedTypes[archARM64]; ok {
			fleetLog.Info("spot prices unavailable, defaulting to arm64")
			return archARM64
		}
		for arch := range groupedTypes {
			return arch
		}
		return ""
	}

	// Find cheapest architecture
	var cheapestArch string
	var cheapestPrice float64
	for arch, price := range archPrices {
		if cheapestArch == "" || price < cheapestPrice {
			cheapestArch = arch
			cheapestPrice = price
		}
	}

	fleetLog.Debug("architecture selected based on spot price",
		slog.String("arch", cheapestArch),
		slog.Float64("avg_price", cheapestPrice))
	return cheapestArch
}

// getAverageSpotPrice returns the average current spot price for instance types.
// Uses a cache with TTL to avoid rate limiting.
func (m *Manager) getAverageSpotPrice(ctx context.Context, instanceTypes []string) float64 {
	if len(instanceTypes) == 0 {
		return 0
	}

	// Check cache first
	m.spotCache.mu.RLock()
	if time.Now().Before(m.spotCache.expires) && len(m.spotCache.prices) > 0 {
		var total float64
		var count int
		for _, instType := range instanceTypes {
			if price, ok := m.spotCache.prices[instType]; ok {
				total += price
				count++
			}
		}
		m.spotCache.mu.RUnlock()
		if count > 0 {
			return total / float64(count)
		}
	} else {
		m.spotCache.mu.RUnlock()
	}

	// Cache miss or expired - fetch from API
	return m.fetchAndCacheSpotPrices(ctx, instanceTypes)
}

// fetchAndCacheSpotPrices fetches spot prices from AWS and updates the cache.
func (m *Manager) fetchAndCacheSpotPrices(ctx context.Context, instanceTypes []string) float64 {
	// Limit to first 10 instance types to avoid API throttling
	queryTypes := instanceTypes
	if len(queryTypes) > 10 {
		queryTypes = queryTypes[:10]
	}

	ec2Types := make([]types.InstanceType, len(queryTypes))
	for i, t := range queryTypes {
		ec2Types[i] = types.InstanceType(t)
	}

	output, err := m.ec2Client.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       ec2Types,
		ProductDescriptions: []string{"Linux/UNIX"},
		MaxResults:          aws.Int32(100),
	})
	if err != nil {
		fleetLog.Warn("spot price query failed", slog.String("error", err.Error()))
		return 0
	}

	if len(output.SpotPriceHistory) == 0 {
		return 0
	}

	// Calculate average price (most recent price per instance type)
	latestPrices := make(map[string]float64)
	for _, sp := range output.SpotPriceHistory {
		instType := string(sp.InstanceType)
		if _, seen := latestPrices[instType]; !seen {
			var price float64
			if sp.SpotPrice != nil {
				if _, err := fmt.Sscanf(*sp.SpotPrice, "%f", &price); err != nil {
					continue
				}
			}
			if price > 0 {
				latestPrices[instType] = price
			}
		}
	}

	if len(latestPrices) == 0 {
		return 0
	}

	// Update cache
	m.spotCache.mu.Lock()
	if m.spotCache.prices == nil {
		m.spotCache.prices = make(map[string]float64)
	}
	for instType, price := range latestPrices {
		m.spotCache.prices[instType] = price
	}
	m.spotCache.expires = time.Now().Add(spotPriceCacheTTL)
	m.spotCache.mu.Unlock()

	var total float64
	for _, price := range latestPrices {
		total += price
	}
	return total / float64(len(latestPrices))
}

// RankInstanceTypesByPrice returns an interleaved sequence weighted by inverse spot price.
// Cheaper types appear more frequently. Caller randomly selects from the pool.
// Falls back to original order when no price data is available.
func (m *Manager) RankInstanceTypesByPrice(ctx context.Context, instanceTypes []string) []string {
	if len(instanceTypes) <= 1 {
		return instanceTypes
	}

	m.spotCache.mu.RLock()
	cacheValid := time.Now().Before(m.spotCache.expires) && len(m.spotCache.prices) > 0
	m.spotCache.mu.RUnlock()

	if !cacheValid {
		m.fetchAndCacheSpotPrices(ctx, instanceTypes)
	}

	m.spotCache.mu.RLock()
	defer m.spotCache.mu.RUnlock()

	var items []weightedType
	var maxPrice float64
	for _, t := range instanceTypes {
		p, ok := m.spotCache.prices[t]
		if !ok {
			items = append(items, weightedType{typ: t, price: 0})
			continue
		}
		items = append(items, weightedType{typ: t, price: p})
		if p > maxPrice {
			maxPrice = p
		}
	}

	if maxPrice == 0 {
		return instanceTypes
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].price == 0 && items[j].price == 0 {
			return false
		}
		if items[i].price == 0 {
			return false
		}
		if items[j].price == 0 {
			return true
		}
		return items[i].price < items[j].price
	})

	const maxWeight = 5
	for i := range items {
		if items[i].price == 0 {
			items[i].weight = 1
			continue
		}
		w := int(math.Round(maxPrice / items[i].price))
		if w < 1 {
			w = 1
		}
		if w > maxWeight {
			w = maxWeight
		}
		items[i].weight = w
	}

	return interleaveWeighted(items)
}

type weightedType struct {
	typ    string
	price  float64
	weight int
}

func interleaveWeighted(items []weightedType) []string {
	remaining := make([]int, len(items))
	var total int
	for i, it := range items {
		remaining[i] = it.weight
		total += it.weight
	}

	seq := make([]string, 0, total)
	for len(seq) < total {
		for i, it := range items {
			if remaining[i] > 0 {
				seq = append(seq, it.typ)
				remaining[i]--
			}
		}
	}
	return seq
}

// filterAvailableInstanceTypes filters instance types to only those available in the region.
// Uses cached availability data, loading from AWS API on first call.
func (m *Manager) filterAvailableInstanceTypes(ctx context.Context, instanceTypes []string) []string {
	if err := m.ensureAvailabilityLoaded(ctx); err != nil {
		fleetLog.Warn("failed to load instance type availability, using all types",
			slog.String("error", err.Error()))
		return instanceTypes
	}

	m.availabilityCache.mu.RLock()
	defer m.availabilityCache.mu.RUnlock()

	var filtered []string
	for _, instType := range instanceTypes {
		if m.availabilityCache.available[instType] {
			filtered = append(filtered, instType)
		}
	}

	if len(filtered) < len(instanceTypes) {
		fleetLog.Debug("filtered unavailable instance types",
			slog.Int("original", len(instanceTypes)),
			slog.Int("available", len(filtered)))
	}

	return filtered
}

// ensureAvailabilityLoaded loads available instance types from AWS if not already cached or expired.
func (m *Manager) ensureAvailabilityLoaded(ctx context.Context) error {
	m.availabilityCache.mu.RLock()
	if m.availabilityCache.loaded && time.Now().Before(m.availabilityCache.expires) {
		m.availabilityCache.mu.RUnlock()
		return nil
	}
	m.availabilityCache.mu.RUnlock()

	m.availabilityCache.mu.Lock()
	defer m.availabilityCache.mu.Unlock()

	// Double-check after acquiring write lock
	if m.availabilityCache.loaded && time.Now().Before(m.availabilityCache.expires) {
		return nil
	}

	available := make(map[string]bool)
	var nextToken *string

	for {
		output, err := m.ec2Client.DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{
			LocationType: types.LocationTypeRegion,
			MaxResults:   aws.Int32(1000),
			NextToken:    nextToken,
		})
		if err != nil {
			return fmt.Errorf("describe instance type offerings: %w", err)
		}

		for _, offering := range output.InstanceTypeOfferings {
			available[string(offering.InstanceType)] = true
		}

		if output.NextToken == nil {
			break
		}
		nextToken = output.NextToken
	}

	m.availabilityCache.available = available
	m.availabilityCache.loaded = true
	m.availabilityCache.expires = time.Now().Add(availabilityCacheTTL)
	fleetLog.Info("loaded instance type availability",
		slog.Int("available_types", len(available)))

	return nil
}

// CreateOnDemandInstance launches a single on-demand instance using RunInstances API.
// Used for warm pool instances where stop/start reliability matters more than spot savings.
func (m *Manager) CreateOnDemandInstance(ctx context.Context, spec *LaunchSpec) (string, error) {
	tags := m.buildTags(spec)

	arch := spec.Arch
	if arch == "" {
		arch = GetInstanceArch(spec.InstanceType)
		if arch == "" {
			return "", fmt.Errorf("cannot determine architecture for instance type %q", spec.InstanceType)
		}
	}
	launchTemplateName := m.getLaunchTemplateForArch(spec.OS, arch)

	input := &ec2.RunInstancesInput{
		LaunchTemplate: &types.LaunchTemplateSpecification{
			LaunchTemplateName: aws.String(launchTemplateName),
			Version:            aws.String("$Latest"),
		},
		InstanceType: types.InstanceType(spec.InstanceType),
		SubnetId:     aws.String(spec.SubnetID),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         tags,
			},
		},
	}

	if spec.StorageGiB > 0 {
		input.BlockDeviceMappings = []types.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/xvda"),
				Ebs: &types.EbsBlockDevice{
					VolumeSize:          aws.Int32(int32(spec.StorageGiB)),
					VolumeType:          types.VolumeTypeGp3,
					DeleteOnTermination: aws.Bool(true),
					Encrypted:           aws.Bool(true),
				},
			},
		}
	}

	output, err := m.ec2Client.RunInstances(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to run instance: %w", err)
	}

	if len(output.Instances) == 0 {
		return "", fmt.Errorf("no instance created")
	}

	instanceID := aws.ToString(output.Instances[0].InstanceId)

	fleetLog.Info("on-demand pool instance created",
		slog.String(logging.KeyInstanceID, instanceID),
		slog.String("instance_type", spec.InstanceType),
		slog.String("pool", spec.Pool))

	return instanceID, nil
}

const instanceNameMaxLen = 64

func buildInstanceName(pool, repo, conditions string) string {
	const prefix = "runs-fleet-runner-"

	var name string
	if pool != "" {
		name = prefix + pool
	} else if repo != "" {
		repoName := repo
		if idx := strings.LastIndex(repo, "/"); idx >= 0 {
			repoName = repo[idx+1:]
		}
		name = prefix + repoName
		if conditions != "" {
			name += "-" + conditions
		}
	} else {
		return "runs-fleet-runner"
	}

	if len(name) > instanceNameMaxLen {
		name = name[:instanceNameMaxLen]
	}
	return name
}
