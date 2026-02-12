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
	testStateRunning      = "running"
	testStateStopped      = "stopped"
	testInstanceStoppedID = "i-stopped1"
	testInstanceNewID     = "i-new"
	testSpotRequestNewID  = "sir-new"
)

//nolint:dupl // Mock struct mirrors DBClient interface - intentional pattern
// MockDBClient implements DBClient interface
type MockDBClient struct {
	GetPoolConfigFunc            func(ctx context.Context, poolName string) (*db.PoolConfig, error)
	UpdatePoolStateFunc          func(ctx context.Context, poolName string, running, stopped int) error
	ListPoolsFunc                func(ctx context.Context) ([]string, error)
	GetPoolP90ConcurrencyFunc    func(ctx context.Context, poolName string, windowHours int) (int, error)
	GetPoolBusyInstanceIDsFunc   func(ctx context.Context, poolName string) ([]string, error)
	AcquirePoolReconcileLockFunc func(ctx context.Context, poolName, owner string, ttl time.Duration) error
	ReleasePoolReconcileLockFunc func(ctx context.Context, poolName, owner string) error
	ClaimInstanceForJobFunc      func(ctx context.Context, instanceID string, jobID int64, ttl time.Duration) error
	ReleaseInstanceClaimFunc     func(ctx context.Context, instanceID string, jobID int64) error
	SaveSpotRequestIDFunc        func(ctx context.Context, instanceID, spotRequestID string, persistent bool) error
	GetSpotRequestIDsFunc        func(ctx context.Context, instanceIDs []string) (map[string]db.SpotRequestInfo, error)
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

func (m *MockDBClient) GetPoolP90Concurrency(ctx context.Context, poolName string, windowHours int) (int, error) {
	if m.GetPoolP90ConcurrencyFunc != nil {
		return m.GetPoolP90ConcurrencyFunc(ctx, poolName, windowHours)
	}
	return 0, nil
}

func (m *MockDBClient) GetPoolBusyInstanceIDs(ctx context.Context, poolName string) ([]string, error) {
	if m.GetPoolBusyInstanceIDsFunc != nil {
		return m.GetPoolBusyInstanceIDsFunc(ctx, poolName)
	}
	return nil, nil
}

func (m *MockDBClient) AcquirePoolReconcileLock(ctx context.Context, poolName, owner string, ttl time.Duration) error {
	if m.AcquirePoolReconcileLockFunc != nil {
		return m.AcquirePoolReconcileLockFunc(ctx, poolName, owner, ttl)
	}
	return nil
}

func (m *MockDBClient) ReleasePoolReconcileLock(ctx context.Context, poolName, owner string) error {
	if m.ReleasePoolReconcileLockFunc != nil {
		return m.ReleasePoolReconcileLockFunc(ctx, poolName, owner)
	}
	return nil
}

func (m *MockDBClient) ClaimInstanceForJob(ctx context.Context, instanceID string, jobID int64, ttl time.Duration) error {
	if m.ClaimInstanceForJobFunc != nil {
		return m.ClaimInstanceForJobFunc(ctx, instanceID, jobID, ttl)
	}
	return nil
}

func (m *MockDBClient) ReleaseInstanceClaim(ctx context.Context, instanceID string, jobID int64) error {
	if m.ReleaseInstanceClaimFunc != nil {
		return m.ReleaseInstanceClaimFunc(ctx, instanceID, jobID)
	}
	return nil
}

func (m *MockDBClient) SaveSpotRequestID(ctx context.Context, instanceID, spotRequestID string, persistent bool) error {
	if m.SaveSpotRequestIDFunc != nil {
		return m.SaveSpotRequestIDFunc(ctx, instanceID, spotRequestID, persistent)
	}
	return nil
}

func (m *MockDBClient) GetSpotRequestIDs(ctx context.Context, instanceIDs []string) (map[string]db.SpotRequestInfo, error) {
	if m.GetSpotRequestIDsFunc != nil {
		return m.GetSpotRequestIDsFunc(ctx, instanceIDs)
	}
	return make(map[string]db.SpotRequestInfo), nil
}

// MockFleetAPI implements FleetAPI interface
type MockFleetAPI struct {
	CreateFleetFunc                    func(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)
	CreateSpotInstanceFunc             func(ctx context.Context, spec *fleet.LaunchSpec) (string, string, error)
	GetSpotRequestIDForInstanceFunc    func(ctx context.Context, instanceID string) (string, error)
	GetSpotRequestIDsForInstancesFunc  func(ctx context.Context, instanceIDs []string) (map[string]string, error)
}

func (m *MockFleetAPI) CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error) {
	if m.CreateFleetFunc != nil {
		return m.CreateFleetFunc(ctx, spec)
	}
	return nil, nil
}

func (m *MockFleetAPI) CreateSpotInstance(ctx context.Context, spec *fleet.LaunchSpec) (string, string, error) {
	if m.CreateSpotInstanceFunc != nil {
		return m.CreateSpotInstanceFunc(ctx, spec)
	}
	return "", "", nil
}

func (m *MockFleetAPI) GetSpotRequestIDForInstance(ctx context.Context, instanceID string) (string, error) {
	if m.GetSpotRequestIDForInstanceFunc != nil {
		return m.GetSpotRequestIDForInstanceFunc(ctx, instanceID)
	}
	return "", nil
}

