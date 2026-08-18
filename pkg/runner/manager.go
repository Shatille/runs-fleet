// Package runner manages runner registration and lifecycle, delegating
// GitHub API calls to pkg/github.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/agent/logship"
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
	// RunnerLogsBucket enables uploading the runner's _diag logs to S3. Empty =
	// disabled, leaving the runner config's RunnerLogs* fields empty.
	RunnerLogsBucket string
	// RunnerGroup is the GitHub runner group to place runners in. Empty = GitHub's
	// Default group, resolved without an API call. Only consulted on the JIT path,
	// which needs the group's numeric ID; token registration passes the name
	// straight through to config.sh.
	//
	// Not yet reachable from config: cmd/server/main.go leaves this empty because
	// there is no RUNS_FLEET_RUNNER_GROUP and production runners already use
	// Default. Wiring it up is a standalone change, not a prerequisite for JIT.
	RunnerGroup string
}

// registrationTokenGetter is the subset of a git-hosting provider's API that
// Manager needs to register a runner. Satisfied by *github.Client today; a
// future provider can satisfy it without Manager changing.
type registrationTokenGetter interface {
	GetRegistrationToken(ctx context.Context, repo string) (*github.RegistrationResult, error)
}

// jitConfigGenerator is the optional capability for minting a just-in-time runner
// config. A provider that satisfies it gets job-bound runners: GitHub ties a JIT
// runner to the single job it was minted for, so it cannot be handed a different
// queued job that merely shares its labels. A provider that does not keeps
// working via token registration, which is why this is a separate interface
// rather than an addition to registrationTokenGetter.
type jitConfigGenerator interface {
	GenerateJITConfig(ctx context.Context, repo string, req github.JITConfigRequest) (string, error)
	ResolveRunnerGroupID(ctx context.Context, repo, groupName string) (*github.RunnerGroupResolution, error)
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

	runnerName := buildRunnerName(req.Pool, repoName, req.Conditions, req.JobID, req.InstanceID)

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

	// Runner logs are keyed by run/job/instance, so the prefix stays flat — a
	// repo segment would break deriving the key from a GitHub job URL alone.
	runnerLogsBucket, runnerLogsPrefix := "", ""
	if m.config.RunnerLogsBucket != "" {
		runnerLogsBucket = m.config.RunnerLogsBucket
		runnerLogsPrefix = logship.DefaultPrefix
	}

	// Prefer a job-bound JIT config; the token stays populated as the fallback so
	// a mint failure costs a steal-able runner rather than the whole dispatch.
	jitConfig := m.mintJITConfig(ctx, req, runnerName)

	// Build runner config with dynamic org from repo
	config := &secrets.RunnerConfig{
		Org:                 org,
		Repo:                req.Repo,
		RunID:               req.RunID,
		CreatedAt:           time.Now().Format(time.RFC3339),
		RegistrationToken:   regResult.Token,
		JITConfig:           jitConfig,
		Labels:              req.Labels,
		RunnerGroup:         m.config.RunnerGroup,
		RunnerName:          runnerName,
		JobID:               req.JobID,
		CacheToken:          cacheToken,
		CacheURL:            m.config.BaseURL,
		TerminationQueueURL: m.config.TerminationQueueURL,
		IsOrg:               regResult.IsOrg,
		BuildkitCacheBucket: buildkitBucket,
		BuildkitCacheRegion: buildkitRegion,
		BuildkitCachePrefix: buildkitPrefix,
		RunnerLogsBucket:    runnerLogsBucket,
		RunnerLogsPrefix:    runnerLogsPrefix,
	}

	runnerLog.Info(ctx, "storing runner config")
	if err := m.secretsStore.Put(ctx, req.InstanceID, config); err != nil {
		return fmt.Errorf("failed to store runner config: %w", err)
	}

	runnerLog.Info(ctx, "runner config stored")
	return nil
}

// mintJITConfig returns an encoded just-in-time runner config, or "" to leave the
// agent on the token path.
//
// Every failure degrades to "" rather than an error: a JIT config is what stops
// another job from stealing this runner, but a runner that never boots is strictly
// worse than a steal-able one. The JIT config itself is never logged — it is a
// credential that registers a runner.
func (m *Manager) mintJITConfig(ctx context.Context, req PrepareRunnerRequest, runnerName string) string {
	generator, ok := m.github.(jitConfigGenerator)
	if !ok {
		return ""
	}

	// Both failure shapes below mean "the requested group did not resolve", so they
	// share one message: alerting greps a single string rather than needing to know
	// which layer failed. The outcome field distinguishes them.
	const groupUnresolved = "runner group unresolved"

	resolution, err := generator.ResolveRunnerGroupID(ctx, req.Repo, m.config.RunnerGroup)
	if err != nil {
		runnerLog.Warn(ctx, groupUnresolved,
			slog.String("runner_group", m.config.RunnerGroup),
			slog.String("outcome", "token_registration"),
			slog.String("error", err.Error()))
		return ""
	}
	// pkg/github cannot log (it is a pure API client), so the Default-group
	// substitution is only visible if this layer reports it. Left silent, a
	// misconfigured group would place every runner in Default unnoticed.
	if resolution.FallbackErr != nil {
		runnerLog.Warn(ctx, groupUnresolved,
			slog.String("runner_group", m.config.RunnerGroup),
			slog.String("outcome", "default_group"),
			slog.Int64("runner_group_id", resolution.ID),
			slog.String("error", resolution.FallbackErr.Error()))
	}

	encoded, err := generator.GenerateJITConfig(ctx, req.Repo, github.JITConfigRequest{
		Name:          runnerName,
		RunnerGroupID: resolution.ID,
		Labels:        req.Labels,
	})
	if err != nil {
		runnerLog.Warn(ctx, "jit config generation failed; falling back to token registration",
			slog.String("error", err.Error()))
		return ""
	}

	runnerLog.Info(ctx, "jit config minted",
		slog.Int64("runner_group_id", resolution.ID))
	return encoded
}

// CleanupRunner deletes the runner configuration from the secrets backend.
func (m *Manager) CleanupRunner(ctx context.Context, instanceID string) error {
	if err := m.secretsStore.Delete(ctx, instanceID); err != nil {
		return fmt.Errorf("failed to delete runner config: %w", err)
	}
	return nil
}

const runnerNameMaxLen = 64

func buildRunnerName(pool, repoName, conditions, jobID, instanceID string) string {
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

	// Job ID suffix distinguishes jobs sharing identical runs-on labels; the
	// instance ID suffix distinguishes duplicate dispatches of the same job —
	// the agent registers with --replace, so two instances sharing one name
	// would evict each other's GitHub registration and fail the job.
	var suffix string
	if jobID != "" {
		jobPart := jobID
		if len(jobPart) > 6 {
			jobPart = jobPart[len(jobPart)-6:]
		}
		suffix = "-" + jobPart
	}
	if instanceID != "" {
		instPart := instanceID
		if len(instPart) > 5 {
			instPart = instPart[len(instPart)-5:]
		}
		suffix += "-" + instPart
	}

	if len(name)+len(suffix) > runnerNameMaxLen {
		name = strings.TrimRight(name[:runnerNameMaxLen-len(suffix)], "-")
	}
	return name + suffix
}
