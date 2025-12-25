package pools

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Test constants to satisfy goconst
const (
	testStateRunning = "running"
	testStateStopped = "stopped"
)

// MockDBClient implements DBClient interface
type MockDBClient struct {
	GetPoolConfigFunc         func(ctx context.Context, poolName string) (*db.PoolConfig, error)
	UpdatePoolStateFunc       func(ctx context.Context, poolName string, running, stopped int) error
	ListPoolsFunc             func(ctx context.Context) ([]string, error)
	GetPoolPeakConcurrencyFunc func(ctx context.Context, poolName string, windowHours int) (int, error)
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

func (m *MockDBClient) GetPoolPeakConcurrency(ctx context.Context, poolName string, windowHours int) (int, error) {
	if m.GetPoolPeakConcurrencyFunc != nil {
		return m.GetPoolPeakConcurrencyFunc(ctx, poolName, windowHours)
	}
	return 0, nil
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

func TestNewManager(t *testing.T) {
	mockDB := &MockDBClient{}
	mockFleet := &MockFleetAPI{}
	cfg := &config.Config{}

	manager := NewManager(mockDB, mockFleet, cfg)

	if manager.dbClient != mockDB {
		t.Error("dbClient not set correctly")
	}
	if manager.fleetManager != mockFleet {
		t.Error("fleetManager not set correctly")
	}
	if manager.config != cfg {
		t.Error("config not set correctly")
	}
	if manager.instanceIdle == nil {
		t.Error("instanceIdle map should be initialized")
	}
	if manager.poolInstances == nil {
		t.Error("poolInstances map should be initialized")
	}
}

// MockEC2API implements EC2API interface
type MockEC2API struct {
	DescribeInstancesFunc  func(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StartInstancesFunc     func(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstancesFunc      func(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	TerminateInstancesFunc func(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

func (m *MockEC2API) DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if m.DescribeInstancesFunc != nil {
		return m.DescribeInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.DescribeInstancesOutput{}, nil
}

func (m *MockEC2API) StartInstances(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	if m.StartInstancesFunc != nil {
		return m.StartInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.StartInstancesOutput{}, nil
}

func (m *MockEC2API) StopInstances(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	if m.StopInstancesFunc != nil {
		return m.StopInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.StopInstancesOutput{}, nil
}

func (m *MockEC2API) TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	if m.TerminateInstancesFunc != nil {
		return m.TerminateInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

// MockCoordinator implements Coordinator interface
type MockCoordinator struct {
	isLeader bool
}

func (m *MockCoordinator) IsLeader() bool {
	return m.isLeader
}

func TestSetEC2Client(t *testing.T) {
	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	mockEC2 := &MockEC2API{}

	manager.SetEC2Client(mockEC2)

	if manager.ec2Client != mockEC2 {
		t.Error("ec2Client not set correctly")
	}
}

func TestSetCoordinator(t *testing.T) {
	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	mockCoord := &MockCoordinator{isLeader: true}

	manager.SetCoordinator(mockCoord)

	if manager.coordinator != mockCoord {
		t.Error("coordinator not set correctly")
	}
}

func TestFilterByState(t *testing.T) {
	manager := &Manager{}
	instances := []PoolInstance{
		{InstanceID: "i-running1", State: "running"},
		{InstanceID: "i-stopped1", State: "stopped"},
		{InstanceID: "i-running2", State: "running"},
		{InstanceID: "i-stopped2", State: "stopped"},
		{InstanceID: "i-pending", State: "pending"},
	}

	running := manager.filterByState(instances, "running")
	if len(running) != 2 {
		t.Errorf("filterByState('running') = %d instances, want 2", len(running))
	}

	stopped := manager.filterByState(instances, "stopped")
	if len(stopped) != 2 {
		t.Errorf("filterByState('stopped') = %d instances, want 2", len(stopped))
	}

	pending := manager.filterByState(instances, "pending")
	if len(pending) != 1 {
		t.Errorf("filterByState('pending') = %d instances, want 1", len(pending))
	}

	terminated := manager.filterByState(instances, "terminated")
	if len(terminated) != 0 {
		t.Errorf("filterByState('terminated') = %d instances, want 0", len(terminated))
	}
}

func TestFilterIdleInstances(t *testing.T) {
	manager := &Manager{}
	now := time.Now()

	// Use clear time boundaries to avoid timing issues
	instances := []PoolInstance{
		{InstanceID: "i-idle-long", State: "running", IdleSince: now.Add(-30 * time.Minute)},
		{InstanceID: "i-idle-medium", State: "running", IdleSince: now.Add(-15 * time.Minute)},
		{InstanceID: "i-idle-short", State: "running", IdleSince: now.Add(-5 * time.Minute)},
		{InstanceID: "i-no-idle", State: "running"},
	}

	// Default timeout (10 min) - should catch instances idle for > 10 min
	idle := manager.filterIdleInstances(instances, 0)
	if len(idle) != 2 {
		t.Errorf("filterIdleInstances(0) = %d instances, want 2 (30min and 15min idle)", len(idle))
	}

	// 20 min timeout - should only catch 30min idle instance
	idle = manager.filterIdleInstances(instances, 20)
	if len(idle) != 1 {
		t.Errorf("filterIdleInstances(20) = %d instances, want 1", len(idle))
	}
	if len(idle) > 0 && idle[0].InstanceID != "i-idle-long" {
		t.Errorf("filterIdleInstances(20) got wrong instance %s, want i-idle-long", idle[0].InstanceID)
	}

	// 3 min timeout - should catch 30min, 15min, and 5min idle instances
	idle = manager.filterIdleInstances(instances, 3)
	if len(idle) != 3 {
		t.Errorf("filterIdleInstances(3) = %d instances, want 3", len(idle))
	}
}

func TestGetScheduledDesiredCounts(t *testing.T) {
	manager := &Manager{}

	tests := []struct {
		name        string
		config      *db.PoolConfig
		wantRunning int
		wantStopped int
	}{
		{
			name: "no schedules - use defaults",
			config: &db.PoolConfig{
				DesiredRunning: 5,
				DesiredStopped: 2,
				Schedules:      []db.PoolSchedule{},
			},
			wantRunning: 5,
			wantStopped: 2,
		},
		{
			name: "nil schedules - use defaults",
			config: &db.PoolConfig{
				DesiredRunning: 3,
				DesiredStopped: 1,
				Schedules:      nil,
			},
			wantRunning: 3,
			wantStopped: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			running, stopped := manager.getScheduledDesiredCounts(tt.config)
			if running != tt.wantRunning {
				t.Errorf("getScheduledDesiredCounts() running = %d, want %d", running, tt.wantRunning)
			}
			if stopped != tt.wantStopped {
				t.Errorf("getScheduledDesiredCounts() stopped = %d, want %d", stopped, tt.wantStopped)
			}
		})
	}
}

//nolint:dupl // Test functions have similar structure but test different EC2 operations - intentional pattern
func TestStartInstances(t *testing.T) {
	tests := []struct {
		name        string
		instanceIDs []string
		mock        *MockEC2API
		wantErr     bool
	}{
		{
			name:        "success",
			instanceIDs: []string{"i-123", "i-456"},
			mock: &MockEC2API{
				StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
					if len(params.InstanceIds) != 2 {
						t.Errorf("StartInstances got %d instances, want 2", len(params.InstanceIds))
					}
					return &ec2.StartInstancesOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:        "empty list",
			instanceIDs: []string{},
			mock:        &MockEC2API{},
			wantErr:     false,
		},
		{
			name:        "ec2 error",
			instanceIDs: []string{"i-error"},
			mock: &MockEC2API{
				StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
					return nil, errors.New("ec2 error")
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
			manager.SetEC2Client(tt.mock)

			err := manager.startInstances(context.Background(), tt.instanceIDs)
			if (err != nil) != tt.wantErr {
				t.Errorf("startInstances() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

//nolint:dupl // Test functions have similar structure but test different EC2 operations - intentional pattern
func TestStopInstances(t *testing.T) {
	tests := []struct {
		name        string
		instanceIDs []string
		mock        *MockEC2API
		wantErr     bool
	}{
		{
			name:        "success",
			instanceIDs: []string{"i-123", "i-456"},
			mock: &MockEC2API{
				StopInstancesFunc: func(_ context.Context, params *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
					if len(params.InstanceIds) != 2 {
						t.Errorf("StopInstances got %d instances, want 2", len(params.InstanceIds))
					}
					return &ec2.StopInstancesOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:        "empty list",
			instanceIDs: []string{},
			mock:        &MockEC2API{},
			wantErr:     false,
		},
		{
			name:        "ec2 error",
			instanceIDs: []string{"i-error"},
			mock: &MockEC2API{
				StopInstancesFunc: func(_ context.Context, _ *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
					return nil, errors.New("ec2 error")
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
			manager.SetEC2Client(tt.mock)
			// Pre-populate idle tracking to verify cleanup
			manager.instanceIdle["i-123"] = time.Now()
			manager.instanceIdle["i-456"] = time.Now()

			err := manager.stopInstances(context.Background(), tt.instanceIDs)
			if (err != nil) != tt.wantErr {
				t.Errorf("stopInstances() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify idle tracking cleanup on success
			if !tt.wantErr && len(tt.instanceIDs) > 0 {
				for _, id := range tt.instanceIDs {
					if _, ok := manager.instanceIdle[id]; ok {
						t.Errorf("instance %s should be removed from idle tracking after stop", id)
					}
				}
			}
		})
	}
}

//nolint:dupl // Test functions have similar structure but test different EC2 operations - intentional pattern
func TestTerminateInstances(t *testing.T) {
	tests := []struct {
		name        string
		instanceIDs []string
		mock        *MockEC2API
		wantErr     bool
	}{
		{
			name:        "success",
			instanceIDs: []string{"i-123", "i-456"},
			mock: &MockEC2API{
				TerminateInstancesFunc: func(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
					if len(params.InstanceIds) != 2 {
						t.Errorf("TerminateInstances got %d instances, want 2", len(params.InstanceIds))
					}
					return &ec2.TerminateInstancesOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:        "empty list",
			instanceIDs: []string{},
			mock:        &MockEC2API{},
			wantErr:     false,
		},
		{
			name:        "ec2 error",
			instanceIDs: []string{"i-error"},
			mock: &MockEC2API{
				TerminateInstancesFunc: func(_ context.Context, _ *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
					return nil, errors.New("ec2 error")
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
			manager.SetEC2Client(tt.mock)
			// Pre-populate idle tracking to verify cleanup
			manager.instanceIdle["i-123"] = time.Now()
			manager.instanceIdle["i-456"] = time.Now()

			err := manager.terminateInstances(context.Background(), tt.instanceIDs)
			if (err != nil) != tt.wantErr {
				t.Errorf("terminateInstances() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify idle tracking cleanup on success
			if !tt.wantErr && len(tt.instanceIDs) > 0 {
				for _, id := range tt.instanceIDs {
					if _, ok := manager.instanceIdle[id]; ok {
						t.Errorf("instance %s should be removed from idle tracking after terminate", id)
					}
				}
			}
		})
	}
}

func TestReconcileWithCoordinator(t *testing.T) {
	// Test that reconcile is skipped when not leader
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			t.Error("ListPools should not be called when not leader")
			return []string{}, nil
		},
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: false})
	manager.SetEC2Client(&MockEC2API{})

	// This should return early because not leader
	manager.reconcile(context.Background())
}

func TestReconcileWithoutEC2Client(t *testing.T) {
	// Test that reconcile logs warning when EC2 client not configured
	listPoolsCalled := false
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			listPoolsCalled = true
			return []string{}, nil
		},
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	// Don't set EC2 client

	manager.reconcile(context.Background())

	// ListPools should NOT be called because EC2 client check happens first
	if listPoolsCalled {
		t.Error("ListPools should not be called when EC2 client is nil")
	}
}

func TestReconcileListPoolsError(_ *testing.T) {
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return nil, errors.New("db error")
		},
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(&MockEC2API{})

	// Should not panic, just log error
	manager.reconcile(context.Background())
}

func TestReconcilePoolConfigError(_ *testing.T) {
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return nil, errors.New("config error")
		},
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(&MockEC2API{})

	// Should not panic, just log error
	manager.reconcile(context.Background())
}

func TestReconcilePoolConfigNotFound(_ *testing.T) {
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return nil, nil
		},
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(&MockEC2API{})

	// Should not panic, just log error
	manager.reconcile(context.Background())
}

func TestGetPoolInstances(t *testing.T) {
	launchTime := time.Now().Add(-1 * time.Hour)
	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(&MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, params *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// Verify filters are set correctly
			if len(params.Filters) != 3 {
				t.Errorf("expected 3 filters, got %d", len(params.Filters))
			}
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-running1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
								LaunchTime:   &launchTime,
							},
							{
								InstanceId:   aws.String("i-stopped1"),
								InstanceType: ec2types.InstanceTypeT3Large,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
	})

	instances, err := manager.getPoolInstances(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("getPoolInstances() error = %v", err)
	}

	if len(instances) != 2 {
		t.Errorf("expected 2 instances, got %d", len(instances))
	}

	// Verify running instance
	var running, stopped *PoolInstance
	for i := range instances {
		switch instances[i].InstanceID {
		case "i-running1":
			running = &instances[i]
		case "i-stopped1":
			stopped = &instances[i]
		}
	}

	if running == nil {
		t.Fatal("running instance not found")
	}
	if running.State != testStateRunning {
		t.Errorf("expected running state, got %s", running.State)
	}
	if running.LaunchTime.IsZero() {
		t.Error("running instance should have launch time")
	}

	if stopped == nil {
		t.Fatal("stopped instance not found")
	}
	if stopped.State != testStateStopped {
		t.Errorf("expected stopped state, got %s", stopped.State)
	}
}

func TestGetPoolInstancesError(t *testing.T) {
	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(&MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return nil, errors.New("ec2 error")
		},
	})

	_, err := manager.getPoolInstances(context.Background(), "test-pool")
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestReconcilePoolScaleUp(t *testing.T) {
	fleetCreateCalled := 0
	startInstancesCalled := false

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 3,
				DesiredStopped: 0,
				InstanceType:   "t3.medium",
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateFleetFunc: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			fleetCreateCalled++
			return []string{"i-new"}, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// Return 1 running instance, need 3 more
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-running1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
						},
					},
				},
			}, nil
		},
		StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			startInstancesCalled = true
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should create 2 new instances (desired 3, have 1)
	if fleetCreateCalled != 2 {
		t.Errorf("expected CreateFleet called 2 times, got %d", fleetCreateCalled)
	}
	// No stopped instances to start
	if startInstancesCalled {
		t.Error("StartInstances should not be called when no stopped instances")
	}
}

func TestReconcilePoolStartStoppedInstances(t *testing.T) {
	startInstancesCalled := false
	startedIDs := []string{}

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 2,
				DesiredStopped: 0,
				InstanceType:   "t3.medium",
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// Return 1 running, 1 stopped - need to start the stopped one
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-running1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
							{
								InstanceId:   aws.String("i-stopped1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			startInstancesCalled = true
			startedIDs = params.InstanceIds
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	if !startInstancesCalled {
		t.Error("StartInstances should be called")
	}
	if len(startedIDs) != 1 || startedIDs[0] != "i-stopped1" {
		t.Errorf("expected to start i-stopped1, got %v", startedIDs)
	}
}

func TestReconcilePoolScaleDown(t *testing.T) {
	terminateInstancesCalled := false

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning:     1,
				DesiredStopped:     0,
				InstanceType:       "t3.medium",
				IdleTimeoutMinutes: 1,
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// Return 3 running instances - need to terminate 2
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-running1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
							{
								InstanceId:   aws.String("i-running2"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
							{
								InstanceId:   aws.String("i-running3"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
						},
					},
				},
			}, nil
		},
		TerminateInstancesFunc: func(_ context.Context, _ *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			terminateInstancesCalled = true
			return &ec2.TerminateInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	// Mark instances as idle for long enough
	idleTime := time.Now().Add(-10 * time.Minute)
	manager.instanceIdle["i-running1"] = idleTime
	manager.instanceIdle["i-running2"] = idleTime
	manager.instanceIdle["i-running3"] = idleTime

	manager.reconcile(context.Background())

	// Should terminate excess instances
	if !terminateInstancesCalled {
		t.Error("TerminateInstances should be called for excess running instances")
	}
}

func TestReconcilePoolTerminateExcessStopped(t *testing.T) {
	terminateInstancesCalled := false
	terminatedIDs := []string{}

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 1,
				DesiredStopped: 1,
				InstanceType:   "t3.medium",
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// Return 1 running, 3 stopped - need to terminate 2 stopped
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-running1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
							{
								InstanceId:   aws.String("i-stopped1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
							{
								InstanceId:   aws.String("i-stopped2"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
							{
								InstanceId:   aws.String("i-stopped3"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
		TerminateInstancesFunc: func(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			terminateInstancesCalled = true
			terminatedIDs = params.InstanceIds
			return &ec2.TerminateInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should terminate 2 excess stopped instances
	if !terminateInstancesCalled {
		t.Error("TerminateInstances should be called for excess stopped instances")
	}
	if len(terminatedIDs) != 2 {
		t.Errorf("expected 2 instances terminated, got %d", len(terminatedIDs))
	}
}

func TestReconcilePoolWithSchedule(t *testing.T) {
	now := time.Now()
	currentHour := now.Hour()
	currentDay := int(now.Weekday())

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 1,
				DesiredStopped: 0,
				InstanceType:   "t3.medium",
				Schedules: []db.PoolSchedule{
					{
						StartHour:      currentHour,
						EndHour:        currentHour + 2,
						DaysOfWeek:     []int{currentDay},
						DesiredRunning: 5,
						DesiredStopped: 2,
					},
				},
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	fleetCreateCount := 0
	mockFleet := &MockFleetAPI{
		CreateFleetFunc: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			fleetCreateCount++
			return []string{"i-new"}, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// Return 0 instances
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should create 5 instances based on schedule (not 1 from default)
	if fleetCreateCount != 5 {
		t.Errorf("expected 5 fleet creates based on schedule, got %d", fleetCreateCount)
	}
}

func TestReconcilePoolUpdateStateError(_ *testing.T) {
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 1,
				DesiredStopped: 0,
				InstanceType:   "t3.medium",
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return errors.New("update error")
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-running1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
						},
					},
				},
			}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	// Should not panic, just log error
	manager.reconcile(context.Background())
}

func TestReconcilePoolStartInstancesError(t *testing.T) {
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 2,
				DesiredStopped: 0,
				InstanceType:   "t3.medium",
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	fleetCreateCount := 0
	mockFleet := &MockFleetAPI{
		CreateFleetFunc: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			fleetCreateCount++
			return []string{"i-new"}, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-stopped1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
		StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			return nil, errors.New("start error")
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// When start fails, should try to create new instances instead
	if fleetCreateCount != 2 {
		t.Errorf("expected 2 fleet creates when start failed, got %d", fleetCreateCount)
	}
}

func TestReconcilePoolCreateFleetError(_ *testing.T) {
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 2,
				DesiredStopped: 0,
				InstanceType:   "t3.medium",
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateFleetFunc: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return nil, errors.New("fleet create error")
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	// Should not panic
	manager.reconcile(context.Background())
}

func TestReconcilePoolStopInstancesError(_ *testing.T) {
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning:     0,
				DesiredStopped:     1,
				InstanceType:       "t3.medium",
				IdleTimeoutMinutes: 1,
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-running1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
						},
					},
				},
			}, nil
		},
		StopInstancesFunc: func(_ context.Context, _ *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			return nil, errors.New("stop error")
		},
		TerminateInstancesFunc: func(_ context.Context, _ *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			return &ec2.TerminateInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	// Mark instance as idle
	manager.instanceIdle["i-running1"] = time.Now().Add(-10 * time.Minute)

	// Should not panic
	manager.reconcile(context.Background())
}

func TestReconcilePoolTerminateInstancesError(_ *testing.T) {
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning:     0,
				DesiredStopped:     0,
				InstanceType:       "t3.medium",
				IdleTimeoutMinutes: 1,
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-running1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
						},
					},
				},
			}, nil
		},
		TerminateInstancesFunc: func(_ context.Context, _ *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			return nil, errors.New("terminate error")
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	// Mark instance as idle
	manager.instanceIdle["i-running1"] = time.Now().Add(-10 * time.Minute)

	// Should not panic
	manager.reconcile(context.Background())
}

func TestGetPoolInstancesIdleTracking(t *testing.T) {
	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})

	// Pre-populate idle tracking for one instance
	existingIdleTime := time.Now().Add(-5 * time.Minute)
	manager.instanceIdle["i-existing"] = existingIdleTime

	manager.SetEC2Client(&MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-existing"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
							{
								InstanceId:   aws.String("i-new"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
							{
								InstanceId:   aws.String("i-stopped"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
	})

	instances, err := manager.getPoolInstances(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("getPoolInstances() error = %v", err)
	}

	// Find instances
	var existing, newInst, stopped *PoolInstance
	for i := range instances {
		switch instances[i].InstanceID {
		case "i-existing":
			existing = &instances[i]
		case "i-new":
			newInst = &instances[i]
		case "i-stopped":
			stopped = &instances[i]
		}
	}

	// Existing instance should retain its idle time
	if existing == nil {
		t.Fatal("existing instance not found")
	}
	if !existing.IdleSince.Equal(existingIdleTime) {
		t.Errorf("existing instance should retain idle time, got %v, want %v", existing.IdleSince, existingIdleTime)
	}

	// New running instance should have idle time initialized
	if newInst == nil {
		t.Fatal("new instance not found")
	}
	if newInst.IdleSince.IsZero() {
		t.Error("new running instance should have idle time initialized")
	}

	// Stopped instance should not have idle time set
	if stopped == nil {
		t.Fatal("stopped instance not found")
	}
	if !stopped.IdleSince.IsZero() {
		t.Error("stopped instance should not have idle time")
	}
}

func TestReconcileEphemeralPoolAutoScaling(t *testing.T) {
	fleetCreateCount := 0

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"ephemeral-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:       "ephemeral-pool",
				DesiredRunning: 1, // Default, should be overridden by peak
				DesiredStopped: 0,
				InstanceType:   "c7g.xlarge",
				Ephemeral:      true,
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
		GetPoolPeakConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) {
			return 3, nil // Peak of 3 concurrent jobs
		},
	}

	mockFleet := &MockFleetAPI{
		CreateFleetFunc: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			fleetCreateCount++
			return []string{fmt.Sprintf("i-new%d", fleetCreateCount)}, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// Return 0 instances - all should be created
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should create 3 instances based on peak concurrency, not 1 from default
	if fleetCreateCount != 3 {
		t.Errorf("expected 3 fleet creates based on peak concurrency, got %d", fleetCreateCount)
	}
}

func TestReconcileEphemeralPoolPeakError(t *testing.T) {
	fleetCreateCount := 0

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"ephemeral-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:       "ephemeral-pool",
				DesiredRunning: 2, // Default, should be used when peak fails
				DesiredStopped: 0,
				InstanceType:   "c7g.xlarge",
				Ephemeral:      true,
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
		GetPoolPeakConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) {
			return 0, errors.New("database error")
		},
	}

	mockFleet := &MockFleetAPI{
		CreateFleetFunc: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			fleetCreateCount++
			return []string{fmt.Sprintf("i-new%d", fleetCreateCount)}, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{})
	manager.SetCoordinator(&MockCoordinator{isLeader: true})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should fallback to default DesiredRunning of 2 when peak fails
	if fleetCreateCount != 2 {
		t.Errorf("expected 2 fleet creates (fallback to default), got %d", fleetCreateCount)
	}
}
