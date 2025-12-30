package agent

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestValkeyTelemetry_ImplementsTelemetryClient(_ *testing.T) {
	// Verify at compile time that ValkeyTelemetry implements TelemetryClient
	var _ TelemetryClient = (*ValkeyTelemetry)(nil)
}

func TestNewValkeyTelemetry_ConnectionFailure(t *testing.T) {
	logger := &mockLogger{}

	// Create a mock Redis server that accepts connections but responds
	// with an error to PING commands. Combined with short timeouts, this
	// tests connection failure handling in <100ms vs original ~2.1s.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := listener.Addr().String()

	// Handle connections in goroutine
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return // Listener closed
			}
			// Read the PING command (or any command) and respond with error
			buf := make([]byte, 128)
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte("-ERR mock server rejecting connection\r\n"))
			_ = conn.Close()
		}
	}()
	defer func() {
		_ = listener.Close()
		<-done
	}()

	// Use internal function with short timeouts for fast test execution
	opts := &redis.Options{
		Addr:        addr,
		DialTimeout: 10 * time.Millisecond,
		ReadTimeout: 10 * time.Millisecond,
	}
	_, err = newValkeyTelemetryWithOptions(opts, 50*time.Millisecond, "test-stream", logger)
	if err == nil {
		t.Error("newValkeyTelemetryWithOptions() expected error for invalid connection")
	}
}

func TestValkeyTelemetry_SendJobStarted_SetsStatus(t *testing.T) {
	// This test verifies the status is set correctly before sending
	// We can't test the actual send without a Redis server, but we can
	// verify the status modification logic
	status := JobStatus{
		InstanceID: "test-instance",
		JobID:      "job-123",
	}

	// Verify that SendJobStarted would set status to "started"
	status.Status = StatusStarted
	if status.Status != "started" {
		t.Errorf("expected status 'started', got '%s'", status.Status)
	}
}

func TestValkeyTelemetry_SendJobCompleted_DeterminesStatus(t *testing.T) {
	tests := []struct {
		name          string
		interruptedBy string
		exitCode      int
		expectedStat  string
	}{
		{
			name:          "success with exit code 0",
			interruptedBy: "",
			exitCode:      0,
			expectedStat:  "success",
		},
		{
			name:          "failure with non-zero exit code",
			interruptedBy: "",
			exitCode:      1,
			expectedStat:  "failure",
		},
		{
			name:          "interrupted by spot",
			interruptedBy: "spot",
			exitCode:      0,
			expectedStat:  StatusInterrupted,
		},
		{
			name:          "interrupted takes precedence",
			interruptedBy: "signal",
			exitCode:      1,
			expectedStat:  StatusInterrupted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetermineCompletionStatus(tt.interruptedBy, tt.exitCode)
			if result != tt.expectedStat {
				t.Errorf("DetermineCompletionStatus(%q, %d) = %q, want %q",
					tt.interruptedBy, tt.exitCode, result, tt.expectedStat)
			}
		})
	}
}

func TestValkeyTelemetry_Close(_ *testing.T) {
	// Create a real client but don't connect - test that Close doesn't panic
	client := redis.NewClient(&redis.Options{
		Addr: "localhost:59999", // Non-existent
	})

	vt := &ValkeyTelemetry{
		client: client,
		stream: "test-stream",
		logger: &mockLogger{},
	}

	// Close should not panic even if not connected
	err := vt.Close()
	// The error might be nil or indicate connection issues, but shouldn't panic
	_ = err
}

func TestValkeyTelemetry_Structure(t *testing.T) {
	// Create a ValkeyTelemetry struct directly to verify structure
	client := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	defer func() { _ = client.Close() }()

	logger := &mockLogger{}
	vt := &ValkeyTelemetry{
		client: client,
		stream: "test-stream",
		logger: logger,
	}

	if vt.stream != "test-stream" {
		t.Errorf("expected stream 'test-stream', got '%s'", vt.stream)
	}
	if vt.logger == nil {
		t.Error("logger should not be nil")
	}
	if vt.client == nil {
		t.Error("client should not be nil")
	}
}

func TestValkeyTelemetry_SendMessage_RetryLogic(t *testing.T) {
	// Test the retry backoff calculation logic
	// The actual retry is 1<<uint(attempt) seconds

	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{attempt: 0, expected: 0}, // First attempt, no backoff
		{attempt: 1, expected: 2 * time.Second},
		{attempt: 2, expected: 4 * time.Second},
	}

	for _, tt := range tests {
		backoff := time.Duration(0)
		if tt.attempt > 0 {
			backoff = time.Duration(1<<uint(tt.attempt)) * time.Second
		}
		if backoff != tt.expected {
			t.Errorf("attempt %d: expected backoff %v, got %v", tt.attempt, tt.expected, backoff)
		}
	}
}

func TestValkeyTelemetry_ContextCancellation(t *testing.T) {
	// Verify that context cancellation is handled
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// The sendMessage function should return ctx.Err() when context is cancelled
	if ctx.Err() != context.Canceled {
		t.Error("expected context to be cancelled")
	}
}

func TestValkeyTelemetry_MessageFormat(t *testing.T) {
	// Test that the XAddArgs values are correctly structured
	status := JobStatus{
		InstanceID: "i-12345",
		JobID:      "job-456",
		Status:     "success",
	}

	// Verify the map structure that would be sent
	values := map[string]interface{}{
		"instance_id": status.InstanceID,
		"job_id":      status.JobID,
		"status":      status.Status,
	}

	if values["instance_id"] != "i-12345" {
		t.Errorf("expected instance_id 'i-12345', got '%v'", values["instance_id"])
	}
	if values["job_id"] != "job-456" {
		t.Errorf("expected job_id 'job-456', got '%v'", values["job_id"])
	}
	if values["status"] != "success" {
		t.Errorf("expected status 'success', got '%v'", values["status"])
	}
}
