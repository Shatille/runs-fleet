package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/vault/api"
)

// VaultConfig holds configuration for Vault secrets backend.
type VaultConfig struct {
	Address    string // VAULT_ADDR
	Namespace  string // VAULT_NAMESPACE (enterprise)
	KVMount    string // KV mount path (default: "secret")
	KVVersion  int    // 0=auto-detect, 1, 2
	BasePath   string // Base path for runner configs (default: "runs-fleet/runners")
	AuthMethod string // "aws", "kubernetes", "approle", "token"

	// AWS IAM auth
	AWSRole   string // Vault role for AWS auth
	AWSRegion string // AWS region for STS calls

	// Kubernetes auth
	K8sAuthMount string // Vault Kubernetes auth mount path (default: "kubernetes")
	K8sRole      string // Vault role for K8s auth
	K8sJWTPath   string // Path to service account token

	// AppRole auth
	AppRoleID       string
	AppRoleSecretID string

	// Token auth (for testing/development)
	Token string
}

// VaultStore implements Store using HashiCorp Vault KV secrets engine.
type VaultStore struct {
	client    *api.Client
	kvMount   string
	basePath  string
	kvVersion int

	// Token renewal
	renewCancel context.CancelFunc
	renewWg     sync.WaitGroup
}

// NewVaultStore creates a Vault-backed secrets store.
func NewVaultStore(ctx context.Context, cfg VaultConfig) (*VaultStore, error) {
	vaultCfg := api.DefaultConfig()
	if cfg.Address != "" {
		vaultCfg.Address = cfg.Address
	}

	// Set HTTP client timeout to prevent indefinite hangs
	vaultCfg.HttpClient = &http.Client{
		Timeout: 30 * time.Second,
	}

	client, err := api.NewClient(vaultCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create Vault client: %w", err)
	}

	if cfg.Namespace != "" {
		client.SetNamespace(cfg.Namespace)
	}

	// Authenticate
	if err := authenticate(ctx, client, cfg); err != nil {
		return nil, fmt.Errorf("vault authentication failed: %w", err)
	}

	kvMount := cfg.KVMount
	if kvMount == "" {
		kvMount = DefaultVaultKVMount
	}

	basePath := cfg.BasePath
	if basePath == "" {
		basePath = DefaultVaultPath
	}

	store := &VaultStore{
		client:    client,
		kvMount:   kvMount,
		basePath:  basePath,
		kvVersion: cfg.KVVersion,
	}

	// Auto-detect KV version if not specified
	if store.kvVersion == 0 {
		version, err := store.detectKVVersion(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to detect KV version: %w", err)
		}
		store.kvVersion = version
	}

	// Start token renewal for long-running processes
	store.startTokenRenewal(ctx)

	return store, nil
}

// NewVaultStoreWithClient creates a Vault store with a pre-configured client (for testing).
func NewVaultStoreWithClient(client *api.Client, kvMount, basePath string, kvVersion int) *VaultStore {
	if kvMount == "" {
		kvMount = DefaultVaultKVMount
	}
	if basePath == "" {
		basePath = DefaultVaultPath
	}
	if kvVersion == 0 {
		kvVersion = 2
	}
	return &VaultStore{
		client:    client,
		kvMount:   kvMount,
		basePath:  basePath,
		kvVersion: kvVersion,
	}
}

// detectKVVersion determines whether the KV engine is v1 or v2.
func (v *VaultStore) detectKVVersion(ctx context.Context) (int, error) {
	mounts, err := v.client.Sys().ListMountsWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to list mounts for KV version detection: %w", err)
	}

	mountPath := v.kvMount + "/"
	mount, ok := mounts[mountPath]
	if !ok {
		return 0, fmt.Errorf("KV mount %q not found", v.kvMount)
	}

	if mount.Options != nil {
		if version, exists := mount.Options["version"]; exists {
			if version == "1" {
				return 1, nil
			}
		}
	}

	return 2, nil
}

// Put stores runner configuration in Vault.
func (v *VaultStore) Put(ctx context.Context, runnerID string, config *RunnerConfig) error {
	data := map[string]interface{}{
		"org":                   config.Org,
		"repo":                  config.Repo,
		"run_id":                config.RunID,
		"jit_token":             config.JITToken,
		"labels":                config.Labels,
		"runner_group":          config.RunnerGroup,
		"job_id":                config.JobID,
		"cache_token":           config.CacheToken,
		"cache_url":             config.CacheURL,
		"termination_queue_url": config.TerminationQueueURL,
		"is_org":                config.IsOrg,
	}

	path := v.secretPath(runnerID)

	if v.kvVersion == 2 {
		_, err := v.client.KVv2(v.kvMount).Put(ctx, v.basePath+"/"+runnerID, data)
		if err != nil {
			return fmt.Errorf("failed to write secret to Vault: %w", err)
		}
	} else {
		_, err := v.client.Logical().WriteWithContext(ctx, path, data)
		if err != nil {
			return fmt.Errorf("failed to write secret to Vault: %w", err)
		}
	}

	return nil
}