func (m *MockFleetAPI) GetSpotRequestIDsForInstances(ctx context.Context, instanceIDs []string) (map[string]string, error) {
	if m.GetSpotRequestIDsForInstancesFunc != nil {
		return m.GetSpotRequestIDsForInstancesFunc(ctx, instanceIDs)
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

//nolint:dupl // Mock struct mirrors EC2API interface - intentional pattern
// MockEC2API implements EC2API interface
type MockEC2API struct {
	DescribeInstancesFunc          func(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StartInstancesFunc             func(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstancesFunc              func(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	TerminateInstancesFunc         func(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	CancelSpotInstanceRequestsFunc func(ctx context.Context, params *ec2.CancelSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error)
	CreateTagsFunc                 func(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
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

func (m *MockEC2API) CancelSpotInstanceRequests(ctx context.Context, params *ec2.CancelSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	if m.CancelSpotInstanceRequestsFunc != nil {
		return m.CancelSpotInstanceRequestsFunc(ctx, params, optFns...)
	}
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

func (m *MockEC2API) CreateTags(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	if m.CreateTagsFunc != nil {
		return m.CreateTagsFunc(ctx, params, optFns...)
	}
	return &ec2.CreateTagsOutput{}, nil
}

func TestSetEC2Client(t *testing.T) {
	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	mockEC2 := &MockEC2API{}

	manager.SetEC2Client(mockEC2)

	if manager.ec2Client != mockEC2 {
		t.Error("ec2Client not set correctly")
	}
}

func TestFilterByState(t *testing.T) {
	manager := &Manager{}
	instances := []PoolInstance{
		{InstanceID: "i-running1", State: "running"},
		{InstanceID: testInstanceStoppedID, State: testStateStopped},
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

func TestReconcileWithLockHeld(t *testing.T) {
	// Test that pool reconciliation is skipped when lock is held by another instance
	getPoolConfigCalled := false
	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		AcquirePoolReconcileLockFunc: func(_ context.Context, _, _ string, _ time.Duration) error {
			return db.ErrPoolReconcileLockHeld
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			getPoolConfigCalled = true
			return &db.PoolConfig{DesiredRunning: 1}, nil
		},
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(&MockEC2API{})

	manager.reconcile(context.Background())

	// GetPoolConfig should NOT be called when lock is held
	if getPoolConfigCalled {
		t.Error("GetPoolConfig should not be called when lock is held by another instance")
	}
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
								InstanceId:   aws.String(testInstanceStoppedID),
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
		case testInstanceStoppedID:
			stopped = &instances[i]
		}
	}

	if running == nil {
		t.Fatal("running instance not found")
		return
	}
	if running.State != testStateRunning {
		t.Errorf("expected running state, got %s", running.State)
	}
	if running.LaunchTime.IsZero() {
		t.Error("running instance should have launch time")
	}

	if stopped == nil {
		t.Fatal("stopped instance not found")
		return
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

func TestSelectSubnet(t *testing.T) {
	tests := []struct {
		name           string
		privateSubnets []string
		publicSubnets  []string
		calls          int
		wantSubnets    []string
	}{
		{
			name:           "private subnets preferred",
			privateSubnets: []string{"subnet-priv1", "subnet-priv2"},
			publicSubnets:  []string{"subnet-pub1"},
			calls:          3,
			wantSubnets:    []string{"subnet-priv1", "subnet-priv2", "subnet-priv1"},
		},
		{
			name:          "falls back to public when no private",
			publicSubnets: []string{"subnet-pub1", "subnet-pub2"},
			calls:         2,
			wantSubnets:   []string{"subnet-pub1", "subnet-pub2"},
		},
		{
			name:  "returns empty when no subnets",
			calls: 1,
			wantSubnets: []string{""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{
				PrivateSubnetIDs: tt.privateSubnets,
				PublicSubnetIDs:  tt.publicSubnets,
			})
			for i := 0; i < tt.calls; i++ {
				got := manager.selectSubnet()
				if got != tt.wantSubnets[i] {
					t.Errorf("call %d: selectSubnet() = %q, want %q", i, got, tt.wantSubnets[i])
				}
			}
		})
	}
}

func TestReconcilePoolScaleUp(t *testing.T) {
	fleetCreateCalled := 0
	startInstancesCalled := false
	var capturedSpecs []*fleet.LaunchSpec

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
		CreateSpotInstanceFunc: func(_ context.Context, spec *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCalled++
			capturedSpecs = append(capturedSpecs, spec)
			return testInstanceNewID, testSpotRequestNewID, nil
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

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-priv1", "subnet-priv2"},
	})
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
	// Verify SubnetID is set on fleet specs
	for i, spec := range capturedSpecs {
		if spec.SubnetID == "" {
			t.Errorf("fleet create call %d: SubnetID should not be empty", i)
		}
	}
}

func TestReconcilePoolScaleUpWithBusyInstances(t *testing.T) {
	// Test that reconciliation creates new instances when running instances are busy
	// desired_ready=2, running=2, busy=2 -> ready=0 -> need 2 new instances
	fleetCreateCalled := 0

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 2, // Desired ready instances
				DesiredStopped: 0,
				InstanceType:   "t3.medium",
			}, nil
		},
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) {
			return []string{"i-busy1", "i-busy2"}, nil // Both instances are busy
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCalled++
			return testInstanceNewID, testSpotRequestNewID, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// 2 running instances, but both busy
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-busy1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
							{
								InstanceId:   aws.String("i-busy2"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
						},
					},
				},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should create 2 new instances because ready=0 and desired=2
	if fleetCreateCalled != 2 {
		t.Errorf("Expected 2 fleet create calls (busy instances don't count as ready), got %d", fleetCreateCalled)
	}
}

func TestReconcilePoolStaleJobRecordsIgnored(t *testing.T) {
	// Stale job records from terminated instances should not inflate busy count.
	// GetPoolBusyInstanceIDs returns instance IDs from DynamoDB, but only IDs
	// matching actual running pool instances should count as busy.
	fleetCreateCalled := 0

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
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) {
			// 100 stale records from long-dead instances + 1 real busy instance
			return []string{
				"i-running1", // matches actual pool instance
				"i-dead1", "i-dead2", "i-dead3", "i-dead4", "i-dead5",
				"i-dead6", "i-dead7", "i-dead8", "i-dead9", "i-dead10",
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCalled++
			return testInstanceNewID, testSpotRequestNewID, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// 2 running pool instances: 1 busy, 1 idle
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
						},
					},
				},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// running=2, busy=1 (only i-running1 intersects), ready=1, desired=1
	// Should NOT create new instances — stale records must be ignored
	if fleetCreateCalled != 0 {
		t.Errorf("Expected 0 fleet create calls (stale records should be ignored), got %d", fleetCreateCalled)
	}
}

func TestReconcilePoolNoScaleDownBusyInstances(t *testing.T) {
	// Test that reconciliation doesn't scale down busy instances
	// desired_ready=1, running=3, busy=2 -> ready=1 -> no scale-down needed
	terminateCalled := false
	stopCalled := false

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 1, // Desired ready instances
				DesiredStopped: 0,
				InstanceType:   "t3.medium",
			}, nil
		},
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) {
			return []string{"i-busy1", "i-busy2"}, nil // 2 instances are busy
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockFleet := &MockFleetAPI{}

	launchTime := time.Now().Add(-2 * time.Hour) // Old enough to be idle
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// 3 running instances: 2 busy + 1 idle
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-busy1"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
								LaunchTime:   &launchTime,
							},
							{
								InstanceId:   aws.String("i-busy2"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
								LaunchTime:   &launchTime,
							},
							{
								InstanceId:   aws.String("i-idle"),
								InstanceType: ec2types.InstanceTypeT3Medium,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
								LaunchTime:   &launchTime,
							},
						},
					},
				},
			}, nil
		},
		TerminateInstancesFunc: func(_ context.Context, _ *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			terminateCalled = true
			return &ec2.TerminateInstancesOutput{}, nil
		},
		StopInstancesFunc: func(_ context.Context, _ *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			stopCalled = true
			return &ec2.StopInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// ready=1 (3 running - 2 busy), desired_ready=1 -> no scale-down needed
	if terminateCalled || stopCalled {
		t.Error("Should not scale down when ready == desired_ready (busy instances protected)")
	}
}

func TestReconcilePoolBusyInstanceIDsError(t *testing.T) {
	// Test that reconciliation aborts when GetPoolBusyInstanceIDs fails
	fleetCreateCalled := false
	terminateCalled := false

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 0, // Would cause scale-down if busy=0
				DesiredStopped: 0,
				InstanceType:   "t3.medium",
			}, nil
		},
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) {
			return nil, errors.New("DynamoDB error")
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCalled = true
			return testInstanceNewID, testSpotRequestNewID, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// 2 running instances that would be scaled down if error was ignored
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
						},
					},
				},
			}, nil
		},
		TerminateInstancesFunc: func(_ context.Context, _ *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			terminateCalled = true
			return &ec2.TerminateInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should NOT scale down or create fleets when busy instance query fails
	if fleetCreateCalled {
		t.Error("Should not create fleet when GetPoolBusyInstanceIDs fails")
	}
	if terminateCalled {
		t.Error("Should not terminate instances when GetPoolBusyInstanceIDs fails - could kill busy instances")
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
								InstanceId:   aws.String(testInstanceStoppedID),
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
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	if !startInstancesCalled {
		t.Error("StartInstances should be called")
	}
	if len(startedIDs) != 1 || startedIDs[0] != testInstanceStoppedID {
		t.Errorf("expected to start %s, got %v", testInstanceStoppedID, startedIDs)
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
								InstanceId:   aws.String(testInstanceStoppedID),
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

func TestReconcilePoolCreateForWarmPool(t *testing.T) {
	// Test warm pool: desiredRunning=0, desiredStopped=1
	// Should create fleet instances to eventually become stopped
	fleetCreateCalled := 0

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"warm-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 0, // No hot standby
				DesiredStopped: 2, // Warm pool: 2 stopped instances
				InstanceType:   "t3.medium",
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, spec *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCalled++
			if spec.Pool != "warm-pool" {
				t.Errorf("expected pool warm-pool, got %s", spec.Pool)
			}
			return testInstanceNewID, testSpotRequestNewID, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// No instances initially
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should create 2 fleet instances to fill stopped pool deficit
	// (ready=0 >= desiredRunning=0, and stopped=0 < desiredStopped=2)
	if fleetCreateCalled != 2 {
		t.Errorf("expected 2 fleet create calls for warm pool deficit, got %d", fleetCreateCalled)
	}
}

func TestReconcilePoolWarmPoolImmediateStop(t *testing.T) {
	// Test that warm pool instances are stopped immediately without waiting for idle timeout
	// This is the key behavior: new instances should be stopped right away to become "warm"
	var stoppedIDs []string

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"warm-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning:     0,  // Warm pool: no hot standby
				DesiredStopped:     1,  // Warm pool: want 1 stopped instance
				IdleTimeoutMinutes: 30, // 30 min idle timeout (should be bypassed for warm pools)
				InstanceType:       "t3.medium",
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
		// Return no busy instances - the running instance is ready to be stopped
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) {
			return []string{}, nil
		},
	}

	mockFleet := &MockFleetAPI{}

	launchTime := time.Now() // Just launched - would NOT pass 30min idle timeout
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
								LaunchTime:   aws.Time(launchTime),
								Tags: []ec2types.Tag{
									{Key: aws.String("runs-fleet:pool"), Value: aws.String("warm-pool")},
								},
							},
						},
					},
				},
			}, nil
		},
		StopInstancesFunc: func(_ context.Context, input *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			stoppedIDs = append(stoppedIDs, input.InstanceIds...)
			return &ec2.StopInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Instance should be stopped immediately because:
	// - desiredRunning=0, desiredStopped=1 (warm pool)
	// - running=1, stopped=0
	// - ready=1 > desiredRunning=0 triggers scale down
	// - Warm pools bypass idle timeout check
	if len(stoppedIDs) != 1 {
		t.Errorf("expected 1 instance stopped immediately, got %d", len(stoppedIDs))
	}
	if len(stoppedIDs) > 0 && stoppedIDs[0] != "i-running1" {
		t.Errorf("expected i-running1 to be stopped, got %s", stoppedIDs[0])
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
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCount++
			return testInstanceNewID, testSpotRequestNewID, nil
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

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-priv1", "subnet-priv2"},
	})
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
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCount++
			return testInstanceNewID, testSpotRequestNewID, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String(testInstanceStoppedID),
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

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-priv1", "subnet-priv2"},
	})
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
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			return "", "", errors.New("fleet create error")
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-priv1", "subnet-priv2"},
	})
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
		return
	}
	if !existing.IdleSince.Equal(existingIdleTime) {
		t.Errorf("existing instance should retain idle time, got %v, want %v", existing.IdleSince, existingIdleTime)
	}

	// New running instance should have idle time initialized
	if newInst == nil {
		t.Fatal("new instance not found")
		return
	}
	if newInst.IdleSince.IsZero() {
		t.Error("new running instance should have idle time initialized")
	}

	// Stopped instance should not have idle time set
	if stopped == nil {
		t.Fatal("stopped instance not found")
		return
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
				DesiredRunning: 1, // Ignored - ephemeral pools always get desiredRunning=0
				DesiredStopped: 0, // Overridden by peak concurrency (3)
				InstanceType:   "c7g.xlarge",
				Ephemeral:      true,
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
		GetPoolP90ConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) {
			return 3, nil // Peak of 3 concurrent jobs
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCount++
			return fmt.Sprintf("i-new%d", fleetCreateCount), fmt.Sprintf("sir-new%d", fleetCreateCount), nil
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

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-priv1", "subnet-priv2"},
	})
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
				DesiredRunning: 0, // Ephemeral pools always have desiredRunning=0
				DesiredStopped: 2, // Fallback to this when peak query fails
				InstanceType:   "c7g.xlarge",
				Ephemeral:      true,
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
		GetPoolP90ConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) {
			return 0, errors.New("database error")
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCount++
			return fmt.Sprintf("i-new%d", fleetCreateCount), fmt.Sprintf("sir-new%d", fleetCreateCount), nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-priv1", "subnet-priv2"},
	})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should fallback to DesiredStopped of 2 when peak fails (ephemeral pools always have desiredRunning=0)
	if fleetCreateCount != 2 {
		t.Errorf("expected 2 fleet creates (fallback to DesiredStopped), got %d", fleetCreateCount)
	}
}

