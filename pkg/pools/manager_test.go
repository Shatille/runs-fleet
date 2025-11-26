package pools

import (
	"context"
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
	ListPoolsFunc       func(ctx context.Context) ([]string, error)
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

func (m *MockDBClient) ListPools(ctx context.Context) ([]string, error) {
	if m.ListPoolsFunc != nil {
		return m.ListPoolsFunc(ctx)
	}
	return []string{}, nil
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
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{}, nil
		},
	}
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

func TestScheduleMatches(t *testing.T) {
	manager := &Manager{}

	tests := []struct {
		name      string
		schedule  db.PoolSchedule
		hour      int
		day       time.Weekday
		wantMatch bool
	}{
		{
			name: "Business hours match",
			schedule: db.PoolSchedule{
				StartHour:  9,
				EndHour:    17,
				DaysOfWeek: []int{1, 2, 3, 4, 5}, // Mon-Fri
			},
			hour:      10,
			day:       time.Monday,
			wantMatch: true,
		},
		{
			name: "Business hours no match - weekend",
			schedule: db.PoolSchedule{
				StartHour:  9,
				EndHour:    17,
				DaysOfWeek: []int{1, 2, 3, 4, 5}, // Mon-Fri
			},
			hour:      10,
			day:       time.Saturday,
			wantMatch: false,
		},
		{
			name: "Business hours no match - before hours",
			schedule: db.PoolSchedule{
				StartHour:  9,
				EndHour:    17,
				DaysOfWeek: []int{1, 2, 3, 4, 5},
			},
			hour:      8,
			day:       time.Monday,
			wantMatch: false,
		},
		{
			name: "Overnight range",
			schedule: db.PoolSchedule{
				StartHour:  22,
				EndHour:    6,
				DaysOfWeek: []int{},
			},
			hour:      23,
			day:       time.Monday,
			wantMatch: true,
		},
		{
			name: "Overnight range - early morning",
			schedule: db.PoolSchedule{
				StartHour:  22,
				EndHour:    6,
				DaysOfWeek: []int{},
			},
			hour:      3,
			day:       time.Tuesday,
			wantMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := manager.scheduleMatches(tt.schedule, tt.hour, tt.day)
			if got != tt.wantMatch {
				t.Errorf("scheduleMatches() = %v, want %v", got, tt.wantMatch)
			}
		})
	}
}

func TestMarkInstanceBusyIdle(t *testing.T) {
	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})

	// Test marking instance busy
	manager.MarkInstanceIdle("i-123456")
	if _, ok := manager.instanceIdle["i-123456"]; !ok {
		t.Error("Instance should be in idle map after MarkInstanceIdle")
	}

	// Test marking instance busy removes from idle
	manager.MarkInstanceBusy("i-123456")
	if _, ok := manager.instanceIdle["i-123456"]; ok {
		t.Error("Instance should not be in idle map after MarkInstanceBusy")
	}
}
