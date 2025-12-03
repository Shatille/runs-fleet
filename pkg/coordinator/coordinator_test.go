package coordinator

import (
	"context"
	"testing"
	"time"
)

type testLogger struct {
	t *testing.T
}

func (l *testLogger) Printf(format string, v ...interface{}) {
	l.t.Logf(format, v...)
}

func (l *testLogger) Println(v ...interface{}) {
	l.t.Log(v...)
}

func TestNoOpCoordinator(t *testing.T) {
	logger := &testLogger{t: t}
	coord := NewNoOpCoordinator(logger)

	ctx := context.Background()

	if err := coord.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if !coord.IsLeader() {
		t.Error("NoOpCoordinator.IsLeader() should always return true")
	}

	if err := coord.Stop(ctx); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}
}

func TestDefaultConfig(t *testing.T) {
	instanceID := "test-instance"
	cfg := DefaultConfig(instanceID)

	if cfg.InstanceID != instanceID {
		t.Errorf("InstanceID = %s, want %s", cfg.InstanceID, instanceID)
	}

	if cfg.LeaseDuration != 60*time.Second {
		t.Errorf("LeaseDuration = %v, want 60s", cfg.LeaseDuration)
	}

	if cfg.HeartbeatInterval != 20*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 20s", cfg.HeartbeatInterval)
	}

	if cfg.RetryInterval != 30*time.Second {
		t.Errorf("RetryInterval = %v, want 30s", cfg.RetryInterval)
	}
}

func TestDefaultConfig_LockTableName(t *testing.T) {
	cfg := DefaultConfig("test-instance")

	if cfg.LockTableName != "runs-fleet-locks" {
		t.Errorf("LockTableName = %s, want runs-fleet-locks", cfg.LockTableName)
	}

	if cfg.LockName != "runs-fleet-leader" {
		t.Errorf("LockName = %s, want runs-fleet-leader", cfg.LockName)
	}
}

func TestDefaultConfig_DifferentInstanceIDs(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
	}{
		{"simple id", "instance-1"},
		{"uuid-like", "i-0abc123def456789"},
		{"pod name", "runs-fleet-pod-xyz-12345"},
		{"empty", ""},
		{"special chars", "instance-test_123.abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig(tt.instanceID)
			if cfg.InstanceID != tt.instanceID {
				t.Errorf("InstanceID = %s, want %s", cfg.InstanceID, tt.instanceID)
			}
		})
	}
}

func TestConfig_AllFields(t *testing.T) {
	cfg := Config{
		InstanceID:        "my-instance",
		LockTableName:     "my-locks-table",
		LockName:          "my-lock",
		LeaseDuration:     30 * time.Second,
		HeartbeatInterval: 10 * time.Second,
		RetryInterval:     15 * time.Second,
	}

	if cfg.InstanceID != "my-instance" {
		t.Errorf("InstanceID = %s, want my-instance", cfg.InstanceID)
	}
	if cfg.LockTableName != "my-locks-table" {
		t.Errorf("LockTableName = %s, want my-locks-table", cfg.LockTableName)
	}
	if cfg.LockName != "my-lock" {
		t.Errorf("LockName = %s, want my-lock", cfg.LockName)
	}
	if cfg.LeaseDuration != 30*time.Second {
		t.Errorf("LeaseDuration = %v, want 30s", cfg.LeaseDuration)
	}
	if cfg.HeartbeatInterval != 10*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 10s", cfg.HeartbeatInterval)
	}
	if cfg.RetryInterval != 15*time.Second {
		t.Errorf("RetryInterval = %v, want 15s", cfg.RetryInterval)
	}
}

func TestNoOpCoordinator_AlwaysLeader(t *testing.T) {
	logger := &testLogger{t: t}
	coord := NewNoOpCoordinator(logger)

	// Check multiple times - should always return true
	for i := 0; i < 10; i++ {
		if !coord.IsLeader() {
			t.Errorf("IsLeader() iteration %d should return true", i)
		}
	}
}

func TestNoOpCoordinator_StartStop_Multiple(t *testing.T) {
	logger := &testLogger{t: t}
	coord := NewNoOpCoordinator(logger)

	ctx := context.Background()

	// Test multiple start/stop cycles
	for i := 0; i < 3; i++ {
		if err := coord.Start(ctx); err != nil {
			t.Fatalf("Start() cycle %d failed: %v", i, err)
		}

		if err := coord.Stop(ctx); err != nil {
			t.Fatalf("Stop() cycle %d failed: %v", i, err)
		}
	}
}

