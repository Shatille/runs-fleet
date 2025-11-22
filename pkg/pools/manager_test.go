package pools

import (
	"context"
	"fmt"
	"testing"

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
	CreateFleetFunc func(ctx context.Context, spec *fleet.LaunchSpec) error
}

func (m *MockFleetAPI) CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) error {
	if m.CreateFleetFunc != nil {
		return m.CreateFleetFunc(ctx, spec)
	}
	return nil
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
			name:     "Pool Exists",
			poolName: "default-pool",
			mockDB: &MockDBClient{
				GetPoolConfigFunc: func(ctx context.Context, poolName string) (*db.PoolConfig, error) {
					return &db.PoolConfig{
						PoolName:       "default-pool",
						DesiredRunning: 5,
						DesiredStopped: 2,
					}, nil
				},
			},
			wantInstanceID: "", // Currently returns empty string in MVP
			wantErr:        false,
		},
		{
			name:     "Pool Not Found",
			poolName: "unknown-pool",
			mockDB: &MockDBClient{
				GetPoolConfigFunc: func(ctx context.Context, poolName string) (*db.PoolConfig, error) {
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
				GetPoolConfigFunc: func(ctx context.Context, poolName string) (*db.PoolConfig, error) {
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
