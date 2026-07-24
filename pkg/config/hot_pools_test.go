package config

import (
	"os"
	"strings"
	"testing"
)

func TestParseHotPools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    map[string]HotPoolSpec
		wantErr string
	}{
		{
			name:  "empty input yields nil map",
			input: "",
			want:  nil,
		},
		{
			name:  "whitespace only yields nil map",
			input: "   \n\t ",
			want:  nil,
		},
		{
			name:  "single pool with explicit fields",
			input: `{"portal-api":{"lingerMinutes":15,"maxHot":2}}`,
			want:  map[string]HotPoolSpec{"portal-api": {LingerMinutes: 15, MaxHot: 2}},
		},
		{
			name:  "maxHot defaults to 1 when omitted",
			input: `{"portal-api":{"lingerMinutes":10}}`,
			want:  map[string]HotPoolSpec{"portal-api": {LingerMinutes: 10, MaxHot: 1}},
		},
		{
			name:  "maxHot defaults to 1 when zero",
			input: `{"portal-api":{"lingerMinutes":10,"maxHot":0}}`,
			want:  map[string]HotPoolSpec{"portal-api": {LingerMinutes: 10, MaxHot: 1}},
		},
		{
			name:  "multiple pools",
			input: `{"a":{"lingerMinutes":5,"maxHot":1},"b":{"lingerMinutes":120,"maxHot":5}}`,
			want: map[string]HotPoolSpec{
				"a": {LingerMinutes: 5, MaxHot: 1},
				"b": {LingerMinutes: 120, MaxHot: 5},
			},
		},
		{
			name:    "unknown field rejected",
			input:   `{"portal-api":{"lingerMinutes":10,"bogus":true}}`,
			wantErr: "unknown field",
		},
		{
			name:    "malformed JSON rejected",
			input:   `{"portal-api":`,
			wantErr: "invalid hot pools JSON",
		},
		{
			name:    "lingerMinutes below 1 rejected",
			input:   `{"portal-api":{"lingerMinutes":0}}`,
			wantErr: "lingerMinutes",
		},
		{
			name:    "lingerMinutes above 120 rejected",
			input:   `{"portal-api":{"lingerMinutes":121}}`,
			wantErr: "lingerMinutes",
		},
		{
			name:    "maxHot above 5 rejected",
			input:   `{"portal-api":{"lingerMinutes":10,"maxHot":6}}`,
			wantErr: "maxHot",
		},
		{
			name:    "maxHot below 0 rejected",
			input:   `{"portal-api":{"lingerMinutes":10,"maxHot":-1}}`,
			wantErr: "maxHot",
		},
		{
			name:    "empty pool name rejected",
			input:   `{"":{"lingerMinutes":10}}`,
			wantErr: "pool name",
		},
		{
			name:    "invalid pool name rejected",
			input:   `{"bad name!":{"lingerMinutes":10}}`,
			wantErr: "pool name",
		},
		{
			name:    "pool name over 63 chars rejected",
			input:   `{"` + strings.Repeat("a", 64) + `":{"lingerMinutes":10}}`,
			wantErr: "pool name",
		},
		{
			name:  "pool name exactly 63 chars accepted",
			input: `{"` + strings.Repeat("a", 63) + `":{"lingerMinutes":10}}`,
			want:  map[string]HotPoolSpec{strings.Repeat("a", 63): {LingerMinutes: 10, MaxHot: 1}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseHotPools(tt.input)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseHotPools(%q) = nil error, want error containing %q", tt.input, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseHotPools(%q) error = %q, want containing %q", tt.input, err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseHotPools(%q) unexpected error: %v", tt.input, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ParseHotPools(%q) = %v (len %d), want %v (len %d)", tt.input, got, len(got), tt.want, len(tt.want))
			}
			for name, wantSpec := range tt.want {
				gotSpec, ok := got[name]
				if !ok {
					t.Fatalf("ParseHotPools(%q) missing pool %q", tt.input, name)
				}
				if gotSpec != wantSpec {
					t.Errorf("ParseHotPools(%q) pool %q = %+v, want %+v", tt.input, name, gotSpec, wantSpec)
				}
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

	t.Run("unset yields nil HotPools", func(t *testing.T) {
		setBase()
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if cfg.HotPools != nil {
			t.Errorf("HotPools = %v, want nil when unset", cfg.HotPools)
		}
	})

	t.Run("valid JSON parses into HotPools", func(t *testing.T) {
		setBase()
		_ = os.Setenv("RUNS_FLEET_HOT_POOLS", `{"portal-api":{"lingerMinutes":15,"maxHot":2}}`)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		spec, ok := cfg.HotPools["portal-api"]
		if !ok {
			t.Fatalf("HotPools missing portal-api: %v", cfg.HotPools)
		}
		if spec.LingerMinutes != 15 || spec.MaxHot != 2 {
			t.Errorf("HotPools[portal-api] = %+v, want {LingerMinutes:15 MaxHot:2}", spec)
		}
	})

	t.Run("invalid JSON fails Load fast", func(t *testing.T) {
		setBase()
		_ = os.Setenv("RUNS_FLEET_HOT_POOLS", `{"portal-api":{"lingerMinutes":999}}`)
		if _, err := Load(); err == nil {
			t.Fatal("Load() = nil error, want failure on out-of-range lingerMinutes")
		}
	})
}
