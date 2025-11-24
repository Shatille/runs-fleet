// Package pools manages warm pool operations for maintaining pre-provisioned EC2 instances.
package pools

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
)

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
