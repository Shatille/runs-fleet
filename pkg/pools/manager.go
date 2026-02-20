// Package pools manages warm pool operations for maintaining pre-provisioned EC2 instances.
package pools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/google/uuid"
)

var poolLog = logging.WithComponent(logging.LogTypePool, "ec2-manager")

// Instance state constants
const (
	stateRunning = "running"
	stateStopped = "stopped"
)

//nolint:dupl // Mock struct in test file mirrors this interface - intentional pattern
// DBClient defines DynamoDB operations for pool configuration.
type DBClient interface {
	GetPoolConfig(ctx context.Context, poolName string) (*db.PoolConfig, error)
	UpdatePoolState(ctx context.Context, poolName string, running, stopped int) error
	ListPools(ctx context.Context) ([]string, error)
	GetPoolP90Concurrency(ctx context.Context, poolName string, windowHours int) (int, error)
	GetPoolBusyInstanceIDs(ctx context.Context, poolName string) ([]string, error)
	AcquirePoolReconcileLock(ctx context.Context, poolName, owner string, ttl time.Duration) error
	ReleasePoolReconcileLock(ctx context.Context, poolName, owner string) error
	ClaimInstanceForJob(ctx context.Context, instanceID string, jobID int64, ttl time.Duration) error
	ReleaseInstanceClaim(ctx context.Context, instanceID string, jobID int64) error
	SaveSpotRequestID(ctx context.Context, instanceID, spotRequestID string, persistent bool) error
	GetSpotRequestIDs(ctx context.Context, instanceIDs []string) (map[string]db.SpotRequestInfo, error)
}

// FleetAPI defines EC2 fleet operations for instance provisioning.
type FleetAPI interface {
	CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)
	CreateSpotInstance(ctx context.Context, spec *fleet.LaunchSpec) (instanceID, spotRequestID string, err error)
	GetSpotRequestIDForInstance(ctx context.Context, instanceID string) (string, error)
	GetSpotRequestIDsForInstances(ctx context.Context, instanceIDs []string) (map[string]string, error)
	RankInstanceTypesByPrice(ctx context.Context, instanceTypes []string) []string
}

//nolint:dupl // Mock struct in test file mirrors this interface - intentional pattern
// EC2API defines EC2 operations for instance management.
type EC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StartInstances(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	CancelSpotInstanceRequests(ctx context.Context, params *ec2.CancelSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error)
	CreateTags(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

// PoolInstance represents an EC2 instance in a pool.
type PoolInstance struct {
	InstanceID   string
	State        string
	LaunchTime   time.Time
	InstanceType string
	IdleSince    time.Time // When the instance became idle (no job assigned)

	// Spec from InstanceCatalog lookup (populated for multi-spec pool matching)
	CPU    int
	RAM    float64
	Arch   string
	Family string
	Gen    int
}

// matchesFlexibleSpec checks if this instance matches the given flexible spec requirements.
// Returns true if spec is nil (legacy behavior: any instance matches).
// Returns false if instance type is not in InstanceCatalog (CPU=0) when spec is provided.
func (p PoolInstance) matchesFlexibleSpec(spec *fleet.FlexibleSpec) bool {
	if spec == nil {
		return true
	}

	// Instance type must be in catalog for spec matching
	// (instances not in catalog have CPU=0 and would fail any CPUMin check)
	if p.CPU == 0 {
		return false
	}

	instSpec := fleet.InstanceSpec{
		Type:   p.InstanceType,
		CPU:    p.CPU,
		RAM:    p.RAM,
		Arch:   p.Arch,
		Family: p.Family,
		Gen:    p.Gen,
	}
	return instSpec.MatchesFlexibleSpec(*spec)
}

// Manager orchestrates warm pool operations and instance assignment.
type Manager struct {
	mu            sync.RWMutex
	dbClient      DBClient
	fleetManager  FleetAPI
	ec2Client     EC2API
	config        *config.Config
	instanceID    string               // Unique identifier for this instance (for distributed locking)
	instanceIdle  map[string]time.Time // Tracks when instances became idle
	poolInstances map[string][]string  // Cache of instance IDs per pool
	subnetIndex   uint64
	randIntn      func(int) int
}

// NewManager creates pool manager with DB and fleet clients.
// Generates a unique instance ID for distributed locking.
func NewManager(dbClient DBClient, fleetManager FleetAPI, cfg *config.Config) *Manager {
	return &Manager{
		dbClient:      dbClient,
		fleetManager:  fleetManager,
		config:        cfg,
		instanceID:    uuid.New().String(),
		instanceIdle:  make(map[string]time.Time),
		poolInstances: make(map[string][]string),
		randIntn:      rand.IntN,
	}
}

// SetEC2Client sets the EC2 client for instance management.
func (m *Manager) SetEC2Client(ec2Client EC2API) {
	m.ec2Client = ec2Client
}

// selectSubnet returns the next subnet ID in round-robin fashion,
// prioritizing private subnets to avoid public IPv4 costs.
func (m *Manager) selectSubnet() string {
	var subnets []string
	if len(m.config.PrivateSubnetIDs) > 0 {
		subnets = m.config.PrivateSubnetIDs
	} else {
		subnets = m.config.PublicSubnetIDs
	}
	if len(subnets) == 0 {
		return ""
	}
	idx := atomic.AddUint64(&m.subnetIndex, 1) - 1
	return subnets[idx%uint64(len(subnets))]
}

// ReconcileLoop runs periodically to maintain pool size.
func (m *Manager) ReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	// Initial reconciliation
	m.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcile(ctx)
		}
	}
}

