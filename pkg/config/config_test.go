package config

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/Shavakan/runs-fleet/internal/validation"
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
			},
			wantErr: false,
		},
		{
			name: "Missing Queue URL",
			env: map[string]string{
				"RUNS_FLEET_VPC_ID":                 "vpc-123",
				"RUNS_FLEET_SUBNET_IDS":             "subnet-1,subnet-2",
				"RUNS_FLEET_GITHUB_WEBHOOK_SECRET":  "secret",
				"RUNS_FLEET_GITHUB_APP_ID":          "123456",
				"RUNS_FLEET_GITHUB_APP_PRIVATE_KEY": "test-key",
				"RUNS_FLEET_SECURITY_GROUP_ID":      "sg-123",
				"RUNS_FLEET_INSTANCE_PROFILE_ARN":   "arn:aws:iam::123456789:instance-profile/test",
			},
			wantErr: true,
		},
		{
			name: "Missing VPC ID",
			env: map[string]string{
				"RUNS_FLEET_QUEUE_URL":              "https://sqs.us-east-1.amazonaws.com/123/queue",
				"RUNS_FLEET_SUBNET_IDS":             "subnet-1,subnet-2",
				"RUNS_FLEET_GITHUB_WEBHOOK_SECRET":  "secret",
				"RUNS_FLEET_GITHUB_APP_ID":          "123456",
				"RUNS_FLEET_GITHUB_APP_PRIVATE_KEY": "test-key",
				"RUNS_FLEET_SECURITY_GROUP_ID":      "sg-123",
				"RUNS_FLEET_INSTANCE_PROFILE_ARN":   "arn:aws:iam::123456789:instance-profile/test",
			},
			wantErr: true,
		},
		{
			name: "Missing Runner Image",
			env: map[string]string{
				"RUNS_FLEET_QUEUE_URL":              "https://sqs.us-east-1.amazonaws.com/123/queue",
				"RUNS_FLEET_VPC_ID":                 "vpc-123",
				"RUNS_FLEET_SUBNET_IDS":             "subnet-1,subnet-2",
				"RUNS_FLEET_GITHUB_WEBHOOK_SECRET":  "secret",
				"RUNS_FLEET_GITHUB_APP_ID":          "123456",
				"RUNS_FLEET_GITHUB_APP_PRIVATE_KEY": "test-key",
				"RUNS_FLEET_SECURITY_GROUP_ID":      "sg-123",
				"RUNS_FLEET_INSTANCE_PROFILE_ARN":   "arn:aws:iam::123456789:instance-profile/test",
			},
			wantErr: true,
		},
		{
			name: "Invalid Runner Image URL",
			env: map[string]string{
				"RUNS_FLEET_QUEUE_URL":              "https://sqs.us-east-1.amazonaws.com/123/queue",
				"RUNS_FLEET_VPC_ID":                 "vpc-123",
				"RUNS_FLEET_SUBNET_IDS":             "subnet-1,subnet-2",
				"RUNS_FLEET_GITHUB_WEBHOOK_SECRET":  "secret",
				"RUNS_FLEET_GITHUB_APP_ID":          "123456",
				"RUNS_FLEET_GITHUB_APP_PRIVATE_KEY": "test-key",
				"RUNS_FLEET_SECURITY_GROUP_ID":      "sg-123",
				"RUNS_FLEET_INSTANCE_PROFILE_ARN":   "arn:aws:iam::123456789:instance-profile/test",
				"RUNS_FLEET_RUNNER_IMAGE":           "docker.io/library/runner:latest",
			},
			wantErr: true,
		},
		{
			name: "Missing Base URL",
			env: map[string]string{
				"RUNS_FLEET_QUEUE_URL":              "https://sqs.us-east-1.amazonaws.com/123/queue",
				"RUNS_FLEET_VPC_ID":                 "vpc-123",
				"RUNS_FLEET_SUBNET_IDS":             "subnet-1,subnet-2",
				"RUNS_FLEET_GITHUB_WEBHOOK_SECRET":  "secret",
				"RUNS_FLEET_GITHUB_APP_ID":          "123456",
				"RUNS_FLEET_GITHUB_APP_PRIVATE_KEY": "test-key",
				"RUNS_FLEET_SECURITY_GROUP_ID":      "sg-123",
				"RUNS_FLEET_INSTANCE_PROFILE_ARN":   "arn:aws:iam::123456789:instance-profile/test",
				"RUNS_FLEET_RUNNER_IMAGE":           "123456789012.dkr.ecr.us-east-1.amazonaws.com/runs-fleet-runner:latest",
			},
			wantErr: true,
		},
		{
			name: "Non-HTTPS Base URL",
			env: map[string]string{
				"RUNS_FLEET_QUEUE_URL":              "https://sqs.us-east-1.amazonaws.com/123/queue",
				"RUNS_FLEET_VPC_ID":                 "vpc-123",
				"RUNS_FLEET_SUBNET_IDS":             "subnet-1,subnet-2",
				"RUNS_FLEET_GITHUB_WEBHOOK_SECRET":  "secret",
				"RUNS_FLEET_GITHUB_APP_ID":          "123456",
				"RUNS_FLEET_GITHUB_APP_PRIVATE_KEY": "test-key",
				"RUNS_FLEET_SECURITY_GROUP_ID":      "sg-123",
				"RUNS_FLEET_INSTANCE_PROFILE_ARN":   "arn:aws:iam::123456789:instance-profile/test",
				"RUNS_FLEET_RUNNER_IMAGE":           "123456789012.dkr.ecr.us-east-1.amazonaws.com/runs-fleet-runner:latest",
				"RUNS_FLEET_BASE_URL":               "http://runs-fleet.example.com",
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
				if len(cfg.SubnetIDs) != 2 {
					t.Errorf("SubnetIDs length = %v, want 2", len(cfg.SubnetIDs))
				}
			}
		})
	}
}

