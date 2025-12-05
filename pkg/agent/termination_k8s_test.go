package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func init() {
	// Use minimal delays in tests to avoid slow test execution
	k8sTerminationDelay = 1 * time.Millisecond
}

// mockTelemetryClient implements TelemetryClient for testing K8sTerminator.
type mockTelemetryClient struct {
	mu                  sync.Mutex
	sendStartedCalls    int
	sendCompletedCalls  int
	sendStartedErr      error
	sendCompletedErr    error
	lastCompletedStatus JobStatus
	sendCompletedFunc   func(ctx context.Context, status JobStatus) error
}

func (m *mockTelemetryClient) SendJobStarted(_ context.Context, _ JobStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendStartedCalls++
	return m.sendStartedErr
}

func (m *mockTelemetryClient) SendJobCompleted(ctx context.Context, status JobStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendCompletedCalls++
	m.lastCompletedStatus = status
	if m.sendCompletedFunc != nil {
		return m.sendCompletedFunc(ctx, status)
	}
	return m.sendCompletedErr
}

func (m *mockTelemetryClient) getCompletedCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sendCompletedCalls
}

func (m *mockTelemetryClient) getLastCompletedStatus() JobStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastCompletedStatus
}

func TestK8sTerminator_ImplementsInstanceTerminator(_ *testing.T) {
	// Verify at compile time that K8sTerminator implements InstanceTerminator
	var _ InstanceTerminator = (*K8sTerminator)(nil)
}

func TestNewK8sTerminator(t *testing.T) {
	logger := &mockLogger{}
	telemetry := &mockTelemetryClient{}

	terminator := NewK8sTerminator(telemetry, logger)

	if terminator == nil {
		t.Fatal("NewK8sTerminator() returned nil")
	}
	if terminator.telemetry == nil {
		t.Error("NewK8sTerminator() did not set telemetry")
	}
	if terminator.logger == nil {
		t.Error("NewK8sTerminator() did not set logger")
	}
}

func TestNewK8sTerminator_NilTelemetry(t *testing.T) {
	logger := &mockLogger{}

	terminator := NewK8sTerminator(nil, logger)

	if terminator == nil {
		t.Fatal("NewK8sTerminator() returned nil")
	}
	if terminator.telemetry != nil {
		t.Error("NewK8sTerminator() should allow nil telemetry")
	}
}

func TestK8sTerminator_TerminateInstance(t *testing.T) {
	t.Run("with telemetry", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(telemetry, logger)

		status := JobStatus{
			InstanceID:      "pod-123",
			JobID:           "job-456",
			ExitCode:        0,
			DurationSeconds: 60,
			CompletedAt:     time.Now(),
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		err := terminator.TerminateInstance(ctx, "pod-123", status)
		if err != nil {
			t.Fatalf("TerminateInstance() error = %v", err)
		}

		if telemetry.getCompletedCalls() != 1 {
			t.Errorf("expected 1 SendJobCompleted call, got %d", telemetry.getCompletedCalls())
		}
	})

	t.Run("without telemetry", func(t *testing.T) {
		logger := &mockLogger{}
		terminator := NewK8sTerminator(nil, logger)

		status := JobStatus{
			InstanceID: "pod-123",
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		err := terminator.TerminateInstance(ctx, "pod-123", status)
		if err != nil {
			t.Fatalf("TerminateInstance() error = %v", err)
		}
		// Should complete without telemetry
	})

	t.Run("telemetry error", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{
			sendCompletedErr: errors.New("telemetry failed"),
		}
		terminator := NewK8sTerminator(telemetry, logger)

		status := JobStatus{
			InstanceID: "pod-123",
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Should not return error even if telemetry fails
		err := terminator.TerminateInstance(ctx, "pod-123", status)
		if err != nil {
			t.Fatalf("TerminateInstance() should not return error for telemetry failure, got: %v", err)
		}
	})
}

func TestK8sTerminator_TerminateWithStatus(t *testing.T) {
	t.Run("success status", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(telemetry, logger)

		err := terminator.TerminateWithStatus("pod-123", "success", 0, 60*time.Second, "")
		if err != nil {
			t.Fatalf("TerminateWithStatus() error = %v", err)
		}

		status := telemetry.getLastCompletedStatus()
		if status.InstanceID != "pod-123" {
			t.Errorf("expected InstanceID 'pod-123', got '%s'", status.InstanceID)
		}
		if status.Status != "success" {
			t.Errorf("expected Status 'success', got '%s'", status.Status)
		}
		if status.ExitCode != 0 {
			t.Errorf("expected ExitCode 0, got %d", status.ExitCode)
		}
		if status.DurationSeconds != 60 {
			t.Errorf("expected DurationSeconds 60, got %d", status.DurationSeconds)
		}
	})

	t.Run("failure status", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(telemetry, logger)

		err := terminator.TerminateWithStatus("pod-456", "failure", 1, 30*time.Second, "job failed")
		if err != nil {
			t.Fatalf("TerminateWithStatus() error = %v", err)
		}

		status := telemetry.getLastCompletedStatus()
		if status.Status != "failure" {
			t.Errorf("expected Status 'failure', got '%s'", status.Status)
		}
		if status.ExitCode != 1 {
			t.Errorf("expected ExitCode 1, got %d", status.ExitCode)
		}
		if status.Error != "job failed" {
			t.Errorf("expected Error 'job failed', got '%s'", status.Error)
		}
	})
}