func (m *Manager) reconcile(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ec2Client == nil {
		return
	}

	pools, err := m.dbClient.ListPools(ctx)
	if err != nil {
		poolLog.Error("list pools failed", slog.String("error", err.Error()))
		return
	}

	for _, poolName := range pools {
		if err := m.reconcilePool(ctx, poolName); err != nil {
			poolLog.Error("pool reconciliation failed",
				slog.String(logging.KeyPoolName, poolName),
				slog.String("error", err.Error()))
		}
	}
}

//nolint:gocyclo // Core reconciliation logic with multiple scale up/down paths
func (m *Manager) reconcilePool(ctx context.Context, poolName string) error {
	// Acquire per-pool lock (65s TTL > 60s reconcile interval)
	if err := m.dbClient.AcquirePoolReconcileLock(ctx, poolName, m.instanceID, 65*time.Second); err != nil {
		// Lock held by another instance or pool deleted - skip silently
		if errors.Is(err, db.ErrPoolReconcileLockHeld) || errors.Is(err, db.ErrPoolNotFound) {
			return nil
		}
		return fmt.Errorf("failed to acquire pool lock: %w", err)
	}
	defer func() {
		if err := m.dbClient.ReleasePoolReconcileLock(ctx, poolName, m.instanceID); err != nil {
			poolLog.Error("pool lock release failed",
				slog.String(logging.KeyPoolName, poolName),
				slog.String("error", err.Error()))
		}
	}()

	poolConfig, err := m.dbClient.GetPoolConfig(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get pool config: %w", err)
	}
	if poolConfig == nil {
		return fmt.Errorf("pool config not found")
	}

	desiredRunning, desiredStopped := m.getScheduledDesiredCounts(poolConfig)

	// For ephemeral pools, override with auto-scaled values based on peak concurrency
	// Ephemeral pools scale stopped instances (not running) for on-demand starts
	if ephRunning, ephStopped, ok := m.getEphemeralAutoScaledCount(ctx, poolName, poolConfig); ok {
		desiredRunning = ephRunning
		desiredStopped = ephStopped
	}

	instances, err := m.getPoolInstances(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get pool instances: %w", err)
	}

	busyIDs, err := m.dbClient.GetPoolBusyInstanceIDs(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get busy instance IDs: %w", err)
	}

	running, stopped, _, _ := countInstanceStates(instances, busyIDs)

	// Busy count = running instances that have jobs assigned (instance-job intersection).
	// Only count actual running instances with matching job records - orphaned job records
	// (from terminated instances) must NOT inflate busy count or they block scale-down.
	runningInstances := m.filterByState(instances, stateRunning)
	busy := len(filterMatchingInstances(runningInstances, busyIDs))
	ready := running - busy

	// Track changes for logging
	var started, stoppedCount, created, terminated int

	// Scale based on ready count, not total running
	// desiredRunning represents desired READY instances (idle, available for jobs)
	if ready < desiredRunning {
		deficit := desiredRunning - ready

		stoppedInstances := m.filterByState(instances, stateStopped)
		toStart := min(deficit, len(stoppedInstances))
		if toStart > 0 {
			instanceIDs := make([]string, toStart)
			for i := 0; i < toStart; i++ {
				instanceIDs[i] = stoppedInstances[i].InstanceID
			}
			if err := m.startInstances(ctx, instanceIDs); err != nil {
				poolLog.Error("instances start failed", slog.String("error", err.Error()))
			} else {
				started += toStart
				deficit -= toStart
			}
		}

		if deficit > 0 {
			created += m.createPoolFleetInstances(ctx, poolName, deficit, poolConfig)
		}
	} else if ready > desiredRunning {
		// Too many ready (idle) instances - stop or terminate excess
		// Only scale down idle instances, never busy ones
		excess := ready - desiredRunning
		runningInstances := m.filterByState(instances, stateRunning)

		// For warm pools (desiredRunning=0), stop instances immediately
		// For hot pools, only stop instances that exceeded idle timeout
		var candidateInstances []PoolInstance
		if desiredRunning == 0 && desiredStopped > 0 {
			// Warm pool: all ready instances are candidates for stopping
			// Reuse busyIDs from earlier query (line would have returned early on error)
			candidateInstances = m.filterReadyInstances(runningInstances, busyIDs)
		} else {
			// Hot pool: only idle instances that exceeded timeout
			candidateInstances = m.filterIdleInstances(runningInstances, poolConfig.IdleTimeoutMinutes)
		}

		toStop := min(excess, len(candidateInstances))
		if toStop > 0 && stopped < desiredStopped {
			canStop := min(toStop, desiredStopped-stopped)
			instanceIDs := make([]string, canStop)
			for i := 0; i < canStop; i++ {
				instanceIDs[i] = candidateInstances[i].InstanceID
			}
			if err := m.stopInstances(ctx, instanceIDs); err != nil {
				poolLog.Error("instances stop failed", slog.String("error", err.Error()))
			} else {
				stoppedCount += canStop
			}
			excess -= canStop
		}

		if excess > 0 {
			// Sort by launch time (oldest first)
			toTerminate := min(excess, len(candidateInstances))
			if toTerminate > 0 {
				instanceIDs := make([]string, toTerminate)
				for i := 0; i < toTerminate; i++ {
					instanceIDs[i] = candidateInstances[i].InstanceID
				}
				if err := m.terminateInstances(ctx, instanceIDs); err != nil {
					poolLog.Error("instances terminate failed", slog.String("error", err.Error()))
				} else {
					terminated += toTerminate
				}
			}
		}
	}

	if stopped > desiredStopped {
		excess := stopped - desiredStopped
		stoppedInstances := m.filterByState(instances, stateStopped)
		toTerminate := min(excess, len(stoppedInstances))
		if toTerminate > 0 {
			instanceIDs := make([]string, toTerminate)
			for i := 0; i < toTerminate; i++ {
				instanceIDs[i] = stoppedInstances[i].InstanceID
			}
			if err := m.terminateInstances(ctx, instanceIDs); err != nil {
				poolLog.Error("stopped instances terminate failed", slog.String("error", err.Error()))
			} else {
				terminated += toTerminate
			}
		}
	} else if stopped < desiredStopped && desiredRunning == 0 {
		// Need more stopped instances for warm/ephemeral pools (desiredRunning=0, desiredStopped>0)
		deficit := desiredStopped - stopped
		created += m.createPoolFleetInstances(ctx, poolName, deficit, poolConfig)
	}

	// Log reconciliation only when changes occurred
	if started+stoppedCount+created+terminated > 0 {
		poolLog.Info("pool reconciled",
			slog.String(logging.KeyPoolName, poolName),
			slog.Int("desired_running", desiredRunning),
			slog.Int("desired_stopped", desiredStopped),
			slog.Int("running", running),
			slog.Int("stopped", stopped),
			slog.Int("ready", ready),
			slog.Int("busy", busy),
			slog.Int("started", started),
			slog.Int("stopped_count", stoppedCount),
			slog.Int("created", created),
			slog.Int("terminated", terminated))
	}

	if err := m.dbClient.UpdatePoolState(ctx, poolName, running, stopped); err != nil {
		poolLog.Error("pool state update failed", slog.String("error", err.Error()))
	}

	return nil
}

