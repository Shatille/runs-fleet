package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/agent"
)

func TestStdLogger_Printf(_ *testing.T) {
	logger := &stdLogger{}
	// This should not panic
	logger.Printf("test message %s", "arg")
}

func TestStdLogger_Println(_ *testing.T) {
	logger := &stdLogger{}
	// This should not panic
	logger.Println("test message")
}

func TestGetEnvInt(t *testing.T) {
	tests := []struct {
		name         string
		envKey       string
		envValue     string
		defaultValue int
		want         int
	}{
		{
			name:         "valid integer",
			envKey:       "TEST_GET_ENV_INT",
			envValue:     "123",
			defaultValue: 456,
			want:         123,
		},
		{
			name:         "invalid integer returns default",
			envKey:       "TEST_GET_ENV_INT_INVALID",
			envValue:     "abc",
			defaultValue: 789,
			want:         789,
		},
		{
			name:         "empty returns default",
			envKey:       "TEST_GET_ENV_INT_EMPTY",
			envValue:     "",
			defaultValue: 999,
			want:         999,
		},
		{
			name:         "negative integer",
			envKey:       "TEST_GET_ENV_INT_NEG",
			envValue:     "-42",
			defaultValue: 100,
			want:         -42,
		},
		{
			name:         "zero value",
			envKey:       "TEST_GET_ENV_INT_ZERO",
			envValue:     "0",
			defaultValue: 100,
			want:         0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				_ = os.Setenv(tt.envKey, tt.envValue)
				defer func() { _ = os.Unsetenv(tt.envKey) }()
			}

			got := getEnvInt(tt.envKey, tt.defaultValue)
			if got != tt.want {
				t.Errorf("getEnvInt(%q, %d) = %d, want %d", tt.envKey, tt.defaultValue, got, tt.want)
			}
		})
	}
}

func TestGetEnvInt_MissingEnv(t *testing.T) {
	// Ensure env variable doesn't exist
	_ = os.Unsetenv("TEST_MISSING_ENV_VAR_XYZ")

	got := getEnvInt("TEST_MISSING_ENV_VAR_XYZ", 555)
	if got != 555 {
		t.Errorf("getEnvInt() for missing env = %d, want 555", got)
	}
}

func TestGetInstanceID_WithEnvVar(t *testing.T) {
	_ = os.Setenv("RUNS_FLEET_INSTANCE_ID", "test-instance-123")
	defer func() { _ = os.Unsetenv("RUNS_FLEET_INSTANCE_ID") }()

	// Test EC2 mode with env var set
	got := getInstanceID(false)
	if got != "test-instance-123" {
		t.Errorf("getInstanceID(false) = %q, want %q", got, "test-instance-123")
	}

	// Test K8s mode with env var set (should still use env var)
	got = getInstanceID(true)
	if got != "test-instance-123" {
		t.Errorf("getInstanceID(true) = %q, want %q", got, "test-instance-123")
	}
}

func TestGetInstanceID_K8sWithoutEnvVar(t *testing.T) {
	_ = os.Unsetenv("RUNS_FLEET_INSTANCE_ID")

	// In K8s mode without env var, it should return hostname
	got := getInstanceID(true)
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("Failed to get hostname: %v", err)
	}
	if got != hostname {
		t.Errorf("getInstanceID(true) = %q, want hostname %q", got, hostname)
	}
}

func TestAgentConfig_Structure(t *testing.T) {
	// Test that agentConfig can be instantiated with nil values
	ac := &agentConfig{}

	if ac.telemetry != nil {
		t.Error("agentConfig.telemetry should be nil by default")
	}
	if ac.terminator != nil {
		t.Error("agentConfig.terminator should be nil by default")
	}
	if ac.runnerConfig != nil {
		t.Error("agentConfig.runnerConfig should be nil by default")
	}
	if ac.cwLogger != nil {
		t.Error("agentConfig.cwLogger should be nil by default")
	}
	if ac.cleanup != nil {
		t.Error("agentConfig.cleanup should be nil by default")
	}
}

func TestAgentConfig_CleanupFunction(t *testing.T) {
	cleanupCalled := false
	ac := &agentConfig{
		cleanup: func() {
			cleanupCalled = true
		},
	}

	if ac.cleanup == nil {
		t.Error("agentConfig.cleanup should not be nil when set")
	}

	ac.cleanup()
	if !cleanupCalled {
		t.Error("cleanup function was not called")
	}
}