func TestK8sTerminator_TerminateOnPanic(t *testing.T) {
	t.Run("string panic", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(telemetry, logger)

		terminator.TerminateOnPanic("pod-789", "job-999", "test panic message")

		status := telemetry.getLastCompletedStatus()
		if status.InstanceID != "pod-789" {
			t.Errorf("expected InstanceID 'pod-789', got '%s'", status.InstanceID)
		}
		if status.JobID != "job-999" {
			t.Errorf("expected JobID 'job-999', got '%s'", status.JobID)
		}
		if status.Status != "failure" {
			t.Errorf("expected Status 'failure', got '%s'", status.Status)
		}
		if status.ExitCode != -1 {
			t.Errorf("expected ExitCode -1, got %d", status.ExitCode)
		}
		if status.Error != "agent panic: test panic message" {
			t.Errorf("expected Error 'agent panic: test panic message', got '%s'", status.Error)
		}
	})

	t.Run("error panic", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(telemetry, logger)

		terminator.TerminateOnPanic("pod-123", "job-456", errors.New("error panic"))

		status := telemetry.getLastCompletedStatus()
		if status.Error != "agent panic: error panic" {
			t.Errorf("expected Error 'agent panic: error panic', got '%s'", status.Error)
		}
	})

	t.Run("other panic type", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(telemetry, logger)

		terminator.TerminateOnPanic("pod-123", "job-456", 12345)

		status := telemetry.getLastCompletedStatus()
		if status.Error != "agent panic: unknown panic" {
			t.Errorf("expected Error 'agent panic: unknown panic', got '%s'", status.Error)
		}
	})

	t.Run("telemetry failure", func(_ *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{
			sendCompletedErr: errors.New("failed"),
		}
		terminator := NewK8sTerminator(telemetry, logger)

		// Should not panic even if telemetry fails
		terminator.TerminateOnPanic("pod-123", "job-456", "panic")
	})
}

func TestFormatPanicValue(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{
			name:     "string value",
			input:    "panic message",
			expected: "panic message",
		},
		{
			name:     "error value",
			input:    errors.New("error message"),
			expected: "error message",
		},
		{
			name:     "int value",
			input:    42,
			expected: "unknown panic",
		},
		{
			name:     "nil value",
			input:    nil,
			expected: "unknown panic",
		},
		{
			name:     "struct value",
			input:    struct{ X int }{X: 1},
			expected: "unknown panic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatPanicValue(tt.input)
			if result != tt.expected {
				t.Errorf("formatPanicValue(%v) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}
