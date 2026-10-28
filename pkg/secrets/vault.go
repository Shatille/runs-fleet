package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/vault/api"

	"github.com/Shavakan/runs-fleet/pkg/logging"
)

var vaultLog = logging.WithComponent(logging.LogTypeSecrets, "vault")

// tokenCheckInterval is how often the renewal loop inspects the token. A var so
// tests can derive the re-login threshold from it.
var tokenCheckInterval = 5 * time.Minute

const reloginTimeout = 30 * time.Second

var errReloginFailed = errors.New("vault re-login failed")

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
	AWSRegion string // operating region (error context only; the STS login signs global/us-east-1)

	// Kubernetes auth
	K8sAuthMount string // Vault Kubernetes auth mount path (default: "kubernetes")
	K8sRole      string // Vault role for K8s auth
	K8sJWTPath   string // Path to service account token

	// AppRole auth
	AppRoleID       string
	AppRoleSecretID string

	// Token auth (for testing/development)
	Token string

	// Metrics, when set, receives authentication failures.
	Metrics AuthFailureRecorder
}

// AuthFailureRecorder receives secrets-backend authentication failures.
// metrics.Publisher satisfies it.
type AuthFailureRecorder interface {
	PublishSecretsAuthFailure(ctx context.Context, backend, reason string) error
}

// VaultStore implements Store using HashiCorp Vault KV secrets engine.
type VaultStore struct {
	client    *api.Client
	kvMount   string
	basePath  string
	kvVersion int

	login   func(context.Context) (string, error)
	loginMu sync.Mutex
	metrics AuthFailureRecorder

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
		login:     reloginFunc(client, cfg),
		metrics:   cfg.Metrics,
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
	if v1Status < 0 || v2Status < 0 {
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
// Returns -1 for network errors or non-HTTP failures.
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

	return -1 // Network error or other non-response failure
}

// Put stores runner configuration in Vault.
func (v *VaultStore) Put(ctx context.Context, runnerID string, config *RunnerConfig) error {
	// Serialize through JSON rather than enumerating fields by hand so the
	// written payload stays symmetric with Get (which unmarshals the same JSON
	// shape). A hand-maintained field map silently dropped struct fields added
	// later; the round-trip cannot.
	jsonBytes, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal runner config: %w", err)
	}
	var data map[string]interface{}
	if err = json.Unmarshal(jsonBytes, &data); err != nil {
		return fmt.Errorf("failed to build Vault secret data: %w", err)
	}

	path := v.secretPath(runnerID)

	err = v.withReauth(ctx, func() error {
		if v.kvVersion == 2 {
			_, putErr := v.client.KVv2(v.kvMount).Put(ctx, v.basePath+"/"+runnerID, data)
			return putErr
		}
		_, putErr := v.client.Logical().WriteWithContext(ctx, path, data)
		return putErr
	})
	if err != nil {
		return fmt.Errorf("failed to write secret to Vault: %w", err)
	}

	return nil
}

