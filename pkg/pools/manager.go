// Package pools manages warm pool operations for maintaining pre-provisioned EC2 instances.
package pools

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
)

// ErrNoInstanceAvailable indicates no warm pool instance is available for assignment.
var ErrNoInstanceAvailable = errors.New("no instance available in pool")

// DBClient defines DynamoDB operations for pool configuration.
type DBClient interface {
	GetPoolConfig(ctx context.Context, poolName string) (*db.PoolConfig, error)
	UpdatePoolState(ctx context.Context, poolName string, running, stopped int) error
}

// FleetAPI defines EC2 fleet operations for instance provisioning.
type FleetAPI interface {
	CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)
}

// Manager orchestrates warm pool operations and instance assignment.
type Manager struct {
	// mu protects in-memory pool state (to be added in Phase 3)
	mu           sync.RWMutex
	dbClient     DBClient
	fleetManager FleetAPI
	config       *config.Config
}

// NewManager creates pool manager with DB and fleet clients.
func NewManager(dbClient DBClient, fleetManager FleetAPI, cfg *config.Config) *Manager {
	return &Manager{
		dbClient:     dbClient,
		fleetManager: fleetManager,
		config:       cfg,
	}
}

// GetInstance attempts to get an instance from the warm pool.
// Returns instance ID if available, or ErrNoInstanceAvailable to trigger cold start.
func (m *Manager) GetInstance(ctx context.Context, poolName string) (string, error) {
	if poolName == "" {
		return "", fmt.Errorf("pool name is required")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	poolConfig, err := m.dbClient.GetPoolConfig(ctx, poolName)
	if err != nil {
		return "", fmt.Errorf("failed to get pool config: %w", err)
	}

	if poolConfig == nil {
		return "", fmt.Errorf("pool %s not found", poolName)
	}

	log.Printf("Checked pool %s: DesiredRunning=%d, DesiredStopped=%d",
		poolName, poolConfig.DesiredRunning, poolConfig.DesiredStopped)

	// Placeholder: Always returns ErrNoInstanceAvailable to force cold start until Phase 3.
	return "", ErrNoInstanceAvailable
}

// ReconcileLoop runs periodically to maintain pool size.
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

func (m *Manager) reconcile(_ context.Context) {
	// TODO: Use ctx for EC2 API calls and cancellation once reconciliation is implemented.
	m.mu.Lock()
	defer m.mu.Unlock()

	log.Println("Reconciling pools... (Not implemented in MVP)")
}