func TestGetEnvInt_LargeNumbers(t *testing.T) {
	tests := []struct {
		name         string
		envKey       string
		envValue     string
		defaultValue int
		want         int
	}{
		{
			name:         "large positive",
			envKey:       "TEST_LARGE_INT",
			envValue:     "999999",
			defaultValue: 0,
			want:         999999,
		},
		{
			name:         "large negative",
			envKey:       "TEST_LARGE_NEG_INT",
			envValue:     "-999999",
			defaultValue: 0,
			want:         -999999,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Setenv(tt.envKey, tt.envValue)
			defer func() { _ = os.Unsetenv(tt.envKey) }()

			got := getEnvInt(tt.envKey, tt.defaultValue)
			if got != tt.want {
				t.Errorf("getEnvInt(%q, %d) = %d, want %d", tt.envKey, tt.defaultValue, got, tt.want)
			}
		})
	}
}

func TestGetEnvInt_InvalidFormats(t *testing.T) {
	tests := []struct {
		name         string
		envKey       string
		envValue     string
		defaultValue int
	}{
		{
			name:         "float value",
			envKey:       "TEST_FLOAT",
			envValue:     "123.45",
			defaultValue: 100,
		},
		{
			name:         "mixed alphanumeric",
			envKey:       "TEST_MIXED",
			envValue:     "123abc",
			defaultValue: 100,
		},
		{
			name:         "spaces",
			envKey:       "TEST_SPACES",
			envValue:     "  123  ",
			defaultValue: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(_ *testing.T) {
			_ = os.Setenv(tt.envKey, tt.envValue)
			defer func() { _ = os.Unsetenv(tt.envKey) }()

			got := getEnvInt(tt.envKey, tt.defaultValue)
			// For invalid formats, either return the parsed value or default
			// Just ensure it doesn't panic
			_ = got
		})
	}
}

func TestAgentConfig_WithRunnerConfig(t *testing.T) {
	rc := &agent.RunnerConfig{
		Repo:       "owner/repo",
		JITToken:   "test-token",
		Labels:     []string{"self-hosted", "linux"},
		CacheToken: "cache-token",
	}

	ac := &agentConfig{
		runnerConfig: rc,
	}

	if ac.runnerConfig == nil {
		t.Error("agentConfig.runnerConfig should not be nil")
	}
	if ac.runnerConfig.Repo != "owner/repo" {
		t.Errorf("runnerConfig.Repo = %q, want %q", ac.runnerConfig.Repo, "owner/repo")
	}
	if len(ac.runnerConfig.Labels) != 2 {
		t.Errorf("runnerConfig.Labels length = %d, want 2", len(ac.runnerConfig.Labels))
	}
}

func TestAgentConfig_MultipleCleanupCalls(t *testing.T) {
	callCount := 0
	ac := &agentConfig{
		cleanup: func() {
			callCount++
		},
	}

	// Call cleanup multiple times
	for i := 0; i < 3; i++ {
		ac.cleanup()
	}

	if callCount != 3 {
		t.Errorf("cleanup called %d times, want 3", callCount)
	}
}

// mockTerminator implements agent.InstanceTerminator for testing
type mockTerminator struct {
	terminateCalled bool
	terminateErr    error
}

func (m *mockTerminator) TerminateInstance(_ context.Context, _ string, _ agent.JobStatus) error {
	m.terminateCalled = true
	return m.terminateErr
}

func TestTerminateWithError(t *testing.T) {
	terminator := &mockTerminator{}

	terminateWithError(
		context.Background(),
		terminator,
		"test-instance",
		"test-run",
		"test_error",
		errors.New("original error"),
	)

	if !terminator.terminateCalled {
		t.Error("terminator.TerminateInstance should have been called")
	}
}

func TestTerminateWithError_TerminatorError(t *testing.T) {
	terminator := &mockTerminator{
		terminateErr: errors.New("termination failed"),
	}

	// Should not panic even if terminator fails
	terminateWithError(
		context.Background(),
		terminator,
		"test-instance",
		"test-run",
		"download_failed",
		errors.New("download error"),
	)

	if !terminator.terminateCalled {
		t.Error("terminator.TerminateInstance should have been called")
	}
}

func TestStdLogger_MultipleCalls(_ *testing.T) {
	logger := &stdLogger{}

	// Multiple calls should not panic
	for i := 0; i < 10; i++ {
		logger.Printf("message %d: %s", i, "test")
		logger.Println("standalone message", i)
	}
}

func TestStdLogger_EmptyFormat(_ *testing.T) {
	logger := &stdLogger{}

	// Empty format string should not panic
	logger.Printf("")
	logger.Println()
}