//nolint:dupl // Test cases have intentionally similar structure with different data
func TestReconcileEphemeralPoolLastJobTimeKeepsMinimum(t *testing.T) {
	fleetCreateCount := 0

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"ephemeral-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:       "ephemeral-pool",
				DesiredRunning: 0, // Default is 0
				DesiredStopped: 0,
				InstanceType:   "c7g.xlarge",
				Ephemeral:      true,
				LastJobTime:    time.Now().Add(-2 * time.Hour), // Last job 2 hours ago (within 4h window)
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
		GetPoolP90ConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) {
			return 0, nil // No jobs in 1-hour peak window
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCount++
			return fmt.Sprintf("i-new%d", fleetCreateCount), fmt.Sprintf("sir-new%d", fleetCreateCount), nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-priv1", "subnet-priv2"},
	})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should create 1 instance because LastJobTime is within 4-hour window
	if fleetCreateCount != 1 {
		t.Errorf("expected 1 fleet create (minimum from LastJobTime), got %d", fleetCreateCount)
	}
}

//nolint:dupl // Test cases have intentionally similar structure with different data
func TestReconcileEphemeralPoolLastJobTimeExpired(t *testing.T) {
	fleetCreateCount := 0

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"ephemeral-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:       "ephemeral-pool",
				DesiredRunning: 0, // Default is 0
				DesiredStopped: 0,
				InstanceType:   "c7g.xlarge",
				Ephemeral:      true,
				LastJobTime:    time.Now().Add(-5 * time.Hour), // Last job 5 hours ago (outside 4h window)
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
		GetPoolP90ConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) {
			return 0, nil // No jobs in 1-hour peak window
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCount++
			return fmt.Sprintf("i-new%d", fleetCreateCount), fmt.Sprintf("sir-new%d", fleetCreateCount), nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-priv1", "subnet-priv2"},
	})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should NOT create any instance because LastJobTime is outside 4-hour window
	if fleetCreateCount != 0 {
		t.Errorf("expected 0 fleet creates (LastJobTime expired), got %d", fleetCreateCount)
	}
}

//nolint:dupl // Test cases have intentionally similar structure with different data
func TestReconcileEphemeralPoolPeakErrorWithRecentActivity(t *testing.T) {
	fleetCreateCount := 0

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"ephemeral-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:       "ephemeral-pool",
				DesiredRunning: 0, // Default is 0
				DesiredStopped: 0,
				InstanceType:   "c7g.xlarge",
				Ephemeral:      true,
				LastJobTime:    time.Now().Add(-2 * time.Hour), // Last job 2 hours ago (within 4h window)
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
		GetPoolP90ConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) {
			return 0, errors.New("database error") // Peak query fails
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			fleetCreateCount++
			return fmt.Sprintf("i-new%d", fleetCreateCount), fmt.Sprintf("sir-new%d", fleetCreateCount), nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-priv1", "subnet-priv2"},
	})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Should still create 1 instance because LastJobTime is within window, despite peak query failure
	if fleetCreateCount != 1 {
		t.Errorf("expected 1 fleet create (fallback to LastJobTime), got %d", fleetCreateCount)
	}
}

func TestGetAvailableInstance_StoppedInstance(t *testing.T) {
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-running1"),
								InstanceType: ec2types.InstanceTypeC7gXlarge,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
							{
								InstanceId:   aws.String(testInstanceStoppedID),
								InstanceType: ec2types.InstanceTypeC7gXlarge,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
	}

	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	instance, err := manager.GetAvailableInstance(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if instance == nil {
		t.Fatal("expected instance, got nil")
	}
	if instance.InstanceID != testInstanceStoppedID {
		t.Errorf("expected stopped instance %s, got %s", testInstanceStoppedID, instance.InstanceID)
	}
	if instance.State != testStateStopped {
		t.Errorf("expected state stopped, got %s", instance.State)
	}
}

func TestGetAvailableInstance_NoStoppedInstances(t *testing.T) {
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String("i-running1"),
								InstanceType: ec2types.InstanceTypeC7gXlarge,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
							},
						},
					},
				},
			}, nil
		},
	}

	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	instance, err := manager.GetAvailableInstance(context.Background(), "test-pool")
	if !errors.Is(err, ErrNoAvailableInstance) {
		t.Errorf("expected ErrNoAvailableInstance, got %v", err)
	}
	if instance != nil {
		t.Errorf("expected nil instance, got %v", instance)
	}
}

func TestGetAvailableInstance_NoEC2Client(t *testing.T) {
	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	// Don't set EC2 client

	_, err := manager.GetAvailableInstance(context.Background(), "test-pool")
	if err == nil {
		t.Error("expected error when EC2 client not configured")
	}
}

func TestStartInstanceForJob(t *testing.T) {
	startCalled := false
	var startedInstanceIDs []string

	mockEC2 := &MockEC2API{
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			startCalled = true
			startedInstanceIDs = params.InstanceIds
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	// Mark instance as idle first
	manager.MarkInstanceIdle("i-test123")

	err := manager.StartInstanceForJob(context.Background(), "i-test123", "owner/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !startCalled {
		t.Error("StartInstances was not called")
	}
	if len(startedInstanceIDs) != 1 || startedInstanceIDs[0] != "i-test123" {
		t.Errorf("wrong instance IDs: %v", startedInstanceIDs)
	}

	// Verify instance is no longer tracked as idle
	manager.mu.RLock()
	_, isIdle := manager.instanceIdle["i-test123"]
	manager.mu.RUnlock()
	if isIdle {
		t.Error("instance should not be in idle map after StartInstanceForJob")
	}
}

func TestStartInstanceForJob_NoEC2Client(t *testing.T) {
	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	// Don't set EC2 client

	err := manager.StartInstanceForJob(context.Background(), "i-test123", "owner/repo")
	if err == nil {
		t.Error("expected error when EC2 client not configured")
	}
}

func TestStartInstanceForJob_CreatesRoleTag(t *testing.T) {
	const instanceID = "i-roletagtest"
	startCalled := false
	createTagsCalled := false
	var taggedInstanceID string
	var taggedRepo string

	mockEC2 := &MockEC2API{
		StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			startCalled = true
			return &ec2.StartInstancesOutput{}, nil
		},
		CreateTagsFunc: func(_ context.Context, params *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			createTagsCalled = true
			if len(params.Resources) > 0 {
				taggedInstanceID = params.Resources[0]
			}
			for _, tag := range params.Tags {
				if *tag.Key == "Role" {
					taggedRepo = *tag.Value
				}
			}
			return &ec2.CreateTagsOutput{}, nil
		},
	}

	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)
	manager.MarkInstanceIdle(instanceID)

	err := manager.StartInstanceForJob(context.Background(), instanceID, "owner/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !startCalled {
		t.Error("StartInstances was not called")
	}
	if !createTagsCalled {
		t.Error("CreateTags was not called")
	}
	if taggedInstanceID != instanceID {
		t.Errorf("tagged wrong instance: got %q, want %q", taggedInstanceID, instanceID)
	}
	if taggedRepo != "owner/repo" {
		t.Errorf("tagged wrong repo: got %q, want %q", taggedRepo, "owner/repo")
	}
}

