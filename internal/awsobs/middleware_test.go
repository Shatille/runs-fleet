package awsobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"
)

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

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

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
		ServiceID:     "DynamoDB",
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

	_, _, err := stack.HandleMiddleware(context.Background(), nil, terminal)
	if !errors.Is(err, retErr) {
		t.Fatalf("HandleMiddleware error = %v, want %v", err, retErr)
	}

	return decodeRecords(t, buf.Bytes())
}

// smithyRequestNew is a no-op request factory; the observability middleware
// never inspects the request value.
func smithyRequestNew() any { return nil }

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
	if got := rec["service"]; got != "DynamoDB" {
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
	if got := rec["service"]; got != "DynamoDB" {
		t.Errorf("service = %v, want DynamoDB", got)
	}
	if got := rec["operation"]; got != "GetItem" {
		t.Errorf("operation = %v, want GetItem", got)
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