// ephemeralScaleDownWindow is the time window for keeping at least one instance
// after the last job. This prevents premature scale-to-zero when jobs are infrequent
// but the pool is still actively used.
const ephemeralScaleDownWindow = 4 * time.Hour

// getEphemeralAutoScaledCount returns the auto-scaled desired counts for ephemeral pools.
// Ephemeral pools scale stopped instances (not running) based on peak concurrency,
// since jobs claim and start stopped instances on-demand via ClaimAndStartPoolInstance.
// Returns (desiredRunning, desiredStopped, true) if scaling was applied, or (0, 0, false) for non-ephemeral pools.
func (m *Manager) getEphemeralAutoScaledCount(ctx context.Context, poolName string, poolConfig *db.PoolConfig) (int, int, bool) {
	if !poolConfig.Ephemeral {
		return 0, 0, false
	}

	// Check recent activity first (fast path, no query needed)
	// This also ensures minimum capacity is maintained if peak query fails
	var recentlyActive bool
	var sinceLastJob time.Duration
	if !poolConfig.LastJobTime.IsZero() {
		sinceLastJob = time.Since(poolConfig.LastJobTime)
		recentlyActive = sinceLastJob < ephemeralScaleDownWindow
	}

	p90, err := m.dbClient.GetPoolP90Concurrency(ctx, poolName, 1) // 1 hour window
	if err != nil {
		poolLog.Error("p90 concurrency query failed",
			slog.String(logging.KeyPoolName, poolName),
			slog.String("error", err.Error()))
		// If recently active, keep minimum capacity despite query failure
		if recentlyActive {
			poolLog.Debug("ephemeral pool keeping minimum capacity (query failed)",
				slog.String(logging.KeyPoolName, poolName),
				slog.Time("last_job_time", poolConfig.LastJobTime),
				slog.Duration("since_last_job", sinceLastJob))
			return 0, 1, true
		}
		return 0, poolConfig.DesiredStopped, true // Ephemeral pools always have desiredRunning=0
	}

	if p90 > 0 {
		desired := max(1, p90)
		poolLog.Info("ephemeral pool auto-scaling",
			slog.String(logging.KeyPoolName, poolName),
			slog.Int("p90_concurrency", p90),
			slog.Int("desired_stopped", desired))
		return 0, desired, true
	}

	// P90 is 0, but check if pool was recently active
	if recentlyActive {
		poolLog.Debug("ephemeral pool keeping minimum capacity",
			slog.String(logging.KeyPoolName, poolName),
			slog.Time("last_job_time", poolConfig.LastJobTime),
			slog.Duration("since_last_job", sinceLastJob))
		return 0, 1, true
	}

	return 0, poolConfig.DesiredStopped, true // Ephemeral pools always have desiredRunning=0
}