func TestStartInstanceForJob_EmptyRepoSkipsTag(t *testing.T) {
	const instanceID = "i-emptyrepotest"
	startCalled := false
	createTagsCalled := false

	mockEC2 := &MockEC2API{
		StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			startCalled = true
			return &ec2.StartInstancesOutput{}, nil
		},
		CreateTagsFunc: func(_ context.Context, _ *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			createTagsCalled = true
			return &ec2.CreateTagsOutput{}, nil
		},
	}

	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)
	manager.MarkInstanceIdle(instanceID)

	err := manager.StartInstanceForJob(context.Background(), instanceID, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !startCalled {
		t.Error("StartInstances was not called")
	}
	if createTagsCalled {
		t.Error("CreateTags should not be called when repo is empty")
	}
}

func TestStartInstanceForJob_CreateTagsError_NonFatal(t *testing.T) {
	const instanceID = "i-tagerrortest"
	startCalled := false
	createTagsCalled := false

	mockEC2 := &MockEC2API{
		StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			startCalled = true
			return &ec2.StartInstancesOutput{}, nil
		},
		CreateTagsFunc: func(_ context.Context, _ *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			createTagsCalled = true
			return nil, errors.New("tagging failed")
		},
	}

	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)
	manager.MarkInstanceIdle(instanceID)

	// Should succeed even if CreateTags fails - tagging is non-critical
	err := manager.StartInstanceForJob(context.Background(), instanceID, "owner/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !startCalled {
		t.Error("StartInstances was not called")
	}
	if !createTagsCalled {
		t.Error("CreateTags was not called")
	}
}

