package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNewSafetyMonitor(t *testing.T) {
	logger := &mockLogger{}
	maxRuntime := 1 * time.Hour

	monitor := NewSafetyMonitor(maxRuntime, logger)

	if monitor.maxRuntime != maxRuntime {
		t.Errorf("expected maxRuntime %v, got %v", maxRuntime, monitor.maxRuntime)
	}
	if monitor.checkInterval != DefaultCheckInterval {
		t.Errorf("expected checkInterval %v, got %v", DefaultCheckInterval, monitor.checkInterval)
	}
	if monitor.logger != logger {
		t.Error("expected logger to be set")
	}
	if monitor.startTime.IsZero() {
		t.Error("expected startTime to be set")
	}
}

func TestSafetyMonitor_SetTimeoutCallback(t *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Hour, logger)

	called := false
	monitor.SetTimeoutCallback(func() {
		called = true
	})

	if monitor.onTimeout == nil {
		t.Error("expected onTimeout callback to be set")
	}

	// Trigger the callback
	monitor.onTimeout()
	if !called {
		t.Error("expected callback to be called")
	}
}

func TestSafetyMonitor_GetRemainingTime(t *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Hour, logger)

	remaining := monitor.GetRemainingTime()

	// Should be close to 1 hour (minus a few milliseconds of execution)
	if remaining < 59*time.Minute || remaining > 1*time.Hour {
		t.Errorf("expected remaining time ~1 hour, got %v", remaining)
	}
}

func TestSafetyMonitor_GetRemainingTime_Expired(t *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Millisecond, logger)

	// Wait for expiration
	time.Sleep(10 * time.Millisecond)

	remaining := monitor.GetRemainingTime()
	if remaining != 0 {
		t.Errorf("expected remaining time 0, got %v", remaining)
	}
}

func TestSafetyMonitor_IsExpired_NotExpired(t *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Hour, logger)

	if monitor.IsExpired() {
		t.Error("expected monitor to not be expired")
	}
}

func TestSafetyMonitor_IsExpired_Expired(t *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Millisecond, logger)

	// Wait for expiration
	time.Sleep(10 * time.Millisecond)

	if !monitor.IsExpired() {
		t.Error("expected monitor to be expired")
	}
}

func TestSafetyMonitor_Monitor_Cancellation(t *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Hour, logger)
	monitor.checkInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		monitor.Monitor(ctx)
		close(done)
	}()

	// Cancel after a short time
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Wait for monitor to stop
	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not stop after cancellation")
	}
}

func TestSafetyMonitor_check_TimeoutCallback(t *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Millisecond, logger)

	// Wait for expiration
	time.Sleep(10 * time.Millisecond)

	var mu sync.Mutex
	called := false
	monitor.SetTimeoutCallback(func() {
		mu.Lock()
		called = true
		mu.Unlock()
	})

	monitor.check()

	mu.Lock()
	if !called {
		t.Error("expected timeout callback to be called")
	}
	mu.Unlock()
}

func TestSafetyMonitor_check_NoTimeoutYet(t *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Hour, logger)

	called := false
	monitor.SetTimeoutCallback(func() {
		called = true
	})

	monitor.check()

	if called {
		t.Error("timeout callback should not be called before expiration")
	}
}

func TestSafetyMonitor_check_LowTimeWarning(t *testing.T) {
	logger := &mockLogger{}
	// Set max runtime to just over 5 minutes so remaining is < 10 min
	monitor := NewSafetyMonitor(5*time.Minute, logger)

	monitor.check()

	// Should have logged a warning
	if len(logger.messages) == 0 {
		t.Error("expected warning message for low remaining time")
	}
}

func TestSafetyMonitor_checkDiskSpace(_ *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Hour, logger)

	// This test assumes we're running on a system with some disk space
	// It mainly tests that the function doesn't panic
	err := monitor.checkDiskSpace()
	// Error is acceptable on some systems, we just verify it doesn't panic
	_ = err
}

func TestSafetyMonitor_checkMemory(_ *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Hour, logger)

	// This test assumes we're running on a system (Linux has /proc/meminfo)
	// It mainly tests that the function doesn't panic
	err := monitor.checkMemory()
	// Error is acceptable on non-Linux systems
	_ = err
}

func TestConstants(t *testing.T) {
	if DefaultCheckInterval != 30*time.Second {
		t.Errorf("expected DefaultCheckInterval 30s, got %v", DefaultCheckInterval)
	}
	if MinDiskSpaceWarning != 500*1024*1024 {
		t.Errorf("expected MinDiskSpaceWarning 500MB, got %d", MinDiskSpaceWarning)
	}
	if MinMemoryWarning != 200*1024*1024 {
		t.Errorf("expected MinMemoryWarning 200MB, got %d", MinMemoryWarning)
	}
}

func TestSafetyMonitor_Monitor_PeriodicCheck(_ *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Hour, logger)
	monitor.checkInterval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go monitor.Monitor(ctx)

	// Wait for at least one check interval
	time.Sleep(100 * time.Millisecond)

	// Cancel and verify we got some messages from checks
	cancel()

	// Note: The number of messages depends on the system
	// We just verify the monitor ran without panicking
}