func TestLoadLabelAliases(t *testing.T) {
	originalEnv := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, e := range originalEnv {
			pair := splitEnv(e)
			_ = os.Setenv(pair[0], pair[1])
		}
	})

	const aliasJSON = `[{"match":"^ci-(\d+)x-(amd64|arm64)$","regex":true,"spec":"cpu=${1}+${1}/arch=${2}"}]`

	os.Clearenv()
	env := map[string]string{
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
		"RUNS_FLEET_LABEL_ALIASES":          aliasJSON,
	}
	for k, v := range env {
		_ = os.Setenv(k, v)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.LabelAliasesJSON != aliasJSON {
		t.Errorf("LabelAliasesJSON = %q, want %q", cfg.LabelAliasesJSON, aliasJSON)
	}
}

func TestLoadCostAttributionTagKeys(t *testing.T) {
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

	tests := []struct {
		name               string
		extraEnv           map[string]string
		wantApplicationKey string
		wantServiceKey     string
	}{
		{
			name:               "defaults when unset",
			extraEnv:           nil,
			wantApplicationKey: "Application",
			wantServiceKey:     "Service",
		},
		{
			name: "custom keys override defaults",
			extraEnv: map[string]string{
				"RUNS_FLEET_TAG_KEY_APPLICATION": "cost:application",
				"RUNS_FLEET_TAG_KEY_SERVICE":     "cost:service",
			},
			wantApplicationKey: "cost:application",
			wantServiceKey:     "cost:service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Clearenv()
			for k, v := range baseEnv {
				_ = os.Setenv(k, v)
			}
			for k, v := range tt.extraEnv {
				_ = os.Setenv(k, v)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.TagKeyApplication != tt.wantApplicationKey {
				t.Errorf("TagKeyApplication = %q, want %q", cfg.TagKeyApplication, tt.wantApplicationKey)
			}
			if cfg.TagKeyService != tt.wantServiceKey {
				t.Errorf("TagKeyService = %q, want %q", cfg.TagKeyService, tt.wantServiceKey)
			}
		})
	}
}

// TestLoadIgnoresMetricsNamespaceEnv asserts the removed RUNS_FLEET_METRICS_NAMESPACE
// knob is no longer read: setting it must not affect config loading, and the
// fixed metric prefixes are owned by the metrics package, not config.
func TestLoadIgnoresMetricsNamespaceEnv(t *testing.T) {
	originalEnv := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, e := range originalEnv {
			pair := splitEnv(e)
			_ = os.Setenv(pair[0], pair[1])
		}
	})

	os.Clearenv()
	for k, v := range map[string]string{
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
		"RUNS_FLEET_METRICS_NAMESPACE":      "teamx",
	} {
		_ = os.Setenv(k, v)
	}

	if _, err := Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
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

