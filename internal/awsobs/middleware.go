// Package awsobs provides a smithy-go middleware that times every AWS SDK
// operation end-to-end and emits metrics for its latency and failures. It is a
// diagnostic safety net: the orchestrator otherwise surfaces a wedged AWS
// connection only as a terminal "context deadline exceeded", with no signal for
// which service or operation hung or for how long. Recording per-operation
// latency and a timeout/error counter (dimensioned by service and operation)
// turns that opaque cascade into observable metrics, replacing the earlier
// per-call WARN log that flooded the logs on SQS ReceiveMessage long-polls.
package awsobs

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/metrics"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"
)

// Compile-time assertion that observe satisfies the smithy Initialize middleware
// contract.
var _ middleware.InitializeMiddleware = (*observe)(nil)

// middlewareID is the unique identifier for the observability middleware within
// the Initialize step. The step rejects duplicate IDs.
const middlewareID = "RunsFleetAWSObservability"

// registerServiceMetadataID is the SDK middleware
// (github.com/aws/aws-sdk-go-v2/aws/middleware RegisterServiceMetadata) that
// seeds the service ID and operation name into the context. The awsobs
// middlewares anchor after it so their GetServiceID/GetOperationName reads see
// populated values: context changes propagate downward only, so a middleware
// registered ahead of it always observes empty service and operation.
const registerServiceMetadataID = "RegisterServiceMetadata"

// cloudWatchServiceID is the AWS SDK service ID for CloudWatch. The CloudWatch
// metrics backend publishes via PutMetricData on the same shared aws.Config that
// carries this middleware, so timing those calls would emit an AWSCallDuration
// metric whose own publish issues another PutMetricData, amplifying without
// bound. CloudWatch calls are therefore not recorded.
const cloudWatchServiceID = "CloudWatch"

// Failure result classifications kept low-cardinality for the failure counter.
const (
	resultTimeout = "timeout"
	resultError   = "error"
)

// Recorder owns the metrics.Publisher the middleware emits to. The publisher is
// set after the shared aws.Config is built (the CloudWatch backend depends on
// that config), so the middleware reads it through an atomic indirection and
// emits to a no-op until SetPublisher is called.
type Recorder struct {
	publisher atomic.Pointer[metrics.Publisher]
}

// NewRecorder returns a Recorder that emits to a no-op publisher until
// SetPublisher installs a real one.
func NewRecorder() *Recorder {
	r := &Recorder{}
	var p metrics.Publisher = metrics.NoopPublisher{}
	r.publisher.Store(&p)
	return r
}

// SetPublisher installs the publisher the middleware emits to. Safe to call
// after the middleware is already attached to a stack.
func (r *Recorder) SetPublisher(p metrics.Publisher) {
	if p == nil {
		p = metrics.NoopPublisher{}
	}
	r.publisher.Store(&p)
}

func (r *Recorder) get() metrics.Publisher {
	return *r.publisher.Load()
}

// Middlewares returns the API options to register on the shared aws.Config via
// awsconfig.WithAPIOptions, attaching the observability middleware to every AWS
// client built from that config. It also returns the Recorder whose SetPublisher
// must be called once the metrics publisher is constructed; until then the
// middleware emits to a no-op.
func Middlewares() ([]func(*middleware.Stack) error, *Recorder) {
	rec := NewRecorder()
	return []func(*middleware.Stack) error{register(rec)}, rec
}

// register adds the observability middleware to a stack's Initialize step. It is
// the single-function form of Middlewares and the unit-test seam for injecting a
// Recorder.
func register(rec *Recorder) func(*middleware.Stack) error {
	mw := &observe{rec: rec}
	return func(stack *middleware.Stack) error {
		// Anchor directly after RegisterServiceMetadata so GetServiceID and
		// GetOperationName are populated when HandleInitialize reads them, while
		// the recorded elapsed time still spans the entire downstream stack
		// (serialize, retry, signing, HTTP round-trip) since that runs inside
		// next(). The metadata seeder runs at the head of Initialize, so this
		// position remains effectively outermost for timing.
		if _, ok := stack.Initialize.Get(registerServiceMetadataID); ok {
			return stack.Initialize.Insert(mw, registerServiceMetadataID, middleware.After)
		}
		// The anchor is absent (e.g. a bare test stack); fall back to the head of
		// the step so the recorded time still spans everything downstream.
		return stack.Initialize.Add(mw, middleware.Before)
	}
}

// observe is an Initialize-step middleware that times the downstream handler and
// emits latency and failure metrics for the call.
type observe struct {
	rec *Recorder
}

// ID identifies the middleware within the Initialize step.
func (m *observe) ID() string { return middlewareID }

// HandleInitialize times next.HandleInitialize and emits the call's latency as a
// duration metric on every call, plus a failure counter on timeout or error. It
// no longer logs per call: the metric carries the high-frequency signal that
// would otherwise flood the logs (SQS ReceiveMessage long-polls 20s). Worker-
// level failures are logged with task identity at the call site.
func (m *observe) HandleInitialize(
	ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler,
) (middleware.InitializeOutput, middleware.Metadata, error) {
	start := time.Now()
	out, metadata, err := next.HandleInitialize(ctx, in)
	elapsed := time.Since(start)

	service := awsmiddleware.GetServiceID(ctx)
	// CloudWatch publishes via this same config; recording its calls would
	// recurse through the metrics backend.
	if service == cloudWatchServiceID {
		return out, metadata, err
	}

	operation := awsmiddleware.GetOperationName(ctx)
	pub := m.rec.get()
	_ = pub.PublishAWSCallDuration(ctx, service, operation, elapsed.Seconds())

	if result := failureResult(err); result != "" {
		_ = pub.PublishAWSCallFailure(ctx, service, operation, result)
	}

	return out, metadata, err
}

// failureResult classifies err for the failure counter: "timeout" for
// timeout/deadline/cancellation, "error" for any other non-nil error, and ""
// when the call succeeded (no counter increment).
func failureResult(err error) string {
	if err == nil {
		return ""
	}
	if isTimeout(err) {
		return resultTimeout
	}
	return resultError
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
