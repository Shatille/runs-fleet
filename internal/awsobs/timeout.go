package awsobs

import (
	"context"
	"time"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"
)

// timeoutMiddlewareID is the unique identifier for the per-operation timeout
// middleware within the Initialize step. The step rejects duplicate IDs.
const timeoutMiddlewareID = "RunsFleetAWSPerOpTimeout"

// PerOperationTimeout returns an API option that bounds every AWS SDK operation
// to d via a derived sub-context, so one slow or wedged call cannot consume the
// whole per-message budget and cascade context-deadline errors across the rest
// of a job. Operations whose name is present in exempt keep the inbound context
// unchanged. When d <= 0 the middleware is a pass-through.
//
// The exemption exists for SQS ReceiveMessage: it long-polls with
// WaitTimeSeconds=20, which is longer than d, so applying the timeout would
// abort every empty poll. The exemption matches on GetOperationName, so the
// middleware must run after RegisterServiceMetadata has populated it; otherwise
// the empty name never matches and ReceiveMessage is wrongly bounded.
//
// The middleware is inserted immediately inside the observability middleware so
// observability stays outermost and records the full operation duration,
// including a per-operation-timeout abort. Observability itself anchors after
// RegisterServiceMetadata, so this position is transitively after it too.
func PerOperationTimeout(d time.Duration, exempt map[string]bool) func(*middleware.Stack) error {
	mw := &perOpTimeout{timeout: d, exempt: exempt}
	return func(stack *middleware.Stack) error {
		// Insert directly after the observability middleware so observability
		// remains the outermost Initialize handler (it times the full call,
		// including a timeout abort) and the bounded sub-context still wraps the
		// entire downstream stack (serialize, retry, signing, HTTP round-trip).
		if _, ok := stack.Initialize.Get(middlewareID); ok {
			return stack.Initialize.Insert(mw, middlewareID, middleware.After)
		}
		// Observability is absent; anchor after RegisterServiceMetadata so the
		// exemption still sees a populated operation name.
		if _, ok := stack.Initialize.Get(registerServiceMetadataID); ok {
			return stack.Initialize.Insert(mw, registerServiceMetadataID, middleware.After)
		}
		// Neither anchor is present (e.g. a bare test stack); fall back to the
		// head of the step so the bounded sub-context still wraps everything
		// downstream.
		return stack.Initialize.Add(mw, middleware.Before)
	}
}

// perOpTimeout is an Initialize-step middleware that derives a bounded
// sub-context per AWS operation, skipping exempt operations.
type perOpTimeout struct {
	timeout time.Duration
	exempt  map[string]bool
}

// ID identifies the middleware within the Initialize step.
func (m *perOpTimeout) ID() string { return timeoutMiddlewareID }

// HandleInitialize bounds the operation to m.timeout unless the operation is
// exempt or the timeout is non-positive.
func (m *perOpTimeout) HandleInitialize(
	ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler,
) (middleware.InitializeOutput, middleware.Metadata, error) {
	if m.timeout > 0 && !m.exempt[awsmiddleware.GetOperationName(ctx)] {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.timeout)
		defer cancel()
	}
	return next.HandleInitialize(ctx, in)
}
