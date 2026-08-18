package logship

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type fakePutter struct {
	mu                sync.Mutex
	puts              map[string][]byte
	meta              map[string]map[string]string
	err               error
	blockUntilCtxDone bool
}

func newFakePutter() *fakePutter {
	return &fakePutter{puts: map[string][]byte{}, meta: map[string]map[string]string{}}
}

func (f *fakePutter) PutObject(ctx context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if f.blockUntilCtxDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	body, err := io.ReadAll(params.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts[*params.Key] = body
	f.meta[*params.Key] = params.Metadata
	return &s3.PutObjectOutput{}, nil
}

func (f *fakePutter) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.puts))
	for k := range f.puts {
		out = append(out, k)
	}
	return out
}

func (f *fakePutter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

func diagDir(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	diag := filepath.Join(root, "_diag")
	if err := os.MkdirAll(diag, 0o755); err != nil {
		t.Fatalf("mkdir _diag: %v", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(diag, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func testConfig() Config {
	return Config{
		Bucket:       "test-bucket",
		RunID:        "42",
		JobID:        "99",
		InstanceID:   "i-abc",
		MaxFileBytes: 1 << 20,
		Timeout:      5 * time.Second,
	}
}

func TestShipUploadsAndGzipsRoundTrip(t *testing.T) {
	const content = "worker log line one\nworker log line two\n"
	root := diagDir(t, map[string]string{"Worker_20260818-101112-utc.log": content})
	f := newFakePutter()

	outcome := NewWithClient(f, testConfig(), nil).Ship(context.Background(), root)

	if outcome != OutcomeUploaded {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeUploaded)
	}
	keys := f.keys()
	if len(keys) != 1 {
		t.Fatalf("uploaded %d objects, want 1: %v", len(keys), keys)
	}
	want := "runner-logs/42/99/i-abc/Worker_20260818-101112-utc.log.gz"
	if keys[0] != want {
		t.Errorf("key = %q, want %q", keys[0], want)
	}
	zr, err := gzip.NewReader(bytes.NewReader(f.puts[keys[0]]))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if string(got) != content {
		t.Errorf("round-tripped %q, want %q", got, content)
	}
}

func TestShipUploadsBothWorkerAndRunnerLogs(t *testing.T) {
	root := diagDir(t, map[string]string{
		"Worker_20260818-101112-utc.log": "worker",
		"Runner_20260818-101010-utc.log": "runner",
		"unrelated.txt":                  "ignored",
	})
	f := newFakePutter()

	if outcome := NewWithClient(f, testConfig(), nil).Ship(context.Background(), root); outcome != OutcomeUploaded {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeUploaded)
	}
	if f.count() != 2 {
		t.Errorf("uploaded %d objects, want 2 (Worker + Runner, not unrelated.txt): %v", f.count(), f.keys())
	}
}

func TestShipKeyUsesUnknownJobWhenJobIDEmpty(t *testing.T) {
	root := diagDir(t, map[string]string{"Worker_a.log": "x"})
	f := newFakePutter()
	cfg := testConfig()
	cfg.JobID = ""

	NewWithClient(f, cfg, nil).Ship(context.Background(), root)

	keys := f.keys()
	if len(keys) != 1 {
		t.Fatalf("uploaded %d objects, want 1", len(keys))
	}
	want := "runner-logs/42/unknown-job/i-abc/Worker_a.log.gz"
	if keys[0] != want {
		t.Errorf("key = %q, want %q", keys[0], want)
	}
}

func TestShipSkipsOversizedFileButUploadsSiblings(t *testing.T) {
	root := diagDir(t, map[string]string{
		"Worker_big.log":   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"Runner_small.log": "ok",
	})
	f := newFakePutter()
	cfg := testConfig()
	cfg.MaxFileBytes = 10

	outcome := NewWithClient(f, cfg, nil).Ship(context.Background(), root)

	if outcome != OutcomePartial {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomePartial)
	}
	keys := f.keys()
	if len(keys) != 1 {
		t.Fatalf("uploaded %d objects, want 1 (the small one): %v", len(keys), keys)
	}
	if filepath.Base(keys[0]) != "Runner_small.log.gz" {
		t.Errorf("uploaded %q, want the under-cap Runner_small.log.gz", keys[0])
	}
}

func TestShipReportsFailedOnPutError(t *testing.T) {
	root := diagDir(t, map[string]string{"Worker_a.log": "x"})
	f := newFakePutter()
	f.err = errors.New("AccessDenied: not authorized to perform s3:PutObject")

	outcome := NewWithClient(f, testConfig(), nil).Ship(context.Background(), root)

	if outcome != OutcomeFailed {
		t.Errorf("outcome = %q, want %q — an AccessDenied must never fail the job", outcome, OutcomeFailed)
	}
}

func TestShipReportsFailedOnTimeoutWithoutBlockingForever(t *testing.T) {
	root := diagDir(t, map[string]string{"Worker_a.log": "x"})
	f := newFakePutter()
	f.blockUntilCtxDone = true
	cfg := testConfig()
	cfg.Timeout = 50 * time.Millisecond

	done := make(chan string, 1)
	go func() { done <- NewWithClient(f, cfg, nil).Ship(context.Background(), root) }()

	select {
	case outcome := <-done:
		if outcome != OutcomeFailed {
			t.Errorf("outcome = %q, want %q", outcome, OutcomeFailed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ship blocked past its own timeout; it must never delay self-termination")
	}
}

func TestShipSkipsWhenNoDiagLogs(t *testing.T) {
	f := newFakePutter()

	outcome := NewWithClient(f, testConfig(), nil).Ship(context.Background(), diagDir(t, nil))

	if outcome != OutcomeSkipped {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeSkipped)
	}
	if f.count() != 0 {
		t.Errorf("uploaded %d objects, want 0", f.count())
	}
}

func TestShipDisabledWhenBucketEmpty(t *testing.T) {
	root := diagDir(t, map[string]string{"Worker_a.log": "x"})
	f := newFakePutter()
	cfg := testConfig()
	cfg.Bucket = ""

	outcome := NewWithClient(f, cfg, nil).Ship(context.Background(), root)

	if outcome != OutcomeDisabled {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeDisabled)
	}
	if f.count() != 0 {
		t.Errorf("uploaded %d objects, want 0 when disabled", f.count())
	}
}

func TestShipMissingRunnerPathSkips(t *testing.T) {
	f := newFakePutter()

	outcome := NewWithClient(f, testConfig(), nil).Ship(context.Background(), filepath.Join(t.TempDir(), "nope"))

	if outcome != OutcomeSkipped {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeSkipped)
	}
}

func TestBuildKeyMatchesShipperKeys(t *testing.T) {
	got := BuildKey("", "42", "99", "i-abc", "Worker_a.log.gz")
	want := "runner-logs/42/99/i-abc/Worker_a.log.gz"
	if got != want {
		t.Errorf("BuildKey() = %q, want %q", got, want)
	}
}

func TestBuildPrefixIsListablePerJobAndPerRun(t *testing.T) {
	if got, want := BuildPrefix("", "42", "99"), "runner-logs/42/99/"; got != want {
		t.Errorf("BuildPrefix(run,job) = %q, want %q", got, want)
	}
	if got, want := BuildPrefix("", "42", ""), "runner-logs/42/"; got != want {
		t.Errorf("BuildPrefix(run,\"\") = %q, want %q", got, want)
	}
}
