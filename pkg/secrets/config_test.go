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
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "kubernetes", K8sRole: "my-role"},
			},
			wantErr: false,
		},
		{
			name: "K8s alias works like kubernetes",
			cfg: Config{
				Backend: BackendVault,
				Vault:   VaultConfig{Address: "https://vault.example.com", AuthMethod: "k8s", K8sRole: "my-role"},
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
