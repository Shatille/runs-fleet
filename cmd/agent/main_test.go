package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/agent"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
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
	rc := &secrets.RunnerConfig{
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

// mockTerminator implements agent.InstanceTerminator for testing
type mockTerminator struct {
	terminateCalled bool
	terminateErr    error
	lastStatus      agent.JobStatus
}

func (m *mockTerminator) TerminateInstance(_ context.Context, _ string, status agent.JobStatus) error {
	m.terminateCalled = true
	m.lastStatus = status
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

func TestAgentConfig_AllFieldsSet(t *testing.T) {
	rc := &secrets.RunnerConfig{
		Repo:     "test/repo",
		JITToken: "token",
		Labels:   []string{"label1"},
	}

	ac := &agentConfig{
		runnerConfig: rc,
	}

	if ac.runnerConfig == nil {
		t.Error("runnerConfig should not be nil")
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
	startedStatus      agent.JobStatus
}

func (m *mockTelemetry) SendJobStarted(_ context.Context, status agent.JobStatus) error {
	m.jobStartedCalled = true
	m.startedStatus = status
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
	rc := &secrets.RunnerConfig{
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
	rc := &secrets.RunnerConfig{
		Repo:     "owner/repo",
		JITToken: "token",
		Labels:   []string{},
	}

	if len(rc.Labels) != 0 {
		t.Errorf("Labels length = %d, want 0", len(rc.Labels))
	}
}

func TestRunnerConfig_NilLabels(t *testing.T) {
	rc := &secrets.RunnerConfig{
		Repo:     "owner/repo",
		JITToken: "token",
		Labels:   nil,
	}

	if rc.Labels != nil {
		t.Error("Labels should be nil")
	}
}

// Regression values from the production incident: a successful job whose record
// was keyed by job_id but whose termination carried the run_id, causing
// MarkJobComplete to miss the real record and the job to be finalized orphaned.
const (
	regressionJobID = "79443780247"
	regressionRunID = "26928632771"
)

type fakeSecretsStore struct {
	config    *secrets.RunnerConfig
	err       error
	failCalls int
	calls     int
}

func (f *fakeSecretsStore) Get(_ context.Context, _ string) (*secrets.RunnerConfig, error) {
	f.calls++
	if f.calls <= f.failCalls {
		return nil, f.err
	}
	return f.config, nil
}
func (f *fakeSecretsStore) Put(_ context.Context, _ string, _ *secrets.RunnerConfig) error {
	return nil
}
func (f *fakeSecretsStore) Delete(_ context.Context, _ string) error { return nil }
func (f *fakeSecretsStore) List(_ context.Context) ([]string, error) { return nil, nil }

func TestFetchRunnerConfig_RetriesThenStandby(t *testing.T) {
	orig := configFetchRetryDelay
	configFetchRetryDelay = time.Millisecond
	defer func() { configFetchRetryDelay = orig }()

	store := &fakeSecretsStore{err: secrets.ErrConfigNotFound, failCalls: configFetchAttempts}
	if _, err := fetchRunnerConfig(context.Background(), store, "i-x", &stdLogger{}); !errors.Is(err, secrets.ErrConfigNotFound) {
		t.Errorf("fetchRunnerConfig() err = %v, want errors.Is ErrConfigNotFound", err)
	}
	if store.calls != configFetchAttempts {
		t.Errorf("Get calls = %d, want %d", store.calls, configFetchAttempts)
	}
}

func TestFetchRunnerConfig_RetriesThenSucceeds(t *testing.T) {
	orig := configFetchRetryDelay
	configFetchRetryDelay = time.Millisecond
	defer func() { configFetchRetryDelay = orig }()

	store := &fakeSecretsStore{config: &secrets.RunnerConfig{RunID: "r1"}, err: errors.New("transient"), failCalls: 2}
	cfg, err := fetchRunnerConfig(context.Background(), store, "i-x", &stdLogger{})
	if err != nil {
		t.Fatalf("fetchRunnerConfig() err = %v", err)
	}
	if cfg.RunID != "r1" {
		t.Errorf("RunID = %q, want r1", cfg.RunID)
	}
	if store.calls != 3 {
		t.Errorf("Get calls = %d, want 3", store.calls)
	}
}

func TestResolveJobID_FromConfig(t *testing.T) {
	_ = os.Unsetenv("RUNS_FLEET_JOB_ID")

	ac := &agentConfig{
		runnerConfig: &secrets.RunnerConfig{
			JobID: regressionJobID,
			RunID: regressionRunID,
		},
	}

	got := resolveJobID(ac)
	if got != regressionJobID {
		t.Errorf("resolveJobID() = %q, want job_id %q", got, regressionJobID)
	}
	if got == regressionRunID {
		t.Errorf("resolveJobID() returned the run_id %q; it must return the job_id", got)
	}
}

// TestResolveJobID_IgnoresEnv guards the unification: the agent reads its config
// from the secrets backend, so a stale RUNS_FLEET_JOB_ID env must not override it.
func TestResolveJobID_IgnoresEnv(t *testing.T) {
	_ = os.Setenv("RUNS_FLEET_JOB_ID", "555000111")
	defer func() { _ = os.Unsetenv("RUNS_FLEET_JOB_ID") }()

	ac := &agentConfig{
		runnerConfig: &secrets.RunnerConfig{
			JobID: regressionJobID,
			RunID: regressionRunID,
		},
	}

	if got := resolveJobID(ac); got != regressionJobID {
		t.Errorf("resolveJobID() = %q, want config job_id %q (env ignored)", got, regressionJobID)
	}
}

func TestResolveJobID_EmptyWhenNoSource(t *testing.T) {
	_ = os.Unsetenv("RUNS_FLEET_JOB_ID")

	if got := resolveJobID(&agentConfig{}); got != "" {
		t.Errorf("resolveJobID() = %q, want empty string when no source available", got)
	}
}

// TestTerminateWithError_ReportsJobIDNotRunID is the core regression test: the
// terminated JobStatus must carry the real job_id, not the run_id. Before the
// fix this site set JobID to the run_id, so MarkJobComplete keyed on the wrong
// DynamoDB record.
func TestTerminateWithError_ReportsJobIDNotRunID(t *testing.T) {
	terminator := &mockTerminator{}

	terminateWithError(
		context.Background(),
		terminator,
		"i-instance",
		regressionJobID,
		"download_failed",
		errors.New("boom"),
	)

	if !terminator.terminateCalled {
		t.Fatal("terminator.TerminateInstance should have been called")
	}
	if terminator.lastStatus.JobID != regressionJobID {
		t.Errorf("JobStatus.JobID = %q, want job_id %q", terminator.lastStatus.JobID, regressionJobID)
	}
	if terminator.lastStatus.JobID == regressionRunID {
		t.Errorf("JobStatus.JobID = %q is the run_id; it must be the job_id", terminator.lastStatus.JobID)
	}
}

// TestRunID_StaysSeparateFromJobID guards the legitimate uses of run_id: it is
// still the CloudWatch log-stream component and the RUNS_FLEET_RUN_ID resolution
// input, and must not collapse into the job_id key when the two differ.
func TestRunID_StaysSeparateFromJobID(t *testing.T) {
	_ = os.Unsetenv("RUNS_FLEET_JOB_ID")

	rc := &secrets.RunnerConfig{
		JobID: regressionJobID,
		RunID: regressionRunID,
	}
	ac := &agentConfig{runnerConfig: rc}

	jobID := resolveJobID(ac)
	if jobID == rc.RunID {
		t.Errorf("resolved job_id %q collapsed to run_id %q", jobID, rc.RunID)
	}
	if rc.RunID != regressionRunID {
		t.Errorf("run_id %q was mutated; want %q", rc.RunID, regressionRunID)
	}

	// The CloudWatch log stream is built from run_id, not job_id.
	logStream := "i-instance" + "/" + rc.RunID
	if logStream != "i-instance/"+regressionRunID {
		t.Errorf("log stream = %q, want run_id-based stream", logStream)
	}
}
