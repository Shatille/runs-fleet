// Package fleet manages EC2 fleet creation and lifecycle for runner instances.
package fleet

import (
	"context"
	"errors"
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
	"github.com/Shavakan/runs-fleet/pkg/tracing"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var fleetLog = logging.WithComponent(logging.LogTypeFleet, "manager")

// EC2API defines EC2 operations for fleet management.
//
//nolint:dupl // Mock struct in test file mirrors this interface - intentional pattern
type EC2API interface {
	CreateFleet(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error)
	CreateTags(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DeleteFleets(ctx context.Context, params *ec2.DeleteFleetsInput, optFns ...func(*ec2.Options)) (*ec2.DeleteFleetsOutput, error)
	DescribeFleetInstances(ctx context.Context, params *ec2.DescribeFleetInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeFleetInstancesOutput, error)
	DescribeSpotPriceHistory(ctx context.Context, params *ec2.DescribeSpotPriceHistoryInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error)
	DescribeInstanceTypeOfferings(ctx context.Context, params *ec2.DescribeInstanceTypeOfferingsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error)
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeSubnets(ctx context.Context, params *ec2.DescribeSubnetsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	RunInstances(ctx context.Context, params *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
}

// CircuitBreaker defines circuit breaker operations.
type CircuitBreaker interface {
	CheckCircuit(ctx context.Context, instanceType string) (circuit.State, error)
}

// MetricsAPI publishes fleet creation metrics.
type MetricsAPI interface {
	PublishFleetCreate(ctx context.Context, capacity, result string) error
	PublishFleetCreateSeconds(ctx context.Context, capacity string, seconds float64) error
}

// spotPriceCache caches spot prices with TTL.
type spotPriceCache struct {
	mu      sync.RWMutex
	fetchMu sync.Mutex // serializes SpotPrice fetches so concurrent callers don't issue redundant API calls
	prices  map[string]float64
	checked map[string]bool // types looked up this window that returned no price
	expires time.Time
}

// availabilityCache caches available instance types in the region.
type availabilityCache struct {
	mu        sync.RWMutex
	available map[string]bool // instance type -> available
	expires   time.Time
	loaded    bool
}

// subnetAZCache caches the AZ each subnet lives in. Subnet-to-AZ is static
// configuration, so a successfully resolved subnet is never re-queried.
type subnetAZCache struct {
	mu sync.Mutex
	az map[string]string // subnet ID -> availability zone
}

const spotPriceCacheTTL = 5 * time.Minute
const availabilityCacheTTL = 24 * time.Hour
const archARM64 = "arm64"

// maxFleetOverrides caps the total launch template overrides emitted across all
// configs in a single CreateFleet request. The overrides are an
// (instance type x subnet) matrix, so this bounds types x AZs. AWS allows up to
// 300 overrides for instant fleets; this conservative cap keeps the request
// small while leaving room to span several AZs with diversified types.
const maxFleetOverrides = 60

// Manager orchestrates EC2 fleet creation for runner instances.
type Manager struct {
	ec2Client         EC2API
	config            *config.Config
	circuitBreaker    CircuitBreaker
	metrics           MetricsAPI
	spotCache         spotPriceCache
	availabilityCache availabilityCache
	subnetAZCache     subnetAZCache
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

// SetMetrics sets the metrics publisher for the manager.
func (m *Manager) SetMetrics(metrics MetricsAPI) {
	m.metrics = metrics
}

// Capacity labels for the fleet_create metrics. CreateFleet requests either spot
// or on-demand capacity; the label set is therefore fixed and low-cardinality.
const (
	capacitySpot     = "spot"
	capacityOnDemand = "on_demand"
)

// fleetCapacityLabel maps the resolved spot decision to the capacity metric
// label so the counter and the latency histogram share the same dimension.
func fleetCapacityLabel(useSpot bool) string {
	if useSpot {
		return capacitySpot
	}
	return capacityOnDemand
}

// LaunchSpec defines EC2 instance launch parameters for workflow job.
type LaunchSpec struct {
	RunID         int64
	InstanceType  string   // Primary instance type (used if InstanceTypes is empty)
	InstanceTypes []string // Multiple instance types for spot diversification (Phase 10)
	SubnetID      string   // Single subnet; used as the fallback when SubnetIDs is empty
	SubnetIDs     []string // All configured subnets; overrides span every AZ when non-empty
	Spot          bool
	Pool          string
	Repo          string // Repository name for cost allocation (Role tag)
	ForceOnDemand bool   // Force on-demand even if spot is preferred (for retries)
	RetryCount    int    // Number of times this job has been retried
	Arch          string // Architecture: amd64, arm64
	StorageGiB    int    // Disk storage in GiB (0 = use launch template default)
	Conditions    string // Resource conditions for instance naming (e.g., "arm64-cpu4-ram16")
	Reason        string // Why this instance is being created (e.g., "ready_deficit", "stopped_replenish")
}

// CreateFleet launches EC2 instances using spot or on-demand capacity via FleetTypeInstant.
// When multiple instance types are specified and spot is enabled, EC2 Fleet
// will select from the pool with the best price-capacity balance.
// When arch is empty and instance types span multiple architectures, multiple
// launch template configs are created to let EC2 Fleet choose the best option.
//
// For warm pool instances that need stop/start capability, use CreateOnDemandInstance instead.
func (m *Manager) CreateFleet(ctx context.Context, spec *LaunchSpec) ([]string, error) {
	instanceTypes := spec.InstanceTypes
	if len(instanceTypes) == 0 && spec.InstanceType != "" {
		instanceTypes = []string{spec.InstanceType}
	}
	ctx, span := tracing.Tracer().Start(ctx, "fleet.create",
		trace.WithAttributes(
			attribute.StringSlice("fleet.instance_types", instanceTypes),
			attribute.Bool("fleet.spot", spec.Spot && !spec.ForceOnDemand),
			attribute.Int("fleet.target_capacity", 1),
		))
	defer span.End()

	useSpot := m.shouldUseSpot(ctx, spec)
	capacity := fleetCapacityLabel(useSpot)
	tags := m.buildTags(spec)

	req, err := m.buildFleetRequest(ctx, spec, useSpot, tags)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	// Track the fleet-creation outcome for the fleet_create counter. The result
	// is "success" only when instances are returned; every error exit below sets
	// it to "failure" before returning.
	result := "failure"
	defer func() {
		if m.metrics != nil {
			if mErr := m.metrics.PublishFleetCreate(ctx, capacity, result); mErr != nil {
				fleetLog.Warn(ctx, "fleet create metric failed", slog.String("error", mErr.Error()))
			}
		}
	}()

	// Time only the EC2 CreateFleet call; this is the latency the operator cares
	// about (the rest of this function is local request building).
	createStart := time.Now()
	output, err := m.ec2Client.CreateFleet(ctx, req)
	if m.metrics != nil {
		if mErr := m.metrics.PublishFleetCreateSeconds(ctx, capacity, time.Since(createStart).Seconds()); mErr != nil {
			fleetLog.Warn(ctx, "fleet create latency metric failed", slog.String("error", mErr.Error()))
		}
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("failed to create fleet: %w", err)
	}

	if err = m.checkFleetErrors(output); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	if len(output.Instances) == 0 {
		noInstancesErr := fmt.Errorf("no instances were created")
		span.RecordError(noInstancesErr)
		span.SetStatus(codes.Error, noInstancesErr.Error())
		return nil, noInstancesErr
	}

	result = "success"

	instanceIDs := make([]string, 0, len(output.Instances))
	for _, inst := range output.Instances {
		instanceIDs = append(instanceIDs, inst.InstanceIds...)
	}

	// Ensure tags are applied even if CreateFleet TagSpecifications fails to propagate
	// (observed with spot instances). Without the runs-fleet:managed tag, IAM policies
	// block housekeeping from terminating orphaned instances.
	if _, tagErr := m.ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: instanceIDs,
		Tags:      tags,
	}); tagErr != nil {
		fleetLog.Warn(ctx, "failed to apply tags to fleet instances",
			slog.String("error", tagErr.Error()),
			slog.Any("instance_ids", instanceIDs))
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
		fleetLog.Warn(ctx, "circuit breaker check failed", slog.String("error", err.Error()))
		return true
	}
	if state == circuit.StateOpen {
		fleetLog.Info(ctx, "circuit breaker open, forcing on-demand",
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
		return m.configureOnDemandRequest(ctx, req, spec)
	}
	return m.configureSpotRequest(ctx, req, spec, launchTemplateConfigs)
}

// configureOnDemandRequest configures the fleet request for on-demand instances.
func (m *Manager) configureOnDemandRequest(ctx context.Context, req *ec2.CreateFleetInput, spec *LaunchSpec) (*ec2.CreateFleetInput, error) {
	req.TargetCapacitySpecification.DefaultTargetCapacityType = types.DefaultTargetCapacityTypeOnDemand
	if spec.ForceOnDemand && spec.RetryCount > 0 {
		fleetLog.Info(ctx, "using on-demand for retry",
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

	// Span every configured AZ with the primary instance type so a single-AZ
	// capacity shortfall does not fail the whole request.
	req.LaunchTemplateConfigs = []types.FleetLaunchTemplateConfigRequest{
		m.buildSingleArchConfig(arch, []string{primaryType}, m.resolveSubnets(ctx, spec), spec.StorageGiB),
	}
	return req, nil
}

// configureSpotRequest configures the fleet request for spot instances with FleetTypeInstant.
func (m *Manager) configureSpotRequest(ctx context.Context, req *ec2.CreateFleetInput, _ *LaunchSpec, launchTemplateConfigs []types.FleetLaunchTemplateConfigRequest) (*ec2.CreateFleetInput, error) {
	req.SpotOptions = &types.SpotOptionsRequest{
		AllocationStrategy: types.SpotAllocationStrategyPriceCapacityOptimized,
	}

	totalTypes := 0
	for _, cfg := range launchTemplateConfigs {
		totalTypes += len(cfg.Overrides)
	}
	if totalTypes > 1 {
		fleetLog.Debug(ctx, "spot diversification enabled",
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

// getLaunchTemplateForArch returns the launch template name for a specific architecture.
// Supported Arch values: "arm64", "amd64", or "" (defaults to arm64).
func (m *Manager) getLaunchTemplateForArch(arch string) string {
	baseName := m.config.LaunchTemplateName
	if baseName == "" {
		baseName = "runs-fleet-runner"
	}

	// amd64 instances use a separate launch template (different AMI)
	if arch == "amd64" {
		return baseName + "-amd64"
	}

	// ARM64 instances (default)
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

	subnets := m.resolveSubnets(ctx, spec)

	// If arch is specified, validate instance types match and create a single config
	if spec.Arch != "" {
		for _, instType := range instanceTypes {
			arch := GetInstanceArch(instType)
			if arch != "" && arch != spec.Arch {
				return nil, fmt.Errorf("instance type %q (arch=%s) conflicts with specified arch=%s", instType, arch, spec.Arch)
			}
		}
		return []types.FleetLaunchTemplateConfigRequest{
			m.buildSingleArchConfig(spec.Arch, instanceTypes, subnets, spec.StorageGiB),
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
		m.buildSingleArchConfig(selectedArch, groupedTypes[selectedArch], subnets, spec.StorageGiB),
	}, nil
}

// resolveSubnets returns the subnets a fleet request should span. When the spec
// carries the full configured list it is collapsed to one subnet per AZ so the
// CreateFleet (instance type x subnet) override matrix never produces duplicate
// (type, AZ) instance pools, which EC2 rejects with InvalidFleetConfig. With
// only a single SubnetID it falls back to that subnet. A nil result (no subnet
// information at all) lets EC2 pick a default subnet.
func (m *Manager) resolveSubnets(ctx context.Context, spec *LaunchSpec) []string {
	if len(spec.SubnetIDs) > 0 {
		return m.subnetsOnePerAZ(ctx, spec.SubnetIDs)
	}
	if spec.SubnetID != "" {
		return []string{spec.SubnetID}
	}
	return nil
}

// subnetsOnePerAZ collapses subnets to at most one per Availability Zone,
// preserving input order and keeping the first subnet seen for each AZ. EC2
// keys an instance pool by (instance type, AZ) rather than by subnet, so two
// subnets in the same AZ would produce duplicate pools in a CreateFleet
// override matrix. If AZ resolution fails the raw list is returned unchanged so
// fleet creation still proceeds (CreateFleet's own retry handles transient
// faults); correctness is preferred whenever AZ data is available.
func (m *Manager) subnetsOnePerAZ(ctx context.Context, subnets []string) []string {
	if len(subnets) <= 1 {
		return subnets
	}

	azBySubnet, err := m.resolveSubnetAZs(ctx, subnets)
	if err != nil {
		fleetLog.Warn(ctx, "failed to resolve subnet availability zones, using all subnets",
			slog.Int("subnets", len(subnets)),
			slog.String("error", err.Error()))
		return subnets
	}

	seenAZ := make(map[string]bool, len(subnets))
	deduped := make([]string, 0, len(subnets))
	for _, subnet := range subnets {
		az, ok := azBySubnet[subnet]
		if !ok {
			// AZ unknown for this subnet: keep it rather than silently drop it.
			deduped = append(deduped, subnet)
			continue
		}
		if seenAZ[az] {
			continue
		}
		seenAZ[az] = true
		deduped = append(deduped, subnet)
	}

	if len(deduped) < len(subnets) {
		fleetLog.Debug(ctx, "collapsed subnets to one per AZ",
			slog.Int("configured", len(subnets)),
			slog.Int("per_az", len(deduped)))
	}
	return deduped
}

// resolveSubnetAZs returns the AZ for each requested subnet, querying
// DescribeSubnets only for subnets not already cached. Subnet-to-AZ is static,
// so resolved entries are cached for the lifetime of the manager. The cache
// lock is never held across the DescribeSubnets call: it is taken briefly to
// read the missing set and again to merge the result, so concurrent fleet
// requests do not serialize on a network round-trip.
func (m *Manager) resolveSubnetAZs(ctx context.Context, subnets []string) (map[string]string, error) {
	missing := m.uncachedSubnets(subnets)

	if len(missing) > 0 {
		output, err := m.ec2Client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
			SubnetIds: missing,
		})
		if err != nil {
			return nil, fmt.Errorf("describe subnets: %w", err)
		}
		m.cacheSubnetAZs(output.Subnets)
	}

	return m.cachedSubnetAZs(subnets), nil
}

// uncachedSubnets returns the subset of subnets whose AZ is not yet cached.
func (m *Manager) uncachedSubnets(subnets []string) []string {
	m.subnetAZCache.mu.Lock()
	defer m.subnetAZCache.mu.Unlock()

	var missing []string
	for _, subnet := range subnets {
		if _, ok := m.subnetAZCache.az[subnet]; !ok {
			missing = append(missing, subnet)
		}
	}
	return missing
}

// cacheSubnetAZs records the AZ of each described subnet in the cache.
func (m *Manager) cacheSubnetAZs(described []types.Subnet) {
	m.subnetAZCache.mu.Lock()
	defer m.subnetAZCache.mu.Unlock()

	if m.subnetAZCache.az == nil {
		m.subnetAZCache.az = make(map[string]string)
	}
	for _, s := range described {
		id := aws.ToString(s.SubnetId)
		az := aws.ToString(s.AvailabilityZone)
		if id != "" && az != "" {
			m.subnetAZCache.az[id] = az
		}
	}
}

// cachedSubnetAZs returns the cached AZ for each subnet that has one.
func (m *Manager) cachedSubnetAZs(subnets []string) map[string]string {
	m.subnetAZCache.mu.Lock()
	defer m.subnetAZCache.mu.Unlock()

	result := make(map[string]string, len(subnets))
	for _, subnet := range subnets {
		if az, ok := m.subnetAZCache.az[subnet]; ok {
			result[subnet] = az
		}
	}
	return result
}

// typesPerSubnet returns how many instance types to place in each subnet so the
// total override count stays within maxFleetOverrides while keeping every subnet
// represented. AZ coverage is the priority: each subnet always gets at least one
// type, trimming types-per-subnet rather than dropping a subnet.
func typesPerSubnet(numTypes, numSubnets int) int {
	if numSubnets <= 0 || numTypes <= 0 {
		return 0
	}
	per := maxFleetOverrides / numSubnets
	if per < 1 {
		per = 1
	}
	if per > numTypes {
		per = numTypes
	}
	return per
}

// buildSingleArchConfig creates a single launch template config for one architecture.
// Overrides are the cross-product of (instance types) x (subnets) so a single
// CreateFleet request spans every configured AZ. The total override count is
// bounded by maxFleetOverrides; when (types x subnets) would exceed it, the
// types-per-subnet count is trimmed so every subnet still appears at least once.
// If storageGiB > 0, adds block device mappings to override root volume size.
func (m *Manager) buildSingleArchConfig(arch string, instanceTypes, subnets []string, storageGiB int) types.FleetLaunchTemplateConfigRequest {
	if len(subnets) == 0 {
		subnets = []string{""}
	}

	perSubnet := typesPerSubnet(len(instanceTypes), len(subnets))

	overrides := make([]types.FleetLaunchTemplateOverridesRequest, 0, len(subnets)*perSubnet)
	for _, subnet := range subnets {
		for i := 0; i < perSubnet; i++ {
			override := types.FleetLaunchTemplateOverridesRequest{
				InstanceType: types.InstanceType(instanceTypes[i]),
			}
			if subnet != "" {
				override.SubnetId = aws.String(subnet)
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

			overrides = append(overrides, override)
		}
	}

	return types.FleetLaunchTemplateConfigRequest{
		LaunchTemplateSpecification: &types.FleetLaunchTemplateSpecificationRequest{
			LaunchTemplateName: aws.String(m.getLaunchTemplateForArch(arch)),
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

	// Add architecture tag
	if spec.Arch != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("runs-fleet:arch"),
			Value: aws.String(spec.Arch),
		})
	}

	// Add Role tag for cost allocation by repository
	if spec.Repo != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("Role"),
			Value: aws.String(spec.Repo),
		})
	}

	// Cost-attribution tags. Values are fixed; only the key names are
	// configurable so a fork with a different tag policy can remap them. Empty
	// config keys fall back to the defaults so runner instances are always tagged.
	tags = append(tags,
		types.Tag{
			Key:   aws.String(m.applicationTagKey()),
			Value: aws.String("runs-fleet"),
		},
		types.Tag{
			Key:   aws.String(m.serviceTagKey()),
			Value: aws.String("runner"),
		},
	)

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

// applicationTagKey returns the tag key for the fixed "runs-fleet" application
// value, defaulting to "Application" when not configured.
func (m *Manager) applicationTagKey() string {
	if m.config.TagKeyApplication != "" {
		return m.config.TagKeyApplication
	}
	return "Application"
}

// serviceTagKey returns the tag key for the fixed "runner" service value,
// defaulting to "Service" when not configured.
func (m *Manager) serviceTagKey() string {
	if m.config.TagKeyService != "" {
		return m.config.TagKeyService
	}
	return "Service"
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
			fleetLog.Info(ctx, "spot prices unavailable, defaulting to arm64")
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

	fleetLog.Debug(ctx, "architecture selected based on spot price",
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
	avg, _ := m.fetchAndCacheSpotPrices(ctx, instanceTypes)
	return avg
}

// SpotPrice returns the current market spot price for a single instance type,
// reusing the 5-minute cache. On a miss it queries once and negatively caches a
// no-price result for the rest of the window, so a type with no spot history
// isn't re-queried on every call. The second return is false when no price is
// available (query failed or no recent spot history), so callers can fall back
// to an estimate.
func (m *Manager) SpotPrice(ctx context.Context, instanceType string) (float64, bool) {
	if price, ok, resolved := m.spotPriceFromCache(instanceType); resolved {
		return price, ok
	}

	// Serialize the fetch path so concurrent callers don't issue redundant API
	// calls for the same type. fetchMu is held across the fetch, but the cache
	// RWMutex is NOT — fetchAndCacheSpotPrices acquires it internally, and
	// readers stay unblocked during the API call.
	m.spotCache.fetchMu.Lock()
	defer m.spotCache.fetchMu.Unlock()

	// Re-check: another caller may have populated/negatively-marked this type
	// while we waited for fetchMu.
	if price, ok, resolved := m.spotPriceFromCache(instanceType); resolved {
		return price, ok
	}

	_, queried := m.fetchAndCacheSpotPrices(ctx, []string{instanceType})

	m.spotCache.mu.Lock()
	defer m.spotCache.mu.Unlock()
	price, ok := m.spotCache.prices[instanceType]
	// Negatively cache only a confirmed absence (the query succeeded but returned
	// no price). A transient fetch failure is left uncached so the next call
	// retries within the TTL window rather than serving fallback for the full TTL.
	if !ok && queried {
		if m.spotCache.checked == nil {
			m.spotCache.checked = make(map[string]bool)
		}
		m.spotCache.checked[instanceType] = true
	}
	return price, ok
}

// spotPriceFromCache reads a single type from the cache. resolved is true only
// when the window is valid and the type is either priced (ok=true) or
// negatively cached as having no price (ok=false); resolved is false when a
// fetch is needed. Negative marks are honored only within the valid window —
// fetchAndCacheSpotPrices clears them when a fresh window opens.
func (m *Manager) spotPriceFromCache(instanceType string) (price float64, ok, resolved bool) {
	m.spotCache.mu.RLock()
	defer m.spotCache.mu.RUnlock()
	if !time.Now().Before(m.spotCache.expires) {
		return 0, false, false
	}
	if p, found := m.spotCache.prices[instanceType]; found {
		return p, true, true
	}
	if m.spotCache.checked[instanceType] {
		return 0, false, true
	}
	return 0, false, false
}

// fetchAndCacheSpotPrices fetches spot prices from AWS and updates the cache.
// The bool reports whether the query itself succeeded (no API error), so callers
// can distinguish "confirmed no price" from "transient fetch failure" — only the
// former should be negatively cached.
func (m *Manager) fetchAndCacheSpotPrices(ctx context.Context, instanceTypes []string) (float64, bool) {
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
		fleetLog.Warn(ctx, "spot price query failed", slog.String("error", err.Error()))
		return 0, false
	}

	if len(output.SpotPriceHistory) == 0 {
		return 0, true
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
		return 0, true
	}

	// Update cache
	m.spotCache.mu.Lock()
	// A fresh window (the prior one had expired) drops stale negative marks so a
	// type absent in an old window is re-checked at most once per TTL.
	if !time.Now().Before(m.spotCache.expires) {
		m.spotCache.checked = nil
	}
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
	return total / float64(len(latestPrices)), true
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
		fleetLog.Warn(ctx, "failed to load instance type availability, using all types",
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
		fleetLog.Debug(ctx, "filtered unavailable instance types",
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
	fleetLog.Info(ctx, "loaded instance type availability",
		slog.Int("available_types", len(available)))

	return nil
}

// capacityErrorCodes are the EC2 RunInstances error codes that signal the
// requested instance type cannot be launched in the targeted AZ right now but
// might succeed in another AZ. Rotating to the next configured subnet on these
// is worthwhile; any other error is a hard failure (bad config, auth) where a
// retry in a different AZ would only waste attempts.
var capacityErrorCodes = map[string]bool{
	"InsufficientInstanceCapacity":          true,
	"InsufficientCapacity":                  true,
	"InsufficientReservedInstancesCapacity": true,
	"Unsupported":                           true, // instance type not offered in this AZ
}

// isCapacityError reports whether err is an AZ-local capacity/availability
// failure that may be resolved by trying a different subnet. It checks the
// smithy APIError code first and falls back to a message substring match for
// errors that don't surface a typed code.
func isCapacityError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && capacityErrorCodes[apiErr.ErrorCode()] {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "insufficient capacity")
}

// CreateOnDemandInstance launches a single on-demand instance using RunInstances API.
// Used for warm pool instances where stop/start reliability matters more than spot savings.
//
// RunInstances targets a single subnet, so when spec.SubnetIDs is populated the
// call is retried across those subnets in order: a capacity-class failure in one
// AZ rotates to the next, while any non-capacity error fails fast without burning
// the remaining attempts. The subnet list is first collapsed to one per AZ so a
// retry always lands in a distinct AZ. With an empty SubnetIDs it behaves as a
// single RunInstances against spec.SubnetID.
func (m *Manager) CreateOnDemandInstance(ctx context.Context, spec *LaunchSpec) (string, error) {
	tags := m.buildTags(spec)

	arch := spec.Arch
	if arch == "" {
		arch = GetInstanceArch(spec.InstanceType)
		if arch == "" {
			return "", fmt.Errorf("cannot determine architecture for instance type %q", spec.InstanceType)
		}
	}
	launchTemplateName := m.getLaunchTemplateForArch(arch)

	subnets := spec.SubnetIDs
	if len(subnets) == 0 {
		subnets = []string{spec.SubnetID}
	} else {
		// RunInstances targets one subnet per call and rotates AZs on capacity
		// errors; collapsing to one subnet per AZ avoids burning a retry on a
		// second same-AZ subnet that shares the same capacity pool.
		subnets = m.subnetsOnePerAZ(ctx, subnets)
	}

	for i, subnet := range subnets {
		input := &ec2.RunInstancesInput{
			LaunchTemplate: &types.LaunchTemplateSpecification{
				LaunchTemplateName: aws.String(launchTemplateName),
				Version:            aws.String("$Latest"),
			},
			InstanceType: types.InstanceType(spec.InstanceType),
			SubnetId:     aws.String(subnet),
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
			// Only a capacity-class failure with another AZ left is worth
			// rotating; anything else (auth, bad config, capacity in the last
			// subnet) is terminal and surfaced immediately.
			if isCapacityError(err) && i < len(subnets)-1 {
				fleetLog.Info(ctx, "on-demand pool instance capacity exhausted, trying next AZ",
					slog.String("instance_type", spec.InstanceType),
					slog.String("pool", spec.Pool),
					slog.String("subnet", subnet),
					slog.String("error", err.Error()))
				continue
			}
			return "", fmt.Errorf("failed to run instance: %w", err)
		}

		if len(output.Instances) == 0 {
			return "", fmt.Errorf("no instance created")
		}

		instanceID := aws.ToString(output.Instances[0].InstanceId)

		fleetLog.Info(ctx, "on-demand pool instance created",
			slog.String(logging.KeyInstanceID, instanceID),
			slog.String("instance_type", spec.InstanceType),
			slog.String("pool", spec.Pool),
			slog.String("subnet", subnet),
			slog.String("reason", spec.Reason))

		return instanceID, nil
	}

	// Unreachable: the loop returns on the final subnet (its capacity error is
	// not eligible for rotation), but the compiler needs a terminal return.
	return "", fmt.Errorf("no instance created")
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
