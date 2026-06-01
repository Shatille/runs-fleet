package termination

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
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

	// The completion-path "processing termination" log carries the full job
	// identity: instance_id + job_id stashed by processMessage, plus run_id + repo
	// enriched from the MarkJobComplete record. run_id comes back as a JSON number.
	proc := findLog(t, &buf, "processing termination")
	if got := proc[logging.KeyInstanceID]; got != instanceID {
		t.Errorf("processing termination: %s = %v, want %s", logging.KeyInstanceID, got, instanceID)
	}
	if got := proc[logging.KeyJobID]; got != jobID {
		t.Errorf("processing termination: %s = %v, want %s", logging.KeyJobID, got, jobID)
	}
	if got := proc[logging.KeyRunID]; got != float64(99) {
		t.Errorf("processing termination: %s = %v, want 99", logging.KeyRunID, got)
	}
	if got := proc[logging.KeyRepo]; got != testRepo {
		t.Errorf("processing termination: %s = %v, want %s", logging.KeyRepo, got, testRepo)
	}
	// job_id must not be emitted twice from re-stashing run_id/repo.
	if c := countLogField(t, &buf, "processing termination", logging.KeyJobID); c != 1 {
		t.Errorf("processing termination: %s emitted %d times, want 1", logging.KeyJobID, c)
	}

	// A log emitted by deleteRunnerConfig must inherit instance_id + job_id from
	// the ctx stashed in processMessage and run_id + repo from the enriched ctx.
	del := findLog(t, &buf, "runner config deleted")
	if got := del[logging.KeyInstanceID]; got != instanceID {
		t.Errorf("runner config deleted: %s = %v, want %s (inherited from ctx)", logging.KeyInstanceID, got, instanceID)
	}
	if got := del[logging.KeyJobID]; got != jobID {
		t.Errorf("runner config deleted: %s = %v, want %s (inherited from ctx)", logging.KeyJobID, got, jobID)
	}
	if got := del[logging.KeyRunID]; got != float64(99) {
		t.Errorf("runner config deleted: %s = %v, want 99 (inherited from ctx)", logging.KeyRunID, got)
	}
	if got := del[logging.KeyRepo]; got != testRepo {
		t.Errorf("runner config deleted: %s = %v, want %s (inherited from ctx)", logging.KeyRepo, got, testRepo)
	}
}

// TestStartedEventDoesNotLogProcessing proves a "started" event is logged as a
// Debug "runner started" line and never as "processing termination", so it does
// not masquerade as a completion being processed.
func TestStartedEventDoesNotLogProcessing(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	handler := NewHandler(&mockQueueAPI{}, &mockDBAPI{}, &mockMetricsAPI{}, &mockSecretsStore{}, &config.Config{})

	msg := Message{InstanceID: "i-started", JobID: "777", Status: "started"}
	body, _ := json.Marshal(msg)

	if err := handler.processMessage(context.Background(), queue.Message{Body: string(body), Handle: "receipt"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if logExists(t, &buf, "processing termination") {
		t.Errorf("started event must not emit 'processing termination'; logs: %s", buf.String())
	}
	started := findLog(t, &buf, "runner started")
	if got := started[logging.KeyInstanceID]; got != "i-started" {
		t.Errorf("runner started: %s = %v, want i-started", logging.KeyInstanceID, got)
	}
	if got := started[logging.KeyJobID]; got != "777" {
		t.Errorf("runner started: %s = %v, want 777", logging.KeyJobID, got)
	}
}

// logExists reports whether any log record with the given msg was emitted.
func logExists(t *testing.T, buf *bytes.Buffer, msg string) bool {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decode log record: %v (buf: %s)", err, buf.String())
		}
		if rec["msg"] == msg {
			return true
		}
	}
	return false
}

// countLogField counts how many times key appears across the JSON-encoded log
// records whose msg matches. A top-level map can hold a key only once, so a
// value > 1 means slog emitted the attr more than once for a single record.
func countLogField(t *testing.T, buf *bytes.Buffer, msg, key string) int {
	t.Helper()
	count := 0
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			t.Fatalf("decode log record: %v (buf: %s)", err, buf.String())
		}
		var rec map[string]any
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("unmarshal log record: %v", err)
		}
		if rec["msg"] != msg {
			continue
		}
		count += strings.Count(string(raw), "\""+key+"\":")
	}
	return count
}
