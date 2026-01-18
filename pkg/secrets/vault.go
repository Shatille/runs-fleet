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

// detectKVVersion determines whether the KV engine is v1 or v2 by probing the mount.
// This avoids requiring sys/mounts read permission.
func (v *VaultStore) detectKVVersion(ctx context.Context) (int, error) {
	// First try the config endpoint - KV v2 has a special config API
	configPath := fmt.Sprintf("%s/config", v.kvMount)
	secret, err := v.client.Logical().ReadWithContext(ctx, configPath)

	// Success - check if response has v2 config fields
	if err == nil && secret != nil && secret.Data != nil {
		// KV v2 config returns specific fields like max_versions, cas_required
		if _, has := secret.Data["max_versions"]; has {
			return 2, nil
		}
		if _, has := secret.Data["cas_required"]; has {
			return 2, nil
		}
		if _, has := secret.Data["delete_version_after"]; has {
			return 2, nil
		}
		// Has data but no v2 config fields - it's a v1 secret named "config"
		return 1, nil
	}

	// Vault SDK returns (nil, nil) for 404 responses
	if err == nil && secret == nil {
		return 1, nil
	}

	var respErr *api.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.StatusCode {
		case 404:
			return 1, nil
		case 403:
			// Permission denied - can't tell from config alone, probe further
			return v.detectKVVersionByProbing(ctx)
		}
	}

	return 0, fmt.Errorf("failed to detect KV version: %w", err)
}

// detectKVVersionByProbing compares responses from v1 and v2 path styles to determine version.
// On v1, the metadata prefix path doesn't exist as a concept.
// On v2, the direct path without data/metadata prefix isn't valid for operations.
func (v *VaultStore) detectKVVersionByProbing(ctx context.Context) (int, error) {
	v1Path := fmt.Sprintf("%s/%s", v.kvMount, v.basePath)
	v2Path := fmt.Sprintf("%s/metadata/%s", v.kvMount, v.basePath)

	v1Status := v.probePathStatus(ctx, v1Path)
	v2Status := v.probePathStatus(ctx, v2Path)

	// Probe failure (network error, timeout) → propagate error
	if v1Status == 0 || v2Status == 0 {
		return 0, fmt.Errorf("failed to probe KV paths: v1=%d v2=%d", v1Status, v2Status)
	}

	// Unexpected status codes (401, 5xx) → propagate error
	validStatus := func(s int) bool { return s == 200 || s == 403 || s == 404 }
	if !validStatus(v1Status) || !validStatus(v2Status) {
		return 0, fmt.Errorf("unexpected probe status codes: v1=%d v2=%d", v1Status, v2Status)
	}

	// v1 path works, v2 metadata denied → v1 (metadata path not in v1 policy)
	if v1Status == 200 && v2Status == 403 {
		return 1, nil
	}

	// v2 metadata path recognized (200 or 403), v1 not found → v2
	if (v2Status == 200 || v2Status == 403) && v1Status == 404 {
		return 2, nil
	}

	// v1 path denied, v2 metadata not found → v1 (v1 path exists, no metadata concept)
	if v1Status == 403 && v2Status == 404 {
		return 1, nil
	}

	// Both succeed → check v2 first (more common in modern setups)
	if v2Status == 200 {
		return 2, nil
	}
	if v1Status == 200 {
		return 1, nil
	}

	// Ambiguous (both 403 or both 404) → default to v1 (safer path construction)
	return 1, nil
}

// probePathStatus returns HTTP status code for a LIST operation on the given path.
func (v *VaultStore) probePathStatus(ctx context.Context, path string) int {
	secret, err := v.client.Logical().ListWithContext(ctx, path)

	if err == nil && secret != nil {
		return 200
	}
	if err == nil && secret == nil {
		return 404 // Vault SDK returns (nil, nil) for 404
	}

	var respErr *api.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode
	}

	return 0
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
