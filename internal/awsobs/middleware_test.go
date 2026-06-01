package awsobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/metrics"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"
)

// testServiceID is the service name the bare-stack metadata seeder injects and
// the metric assertions expect.
const testServiceID = "DynamoDB"

// timeoutErr is a net.Error whose Timeout reports true, used to exercise the
// timeout branch without relying on context.DeadlineExceeded.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

// durationCall records one PublishAWSCallDuration invocation.
type durationCall struct {
	service   string
	operation string
	seconds   float64
}

// failureCall records one PublishAWSCallFailure invocation.
type failureCall struct {
	service   string
	operation string
	result    string
}

// fakePublisher is a metrics.Publisher that captures the AWS-call metrics the
// observability middleware emits. All other methods inherit the no-op behavior.
type fakePublisher struct {
	metrics.NoopPublisher
	mu        sync.Mutex
	durations []durationCall
	failures  []failureCall
}

func (f *fakePublisher) PublishAWSCallDuration(_ context.Context, service, operation string, seconds float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.durations = append(f.durations, durationCall{service, operation, seconds})
	return nil
}

func (f *fakePublisher) PublishAWSCallFailure(_ context.Context, service, operation, result string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, failureCall{service, operation, result})
	return nil
}

func (f *fakePublisher) snapshotDurations() []durationCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]durationCall(nil), f.durations...)
}

func (f *fakePublisher) snapshotFailures() []failureCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]failureCall(nil), f.failures...)
}

