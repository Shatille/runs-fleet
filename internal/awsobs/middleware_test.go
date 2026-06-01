package awsobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"
)

// testServiceID is the service name the bare-stack metadata seeder injects and
// the slow/timeout assertions expect.
const testServiceID = "DynamoDB"

// timeoutErr is a net.Error whose Timeout reports true, used to exercise the
// timeout branch without relying on context.DeadlineExceeded.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

// runStack builds an Initialize-step stack with the observability middleware
// (at the given threshold) plus a terminal handler that registers service and
// operation metadata, then sleeps and returns the supplied error. It returns
// the captured log records as decoded JSON maps.
func runStack(t *testing.T, threshold, sleep time.Duration, retErr error) []map[string]any {
	t.Helper()
	return runStackCtx(context.Background(), t, threshold, sleep, retErr)
}

// runStackCtx is runStack with an explicit invocation context, used to assert
// that task identity stashed on the context via logging.ContextWith reaches the
// observability record. The logger is wrapped exactly as production wires it (a
// contextHandler over the JSON handler) so context-stashed attrs are injected.
func runStackCtx(
	ctx context.Context, t *testing.T, threshold, sleep time.Duration, retErr error,
) []map[string]any {
	t.Helper()

	var buf bytes.Buffer
	log := logging.NewWithHandler(newCtxJSONHandler(&buf))

	stack := middleware.NewStack("test", smithyRequestNew)
	if err := register(log, threshold)(stack); err != nil {
		t.Fatalf("register middleware: %v", err)
	}
	// On this bare stack RegisterServiceMetadata is not yet present when register
	// runs, so the observability middleware takes its fallback head-of-step
	// position. RegisterServiceMetadata, added last with Before, then sits ahead
	// of it and seeds service/operation before the observability middleware reads
	// them. The real SDK ordering (metadata present first, anchored after) is
	// covered in realstack_test.go.
	if err := stack.Initialize.Add(&awsmiddleware.RegisterServiceMetadata{
		ServiceID:     testServiceID,
		OperationName: "GetItem",
	}, middleware.Before); err != nil {
		t.Fatalf("register metadata middleware: %v", err)
	}

	terminal := middleware.HandlerFunc(func(_ context.Context, _ any) (any, middleware.Metadata, error) {
		if sleep > 0 {
			time.Sleep(sleep)
		}
		return nil, middleware.Metadata{}, retErr
	})

	_, _, err := stack.HandleMiddleware(ctx, nil, terminal)
	if !errors.Is(err, retErr) {
		t.Fatalf("HandleMiddleware error = %v, want %v", err, retErr)
	}

	return decodeRecords(t, buf.Bytes())
}

// smithyRequestNew is a no-op request factory; the observability middleware
// never inspects the request value.
func smithyRequestNew() any { return nil }

