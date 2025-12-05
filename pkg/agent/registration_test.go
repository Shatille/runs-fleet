package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

func init() {
	// Use minimal delays in tests to avoid slow test execution
	registrationRetryBaseDelay = 1 * time.Millisecond
}

const (
	testTokenValue = "test-token"
)

type mockSSMAPI struct {
	getParameterFunc func(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

func (m *mockSSMAPI) GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	if m.getParameterFunc != nil {
		return m.getParameterFunc(ctx, params, optFns...)
	}
	return nil, errors.New("not implemented")
}

func TestRegistrar_FetchConfig_Success(t *testing.T) {
	config := RunnerConfig{
		Org:      "test-org",
		JITToken: testTokenValue,
		Labels:   []string{"self-hosted", "linux"},
		IsOrg:    true,
	}
	configJSON, _ := json.Marshal(config)

	mock := &mockSSMAPI{
		getParameterFunc: func(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
			return &ssm.GetParameterOutput{
				Parameter: &types.Parameter{
					Value: aws.String(string(configJSON)),
				},
			}, nil
		},
	}

	registrar := &Registrar{
		ssmClient: mock,
		logger:    &mockLogger{},
	}

	result, err := registrar.FetchConfig(context.Background(), "/runs-fleet/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Org != "test-org" {
		t.Errorf("expected org 'test-org', got %q", result.Org)
	}
	if result.JITToken != testTokenValue {
		t.Errorf("expected jit_token '%s', got %q", testTokenValue, result.JITToken)
	}
	if !result.IsOrg {
		t.Error("expected IsOrg to be true")
	}
}

func TestRegistrar_FetchConfig_RetryOnError(t *testing.T) {
	attempts := 0
	config := RunnerConfig{Org: "test-org", JITToken: "token", IsOrg: true}
	configJSON, _ := json.Marshal(config)

	mock := &mockSSMAPI{
		getParameterFunc: func(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
			attempts++
			if attempts < 3 {
				return nil, errors.New("temporary error")
			}
			return &ssm.GetParameterOutput{
				Parameter: &types.Parameter{
					Value: aws.String(string(configJSON)),
				},
			}, nil
		},
	}

	registrar := &Registrar{
		ssmClient: mock,
		logger:    &mockLogger{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := registrar.FetchConfig(ctx, "/runs-fleet/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Org != "test-org" {
		t.Errorf("expected org 'test-org', got %q", result.Org)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestRegistrar_FetchConfig_AllRetriesFailed(t *testing.T) {
	mock := &mockSSMAPI{
		getParameterFunc: func(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
			return nil, errors.New("persistent error")
		},
	}

	registrar := &Registrar{
		ssmClient: mock,
		logger:    &mockLogger{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := registrar.FetchConfig(ctx, "/runs-fleet/test")
	if err == nil {
		t.Error("expected error after all retries failed")
	}
}

func TestRegistrar_FetchConfig_InvalidJSON(t *testing.T) {
	mock := &mockSSMAPI{
		getParameterFunc: func(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
			return &ssm.GetParameterOutput{
				Parameter: &types.Parameter{
					Value: aws.String("invalid json"),
				},
			}, nil
		},
	}

	registrar := &Registrar{
		ssmClient: mock,
		logger:    &mockLogger{},
	}

	_, err := registrar.FetchConfig(context.Background(), "/runs-fleet/test")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestRegistrar_FetchConfig_NilParameter(t *testing.T) {
	mock := &mockSSMAPI{
		getParameterFunc: func(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
			return &ssm.GetParameterOutput{
				Parameter: nil,
			}, nil
		},
	}

	registrar := &Registrar{
		ssmClient: mock,
		logger:    &mockLogger{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := registrar.FetchConfig(ctx, "/runs-fleet/test")
	if err == nil {
		t.Error("expected error for nil parameter")
	}
}

func TestRegistrar_SetRunnerEnvironment(t *testing.T) {
	tmpDir := t.TempDir()

	registrar := &Registrar{
		logger: &mockLogger{},
	}

	cacheURL := "https://cache.example.com"
	cacheToken := "cache-token-123"

	err := registrar.SetRunnerEnvironment(tmpDir, cacheURL, cacheToken)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	envFile := filepath.Join(tmpDir, ".env")
	content, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("failed to read .env file: %v", err)
	}

	contentStr := string(content)
	if !contains(contentStr, "RUNNER_ALLOW_RUNASROOT=1") {
		t.Error("expected RUNNER_ALLOW_RUNASROOT in .env")
	}
	if !contains(contentStr, "ACTIONS_CACHE_URL=https://cache.example.com") {
		t.Error("expected ACTIONS_CACHE_URL in .env")
	}
	if !contains(contentStr, "ACTIONS_CACHE_TOKEN=cache-token-123") {
		t.Error("expected ACTIONS_CACHE_TOKEN in .env")
	}
}

func TestRegistrar_SetRunnerEnvironment_NoCache(t *testing.T) {
	tmpDir := t.TempDir()

	registrar := &Registrar{
		logger: &mockLogger{},
	}

	err := registrar.SetRunnerEnvironment(tmpDir, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	envFile := filepath.Join(tmpDir, ".env")
	content, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("failed to read .env file: %v", err)
	}

	contentStr := string(content)
	if contains(contentStr, "ACTIONS_CACHE_URL") {
		t.Error("should not contain ACTIONS_CACHE_URL when empty")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestRegistrar_RegisterRunner_ConfigNotFound(t *testing.T) {
	tmpDir := t.TempDir()

	registrar := &Registrar{
		logger: &mockLogger{},
	}

	config := &RunnerConfig{
		Org:      "test-org",
		JITToken: "token",
		IsOrg:    true,
	}

	err := registrar.RegisterRunner(context.Background(), config, tmpDir)
	if err == nil {
		t.Error("expected error when config.sh not found")
	}
}

func TestRegistrar_RegisterRunner_OrgRequired(t *testing.T) {
	tmpDir := t.TempDir()
	configScript := filepath.Join(tmpDir, "config.sh")
	if err := os.WriteFile(configScript, []byte("#!/bin/bash\n"), 0755); err != nil {
		t.Fatalf("failed to create config.sh: %v", err)
	}

	registrar := &Registrar{
		logger: &mockLogger{},
	}

	config := &RunnerConfig{
		Org:      "",
		JITToken: "token",
		IsOrg:    true,
	}

	err := registrar.RegisterRunner(context.Background(), config, tmpDir)
	if err == nil {
		t.Error("expected error when org is required but empty")
	}
}

func TestRegistrar_RegisterRunner_RepoRequired(t *testing.T) {
	tmpDir := t.TempDir()
	configScript := filepath.Join(tmpDir, "config.sh")
	if err := os.WriteFile(configScript, []byte("#!/bin/bash\n"), 0755); err != nil {
		t.Fatalf("failed to create config.sh: %v", err)
	}

	registrar := &Registrar{
		logger: &mockLogger{},
	}

	config := &RunnerConfig{
		Repo:     "",
		JITToken: "token",
		IsOrg:    false,
	}

	err := registrar.RegisterRunner(context.Background(), config, tmpDir)
	if err == nil {
		t.Error("expected error when repo is required but empty")
	}
}