// runStack builds an Initialize-step stack with the observability middleware
// (emitting to fake) plus a terminal handler that registers service and
// operation metadata, then sleeps and returns the supplied error.
func runStack(t *testing.T, fake metrics.Publisher, serviceID string, sleep time.Duration, retErr error) {
	t.Helper()

	rec := NewRecorder()
	rec.SetPublisher(fake)

	stack := middleware.NewStack("test", smithyRequestNew)
	if err := register(rec)(stack); err != nil {
		t.Fatalf("register middleware: %v", err)
	}
	// On this bare stack RegisterServiceMetadata is not yet present when register
	// runs, so the observability middleware takes its fallback head-of-step
	// position. RegisterServiceMetadata, added last with Before, then sits ahead
	// of it and seeds service/operation before the observability middleware reads
	// them. The real SDK ordering (metadata present first, anchored after) is
	// covered in realstack_test.go.
	if err := stack.Initialize.Add(&awsmiddleware.RegisterServiceMetadata{
		ServiceID:     serviceID,
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

func TestMiddlewareEmitsDurationOnEveryCall(t *testing.T) {
	t.Parallel()

	fake := &fakePublisher{}
	// A fast successful call must still emit a duration metric so latency
	// percentiles are observable, and it must not emit a failure counter.
	runStack(t, fake, testServiceID, 0, nil)

	durations := fake.snapshotDurations()
	if len(durations) != 1 {
		t.Fatalf("expected exactly one duration metric, got %d: %v", len(durations), durations)
	}
	d := durations[0]
	if d.service != testServiceID {
		t.Errorf("duration service = %q, want %q", d.service, testServiceID)
	}
	if d.operation != "GetItem" {
		t.Errorf("duration operation = %q, want GetItem", d.operation)
	}
	if d.seconds < 0 {
		t.Errorf("duration seconds = %v, want >= 0", d.seconds)
	}
	if failures := fake.snapshotFailures(); len(failures) != 0 {
		t.Errorf("expected no failure metrics on success, got %v", failures)
	}
}

func TestMiddlewareSlowCallEmitsNoWarnLog(t *testing.T) {
	// Not parallel: swaps the global slog default to capture any stray log output.
	//
	// A slow call must emit only the duration metric: the per-call WARN that
	// flooded the logs (SQS ReceiveMessage long-polls 20s) is gone. The middleware
	// no longer holds a logger, so this guards against any reintroduced logging.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	fake := &fakePublisher{}
	runStack(t, fake, testServiceID, 20*time.Millisecond, nil)

	if got := len(fake.snapshotDurations()); got != 1 {
		t.Fatalf("expected exactly one duration metric for a slow call, got %d", got)
	}
	if records := decodeRecords(t, buf.Bytes()); len(records) != 0 {
		t.Fatalf("expected no log records for a slow call, got %d: %v", len(records), records)
	}
}

func TestMiddlewareTimeoutEmitsFailureCounter(t *testing.T) {
	t.Parallel()

	fake := &fakePublisher{}
	runStack(t, fake, testServiceID, 0, context.DeadlineExceeded)

	if got := len(fake.snapshotDurations()); got != 1 {
		t.Fatalf("expected exactly one duration metric, got %d", got)
	}
	failures := fake.snapshotFailures()
	if len(failures) != 1 {
		t.Fatalf("expected exactly one failure metric, got %d: %v", len(failures), failures)
	}
	f := failures[0]
	if f.service != testServiceID {
		t.Errorf("failure service = %q, want %q", f.service, testServiceID)
	}
	if f.operation != "GetItem" {
		t.Errorf("failure operation = %q, want GetItem", f.operation)
	}
	if f.result != resultTimeout {
		t.Errorf("failure result = %q, want %q", f.result, resultTimeout)
	}
}

func TestMiddlewareNetTimeoutEmitsTimeoutResult(t *testing.T) {
	t.Parallel()

	fake := &fakePublisher{}
	runStack(t, fake, testServiceID, 0, timeoutErr{})

	failures := fake.snapshotFailures()
	if len(failures) != 1 {
		t.Fatalf("expected exactly one failure metric, got %d: %v", len(failures), failures)
	}
	if failures[0].result != resultTimeout {
		t.Errorf("failure result = %q, want %q for a net.Error timeout", failures[0].result, resultTimeout)
	}
}

func TestMiddlewareNonTimeoutErrorEmitsErrorResult(t *testing.T) {
	t.Parallel()

	fake := &fakePublisher{}
	runStack(t, fake, testServiceID, 0, errors.New("validation error"))

	failures := fake.snapshotFailures()
	if len(failures) != 1 {
		t.Fatalf("expected exactly one failure metric, got %d: %v", len(failures), failures)
	}
	if failures[0].result != resultError {
		t.Errorf("failure result = %q, want %q for an ordinary error", failures[0].result, resultError)
	}
	// The duration is still recorded for failed calls.
	if got := len(fake.snapshotDurations()); got != 1 {
		t.Errorf("expected one duration metric for a failed call, got %d", got)
	}
}

func TestMiddlewareSkipsCloudWatch(t *testing.T) {
	t.Parallel()

	// CloudWatch is the metrics backend itself; recording its calls would recurse
	// through PutMetricData, so the middleware emits nothing for it.
	fake := &fakePublisher{}
	runStack(t, fake, cloudWatchServiceID, 0, context.DeadlineExceeded)

	if got := fake.snapshotDurations(); len(got) != 0 {
		t.Errorf("expected no duration metrics for CloudWatch, got %v", got)
	}
	if got := fake.snapshotFailures(); len(got) != 0 {
		t.Errorf("expected no failure metrics for CloudWatch, got %v", got)
	}
}

func TestFailureResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain", errors.New("boom"), resultError},
		{"deadline", context.DeadlineExceeded, resultTimeout},
		{"canceled", context.Canceled, resultTimeout},
		{"net timeout", timeoutErr{}, resultTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := failureResult(tt.err); got != tt.want {
				t.Errorf("failureResult(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
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

func TestRecorderDefaultsToNoop(t *testing.T) {
	t.Parallel()

	// A fresh recorder must not panic when the middleware emits before any real
	// publisher is installed (the wiring window before initMetrics runs).
	rec := NewRecorder()
	if err := rec.get().PublishAWSCallDuration(context.Background(), "S3", "GetObject", 0.1); err != nil {
		t.Errorf("default recorder publish error = %v", err)
	}
	// SetPublisher(nil) must fall back to no-op rather than installing a nil that
	// panics on use.
	rec.SetPublisher(nil)
	if err := rec.get().PublishAWSCallFailure(context.Background(), "S3", "GetObject", resultError); err != nil {
		t.Errorf("nil-fallback recorder publish error = %v", err)
	}
}

func TestMiddlewaresRegistersOnStack(t *testing.T) {
	t.Parallel()

	apps, rec := Middlewares()
	if rec == nil {
		t.Fatal("Middlewares() returned nil recorder")
	}
	stack := middleware.NewStack("test", smithyRequestNew)
	for _, apply := range apps {
		if err := apply(stack); err != nil {
			t.Fatalf("apply middleware option: %v", err)
		}
	}
	if _, ok := stack.Initialize.Get(middlewareID); !ok {
		t.Fatalf("middleware %q not registered on Initialize step", middlewareID)
	}
}
