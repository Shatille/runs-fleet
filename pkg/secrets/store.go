// Package secrets provides a unified interface for storing and retrieving
// runner configuration secrets across different backends (SSM, Vault).
package secrets

import (
	"context"
)

// RunnerConfig represents configuration passed to runners.
// This is the canonical structure used by both server and agent components.
type RunnerConfig struct {
	Org                 string   `json:"org"`
	Repo                string   `json:"repo,omitempty"`
	RunID               string   `json:"run_id"`
	JITToken            string   `json:"jit_token"`
	Labels              []string `json:"labels"`
	RunnerGroup         string   `json:"runner_group,omitempty"`
	JobID               string   `json:"job_id,omitempty"`
	CacheToken          string   `json:"cache_token,omitempty"`
	CacheURL            string   `json:"cache_url,omitempty"`
	TerminationQueueURL string   `json:"termination_queue_url,omitempty"`
	IsOrg               bool     `json:"is_org"`
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
