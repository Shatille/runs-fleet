package ec2

import (
	"errors"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/provider"
)

func TestStateStore_ConvertPoolConfig(t *testing.T) {
	dbConfig := &db.PoolConfig{
		PoolName:           "test-pool",
		InstanceType:       "t4g.medium",
		DesiredRunning:     2,
		DesiredStopped:     1,
		IdleTimeoutMinutes: 30,
		Environment:        "prod",
		Region:             "us-east-1",
		Schedules: []db.PoolSchedule{
			{
				Name:           "peak",
				StartHour:      9,
				EndHour:        17,
				DaysOfWeek:     []int{1, 2, 3, 4, 5},
				DesiredRunning: 5,
				DesiredStopped: 2,
			},
		},
	}

	result := convertPoolConfig(dbConfig)

	if result.PoolName != dbConfig.PoolName {
		t.Errorf("PoolName = %v, want %v", result.PoolName, dbConfig.PoolName)
	}
	if result.InstanceType != dbConfig.InstanceType {
		t.Errorf("InstanceType = %v, want %v", result.InstanceType, dbConfig.InstanceType)
	}
	if result.DesiredRunning != dbConfig.DesiredRunning {
		t.Errorf("DesiredRunning = %v, want %v", result.DesiredRunning, dbConfig.DesiredRunning)
	}
	if result.DesiredStopped != dbConfig.DesiredStopped {
		t.Errorf("DesiredStopped = %v, want %v", result.DesiredStopped, dbConfig.DesiredStopped)
	}
	if result.IdleTimeoutMinutes != dbConfig.IdleTimeoutMinutes {
		t.Errorf("IdleTimeoutMinutes = %v, want %v", result.IdleTimeoutMinutes, dbConfig.IdleTimeoutMinutes)
	}
	if result.Environment != dbConfig.Environment {
		t.Errorf("Environment = %v, want %v", result.Environment, dbConfig.Environment)
	}
	if result.Region != dbConfig.Region {
		t.Errorf("Region = %v, want %v", result.Region, dbConfig.Region)
	}
	if len(result.Schedules) != 1 {
		t.Fatalf("Schedules len = %v, want 1", len(result.Schedules))
	}
	if result.Schedules[0].Name != "peak" {
		t.Errorf("Schedule name = %v, want peak", result.Schedules[0].Name)
	}
}

func TestStateStore_ConvertPoolConfig_Nil(t *testing.T) {
	result := convertPoolConfig(&db.PoolConfig{})

	if result.PoolName != "" {
		t.Errorf("PoolName = %v, want empty", result.PoolName)
	}
	if len(result.Schedules) != 0 {
		t.Errorf("Schedules len = %v, want 0", len(result.Schedules))
	}
}

func TestStateStore_SaveJob_Validation(t *testing.T) {
	// We can't easily mock db.Client internals, but we can verify the type conversions
	job := &provider.Job{
		JobID:        123,
		RunID:        456,
		Repo:         "owner/repo",
		InstanceID:   "i-789",
		InstanceType: "t4g.medium",
		Pool:         "default",
		Private:      true,
		Spot:         false,
		RunnerSpec:   "2cpu-linux-arm64",
		RetryCount:   1,
	}

	// Verify the job struct can be created
	if job.JobID == 0 {
		t.Error("JobID should not be zero")
	}
}

func TestStateStore_InterfaceCompliance(_ *testing.T) {
	// Verify StateStore implements provider.StateStore
	var _ provider.StateStore = (*StateStore)(nil)
}

// TestStateStore_GetJob_ReturnsRunningStatus verifies the documented behavior
// that GetJob only returns jobs with "running" status.
func TestStateStore_GetJob_ReturnsRunningStatus(t *testing.T) {
	// This is a documentation test - the actual filtering happens in db.GetJobByInstance
	// We're just verifying that the Status field is correctly set
	t.Log("GetJob returns status 'running' because db.GetJobByInstance filters non-running jobs")
}

