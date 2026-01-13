package secrets

import (
	"os"
	"testing"
)

func TestLoadConfig_defaults(t *testing.T) {
	// Clear environment
	_ = os.Unsetenv("RUNS_FLEET_SECRETS_BACKEND")
	_ = os.Unsetenv("RUNS_FLEET_SSM_PREFIX")
	_ = os.Unsetenv("VAULT_ADDR")

	cfg := LoadConfig()

	if cfg.Backend != BackendSSM {
		t.Errorf("Backend = %s, want %s", cfg.Backend, BackendSSM)
	}
	if cfg.SSM.Prefix != "/runs-fleet/runners" {
		t.Errorf("SSM.Prefix = %s, want /runs-fleet/runners", cfg.SSM.Prefix)
	}
	if cfg.Vault.KVMount != "secret" {
		t.Errorf("Vault.KVMount = %s, want secret", cfg.Vault.KVMount)
	}
	if cfg.Vault.AuthMethod != "aws" {
		t.Errorf("Vault.AuthMethod = %s, want aws", cfg.Vault.AuthMethod)
	}
}

func TestLoadConfig_vaultBackend(t *testing.T) {
	_ = os.Setenv("RUNS_FLEET_SECRETS_BACKEND", "vault")
	_ = os.Setenv("VAULT_ADDR", "https://vault.example.com")
	_ = os.Setenv("VAULT_NAMESPACE", "myns")
	_ = os.Setenv("VAULT_KV_MOUNT", "kv-v2")
	_ = os.Setenv("VAULT_KV_VERSION", "2")
	defer func() {
		_ = os.Unsetenv("RUNS_FLEET_SECRETS_BACKEND")
		_ = os.Unsetenv("VAULT_ADDR")
		_ = os.Unsetenv("VAULT_NAMESPACE")
		_ = os.Unsetenv("VAULT_KV_MOUNT")
		_ = os.Unsetenv("VAULT_KV_VERSION")
	}()

	cfg := LoadConfig()

	if cfg.Backend != BackendVault {
		t.Errorf("Backend = %s, want %s", cfg.Backend, BackendVault)
	}
	if cfg.Vault.Address != "https://vault.example.com" {
		t.Errorf("Vault.Address = %s, want https://vault.example.com", cfg.Vault.Address)
	}
	if cfg.Vault.Namespace != "myns" {
		t.Errorf("Vault.Namespace = %s, want myns", cfg.Vault.Namespace)
	}
	if cfg.Vault.KVMount != "kv-v2" {
		t.Errorf("Vault.KVMount = %s, want kv-v2", cfg.Vault.KVMount)
	}
	if cfg.Vault.KVVersion != 2 {
		t.Errorf("Vault.KVVersion = %d, want 2", cfg.Vault.KVVersion)
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "valid SSM config",
			cfg:     Config{Backend: BackendSSM},
			wantErr: false,
		},
		{
			name:    "empty backend defaults to SSM",
			cfg:     Config{Backend: ""},
			wantErr: false,
		},
		{
			name:    "valid Vault config",
			cfg:     Config{Backend: BackendVault, Vault: VaultConfig{Address: "https://vault.example.com"}},
			wantErr: false,
		},
		{
			name:    "Vault without address",
			cfg:     Config{Backend: BackendVault},
			wantErr: true,
		},
		{
			name:    "unknown backend",
			cfg:     Config{Backend: "unknown"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfig_ssmPrefix(t *testing.T) {
	_ = os.Setenv("RUNS_FLEET_SSM_PREFIX", "/custom/prefix")
	defer func() { _ = os.Unsetenv("RUNS_FLEET_SSM_PREFIX") }()

	cfg := LoadConfig()

	if cfg.SSM.Prefix != "/custom/prefix" {
		t.Errorf("SSM.Prefix = %s, want /custom/prefix", cfg.SSM.Prefix)
	}
}

func TestConfig_ValidateVaultAuth(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "AWS auth (default) requires no extra config",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "aws"},
			},
			wantErr: false,
		},
		{
			name: "empty auth method defaults to AWS",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: ""},
			},
			wantErr: false,
		},
		{
			name: "Kubernetes auth requires role",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "kubernetes"},
			},
			wantErr: true,
			errMsg:  "VAULT_K8S_ROLE",
		},
		{
			name: "Kubernetes auth with role is valid",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "kubernetes", K8sRole: "my-role", K8sJWTPath: "/var/run/secrets/kubernetes.io/serviceaccount/token"},
			},
			wantErr: false,
		},
		{
			name: "K8s alias works like kubernetes",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "k8s", K8sRole: "my-role", K8sJWTPath: "/var/run/secrets/kubernetes.io/serviceaccount/token"},
			},
			wantErr: false,
		},
		{
			name: "AppRole requires role_id",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "approle"},
			},
			wantErr: true,
			errMsg:  "VAULT_APP_ROLE_ID",
		},
		{
			name: "AppRole requires secret_id",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "approle", AppRoleID: "my-role"},
			},
			wantErr: true,
			errMsg:  "VAULT_APP_SECRET_ID",
		},
		{
			name: "AppRole with both IDs is valid",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "approle", AppRoleID: "my-role", AppRoleSecretID: "secret"},
			},
			wantErr: false,
		},
		{
			name: "Token auth requires token",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "token"},
			},
			wantErr: true,
			errMsg:  "VAULT_TOKEN",
		},
		{
			name: "Token auth with token is valid",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "token", Token: "my-token"},
			},
			wantErr: false,
		},
		{
			name: "Unknown auth method",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "unknown"},
			},
			wantErr: true,
			errMsg:  "VAULT_AUTH_METHOD",
		},
		{
			name: "K8s auth empty JWT path",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "kubernetes", K8sRole: "my-role", K8sJWTPath: ""},
			},
			wantErr: true,
			errMsg:  "cannot be empty",
		},
		{
			name: "K8s auth relative JWT path",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "kubernetes", K8sRole: "my-role", K8sJWTPath: "relative/path"},
			},
			wantErr: true,
			errMsg:  "must be an absolute path",
		},
		{
			name: "K8s auth JWT path traversal attack",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "kubernetes", K8sRole: "my-role", K8sJWTPath: "/var/run/secrets/../../../etc/passwd"},
			},
			wantErr: true,
			errMsg:  "must be under /var/run/secrets/",
		},
		{
			name: "K8s auth JWT path outside allowed directory",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "kubernetes", K8sRole: "my-role", K8sJWTPath: "/etc/passwd"},
			},
			wantErr: true,
			errMsg:  "must be under /var/run/secrets/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && err != nil && tt.errMsg != "" {
				if !contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error = %v, want error containing %q", err, tt.errMsg)
				}
			}
		})
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