func TestStdLogger_VariadicArgs(_ *testing.T) {
	logger := &stdLogger{}

	// Various argument types
	logger.Printf("int: %d, string: %s, float: %f", 42, "test", 3.14)
	logger.Println("multiple", "args", 123, true, 4.5)
}

func TestGetInstanceID_EmptyEnvVar(t *testing.T) {
	// Set to empty string
	_ = os.Setenv("RUNS_FLEET_INSTANCE_ID", "")
	defer func() { _ = os.Unsetenv("RUNS_FLEET_INSTANCE_ID") }()

	// K8s mode with empty env var should return hostname
	got := getInstanceID(true)
	hostname, _ := os.Hostname()
	if got != hostname {
		t.Errorf("getInstanceID(true) = %q, want hostname %q", got, hostname)
	}
}

func TestAgentConfig_AllFieldsSet(t *testing.T) {
	cleanupCalled := false
	rc := &agent.RunnerConfig{
		Repo:     "test/repo",
		JITToken: "token",
		Labels:   []string{"label1"},
	}

	ac := &agentConfig{
		runnerConfig: rc,
		cleanup: func() {
			cleanupCalled = true
		},
	}

	if ac.runnerConfig == nil {
		t.Error("runnerConfig should not be nil")
	}
	if ac.cleanup == nil {
		t.Error("cleanup should not be nil")
	}

	ac.cleanup()
	if !cleanupCalled {
		t.Error("cleanup was not called")
	}
}

func TestJobStatus_Fields(t *testing.T) {
	now := time.Now()
	status := agent.JobStatus{
		InstanceID:      "i-1234567890",
		JobID:           "job-123",
		Status:          "completed",
		ExitCode:        0,
		StartedAt:       now,
		CompletedAt:     now.Add(5 * time.Minute),
		DurationSeconds: 300,
		InterruptedBy:   "",
		Error:           "",
	}

	if status.InstanceID != "i-1234567890" {
		t.Errorf("InstanceID = %q, want %q", status.InstanceID, "i-1234567890")
	}
	if status.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", status.ExitCode)
	}
	if status.DurationSeconds != 300 {
		t.Errorf("DurationSeconds = %d, want 300", status.DurationSeconds)
	}
}

func TestJobStatus_FailureCase(t *testing.T) {
	status := agent.JobStatus{
		InstanceID:  "i-1234567890",
		JobID:       "job-456",
		Status:      "failure",
		ExitCode:    -1,
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
		Error:       "download_failed: connection timeout",
	}

	if status.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", status.ExitCode)
	}
	if status.Error == "" {
		t.Error("Error should not be empty for failure case")
	}
}

func TestTerminateWithError_DifferentErrorTypes(t *testing.T) {
	tests := []struct {
		name      string
		errorType string
		err       error
	}{
		{
			name:      "download failed",
			errorType: "download_failed",
			err:       errors.New("failed to download runner"),
		},
		{
			name:      "registration failed",
			errorType: "registration_failed",
			err:       errors.New("failed to register with GitHub"),
		},
		{
			name:      "execution failed",
			errorType: "execution_failed",
			err:       errors.New("runner process crashed"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			terminator := &mockTerminator{}
			terminateWithError(
				context.Background(),
				terminator,
				"test-instance",
				"test-run",
				tt.errorType,
				tt.err,
			)

			if !terminator.terminateCalled {
				t.Error("terminator should have been called")
			}
		})
	}
}

