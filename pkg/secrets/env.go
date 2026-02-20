package secrets

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// EnvStore reads runner configuration from environment variables.
// Used when bootstrap has already fetched config from external source (e.g., Vault)
// and written it to the environment file.
type EnvStore struct{}

// NewEnvStore creates a new environment-based secrets store.
func NewEnvStore() *EnvStore {
	return &EnvStore{}
}

// Put is not supported for EnvStore (read-only store).
func (s *EnvStore) Put(_ context.Context, _ string, _ *RunnerConfig) error {
	return fmt.Errorf("EnvStore is read-only; config must be set via environment variables")
}

// Get retrieves runner configuration from environment variables.
// The runnerID parameter is ignored as config is already in env vars.
func (s *EnvStore) Get(_ context.Context, _ string) (*RunnerConfig, error) {
	jitToken := os.Getenv("RUNS_FLEET_JIT_TOKEN")
	if jitToken == "" {
		return nil, fmt.Errorf("RUNS_FLEET_JIT_TOKEN is required when using env backend")
	}

	org := os.Getenv("RUNS_FLEET_ORG")
	repo := os.Getenv("RUNS_FLEET_REPO")
	if org == "" && repo == "" {
		return nil, fmt.Errorf("RUNS_FLEET_ORG or RUNS_FLEET_REPO is required when using env backend")
	}

	config := &RunnerConfig{
		Org:                 org,
		Repo:                repo,
		RunID:               os.Getenv("RUNS_FLEET_RUN_ID"),
		JITToken:            jitToken,
		JobID:               os.Getenv("RUNS_FLEET_JOB_ID"),
		CacheToken:          os.Getenv("RUNS_FLEET_CACHE_TOKEN"),
		CacheURL:            os.Getenv("RUNS_FLEET_CACHE_URL"),
		TerminationQueueURL: os.Getenv("RUNS_FLEET_TERMINATION_QUEUE_URL"),
		RunnerGroup:         os.Getenv("RUNS_FLEET_RUNNER_GROUP"),
		RunnerName:          os.Getenv("RUNS_FLEET_RUNNER_NAME"),
	}

	// Parse labels from comma-separated string
	labelsStr := os.Getenv("RUNS_FLEET_LABELS")
	if labelsStr != "" {
		config.Labels = splitLabels(labelsStr)
	}

	// Parse IsOrg flag
	isOrgStr := os.Getenv("RUNS_FLEET_IS_ORG")
	config.IsOrg = isOrgStr == "true" || isOrgStr == "1"

	return config, nil
}

// Delete is not supported for EnvStore (read-only store).
func (s *EnvStore) Delete(_ context.Context, _ string) error {
	return fmt.Errorf("EnvStore is read-only; cannot delete configuration")
}

// List is not supported for EnvStore (single config only).
func (s *EnvStore) List(_ context.Context) ([]string, error) {
	return nil, fmt.Errorf("EnvStore does not support listing; only current instance config available")
}

// splitLabels splits comma-separated labels into a slice.
func splitLabels(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var labels []string
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			labels = append(labels, trimmed)
		}
	}
	return labels
}