// newCtxJSONHandler builds the production handler chain (contextHandler over a
// JSON handler) so context-stashed task identity is emitted on *Context logs.
func newCtxJSONHandler(buf *bytes.Buffer) slog.Handler {
	return logging.NewContextHandler(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func decodeRecords(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var records []map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		records = append(records, rec)
	}
	return records
}

func TestMiddlewareFastSuccessNotLogged(t *testing.T) {
	t.Parallel()

	records := runStack(t, time.Second, 0, nil)
	if len(records) != 0 {
		t.Fatalf("expected no log records for a fast successful call, got %d: %v", len(records), records)
	}
}

func TestMiddlewareSlowCallLogged(t *testing.T) {
	t.Parallel()

	records := runStack(t, 5*time.Millisecond, 20*time.Millisecond, nil)
	if len(records) != 1 {
		t.Fatalf("expected exactly one log record, got %d: %v", len(records), records)
	}
	rec := records[0]

	if got := rec["level"]; got != "WARN" {
		t.Errorf("level = %v, want WARN", got)
	}
	if got := rec["service"]; got != testServiceID {
		t.Errorf("service = %v, want DynamoDB", got)
	}
	if got := rec["operation"]; got != "GetItem" {
		t.Errorf("operation = %v, want GetItem", got)
	}
	if got := rec["slow"]; got != true {
		t.Errorf("slow = %v, want true", got)
	}
	if got := rec["timeout"]; got != false {
		t.Errorf("timeout = %v, want false", got)
	}
	dur, ok := rec["duration_ms"].(float64)
	if !ok {
		t.Fatalf("duration_ms missing or not a number: %v", rec["duration_ms"])
	}
	if dur < 0 {
		t.Errorf("duration_ms = %v, want >= 0", dur)
	}
	if _, present := rec["error"]; present {
		t.Errorf("error field should be absent on a slow successful call, got %v", rec["error"])
	}
}

func TestMiddlewareDeadlineExceededLogged(t *testing.T) {
	t.Parallel()

	// Generous threshold so the call is flagged solely on the error path.
	records := runStack(t, time.Hour, 0, context.DeadlineExceeded)
	if len(records) != 1 {
		t.Fatalf("expected exactly one log record, got %d: %v", len(records), records)
	}
	rec := records[0]

	if got := rec["level"]; got != "WARN" {
		t.Errorf("level = %v, want WARN", got)
	}
	if got := rec["timeout"]; got != true {
		t.Errorf("timeout = %v, want true", got)
	}
	if got := rec["slow"]; got != false {
		t.Errorf("slow = %v, want false", got)
	}
	if got := rec["error"]; got != context.DeadlineExceeded.Error() {
		t.Errorf("error = %v, want %q", got, context.DeadlineExceeded.Error())
	}
	if got := rec["service"]; got != testServiceID {
		t.Errorf("service = %v, want DynamoDB", got)
	}
	if got := rec["operation"]; got != "GetItem" {
		t.Errorf("operation = %v, want GetItem", got)
	}
}

func TestMiddlewareCarriesTaskIdentityFromContext(t *testing.T) {
	t.Parallel()

	// Identity stashed on the call context (as the worker/webhook entry points do)
	// must appear on the AWS-call observability record so a wedged AWS call is
	// attributable to the job that issued it. The middleware itself adds no
	// job/run/repo fields.
	ctx := logging.ContextWithJob(context.Background(), 4242, 9999, "octo/repo")
	records := runStackCtx(ctx, t, time.Hour, 0, context.DeadlineExceeded)
	if len(records) != 1 {
		t.Fatalf("expected exactly one log record, got %d: %v", len(records), records)
	}
	rec := records[0]

	if got := rec[logging.KeyJobID]; got != float64(4242) {
		t.Errorf("job_id = %v, want 4242 (must come from the call context)", got)
	}
	if got := rec[logging.KeyRunID]; got != float64(9999) {
		t.Errorf("run_id = %v, want 9999", got)
	}
	if got := rec[logging.KeyRepo]; got != "octo/repo" {
		t.Errorf("repo = %v, want octo/repo", got)
	}
	// The middleware's own fields must still be present alongside the identity.
	if got := rec["timeout"]; got != true {
		t.Errorf("timeout = %v, want true", got)
	}
	if got := rec["service"]; got != testServiceID {
		t.Errorf("service = %v, want DynamoDB", got)
	}
}

func TestMiddlewareNoTaskIdentityWhenContextEmpty(t *testing.T) {
	t.Parallel()

	// A bare context carries no identity; the record must not invent job fields.
	records := runStackCtx(context.Background(), t, time.Hour, 0, context.DeadlineExceeded)
	if len(records) != 1 {
		t.Fatalf("expected exactly one log record, got %d: %v", len(records), records)
	}
	if _, present := records[0][logging.KeyJobID]; present {
		t.Errorf("job_id must be absent when the context carries no identity, got %v", records[0][logging.KeyJobID])
	}
}

func TestMiddlewareNetTimeoutLogged(t *testing.T) {
	t.Parallel()

	records := runStack(t, time.Hour, 0, timeoutErr{})
	if len(records) != 1 {
		t.Fatalf("expected exactly one log record, got %d: %v", len(records), records)
	}
	if got := records[0]["timeout"]; got != true {
		t.Errorf("timeout = %v, want true for net.Error timeout", got)
	}
}

func TestMiddlewareNonTimeoutErrorNotLogged(t *testing.T) {
	t.Parallel()

	// A fast call that fails with an ordinary error is neither slow nor a
	// timeout, so it must not be logged.
	records := runStack(t, time.Hour, 0, errors.New("validation error"))
	if len(records) != 0 {
		t.Fatalf("expected no log records for a fast non-timeout error, got %d: %v", len(records), records)
	}
}

func TestIsTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, true},
		{"wrapped deadline", errors.Join(errors.New("op failed"), context.DeadlineExceeded), true},
		{"net timeout", timeoutErr{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isTimeout(tt.err); got != tt.want {
				t.Errorf("isTimeout(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestMiddlewaresRegistersOnStack(t *testing.T) {
	t.Parallel()

	stack := middleware.NewStack("test", smithyRequestNew)
	for _, apply := range Middlewares(nil) {
		if err := apply(stack); err != nil {
			t.Fatalf("apply middleware option: %v", err)
		}
	}
	if _, ok := stack.Initialize.Get(middlewareID); !ok {
		t.Fatalf("middleware %q not registered on Initialize step", middlewareID)
	}
}