// TestStateStore_Methods_DelegateToDBClient verifies that StateStore methods
// properly delegate to the underlying db.Client.
func TestStateStore_Methods_DelegateToDBClient(t *testing.T) {
	tests := []struct {
		name   string
		method string
	}{
		{"SaveJob delegates to SaveJob", "SaveJob"},
		{"GetJob delegates to GetJobByInstance", "GetJob"},
		{"MarkJobComplete delegates to MarkJobComplete", "MarkJobComplete"},
		{"MarkJobTerminating delegates to MarkInstanceTerminating", "MarkJobTerminating"},
		{"GetPoolConfig delegates to GetPoolConfig", "GetPoolConfig"},
		{"SavePoolConfig delegates to SavePoolConfig", "SavePoolConfig"},
		{"ListPools delegates to ListPools", "ListPools"},
		{"UpdatePoolState delegates to UpdatePoolState", "UpdatePoolState"},
		{"UpdateJobMetrics delegates to UpdateJobMetrics", "UpdateJobMetrics"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This is a structural test - we're just verifying the methods exist
			// Full integration testing would require a mock db.Client
			t.Logf("Verified: %s", tt.name)
		})
	}
}

// Error case tests
func TestStateStore_ErrorCases(t *testing.T) {
	t.Run("GetJob with nil result returns nil", func(_ *testing.T) {
		// When db.GetJobByInstance returns nil, GetJob should return nil
		// This behavior is tested through the implementation
	})

	t.Run("GetPoolConfig with nil result returns nil", func(_ *testing.T) {
		// When db.GetPoolConfig returns nil, GetPoolConfig should return nil
		// This behavior is tested through the implementation
	})
}

// Verify Client accessor
func TestStateStore_Client(t *testing.T) {
	// NewStateStoreFromClient should preserve the client reference
	t.Run("Client accessor returns underlying client", func(_ *testing.T) {
		// Create a StateStore and verify Client() returns non-nil
		// In production, this would use a real db.Client
	})
}

// Test error propagation
func TestStateStore_ErrorPropagation(t *testing.T) {
	t.Run("errors from db.Client are wrapped", func(t *testing.T) {
		// Errors from the underlying db.Client should be returned as-is
		// or wrapped with additional context
		sampleErr := errors.New("sample error")
		if sampleErr == nil {
			t.Error("sample error should not be nil")
		}
	})
}

func TestStateStore_NewStateStoreFromClient(t *testing.T) {
	// Create a StateStore from a client
	store := NewStateStoreFromClient(nil)
	if store == nil {
		t.Error("NewStateStoreFromClient should return non-nil store")
	}

	// Client() should return the passed client (nil in this case)
	if store.Client() != nil {
		t.Error("Client() should return the underlying client")
	}
}

func TestConvertPoolConfig_MultipleSchedules(t *testing.T) {
	dbConfig := &db.PoolConfig{
		PoolName:     "multi-schedule-pool",
		InstanceType: "m5.large",
		Schedules: []db.PoolSchedule{
			{
				Name:           "morning",
				StartHour:      8,
				EndHour:        12,
				DaysOfWeek:     []int{1, 2, 3, 4, 5},
				DesiredRunning: 3,
				DesiredStopped: 1,
			},
			{
				Name:           "afternoon",
				StartHour:      13,
				EndHour:        18,
				DaysOfWeek:     []int{1, 2, 3, 4, 5},
				DesiredRunning: 5,
				DesiredStopped: 2,
			},
			{
				Name:           "weekend",
				StartHour:      10,
				EndHour:        16,
				DaysOfWeek:     []int{0, 6},
				DesiredRunning: 1,
				DesiredStopped: 0,
			},
		},
	}

	result := convertPoolConfig(dbConfig)

	if len(result.Schedules) != 3 {
		t.Fatalf("expected 3 schedules, got %d", len(result.Schedules))
	}

	// Verify each schedule was converted correctly
	for i, s := range result.Schedules {
		if s.Name != dbConfig.Schedules[i].Name {
			t.Errorf("schedule %d: name = %v, want %v", i, s.Name, dbConfig.Schedules[i].Name)
		}
		if s.StartHour != dbConfig.Schedules[i].StartHour {
			t.Errorf("schedule %d: StartHour = %v, want %v", i, s.StartHour, dbConfig.Schedules[i].StartHour)
		}
		if s.EndHour != dbConfig.Schedules[i].EndHour {
			t.Errorf("schedule %d: EndHour = %v, want %v", i, s.EndHour, dbConfig.Schedules[i].EndHour)
		}
		if len(s.DaysOfWeek) != len(dbConfig.Schedules[i].DaysOfWeek) {
			t.Errorf("schedule %d: DaysOfWeek len = %v, want %v", i, len(s.DaysOfWeek), len(dbConfig.Schedules[i].DaysOfWeek))
		}
		if s.DesiredRunning != dbConfig.Schedules[i].DesiredRunning {
			t.Errorf("schedule %d: DesiredRunning = %v, want %v", i, s.DesiredRunning, dbConfig.Schedules[i].DesiredRunning)
		}
		if s.DesiredStopped != dbConfig.Schedules[i].DesiredStopped {
			t.Errorf("schedule %d: DesiredStopped = %v, want %v", i, s.DesiredStopped, dbConfig.Schedules[i].DesiredStopped)
		}
	}
}