func TestClaimAndStartPoolInstance_Success(t *testing.T) {
	startCalled := false
	describeCalled := false
	claimCalled := false

	mockDB := &MockDBClient{
		ClaimInstanceForJobFunc: func(_ context.Context, instanceID string, jobID int64, _ time.Duration) error {
			claimCalled = true
			if instanceID != testInstanceStoppedID {
				t.Errorf("wrong instance claimed: %s", instanceID)
			}
			if jobID != 12345 {
				t.Errorf("wrong jobID: %d", jobID)
			}
			return nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			describeCalled = true
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String(testInstanceStoppedID),
								InstanceType: ec2types.InstanceTypeC7gXlarge,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			startCalled = true
			if len(params.InstanceIds) != 1 || params.InstanceIds[0] != testInstanceStoppedID {
				t.Errorf("wrong instance started: %v", params.InstanceIds)
			}
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	instance, err := manager.ClaimAndStartPoolInstance(context.Background(), "test-pool", 12345, "owner/repo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !describeCalled {
		t.Error("DescribeInstances was not called")
	}
	if !claimCalled {
		t.Error("ClaimInstanceForJob was not called")
	}
	if !startCalled {
		t.Error("StartInstances was not called")
	}
	if instance == nil {
		t.Fatal("expected instance, got nil")
	}
	if instance.InstanceID != testInstanceStoppedID {
		t.Errorf("expected instance %s, got %s", testInstanceStoppedID, instance.InstanceID)
	}

	// Verify instance is marked as busy (not in idle map)
	manager.mu.RLock()
	_, isIdle := manager.instanceIdle[testInstanceStoppedID]
	manager.mu.RUnlock()
	if isIdle {
		t.Error("instance should not be in idle map after ClaimAndStartPoolInstance")
	}
}

func TestClaimAndStartPoolInstance_NoInstance(t *testing.T) {
	mockDB := &MockDBClient{}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{},
			}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	instance, err := manager.ClaimAndStartPoolInstance(context.Background(), "test-pool", 12345, "owner/repo", nil)
	if !errors.Is(err, ErrNoAvailableInstance) {
		t.Errorf("expected ErrNoAvailableInstance, got %v", err)
	}
	if instance != nil {
		t.Errorf("expected nil instance, got %v", instance)
	}
}

func TestClaimAndStartPoolInstance_StartError(t *testing.T) {
	releaseCalled := false
	mockDB := &MockDBClient{
		ClaimInstanceForJobFunc: func(_ context.Context, _ string, _ int64, _ time.Duration) error {
			return nil
		},
		ReleaseInstanceClaimFunc: func(_ context.Context, instanceID string, jobID int64) error {
			releaseCalled = true
			if instanceID != testInstanceStoppedID {
				t.Errorf("wrong instance released: %s", instanceID)
			}
			if jobID != 12345 {
				t.Errorf("wrong jobID released: %d", jobID)
			}
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
								InstanceId:   aws.String(testInstanceStoppedID),
								InstanceType: ec2types.InstanceTypeC7gXlarge,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
		StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			return nil, errors.New("start failed")
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	instance, err := manager.ClaimAndStartPoolInstance(context.Background(), "test-pool", 12345, "owner/repo", nil)
	if err == nil {
		t.Error("expected error when start fails")
	}
	if instance != nil {
		t.Errorf("expected nil instance when start fails, got %v", instance)
	}
	if !releaseCalled {
		t.Error("ReleaseInstanceClaim should be called when start fails")
	}
}

func TestClaimAndStartPoolInstance_ClaimConflict(t *testing.T) {
	// Test that when first instance is already claimed, it tries the next one
	claimAttempts := 0
	mockDB := &MockDBClient{
		ClaimInstanceForJobFunc: func(_ context.Context, instanceID string, _ int64, _ time.Duration) error {
			claimAttempts++
			if instanceID == "i-stopped1" {
				return db.ErrInstanceAlreadyClaimed
			}
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
								InstanceId:   aws.String("i-stopped1"),
								InstanceType: ec2types.InstanceTypeC7gXlarge,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
							{
								InstanceId:   aws.String("i-stopped2"),
								InstanceType: ec2types.InstanceTypeC7gXlarge,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			// Should be starting the second instance
			if len(params.InstanceIds) != 1 || params.InstanceIds[0] != "i-stopped2" {
				t.Errorf("expected i-stopped2 to be started, got %v", params.InstanceIds)
			}
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	instance, err := manager.ClaimAndStartPoolInstance(context.Background(), "test-pool", 12345, "owner/repo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if instance == nil {
		t.Fatal("expected instance, got nil")
	}
	if instance.InstanceID != "i-stopped2" {
		t.Errorf("expected i-stopped2, got %s", instance.InstanceID)
	}
	if claimAttempts != 2 {
		t.Errorf("expected 2 claim attempts, got %d", claimAttempts)
	}
}

func TestClaimAndStartPoolInstance_NilDBClient(t *testing.T) {
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{
						Instances: []ec2types.Instance{
							{
								InstanceId:   aws.String(testInstanceStoppedID),
								InstanceType: ec2types.InstanceTypeC7gXlarge,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
	}

	manager := NewManager(nil, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	_, err := manager.ClaimAndStartPoolInstance(context.Background(), "test-pool", 12345, "owner/repo", nil)
	if err == nil {
		t.Error("expected error when DB client is nil")
	}
}

func TestStopPoolInstance_Success(t *testing.T) {
	stopCalled := false

	mockEC2 := &MockEC2API{
		StopInstancesFunc: func(_ context.Context, params *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			stopCalled = true
			if len(params.InstanceIds) != 1 || params.InstanceIds[0] != "i-test123" {
				t.Errorf("wrong instance stopped: %v", params.InstanceIds)
			}
			return &ec2.StopInstancesOutput{}, nil
		},
	}

	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	// Mark instance as idle first
	manager.MarkInstanceIdle("i-test123")

	err := manager.StopPoolInstance(context.Background(), "i-test123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stopCalled {
		t.Error("StopInstances was not called")
	}

	// Verify instance is removed from idle tracking
	manager.mu.RLock()
	_, isIdle := manager.instanceIdle["i-test123"]
	manager.mu.RUnlock()
	if isIdle {
		t.Error("instance should be removed from idle map after StopPoolInstance")
	}
}

func TestStopPoolInstance_NoEC2Client(t *testing.T) {
	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	// Don't set EC2 client

	err := manager.StopPoolInstance(context.Background(), "i-test123")
	if err == nil {
		t.Error("expected error when EC2 client not configured")
	}
}

func TestStopPoolInstance_Error(t *testing.T) {
	mockEC2 := &MockEC2API{
		StopInstancesFunc: func(_ context.Context, _ *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			return nil, errors.New("stop failed")
		},
	}

	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	err := manager.StopPoolInstance(context.Background(), "i-test123")
	if err == nil {
		t.Error("expected error when stop fails")
	}
}

func TestClaimAndStartPoolInstance_ConcurrentClaims(t *testing.T) {
	// Test that when multiple goroutines attempt to claim the same instance,
	// only one succeeds and the other either fails or gets a different instance
	claimCount := 0
	startCount := 0

	mockDB := &MockDBClient{
		ClaimInstanceForJobFunc: func(_ context.Context, _ string, _ int64, _ time.Duration) error {
			claimCount++
			// First claim succeeds, subsequent claims fail with conflict
			if claimCount > 1 {
				return db.ErrInstanceAlreadyClaimed
			}
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
								InstanceId:   aws.String("i-only-one"),
								InstanceType: ec2types.InstanceTypeC7gXlarge,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
		StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			startCount++
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	// Run two concurrent claims
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(jobID int64) {
			_, err := manager.ClaimAndStartPoolInstance(context.Background(), "test-pool", jobID, "owner/repo", nil)
			results <- err
		}(int64(12345 + i))
	}

	// Collect results
	var successCount, errorCount int
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			successCount++
		} else if errors.Is(err, ErrNoAvailableInstance) {
			errorCount++
		}
	}

	// With only one instance available, one should succeed and one should fail
	if successCount != 1 {
		t.Errorf("expected exactly 1 success, got %d", successCount)
	}
	if errorCount != 1 {
		t.Errorf("expected exactly 1 ErrNoAvailableInstance error, got %d", errorCount)
	}
	// Instance should only be started once
	if startCount != 1 {
		t.Errorf("expected instance to be started once, got %d starts", startCount)
	}
}

func TestClaimAndStartPoolInstance_DBClaimFailure(t *testing.T) {
	// Test that a non-conflict DB error (e.g., network error) stops iteration
	// and returns the error instead of trying next instance
	dbError := errors.New("DynamoDB network error")

	mockDB := &MockDBClient{
		ClaimInstanceForJobFunc: func(_ context.Context, _ string, _ int64, _ time.Duration) error {
			return dbError
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
								InstanceType: ec2types.InstanceTypeC7gXlarge,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
							{
								InstanceId:   aws.String("i-stopped2"),
								InstanceType: ec2types.InstanceTypeC7gXlarge,
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	_, err := manager.ClaimAndStartPoolInstance(context.Background(), "test-pool", 12345, "owner/repo", nil)
	if err == nil {
		t.Fatal("expected error when DB claim fails with non-conflict error")
	}
	// Error should mention the DB error, not ErrNoAvailableInstance
	if errors.Is(err, ErrNoAvailableInstance) {
		t.Error("should return DB error, not ErrNoAvailableInstance")
	}
}

//nolint:dupl // Test cases have intentionally similar structure with different data
func TestReconcilePoolOrphanedJobsDontBlockScaleDown(t *testing.T) {
	// Orphaned job records (from terminated instances) must NOT prevent stopping
	// idle running instances in warm pools (desiredRunning=0).
	// This is the critical warm pool invariant: running instances should either
	// be busy (running a job) or get stopped immediately.
	var stoppedInstances []string

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"warm-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 0, // Warm pool: no hot standby
				DesiredStopped: 5, // Pool capacity
				InstanceType:   "t3.medium",
			}, nil
		},
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) {
			// 5 orphaned job records from terminated instances - these should NOT
			// prevent scale-down of the 3 idle running instances
			return []string{"i-dead1", "i-dead2", "i-dead3", "i-dead4", "i-dead5"}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// 3 running (all idle - no matching busy IDs), 2 stopped
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
					{InstanceId: aws.String("i-idle1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-idle2"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-idle3"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-stopped1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
					{InstanceId: aws.String("i-stopped2"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
				}}},
			}, nil
		},
		StopInstancesFunc: func(_ context.Context, input *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			stoppedInstances = append(stoppedInstances, input.InstanceIds...)
			return &ec2.StopInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{PrivateSubnetIDs: []string{"subnet-1"}})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// running=3, stopped=2, busy=0 (orphaned IDs don't match any running instances)
	// ready=3-0=3, excess=3-0=3, all 3 idle instances should be stopped
	if len(stoppedInstances) != 3 {
		t.Errorf("Expected 3 instances stopped (orphaned jobs must not block scale-down), got %d: %v",
			len(stoppedInstances), stoppedInstances)
	}
}

func TestReconcilePoolIdleRunningInstancesGetStopped(t *testing.T) {
	// Warm pool invariant: running instances that are not busy must be stopped.
	// This test verifies that idle running instances are returned to stopped state.
	var stoppedInstances []string

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"warm-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{DesiredRunning: 0, DesiredStopped: 3, InstanceType: "t3.medium"}, nil
		},
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) {
			return []string{"i-busy"}, nil // 1 instance is busy
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error { return nil },
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// 3 running: 1 busy + 2 idle; 1 stopped
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
					{InstanceId: aws.String("i-busy"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-idle1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-idle2"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-stopped1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
				}}},
			}, nil
		},
		StopInstancesFunc: func(_ context.Context, input *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			stoppedInstances = append(stoppedInstances, input.InstanceIds...)
			return &ec2.StopInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{PrivateSubnetIDs: []string{"subnet-1"}})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// running=3, busy=1 (i-busy matches), ready=2, excess=2
	// Both idle instances should be stopped, busy instance protected
	if len(stoppedInstances) != 2 {
		t.Errorf("Expected 2 idle instances stopped, got %d: %v", len(stoppedInstances), stoppedInstances)
	}
	for _, id := range stoppedInstances {
		if id == "i-busy" {
			t.Error("Busy instance i-busy should NOT be stopped")
		}
	}
}

//nolint:dupl // Test cases have intentionally similar structure with different data
func TestReconcilePoolBusyCountUsesInstanceIntersection(t *testing.T) {
	// Busy count must be calculated by intersecting job records with actual
	// running pool instances. Job records for non-existent instances (orphaned)
	// must not inflate the busy count.
	//
	// This test verifies that idle instances get stopped despite orphaned job
	// records - the critical behavior that was broken before the fix.
	var stoppedInstances []string

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"test-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 0,
				DesiredStopped: 5,
				InstanceType:   "t3.medium",
			}, nil
		},
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) {
			// 1 real busy instance + 10 orphaned job records
			return []string{
				"i-running1", // matches actual running instance
				"i-orphan1", "i-orphan2", "i-orphan3", "i-orphan4", "i-orphan5",
				"i-orphan6", "i-orphan7", "i-orphan8", "i-orphan9", "i-orphan10",
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			return testInstanceNewID, testSpotRequestNewID, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// 3 running instances: 1 busy (i-running1), 2 idle
			// 2 stopped instances
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
					{InstanceId: aws.String("i-running1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-running2"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-running3"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-stopped1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
					{InstanceId: aws.String("i-stopped2"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
				}}},
			}, nil
		},
		StopInstancesFunc: func(_ context.Context, input *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			stoppedInstances = append(stoppedInstances, input.InstanceIds...)
			return &ec2.StopInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// Correct calculation:
	// running=3, stopped=2
	// busy = intersection of running instances with busyIDs = 1 (only i-running1)
	// ready = 3 - 1 = 2
	// ready (2) > desiredRunning (0), excess=2

	// INCORRECT calculation (the bug we're fixing):
	// busy = max(11, 1) = 11 (inflated by orphaned job count)
	// ready = 3 - 11 = -8, clamped to 0
	// ready (0) > desiredRunning (0) = false, NO scale-down!

	// Critical: 2 idle instances MUST be stopped (orphaned jobs must not block)
	if len(stoppedInstances) != 2 {
		t.Errorf("Expected 2 instances stopped (busy=1 from intersection, not 11 from job count), got %d: %v",
			len(stoppedInstances), stoppedInstances)
	}
}

func TestReconcilePoolMixedOrphanedAndRealJobs(t *testing.T) {
	// Test scenario with a mix of orphaned jobs and real running jobs.
	// Only real jobs (on existing running instances) should count as busy.
	var stoppedInstances []string

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"mixed-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				DesiredRunning: 0,
				DesiredStopped: 10,
				InstanceType:   "t3.medium",
			}, nil
		},
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) {
			// 2 real busy + 3 orphaned
			return []string{
				"i-busy1", "i-busy2",                   // real - match running instances
				"i-orphan1", "i-orphan2", "i-orphan3", // orphaned - instances don't exist
			}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error {
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			return testInstanceNewID, testSpotRequestNewID, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// 5 running: 2 busy + 3 idle; 3 stopped
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
					{InstanceId: aws.String("i-busy1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-busy2"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-idle1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-idle2"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-idle3"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
					{InstanceId: aws.String("i-stopped1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
					{InstanceId: aws.String("i-stopped2"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
					{InstanceId: aws.String("i-stopped3"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
				}}},
			}, nil
		},
		StopInstancesFunc: func(_ context.Context, input *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			stoppedInstances = append(stoppedInstances, input.InstanceIds...)
			return &ec2.StopInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	// running=5, stopped=3
	// busy = 2 (only i-busy1, i-busy2 are real - orphaned don't match any instances)
	// ready = 5 - 2 = 3
	// excess = 3 - 0 = 3
	// All 3 idle instances should be stopped

	if len(stoppedInstances) != 3 {
		t.Errorf("Expected 3 idle instances stopped (busy=2 real, not 5 total), got %d: %v",
			len(stoppedInstances), stoppedInstances)
	}

	// Verify busy instances were NOT stopped
	for _, id := range stoppedInstances {
		if id == "i-busy1" || id == "i-busy2" {
			t.Errorf("Busy instance %s should NOT be stopped", id)
		}
	}
}

// TestTerminateInstancesWithSpotCancellation tests spot request cancellation during termination.
func TestTerminateInstancesWithSpotCancellation(t *testing.T) {
	tests := []struct {
		name                       string
		instanceIDs                []string
		spotRequestInfo            map[string]db.SpotRequestInfo
		getSpotRequestIDsError     error
		cancelSpotError            error
		wantCancelSpotRequestIDs   []string
		wantCancelSpotRequestsCall bool
	}{
		{
			name:        "cancels persistent spot requests after termination",
			instanceIDs: []string{"i-123", "i-456"},
			spotRequestInfo: map[string]db.SpotRequestInfo{
				"i-123": {InstanceID: "i-123", SpotRequestID: "sir-aaa", Persistent: true},
				"i-456": {InstanceID: "i-456", SpotRequestID: "sir-bbb", Persistent: true},
			},
			wantCancelSpotRequestIDs:   []string{"sir-aaa", "sir-bbb"},
			wantCancelSpotRequestsCall: true,
		},
		{
			name:        "skips non-persistent spot requests",
			instanceIDs: []string{"i-123", "i-456"},
			spotRequestInfo: map[string]db.SpotRequestInfo{
				"i-123": {InstanceID: "i-123", SpotRequestID: "sir-aaa", Persistent: true},
				"i-456": {InstanceID: "i-456", SpotRequestID: "sir-bbb", Persistent: false},
			},
			wantCancelSpotRequestIDs:   []string{"sir-aaa"},
			wantCancelSpotRequestsCall: true,
		},
		{
			name:        "skips instances with empty spot request ID",
			instanceIDs: []string{"i-123", "i-456"},
			spotRequestInfo: map[string]db.SpotRequestInfo{
				"i-123": {InstanceID: "i-123", SpotRequestID: "sir-aaa", Persistent: true},
				"i-456": {InstanceID: "i-456", SpotRequestID: "", Persistent: true},
			},
			wantCancelSpotRequestIDs:   []string{"sir-aaa"},
			wantCancelSpotRequestsCall: true,
		},
		{
			name:                       "handles no spot requests gracefully",
			instanceIDs:                []string{"i-123", "i-456"},
			spotRequestInfo:            map[string]db.SpotRequestInfo{},
			wantCancelSpotRequestsCall: false,
		},
		{
			name:                       "handles GetSpotRequestIDs error gracefully",
			instanceIDs:                []string{"i-123"},
			getSpotRequestIDsError:     errors.New("db error"),
			wantCancelSpotRequestsCall: false,
		},
		{
			name:        "continues on CancelSpotInstanceRequests error",
			instanceIDs: []string{"i-123"},
			spotRequestInfo: map[string]db.SpotRequestInfo{
				"i-123": {InstanceID: "i-123", SpotRequestID: "sir-aaa", Persistent: true},
			},
			cancelSpotError:            errors.New("cancel error"),
			wantCancelSpotRequestIDs:   []string{"sir-aaa"},
			wantCancelSpotRequestsCall: true,
		},
		{
			name:        "handles all instances being non-persistent",
			instanceIDs: []string{"i-123", "i-456"},
			spotRequestInfo: map[string]db.SpotRequestInfo{
				"i-123": {InstanceID: "i-123", SpotRequestID: "sir-aaa", Persistent: false},
				"i-456": {InstanceID: "i-456", SpotRequestID: "sir-bbb", Persistent: false},
			},
			wantCancelSpotRequestsCall: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cancelSpotCalled bool
			var canceledSpotRequestIDs []string

			mockDB := &MockDBClient{
				GetSpotRequestIDsFunc: func(_ context.Context, _ []string) (map[string]db.SpotRequestInfo, error) {
					if tt.getSpotRequestIDsError != nil {
						return nil, tt.getSpotRequestIDsError
					}
					return tt.spotRequestInfo, nil
				},
			}

			mockEC2 := &MockEC2API{
				TerminateInstancesFunc: func(_ context.Context, _ *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
					return &ec2.TerminateInstancesOutput{}, nil
				},
				CancelSpotInstanceRequestsFunc: func(_ context.Context, params *ec2.CancelSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
					cancelSpotCalled = true
					canceledSpotRequestIDs = params.SpotInstanceRequestIds
					if tt.cancelSpotError != nil {
						return nil, tt.cancelSpotError
					}
					return &ec2.CancelSpotInstanceRequestsOutput{}, nil
				},
			}

			manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
			manager.SetEC2Client(mockEC2)

			err := manager.terminateInstances(context.Background(), tt.instanceIDs)
			if err != nil {
				t.Fatalf("terminateInstances() unexpected error: %v", err)
			}

			if cancelSpotCalled != tt.wantCancelSpotRequestsCall {
				t.Errorf("CancelSpotInstanceRequests called = %v, want %v", cancelSpotCalled, tt.wantCancelSpotRequestsCall)
			}

			if tt.wantCancelSpotRequestsCall && tt.wantCancelSpotRequestIDs != nil {
				if len(canceledSpotRequestIDs) != len(tt.wantCancelSpotRequestIDs) {
					t.Errorf("cancelled spot request IDs count = %d, want %d",
						len(canceledSpotRequestIDs), len(tt.wantCancelSpotRequestIDs))
				}

				wantSet := make(map[string]bool)
				for _, id := range tt.wantCancelSpotRequestIDs {
					wantSet[id] = true
				}
				for _, id := range canceledSpotRequestIDs {
					if !wantSet[id] {
						t.Errorf("unexpected spot request ID cancelled: %s", id)
					}
				}
			}
		})
	}
}

// TestCreatePoolFleetInstancesSavesSpotRequestID tests that spot request IDs are saved during instance creation.
// CreateSpotInstance returns the spot request ID directly (no separate query needed).
func TestCreatePoolFleetInstancesSavesSpotRequestID(t *testing.T) {
	tests := []struct {
		name             string
		count            int
		instanceID       string
		spotRequestID    string
		saveSpotError    error
		wantSavedSpotIDs map[string]string
	}{
		{
			name:          "saves spot request ID from CreateSpotInstance",
			count:         1,
			instanceID:    "i-123",
			spotRequestID: "sir-aaa",
			wantSavedSpotIDs: map[string]string{
				"i-123": "sir-aaa",
			},
		},
		{
			name:             "handles SaveSpotRequestID error gracefully",
			count:            1,
			instanceID:       "i-123",
			spotRequestID:    "sir-aaa",
			saveSpotError:    errors.New("save error"),
			wantSavedSpotIDs: map[string]string{},
		},
		{
			name:             "skips save when spot request ID is empty",
			count:            1,
			instanceID:       "i-123",
			spotRequestID:    "",
			wantSavedSpotIDs: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			savedSpotIDs := make(map[string]string)

			mockDB := &MockDBClient{
				SaveSpotRequestIDFunc: func(_ context.Context, instanceID, spotRequestID string, persistent bool) error {
					if tt.saveSpotError != nil {
						return tt.saveSpotError
					}
					if !persistent {
						t.Errorf("SaveSpotRequestID called with persistent=false for pool instance")
					}
					savedSpotIDs[instanceID] = spotRequestID
					return nil
				},
			}

			mockFleet := &MockFleetAPI{
				CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
					return tt.instanceID, tt.spotRequestID, nil
				},
			}

			manager := NewManager(mockDB, mockFleet, &config.Config{
				PrivateSubnetIDs: []string{"subnet-1"},
			})
			manager.SetEC2Client(&MockEC2API{})

			poolConfig := &db.PoolConfig{
				PoolName:       "test-pool",
				InstanceType:   "t4g.medium",
				DesiredRunning: 2,
			}

			created := manager.createPoolFleetInstances(context.Background(), "test-pool", tt.count, poolConfig)
			if created != tt.count {
				t.Fatalf("createPoolFleetInstances() created %d instances, want %d", created, tt.count)
			}

			for wantInstance, wantSpotID := range tt.wantSavedSpotIDs {
				if savedSpotIDs[wantInstance] != wantSpotID {
					t.Errorf("SaveSpotRequestID for %s = %q, want %q",
						wantInstance, savedSpotIDs[wantInstance], wantSpotID)
				}
			}

			if len(savedSpotIDs) != len(tt.wantSavedSpotIDs) {
				t.Errorf("SaveSpotRequestID called for %d instances, want %d",
					len(savedSpotIDs), len(tt.wantSavedSpotIDs))
			}
		})
	}
}

func TestCreatePoolFleetInstances_PartialSuccess(t *testing.T) {
	callCount := 0
	savedSpotIDs := make(map[string]string)

	mockDB := &MockDBClient{
		SaveSpotRequestIDFunc: func(_ context.Context, instanceID, spotRequestID string, _ bool) error {
			savedSpotIDs[instanceID] = spotRequestID
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			callCount++
			if callCount == 2 {
				return "", "", errors.New("capacity error")
			}
			return fmt.Sprintf("i-ok%d", callCount), fmt.Sprintf("sir-ok%d", callCount), nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(&MockEC2API{})

	poolConfig := &db.PoolConfig{PoolName: "test-pool", InstanceType: "t4g.medium", DesiredRunning: 3}

	created := manager.createPoolFleetInstances(context.Background(), "test-pool", 3, poolConfig)
	if created != 2 {
		t.Errorf("created = %d, want 2 (1 failed out of 3)", created)
	}
	if callCount != 3 {
		t.Errorf("CreateSpotInstance called %d times, want 3", callCount)
	}
	if len(savedSpotIDs) != 2 {
		t.Errorf("SaveSpotRequestID called %d times, want 2 (skip failed instance)", len(savedSpotIDs))
	}
}

func TestCreatePoolFleetInstances_NoSubnets(t *testing.T) {
	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			t.Error("CreateSpotInstance should not be called when no subnets configured")
			return "", "", nil
		},
	}

	manager := NewManager(&MockDBClient{}, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{},
		PublicSubnetIDs:  []string{},
	})
	manager.SetEC2Client(&MockEC2API{})

	poolConfig := &db.PoolConfig{PoolName: "test-pool", InstanceType: "t4g.medium"}

	created := manager.createPoolFleetInstances(context.Background(), "test-pool", 2, poolConfig)
	if created != 0 {
		t.Errorf("created = %d, want 0 when no subnets", created)
	}
}

func TestCreatePoolFleetInstances_AllFail(t *testing.T) {
	callCount := 0

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			callCount++
			return "", "", errors.New("every call fails")
		},
	}

	manager := NewManager(&MockDBClient{}, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(&MockEC2API{})

	poolConfig := &db.PoolConfig{PoolName: "test-pool", InstanceType: "t4g.medium"}

	created := manager.createPoolFleetInstances(context.Background(), "test-pool", 3, poolConfig)
	if created != 0 {
		t.Errorf("created = %d, want 0 when all calls fail", created)
	}
	if callCount != 3 {
		t.Errorf("CreateSpotInstance called %d times, want 3 (should try all)", callCount)
	}
}

func TestCreatePoolFleetInstances_EmptySpotRequestID(t *testing.T) {
	saveCallCount := 0

	mockDB := &MockDBClient{
		SaveSpotRequestIDFunc: func(_ context.Context, _, _ string, _ bool) error {
			saveCallCount++
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			return "i-123", "", nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(&MockEC2API{})

	poolConfig := &db.PoolConfig{PoolName: "test-pool", InstanceType: "t4g.medium"}

	created := manager.createPoolFleetInstances(context.Background(), "test-pool", 1, poolConfig)
	if created != 1 {
		t.Errorf("created = %d, want 1", created)
	}
	if saveCallCount != 0 {
		t.Errorf("SaveSpotRequestID called %d times, want 0 when spotReqID is empty", saveCallCount)
	}
}

func TestCreatePoolFleetInstances_SpecFields(t *testing.T) {
	var capturedSpec *fleet.LaunchSpec

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, spec *fleet.LaunchSpec) (string, string, error) {
			capturedSpec = spec
			return testInstanceNewID, testSpotRequestNewID, nil
		},
	}

	manager := NewManager(&MockDBClient{}, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-priv1", "subnet-priv2"},
	})
	manager.SetEC2Client(&MockEC2API{})

	poolConfig := &db.PoolConfig{
		PoolName:     "my-pool",
		InstanceType: "c7g.xlarge",
		Arch:         "arm64",
	}

	manager.createPoolFleetInstances(context.Background(), "my-pool", 1, poolConfig)

	if capturedSpec == nil {
		t.Fatal("CreateSpotInstance was not called")
	}
	if capturedSpec.Pool != "my-pool" {
		t.Errorf("Pool = %q, want my-pool", capturedSpec.Pool)
	}
	if capturedSpec.InstanceType != "c7g.xlarge" {
		t.Errorf("InstanceType = %q, want c7g.xlarge", capturedSpec.InstanceType)
	}
	if capturedSpec.Arch != "arm64" {
		t.Errorf("Arch = %q, want arm64", capturedSpec.Arch)
	}
	if capturedSpec.SubnetID == "" {
		t.Error("SubnetID should not be empty")
	}
	if capturedSpec.RunID == 0 {
		t.Error("RunID should not be zero")
	}
	// Verify Spot and PersistentSpot are NOT set (CreateSpotInstance handles spot directly)
	if capturedSpec.Spot {
		t.Error("Spot should be false (CreateSpotInstance handles spot internally)")
	}
}

