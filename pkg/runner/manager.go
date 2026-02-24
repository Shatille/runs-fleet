package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/cache"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

var runnerLog = logging.WithComponent(logging.LogTypeRunner, "manager")

// ManagerConfig holds configuration for the runner manager.
type ManagerConfig struct {
	CacheSecret          string
	CacheURL             string
	TerminationQueueURL  string
}

// Manager handles runner registration and secrets configuration.
type Manager struct {
	github       *GitHubClient
	secretsStore secrets.Store
	config       ManagerConfig
}

// NewManager creates a new runner manager.
func NewManager(githubClient *GitHubClient, secretsStore secrets.Store, config ManagerConfig) *Manager {
	return &Manager{
		github:       githubClient,
		secretsStore: secretsStore,
		config:       config,
	}
}

// PrepareRunnerRequest contains parameters for preparing a runner.
type PrepareRunnerRequest struct {
	InstanceID string
	JobID      string
	RunID      string
	Repo       string // owner/repo format for repo-level registration
	Labels     []string
	Pool       string
	Conditions string // resource conditions for runner naming (e.g., "arm64-cpu4-ram16")
}

// PrepareRunner stores runner configuration in the secrets backend.
// This should be called after the EC2 instance is created but before it boots.
func (m *Manager) PrepareRunner(ctx context.Context, req PrepareRunnerRequest) error {
	// Extract org from repo string (owner/repo format, required)
	if req.Repo == "" {
		return fmt.Errorf("repo is required (owner/repo format)")
	}
	parts := strings.SplitN(req.Repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid repo format, expected owner/repo: %s", req.Repo)
	}
	org := parts[0]
	repoName := parts[1]

	runnerName := buildRunnerName(req.Pool, repoName, req.Conditions)

	// Get registration token from GitHub (returns token and whether owner is an org)
	runnerLog.Info("fetching registration token",
		slog.String("runner_name", runnerName),
		slog.String(logging.KeyOwner, org),
		slog.String(logging.KeyRepo, req.Repo))
	regResult, err := m.github.GetRegistrationToken(ctx, req.Repo)
	if err != nil {
		return fmt.Errorf("failed to get registration token: %w", err)
	}

	// Generate cache token with repository scope for cache isolation
	cacheToken := ""
	if m.config.CacheSecret != "" {
		cacheToken = cache.GenerateCacheToken(m.config.CacheSecret, req.JobID, req.InstanceID, req.Repo)
	}

	// Build runner config with dynamic org from repo
	config := &secrets.RunnerConfig{
		Org:                 org,
		Repo:                req.Repo,
		RunID:               req.RunID,
		JITToken:            regResult.Token,
		Labels:              req.Labels,
		RunnerName:          runnerName,
		JobID:               req.JobID,
		CacheToken:          cacheToken,
		CacheURL:            m.config.CacheURL,
		TerminationQueueURL: m.config.TerminationQueueURL,
		IsOrg:               regResult.IsOrg,
	}

	runnerLog.Info("storing runner config", slog.String(logging.KeyInstanceID, req.InstanceID))
	if err := m.secretsStore.Put(ctx, req.InstanceID, config); err != nil {
		return fmt.Errorf("failed to store runner config: %w", err)
	}

	runnerLog.Info("runner config stored", slog.String(logging.KeyInstanceID, req.InstanceID))
	return nil
}

// CleanupRunner deletes the runner configuration from the secrets backend.
func (m *Manager) CleanupRunner(ctx context.Context, instanceID string) error {
	if err := m.secretsStore.Delete(ctx, instanceID); err != nil {
		return fmt.Errorf("failed to delete runner config: %w", err)
	}
	return nil
}

// GetRegistrationToken gets a registration token for GitHub Actions runners.
// Used by K8s backend to get JIT token before pod creation.
// EC2 backend uses PrepareRunner instead (handles SSM storage).
func (m *Manager) GetRegistrationToken(ctx context.Context, repo string) (*RegistrationResult, error) {
	return m.github.GetRegistrationToken(ctx, repo)
}

const runnerNameMaxLen = 64

func buildRunnerName(pool, repoName, conditions string) string {
	const prefix = "runs-fleet-runner-"

	var name string
	if pool != "" {
		name = prefix + pool
	} else if repoName != "" {
		name = prefix + repoName
		if conditions != "" {
			name += "-" + conditions
		}
	} else {
		return "runs-fleet-runner"
	}

	if len(name) > runnerNameMaxLen {
		name = name[:runnerNameMaxLen]
	}
	return name
}
