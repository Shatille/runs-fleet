package awsobs

import (
	"context"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"
)

// runTimeoutStack builds an Initialize-step stack with the per-operation timeout
// middleware (at the given timeout and exemptions) plus a RegisterServiceMetadata
// seeder for operationName and a terminal handler that captures the deadline the
// timeout middleware imposes on the downstream context.
func runTimeoutStack(
	t *testing.T, timeout time.Duration, exempt map[string]bool, operationName string,
) (deadline time.Time, hasDeadline bool) {
	t.Helper()

	stack := middleware.NewStack("test", smithyRequestNew)
	if err := PerOperationTimeout(timeout, exempt)(stack); err != nil {
		t.Fatalf("register per-op timeout middleware: %v", err)
	}
	// Seed the operation name into the context. Added last with Before, it sits
	// at the head of the step and runs before the timeout middleware reads it.
	if err := stack.Initialize.Add(&awsmiddleware.RegisterServiceMetadata{
		ServiceID:     "SQS",
		OperationName: operationName,
	}, middleware.Before); err != nil {
		t.Fatalf("register metadata middleware: %v", err)
	}

	terminal := middleware.HandlerFunc(func(ctx context.Context, _ any) (any, middleware.Metadata, error) {
		deadline, hasDeadline = ctx.Deadline()
		return nil, middleware.Metadata{}, nil
	})

	if _, _, err := stack.HandleMiddleware(context.Background(), nil, terminal); err != nil {
		t.Fatalf("HandleMiddleware: %v", err)
	}
	return deadline, hasDeadline
}

func TestPerOperationTimeout_NormalOperationBounded(t *testing.T) {
	t.Parallel()

	start := time.Now()
	deadline, hasDeadline := runTimeoutStack(t, config.AWSPerOpTimeout, nil, "GetItem")

	if !hasDeadline {
		t.Fatal("expected the downstream context to carry a deadline for a normal operation")
	}
	remaining := time.Until(deadline)
	// The deadline must be ~AWSPerOpTimeout out; allow generous slack for the
	// elapsed time spent invoking the stack on a slow CI host.
	if remaining <= 0 || remaining > config.AWSPerOpTimeout {
		t.Errorf("deadline remaining = %v, want in (0, %v]", remaining, config.AWSPerOpTimeout)
	}
	if elapsed := time.Since(start); deadline.Before(start.Add(config.AWSPerOpTimeout - elapsed - time.Second)) {
		t.Errorf("deadline %v is too early relative to AWSPerOpTimeout %v", deadline, config.AWSPerOpTimeout)
	}
}

func TestPerOperationTimeout_ReceiveMessageExempt(t *testing.T) {
	t.Parallel()

	_, hasDeadline := runTimeoutStack(t, config.AWSPerOpTimeout, map[string]bool{"ReceiveMessage": true}, "ReceiveMessage")

	if hasDeadline {
		t.Fatal("ReceiveMessage must be exempt: no per-operation deadline may be imposed (would break the 20s long-poll)")
	}
}

func TestPerOperationTimeout_NonExemptSQSOpBounded(t *testing.T) {
	t.Parallel()

	// SendMessage/DeleteMessage are fast and not exempt, so they are bounded
	// even on the SQS client.
	_, hasDeadline := runTimeoutStack(t, config.AWSPerOpTimeout, map[string]bool{"ReceiveMessage": true}, "SendMessage")

	if !hasDeadline {
		t.Fatal("non-exempt SQS operations (e.g. SendMessage) must be bounded by the per-operation timeout")
	}
}

func TestPerOperationTimeout_ZeroDurationIsPassThrough(t *testing.T) {
	t.Parallel()

	_, hasDeadline := runTimeoutStack(t, 0, nil, "GetItem")

	if hasDeadline {
		t.Fatal("a non-positive timeout must be a pass-through and impose no deadline")
	}
}

func TestPerOperationTimeout_RegistersOnStack(t *testing.T) {
	t.Parallel()

	stack := middleware.NewStack("test", smithyRequestNew)
	if err := PerOperationTimeout(config.AWSPerOpTimeout, nil)(stack); err != nil {
		t.Fatalf("apply per-op timeout option: %v", err)
	}
	if _, ok := stack.Initialize.Get(timeoutMiddlewareID); !ok {
		t.Fatalf("middleware %q not registered on Initialize step", timeoutMiddlewareID)
	}
}

// TestPerOperationTimeout_ObservabilityStaysOutermost asserts that when both the
// observability and timeout middlewares are registered (observability first, as
// in main.go), observability remains at the head of the Initialize order so it
// times the full call including a per-operation-timeout abort.
func TestPerOperationTimeout_ObservabilityStaysOutermost(t *testing.T) {
	t.Parallel()

	stack := middleware.NewStack("test", smithyRequestNew)
	for _, apply := range Middlewares(nil) {
		if err := apply(stack); err != nil {
			t.Fatalf("apply observability middleware: %v", err)
		}
	}
	if err := PerOperationTimeout(config.AWSPerOpTimeout, nil)(stack); err != nil {
		t.Fatalf("apply per-op timeout middleware: %v", err)
	}

	ids := stack.Initialize.List()
	obsIdx, timeoutIdx := -1, -1
	for i, id := range ids {
		switch id {
		case middlewareID:
			obsIdx = i
		case timeoutMiddlewareID:
			timeoutIdx = i
		}
	}
	if obsIdx == -1 || timeoutIdx == -1 {
		t.Fatalf("expected both middlewares registered; order = %v", ids)
	}
	if obsIdx > timeoutIdx {
		t.Errorf("observability (idx %d) must run before/outside the timeout (idx %d); order = %v",
			obsIdx, timeoutIdx, ids)
	}
}