// Get retrieves runner configuration from Vault.
func (v *VaultStore) Get(ctx context.Context, runnerID string) (*RunnerConfig, error) {
	var data map[string]interface{}

	if v.kvVersion == 2 {
		secret, err := v.client.KVv2(v.kvMount).Get(ctx, v.basePath+"/"+runnerID)
		if err != nil {
			return nil, fmt.Errorf("failed to read secret from Vault: %w", err)
		}
		if secret == nil || secret.Data == nil {
			return nil, fmt.Errorf("secret not found")
		}
		data = secret.Data
	} else {
		secret, err := v.client.Logical().ReadWithContext(ctx, v.secretPath(runnerID))
		if err != nil {
			return nil, fmt.Errorf("failed to read secret from Vault: %w", err)
		}
		if secret == nil || secret.Data == nil {
			return nil, fmt.Errorf("secret not found")
		}
		data = secret.Data
	}

	// Convert map to RunnerConfig via JSON
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal secret data: %w", err)
	}

	var config RunnerConfig
	if err := json.Unmarshal(jsonBytes, &config); err != nil {
		return nil, fmt.Errorf("failed to parse runner config: %w", err)
	}

	return &config, nil
}

// Delete removes runner configuration from Vault.
func (v *VaultStore) Delete(ctx context.Context, runnerID string) error {
	if v.kvVersion == 2 {
		// For KV v2, we need to delete metadata to fully remove the secret
		err := v.client.KVv2(v.kvMount).DeleteMetadata(ctx, v.basePath+"/"+runnerID)
		if err != nil && !isNotFoundError(err) {
			return fmt.Errorf("failed to delete secret from Vault: %w", err)
		}
	} else {
		_, err := v.client.Logical().DeleteWithContext(ctx, v.secretPath(runnerID))
		if err != nil && !isNotFoundError(err) {
			return fmt.Errorf("failed to delete secret from Vault: %w", err)
		}
	}

	return nil
}

// isNotFoundError checks if the error indicates a secret was not found.
func isNotFoundError(err error) bool {
	var respErr *api.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == 404
	}
	return false
}

// List returns all runner IDs with stored configuration.
func (v *VaultStore) List(ctx context.Context) ([]string, error) {
	var listPath string
	if v.kvVersion == 2 {
		listPath = fmt.Sprintf("%s/metadata/%s", v.kvMount, v.basePath)
	} else {
		listPath = fmt.Sprintf("%s/%s", v.kvMount, v.basePath)
	}

	secret, err := v.client.Logical().ListWithContext(ctx, listPath)
	if err != nil {
		return nil, fmt.Errorf("failed to list secrets from Vault: %w", err)
	}

	if secret == nil || secret.Data == nil {
		return []string{}, nil
	}

	keysRaw, ok := secret.Data["keys"]
	if !ok {
		return []string{}, nil
	}

	keysSlice, ok := keysRaw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected keys format from Vault")
	}

	var runnerIDs []string
	for _, key := range keysSlice {
		keyStr, ok := key.(string)
		if !ok {
			continue
		}
		// Skip directory entries (end with /)
		if strings.HasSuffix(keyStr, "/") {
			continue
		}
		runnerIDs = append(runnerIDs, keyStr)
	}

	return runnerIDs, nil
}

// secretPath returns the full path for a KV v1 secret.
func (v *VaultStore) secretPath(runnerID string) string {
	return fmt.Sprintf("%s/%s/%s", v.kvMount, v.basePath, runnerID)
}

// startTokenRenewal starts a background goroutine that renews the Vault token
// before it expires. This is critical for long-running server processes.
func (v *VaultStore) startTokenRenewal(ctx context.Context) {
	renewCtx, cancel := context.WithCancel(ctx)
	v.renewCancel = cancel

	v.renewWg.Add(1)
	go func() {
		defer v.renewWg.Done()
		v.renewTokenLoop(renewCtx)
	}()
}

// renewTokenLoop periodically renews the Vault token.
func (v *VaultStore) renewTokenLoop(ctx context.Context) {
	// Check token TTL and renew at 50% of remaining time
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := v.maybeRenewToken(ctx); err != nil {
				// Log but don't fail - next iteration will retry
				continue
			}
		}
	}
}

// maybeRenewToken checks if token needs renewal and renews it.
func (v *VaultStore) maybeRenewToken(ctx context.Context) error {
	// Look up current token info
	secret, err := v.client.Auth().Token().LookupSelfWithContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to lookup token: %w", err)
	}

	if secret == nil || secret.Data == nil {
		return nil
	}

	// Get TTL from token data
	ttlRaw, ok := secret.Data["ttl"]
	if !ok {
		return nil
	}

	// TTL can be json.Number or float64 depending on response
	var ttlSeconds float64
	switch t := ttlRaw.(type) {
	case json.Number:
		ttlSeconds, _ = t.Float64()
	case float64:
		ttlSeconds = t
	default:
		return nil
	}

	// Renew if less than 50% TTL remaining
	if ttlSeconds > 0 && ttlSeconds < 3600 {
		_, err := v.client.Auth().Token().RenewSelfWithContext(ctx, 0)
		if err != nil {
			return fmt.Errorf("failed to renew token: %w", err)
		}
	}

	return nil
}

// Close stops token renewal and cleans up resources.
func (v *VaultStore) Close() {
	if v.renewCancel != nil {
		v.renewCancel()
		v.renewWg.Wait()
	}
}

// Ensure VaultStore implements Store.
var _ Store = (*VaultStore)(nil)