func TestCreatePoolFleetInstances_InstanceTypeCycling(t *testing.T) {
	var capturedTypes []string

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, spec *fleet.LaunchSpec) (string, string, error) {
			capturedTypes = append(capturedTypes, spec.InstanceType)
			return "i-" + spec.InstanceType, "sir-1", nil
		},
	}

	manager := NewManager(&MockDBClient{}, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(&MockEC2API{})

	poolConfig := &db.PoolConfig{
		PoolName: "test-pool",
		CPUMin:   4, CPUMax: 8,
		Arch: "arm64",
	}

	created := manager.createPoolFleetInstances(context.Background(), "test-pool", 5, poolConfig)
	if created != 5 {
		t.Fatalf("created = %d, want 5", created)
	}

	if len(capturedTypes) != 5 {
		t.Fatalf("got %d calls, want 5", len(capturedTypes))
	}

	// With multiple resolved types, instances should cycle (not all the same)
	allSame := true
	for _, typ := range capturedTypes[1:] {
		if typ != capturedTypes[0] {
			allSame = false
			break
		}
	}
	// resolvePoolInstanceTypes with CPUMin/Max returns multiple types for arm64
	// If multiple types available, cycling should produce variety
	types, _ := resolvePoolInstanceTypes(poolConfig)
	if len(types) > 1 && allSame {
		t.Errorf("all 5 instances used same type %q despite %d types available", capturedTypes[0], len(types))
	}
}

