package housekeeping

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// runnerNamePrefix marks a registration this fleet minted. Ownership cannot be
// inferred any other way from the runners API, so anything without it belongs to
// another system and is never touched.
const runnerNamePrefix = "runs-fleet-runner-"

// maxRunnerDeregistrations bounds the deletes one cycle issues. A large backlog
// drains across cycles instead of spending the whole GitHub API budget at once.
const maxRunnerDeregistrations = 200

// RegisteredRunner is a self-hosted runner registration.
type RegisteredRunner struct {
	ID     int64
	Name   string
	Status string
	Busy   bool
}

// RunnerRegistry lists and removes self-hosted runner registrations.
type RunnerRegistry interface {
	ListRunners(ctx context.Context, repo string) ([]RegisteredRunner, error)
	DeleteRunner(ctx context.Context, repo string, runnerID int64) error
}

// minOfflineAge is how long a registration must have been continuously offline
// before it is removed.
//
// A registration exists from the moment its JIT config is minted — before the
// instance boots — so a live runner reads offline for its whole startup, and an
// agent may sit in standby for up to RUNS_FLEET_STANDBY_DEADLINE_MINUTES (2h by
// default) before it ever registers a job. This window clears both with room to
// spare, so anything past it has outlived every path that could explain it.
const minOfflineAge = 6 * time.Hour

// RunnerSightingStore records when a registration was first seen offline. It is
// durable rather than in-process because the orchestrator runs multiple replicas
// and the housekeeping task lock only serializes one tick: consecutive sweeps
// are usually run by different replicas, so a per-process counter would never
// accumulate and the sweep would never delete anything.
type RunnerSightingStore interface {
	RecordRunnerOffline(ctx context.Context, repo string, runnerID int64, now time.Time) (time.Duration, error)
	ForgetRunnerOffline(ctx context.Context, repo string, runnerID int64) error
}

// ActiveReposFunc returns the repos worth sweeping.
type ActiveReposFunc func(ctx context.Context) ([]string, error)

// SetRunnerRegistry wires the orphaned-runner sweep. When any dependency is
// unset the sweep is a no-op, so it can never delete on an unrecorded age.
func (t *Tasks) SetRunnerRegistry(r RunnerRegistry, repos ActiveReposFunc, sightings RunnerSightingStore) {
	t.runnerRegistry = r
	t.activeRepos = repos
	t.sightings = sightings
}

// ExecuteOrphanedRunners deletes runner registrations left behind by runners
// that never ran a job.
//
// GitHub removes an ephemeral runner only after it *completes* work, so a runner
// whose instance was terminated first — the unconfirmed-runner watchdog kills at
// 5 minutes and requeues, minting a fresh registration each time — stays
// registered forever. Observed at 360 of 369 registrations in one repo.
//
// Only offline, non-busy registrations bearing this fleet's name prefix are
// removed: online means a live instance is waiting for or running work, and busy
// means a job is executing right now.
func (t *Tasks) ExecuteOrphanedRunners(ctx context.Context) error {
	if t.runnerRegistry == nil || t.activeRepos == nil || t.sightings == nil {
		return nil
	}

	repos, err := t.activeRepos(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	deleted := 0
	for _, repo := range repos {
		runners, err := t.runnerRegistry.ListRunners(ctx, repo)
		if err != nil {
			// Best-effort cleanup: one unreachable repo must not stop the rest,
			// and the backlog is drained again next cycle.
			t.logger().Warn(ctx, "runner listing failed; skipping repo",
				slog.String("repo", repo),
				slog.String("error", err.Error()))
			continue
		}

		for _, r := range runners {
			if !strings.HasPrefix(r.Name, runnerNamePrefix) {
				continue
			}
			if r.Status != "offline" || r.Busy {
				// Alive: drop any sighting so a later offline reading has to
				// serve its own full window rather than inheriting a stale one.
				if err := t.sightings.ForgetRunnerOffline(ctx, repo, r.ID); err != nil {
					t.logger().Warn(ctx, "clearing runner sighting failed",
						slog.String("repo", repo),
						slog.String("error", err.Error()))
				}
				continue
			}

			age, err := t.sightings.RecordRunnerOffline(ctx, repo, r.ID, now)
			if err != nil {
				// Without a trustworthy age this runner cannot be judged, and
				// deleting on an unknown age is the one mistake that costs a job.
				t.logger().Warn(ctx, "recording runner sighting failed; leaving registration",
					slog.String("repo", repo),
					slog.String("error", err.Error()))
				continue
			}
			if age < minOfflineAge || deleted >= maxRunnerDeregistrations {
				continue
			}

			if err := t.runnerRegistry.DeleteRunner(ctx, repo, r.ID); err != nil {
				t.logger().Warn(ctx, "runner deregistration failed",
					slog.String("repo", repo),
					slog.String("runner_name", r.Name),
					slog.String("error", err.Error()))
				continue
			}
			if err := t.sightings.ForgetRunnerOffline(ctx, repo, r.ID); err != nil {
				t.logger().Warn(ctx, "clearing runner sighting failed after delete",
					slog.String("repo", repo),
					slog.String("error", err.Error()))
			}
			deleted++
		}
	}

	if deleted > 0 {
		t.logger().Info(ctx, "deregistered orphaned runners", slog.Int(logging.KeyCount, deleted))
	} else {
		t.logger().Debug(ctx, "no orphaned runner registrations to remove")
	}
	return nil
}