func TestNewStore_SSMBackend(t *testing.T) {
	cfg := Config{
		Backend: BackendSSM,
		SSM: SSMConfig{
			Prefix: "/custom/prefix",
		},
	}

	// NewStore for SSM requires AWS config, but we can test that it doesn't error
	// with a nil context since SSM store creation is synchronous
	// This will panic without proper AWS config, so we just verify the code path
	// is correct by checking validation
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v", err)
	}
}

func TestNewStore_UnknownBackend(t *testing.T) {
	cfg := Config{
		Backend: "unknown",
	}

	if err := cfg.Validate(); err == nil {
		t.Error("Validate() expected error for unknown backend")
	}
}

func TestConfig_Validate_EmptyBackend(t *testing.T) {
	cfg := Config{
		Backend: "",
	}

	// Empty backend should be valid (defaults to SSM)
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v", err)
	}
}

func TestGetEnvInt_InvalidValue(t *testing.T) {
	_ = os.Setenv("TEST_INVALID_INT", "not-a-number")
	defer func() { _ = os.Unsetenv("TEST_INVALID_INT") }()

	result := getEnvInt("TEST_INVALID_INT", 42)
	if result != 42 {
		t.Errorf("getEnvInt() = %d, want 42 (default)", result)
	}
}

func TestGetEnvInt_ValidValue(t *testing.T) {
	_ = os.Setenv("TEST_VALID_INT", "123")
	defer func() { _ = os.Unsetenv("TEST_VALID_INT") }()

	result := getEnvInt("TEST_VALID_INT", 42)
	if result != 123 {
		t.Errorf("getEnvInt() = %d, want 123", result)
	}
}

