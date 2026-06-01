package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

// newCapturingDefault sets slog.Default() to a contextHandler-wrapped JSON
// handler writing into buf, mirroring how Init() builds the chain, and restores
// a quiet default on cleanup.
func newCapturingDefault(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	inner := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(newContextHandler(inner)))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	})
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected JSON log output, got: %s (err: %v)", buf.String(), err)
	}
	return entry
}

func TestContextWith_EmittedViaContextMethod(t *testing.T) {
	var buf bytes.Buffer
	newCapturingDefault(t, &buf)

	ctx := ContextWithJob(context.Background(), 42, 99, "octo/repo")
	logger := WithComponent(LogTypeQueue, "worker")
	logger.Error(ctx, "boom", slog.String("error", "deadline"))

	entry := decode(t, &buf)
	if got := entry[KeyJobID]; got != float64(42) {
		t.Errorf("job_id = %v, want 42", got)
	}
	if got := entry[KeyRunID]; got != float64(99) {
		t.Errorf("run_id = %v, want 99", got)
	}
	if got := entry[KeyRepo]; got != "octo/repo" {
		t.Errorf("repo = %v, want octo/repo", got)
	}
	if got := entry["error"]; got != "deadline" {
		t.Errorf("error = %v, want deadline", got)
	}
}

// TestContextWith_InjectedOnEveryLevel proves every Logger level (Error/Warn/
// Info/Debug) injects ctx-stashed identity, since each requires a context and
// delegates to the underlying *Context slog method. There is no non-context
// method that could bypass the handler.
func TestContextWith_InjectedOnEveryLevel(t *testing.T) {
	ctx := ContextWithJob(context.Background(), 42, 99, "octo/repo")
	logger := WithComponent(LogTypeQueue, "worker")

	cases := []struct {
		name string
		emit func()
	}{
		{"error", func() { logger.Error(ctx, "boom") }},
		{"warn", func() { logger.Warn(ctx, "boom") }},
		{"info", func() { logger.Info(ctx, "boom") }},
		{"debug", func() { logger.Debug(ctx, "boom") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			newCapturingDefault(t, &buf)
			tc.emit()
			entry := decode(t, &buf)
			if got := entry[KeyJobID]; got != float64(42) {
				t.Errorf("job_id = %v, want 42 (ctx identity must inject on %s)", got, tc.name)
			}
			if got := entry[KeyRunID]; got != float64(99) {
				t.Errorf("run_id = %v, want 99", got)
			}
		})
	}
}

func TestContextWith_Merges(t *testing.T) {
	var buf bytes.Buffer
	newCapturingDefault(t, &buf)

	ctx := ContextWith(context.Background(), slog.Int64(KeyRunID, 7))
	ctx = ContextWith(ctx, slog.String(KeyRepo, "octo/repo"))

	logger := WithComponent(LogTypeFleet, "manager")
	logger.Warn(ctx, "circuit breaker check failed")

	entry := decode(t, &buf)
	if got := entry[KeyRunID]; got != float64(7) {
		t.Errorf("run_id = %v, want 7", got)
	}
	if got := entry[KeyRepo]; got != "octo/repo" {
		t.Errorf("repo = %v, want octo/repo", got)
	}
}

func TestContextWithJob_OmitsZeroValues(t *testing.T) {
	var buf bytes.Buffer
	newCapturingDefault(t, &buf)

	ctx := ContextWithJob(context.Background(), 0, 99, "")
	WithComponent(LogTypeQueue, "worker").Info(ctx, "partial identity")

	entry := decode(t, &buf)
	if _, present := entry[KeyJobID]; present {
		t.Errorf("job_id should be omitted when zero, got %v", entry[KeyJobID])
	}
	if got := entry[KeyRunID]; got != float64(99) {
		t.Errorf("run_id = %v, want 99", got)
	}
	if _, present := entry[KeyRepo]; present {
		t.Errorf("repo should be omitted when empty, got %v", entry[KeyRepo])
	}
}

// TestContextWith_DeepCallSite asserts identity stashed at an entry point reaches
// a log emitted by a deep function that adds no manual identity fields itself.
func TestContextWith_DeepCallSite(t *testing.T) {
	var buf bytes.Buffer
	newCapturingDefault(t, &buf)

	deepLog := WithComponent(LogTypeFleet, "manager")
	deep := func(ctx context.Context) {
		// No job_id/run_id/repo added here; they must come from the context.
		deepLog.Warn(ctx, "circuit breaker check failed", slog.String("error", "deadline exceeded"))
	}

	ctx := ContextWithJob(context.Background(), 1234, 5678, "octo/deep")
	deep(ctx)

	entry := decode(t, &buf)
	if got := entry[KeyJobID]; got != float64(1234) {
		t.Errorf("job_id = %v, want 1234 (deep log must inherit ctx identity)", got)
	}
	if got := entry[KeyRunID]; got != float64(5678) {
		t.Errorf("run_id = %v, want 5678", got)
	}
	if got := entry[KeyRepo]; got != "octo/deep" {
		t.Errorf("repo = %v, want octo/deep", got)
	}
}

func TestContextHandler_WithAttrsAndGroupDelegate(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := newContextHandler(inner)

	// WithAttrs: the attr must appear on emitted records.
	withAttrs := h.WithAttrs([]slog.Attr{slog.String("static", "v")})
	logger := slog.New(withAttrs)
	ctx := ContextWith(context.Background(), slog.Int64(KeyJobID, 5))
	logger.ErrorContext(ctx, "msg")

	entry := decode(t, &buf)
	if got := entry["static"]; got != "v" {
		t.Errorf("WithAttrs static attr = %v, want v", got)
	}
	if got := entry[KeyJobID]; got != float64(5) {
		t.Errorf("ctx job_id = %v, want 5 (context attrs must still flow after WithAttrs)", got)
	}

	// WithGroup: subsequent attrs nest under the group name.
	buf.Reset()
	withGroup := newContextHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})).WithGroup("g")
	slog.New(withGroup).Error("grouped", slog.String("k", "x"))
	entry = decode(t, &buf)
	grp, ok := entry["g"].(map[string]any)
	if !ok {
		t.Fatalf("expected group object under key g, got: %s", buf.String())
	}
	if got := grp["k"]; got != "x" {
		t.Errorf("grouped attr k = %v, want x", got)
	}
}
