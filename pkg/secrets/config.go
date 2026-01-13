package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Backend constants for secrets backend selection.
const (
	BackendSSM   = "ssm"
	BackendVault = "vault"
)

// Auth method constants for Vault.
const (
	AuthMethodAWS        = "aws"
	AuthMethodKubernetes = "kubernetes"
	AuthMethodK8s        = "k8s"
	AuthMethodAppRole    = "approle"
	AuthMethodToken      = "token"
)

// Default paths.
const (
	DefaultSSMPrefix    = "/runs-fleet/runners"
	DefaultVaultKVMount = "secret"
	DefaultVaultPath    = "runs-fleet/runners"
)

// Config holds configuration for secrets backend.
type Config struct {
	Backend string // "ssm" or "vault" (default: "ssm")
	SSM     SSMConfig
	Vault   VaultConfig
}

// SSMConfig holds SSM-specific configuration.
type SSMConfig struct {
	Prefix string // Parameter path prefix (default: "/runs-fleet/runners")
}

// LoadConfig loads secrets configuration from environment variables.
func LoadConfig() Config {
	cfg := Config{
		Backend: getEnv("RUNS_FLEET_SECRETS_BACKEND", BackendSSM),
		SSM: SSMConfig{
			Prefix: getEnv("RUNS_FLEET_SSM_PREFIX", DefaultSSMPrefix),
		},
		Vault: VaultConfig{
			Address:         getEnv("VAULT_ADDR", ""),
			Namespace:       getEnv("VAULT_NAMESPACE", ""),
			KVMount:         getEnv("VAULT_KV_MOUNT", DefaultVaultKVMount),
			KVVersion:       getEnvInt("VAULT_KV_VERSION", 0),
			BasePath:        getEnv("VAULT_BASE_PATH", DefaultVaultPath),
			AuthMethod:      getEnv("VAULT_AUTH_METHOD", AuthMethodAWS),
			AWSRole:         getEnv("VAULT_AWS_ROLE", "runs-fleet"),
			AWSRegion:       getEnv("VAULT_AWS_REGION", os.Getenv("AWS_REGION")),
			K8sRole:         getEnv("VAULT_K8S_ROLE", ""),
			K8sJWTPath:      getEnv("VAULT_K8S_JWT_PATH", "/var/run/secrets/kubernetes.io/serviceaccount/token"),
			AppRoleID:       getEnv("VAULT_APP_ROLE_ID", ""),
			AppRoleSecretID: getEnv("VAULT_APP_SECRET_ID", ""),
			Token:           getEnv("VAULT_TOKEN", ""),
		},
	}
	return cfg
}

// NewStore creates a secrets store based on configuration.
func NewStore(ctx context.Context, cfg Config, awsCfg aws.Config) (Store, error) {
	switch cfg.Backend {
	case BackendVault:
		return NewVaultStore(ctx, cfg.Vault)
	case BackendSSM, "":
		return NewSSMStore(awsCfg, cfg.SSM.Prefix), nil
	default:
		return nil, fmt.Errorf("unknown secrets backend: %s", cfg.Backend)
	}
}

// Validate checks that the configuration is valid for the selected backend.
func (c *Config) Validate() error {
	switch c.Backend {
	case BackendVault:
		if c.Vault.Address == "" {
			return fmt.Errorf("VAULT_ADDR is required when using Vault backend")
		}
		// Validate auth method requirements
		if err := c.validateVaultAuth(); err != nil {
			return err
		}
	case BackendSSM, "":
		// SSM has no required configuration
	default:
		return fmt.Errorf("RUNS_FLEET_SECRETS_BACKEND must be 'ssm' or 'vault', got %q", c.Backend)
	}
	return nil
}

// validateVaultAuth checks that required auth parameters are configured.
func (c *Config) validateVaultAuth() error {
	switch c.Vault.AuthMethod {
	case AuthMethodKubernetes, AuthMethodK8s:
		if c.Vault.K8sRole == "" {
			return fmt.Errorf("VAULT_K8S_ROLE is required for Kubernetes auth")
		}
		if err := validateK8sJWTPath(c.Vault.K8sJWTPath); err != nil {
			return err
		}
	case AuthMethodAppRole:
		if c.Vault.AppRoleID == "" {
			return fmt.Errorf("VAULT_APP_ROLE_ID is required for AppRole auth")
		}
		if c.Vault.AppRoleSecretID == "" {
			return fmt.Errorf("VAULT_APP_SECRET_ID is required for AppRole auth")
		}
	case AuthMethodToken:
		if c.Vault.Token == "" && os.Getenv("VAULT_TOKEN") == "" {
			return fmt.Errorf("VAULT_TOKEN is required for token auth")
		}
	case AuthMethodAWS, "":
		// AWS auth uses IAM credentials automatically
	default:
		return fmt.Errorf("VAULT_AUTH_METHOD must be 'aws', 'kubernetes', 'k8s', 'approle', or 'token', got %q", c.Vault.AuthMethod)
	}
	return nil
}

// validateK8sJWTPath validates the Kubernetes JWT token path for security.
func validateK8sJWTPath(path string) error {
	if path == "" {
		return fmt.Errorf("VAULT_K8S_JWT_PATH cannot be empty for Kubernetes auth")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("VAULT_K8S_JWT_PATH must be an absolute path, got %q", path)
	}
	cleanPath := filepath.Clean(path)
	if !strings.HasPrefix(cleanPath, "/var/run/secrets/") {
		return fmt.Errorf("VAULT_K8S_JWT_PATH must be under /var/run/secrets/, got %q", path)
	}
	return nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	result, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return result
}
