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
				"RUNS_FLEET_QUEUE_URL":              "https://sqs.us-east-1.amazonaws.com/123/queue",
				"RUNS_FLEET_VPC_ID":                 "vpc-123",
				"RUNS_FLEET_PUBLIC_SUBNET_IDS":      "subnet-1,subnet-2",
				"RUNS_FLEET_GITHUB_WEBHOOK_SECRET":  "secret",
				"RUNS_FLEET_GITHUB_APP_ID":          "123456",
				"RUNS_FLEET_GITHUB_APP_PRIVATE_KEY": "test-key",
				"RUNS_FLEET_SECURITY_GROUP_ID":      "sg-123",
				"RUNS_FLEET_INSTANCE_PROFILE_ARN":   "arn:aws:iam::123456789:instance-profile/test",
			},
			wantErr: false,
		},
		{
			name: "Missing Queue URL",
			env: map[string]string{
				"RUNS_FLEET_VPC_ID":                 "vpc-123",
				"RUNS_FLEET_PUBLIC_SUBNET_IDS":      "subnet-1,subnet-2",
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
				"RUNS_FLEET_PUBLIC_SUBNET_IDS":      "subnet-1,subnet-2",
				"RUNS_FLEET_GITHUB_WEBHOOK_SECRET":  "secret",
				"RUNS_FLEET_GITHUB_APP_ID":          "123456",
				"RUNS_FLEET_GITHUB_APP_PRIVATE_KEY": "test-key",
				"RUNS_FLEET_SECURITY_GROUP_ID":      "sg-123",
				"RUNS_FLEET_INSTANCE_PROFILE_ARN":   "arn:aws:iam::123456789:instance-profile/test",
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

func TestIsEC2Backend(t *testing.T) {
	tests := []struct {
		backend string
		want    bool
	}{
		{"", true},
		{"ec2", true},
		{"k8s", false},
	}
	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			cfg := &Config{DefaultBackend: tt.backend}
			if got := cfg.IsEC2Backend(); got != tt.want {
				t.Errorf("IsEC2Backend() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsK8sBackend(t *testing.T) {
	tests := []struct {
		backend string
		want    bool
	}{
		{"", false},
		{"ec2", false},
		{"k8s", true},
	}
	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			cfg := &Config{DefaultBackend: tt.backend}
			if got := cfg.IsK8sBackend(); got != tt.want {
				t.Errorf("IsK8sBackend() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateK8sConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name: "valid K8s config",
			cfg: &Config{
				KubeNamespace:   "runs-fleet",
				KubeRunnerImage: "runner:latest",
			},
			wantErr: false,
		},
		{
			name: "missing namespace",
			cfg: &Config{
				KubeNamespace:   "",
				KubeRunnerImage: "runner:latest",
			},
			wantErr: true,
		},
		{
			name: "missing runner image",
			cfg: &Config{
				KubeNamespace:   "runs-fleet",
				KubeRunnerImage: "",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validateK8sConfig()
			if (err != nil) != tt.wantErr {
				t.Errorf("validateK8sConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseNodeSelector(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr bool
	}{
		{
			name:  "empty",
			input: "",
			want:  map[string]string{},
		},
		{
			name:  "single pair",
			input: "kubernetes.io/arch=arm64",
			want:  map[string]string{"kubernetes.io/arch": "arm64"},
		},
		{
			name:  "multiple pairs",
			input: "kubernetes.io/arch=arm64,node.kubernetes.io/instance-type=c7g.xlarge",
			want: map[string]string{
				"kubernetes.io/arch":                "arm64",
				"node.kubernetes.io/instance-type": "c7g.xlarge",
			},
		},
		{
			name:  "with spaces",
			input: " key1=value1 , key2=value2 ",
			want:  map[string]string{"key1": "value1", "key2": "value2"},
		},
		{
			name:    "invalid value too long",
			input:   "key=" + string(make([]byte, 64)),
			wantErr: true,
		},
		{
			name:    "invalid value starts with dash",
			input:   "key=-value",
			wantErr: true,
		},
		{
			name:    "missing equals",
			input:   "key-without-equals",
			wantErr: true,
		},
		{
			name:    "empty key",
			input:   "=value",
			wantErr: true,
		},
		{
			name:    "invalid key too long",
			input:   string(make([]byte, 64)) + "=value",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNodeSelector(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseNodeSelector() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("parseNodeSelector() length = %v, want %v", len(got), len(tt.want))
				return
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("parseNodeSelector()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestValidateK8sLabelKey(t *testing.T) {
	tests := []struct {
		key     string
		wantErr bool
	}{
		{"valid", false},
		{"valid-key", false},
		{"valid_key", false},
		{"valid.key", false},
		{"kubernetes.io/arch", false},
		{"node.kubernetes.io/instance-type", false},
		{"example.com/key", false},
		{"", true},
		{"-invalid", true},
		{"invalid-", true},
		{"kubernetes.io/", true},
		{"/name", true},
		{string(make([]byte, 64)), true},
		// Prefix validation edge cases
		{"Kubernetes.io/arch", true},  // uppercase in prefix
		{"-example.com/key", true},    // prefix starts with dash
		{"example.com-/key", true},    // prefix ends with dash
		{"example.Com/key", true},     // uppercase in prefix middle
		{"9example.com/key", false},   // prefix starts with number (valid)
		{"example.com9/key", false},   // prefix ends with number (valid)
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			err := validateK8sLabelKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateK8sLabelKey(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			}
		})
	}
}

func TestIsValidK8sLabelValue(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", true},
		{"valid", true},
		{"valid-value", true},
		{"valid_value", true},
		{"valid.value", true},
		{"Valid123", true},
		{"-invalid", false},
		{"invalid-", false},
		{".invalid", false},
		{"invalid.", false},
		{"in valid", false},
		{string(make([]byte, 64)), false},
		{"value\u00e9", false}, // non-ASCII character
		{"v\u4e2d\u6587", false}, // Chinese characters
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := isValidK8sLabelValue(tt.value); got != tt.want {
				t.Errorf("isValidK8sLabelValue(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestValidateInvalidBackend(t *testing.T) {
	cfg := &Config{
		DefaultBackend:      "invalid",
		GitHubWebhookSecret: "secret",
		GitHubAppID:         "123",
		GitHubAppPrivateKey: "key",
		QueueURL:            "https://sqs.example.com",
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() should return error for invalid backend")
	}
}