// getScheduledDesiredCounts returns the desired running and stopped counts based on schedule.
func (m *Manager) getScheduledDesiredCounts(poolConfig *db.PoolConfig) (running, stopped int) {
	if len(poolConfig.Schedules) == 0 {
		return poolConfig.DesiredRunning, poolConfig.DesiredStopped
	}

	now := time.Now()
	currentHour := now.Hour()
	currentDay := now.Weekday()

	// Find matching schedule
	for _, schedule := range poolConfig.Schedules {
		if m.scheduleMatches(schedule, currentHour, currentDay) {
			return schedule.DesiredRunning, schedule.DesiredStopped
		}
	}

	// Fall back to default
	return poolConfig.DesiredRunning, poolConfig.DesiredStopped
}

// scheduleMatches checks if a schedule applies to the current time.
func (m *Manager) scheduleMatches(schedule db.PoolSchedule, hour int, day time.Weekday) bool {
	if len(schedule.DaysOfWeek) > 0 {
		dayMatch := false
		for _, d := range schedule.DaysOfWeek {
			if d == int(day) {
				dayMatch = true
				break
			}
		}
		if !dayMatch {
			return false
		}
	}

	if schedule.StartHour <= schedule.EndHour {
		// Normal range (e.g., 9-17)
		return hour >= schedule.StartHour && hour < schedule.EndHour
	}
	// Overnight range (e.g., 22-6)
	return hour >= schedule.StartHour || hour < schedule.EndHour
}

