package termination

import (
	"bytes"
	"context"
	"encoding/json"
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

// TestProcessMessageLogsCarryStashedIdentity proves the termination consumer
// stashes instance_id and job_id onto the context per message, so converted
// logs at and below processMessage inherit them.
func TestProcessMessageLogsCarryStashedIdentity(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	const (
		instanceID = "i-ctxlog456"
		jobID      = "12345678901"
	)

	handler := NewHandler(&mockQueueAPI{}, &mockDBAPI{}, &mockMetricsAPI{}, &mockSecretsStore{}, &config.Config{})

	msg := Message{
		InstanceID:      instanceID,
		JobID:           jobID,
		Status:          "success",
		DurationSeconds: 60,
		StartedAt:       time.Now().Add(-time.Minute),
		CompletedAt:     time.Now(),
	}
	body, _ := json.Marshal(msg)

	if err := handler.processMessage(context.Background(), queue.Message{Body: string(body), Handle: "receipt"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	proc := findLog(t, &buf, "processing termination")
	if got := proc[logging.KeyInstanceID]; got != instanceID {
		t.Errorf("processing termination: %s = %v, want %s", logging.KeyInstanceID, got, instanceID)
	}
	if got := proc[logging.KeyJobID]; got != jobID {
		t.Errorf("processing termination: %s = %v, want %s", logging.KeyJobID, got, jobID)
	}

	// A log emitted by deleteRunnerConfig must inherit instance_id from the ctx
	// stashed in processMessage without restating it.
	del := findLog(t, &buf, "runner config deleted")
	if got := del[logging.KeyInstanceID]; got != instanceID {
		t.Errorf("runner config deleted: %s = %v, want %s (inherited from ctx)", logging.KeyInstanceID, got, instanceID)
	}
}
