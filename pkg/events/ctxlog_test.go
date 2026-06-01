package events

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

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

// TestSpotInterruptionLogsCarryStashedIdentity proves the events consumer stashes
// instance_id (and job_id once resolved) onto the context so converted logs
// inherit them without restating the fields at each call site.
func TestSpotInterruptionLogsCarryStashedIdentity(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	const instanceID = "i-ctxlog123"

	mockDB := &MockDBAPI{
		MarkInstanceTerminatingFunc: func(_ context.Context, _ string) error { return nil },
		GetJobByInstanceFunc: func(_ context.Context, _ string) (*JobInfo, error) {
			return &JobInfo{JobID: 4242, RunID: 9999, Repo: "octo/repo", Spot: true}, nil
		},
	}
	mockQueue := &MockQueueAPI{
		SendMessageFunc:   func(_ context.Context, _ *queue.JobMessage) error { return nil },
		DeleteMessageFunc: func(_ context.Context, _ string) error { return nil },
	}
	mockMetrics := &MockMetricsAPI{
		PublishSpotInterruptionFunc: func(_ context.Context, _ string) error { return nil },
	}

	handler := &Handler{
		queueClient: mockQueue,
		dbClient:    mockDB,
		metrics:     mockMetrics,
		config:      &config.Config{},
	}
	handler.SetJobQueue(mockQueue)

	body := `{
		"detail-type": "EC2 Spot Instance Interruption Warning",
		"detail": {"instance-id": "` + instanceID + `", "instance-action": "terminate"}
	}`
	handler.processEvent(context.Background(), queue.Message{Body: body, Handle: "receipt"})

	recv := findLog(t, &buf, "spot interruption received")
	if got := recv[logging.KeyInstanceID]; got != instanceID {
		t.Errorf("spot interruption received: %s = %v, want %s", logging.KeyInstanceID, got, instanceID)
	}

	requeued := findLog(t, &buf, "job requeued after spot interruption")
	if got := requeued[logging.KeyInstanceID]; got != instanceID {
		t.Errorf("requeue log: %s = %v, want %s (inherited from ctx)", logging.KeyInstanceID, got, instanceID)
	}
	if got := requeued[logging.KeyJobID]; got != float64(4242) {
		t.Errorf("requeue log: %s = %v, want 4242 (inherited from ctx)", logging.KeyJobID, got)
	}
}