// ec2Env returns a baseline of env vars sufficient to satisfy Load's required-field
// validation, optionally merged with caller-provided overrides.
func ec2Env(extra map[string]string) map[string]string {
	env := map[string]string{
		"RUNS_FLEET_QUEUE_URL":              "https://sqs.us-east-1.amazonaws.com/123/queue",
		"RUNS_FLEET_VPC_ID":                 "vpc-123",
		"RUNS_FLEET_SUBNET_IDS":             "subnet-1,subnet-2",
		"RUNS_FLEET_GITHUB_WEBHOOK_SECRET":  "secret",
		"RUNS_FLEET_GITHUB_APP_ID":          "123",
		"RUNS_FLEET_GITHUB_APP_PRIVATE_KEY": "key",
		"RUNS_FLEET_SECURITY_GROUP_ID":      "sg-123",
		"RUNS_FLEET_INSTANCE_PROFILE_ARN":   "arn:aws:iam::123456789:instance-profile/test",
		"RUNS_FLEET_RUNNER_IMAGE":           "123456789012.dkr.ecr.us-east-1.amazonaws.com/runs-fleet-runner:latest",
		"RUNS_FLEET_BASE_URL":               "https://runs-fleet.example.com",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func TestGetEnvBool(t *testing.T) {
	tests := []struct {
		name         string
		envKey       string
		envValue     string
		defaultValue bool
		want         bool
	}{
		{"empty returns default true", "TEST_BOOL_EMPTY", "", true, true},
		{"empty returns default false", "TEST_BOOL_EMPTY2", "", false, false},
		{"true string", "TEST_BOOL_TRUE", "true", false, true},
		{"false string", "TEST_BOOL_FALSE", "false", true, false},
		{"1 is true", "TEST_BOOL_ONE", "1", false, true},
		{"0 is false", "TEST_BOOL_ZERO", "0", true, false},
		{"TRUE uppercase", "TEST_BOOL_UPPER", "TRUE", false, true},
		{"FALSE uppercase", "TEST_BOOL_UPPER_F", "FALSE", true, false},
		{"invalid returns default", "TEST_BOOL_INVALID", "invalid", true, true},
		{"yes is invalid", "TEST_BOOL_YES", "yes", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				t.Setenv(tt.envKey, tt.envValue)
			}
			if got := getEnvBool(tt.envKey, tt.defaultValue); got != tt.want {
				t.Errorf("getEnvBool(%q, %v) = %v, want %v", tt.envKey, tt.defaultValue, got, tt.want)
			}
		})
	}
}

func TestGetEnvFloat(t *testing.T) {
	tests := []struct {
		name         string
		envKey       string
		envValue     string
		defaultValue float64
		want         float64
	}{
		{"empty returns default", "TEST_FLOAT_EMPTY", "", 1.0, 1.0},
		{"valid float", "TEST_FLOAT_VALID", "0.5", 1.0, 0.5},
		{"valid integer as float", "TEST_FLOAT_INT", "2", 1.0, 2.0},
		{"zero", "TEST_FLOAT_ZERO", "0", 1.0, 0.0},
		{"negative", "TEST_FLOAT_NEG", "-0.5", 1.0, -0.5},
		{"invalid returns default", "TEST_FLOAT_INVALID", "invalid", 0.75, 0.75},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				t.Setenv(tt.envKey, tt.envValue)
			}
			if got := getEnvFloat(tt.envKey, tt.defaultValue); got != tt.want {
				t.Errorf("getEnvFloat(%q, %v) = %v, want %v", tt.envKey, tt.defaultValue, got, tt.want)
			}
		})
	}
}