// Get retrieves runner configuration from Vault.
func (v *VaultStore) Get(ctx context.Context, runnerID string) (*RunnerConfig, error) {
	var data map[string]interface{}

	if v.kvVersion == 2 {
		var secret *api.KVSecret
		err := v.withReauth(ctx, func() error {
			var err error
			secret, err = v.client.KVv2(v.kvMount).Get(ctx, v.basePath+"/"+runnerID)
			return err
		})
		if err != nil {
			if isNotFoundError(err) {
				return nil, fmt.Errorf("%s: %w", runnerID, ErrConfigNotFound)
			}
			return nil, fmt.Errorf("failed to read secret from Vault: %w", err)
		}
		if secret == nil || secret.Data == nil {
			return nil, fmt.Errorf("%s: %w", runnerID, ErrConfigNotFound)
		}
		data = secret.Data
	} else {
		var secret *api.Secret
		err := v.withReauth(ctx, func() error {
			var err error
			secret, err = v.client.Logical().ReadWithContext(ctx, v.secretPath(runnerID))
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("failed to read secret from Vault: %w", err)
		}
		// KV v1 returns (nil, nil) for a missing secret.
		if secret == nil || secret.Data == nil {
			return nil, fmt.Errorf("%s: %w", runnerID, ErrConfigNotFound)
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
	err := v.withReauth(ctx, func() error {
		if v.kvVersion == 2 {
			// For KV v2, we need to delete metadata to fully remove the secret
			return v.client.KVv2(v.kvMount).DeleteMetadata(ctx, v.basePath+"/"+runnerID)
		}
		_, err := v.client.Logical().DeleteWithContext(ctx, v.secretPath(runnerID))
		return err
	})
	if err != nil && !isNotFoundError(err) {
		return fmt.Errorf("failed to delete secret from Vault: %w", err)
	}

	return nil
}

// isNotFoundError checks if the error indicates a secret was not found.
func isNotFoundError(err error) bool {
	if errors.Is(err, api.ErrSecretNotFound) {
		return true
	}
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

	var secret *api.Secret
	err := v.withReauth(ctx, func() error {
		var err error
		secret, err = v.client.Logical().ListWithContext(ctx, listPath)
		return err
	})
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
	ticker := time.NewTicker(tokenCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			v.checkToken(ctx)
		}
	}
}

func (v *VaultStore) checkToken(ctx context.Context) {
	if err := v.maybeRenewToken(ctx); err != nil {
		vaultLog.Warn(ctx, "vault token check failed; retrying next tick",
			slog.String("error", err.Error()))
		if !errors.Is(err, errReloginFailed) {
			v.recordAuthFailure(ctx, "renew")
		}
	}
}

func (v *VaultStore) recordAuthFailure(ctx context.Context, reason string) {
	if v.metrics == nil {
		return
	}
	if err := v.metrics.PublishSecretsAuthFailure(ctx, BackendVault, reason); err != nil {
		vaultLog.Error(ctx, "secrets auth-failure metric failed",
			slog.String("reason", reason), slog.String("error", err.Error()))
	}
}

// maybeRenewToken renews a token nearing expiry, and logs in again when the
// token is revoked or renewal can no longer extend it past the role's max TTL.
func (v *VaultStore) maybeRenewToken(ctx context.Context) error {
	staleToken := v.client.Token()

	secret, err := v.client.Auth().Token().LookupSelfWithContext(ctx)
	if err != nil {
		if v.login != nil && isPermissionDenied(err) {
			return v.relogin(ctx, staleToken)
		}
		return fmt.Errorf("failed to lookup token: %w", err)
	}

	if secret == nil || secret.Data == nil {
		return nil
	}

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

	remaining := time.Duration(ttlSeconds * float64(time.Second))
	var renewErr error
	if ttlSeconds > 0 && ttlSeconds < 3600 {
		renewed, err := v.client.Auth().Token().RenewSelfWithContext(ctx, 0)
		switch {
		case err != nil && v.login != nil && isPermissionDenied(err):
			return v.relogin(ctx, staleToken)
		case err != nil:
			renewErr = fmt.Errorf("failed to renew token: %w", err)
		case renewed != nil:
			if ttl, ttlErr := renewed.TokenTTL(); ttlErr == nil && ttl > 0 {
				remaining = ttl
			}
		}
	}

	// Near the role's max TTL Vault returns a truncated lease; log in before it lapses.
	if v.login != nil && remaining > 0 && remaining < 2*tokenCheckInterval {
		return v.relogin(ctx, staleToken)
	}

	return renewErr
}

// withReauth runs op, and when Vault rejects the token, logs in again and
// retries op once.
func (v *VaultStore) withReauth(ctx context.Context, op func() error) error {
	staleToken := v.client.Token()
	err := op()
	if err == nil || v.login == nil || !isPermissionDenied(err) {
		return err
	}
	if loginErr := v.relogin(ctx, staleToken); loginErr != nil {
		return errors.Join(err, loginErr)
	}
	return op()
}

// relogin replaces staleToken with a fresh login. Concurrent callers holding
// the same stale token share one login.
func (v *VaultStore) relogin(ctx context.Context, staleToken string) error {
	v.loginMu.Lock()
	defer v.loginMu.Unlock()

	if v.client.Token() != staleToken {
		return nil
	}

	// Detached: waiters share this login, so one caller's cancellation must not fail them all.
	loginCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reloginTimeout)
	defer cancel()

	token, err := v.login(loginCtx)
	if err == nil && token == "" {
		err = errors.New("login returned no client token")
	}
	if err != nil {
		vaultLog.Error(ctx, "vault re-login failed", slog.String("error", err.Error()))
		v.recordAuthFailure(ctx, "relogin")
		return fmt.Errorf("%w: %w", errReloginFailed, err)
	}

	v.client.SetToken(token)
	vaultLog.Info(ctx, "vault re-login succeeded")
	return nil
}

func isPermissionDenied(err error) bool {
	var respErr *api.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusForbidden
}

// reloginFunc returns how to obtain a fresh token for cfg, or nil when the
// token was supplied statically and cannot be replaced.
func reloginFunc(client *api.Client, cfg VaultConfig) func(context.Context) (string, error) {
	if cfg.AuthMethod == AuthMethodToken || (cfg.AuthMethod == "" && os.Getenv("VAULT_TOKEN") != "") {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		// Log in on a clone so a failed login leaves the live token in place.
		fresh, err := client.CloneWithHeaders()
		if err != nil {
			return "", fmt.Errorf("failed to clone Vault client: %w", err)
		}
		fresh.ClearToken()
		if err := authenticate(ctx, fresh, cfg); err != nil {
			return "", err
		}
		return fresh.Token(), nil
	}
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
