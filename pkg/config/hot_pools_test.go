package config

import (
	"os"
	"strings"
	"testing"
)

func TestParseHotPoolCaps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    HotPoolCaps
		wantErr string
	}{
		{
			name:  "empty input yields all defaults",
			input: "",
			want:  DefaultHotPoolCaps(),
		},
		{
			name:  "whitespace only yields all defaults",
			input: "   \n\t ",
			want:  DefaultHotPoolCaps(),
		},
		{
			name:  "explicit full object",
			input: `{"maxLingerMinutes":20,"maxHot":2,"minJobsToActivate":10,"lookbackDays":14,"burstGapMinutes":30}`,
			want:  HotPoolCaps{MaxLingerMinutes: 20, MaxHot: 2, MinJobsToActivate: 10, LookbackDays: 14, BurstGapMinutes: 30},
		},
		{
			name:  "omitted fields take defaults",
			input: `{"maxLingerMinutes":45}`,
			want:  HotPoolCaps{MaxLingerMinutes: 45, MaxHot: 3, MinJobsToActivate: 20, LookbackDays: 7, BurstGapMinutes: 20},
		},
		{
			name:  "zero field takes default",
			input: `{"maxHot":0,"maxLingerMinutes":10}`,
			want:  HotPoolCaps{MaxLingerMinutes: 10, MaxHot: 3, MinJobsToActivate: 20, LookbackDays: 7, BurstGapMinutes: 20},
		},
		{
			name:    "unknown field rejected",
			input:   `{"bogus":true}`,
			wantErr: "invalid hot pool caps JSON",
		},
		{
			name:    "malformed JSON rejected",
			input:   `{"maxHot":`,
			wantErr: "invalid hot pool caps JSON",
		},
		{
			name:    "negative rejected",
			input:   `{"maxHot":-1}`,
			wantErr: "non-negative",
		},
		{
			name:    "maxLingerMinutes above ceiling rejected",
			input:   `{"maxLingerMinutes":121}`,
			wantErr: "maxLingerMinutes",
		},
		{
			name:    "maxHot above ceiling rejected",
			input:   `{"maxHot":11}`,
			wantErr: "maxHot",
		},
		{
			name:    "lookbackDays above ceiling rejected",
			input:   `{"lookbackDays":91}`,
			wantErr: "lookbackDays",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseHotPoolCaps(tt.input)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseHotPoolCaps(%q) = nil error, want %q", tt.input, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseHotPoolCaps(%q) error = %q, want containing %q", tt.input, err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseHotPoolCaps(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseHotPoolCaps(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestLoadHotPools(t *testing.T) {
	originalEnv := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, e := range originalEnv {
			pair := splitEnv(e)
			_ = os.Setenv(pair[0], pair[1])
		}
	})

	baseEnv := map[string]string{
		"RUNS_FLEET_QUEUE_URL":              "https://sqs.us-east-1.amazonaws.com/123/queue",
		"RUNS_FLEET_VPC_ID":                 "vpc-123",
		"RUNS_FLEET_SUBNET_IDS":             "subnet-1,subnet-2",
		"RUNS_FLEET_GITHUB_WEBHOOK_SECRET":  "secret",
		"RUNS_FLEET_GITHUB_APP_ID":          "123456",
		"RUNS_FLEET_GITHUB_APP_PRIVATE_KEY": "test-key",
		"RUNS_FLEET_SECURITY_GROUP_ID":      "sg-123",
		"RUNS_FLEET_INSTANCE_PROFILE_ARN":   "arn:aws:iam::123456789:instance-profile/test",
		"RUNS_FLEET_RUNNER_IMAGE":           "123456789012.dkr.ecr.us-east-1.amazonaws.com/runs-fleet-runner:latest",
		"RUNS_FLEET_BASE_URL":               "https://runs-fleet.example.com",
	}

	setBase := func() {
		os.Clearenv()
		for k, v := range baseEnv {
			_ = os.Setenv(k, v)
		}
	}

	t.Run("unset yields disabled with default caps", func(t *testing.T) {
		setBase()
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if cfg.HotPoolsEnabled {
			t.Error("HotPoolsEnabled = true, want false when unset")
		}
		if cfg.HotPoolCaps != DefaultHotPoolCaps() {
			t.Errorf("HotPoolCaps = %+v, want defaults", cfg.HotPoolCaps)
		}
	})

	t.Run("enabled toggle parses", func(t *testing.T) {
		setBase()
		_ = os.Setenv("RUNS_FLEET_HOT_POOLS_ENABLED", "true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if !cfg.HotPoolsEnabled {
			t.Error("HotPoolsEnabled = false, want true")
		}
	})

	t.Run("caps JSON parses", func(t *testing.T) {
		setBase()
		_ = os.Setenv("RUNS_FLEET_HOT_POOL_CAPS", `{"maxLingerMinutes":20,"maxHot":2}`)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if cfg.HotPoolCaps.MaxLingerMinutes != 20 || cfg.HotPoolCaps.MaxHot != 2 {
			t.Errorf("HotPoolCaps = %+v, want maxLinger 20 maxHot 2", cfg.HotPoolCaps)
		}
	})

	t.Run("invalid caps JSON fails Load fast", func(t *testing.T) {
		setBase()
		_ = os.Setenv("RUNS_FLEET_HOT_POOL_CAPS", `{"maxHot":999}`)
		if _, err := Load(); err == nil {
			t.Fatal("Load() = nil error, want failure on out-of-range maxHot")
		}
	})
}