func (m *Manager) getPoolInstances(ctx context.Context, poolName string) ([]PoolInstance, error) {
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

	output, err := m.ec2Client.DescribeInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %w", err)
	}

	var instances []PoolInstance
	for _, reservation := range output.Reservations {
		for _, inst := range reservation.Instances {
			instanceType := string(inst.InstanceType)
			instance := PoolInstance{
				InstanceID:   aws.ToString(inst.InstanceId),
				State:        string(inst.State.Name),
				InstanceType: instanceType,
			}
			if inst.LaunchTime != nil {
				instance.LaunchTime = *inst.LaunchTime
			}

			// Populate spec fields from InstanceCatalog for multi-spec pool matching
			if spec, ok := fleet.GetInstanceSpec(instanceType); ok {
				instance.CPU = spec.CPU
				instance.RAM = spec.RAM
				instance.Arch = spec.Arch
				instance.Family = spec.Family
				instance.Gen = spec.Gen
			} else {
				poolLog.Warn("instance type not in catalog, spec matching may fail",
					slog.String(logging.KeyInstanceID, instance.InstanceID),
					slog.String("instance_type", instanceType))
			}

			// Check idle tracking
			if idleSince, ok := m.instanceIdle[instance.InstanceID]; ok {
				instance.IdleSince = idleSince
			} else if instance.State == stateRunning {
				// Initialize idle tracking for new running instances
				m.instanceIdle[instance.InstanceID] = time.Now()
				instance.IdleSince = time.Now()
			}

			instances = append(instances, instance)
		}
	}

	return instances, nil
}

func (m *Manager) filterByState(instances []PoolInstance, state string) []PoolInstance {
	var filtered []PoolInstance
	for _, inst := range instances {
		if inst.State == state {
			filtered = append(filtered, inst)
		}
	}
	return filtered
}

func (m *Manager) filterIdleInstances(instances []PoolInstance, idleTimeoutMinutes int) []PoolInstance {
	if idleTimeoutMinutes <= 0 {
		idleTimeoutMinutes = 10 // Default 10 minutes
	}

	threshold := time.Now().Add(-time.Duration(idleTimeoutMinutes) * time.Minute)
	var idle []PoolInstance
	for _, inst := range instances {
		if !inst.IdleSince.IsZero() && inst.IdleSince.Before(threshold) {
			idle = append(idle, inst)
		}
	}
	return idle
}

// filterReadyInstances returns running instances that are not busy (no running job).
// Used for warm pools where we want to stop instances immediately without idle timeout.
func (m *Manager) filterReadyInstances(instances []PoolInstance, busyInstanceIDs []string) []PoolInstance {
	busySet := make(map[string]struct{}, len(busyInstanceIDs))
	for _, id := range busyInstanceIDs {
		busySet[id] = struct{}{}
	}

	var ready []PoolInstance
	for _, inst := range instances {
		if inst.State != stateRunning {
			continue
		}
		if _, isBusy := busySet[inst.InstanceID]; !isBusy {
			ready = append(ready, inst)
		}
	}
	return ready
}

// filterMatchingInstances returns instances whose IDs are in the given list.
func filterMatchingInstances(instances []PoolInstance, instanceIDs []string) []PoolInstance {
	idSet := make(map[string]struct{}, len(instanceIDs))
	for _, id := range instanceIDs {
		idSet[id] = struct{}{}
	}

	var matched []PoolInstance
	for _, inst := range instances {
		if _, ok := idSet[inst.InstanceID]; ok {
			matched = append(matched, inst)
		}
	}
	return matched
}

func (m *Manager) startInstances(ctx context.Context, instanceIDs []string) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	_, err := m.ec2Client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to start instances: %w", err)
	}

	poolLog.Info("instances started",
		slog.Int(logging.KeyCount, len(instanceIDs)),
		slog.Any("instance_ids", instanceIDs))
	return nil
}

func (m *Manager) stopInstances(ctx context.Context, instanceIDs []string) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	_, err := m.ec2Client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to stop instances: %w", err)
	}

	// Remove from idle tracking
	for _, id := range instanceIDs {
		delete(m.instanceIdle, id)
	}

	poolLog.Info("instances stopped",
		slog.Int(logging.KeyCount, len(instanceIDs)),
		slog.Any("instance_ids", instanceIDs))
	return nil
}

func (m *Manager) terminateInstances(ctx context.Context, instanceIDs []string) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	_, err := m.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instances: %w", err)
	}

	// Cancel persistent spot requests AFTER successful termination to avoid orphaning
	// If cancellation fails, housekeeping will clean up orphaned spot requests later
	if m.dbClient != nil {
		m.cancelSpotRequestsForInstances(ctx, instanceIDs)
	}

	// Remove from idle tracking
	for _, id := range instanceIDs {
		delete(m.instanceIdle, id)
	}

	poolLog.Info("instances terminated",
		slog.Int(logging.KeyCount, len(instanceIDs)),
		slog.Any("instance_ids", instanceIDs))
	return nil
}