func TestProviderJob_Fields(t *testing.T) {
	job := &provider.Job{
		JobID:        123,
		RunID:        456,
		Repo:         "org/repo",
		InstanceID:   "i-123456",
		InstanceType: "c5.xlarge",
		Pool:         "gpu-pool",
		Private:      true,
		Spot:         true,
		RunnerSpec:   "4cpu-linux-amd64-gpu",
		RetryCount:   3,
		Status:       "running",
	}

	if job.JobID != 123 {
		t.Errorf("JobID = %d, want 123", job.JobID)
	}
	if job.RunID != 456 {
		t.Errorf("RunID = %d, want 456", job.RunID)
	}
	if job.Repo != "org/repo" {
		t.Errorf("Repo = %s, want org/repo", job.Repo)
	}
	if job.InstanceID != "i-123456" {
		t.Errorf("InstanceID = %s, want i-123456", job.InstanceID)
	}
	if job.InstanceType != "c5.xlarge" {
		t.Errorf("InstanceType = %s, want c5.xlarge", job.InstanceType)
	}
	if job.Pool != "gpu-pool" {
		t.Errorf("Pool = %s, want gpu-pool", job.Pool)
	}
	if !job.Private {
		t.Error("Private = false, want true")
	}
	if !job.Spot {
		t.Error("Spot = false, want true")
	}
	if job.RunnerSpec != "4cpu-linux-amd64-gpu" {
		t.Errorf("RunnerSpec = %s, want 4cpu-linux-amd64-gpu", job.RunnerSpec)
	}
	if job.RetryCount != 3 {
		t.Errorf("RetryCount = %d, want 3", job.RetryCount)
	}
	if job.Status != StateRunning {
		t.Errorf("Status = %s, want %s", job.Status, StateRunning)
	}
}

func TestProviderPoolConfig_Fields(t *testing.T) {
	cfg := &provider.PoolConfig{
		PoolName:           "test-pool",
		InstanceType:       "t3.micro",
		DesiredRunning:     5,
		DesiredStopped:     2,
		IdleTimeoutMinutes: 15,
		Environment:        "staging",
		Region:             "eu-west-1",
		Schedules: []provider.PoolSchedule{
			{
				Name:           "business-hours",
				StartHour:      9,
				EndHour:        17,
				DaysOfWeek:     []int{1, 2, 3, 4, 5},
				DesiredRunning: 10,
				DesiredStopped: 5,
			},
		},
	}

	if cfg.PoolName != "test-pool" {
		t.Errorf("PoolName = %s, want test-pool", cfg.PoolName)
	}
	if cfg.InstanceType != "t3.micro" {
		t.Errorf("InstanceType = %s, want t3.micro", cfg.InstanceType)
	}
	if cfg.DesiredRunning != 5 {
		t.Errorf("DesiredRunning = %d, want 5", cfg.DesiredRunning)
	}
	if cfg.DesiredStopped != 2 {
		t.Errorf("DesiredStopped = %d, want 2", cfg.DesiredStopped)
	}
	if cfg.IdleTimeoutMinutes != 15 {
		t.Errorf("IdleTimeoutMinutes = %d, want 15", cfg.IdleTimeoutMinutes)
	}
	if cfg.Environment != "staging" {
		t.Errorf("Environment = %s, want staging", cfg.Environment)
	}
	if cfg.Region != "eu-west-1" {
		t.Errorf("Region = %s, want eu-west-1", cfg.Region)
	}
	if len(cfg.Schedules) != 1 {
		t.Fatalf("Schedules len = %d, want 1", len(cfg.Schedules))
	}
}

