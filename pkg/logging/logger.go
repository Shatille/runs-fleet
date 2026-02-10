// Package logging provides structured JSON logging using slog.
package logging

import (
	"context"
	golog "log"
	"log/slog"
	"os"
	"slices"
)

// Init configures the default slog logger with JSON output and
// redirects stdlib log to the structured logger.
func Init() {
	hostname := os.Getenv("HOSTNAME")
	if hostname == "" {
		var err error
		hostname, err = os.Hostname()
		if err != nil {
			hostname = "unknown"
		}
	}

	baseHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	handler := baseHandler.WithAttrs([]slog.Attr{
		slog.String(KeyHost, hostname),
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)

	// Redirect stdlib log.Print* calls to slog
	golog.SetOutput(&slogWriter{logger: logger})
	golog.SetFlags(0)
}

// slogWriter redirects stdlib log output to slog with logType=stdlib marker.
type slogWriter struct {
	logger *slog.Logger
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	msg := string(p)
	if len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}
	w.logger.Warn(msg, KeyLogType, "stdlib")
	return len(p), nil
}

// Common attribute keys for consistent field naming.
const (
	KeyAction       = "action"
	KeyAudit        = "audit"
	KeyBackend      = "backend"
	KeyComponent    = "component"
	KeyCount        = "count"
	KeyDuration     = "duration_ms"
	KeyError        = "error"
	KeyHost         = "host"
	KeyInstanceID   = "instance_id"
	KeyInstanceType = "instance_type"
	KeyJobID        = "job_id"
	KeyJobName      = "job_name"
	KeyLogType      = "log_type"
	KeyNamespace    = "namespace"
	KeyOwner        = "owner"
	KeyPoolName     = "pool_name"
	KeyQueueURL     = "queue_url"
	KeyReason       = "reason"
	KeyRemoteAddr   = "remote_addr"
	KeyResult       = "result"
	KeyRepo         = "repo"
	KeyRunID        = "run_id"
	KeyTask         = "task"
	KeyUser         = "user"
	KeyWorkflowName = "workflow_name"
)

// Log types for categorization.
const (
	LogTypeServer      = "server"
	LogTypeWebhook     = "webhook"
	LogTypeQueue       = "queue"
	LogTypePool        = "pool"
	LogTypeHousekeep   = "housekeep"
	LogTypeTermination = "termination"
	LogTypeEvents      = "events"
	LogTypeCache       = "cache"
	LogTypeAdmin       = "admin"
	LogTypeFleet       = "fleet"
	LogTypeCircuit     = "circuit"
	LogTypeRunner      = "runner"
	LogTypeCost        = "cost"
	LogTypeK8s         = "k8s"
	LogTypeMetrics     = "metrics"
	LogTypeDB          = "db"
	LogTypeAgent       = "agent"
)

// Logger wraps slog.Logger with convenience methods.
// It uses a lazyHandler so that package-level loggers created before Init()
// pick up the JSON handler configured later.
type Logger struct {
	*slog.Logger
}

// New creates a new Logger with the given attributes.
// The returned logger resolves slog.Default() at log time, not at creation time.
func New(attrs ...any) *Logger {
	h := &lazyHandler{preAttrs: argsToAttrs(attrs)}
	return &Logger{Logger: slog.New(h)}
}

// With returns a new Logger with additional attributes.
func (l *Logger) With(attrs ...any) *Logger {
	return &Logger{Logger: l.Logger.With(attrs...)}
}

// WithComponent returns a new Logger with the component and log_type attributes set.
func WithComponent(logType, component string) *Logger {
	return New(KeyLogType, logType, KeyComponent, component)
}

// lazyHandler delegates to slog.Default().Handler() at log time,
// allowing package-level loggers created before Init() to use
// the JSON handler configured later.
type lazyHandler struct {
	preAttrs []slog.Attr
	groups   []string
}

func (h *lazyHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return slog.Default().Handler().Enabled(ctx, level)
}

func (h *lazyHandler) resolve() slog.Handler {
	handler := slog.Default().Handler()
	if len(h.preAttrs) > 0 {
		handler = handler.WithAttrs(h.preAttrs)
	}
	for _, g := range h.groups {
		handler = handler.WithGroup(g)
	}
	return handler
}

func (h *lazyHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.resolve().Handle(ctx, r)
}

func (h *lazyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &lazyHandler{
		preAttrs: append(slices.Clone(h.preAttrs), attrs...),
		groups:   slices.Clone(h.groups),
	}
}

func (h *lazyHandler) WithGroup(name string) slog.Handler {
	return &lazyHandler{
		preAttrs: slices.Clone(h.preAttrs),
		groups:   append(slices.Clone(h.groups), name),
	}
}

// argsToAttrs converts slog-style key-value args to []slog.Attr.
func argsToAttrs(args []any) []slog.Attr {
	var attrs []slog.Attr
	for i := 0; i < len(args); {
		if attr, ok := args[i].(slog.Attr); ok {
			attrs = append(attrs, attr)
			i++
			continue
		}
		if i+1 < len(args) {
			if key, ok := args[i].(string); ok {
				attrs = append(attrs, slog.Any(key, args[i+1]))
			}
			i += 2
		} else {
			i++
		}
	}
	return attrs
}