// cancelSpotRequestsForInstances cancels persistent spot requests for the given instances.
// Logs warnings on failure but does not return errors (best-effort cleanup).
func (m *Manager) cancelSpotRequestsForInstances(ctx context.Context, instanceIDs []string) {
	spotRequests, err := m.dbClient.GetSpotRequestIDs(ctx, instanceIDs)
	if err != nil {
		poolLog.Warn("spot request lookup failed",
			slog.String("error", err.Error()))
		return
	}

	if len(spotRequests) == 0 {
		return
	}

	var spotRequestIDs []string
	for _, info := range spotRequests {
		if info.SpotRequestID != "" && info.Persistent {
			spotRequestIDs = append(spotRequestIDs, info.SpotRequestID)
		}
	}

	if len(spotRequestIDs) == 0 {
		return
	}

	_, err = m.ec2Client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: spotRequestIDs,
	})
	if err != nil {
		poolLog.Warn("spot request cancellation failed",
			slog.Int("count", len(spotRequestIDs)),
			slog.String("error", err.Error()))
		return
	}

	poolLog.Info("spot requests cancelled",
		slog.Int("count", len(spotRequestIDs)))
}

// MarkInstanceBusy marks an instance as busy (has an assigned job).
func (m *Manager) MarkInstanceBusy(instanceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.instanceIdle, instanceID)
}

// MarkInstanceIdle marks an instance as idle (no assigned job).
func (m *Manager) MarkInstanceIdle(instanceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instanceIdle[instanceID] = time.Now()
}

// ErrNoAvailableInstance is returned when no instance is available in the pool.
var ErrNoAvailableInstance = errors.New("no available instance in pool")

// AvailableInstance represents an instance that can be assigned to a job.
type AvailableInstance struct {
	InstanceID   string
	InstanceType string
	State        string // "stopped" or "running"
}

// GetAvailableInstance finds a stopped instance in the pool that can be started for a job.
// Returns ErrNoAvailableInstance if no stopped instances are available.
// Running idle instances are not returned - they require agent changes for reuse.
// Note: This method only finds an instance. Use ClaimAndStartPoolInstance for atomic claim+start.
func (m *Manager) GetAvailableInstance(ctx context.Context, poolName string) (*AvailableInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.getAvailableInstanceLocked(ctx, poolName)
}

// getAvailableInstanceLocked finds an available instance while already holding the lock.
func (m *Manager) getAvailableInstanceLocked(ctx context.Context, poolName string) (*AvailableInstance, error) {
	if m.ec2Client == nil {
		return nil, fmt.Errorf("EC2 client not configured")
	}

	instances, err := m.getPoolInstances(ctx, poolName)
	if err != nil {
		return nil, fmt.Errorf("failed to get pool instances: %w", err)
	}

	// Find stopped instances (can be started with new config)
	for _, inst := range instances {
		if inst.State == stateStopped {
			return &AvailableInstance{
				InstanceID:   inst.InstanceID,
				InstanceType: inst.InstanceType,
				State:        inst.State,
			}, nil
		}
	}

	return nil, ErrNoAvailableInstance
}

// StartInstanceForJob starts a stopped pool instance for a job.
// The caller must have already updated the instance's runner config in SSM.
// The repo parameter is used to tag the instance with the Role tag for cost allocation.
// Returns the instance ID on success.
// Note: Prefer ClaimAndStartPoolInstance for atomic claim+start to avoid race conditions.
func (m *Manager) StartInstanceForJob(ctx context.Context, instanceID, repo string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.startInstanceForJobLocked(ctx, instanceID, repo)
}

// startInstanceForJobLocked starts an instance while already holding the lock.
// If repo is non-empty, the instance is tagged with the Role tag for cost allocation.
func (m *Manager) startInstanceForJobLocked(ctx context.Context, instanceID, repo string) error {
	if m.ec2Client == nil {
		return fmt.Errorf("EC2 client not configured")
	}

	_, err := m.ec2Client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("failed to start instance %s: %w", instanceID, err)
	}

	// Tag the instance with Role for cost allocation
	// Pool instances are created without this tag since the repo is unknown at creation time
	if repo != "" {
		_, err := m.ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{instanceID},
			Tags: []types.Tag{
				{
					Key:   aws.String("Role"),
					Value: aws.String(repo),
				},
			},
		})
		if err != nil {
			// Log but don't fail - tagging is for cost allocation, not critical path
			poolLog.Warn("failed to tag instance with Role",
				slog.String(logging.KeyInstanceID, instanceID),
				slog.String("repo", repo),
				slog.String("error", err.Error()))
		}
	}

	// Mark as busy immediately to prevent reconciler from touching it
	delete(m.instanceIdle, instanceID)

	poolLog.Info("pool instance started for job",
		slog.String(logging.KeyInstanceID, instanceID))
	return nil
}

