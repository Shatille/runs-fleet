// Package awsobs provides a smithy-go middleware that times every AWS SDK
// operation end-to-end and emits a structured record when a call is slow or
// fails with a timeout-like error. It is a diagnostic safety net: the
// orchestrator otherwise surfaces a wedged AWS connection only as a terminal
// "context deadline exceeded", with no signal for which service, operation, or
// endpoint hung or for how long. Recording per-operation latency turns that
// opaque cascade into a localized slow/timeout log.
package awsobs

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"
)

// middlewareID is the unique identifier for the observability middleware within
// the Initialize step. The step rejects duplicate IDs.
const middlewareID = "RunsFleetAWSObservability"

// Middlewares returns the API options to register on the shared aws.Config via
// awsconfig.WithAPIOptions, attaching the observability middleware to every AWS
// client built from that config. The provided logger should already carry the
// component/log_type context; when nil, a default awsobs logger is used.
func Middlewares(log *slog.Logger) []func(*middleware.Stack) error {
	return []func(*middleware.Stack) error{register(log, config.AWSSlowCallThreshold)}
}

// register adds the observability middleware to a stack's Initialize step. It is
// the single-function form of Middlewares and is also the unit-test seam for
// injecting a custom threshold.
func register(log *slog.Logger, threshold time.Duration) func(*middleware.Stack) error {
	obsLog := log
	if obsLog == nil {
		obsLog = logging.WithComponent(logging.LogTypeAWS, "observability").Logger
	}
	mw := &observe{log: obsLog, threshold: threshold}
	return func(stack *middleware.Stack) error {
		// Insert at the front of the Initialize step so the recorded elapsed
		// time spans the entire downstream stack (serialize, retry, signing,
		// HTTP round-trip) rather than a partial inner segment.
		return stack.Initialize.Add(mw, middleware.Before)
	}
}

// observe is an Initialize-step middleware that times the downstream handler and
// logs slow or timeout-like calls.
type observe struct {
	log       *slog.Logger
	threshold time.Duration
}

// ID identifies the middleware within the Initialize step.
func (m *observe) ID() string { return middlewareID }

// HandleInitialize times next.HandleInitialize and logs a WARN when the call
// exceeds the slow-call threshold or returns a timeout-like error. Successful
// fast calls are not logged, to avoid flooding logs on the hot path.
func (m *observe) HandleInitialize(
	ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler,
) (middleware.InitializeOutput, middleware.Metadata, error) {
	start := time.Now()
	out, metadata, err := next.HandleInitialize(ctx, in)
	elapsed := time.Since(start)

	slow := elapsed >= m.threshold
	timeout := isTimeout(err)
	if !slow && !timeout {
		return out, metadata, err
	}

	attrs := []any{
		slog.String(logging.KeyService, awsmiddleware.GetServiceID(ctx)),
		slog.String(logging.KeyOperation, awsmiddleware.GetOperationName(ctx)),
		slog.Int64(logging.KeyDuration, elapsed.Milliseconds()),
		slog.Bool("slow", slow),
		slog.Bool("timeout", timeout),
	}
	if err != nil {
		attrs = append(attrs, slog.String(logging.KeyError, err.Error()))
	}
	m.log.Warn("slow or failed AWS SDK call", attrs...)

	return out, metadata, err
}

// isTimeout reports whether err represents a timeout, deadline, or cancellation,
// the failure modes a wedged AWS connection ultimately surfaces as.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}