func TestCreatePoolFleetInstances_SaveSpotRequestIDError(t *testing.T) {
	saveErrors := 0

	mockDB := &MockDBClient{
		SaveSpotRequestIDFunc: func(_ context.Context, _, _ string, _ bool) error {
			saveErrors++
			return errors.New("dynamo throttle")
		},
	}

	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			return testInstanceNewID, testSpotRequestNewID, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(&MockEC2API{})

	poolConfig := &db.PoolConfig{PoolName: "test-pool", InstanceType: "t4g.medium"}

	created := manager.createPoolFleetInstances(context.Background(), "test-pool", 2, poolConfig)
	// Instance creation succeeds even if spot ID save fails
	if created != 2 {
		t.Errorf("created = %d, want 2 (save failures don't block creation)", created)
	}
	if saveErrors != 2 {
		t.Errorf("SaveSpotRequestID errors = %d, want 2", saveErrors)
	}
}

func TestCreatePoolFleetInstances_EmptyInstanceTypes(t *testing.T) {
	mockFleet := &MockFleetAPI{
		CreateSpotInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, string, error) {
			t.Error("CreateSpotInstance should not be called when no instance types resolved")
			return "", "", nil
		},
	}

	manager := NewManager(&MockDBClient{}, mockFleet, &config.Config{
		PrivateSubnetIDs: []string{"subnet-1"},
	})
	manager.SetEC2Client(&MockEC2API{})

	// Empty InstanceType should fail resolvePoolInstanceTypes
	poolConfig := &db.PoolConfig{PoolName: "test-pool", InstanceType: ""}

	created := manager.createPoolFleetInstances(context.Background(), "test-pool", 2, poolConfig)
	if created != 0 {
		t.Errorf("created = %d, want 0 when no instance types resolved", created)
	}
}