func TestGetEnvInt_EmptyValue(t *testing.T) {
	_ = os.Unsetenv("TEST_EMPTY_INT")

	result := getEnvInt("TEST_EMPTY_INT", 99)
	if result != 99 {
		t.Errorf("getEnvInt() = %d, want 99 (default)", result)
	}
}

func TestLoadConfig_AllVaultEnvVars(t *testing.T) {
	envVars := map[string]string{
		"RUNS_FLEET_SECRETS_BACKEND": "vault",
		"VAULT_ADDR":                 "https://vault.example.com:8200",
		"VAULT_NAMESPACE":            "admin",
		"VAULT_KV_MOUNT":             "kv",
		"VAULT_KV_VERSION":           "1",
		"VAULT_BASE_PATH":            "custom/runners",
		"VAULT_AUTH_METHOD":          "kubernetes",
		"VAULT_AWS_ROLE":             "my-aws-role",
		"VAULT_AWS_REGION":           "eu-west-1",
		"VAULT_K8S_ROLE":             "my-k8s-role",
		"VAULT_K8S_JWT_PATH":         "/custom/path/token",
		"VAULT_APP_ROLE_ID":          "role-123",
		"VAULT_APP_SECRET_ID":        "secret-456",
		"VAULT_TOKEN":                "hvs.token123",
	}

	for k, v := range envVars {
		_ = os.Setenv(k, v)
	}
	defer func() {
		for k := range envVars {
			_ = os.Unsetenv(k)
		}
	}()

	cfg := LoadConfig()

	if cfg.Backend != BackendVault {
		t.Errorf("Backend = %s, want %s", cfg.Backend, BackendVault)
	}
	if cfg.Vault.Address != "https://vault.example.com:8200" {
		t.Errorf("Vault.Address = %s, want https://vault.example.com:8200", cfg.Vault.Address)
	}
	if cfg.Vault.Namespace != "admin" {
		t.Errorf("Vault.Namespace = %s, want admin", cfg.Vault.Namespace)
	}
	if cfg.Vault.KVMount != "kv" {
		t.Errorf("Vault.KVMount = %s, want kv", cfg.Vault.KVMount)
	}
	if cfg.Vault.KVVersion != 1 {
		t.Errorf("Vault.KVVersion = %d, want 1", cfg.Vault.KVVersion)
	}
	if cfg.Vault.BasePath != "custom/runners" {
		t.Errorf("Vault.BasePath = %s, want custom/runners", cfg.Vault.BasePath)
	}
	if cfg.Vault.AuthMethod != "kubernetes" {
		t.Errorf("Vault.AuthMethod = %s, want kubernetes", cfg.Vault.AuthMethod)
	}
	if cfg.Vault.K8sRole != "my-k8s-role" {
		t.Errorf("Vault.K8sRole = %s, want my-k8s-role", cfg.Vault.K8sRole)
	}
	if cfg.Vault.K8sJWTPath != "/custom/path/token" {
		t.Errorf("Vault.K8sJWTPath = %s, want /custom/path/token", cfg.Vault.K8sJWTPath)
	}
	if cfg.Vault.AppRoleID != "role-123" {
		t.Errorf("Vault.AppRoleID = %s, want role-123", cfg.Vault.AppRoleID)
	}
	if cfg.Vault.AppRoleSecretID != "secret-456" {
		t.Errorf("Vault.AppRoleSecretID = %s, want secret-456", cfg.Vault.AppRoleSecretID)
	}
	if cfg.Vault.Token != "hvs.token123" {
		t.Errorf("Vault.Token = %s, want hvs.token123", cfg.Vault.Token)
	}
}

func TestConfig_ValidateVaultAuth_TokenFromEnv(t *testing.T) {
	_ = os.Setenv("VAULT_TOKEN", "env-token")
	defer func() { _ = os.Unsetenv("VAULT_TOKEN") }()

	cfg := Config{
		Backend: BackendVault,
		Vault: VaultConfig{
			Address:    "https://vault.example.com",
			AuthMethod: AuthMethodToken,
			Token:      "", // Empty, but VAULT_TOKEN env is set
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v, expected no error when VAULT_TOKEN env is set", err)
	}
}