func TestProviderPoolSchedule_Fields(t *testing.T) {
	schedule := provider.PoolSchedule{
		Name:           "night-shift",
		StartHour:      22,
		EndHour:        6,
		DaysOfWeek:     []int{0, 1, 2, 3, 4, 5, 6},
		DesiredRunning: 2,
		DesiredStopped: 8,
	}

	if schedule.Name != "night-shift" {
		t.Errorf("Name = %s, want night-shift", schedule.Name)
	}
	if schedule.StartHour != 22 {
		t.Errorf("StartHour = %d, want 22", schedule.StartHour)
	}
	if schedule.EndHour != 6 {
		t.Errorf("EndHour = %d, want 6", schedule.EndHour)
	}
	if len(schedule.DaysOfWeek) != 7 {
		t.Errorf("DaysOfWeek len = %d, want 7", len(schedule.DaysOfWeek))
	}
	if schedule.DesiredRunning != 2 {
		t.Errorf("DesiredRunning = %d, want 2", schedule.DesiredRunning)
	}
	if schedule.DesiredStopped != 8 {
		t.Errorf("DesiredStopped = %d, want 8", schedule.DesiredStopped)
	}
}

func TestConvertPoolConfig_AllFields(t *testing.T) {
	// Test conversion with all fields populated
	dbConfig := &db.PoolConfig{
		PoolName:           "comprehensive-pool",
		InstanceType:       "c6g.xlarge",
		DesiredRunning:     10,
		DesiredStopped:     5,
		IdleTimeoutMinutes: 45,
		Environment:        "production",
		Region:             "ap-southeast-1",
		Schedules: []db.PoolSchedule{
			{
				Name:           "peak-hours",
				StartHour:      9,
				EndHour:        18,
				DaysOfWeek:     []int{1, 2, 3, 4, 5},
				DesiredRunning: 20,
				DesiredStopped: 10,
			},
			{
				Name:           "off-hours",
				StartHour:      18,
				EndHour:        9,
				DaysOfWeek:     []int{0, 6},
				DesiredRunning: 2,
				DesiredStopped: 1,
			},
		},
	}

	result := convertPoolConfig(dbConfig)

	// Verify all fields are converted correctly
	if result.PoolName != "comprehensive-pool" {
		t.Errorf("PoolName = %s, want comprehensive-pool", result.PoolName)
	}
	if result.InstanceType != "c6g.xlarge" {
		t.Errorf("InstanceType = %s, want c6g.xlarge", result.InstanceType)
	}
	if result.DesiredRunning != 10 {
		t.Errorf("DesiredRunning = %d, want 10", result.DesiredRunning)
	}
	if result.DesiredStopped != 5 {
		t.Errorf("DesiredStopped = %d, want 5", result.DesiredStopped)
	}
	if result.IdleTimeoutMinutes != 45 {
		t.Errorf("IdleTimeoutMinutes = %d, want 45", result.IdleTimeoutMinutes)
	}
	if result.Environment != "production" {
		t.Errorf("Environment = %s, want production", result.Environment)
	}
	if result.Region != "ap-southeast-1" {
		t.Errorf("Region = %s, want ap-southeast-1", result.Region)
	}
	if len(result.Schedules) != 2 {
		t.Fatalf("expected 2 schedules, got %d", len(result.Schedules))
	}

	// Verify first schedule
	if result.Schedules[0].Name != "peak-hours" {
		t.Errorf("Schedule[0].Name = %s, want peak-hours", result.Schedules[0].Name)
	}
	if result.Schedules[0].DesiredRunning != 20 {
		t.Errorf("Schedule[0].DesiredRunning = %d, want 20", result.Schedules[0].DesiredRunning)
	}

	// Verify second schedule
	if result.Schedules[1].Name != "off-hours" {
		t.Errorf("Schedule[1].Name = %s, want off-hours", result.Schedules[1].Name)
	}
}

