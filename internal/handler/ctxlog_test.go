package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/google/go-github/v57/github"
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

// TestJobEnqueuedLogCarriesRequestedSpec proves the "job enqueued" log surfaces
// the full requested compute spec parsed from the workflow label, so the
// requested shape is correlatable to the instance that ultimately launches.
func TestJobEnqueuedLogCarriesRequestedSpec(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	const label = "runs-fleet=67890/cpu=4+8/ram=8+16/family=c7g+m7g/arch=arm64/gen=7/disk=100/pool=default/spot=false"
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:     github.Int64(12345),
			Name:   github.String("test-job"),
			Labels: []string{label},
		},
		Repo: &github.Repository{
			FullName: github.String("owner/repo"),
		},
	}

	mockQueue := &MockQueue{}
	msg, err := HandleWorkflowJobQueued(context.Background(), event, mockQueue, nil)
	if err != nil {
		t.Fatalf("HandleWorkflowJobQueued() unexpected error: %v", err)
	}
	if msg == nil {
		t.Fatal("HandleWorkflowJobQueued() expected message, got nil")
	}

	rec := findLog(t, &buf, "job enqueued")

	if got := rec["arch"]; got != "arm64" {
		t.Errorf("arch = %v, want arm64", got)
	}
	if got := rec[logging.KeyPoolName]; got != "default" {
		t.Errorf("%s = %v, want default", logging.KeyPoolName, got)
	}
	if got := rec["original_label"]; got != label {
		t.Errorf("original_label = %v, want %q", got, label)
	}
	if got := rec["cpu_min"]; got != float64(4) {
		t.Errorf("cpu_min = %v, want 4", got)
	}
	if got := rec["cpu_max"]; got != float64(8) {
		t.Errorf("cpu_max = %v, want 8", got)
	}
	if got := rec["ram_min"]; got != float64(8) {
		t.Errorf("ram_min = %v, want 8", got)
	}
	if got := rec["ram_max"]; got != float64(16) {
		t.Errorf("ram_max = %v, want 16", got)
	}
	if got := rec["gen"]; got != float64(7) {
		t.Errorf("gen = %v, want 7", got)
	}
	if got := rec["disk"]; got != float64(100) {
		t.Errorf("disk = %v, want 100", got)
	}
	if got := rec["spot"]; got != false {
		t.Errorf("spot = %v, want false", got)
	}
	families, ok := rec["families"].([]any)
	if !ok {
		t.Fatalf("families = %v (%T), want a list", rec["families"], rec["families"])
	}
	if len(families) != 2 || families[0] != "c7g" || families[1] != "m7g" {
		t.Errorf("families = %v, want [c7g m7g]", families)
	}
}
