package logging

import (
	"context"
	"log/slog"
	"slices"
)

// ctxKey is the private context key under which task-identity attrs are stashed.
type ctxKey struct{}

// ContextWith returns a child context carrying attrs for later log enrichment.
// Attrs accumulate: any attrs already stashed on ctx are preserved and the new
// ones appended. The stashed attrs are emitted whenever a log is produced through
// a *logging.Logger, because every Logger method (Error, Warn, Info, Debug)
// requires a context and delegates to the underlying *Context slog method, which
// passes ctx to the contextHandler. Stash a key at most once per context branch:
// slog does not de-duplicate, so re-stashing a key already present emits it twice.
func ContextWith(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	existing := attrsFromContext(ctx)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, ctxKey{}, merged)
}

// ContextWithJob stashes the standard task-identity fields (job_id, run_id, repo)
// on ctx using the canonical key constants. Zero-valued fields are omitted so a
// partially known identity (e.g. run_id before job_id is resolved) still works.
func ContextWithJob(ctx context.Context, jobID, runID int64, repo string) context.Context {
	attrs := make([]slog.Attr, 0, 3)
	if jobID != 0 {
		attrs = append(attrs, slog.Int64(KeyJobID, jobID))
	}
	if runID != 0 {
		attrs = append(attrs, slog.Int64(KeyRunID, runID))
	}
	if repo != "" {
		attrs = append(attrs, slog.String(KeyRepo, repo))
	}
	return ContextWith(ctx, attrs...)
}

// attrsFromContext returns the attrs stashed on ctx, or nil if none.
func attrsFromContext(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	return attrs
}

// contextHandler wraps another slog.Handler and, on Handle, injects any
// task-identity attrs stashed on the context via ContextWith before delegating.
type contextHandler struct {
	inner slog.Handler
}

// NewContextHandler wraps inner so that attrs stashed on the context via
// ContextWith are injected into every record on the *Context log methods. Init()
// uses this to wrap the JSON handler; callers wiring a custom slog handler (e.g.
// to a buffer) can use it to get the same context propagation.
func NewContextHandler(inner slog.Handler) slog.Handler {
	return newContextHandler(inner)
}

// newContextHandler wraps inner so that context-stashed attrs are added to every
// record passed to Handle.
func newContextHandler(inner slog.Handler) slog.Handler {
	return &contextHandler{inner: inner}
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, rec slog.Record) error {
	if attrs := attrsFromContext(ctx); len(attrs) > 0 {
		rec = rec.Clone()
		rec.AddAttrs(attrs...)
	}
	return h.inner.Handle(ctx, rec)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{inner: h.inner.WithAttrs(slices.Clone(attrs))}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{inner: h.inner.WithGroup(name)}
}
