// Package runner manages runner registration and lifecycle, delegating
// GitHub API calls to pkg/github.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/cache"
	"github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

var runnerLog = logging.WithComponent(logging.LogTypeRunner, "manager")

// ManagerConfig holds configuration for the runner manager.
type ManagerConfig struct {
	CacheSecret         string
	BaseURL             string
	TerminationQueueURL string
	// BuildkitCacheBucket enables transparent Docker layer caching. Empty =
	// disabled: the manager then leaves the runner config's Buildkit* fields
	// empty and the whole feature stays inert. BuildkitCacheRegion is the S3
	// region for the cache backend.
	BuildkitCacheBucket string
	BuildkitCacheRegion string
}

// registrationTokenGetter is the subset of a git-hosting provider's API that
// Manager needs to register a runner. Satisfied by *github.Client today; a
// future provider can satisfy it without Manager changing.
type registrationTokenGetter interface {
	GetRegistrationToken(ctx context.Context, repo string) (*github.RegistrationResult, error)
}

// Manager handles runner registration and secrets configuration.
type Manager struct {
	github       registrationTokenGetter
	secretsStore secrets.Store
	config       ManagerConfig
}

// NewManager creates a new runner manager.
func NewManager(githubClient registrationTokenGetter, secretsStore secrets.Store, config ManagerConfig) *Manager {
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

	runnerName := buildRunnerName(req.Pool, repoName, req.Conditions, req.JobID)

	// Get registration token from GitHub (returns token and whether owner is an org)
	runnerLog.Info(ctx, "fetching registration token",
		slog.String("runner_name", runnerName),
		slog.String(logging.KeyOwner, org))
	regResult, err := m.github.GetRegistrationToken(ctx, req.Repo)
	if err != nil {
		return fmt.Errorf("failed to get registration token: %w", err)
	}

	// Generate cache token with repository scope for cache isolation
	cacheToken := ""
	if m.config.CacheSecret != "" {
		cacheToken = cache.GenerateCacheToken(m.config.CacheSecret, req.JobID, req.InstanceID, req.Repo)
	}

	// Transparent Docker layer cache: only populated when the bucket is
	// configured. The buildkit/<org>/<repo>/ prefix scopes the layer cache to
	// this repo (conventional, not enforced — same-org trust domain).
	buildkitBucket, buildkitRegion, buildkitPrefix := "", "", ""
	if m.config.BuildkitCacheBucket != "" {
		buildkitBucket = m.config.BuildkitCacheBucket
		buildkitRegion = m.config.BuildkitCacheRegion
		buildkitPrefix = fmt.Sprintf("buildkit/%s/%s/", org, repoName)
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
		CacheURL:            m.config.BaseURL,
		TerminationQueueURL: m.config.TerminationQueueURL,
		IsOrg:               regResult.IsOrg,
		BuildkitCacheBucket: buildkitBucket,
		BuildkitCacheRegion: buildkitRegion,
		BuildkitCachePrefix: buildkitPrefix,
	}

	runnerLog.Info(ctx, "storing runner config")
	if err := m.secretsStore.Put(ctx, req.InstanceID, config); err != nil {
		return fmt.Errorf("failed to store runner config: %w", err)
	}

	runnerLog.Info(ctx, "runner config stored")
	return nil
}

// CleanupRunner deletes the runner configuration from the secrets backend.
func (m *Manager) CleanupRunner(ctx context.Context, instanceID string) error {
	if err := m.secretsStore.Delete(ctx, instanceID); err != nil {
		return fmt.Errorf("failed to delete runner config: %w", err)
	}
	return nil
}

const runnerNameMaxLen = 64

func buildRunnerName(pool, repoName, conditions, jobID string) string {
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
		name = "runs-fleet-runner"
	}

	// Append short job ID suffix to guarantee uniqueness when multiple jobs
	// in the same workflow run share identical runs-on labels.
	if jobID != "" {
		suffix := jobID
		if len(suffix) > 6 {
			suffix = suffix[len(suffix)-6:]
		}
		name += "-" + suffix
	}

	if len(name) > runnerNameMaxLen {
		name = name[:runnerNameMaxLen]
	}
	return name
}