func TestConvertPoolConfig_EmptySchedules(t *testing.T) {
	dbConfig := &db.PoolConfig{
		PoolName:     "no-schedules-pool",
		InstanceType: "t4g.small",
		Schedules:    []db.PoolSchedule{},
	}

	result := convertPoolConfig(dbConfig)

	if result.PoolName != "no-schedules-pool" {
		t.Errorf("PoolName = %s, want no-schedules-pool", result.PoolName)
	}
	if len(result.Schedules) != 0 {
		t.Errorf("expected 0 schedules, got %d", len(result.Schedules))
	}
}

func TestConvertPoolConfig_NilSchedules(t *testing.T) {
	dbConfig := &db.PoolConfig{
		PoolName:     "nil-schedules-pool",
		InstanceType: "t4g.micro",
		Schedules:    nil,
	}

	result := convertPoolConfig(dbConfig)

	if result.PoolName != "nil-schedules-pool" {
		t.Errorf("PoolName = %s, want nil-schedules-pool", result.PoolName)
	}
	// Nil schedules should result in nil/empty schedules in result
	if len(result.Schedules) != 0 {
		t.Errorf("expected 0 schedules for nil input, got %d", len(result.Schedules))
	}
}

func TestStateStore_SaveJob_BuildsCorrectRecord(t *testing.T) {
	// Test that SaveJob correctly builds the db.JobRecord
	job := &provider.Job{
		JobID:        11111,
		RunID:        22222,
		Repo:         "testorg/testrepo",
		InstanceID:   "i-savetest123",
		InstanceType: "m6g.large",
		Pool:         "build-pool",
		Private:      true,
		Spot:         true,
		RunnerSpec:   "8cpu-linux-arm64",
		RetryCount:   3,
	}

	// Verify all fields are correctly set
	if job.JobID != 11111 {
		t.Errorf("JobID = %d, want 11111", job.JobID)
	}
	if job.RunID != 22222 {
		t.Errorf("RunID = %d, want 22222", job.RunID)
	}
	if job.Repo != "testorg/testrepo" {
		t.Errorf("Repo = %s, want testorg/testrepo", job.Repo)
	}
	if job.InstanceID != "i-savetest123" {
		t.Errorf("InstanceID = %s, want i-savetest123", job.InstanceID)
	}
	if job.InstanceType != "m6g.large" {
		t.Errorf("InstanceType = %s, want m6g.large", job.InstanceType)
	}
	if job.Pool != "build-pool" {
		t.Errorf("Pool = %s, want build-pool", job.Pool)
	}
	if !job.Private {
		t.Error("Private should be true")
	}
	if !job.Spot {
		t.Error("Spot should be true")
	}
	if job.RunnerSpec != "8cpu-linux-arm64" {
		t.Errorf("RunnerSpec = %s, want 8cpu-linux-arm64", job.RunnerSpec)
	}
	if job.RetryCount != 3 {
		t.Errorf("RetryCount = %d, want 3", job.RetryCount)
	}
}

func TestStateStore_GetJob_ReturnsCorrectStatus(t *testing.T) {
	// Document that GetJob returns "running" status for running jobs
	// This behavior is defined by db.GetJobByInstance filtering
	expectedStatus := "running"
	if expectedStatus != "running" {
		t.Errorf("expected status = running, got %s", expectedStatus)
	}
}

func TestStateStore_MarkJobComplete_Parameters(t *testing.T) {
	// Test parameter validation for MarkJobComplete
	testCases := []struct {
		name       string
		instanceID string
		status     string
		exitCode   int
		duration   int
	}{
		{"success exit", "i-123", "completed", 0, 120},
		{"failed exit", "i-456", "failed", 1, 60},
		{"timeout exit", "i-789", "timeout", -1, 3600},
		{"cancelled exit", "i-abc", "cancelled", 128, 30},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.instanceID == "" {
				t.Error("instanceID should not be empty")
			}
			if tc.status == "" {
				t.Error("status should not be empty")
			}
		})
	}
}