func TestGetEnvIntDefault(t *testing.T) {
	tests := []struct {
		name         string
		envKey       string
		envValue     string
		defaultValue int
		want         int
	}{
		{"empty returns default", "TEST_INT_DEF_EMPTY", "", 100, 100},
		{"valid int", "TEST_INT_DEF_VALID", "50", 100, 50},
		{"zero", "TEST_INT_DEF_ZERO", "0", 100, 0},
		{"negative", "TEST_INT_DEF_NEG", "-10", 100, -10},
		{"invalid returns default", "TEST_INT_DEF_INVALID", "invalid", 200, 200},
		{"float parses integer part", "TEST_INT_DEF_FLOAT", "1.5", 300, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				t.Setenv(tt.envKey, tt.envValue)
			}
			if got := getEnvIntDefault(tt.envKey, tt.defaultValue); got != tt.want {
				t.Errorf("getEnvIntDefault(%q, %v) = %v, want %v", tt.envKey, tt.defaultValue, got, tt.want)
			}
		})
	}
}

func TestSplitAndFilter(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", []string{}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b , c ", []string{"a", "b", "c"}},
		{"single", []string{"single"}},
		{"a,,b", []string{"a", "b"}},
		{" , , ", []string{}},
		{"subnet-1,subnet-2,subnet-3", []string{"subnet-1", "subnet-2", "subnet-3"}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := splitAndFilter(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("splitAndFilter(%q) length = %v, want %v", tt.input, len(got), len(tt.want))
				return
			}
			for i, w := range tt.want {
				if got[i] != w {
					t.Errorf("splitAndFilter(%q)[%d] = %q, want %q", tt.input, i, got[i], w)
				}
			}
		})
	}
}

