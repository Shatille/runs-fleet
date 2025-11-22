package pools

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
)

// MockDBClient implements DBClient interface
type MockDBClient struct {
	GetPoolConfigFunc   func(ctx context.Context, poolName string) (*db.PoolConfig, error)
	UpdatePoolStateFunc func(ctx context.Context, poolName string, running, stopped int) error
}

func (m *MockDBClient) GetPoolConfig(ctx context.Context, poolName string) (*db.PoolConfig, error) {
	if m.GetPoolConfigFunc != nil {
		return m.GetPoolConfigFunc(ctx, poolName)
	}
	return nil, nil
}

func (m *MockDBClient) UpdatePoolState(ctx context.Context, poolName string, running, stopped int) error {
	if m.UpdatePoolStateFunc != nil {
		return m.UpdatePoolStateFunc(ctx, poolName, running, stopped)
	}
	return nil
}

// MockFleetAPI implements FleetAPI interface
type MockFleetAPI struct {
	CreateFleetFunc func(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)
}

func (m *MockFleetAPI) CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error) {
	if m.CreateFleetFunc != nil {
		return m.CreateFleetFunc(ctx, spec)
	}
	return nil, nil
}

func TestReconcileLoop(t *testing.T) {
	mockDB := &MockDBClient{}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		manager.ReconcileLoop(ctx)
		close(done)
	}()

	cancel()

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer timeoutCancel()

	select {
	case <-done:
	case <-timeoutCtx.Done():
		t.Error("ReconcileLoop did not stop after context cancellation")
	}
}

func TestGetInstance(t *testing.T) {
	tests := []struct {
		name           string
		poolName       string
		mockDB         *MockDBClient
		wantInstanceID string
		wantErr        bool
	}{
		{
			name:           "Empty Pool Name",
			poolName:       "",
			mockDB:         &MockDBClient{},
			wantInstanceID: "",
			wantErr:        true,
		},
		{
			name:     "Pool Exists But No Instance Available",
			poolName: "default-pool",
			mockDB: &MockDBClient{
				GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
					return &db.PoolConfig{
						PoolName:       "default-pool",
						DesiredRunning: 5,
						DesiredStopped: 2,
					}, nil
				},
			},
			wantInstanceID: "",
			wantErr:        true, // Now returns ErrNoInstanceAvailable
		},
		{
			name:     "Pool Not Found",
			poolName: "unknown-pool",
			mockDB: &MockDBClient{
				GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
					return nil, nil
				},
			},
			wantInstanceID: "",
			wantErr:        true,
		},
		{
			name:     "DB Error",
			poolName: "error-pool",
			mockDB: &MockDBClient{
				GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
					return nil, fmt.Errorf("db error")
				},
			},
			wantInstanceID: "",
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager(tt.mockDB, &MockFleetAPI{}, &config.Config{})
			got, err := manager.GetInstance(context.Background(), tt.poolName)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetInstance() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.wantInstanceID {
				t.Errorf("GetInstance() = %v, want %v", got, tt.wantInstanceID)
			}
		})
	}
}
