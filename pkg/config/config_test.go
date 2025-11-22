package config

import (
	"os"
	"testing"
)

func TestLoad(t *testing.T) {
	// Save original env vars and restore after test
	originalEnv := os.Environ()
	defer func() {
		os.Clearenv()
		for _, e := range originalEnv {
			pair := splitEnv(e)
			os.Setenv(pair[0], pair[1])
		}
	}()

	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{
			name: "Valid Config",
			env: map[string]string{
				"RUNS_FLEET_GITHUB_ORG":            "test-org",
				"RUNS_FLEET_QUEUE_URL":             "https://sqs.us-east-1.amazonaws.com/123/queue",
				"RUNS_FLEET_VPC_ID":                "vpc-123",
				"RUNS_FLEET_PUBLIC_SUBNET_IDS":     "subnet-1,subnet-2",
				"RUNS_FLEET_GITHUB_WEBHOOK_SECRET": "secret",
			},
			wantErr: false,
		},
		{
			name: "Missing GitHub Org",
			env: map[string]string{
				"RUNS_FLEET_QUEUE_URL": "https://sqs.us-east-1.amazonaws.com/123/queue",
				"RUNS_FLEET_VPC_ID":    "vpc-123",
			},
			wantErr: true,
		},
		{
			name: "Missing Queue URL",
			env: map[string]string{
				"RUNS_FLEET_GITHUB_ORG": "test-org",
				"RUNS_FLEET_VPC_ID":     "vpc-123",
			},
			wantErr: true,
		},
		{
			name: "Missing VPC ID",
			env: map[string]string{
				"RUNS_FLEET_GITHUB_ORG": "test-org",
				"RUNS_FLEET_QUEUE_URL":  "https://sqs.us-east-1.amazonaws.com/123/queue",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Clearenv()
			for k, v := range tt.env {
				os.Setenv(k, v)
			}

			cfg, err := Load()
			if (err != nil) != tt.wantErr {
				t.Errorf("Load() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if cfg.GitHubOrg != tt.env["RUNS_FLEET_GITHUB_ORG"] {
					t.Errorf("GitHubOrg = %v, want %v", cfg.GitHubOrg, tt.env["RUNS_FLEET_GITHUB_ORG"])
				}
				if len(cfg.PublicSubnetIDs) != 2 {
					t.Errorf("PublicSubnetIDs length = %v, want 2", len(cfg.PublicSubnetIDs))
				}
			}
		})
	}
}

func TestGetEnvInt(t *testing.T) {
	os.Setenv("TEST_INT", "123")
	os.Setenv("TEST_BAD_INT", "abc")
	defer os.Unsetenv("TEST_INT")
	defer os.Unsetenv("TEST_BAD_INT")

	if val := getEnvInt("TEST_INT", 0); val != 123 {
		t.Errorf("getEnvInt(TEST_INT) = %d, want 123", val)
	}
	if val := getEnvInt("TEST_BAD_INT", 456); val != 456 {
		t.Errorf("getEnvInt(TEST_BAD_INT) = %d, want 456", val)
	}
	if val := getEnvInt("TEST_MISSING", 789); val != 789 {
		t.Errorf("getEnvInt(TEST_MISSING) = %d, want 789", val)
	}
}

// Helper to split env string "KEY=VALUE"
func splitEnv(s string) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s, ""}
}
