package housekeeping

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/logging"
)

func captureCtxLogs(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	inner := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(logging.NewContextHandler(inner)))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	})
}

func findLog(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decode log record: %v (buf: %s)", err, buf.String())
		}
		if rec["msg"] == msg {
			return rec
		}
	}
	t.Fatalf("no log record with msg %q found in: %s", msg, buf.String())
	return nil
}

// loggingTaskExecutor emits a log from within an executor using the same
// component logger as production code, proving that the task identity stashed
// by the runner reaches logs emitted deep in a task.
type loggingTaskExecutor struct {
	mockTaskExecutor
	log *logging.Logger
}

func (e *loggingTaskExecutor) ExecuteOrphanedInstances(ctx context.Context) error {
	e.log.Info(ctx, "executor reached")
	return errors.New("boom")
}

// TestRunnerLogsCarryTask proves the runner stashes the task type onto the
// context per task, so both a log emitted inside the executor and the runner's
// own "task failed" log inherit it without restating the field.
func TestRunnerLogsCarryTask(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	executor := &loggingTaskExecutor{log: logging.WithComponent(logging.LogTypeHousekeep, "tasks")}
	r := NewRunner(executor, DefaultSchedulerConfig())

	r.tryRunTask(context.Background(), findSpec(t, r, TaskOrphanedInstances))

	want := string(TaskOrphanedInstances)

	reached := findLog(t, &buf, "executor reached")
	if got := reached[logging.KeyTask]; got != want {
		t.Errorf("executor log: %s = %v, want %s (inherited from ctx)", logging.KeyTask, got, want)
	}

	failed := findLog(t, &buf, "task failed")
	if got := failed[logging.KeyTask]; got != want {
		t.Errorf("task failed log: %s = %v, want %s (inherited from ctx)", logging.KeyTask, got, want)
	}
}
