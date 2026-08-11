// Package secrets provides a unified interface for storing and retrieving
// runner configuration secrets across different backends (SSM, Vault).
package secrets

import (
	"context"
	"errors"
)

// ErrConfigNotFound is returned by Store.Get when no runner config exists for the
// given runner ID — e.g. a warm-pool instance that booted before being assigned a
// job. Callers use errors.Is to distinguish this from real backend failures.
var ErrConfigNotFound = errors.New("runner config not found")

// RunnerConfig represents configuration passed to runners.
// This is the canonical structure used by both server and agent components.
type RunnerConfig struct {
	Org   string `json:"org"`
	Repo  string `json:"repo,omitempty"`
	RunID string `json:"run_id"`
	// RegistrationToken is a GitHub runner registration token, passed to
	// `config.sh --token`. It registers a runner bound to a LABEL SET, so GitHub
	// may hand that runner any queued job matching those labels — see JITConfig
	// for the job-bound alternative.
	//
	// The json tag stays `jit_token` deliberately: it is a wire contract with
	// agents already in flight, which read configs written before this rename. The
	// name was always a misnomer — this is not a JIT credential.
	RegistrationToken string `json:"jit_token"`
	// JITConfig is GitHub's encoded just-in-time runner configuration, handed to
	// run.sh via ACTIONS_RUNNER_INPUT_JITCONFIG. When set it supersedes
	// RegistrationToken: a JIT-configured runner is bound by GitHub to the single
	// job it was minted for, so it cannot be handed a different queued job that
	// merely shares its labels. Empty means fall back to token registration via
	// config.sh.
	//
	// Like RegistrationToken this is a credential that registers a runner — never
	// log it, and never put it in a resource tag.
	JITConfig           string   `json:"jit_config,omitempty"`
	Labels              []string `json:"labels"`
	RunnerGroup         string   `json:"runner_group,omitempty"`
	RunnerName          string   `json:"runner_name,omitempty"`
	JobID               string   `json:"job_id,omitempty"`
	CacheToken          string   `json:"cache_token,omitempty"`
	CacheURL            string   `json:"cache_url,omitempty"`
	TerminationQueueURL string   `json:"termination_queue_url,omitempty"`
	IsOrg               bool     `json:"is_org"`
	// CreatedAt (RFC3339) is when this config was written. It bounds how long an
	// agent could still be acting on it, which is what lets housekeeping tell a
	// live assignment from an abandoned one. omitempty: a config written by an
	// older orchestrator has no stamp and is treated as unknown-age.
	CreatedAt string `json:"created_at,omitempty"`
	// BuildkitCache* carry the transparent Docker layer-cache config the agent
	// writes into the runner .env so the on-host buildx shim can add S3
	// cache-from/cache-to. All omitempty: absent (feature disabled, or an older
	// orchestrator) is inert both directions.
	BuildkitCacheBucket string `json:"buildkit_cache_bucket,omitempty"`
	BuildkitCacheRegion string `json:"buildkit_cache_region,omitempty"`
	BuildkitCachePrefix string `json:"buildkit_cache_prefix,omitempty"`
}

// Store defines operations for storing and retrieving runner configuration.
type Store interface {
	// Put stores runner configuration for a given runner ID.
	Put(ctx context.Context, runnerID string, config *RunnerConfig) error

	// Get retrieves runner configuration by runner ID.
	Get(ctx context.Context, runnerID string) (*RunnerConfig, error)

	// Delete removes runner configuration.
	Delete(ctx context.Context, runnerID string) error

	// List returns all runner IDs with stored configuration.
	// Required for housekeeping scan operations.
	List(ctx context.Context) ([]string, error)
}