// TestClaimAndStartPoolInstance_WithSpec tests that spec filtering selects correct instances.
func TestClaimAndStartPoolInstance_WithSpec(t *testing.T) {
	tests := []struct {
		name             string
		instances        []ec2types.Instance
		spec             *fleet.FlexibleSpec
		wantInstanceID   string
		wantNoInstance   bool
	}{
		{
			name: "spec filters by arch - ARM64",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-amd64"),
					InstanceType: ec2types.InstanceTypeC6iXlarge, // amd64
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
				{
					InstanceId:   aws.String("i-arm64"),
					InstanceType: ec2types.InstanceTypeC7gXlarge, // arm64
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec: &fleet.FlexibleSpec{
				Arch: "arm64",
			},
			wantInstanceID: "i-arm64",
		},
		{
			name: "spec filters by arch - AMD64",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-amd64"),
					InstanceType: ec2types.InstanceTypeC6iXlarge, // amd64
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
				{
					InstanceId:   aws.String("i-arm64"),
					InstanceType: ec2types.InstanceTypeC7gXlarge, // arm64
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec: &fleet.FlexibleSpec{
				Arch: "amd64",
			},
			wantInstanceID: "i-amd64",
		},
		{
			name: "spec filters by CPU - selects smallest",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-8cpu"),
					InstanceType: ec2types.InstanceTypeC7g2xlarge, // 8 CPU
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
				{
					InstanceId:   aws.String("i-4cpu"),
					InstanceType: ec2types.InstanceTypeC7gXlarge, // 4 CPU
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec: &fleet.FlexibleSpec{
				CPUMin: 4,
				CPUMax: 8,
				Arch:   "arm64",
			},
			wantInstanceID: "i-4cpu", // Best-fit: smallest matching
		},
		{
			name: "spec filters out all instances",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-small"),
					InstanceType: ec2types.InstanceTypeC7gLarge, // 2 CPU
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec: &fleet.FlexibleSpec{
				CPUMin: 8, // No instance with 8+ CPUs
			},
			wantNoInstance: true,
		},
		{
			name: "nil spec matches any instance",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-any"),
					InstanceType: ec2types.InstanceTypeC7gXlarge,
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec:           nil, // Legacy behavior: any instance
			wantInstanceID: "i-any",
		},
		{
			name: "best-fit tie-breaker - same CPU, select lower RAM",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-4cpu-16ram"),
					InstanceType: ec2types.InstanceTypeM7gXlarge, // 4 CPU, 16 RAM
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
				{
					InstanceId:   aws.String("i-4cpu-8ram"),
					InstanceType: ec2types.InstanceTypeC7gXlarge, // 4 CPU, 8 RAM
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec: &fleet.FlexibleSpec{
				CPUMin: 4,
				CPUMax: 4,
				Arch:   "arm64",
			},
			wantInstanceID: "i-4cpu-8ram", // Lower RAM wins tie-breaker
		},
		{
			name: "skips running instances even if they match spec",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-running-matches"),
					InstanceType: ec2types.InstanceTypeC7gXlarge,
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
				},
				{
					InstanceId:   aws.String("i-stopped-matches"),
					InstanceType: ec2types.InstanceTypeC7g2xlarge,
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec: &fleet.FlexibleSpec{
				CPUMin: 4,
				Arch:   "arm64",
			},
			wantInstanceID: "i-stopped-matches",
		},
		{
			name: "all instances running - no available instance",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-running1"),
					InstanceType: ec2types.InstanceTypeC7gXlarge,
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
				},
				{
					InstanceId:   aws.String("i-running2"),
					InstanceType: ec2types.InstanceTypeC7g2xlarge,
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
				},
			},
			spec: &fleet.FlexibleSpec{
				CPUMin: 4,
				Arch:   "arm64",
			},
			wantNoInstance: true,
		},
		{
			name: "filters by generation",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-gen7"),
					InstanceType: ec2types.InstanceTypeC7gXlarge, // Gen 7
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
				{
					InstanceId:   aws.String("i-gen8"),
					InstanceType: ec2types.InstanceTypeC8gXlarge, // Gen 8
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec: &fleet.FlexibleSpec{
				CPUMin: 4,
				Gen:    8,
				Arch:   "arm64",
			},
			wantInstanceID: "i-gen8",
		},
		{
			name: "filters by family - single family",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-c7g"),
					InstanceType: ec2types.InstanceTypeC7gXlarge, // c7g family
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
				{
					InstanceId:   aws.String("i-m7g"),
					InstanceType: ec2types.InstanceTypeM7gXlarge, // m7g family
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec: &fleet.FlexibleSpec{
				CPUMin:   4,
				Families: []string{"m7g"},
				Arch:     "arm64",
			},
			wantInstanceID: "i-m7g",
		},
		{
			name: "filters by family - multiple families",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-t4g"),
					InstanceType: ec2types.InstanceTypeT4gXlarge, // t4g family - not in filter
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
				{
					InstanceId:   aws.String("i-c7g"),
					InstanceType: ec2types.InstanceTypeC7gXlarge, // c7g family - in filter
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec: &fleet.FlexibleSpec{
				CPUMin:   4,
				Families: []string{"c7g", "m7g"},
				Arch:     "arm64",
			},
			wantInstanceID: "i-c7g",
		},
		{
			name: "RAM filter - minimum RAM",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-8ram"),
					InstanceType: ec2types.InstanceTypeC7gXlarge, // 4 CPU, 8 RAM
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
				{
					InstanceId:   aws.String("i-16ram"),
					InstanceType: ec2types.InstanceTypeM7gXlarge, // 4 CPU, 16 RAM
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec: &fleet.FlexibleSpec{
				CPUMin: 4,
				RAMMin: 12, // Needs at least 12GB
				Arch:   "arm64",
			},
			wantInstanceID: "i-16ram",
		},
		{
			name: "mixed arch pool - selects correct arch",
			instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-amd64-1"),
					InstanceType: ec2types.InstanceTypeC6iXlarge, // amd64, 4 CPU
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
				{
					InstanceId:   aws.String("i-arm64-1"),
					InstanceType: ec2types.InstanceTypeC7gXlarge, // arm64, 4 CPU
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
				{
					InstanceId:   aws.String("i-amd64-2"),
					InstanceType: ec2types.InstanceTypeC6i2xlarge, // amd64, 8 CPU
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			},
			spec: &fleet.FlexibleSpec{
				CPUMin: 4,
				CPUMax: 8,
				Arch:   "amd64",
			},
			wantInstanceID: "i-amd64-1", // Smallest amd64 instance
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &MockDBClient{
				ClaimInstanceForJobFunc: func(_ context.Context, _ string, _ int64, _ time.Duration) error {
					return nil
				},
			}

			mockEC2 := &MockEC2API{
				DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
					return &ec2.DescribeInstancesOutput{
						Reservations: []ec2types.Reservation{{Instances: tt.instances}},
					}, nil
				},
				StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
					return &ec2.StartInstancesOutput{}, nil
				},
			}

			manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
			manager.SetEC2Client(mockEC2)

			instance, err := manager.ClaimAndStartPoolInstance(context.Background(), "test-pool", 12345, "owner/repo", tt.spec)

			if tt.wantNoInstance {
				if !errors.Is(err, ErrNoAvailableInstance) {
					t.Errorf("expected ErrNoAvailableInstance, got %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if instance == nil {
				t.Fatal("expected instance, got nil")
			}
			if instance.InstanceID != tt.wantInstanceID {
				t.Errorf("InstanceID = %s, want %s", instance.InstanceID, tt.wantInstanceID)
			}
		})
	}
}

// TestClaimAndStartPoolInstance_WithSpec_ClaimConflict tests that when first matching
// instance is already claimed, it tries the next matching instance.
func TestClaimAndStartPoolInstance_WithSpec_ClaimConflict(t *testing.T) {
	claimAttempts := 0
	mockDB := &MockDBClient{
		ClaimInstanceForJobFunc: func(_ context.Context, instanceID string, _ int64, _ time.Duration) error {
			claimAttempts++
			// First ARM64 instance (smaller) is already claimed
			if instanceID == "i-arm64-small" {
				return db.ErrInstanceAlreadyClaimed
			}
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
								InstanceId:   aws.String("i-arm64-small"),
								InstanceType: ec2types.InstanceTypeC7gXlarge, // 4 CPU - best fit but claimed
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
							{
								InstanceId:   aws.String("i-arm64-large"),
								InstanceType: ec2types.InstanceTypeC7g2xlarge, // 8 CPU - second best
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
							{
								InstanceId:   aws.String("i-amd64"),
								InstanceType: ec2types.InstanceTypeC6iXlarge, // wrong arch
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			if len(params.InstanceIds) != 1 || params.InstanceIds[0] != "i-arm64-large" {
				t.Errorf("expected i-arm64-large to be started, got %v", params.InstanceIds)
			}
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	spec := &fleet.FlexibleSpec{
		CPUMin: 4,
		Arch:   "arm64",
	}

	instance, err := manager.ClaimAndStartPoolInstance(context.Background(), "test-pool", 12345, "owner/repo", spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if instance == nil {
		t.Fatal("expected instance, got nil")
	}
	if instance.InstanceID != "i-arm64-large" {
		t.Errorf("expected i-arm64-large, got %s", instance.InstanceID)
	}
	if claimAttempts != 2 {
		t.Errorf("expected 2 claim attempts, got %d", claimAttempts)
	}
}

// TestClaimAndStartPoolInstance_UnknownInstanceType tests that instances with types
// not in InstanceCatalog are excluded from spec matching.
func TestClaimAndStartPoolInstance_UnknownInstanceType(t *testing.T) {
	mockDB := &MockDBClient{
		ClaimInstanceForJobFunc: func(_ context.Context, _ string, _ int64, _ time.Duration) error {
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
								InstanceId:   aws.String("i-unknown"),
								InstanceType: "unknown.type", // Not in InstanceCatalog
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
							{
								InstanceId:   aws.String("i-known"),
								InstanceType: ec2types.InstanceTypeC7gXlarge, // In catalog
								State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
							},
						},
					},
				},
			}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			if len(params.InstanceIds) != 1 || params.InstanceIds[0] != "i-known" {
				t.Errorf("expected i-known to be started (unknown type excluded), got %v", params.InstanceIds)
			}
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	spec := &fleet.FlexibleSpec{
		CPUMin: 4,
		Arch:   "arm64",
	}

	instance, err := manager.ClaimAndStartPoolInstance(context.Background(), "test-pool", 12345, "owner/repo", spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if instance == nil {
		t.Fatal("expected instance, got nil")
	}
	if instance.InstanceID != "i-known" {
		t.Errorf("expected i-known, got %s", instance.InstanceID)
	}
}