func TestNoOpCoordinator_StopWithCancelledContext(t *testing.T) {
	logger := &testLogger{t: t}
	coord := NewNoOpCoordinator(logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Stop should still succeed with cancelled context (no-op)
	err := coord.Stop(ctx)
	if err != nil {
		t.Errorf("Stop() with cancelled context should not error, got: %v", err)
	}
}

func TestNoOpCoordinator_StartWithCancelledContext(t *testing.T) {
	logger := &testLogger{t: t}
	coord := NewNoOpCoordinator(logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Start should still succeed with cancelled context (no-op)
	err := coord.Start(ctx)
	if err != nil {
		t.Errorf("Start() with cancelled context should not error, got: %v", err)
	}
}

func TestNoOpCoordinator_ConcurrentIsLeader(t *testing.T) {
	logger := &testLogger{t: t}
	coord := NewNoOpCoordinator(logger)

	done := make(chan bool)

	// Test concurrent calls to IsLeader
	for i := 0; i < 10; i++ {
		go func() {
			if !coord.IsLeader() {
				t.Error("IsLeader() should always return true")
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestNoOpCoordinator_Interface(t *testing.T) {
	logger := &testLogger{t: t}

	// Verify NoOpCoordinator implements Coordinator interface
	var _ Coordinator = NewNoOpCoordinator(logger)
}

func TestCoordinatorInterface_Methods(t *testing.T) {
	// Test that the Coordinator interface has the expected methods
	logger := &testLogger{t: t}
	var coord Coordinator = NewNoOpCoordinator(logger)

	ctx := context.Background()

	// All methods should be callable and return without error
	if err := coord.Start(ctx); err != nil {
		t.Errorf("Start() error = %v", err)
	}

	if !coord.IsLeader() {
		t.Error("IsLeader() should return true for NoOpCoordinator")
	}

	if err := coord.Stop(ctx); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

func TestConfig_ZeroValues(t *testing.T) {
	cfg := Config{}

	if cfg.InstanceID != "" {
		t.Errorf("InstanceID should be empty, got %s", cfg.InstanceID)
	}
	if cfg.LockTableName != "" {
		t.Errorf("LockTableName should be empty, got %s", cfg.LockTableName)
	}
	if cfg.LockName != "" {
		t.Errorf("LockName should be empty, got %s", cfg.LockName)
	}
	if cfg.LeaseDuration != 0 {
		t.Errorf("LeaseDuration should be 0, got %v", cfg.LeaseDuration)
	}
	if cfg.HeartbeatInterval != 0 {
		t.Errorf("HeartbeatInterval should be 0, got %v", cfg.HeartbeatInterval)
	}
	if cfg.RetryInterval != 0 {
		t.Errorf("RetryInterval should be 0, got %v", cfg.RetryInterval)
	}
}

func TestDefaultConfig_DurationRelationships(t *testing.T) {
	cfg := DefaultConfig("test")

	// HeartbeatInterval should be less than LeaseDuration
	if cfg.HeartbeatInterval >= cfg.LeaseDuration {
		t.Errorf("HeartbeatInterval (%v) should be less than LeaseDuration (%v)",
			cfg.HeartbeatInterval, cfg.LeaseDuration)
	}

	// RetryInterval should be less than LeaseDuration
	if cfg.RetryInterval >= cfg.LeaseDuration {
		t.Errorf("RetryInterval (%v) should be less than LeaseDuration (%v)",
			cfg.RetryInterval, cfg.LeaseDuration)
	}
}

func TestNoOpCoordinator_WithTimeout(t *testing.T) {
	logger := &testLogger{t: t}
	coord := NewNoOpCoordinator(logger)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := coord.Start(ctx); err != nil {
		t.Errorf("Start() with timeout error = %v", err)
	}

	// Sleep past the timeout
	time.Sleep(150 * time.Millisecond)

	// Operations should still work on NoOp coordinator (no-op)
	if !coord.IsLeader() {
		t.Error("IsLeader() should still return true after context timeout")
	}

	// Stop with a fresh context since the old one is expired
	if err := coord.Stop(context.Background()); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}