func TestParseTags(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr bool
	}{
		{
			name:  "empty string",
			input: "",
			want:  nil,
		},
		{
			name:  "single tag",
			input: `{"Team":"platform"}`,
			want:  map[string]string{"Team": "platform"},
		},
		{
			name:  "multiple tags",
			input: `{"Team":"platform","Project":"ci-runners","CostCenter":"engineering"}`,
			want: map[string]string{
				"Team":       "platform",
				"Project":    "ci-runners",
				"CostCenter": "engineering",
			},
		},
		{
			name:  "empty object",
			input: `{}`,
			want:  map[string]string{},
		},
		{
			name:  "tag with spaces in value",
			input: `{"Description":"GitHub Actions Runner"}`,
			want:  map[string]string{"Description": "GitHub Actions Runner"},
		},
		{
			name:    "invalid JSON",
			input:   "not-valid-json",
			wantErr: true,
		},
		{
			name:    "array instead of object",
			input:   `["tag1","tag2"]`,
			wantErr: true,
		},
		{
			name:    "nested object",
			input:   `{"outer":{"inner":"value"}}`,
			wantErr: true,
		},
		{
			name:    "aws prefix rejected",
			input:   `{"aws:createdBy":"user"}`,
			wantErr: true,
		},
		{
			name:    "AWS prefix case insensitive",
			input:   `{"AWS:createdBy":"user"}`,
			wantErr: true,
		},
		{
			name:    "runs-fleet prefix rejected",
			input:   `{"runs-fleet:custom":"value"}`,
			wantErr: true,
		},
		{
			name:    "runs-fleet prefix case insensitive",
			input:   `{"Runs-Fleet:custom":"value"}`,
			wantErr: true,
		},
		{
			name:    "empty key rejected",
			input:   `{"":"value"}`,
			wantErr: true,
		},
	}

	// Generate long strings for boundary tests
	longKey129 := strings.Repeat("k", 129)
	longKey128 := strings.Repeat("k", 128)
	longVal257 := strings.Repeat("v", 257)
	longVal256 := strings.Repeat("v", 256)

	boundaryTests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr bool
	}{
		{
			name:    "key exceeds 128 chars",
			input:   `{"` + longKey129 + `":"value"}`,
			wantErr: true,
		},
		{
			name:  "key at 128 chars allowed",
			input: `{"` + longKey128 + `":"value"}`,
			want:  map[string]string{longKey128: "value"},
		},
		{
			name:    "value exceeds 256 chars",
			input:   `{"key":"` + longVal257 + `"}`,
			wantErr: true,
		},
		{
			name:  "value at 256 chars allowed",
			input: `{"key":"` + longVal256 + `"}`,
			want:  map[string]string{"key": longVal256},
		},
	}
	tests = append(tests, boundaryTests...)

	// Test 35 custom tag limit (AWS 50 - 15 reserved for system tags)
	t.Run("tag count limit", func(t *testing.T) {
		// 35 tags should pass
		tags35 := make(map[string]string, 35)
		for i := 0; i < 35; i++ {
			tags35[strings.Repeat("a", i+1)] = "v"
		}
		json35 := "{"
		first := true
		for k, v := range tags35 {
			if !first {
				json35 += ","
			}
			json35 += `"` + k + `":"` + v + `"`
			first = false
		}
		json35 += "}"
		got, err := parseTags(json35)
		if err != nil {
			t.Errorf("35 tags should be allowed, got error: %v", err)
		}
		if len(got) != 35 {
			t.Errorf("expected 35 tags, got %d", len(got))
		}

		// 36 tags should fail
		tags36 := make(map[string]string, 36)
		for i := 0; i < 36; i++ {
			tags36[strings.Repeat("b", i+1)] = "v"
		}
		json36 := "{"
		first = true
		for k, v := range tags36 {
			if !first {
				json36 += ","
			}
			json36 += `"` + k + `":"` + v + `"`
			first = false
		}
		json36 += "}"
		_, err = parseTags(json36)
		if err == nil {
			t.Error("36 tags should be rejected (exceeds 35 custom tag limit)")
		}
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTags(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseTags() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if tt.want == nil && got != nil {
				t.Errorf("parseTags() = %v, want nil", got)
				return
			}
			if tt.want != nil && got == nil {
				t.Errorf("parseTags() = nil, want %v", tt.want)
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("parseTags() length = %v, want %v", len(got), len(tt.want))
				return
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("parseTags()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestValidateBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "valid https", url: "https://runs-fleet.example.com", wantErr: false},
		{name: "valid https with port and path", url: "https://runs-fleet.example.com:8443/cache", wantErr: false},
		{name: "empty", url: "", wantErr: true},
		{name: "non-https scheme", url: "http://runs-fleet.example.com", wantErr: true},
		{name: "no scheme", url: "runs-fleet.example.com", wantErr: true},
		{name: "port-only host", url: "https://:443/path", wantErr: true},
		{name: "scheme only", url: "https://", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBaseURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateBaseURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestValidateHostPort(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"valid IPv4", "127.0.0.1:8125", false},
		{"valid hostname", "localhost:8125", false},
		{"valid hostname with domain", "datadog.example.com:8125", false},
		{"valid IPv6", "[::1]:8125", false},
		{"valid port 1", "localhost:1", false},
		{"valid port 65535", "localhost:65535", false},
		{"valid hostname uppercase", "LOCALHOST:8125", false},
		{"empty address", "", true},
		{"missing port", "localhost", true},
		{"empty host", ":8125", true},
		{"port zero", "localhost:0", true},
		{"port negative", "localhost:-1", true},
		{"port too high", "localhost:65536", true},
		{"port non-numeric", "localhost:abc", true},
		{"invalid format", "not-valid", true},
		{"multiple colons without brackets", "::1:8125", true},
		{"malformed IPv6 missing bracket", "[::1:8125", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHostPort(tt.addr)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateHostPort(%q) error = %v, wantErr %v", tt.addr, err, tt.wantErr)
			}
		})
	}
}

func TestValidateMetricsConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name: "datadog disabled",
			cfg: &Config{
				MetricsDatadogEnabled: false,
			},
			wantErr: false,
		},
		{
			name: "datadog enabled with valid address",
			cfg: &Config{
				MetricsDatadogEnabled: true,
				MetricsDatadogAddr:    "127.0.0.1:8125",
			},
			wantErr: false,
		},
		{
			name: "datadog enabled missing address",
			cfg: &Config{
				MetricsDatadogEnabled: true,
				MetricsDatadogAddr:    "",
			},
			wantErr: true,
		},
		{
			name: "datadog enabled invalid address",
			cfg: &Config{
				MetricsDatadogEnabled: true,
				MetricsDatadogAddr:    "invalid-address",
			},
			wantErr: true,
		},
		{
			name: "datadog enabled invalid port",
			cfg: &Config{
				MetricsDatadogEnabled: true,
				MetricsDatadogAddr:    "localhost:99999",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validateMetricsConfig()
			if (err != nil) != tt.wantErr {
				t.Errorf("validateMetricsConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestVaultAuthConfigLoading(t *testing.T) {
	originalEnv := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, e := range originalEnv {
			pair := splitEnv(e)
			_ = os.Setenv(pair[0], pair[1])
		}
	})

	tests := []struct {
		name                string
		env                 map[string]string
		wantVaultAuthMethod string
		wantVaultK8sRole    string
		wantVaultK8sJWTPath string
	}{
		{
			name:                "defaults when not set",
			env:                 ec2Env(nil),
			wantVaultAuthMethod: "aws",
			wantVaultK8sRole:    "",
			wantVaultK8sJWTPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		},
		{
			name: "kubernetes auth method",
			env: ec2Env(map[string]string{
				"VAULT_AUTH_METHOD":  "kubernetes",
				"VAULT_K8S_ROLE":     "runs-fleet",
				"VAULT_K8S_JWT_PATH": "/var/run/secrets/custom/token",
			}),
			wantVaultAuthMethod: "kubernetes",
			wantVaultK8sRole:    "runs-fleet",
			wantVaultK8sJWTPath: "/var/run/secrets/custom/token",
		},
		{
			name: "k8s auth method alias",
			env: ec2Env(map[string]string{
				"VAULT_AUTH_METHOD": "k8s",
				"VAULT_K8S_ROLE":    "my-role",
			}),
			wantVaultAuthMethod: "k8s",
			wantVaultK8sRole:    "my-role",
			wantVaultK8sJWTPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		},
		{
			name: "token auth method",
			env: ec2Env(map[string]string{
				"VAULT_AUTH_METHOD": "token",
			}),
			wantVaultAuthMethod: "token",
			wantVaultK8sRole:    "",
			wantVaultK8sJWTPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Clearenv()
			for k, v := range tt.env {
				_ = os.Setenv(k, v)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}

			if cfg.VaultAuthMethod != tt.wantVaultAuthMethod {
				t.Errorf("VaultAuthMethod = %q, want %q", cfg.VaultAuthMethod, tt.wantVaultAuthMethod)
			}
			if cfg.VaultK8sRole != tt.wantVaultK8sRole {
				t.Errorf("VaultK8sRole = %q, want %q", cfg.VaultK8sRole, tt.wantVaultK8sRole)
			}
			if cfg.VaultK8sJWTPath != tt.wantVaultK8sJWTPath {
				t.Errorf("VaultK8sJWTPath = %q, want %q", cfg.VaultK8sJWTPath, tt.wantVaultK8sJWTPath)
			}
		})
	}
}

func TestValidateVaultAuthConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid aws auth",
			cfg: &Config{
				SecretsBackend:  "vault",
				VaultAddr:       "https://vault.example.com",
				VaultAuthMethod: "aws",
			},
			wantErr: false,
		},
		{
			name: "valid kubernetes auth",
			cfg: &Config{
				SecretsBackend:  "vault",
				VaultAddr:       "https://vault.example.com",
				VaultAuthMethod: "kubernetes",
				VaultK8sRole:    "my-role",
				VaultK8sJWTPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
			},
			wantErr: false,
		},
		{
			name: "kubernetes auth missing role",
			cfg: &Config{
				SecretsBackend:  "vault",
				VaultAddr:       "https://vault.example.com",
				VaultAuthMethod: "kubernetes",
				VaultK8sRole:    "",
				VaultK8sJWTPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
			},
			wantErr: true,
			errMsg:  "VAULT_K8S_ROLE is required",
		},
		{
			name: "kubernetes auth path traversal attempt",
			cfg: &Config{
				SecretsBackend:  "vault",
				VaultAddr:       "https://vault.example.com",
				VaultAuthMethod: "kubernetes",
				VaultK8sRole:    "my-role",
				VaultK8sJWTPath: "/etc/passwd",
			},
			wantErr: true,
			errMsg:  "must be under /var/run/secrets/",
		},
		{
			name: "kubernetes auth relative path",
			cfg: &Config{
				SecretsBackend:  "vault",
				VaultAddr:       "https://vault.example.com",
				VaultAuthMethod: "kubernetes",
				VaultK8sRole:    "my-role",
				VaultK8sJWTPath: "relative/path/token",
			},
			wantErr: true,
			errMsg:  "must be an absolute path",
		},
		{
			name: "kubernetes auth path with traversal in allowed dir",
			cfg: &Config{
				SecretsBackend:  "vault",
				VaultAddr:       "https://vault.example.com",
				VaultAuthMethod: "kubernetes",
				VaultK8sRole:    "my-role",
				VaultK8sJWTPath: "/var/run/secrets/../../../etc/passwd",
			},
			wantErr: true,
			errMsg:  "must be under /var/run/secrets/",
		},
		{
			name: "invalid auth method",
			cfg: &Config{
				SecretsBackend:  "vault",
				VaultAddr:       "https://vault.example.com",
				VaultAuthMethod: "invalid-method",
			},
			wantErr: true,
			errMsg:  "VAULT_AUTH_METHOD must be",
		},
		{
			name: "valid token auth",
			cfg: &Config{
				SecretsBackend:  "vault",
				VaultAddr:       "https://vault.example.com",
				VaultAuthMethod: "token",
			},
			wantErr: false,
		},
		{
			name: "ssm backend skips vault validation",
			cfg: &Config{
				SecretsBackend:  "ssm",
				VaultAuthMethod: "invalid-method", // Should not be validated
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validateSecretsConfig()
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSecretsConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("validateSecretsConfig() error = %v, want error containing %q", err, tt.errMsg)
			}
		})
	}
}

