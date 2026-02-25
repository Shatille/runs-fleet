package secrets

import (
	"context"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func TestEnvStore_Get(t *testing.T) {
	store := NewEnvStore()
	ctx := context.Background()

	tests := []struct {
		name     string
		envVars  map[string]string
		wantErr  bool
		validate func(*testing.T, *RunnerConfig)
	}{
		{
			name: "valid config with all fields",
			envVars: map[string]string{
				"RUNS_FLEET_JIT_TOKEN":            "test-jit-token",
				"RUNS_FLEET_ORG":                  "test-org",
				"RUNS_FLEET_REPO":                 "test-org/test-repo",
				"RUNS_FLEET_RUN_ID":               "12345",
				"RUNS_FLEET_JOB_ID":               "67890",
				"RUNS_FLEET_CACHE_TOKEN":          "cache-token",
				"RUNS_FLEET_CACHE_URL":            "https://cache.example.com",
				"RUNS_FLEET_TERMINATION_QUEUE_URL": "https://sqs.example.com/queue",
				"RUNS_FLEET_LABELS":               "self-hosted,linux,arm64",
				"RUNS_FLEET_IS_ORG":               "true",
				"RUNS_FLEET_RUNNER_NAME":           "runs-fleet-runner-default",
			},
			wantErr: false,
			validate: func(t *testing.T, cfg *RunnerConfig) {
				if cfg.JITToken != "test-jit-token" {
					t.Errorf("JITToken = %q, want %q", cfg.JITToken, "test-jit-token")
				}
				if cfg.Org != "test-org" {
					t.Errorf("Org = %q, want %q", cfg.Org, "test-org")
				}
				if cfg.Repo != "test-org/test-repo" {
					t.Errorf("Repo = %q, want %q", cfg.Repo, "test-org/test-repo")
				}
				if cfg.RunID != "12345" {
					t.Errorf("RunID = %q, want %q", cfg.RunID, "12345")
				}
				if len(cfg.Labels) != 3 {
					t.Errorf("Labels length = %d, want 3", len(cfg.Labels))
				}
				if !cfg.IsOrg {
					t.Error("IsOrg = false, want true")
				}
				if cfg.RunnerName != "runs-fleet-runner-default" {
					t.Errorf("RunnerName = %q, want %q", cfg.RunnerName, "runs-fleet-runner-default")
				}
			},
		},
		{
			name: "valid config with repo only",
			envVars: map[string]string{
				"RUNS_FLEET_JIT_TOKEN": "test-jit-token",
				"RUNS_FLEET_REPO":      "owner/repo",
			},
			wantErr: false,
			validate: func(t *testing.T, cfg *RunnerConfig) {
				if cfg.JITToken != "test-jit-token" {
					t.Errorf("JITToken = %q, want %q", cfg.JITToken, "test-jit-token")
				}
				if cfg.Repo != "owner/repo" {
					t.Errorf("Repo = %q, want %q", cfg.Repo, "owner/repo")
				}
			},
		},
		{
			name: "valid config with org only",
			envVars: map[string]string{
				"RUNS_FLEET_JIT_TOKEN": "test-jit-token",
				"RUNS_FLEET_ORG":       "test-org",
			},
			wantErr: false,
			validate: func(t *testing.T, cfg *RunnerConfig) {
				if cfg.Org != "test-org" {
					t.Errorf("Org = %q, want %q", cfg.Org, "test-org")
				}
			},
		},
		{
			name:    "missing jit token",
			envVars: map[string]string{},
			wantErr: true,
		},
		{
			name: "missing org and repo",
			envVars: map[string]string{
				"RUNS_FLEET_JIT_TOKEN": "test-jit-token",
			},
			wantErr: true,
		},
		{
			name: "labels with spaces",
			envVars: map[string]string{
				"RUNS_FLEET_JIT_TOKEN": "test-jit-token",
				"RUNS_FLEET_ORG":       "test-org",
				"RUNS_FLEET_LABELS":    " self-hosted , linux , arm64 ",
			},
			wantErr: false,
			validate: func(t *testing.T, cfg *RunnerConfig) {
				if len(cfg.Labels) != 3 {
					t.Errorf("Labels length = %d, want 3", len(cfg.Labels))
				}
				if cfg.Labels[0] != "self-hosted" {
					t.Errorf("Labels[0] = %q, want %q", cfg.Labels[0], "self-hosted")
				}
			},
		},
		{
			name: "is_org as 1",
			envVars: map[string]string{
				"RUNS_FLEET_JIT_TOKEN": "test-jit-token",
				"RUNS_FLEET_ORG":       "test-org",
				"RUNS_FLEET_IS_ORG":    "1",
			},
			wantErr: false,
			validate: func(t *testing.T, cfg *RunnerConfig) {
				if !cfg.IsOrg {
					t.Error("IsOrg = false, want true")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear relevant env vars
			envKeys := []string{
				"RUNS_FLEET_JIT_TOKEN", "RUNS_FLEET_ORG", "RUNS_FLEET_REPO",
				"RUNS_FLEET_RUN_ID", "RUNS_FLEET_JOB_ID", "RUNS_FLEET_CACHE_TOKEN",
				"RUNS_FLEET_CACHE_URL", "RUNS_FLEET_TERMINATION_QUEUE_URL",
				"RUNS_FLEET_LABELS", "RUNS_FLEET_IS_ORG", "RUNS_FLEET_RUNNER_GROUP",
				"RUNS_FLEET_RUNNER_NAME",
			}
			for _, key := range envKeys {
				_ = os.Unsetenv(key)
			}

			// Set test env vars
			for key, value := range tt.envVars {
				_ = os.Setenv(key, value)
			}

			cfg, err := store.Get(ctx, "any-instance-id")
			if (err != nil) != tt.wantErr {
				t.Errorf("Get() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && tt.validate != nil {
				tt.validate(t, cfg)
			}

			// Cleanup
			for key := range tt.envVars {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func TestEnvStore_Put(t *testing.T) {
	store := NewEnvStore()
	err := store.Put(context.Background(), "id", &RunnerConfig{})
	if err == nil {
		t.Error("Put() expected error for read-only store")
	}
}

func TestEnvStore_Delete(t *testing.T) {
	store := NewEnvStore()
	err := store.Delete(context.Background(), "id")
	if err == nil {
		t.Error("Delete() expected error for read-only store")
	}
}

func TestEnvStore_List(t *testing.T) {
	store := NewEnvStore()
	_, err := store.List(context.Background())
	if err == nil {
		t.Error("List() expected error for EnvStore")
	}
}

func TestNewStore_EnvBackend(t *testing.T) {
	cfg := Config{Backend: BackendEnv}
	store, err := NewStore(context.Background(), cfg, aws.Config{})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if _, ok := store.(*EnvStore); !ok {
		t.Errorf("NewStore() returned %T, want *EnvStore", store)
	}
}

func TestConfig_Validate_EnvBackend(t *testing.T) {
	cfg := Config{Backend: BackendEnv}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil", err)
	}
}
