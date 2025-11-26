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