func TestValidateVaultK8sJWTPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid default path",
			path:    "/var/run/secrets/kubernetes.io/serviceaccount/token",
			wantErr: false,
		},
		{
			name:    "valid custom path under secrets",
			path:    "/var/run/secrets/custom/token",
			wantErr: false,
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: true,
			errMsg:  "cannot be empty",
		},
		{
			name:    "relative path",
			path:    "var/run/secrets/token",
			wantErr: true,
			errMsg:  "must be an absolute path",
		},
		{
			name:    "path outside allowed directory",
			path:    "/etc/passwd",
			wantErr: true,
			errMsg:  "must be under /var/run/secrets/",
		},
		{
			name:    "path traversal attack",
			path:    "/var/run/secrets/../../../etc/shadow",
			wantErr: true,
			errMsg:  "must be under /var/run/secrets/",
		},
		{
			name:    "path to home directory",
			path:    "/home/user/.ssh/id_rsa",
			wantErr: true,
			errMsg:  "must be under /var/run/secrets/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validation.ValidateK8sJWTPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateK8sJWTPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("ValidateK8sJWTPath(%q) error = %v, want error containing %q", tt.path, err, tt.errMsg)
			}
		})
	}
}

func TestValidateECRImageURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		// Valid URLs
		{"valid with tag", "123456789012.dkr.ecr.us-east-1.amazonaws.com/repo:latest", false},
		{"valid with digest", "123456789012.dkr.ecr.us-east-1.amazonaws.com/repo@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"valid with tag and digest", "123456789012.dkr.ecr.us-east-1.amazonaws.com/repo:v1.0@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"valid nested repo", "123456789012.dkr.ecr.us-west-2.amazonaws.com/org/repo:tag", false},
		{"valid deeply nested", "123456789012.dkr.ecr.eu-west-1.amazonaws.com/org/team/repo:v1.2.3", false},
		{"valid without tag", "123456789012.dkr.ecr.ap-northeast-1.amazonaws.com/repo", false},
		{"valid with underscore in repo", "123456789012.dkr.ecr.us-east-1.amazonaws.com/my_repo:tag", false},
		{"valid with dots in repo", "123456789012.dkr.ecr.us-east-1.amazonaws.com/my.repo:tag", false},

		// Invalid URLs
		{"docker hub", "docker.io/library/runner:latest", true},
		{"missing account id", ".dkr.ecr.us-east-1.amazonaws.com/repo:tag", true},
		{"short account id", "12345.dkr.ecr.us-east-1.amazonaws.com/repo:tag", true},
		{"long account id", "1234567890123.dkr.ecr.us-east-1.amazonaws.com/repo:tag", true},
		{"missing region", "123456789012.dkr.ecr..amazonaws.com/repo:tag", true},
		{"missing repo", "123456789012.dkr.ecr.us-east-1.amazonaws.com/", true},
		{"invalid chars in repo", "123456789012.dkr.ecr.us-east-1.amazonaws.com/REPO:tag", true},
		{"repo starting with dash", "123456789012.dkr.ecr.us-east-1.amazonaws.com/-repo:tag", true},
		{"repo ending with dash", "123456789012.dkr.ecr.us-east-1.amazonaws.com/repo-:tag", true},
		{"malicious url suffix", "evil.dkr.ecr.com.amazonaws.com.attacker.com/repo:tag", true},
		{"valid account with domain suffix", "123456789012.dkr.ecr.us-east-1.amazonaws.com.attacker.com/repo:tag", true},
		{"subdomain attack", "attacker.123456789012.dkr.ecr.us-east-1.amazonaws.com/repo:tag", true},
		{"missing amazonaws.com", "123456789012.dkr.ecr.us-east-1.aws.com/repo:tag", true},
		{"invalid digest length", "123456789012.dkr.ecr.us-east-1.amazonaws.com/repo@sha256:abc", true},
		{"invalid digest chars", "123456789012.dkr.ecr.us-east-1.amazonaws.com/repo@sha256:ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateECRImageURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateECRImageURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	})
	return &buf
}