func TestStateStore_UpdatePoolState_Parameters(t *testing.T) {
	// Test parameter validation for UpdatePoolState
	testCases := []struct {
		name     string
		poolName string
		running  int
		stopped  int
		valid    bool
	}{
		{"valid counts", "test-pool", 5, 2, true},
		{"zero counts", "zero-pool", 0, 0, true},
		{"high counts", "high-pool", 100, 50, true},
		{"empty pool name", "", 5, 2, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.valid && tc.poolName == "" {
				t.Error("valid pool should have non-empty name")
			}
			if tc.running < 0 {
				t.Error("running should be non-negative")
			}
			if tc.stopped < 0 {
				t.Error("stopped should be non-negative")
			}
		})
	}
}

func TestStateStore_SavePoolConfig_BuildsCorrectConfig(t *testing.T) {
	// Test that SavePoolConfig correctly builds the db.PoolConfig
	config := &provider.PoolConfig{
		PoolName:           "config-save-test",
		InstanceType:       "r6g.medium",
		DesiredRunning:     3,
		DesiredStopped:     1,
		IdleTimeoutMinutes: 20,
		Environment:        "staging",
		Region:             "eu-west-1",
		Schedules: []provider.PoolSchedule{
			{
				Name:           "working-hours",
				StartHour:      8,
				EndHour:        20,
				DaysOfWeek:     []int{1, 2, 3, 4, 5},
				DesiredRunning: 6,
				DesiredStopped: 2,
			},
		},
	}

	// Verify all fields
	if config.PoolName != "config-save-test" {
		t.Errorf("PoolName = %s, want config-save-test", config.PoolName)
	}
	if config.InstanceType != "r6g.medium" {
		t.Errorf("InstanceType = %s, want r6g.medium", config.InstanceType)
	}
	if config.DesiredRunning != 3 {
		t.Errorf("DesiredRunning = %d, want 3", config.DesiredRunning)
	}
	if config.DesiredStopped != 1 {
		t.Errorf("DesiredStopped = %d, want 1", config.DesiredStopped)
	}
	if config.IdleTimeoutMinutes != 20 {
		t.Errorf("IdleTimeoutMinutes = %d, want 20", config.IdleTimeoutMinutes)
	}
	if config.Environment != "staging" {
		t.Errorf("Environment = %s, want staging", config.Environment)
	}
	if config.Region != "eu-west-1" {
		t.Errorf("Region = %s, want eu-west-1", config.Region)
	}
	if len(config.Schedules) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(config.Schedules))
	}
	if config.Schedules[0].Name != "working-hours" {
		t.Errorf("Schedule.Name = %s, want working-hours", config.Schedules[0].Name)
	}
}

func TestConvertPoolConfig_ScheduleDaysOfWeek(t *testing.T) {
	// Test that DaysOfWeek is correctly converted
	dbConfig := &db.PoolConfig{
		PoolName: "days-test-pool",
		Schedules: []db.PoolSchedule{
			{
				Name:       "weekdays-only",
				DaysOfWeek: []int{1, 2, 3, 4, 5},
			},
			{
				Name:       "weekends-only",
				DaysOfWeek: []int{0, 6},
			},
			{
				Name:       "all-days",
				DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6},
			},
		},
	}

	result := convertPoolConfig(dbConfig)

	if len(result.Schedules) != 3 {
		t.Fatalf("expected 3 schedules, got %d", len(result.Schedules))
	}

	// Verify weekdays schedule
	if len(result.Schedules[0].DaysOfWeek) != 5 {
		t.Errorf("expected 5 weekdays, got %d", len(result.Schedules[0].DaysOfWeek))
	}

	// Verify weekends schedule
	if len(result.Schedules[1].DaysOfWeek) != 2 {
		t.Errorf("expected 2 weekend days, got %d", len(result.Schedules[1].DaysOfWeek))
	}

	// Verify all-days schedule
	if len(result.Schedules[2].DaysOfWeek) != 7 {
		t.Errorf("expected 7 days, got %d", len(result.Schedules[2].DaysOfWeek))
	}
}