// instanceClaimTTL is the TTL for instance claims in DynamoDB.
// Set to 5 minutes to allow for EC2 start + SSM config + agent boot.
const instanceClaimTTL = 5 * time.Minute

// ClaimAndStartPoolInstance atomically finds, claims, and starts a stopped pool instance.
// Returns the claimed instance on success, ErrNoAvailableInstance if no instance available.
//
// The jobID parameter is used for distributed locking via DynamoDB conditional writes.
// This prevents race conditions in multi-orchestrator deployments where multiple
// orchestrators might try to claim the same stopped instance for different jobs.
//
// The repo parameter is used to tag the instance with the Role tag for cost allocation.
// Pool instances are created without knowing which repo will use them, so the tag is
// applied when the instance is assigned to a job.
//
// The spec parameter is used for multi-spec pool matching. If non-nil, only instances
// that satisfy the spec requirements are considered. Matching instances are sorted
// by CPU ascending (best-fit: smallest instance that meets requirements).
// If nil, any stopped instance is eligible (legacy behavior).
//
// The claim flow is:
//  1. Find all stopped instances in the pool
//  2. Filter by spec requirements (if spec is provided)
//  3. Sort by CPU ascending (best-fit selection)
//  4. For each instance, atomically claim it in DynamoDB (fails if another job claimed it)
//  5. Start the EC2 instance and tag it with the repo
//  6. If start fails, release the DynamoDB claim and return error
//
// The caller is responsible for releasing the claim after job completion or failure
// by calling the DB's ReleaseInstanceClaim method.
func (m *Manager) ClaimAndStartPoolInstance(ctx context.Context, poolName string, jobID int64, repo string, spec *fleet.FlexibleSpec) (*AvailableInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.dbClient == nil {
		return nil, fmt.Errorf("DB client not configured for distributed locking")
	}

	// Get all stopped instances in the pool
	instances, err := m.getPoolInstances(ctx, poolName)
	if err != nil {
		return nil, fmt.Errorf("failed to get pool instances: %w", err)
	}

	// Filter stopped instances matching the spec
	var candidates []PoolInstance
	for _, inst := range instances {
		if inst.State != stateStopped {
			continue
		}
		if !inst.matchesFlexibleSpec(spec) {
			continue
		}
		candidates = append(candidates, inst)
	}

	// Sort by CPU ascending (best-fit: smallest instance that meets requirements)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CPU != candidates[j].CPU {
			return candidates[i].CPU < candidates[j].CPU
		}
		return candidates[i].RAM < candidates[j].RAM
	})

	// Try to claim and start each candidate instance
	for _, inst := range candidates {
		instance := &AvailableInstance{
			InstanceID:   inst.InstanceID,
			InstanceType: inst.InstanceType,
			State:        inst.State,
		}

		// Atomically claim this instance in DynamoDB before starting
		if err := m.dbClient.ClaimInstanceForJob(ctx, instance.InstanceID, jobID, instanceClaimTTL); err != nil {
			if errors.Is(err, db.ErrInstanceAlreadyClaimed) {
				continue
			}
			return nil, fmt.Errorf("failed to claim instance in DynamoDB: %w", err)
		}

		// Start the instance while holding both local mutex and DynamoDB claim
		if err := m.startInstanceForJobLocked(ctx, instance.InstanceID, repo); err != nil {
			// Release the DynamoDB claim since we failed to start
			if releaseErr := m.dbClient.ReleaseInstanceClaim(ctx, instance.InstanceID, jobID); releaseErr != nil {
				poolLog.Error("instance claim release failed after start failure",
					slog.String(logging.KeyInstanceID, instance.InstanceID),
					slog.String("error", releaseErr.Error()))
			}
			return nil, fmt.Errorf("failed to start claimed instance: %w", err)
		}

		return instance, nil
	}

	return nil, ErrNoAvailableInstance
}

// StopPoolInstance stops a pool instance (e.g., after failed SSM config).
func (m *Manager) StopPoolInstance(ctx context.Context, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ec2Client == nil {
		return fmt.Errorf("EC2 client not configured")
	}

	_, err := m.ec2Client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("failed to stop instance %s: %w", instanceID, err)
	}

	// Remove from busy tracking
	delete(m.instanceIdle, instanceID)

	poolLog.Info("pool instance stopped",
		slog.String(logging.KeyInstanceID, instanceID))
	return nil
}