func TestGetEnvBoolInvalidLogsWarning(t *testing.T) {
	buf := captureSlog(t)

	t.Setenv("TEST_BOOL_INVALID", "notabool")

	got := getEnvBool("TEST_BOOL_INVALID", true)
	if got != true {
		t.Errorf("getEnvBool() = %v, want true", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "invalid boolean env var") {
		t.Errorf("expected warning log, got: %s", logged)
	}
	if !strings.Contains(logged, "TEST_BOOL_INVALID") {
		t.Errorf("expected key in log, got: %s", logged)
	}
}

func TestGetEnvFloatInvalidLogsWarning(t *testing.T) {
	buf := captureSlog(t)

	t.Setenv("TEST_FLOAT_INVALID", "notafloat")

	got := getEnvFloat("TEST_FLOAT_INVALID", 0.5)
	if got != 0.5 {
		t.Errorf("getEnvFloat() = %v, want 0.5", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "invalid float env var") {
		t.Errorf("expected warning log, got: %s", logged)
	}
	if !strings.Contains(logged, "TEST_FLOAT_INVALID") {
		t.Errorf("expected key in log, got: %s", logged)
	}
}

func TestValidateMetricsSampleRateBounds(t *testing.T) {
	tests := []struct {
		name     string
		rate     float64
		wantRate float64
		wantWarn bool
	}{
		{"valid 0.5", 0.5, 0.5, false},
		{"valid 0.0", 0.0, 0.0, false},
		{"valid 1.0", 1.0, 1.0, false},
		{"negative clamped to 0", -0.5, 0.0, true},
		{"above 1 clamped to 1", 2.0, 1.0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureSlog(t)
			cfg := &Config{MetricsDatadogSampleRate: tt.rate}

			_ = cfg.validateMetricsConfig()

			if cfg.MetricsDatadogSampleRate != tt.wantRate {
				t.Errorf("sample rate = %v, want %v", cfg.MetricsDatadogSampleRate, tt.wantRate)
			}
			logged := buf.String()
			hasWarn := strings.Contains(logged, "RUNS_FLEET_METRICS_DATADOG_SAMPLE_RATE")
			if hasWarn != tt.wantWarn {
				t.Errorf("warning logged = %v, want %v (log: %s)", hasWarn, tt.wantWarn, logged)
			}
		})
	}
}

func TestValidateMetricsBufferPoolSizeBounds(t *testing.T) {
	tests := []struct {
		name     string
		size     int
		wantSize int
		wantWarn bool
	}{
		{"valid 0", 0, 0, false},
		{"valid 100", 100, 100, false},
		{"negative clamped to 0", -5, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureSlog(t)
			cfg := &Config{MetricsDatadogBufferPoolSize: tt.size}

			_ = cfg.validateMetricsConfig()

			if cfg.MetricsDatadogBufferPoolSize != tt.wantSize {
				t.Errorf("buffer pool size = %v, want %v", cfg.MetricsDatadogBufferPoolSize, tt.wantSize)
			}
			logged := buf.String()
			hasWarn := strings.Contains(logged, "RUNS_FLEET_METRICS_DATADOG_BUFFER_POOL_SIZE")
			if hasWarn != tt.wantWarn {
				t.Errorf("warning logged = %v, want %v (log: %s)", hasWarn, tt.wantWarn, logged)
			}
		})
	}
}