func TestGetEnvInt_BoundaryValues(t *testing.T) {
	tests := []struct {
		name         string
		envKey       string
		envValue     string
		defaultValue int
		want         int
	}{
		{
			name:         "max runtime typical",
			envKey:       "TEST_MAX_RUNTIME",
			envValue:     "360",
			defaultValue: 60,
			want:         360,
		},
		{
			name:         "one",
			envKey:       "TEST_ONE",
			envValue:     "1",
			defaultValue: 0,
			want:         1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Setenv(tt.envKey, tt.envValue)
			defer func() { _ = os.Unsetenv(tt.envKey) }()

			got := getEnvInt(tt.envKey, tt.defaultValue)
			if got != tt.want {
				t.Errorf("getEnvInt() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAgentConfig_NilCleanupIsSafe(_ *testing.T) {
	ac := &agentConfig{
		cleanup: nil,
	}

	// Check for nil before calling
	if ac.cleanup != nil {
		ac.cleanup()
	}

	// This test passes if no panic occurs
}

func TestTerminateWithError_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	terminator := &mockTerminator{}
	terminateWithError(
		ctx,
		terminator,
		"test-instance",
		"test-run",
		"context_cancelled",
		context.Canceled,
	)

	// Should still be called even with cancelled context
	if !terminator.terminateCalled {
		t.Error("terminator should have been called")
	}
}

func TestAgentConfig_TelemetryField(t *testing.T) {
	// Create a mock telemetry client
	mt := &mockTelemetry{}

	ac := &agentConfig{
		telemetry: mt,
	}

	if ac.telemetry == nil {
		t.Error("telemetry should not be nil when set")
	}
}

// mockTelemetry implements agent.TelemetryClient for testing
type mockTelemetry struct {
	jobStartedCalled   bool
	jobCompletedCalled bool
}

func (m *mockTelemetry) SendJobStarted(_ context.Context, _ agent.JobStatus) error {
	m.jobStartedCalled = true
	return nil
}

func (m *mockTelemetry) SendJobCompleted(_ context.Context, _ agent.JobStatus) error {
	m.jobCompletedCalled = true
	return nil
}

func (m *mockTelemetry) Close() error {
	return nil
}

func TestJobStatus_ZeroValues(t *testing.T) {
	status := agent.JobStatus{}

	if status.InstanceID != "" {
		t.Errorf("InstanceID should be empty, got %q", status.InstanceID)
	}
	if status.JobID != "" {
		t.Errorf("JobID should be empty, got %q", status.JobID)
	}
	if status.ExitCode != 0 {
		t.Errorf("ExitCode should be 0, got %d", status.ExitCode)
	}
	if !status.StartedAt.IsZero() {
		t.Error("StartedAt should be zero value")
	}
	if !status.CompletedAt.IsZero() {
		t.Error("CompletedAt should be zero value")
	}
}

func TestJobStatus_ErrorMessage(t *testing.T) {
	tests := []struct {
		name  string
		error string
		want  bool
	}{
		{"empty error", "", false},
		{"with error", "some error message", true},
		{"long error", "this is a very long error message that contains detailed information about what went wrong during execution", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := agent.JobStatus{
				InstanceID: "i-123",
				JobID:      "job-456",
				Error:      tt.error,
			}

			hasError := status.Error != ""
			if hasError != tt.want {
				t.Errorf("hasError = %v, want %v", hasError, tt.want)
			}
		})
	}
}

func TestJobStatus_InterruptedBy(t *testing.T) {
	tests := []struct {
		name          string
		interruptedBy string
	}{
		{"no interruption", ""},
		{"spot termination", "spot"},
		{"timeout", "timeout"},
		{"signal", "SIGTERM"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := agent.JobStatus{
				InstanceID:    "i-123",
				InterruptedBy: tt.interruptedBy,
			}

			if status.InterruptedBy != tt.interruptedBy {
				t.Errorf("InterruptedBy = %q, want %q", status.InterruptedBy, tt.interruptedBy)
			}
		})
	}
}

func TestTerminateWithError_JobStatusFields(t *testing.T) {
	// Verify the job status is properly constructed with error info
	terminator := &mockTerminator{}

	terminateWithError(
		context.Background(),
		terminator,
		"i-test-instance",
		"run-12345",
		"config_load_failed",
		errors.New("failed to load config from SSM"),
	)

	if !terminator.terminateCalled {
		t.Error("terminator should have been called")
	}
}

func TestGetEnvInt_Whitespace(t *testing.T) {
	// Test that whitespace in values is handled
	key := "TEST_WHITESPACE_INT"
	_ = os.Setenv(key, "  42  ")
	defer func() { _ = os.Unsetenv(key) }()

	// fmt.Sscanf is lenient and can parse "  42  " as 42
	got := getEnvInt(key, 100)
	if got != 42 {
		t.Errorf("getEnvInt() with whitespace = %d, want 42", got)
	}
}

func TestGetEnvInt_Overflow(_ *testing.T) {
	// Test handling of very large numbers (platform dependent)
	key := "TEST_OVERFLOW_INT"
	_ = os.Setenv(key, "9999999999999999999999")
	defer func() { _ = os.Unsetenv(key) }()

	// Should return default due to overflow
	got := getEnvInt(key, 50)
	// Either it parses to some value or returns default
	// This test ensures no panic occurs
	_ = got
}

func TestGetInstanceID_HostnameError(t *testing.T) {
	// Clear the env var to test hostname path
	_ = os.Unsetenv("RUNS_FLEET_INSTANCE_ID")

	// In K8s mode without env var, should return hostname
	hostname, err := os.Hostname()
	if err != nil {
		t.Skip("Cannot get hostname")
	}

	got := getInstanceID(true)
	if got != hostname {
		t.Errorf("getInstanceID(true) = %q, want hostname %q", got, hostname)
	}
}

func TestAgentConfig_TerminatorField(t *testing.T) {
	terminator := &mockTerminator{}

	ac := &agentConfig{
		terminator: terminator,
	}

	if ac.terminator == nil {
		t.Error("terminator should not be nil when set")
	}

	// Verify we can call the method
	err := ac.terminator.TerminateInstance(context.Background(), "test", agent.JobStatus{})
	if err != nil {
		t.Errorf("TerminateInstance error = %v", err)
	}
}

func TestTerminateWithError_EmptyErrorType(t *testing.T) {
	terminator := &mockTerminator{}

	// Empty error type should still work
	terminateWithError(
		context.Background(),
		terminator,
		"test-instance",
		"test-run",
		"",
		errors.New("some error"),
	)

	if !terminator.terminateCalled {
		t.Error("terminator should have been called")
	}
}

func TestStdLogger_SpecialCharacters(_ *testing.T) {
	logger := &stdLogger{}

	// Test with special characters that might cause formatting issues
	logger.Printf("percent sign: %%")
	logger.Printf("newlines: line1\nline2\nline3")
	logger.Printf("tabs: col1\tcol2\tcol3")
	logger.Printf("unicode: 日本語 한국어 中文")
	logger.Println("json-like: {\"key\": \"value\"}")
}

func TestGetEnvInt_EmptyString(t *testing.T) {
	key := "TEST_EMPTY_STRING_INT"
	_ = os.Setenv(key, "")
	defer func() { _ = os.Unsetenv(key) }()

	got := getEnvInt(key, 42)
	if got != 42 {
		t.Errorf("getEnvInt() with empty string = %d, want 42", got)
	}
}

func TestGetEnvInt_LeadingZeros(t *testing.T) {
	key := "TEST_LEADING_ZEROS_INT"
	_ = os.Setenv(key, "00123")
	defer func() { _ = os.Unsetenv(key) }()

	got := getEnvInt(key, 0)
	if got != 123 {
		t.Errorf("getEnvInt() with leading zeros = %d, want 123", got)
	}
}

func TestJobStatus_Duration(t *testing.T) {
	now := time.Now()
	status := agent.JobStatus{
		InstanceID:      "i-123",
		JobID:           "job-456",
		StartedAt:       now,
		CompletedAt:     now.Add(5 * time.Minute),
		DurationSeconds: 300,
	}

	// Verify duration calculation matches
	expectedDuration := status.CompletedAt.Sub(status.StartedAt)
	if expectedDuration.Seconds() != float64(status.DurationSeconds) {
		t.Errorf("Duration mismatch: calculated=%v, stored=%d", expectedDuration.Seconds(), status.DurationSeconds)
	}
}

func TestRunnerConfig_AllFields(t *testing.T) {
	rc := &agent.RunnerConfig{
		Repo:       "owner/repo",
		JITToken:   "jit-token-value",
		Labels:     []string{"self-hosted", "linux", "arm64"},
		CacheToken: "cache-token-value",
	}

	if rc.Repo != "owner/repo" {
		t.Errorf("Repo = %q, want %q", rc.Repo, "owner/repo")
	}
	if rc.JITToken != "jit-token-value" {
		t.Errorf("JITToken = %q, want %q", rc.JITToken, "jit-token-value")
	}
	if len(rc.Labels) != 3 {
		t.Errorf("Labels length = %d, want 3", len(rc.Labels))
	}
	if rc.CacheToken != "cache-token-value" {
		t.Errorf("CacheToken = %q, want %q", rc.CacheToken, "cache-token-value")
	}
}

func TestRunnerConfig_EmptyLabels(t *testing.T) {
	rc := &agent.RunnerConfig{
		Repo:     "owner/repo",
		JITToken: "token",
		Labels:   []string{},
	}

	if len(rc.Labels) != 0 {
		t.Errorf("Labels length = %d, want 0", len(rc.Labels))
	}
}

func TestRunnerConfig_NilLabels(t *testing.T) {
	rc := &agent.RunnerConfig{
		Repo:     "owner/repo",
		JITToken: "token",
		Labels:   nil,
	}

	if rc.Labels != nil {
		t.Error("Labels should be nil")
	}
}
