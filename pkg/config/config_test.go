package config

import (
	"os"
	"testing"
)

func TestLoad(t *testing.T) {
	originalEnv := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, e := range originalEnv {
			pair := splitEnv(e)
			_ = os.Setenv(pair[0], pair[1])
		}
	})

	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{
			name: "Valid Config",
			env: map[string]string{
				"RUNS_FLEET_GITHUB_ORG":              "test-org",
				"RUNS_FLEET_QUEUE_URL":               "https://sqs.us-east-1.amazonaws.com/123/queue",
				"RUNS_FLEET_VPC_ID":                  "vpc-123",
				"RUNS_FLEET_PUBLIC_SUBNET_IDS":       "subnet-1,subnet-2",
				"RUNS_FLEET_GITHUB_WEBHOOK_SECRET":   "secret",
				"RUNS_FLEET_GITHUB_APP_ID":           "123456",
				"RUNS_FLEET_GITHUB_APP_PRIVATE_KEY":  "test-key",
				"RUNS_FLEET_SECURITY_GROUP_ID":       "sg-123",
				"RUNS_FLEET_INSTANCE_PROFILE_ARN":    "arn:aws:iam::123456789:instance-profile/test",
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
				_ = os.Setenv(k, v)
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
	_ = os.Setenv("TEST_INT", "123")
	_ = os.Setenv("TEST_BAD_INT", "abc")
	t.Cleanup(func() {
		_ = os.Unsetenv("TEST_INT")
		_ = os.Unsetenv("TEST_BAD_INT")
	})

	val, err := getEnvInt("TEST_INT", 0)
	if err != nil {
		t.Errorf("getEnvInt(TEST_INT) unexpected error: %v", err)
	}
	if val != 123 {
		t.Errorf("getEnvInt(TEST_INT) = %d, want 123", val)
	}

	val, err = getEnvInt("TEST_BAD_INT", 456)
	if err == nil {
		t.Errorf("getEnvInt(TEST_BAD_INT) expected error for invalid integer, got nil")
	}
	if val != 0 {
		t.Errorf("getEnvInt(TEST_BAD_INT) = %d, want 0 on error", val)
	}

	val, err = getEnvInt("TEST_MISSING", 789)
	if err != nil {
		t.Errorf("getEnvInt(TEST_MISSING) unexpected error: %v", err)
	}
	if val != 789 {
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
