// Package pools manages warm pool operations for maintaining pre-provisioned EC2 instances.
package pools

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// DBClient defines DynamoDB operations for pool configuration.
type DBClient interface {
	GetPoolConfig(ctx context.Context, poolName string) (*db.PoolConfig, error)
	UpdatePoolState(ctx context.Context, poolName string, running, stopped int) error
	ListPools(ctx context.Context) ([]string, error)
}

// FleetAPI defines EC2 fleet operations for instance provisioning.
type FleetAPI interface {
	CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)
}

// EC2API defines EC2 operations for instance management.
type EC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StartInstances(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// Coordinator defines distributed coordination operations.
type Coordinator interface {
	IsLeader() bool
}

// PoolInstance represents an EC2 instance in a pool.
type PoolInstance struct {
	InstanceID   string
	State        string
	LaunchTime   time.Time
	InstanceType string
	IdleSince    time.Time // When the instance became idle (no job assigned)
}

// Manager orchestrates warm pool operations and instance assignment.
type Manager struct {
	mu            sync.RWMutex
	dbClient      DBClient
	fleetManager  FleetAPI
	ec2Client     EC2API
	coordinator   Coordinator
	config        *config.Config
	instanceIdle  map[string]time.Time // Tracks when instances became idle
	poolInstances map[string][]string  // Cache of instance IDs per pool
}

// NewManager creates pool manager with DB and fleet clients.
func NewManager(dbClient DBClient, fleetManager FleetAPI, cfg *config.Config) *Manager {
	return &Manager{
		dbClient:      dbClient,
		fleetManager:  fleetManager,
		config:        cfg,
		instanceIdle:  make(map[string]time.Time),
		poolInstances: make(map[string][]string),
	}
}

// SetEC2Client sets the EC2 client for instance management.
func (m *Manager) SetEC2Client(ec2Client EC2API) {
	m.ec2Client = ec2Client
}

// SetCoordinator sets the coordinator for leader election.
func (m *Manager) SetCoordinator(coordinator Coordinator) {
	m.coordinator = coordinator
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
	if m.coordinator != nil && !m.coordinator.IsLeader() {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ec2Client == nil {
		log.Println("EC2 client not configured, skipping reconciliation")
		return
	}

	pools, err := m.dbClient.ListPools(ctx)
	if err != nil {
		log.Printf("Failed to list pools: %v", err)
		return
	}

	for _, poolName := range pools {
		if err := m.reconcilePool(ctx, poolName); err != nil {
			log.Printf("Failed to reconcile pool %s: %v", poolName, err)
		}
	}
}

func (m *Manager) reconcilePool(ctx context.Context, poolName string) error {
	poolConfig, err := m.dbClient.GetPoolConfig(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get pool config: %w", err)
	}
	if poolConfig == nil {
		return fmt.Errorf("pool config not found")
	}

	desiredRunning, desiredStopped := m.getScheduledDesiredCounts(poolConfig)

	instances, err := m.getPoolInstances(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get pool instances: %w", err)
	}

	var running, stopped int
	for _, inst := range instances {
		switch inst.State {
		case "running":
			running++
		case "stopped":
			stopped++
		}
	}

	log.Printf("Pool %s: desired running=%d stopped=%d, actual running=%d stopped=%d",
		poolName, desiredRunning, desiredStopped, running, stopped)

	if running < desiredRunning {
		deficit := desiredRunning - running

		stoppedInstances := m.filterByState(instances, "stopped")
		toStart := min(deficit, len(stoppedInstances))
		if toStart > 0 {
			instanceIDs := make([]string, toStart)
			for i := 0; i < toStart; i++ {
				instanceIDs[i] = stoppedInstances[i].InstanceID
			}
			if err := m.startInstances(ctx, instanceIDs); err != nil {
				log.Printf("Failed to start instances: %v", err)
			} else {
				deficit -= toStart
			}
		}

		if deficit > 0 {
			for i := 0; i < deficit; i++ {
				spec := &fleet.LaunchSpec{
					RunID:        time.Now().UnixNano(),
					InstanceType: poolConfig.InstanceType,
					Pool:         poolName,
					Spot:         true,
				}
				if _, err := m.fleetManager.CreateFleet(ctx, spec); err != nil {
					log.Printf("Failed to create fleet instance for pool %s: %v", poolName, err)
				}
			}
		}
	} else if running > desiredRunning {
		// Too many running instances - stop or terminate excess
		excess := running - desiredRunning
		runningInstances := m.filterByState(instances, "running")

		// First, check idle instances (those without assigned jobs)
		idleInstances := m.filterIdleInstances(runningInstances, poolConfig.IdleTimeoutMinutes)

		toStop := min(excess, len(idleInstances))
		if toStop > 0 && stopped < desiredStopped {
			canStop := min(toStop, desiredStopped-stopped)
			instanceIDs := make([]string, canStop)
			for i := 0; i < canStop; i++ {
				instanceIDs[i] = idleInstances[i].InstanceID
			}
			if err := m.stopInstances(ctx, instanceIDs); err != nil {
				log.Printf("Failed to stop instances: %v", err)
			}
			excess -= canStop
		}

		if excess > 0 {
			// Sort by launch time (oldest first)
			toTerminate := min(excess, len(idleInstances))
			if toTerminate > 0 {
				instanceIDs := make([]string, toTerminate)
				for i := 0; i < toTerminate; i++ {
					instanceIDs[i] = idleInstances[i].InstanceID
				}
				if err := m.terminateInstances(ctx, instanceIDs); err != nil {
					log.Printf("Failed to terminate instances: %v", err)
				}
			}
		}
	}

	if stopped > desiredStopped {
		excess := stopped - desiredStopped
		stoppedInstances := m.filterByState(instances, "stopped")
		toTerminate := min(excess, len(stoppedInstances))
		if toTerminate > 0 {
			instanceIDs := make([]string, toTerminate)
			for i := 0; i < toTerminate; i++ {
				instanceIDs[i] = stoppedInstances[i].InstanceID
			}
			if err := m.terminateInstances(ctx, instanceIDs); err != nil {
				log.Printf("Failed to terminate stopped instances: %v", err)
			}
		}
	}

	if err := m.dbClient.UpdatePoolState(ctx, poolName, running, stopped); err != nil {
		log.Printf("Failed to update pool state: %v", err)
	}

	return nil
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
			instance := PoolInstance{
				InstanceID:   aws.ToString(inst.InstanceId),
				State:        string(inst.State.Name),
				InstanceType: string(inst.InstanceType),
			}
			if inst.LaunchTime != nil {
				instance.LaunchTime = *inst.LaunchTime
			}

			// Check idle tracking
			if idleSince, ok := m.instanceIdle[instance.InstanceID]; ok {
				instance.IdleSince = idleSince
			} else if instance.State == "running" {
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

	log.Printf("Started %d instances: %v", len(instanceIDs), instanceIDs)
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

	log.Printf("Stopped %d instances: %v", len(instanceIDs), instanceIDs)
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

	// Remove from idle tracking
	for _, id := range instanceIDs {
		delete(m.instanceIdle, id)
	}

	log.Printf("Terminated %d instances: %v", len(instanceIDs), instanceIDs)
	return nil
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
