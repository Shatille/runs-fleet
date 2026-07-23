package agent

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

// BuildCache outcome values, mirroring cmd/agent's cacheInterception* discipline.
const (
	BuildCacheEngaged  = "engaged"
	BuildCacheSkipped  = "skipped"
	BuildCacheFailed   = "failed"
	BuildCacheDisabled = "disabled"
)

// buildCacheOutcomeFile is the runner-dir file the buildx shim appends its
// per-invocation outcome to. The agent tells the shim about it via the
// RUNS_FLEET_BUILDKIT_CACHE_OUTCOME env var and reads it before termination.
const buildCacheOutcomeFile = "_rf_buildkit_cache_outcome"

// WriteBuildkitCacheEnv appends the transparent Docker layer-cache env vars to
// the runner .env when the config carries a cache bucket, and returns the
// outcome-file path the shim will write to (empty when the feature is
// disabled). It is best-effort like the rest of the .env plumbing: a write
// failure is not fatal (an empty return simply means no outcome to read later).
func (r *Registrar) WriteBuildkitCacheEnv(runnerPath string, cfg *secrets.RunnerConfig) string {
	if cfg == nil || cfg.BuildkitCacheBucket == "" {
		return ""
	}
	outcomeFile := filepath.Join(runnerPath, buildCacheOutcomeFile)
	entries := [][2]string{
		{"RUNS_FLEET_BUILDKIT_CACHE_BUCKET", cfg.BuildkitCacheBucket},
		{"RUNS_FLEET_BUILDKIT_CACHE_REGION", cfg.BuildkitCacheRegion},
		{"RUNS_FLEET_BUILDKIT_CACHE_PREFIX", cfg.BuildkitCachePrefix},
		{"RUNS_FLEET_BUILDKIT_CACHE_OUTCOME", outcomeFile},
	}
	for _, e := range entries {
		if err := r.AppendRunnerEnv(runnerPath, e[0], e[1]); err != nil {
			r.logger.Printf("Warning: failed to write %s to .env: %v", e[0], err)
			return ""
		}
	}
	return outcomeFile
}

// ReadBuildCacheOutcome returns the coarse outcome (engaged|skipped|failed|
// disabled) recorded by the shim for this job, reading the last non-empty line
// of the outcome file. An empty path or a missing/unreadable file — the shim
// was never invoked (no docker build ran) — reports "disabled". The shim writes
// finer detail after a colon (e.g. "skipped:no-cache-builder"); only the prefix
// is kept so the metric stays low-cardinality.
func ReadBuildCacheOutcome(outcomeFile string) string {
	if outcomeFile == "" {
		return BuildCacheDisabled
	}
	b, err := os.ReadFile(outcomeFile)
	if err != nil {
		return BuildCacheDisabled
	}
	last := lastNonEmptyLine(string(b))
	if last == "" {
		return BuildCacheDisabled
	}
	if idx := strings.IndexByte(last, ':'); idx >= 0 {
		return last[:idx]
	}
	return last
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
