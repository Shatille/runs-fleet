package main

import (
	"os"
	"testing"
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
	if ac.cacheClient != nil {
		t.Error("agentConfig.cacheClient should be nil by default")
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
