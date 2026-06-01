package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
)

func captureCtxLogs(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	inner := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(logging.NewContextHandler(inner)))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	})
}

func recordsWithMsg(t *testing.T, buf *bytes.Buffer, msg string) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestWarmPoolAssignLogsCarryIdentityNoDuplicate proves that the warm-pool
// assigner stashes instance_id once on the context, and that passing that
// context into PrepareRunner does not produce a duplicate instance_id key
// (PrepareRunner no longer re-stashes it). job_id is supplied by the upstream
// consumer's ContextWithJob.
func TestWarmPoolAssignLogsCarryIdentityNoDuplicate(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	const instanceID = "i-ctxlog789"

	runnerLog := logging.WithComponent(logging.LogTypeRunner, "manager")

	assigner := &WarmPoolAssigner{
		Pool: &mockPoolManager{
			claimAndStartFunc: func(_ context.Context, _ string, _ int64, _ string, _ *fleet.FlexibleSpec) (*pools.AvailableInstance, error) {
				return &pools.AvailableInstance{InstanceID: instanceID, InstanceType: "c7g.xlarge"}, nil
			},
		},
		Runner: &mockRunnerPreparer{
			prepareFunc: func(ctx context.Context, _ runner.PrepareRunnerRequest) error {
				// Emit a log on the ctx PrepareRunner received, like the real one.
				runnerLog.Info(ctx, "storing runner config")
				return nil
			},
		},
		DB: &mockJobDBClient{hasJobsTable: true},
	}

	// Upstream consumer stashes job/run/repo before the warm-pool path runs.
	ctx := logging.ContextWithJob(context.Background(), 4242, 9999, "octo/repo")
	job := &queue.JobMessage{JobID: 4242, RunID: 9999, Repo: "octo/repo", Pool: "default"}

	res, err := assigner.TryAssignToWarmPool(ctx, job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || !res.Assigned {
		t.Fatalf("expected assignment, got %+v", res)
	}

	// "instance claimed" carries instance_id (stashed) + job_id (upstream).
	claimed := recordsWithMsg(t, &buf, "instance claimed")
	if len(claimed) != 1 {
		t.Fatalf("expected 1 'instance claimed' log, got %d", len(claimed))
	}
	var rec map[string]any
	_ = json.Unmarshal([]byte(claimed[0]), &rec)
	if rec[logging.KeyInstanceID] != instanceID {
		t.Errorf("instance claimed: %s = %v, want %s", logging.KeyInstanceID, rec[logging.KeyInstanceID], instanceID)
	}
	if rec[logging.KeyJobID] != float64(4242) {
		t.Errorf("instance claimed: %s = %v, want 4242 (from upstream ctx)", logging.KeyJobID, rec[logging.KeyJobID])
	}

	// The log emitted from inside PrepareRunner must carry exactly one
	// instance_id key (regression guard against double-stash).
	stored := recordsWithMsg(t, &buf, "storing runner config")
	if len(stored) != 1 {
		t.Fatalf("expected 1 'storing runner config' log, got %d", len(stored))
	}
	if n := strings.Count(stored[0], `"`+logging.KeyInstanceID+`"`); n != 1 {
		t.Errorf("PrepareRunner log has %d instance_id keys, want exactly 1: %s", n, stored[0])
	}
}

// TestPrepareRunnersLogsCarryInstanceID proves that the direct/cold-start path
// (PrepareRunners) stashes instance_id onto the per-instance context, so the
// runner manager's config logs (fetching token / storing config / config
// stored) carry instance_id on every path, not just the warm-pool path.
func TestPrepareRunnersLogsCarryInstanceID(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	const instanceID = "i-coldstart42"

	runnerLog := logging.WithComponent(logging.LogTypeRunner, "manager")

	preparer := &mockRunnerPreparer{
		prepareFunc: func(ctx context.Context, _ runner.PrepareRunnerRequest) error {
			runnerLog.Info(ctx, "storing runner config")
			return nil
		},
	}

	// Upstream consumer stashes job/run/repo before the cold-start path runs.
	ctx := logging.ContextWithJob(context.Background(), 4242, 9999, "octo/repo")
	job := &queue.JobMessage{JobID: 4242, RunID: 9999, Repo: "octo/repo"}

	failed := PrepareRunners(ctx, preparer, job, []string{instanceID})
	if len(failed) != 0 {
		t.Fatalf("PrepareRunners() failed = %v, want none", failed)
	}

	stored := recordsWithMsg(t, &buf, "storing runner config")
	if len(stored) != 1 {
		t.Fatalf("expected 1 'storing runner config' log, got %d", len(stored))
	}
	var rec map[string]any
	_ = json.Unmarshal([]byte(stored[0]), &rec)
	if rec[logging.KeyInstanceID] != instanceID {
		t.Errorf("storing runner config: %s = %v, want %s", logging.KeyInstanceID, rec[logging.KeyInstanceID], instanceID)
	}
	if n := strings.Count(stored[0], `"`+logging.KeyInstanceID+`"`); n != 1 {
		t.Errorf("log has %d instance_id keys, want exactly 1: %s", n, stored[0])
	}
}
