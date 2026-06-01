package housekeeping

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/queue"
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
// by processMessage reaches logs emitted deep in a task.
type loggingTaskExecutor struct {
	mockTaskExecutor
	log *logging.Logger
}

func (e *loggingTaskExecutor) ExecuteOrphanedInstances(ctx context.Context) error {
	e.log.Info(ctx, "executor reached")
	return errors.New("boom")
}

// TestProcessMessageLogsCarryTask proves that the housekeeping consumer stashes
// the task type onto the context per message, so both a log emitted inside the
// executor and the handler's own "task failed" log inherit it without restating
// the field.
func TestProcessMessageLogsCarryTask(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	executor := &loggingTaskExecutor{log: logging.WithComponent(logging.LogTypeHousekeep, "tasks")}
	handler := NewHandler(&mockQueueAPI{}, executor, &config.Config{})

	body, _ := json.Marshal(Message{TaskType: TaskOrphanedInstances, Timestamp: time.Now()})
	err := handler.processMessage(context.Background(), queue.Message{Body: string(body), Handle: testReceipt})
	if err == nil {
		t.Fatal("expected error from failing executor")
	}

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
