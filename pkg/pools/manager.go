package pools

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
)

type DBClient interface {
	GetPoolConfig(ctx context.Context, poolName string) (*db.PoolConfig, error)
	UpdatePoolState(ctx context.Context, poolName string, running, stopped int) error
}

type FleetAPI interface {
	CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) error
}

type Manager struct {
	dbClient     DBClient
	fleetManager FleetAPI
	config       *config.Config
}

func NewManager(dbClient DBClient, fleetManager FleetAPI, cfg *config.Config) *Manager {
	return &Manager{
		dbClient:     dbClient,
		fleetManager: fleetManager,
		config:       cfg,
	}
}

// GetInstance attempts to get an instance from the warm pool
// For MVP, this is a placeholder that checks config but falls back to cold start if needed
// In a real system, this would grab a pre-warmed instance ID from a queue or DB
func (m *Manager) GetInstance(ctx context.Context, poolName string) (string, error) {
	// 1. Check if pool exists and has capacity
	poolConfig, err := m.dbClient.GetPoolConfig(ctx, poolName)
	if err != nil {
		return "", fmt.Errorf("failed to get pool config: %w", err)
	}

	if poolConfig == nil {
		return "", fmt.Errorf("pool %s not found", poolName)
	}

	// 2. In a real implementation, we would:
	//    a. Check for "Hot" instances (running, idle) -> Return Instance ID
	//    b. Check for "Stopped" instances -> Start one -> Return Instance ID
	//    c. If none, return error (trigger cold start)

	// For MVP, we just log that we checked the pool
	log.Printf("Checked pool %s: DesiredRunning=%d, DesiredStopped=%d",
		poolName, poolConfig.DesiredRunning, poolConfig.DesiredStopped)

	// Simulate "no instance available" to force cold start for now
	// or implement basic logic if we had state tracking
	return "", nil
}

// ReconcileLoop runs periodically to maintain pool size
func (m *Manager) ReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

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
	// 1. List all pools (needs Scan or known list)
	// For MVP, we might just check a hardcoded list or single pool if we don't implement Scan
	log.Println("Reconciling pools... (Not implemented in MVP)")
}