// countInstanceStates counts running, stopped, busy, and ready instances.
// Busy count is intersected with actual pool instances to prevent stale job records
// from inflating the count and triggering runaway scaling.
func countInstanceStates(instances []PoolInstance, busyIDs []string) (running, stopped, busy, ready int) {
	for _, inst := range instances {
		switch inst.State {
		case stateRunning:
			running++
		case stateStopped:
			stopped++
		}
	}

	busySet := make(map[string]struct{}, len(busyIDs))
	for _, id := range busyIDs {
		busySet[id] = struct{}{}
	}
	for _, inst := range instances {
		if inst.State != stateRunning {
			continue
		}
		if _, ok := busySet[inst.InstanceID]; ok {
			busy++
		}
	}
	ready = running - busy
	return
}

// createPoolFleetInstances creates new spot instances for a pool.
// Returns the number of instances successfully created.
// Uses CreateSpotInstance (RunInstances API) instead of CreateFleet with FleetTypeMaintain
// because maintain-type fleet instances cannot be manually stopped/started.
func (m *Manager) createPoolFleetInstances(ctx context.Context, poolName string, count int, poolConfig *db.PoolConfig) int {
	instanceTypes, arch := resolvePoolInstanceTypes(poolConfig)
	if len(instanceTypes) == 0 {
		poolLog.Warn("no instance types resolved",
			slog.String(logging.KeyPoolName, poolName))
		return 0
	}

	ranked := m.fleetManager.RankInstanceTypesByPrice(ctx, instanceTypes)

	var created int
	for i := 0; i < count; i++ {
		subnetID := m.selectSubnet()
		if subnetID == "" {
			poolLog.Error("no subnets configured for pool",
				slog.String(logging.KeyPoolName, poolName))
			break
		}
		spec := &fleet.LaunchSpec{
			RunID:        time.Now().UnixNano(),
			InstanceType: ranked[m.randIntn(len(ranked))],
			SubnetID:     subnetID,
			Pool:         poolName,
			Arch:         arch,
		}
		instanceID, spotReqID, err := m.fleetManager.CreateSpotInstance(ctx, spec)
		if err != nil {
			poolLog.Error("spot instance creation failed",
				slog.String(logging.KeyPoolName, poolName),
				slog.String("error", err.Error()))
			continue
		}
		created++

		// Store spot request ID for later cancellation on terminate
		// CreateSpotInstance returns the spot request ID directly (no separate query needed)
		if spotReqID != "" {
			if err := m.dbClient.SaveSpotRequestID(ctx, instanceID, spotReqID, true); err != nil {
				poolLog.Error("spot request ID save failed, may leak on termination",
					slog.String(logging.KeyInstanceID, instanceID),
					slog.String("spot_request_id", spotReqID),
					slog.String(logging.KeyPoolName, poolName),
					slog.String("error", err.Error()))
			}
		}
	}
	return created
}

// resolvePoolInstanceTypes resolves instance types for a pool configuration.
// For ephemeral pools with flexible specs, uses GitHub's resolution logic.
// For legacy pools with a single InstanceType, returns that type.
func resolvePoolInstanceTypes(poolConfig *db.PoolConfig) ([]string, string) {
	// If pool has flexible specs, resolve them
	if poolConfig.CPUMin > 0 || poolConfig.CPUMax > 0 {
		jobConfig := &github.JobConfig{
			Arch:     poolConfig.Arch,
			CPUMin:   poolConfig.CPUMin,
			CPUMax:   poolConfig.CPUMax,
			RAMMin:   poolConfig.RAMMin,
			RAMMax:   poolConfig.RAMMax,
			Families: poolConfig.Families,
		}
		if err := github.ResolveFlexibleSpec(jobConfig); err != nil {
			poolLog.Error("flexible spec resolution failed", slog.String("error", err.Error()))
			// Fall back to pool's InstanceType if resolution fails
			if poolConfig.InstanceType != "" {
				return []string{poolConfig.InstanceType}, poolConfig.Arch
			}
			return nil, ""
		}
		return jobConfig.InstanceTypes, jobConfig.Arch
	}

	// Legacy pool with single instance type
	if poolConfig.InstanceType != "" {
		return []string{poolConfig.InstanceType}, poolConfig.Arch
	}

	return nil, ""
}
