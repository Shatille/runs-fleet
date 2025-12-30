package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// waitForCheck waits for n check callbacks with a timeout.
// Returns true if all checks were received, false on timeout.
func waitForCheck(ch <-chan struct{}, n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-deadline:
			return false
		}
	}
	return true
}

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
	monitor := NewSafetyMonitor(1*time.Minute, logger)

	// Set startTime in the past to simulate expiration (no time.Sleep needed)
	monitor.startTime = time.Now().Add(-2 * time.Minute)

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
	monitor := NewSafetyMonitor(1*time.Minute, logger)

	// Set startTime in the past to simulate expiration (no time.Sleep needed)
	monitor.startTime = time.Now().Add(-2 * time.Minute)

	if !monitor.IsExpired() {
		t.Error("expected monitor to be expired")
	}
}

func TestSafetyMonitor_Monitor_Cancellation(t *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Hour, logger)
	monitor.checkInterval = 10 * time.Millisecond

	// Set up check callback to signal when a check occurs
	checkCh := make(chan struct{}, 1)
	monitor.SetCheckCallback(func() {
		select {
		case checkCh <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		monitor.Monitor(ctx)
		close(done)
	}()

	// Wait for at least one check to confirm monitor is running (channel-based sync)
	if !waitForCheck(checkCh, 1, 2*time.Second) {
		t.Fatal("timed out waiting for check to be called")
	}
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
	monitor := NewSafetyMonitor(1*time.Minute, logger)

	// Set startTime in the past to simulate expiration (no time.Sleep needed)
	monitor.startTime = time.Now().Add(-2 * time.Minute)

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

func TestSafetyMonitor_Monitor_PeriodicCheck(t *testing.T) {
	logger := &mockLogger{}
	monitor := NewSafetyMonitor(1*time.Hour, logger)
	monitor.checkInterval = 10 * time.Millisecond

	// Set up check callback to signal when checks occur
	checkCh := make(chan struct{}, 10)
	monitor.SetCheckCallback(func() {
		select {
		case checkCh <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go monitor.Monitor(ctx)

	// Wait for at least 2 checks to confirm periodic checking works (channel-based sync)
	if !waitForCheck(checkCh, 2, 2*time.Second) {
		t.Fatal("timed out waiting for periodic checks")
	}

	// Cancel and verify we got checks
	cancel()

	// Note: The number of messages depends on the system
	// We just verify the monitor ran without panicking
}
