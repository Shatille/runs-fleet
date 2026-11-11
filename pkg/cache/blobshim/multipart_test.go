package blobshim

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/cache/wire"
)

// sdkBlockID builds a block ID the way @azure/storage-blob's generateBlockID
// does: base64 of a 36-character UUID followed by the index zero-padded to 12 digits.
func sdkBlockID(i int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("0f8fad5b-d9cb-469f-a165-70867728950e%012d", i)))
}

// fakeOrchestrator plays the orchestrator's multipart routes against the fake
// S3: parts land at /parts/<uploadID>/<n>, and complete concatenates them in
// the order given into /<key>.
type fakeOrchestrator struct {
	s3          *fakeS3
	base        string
	unsupported bool

	mu        sync.Mutex
	creates   int
	completed []wire.MultipartPart
	size      int64
	aborted   []string
}

func (o *fakeOrchestrator) CreateMultipart(_ context.Context, key, _ string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.unsupported {
		return "", ErrMultipartUnsupported
	}
	o.creates++
	return fmt.Sprintf("up-%s-%d", key, o.creates), nil
}

func (o *fakeOrchestrator) PresignPart(_ context.Context, _, _, uploadID string, part int32) (string, error) {
	return fmt.Sprintf("%s/parts/%s/%d", o.base, uploadID, part), nil
}

func (o *fakeOrchestrator) CompleteMultipart(_ context.Context, key, _, uploadID string, parts []wire.MultipartPart, size int64) error {
	var obj []byte
	for _, p := range parts {
		obj = append(obj, o.s3.get(fmt.Sprintf("/parts/%s/%d", uploadID, p.PartNumber))...)
	}
	o.s3.seed("/"+key, obj)
	o.mu.Lock()
	o.completed, o.size = parts, size
	o.mu.Unlock()
	return nil
}

func (o *fakeOrchestrator) AbortMultipart(_ context.Context, _, _, uploadID string) error {
	o.mu.Lock()
	o.aborted = append(o.aborted, uploadID)
	o.mu.Unlock()
	return nil
}

func setupMultipart(t *testing.T) (*Handler, *fakeS3, *httptest.Server, *fakeOrchestrator, string) {
	t.Helper()
	h, s3, up, _ := setupWithBytes(t)
	orch := &fakeOrchestrator{s3: s3, base: up.URL}
	h.SetMultipart(orch)
	h.minPartSize = 1
	return h, s3, up, orch, h.staging
}

func uploadURL(upstream *httptest.Server, key string) string {
	return PathPrefix + EncodeUploadTarget(upstream.URL+"/"+key, key, "v1")
}

func stage(t *testing.T, h *Handler, blobPath, id, body string) int {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, blobPath+"?comp=block&blockid="+urlQueryEscape(id), strings.NewReader(body))
	h.ServeHTTP(w, r)
	return w.Code
}

func commit(t *testing.T, h *Handler, blobPath string, ids ...string) int {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, blobPath+"?comp=blocklist", strings.NewReader(blockListXML(ids...)))
	h.ServeHTTP(w, r)
	return w.Code
}

func urlQueryEscape(s string) string {
	return strings.NewReplacer("+", "%2B", "/", "%2F", "=", "%3D").Replace(s)
}

func stagedFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		sub, _ := os.ReadDir(dir + "/" + e.Name())
		n += len(sub)
	}
	return n
}

func TestBlockIndex(t *testing.T) {
	t.Parallel()

	for _, i := range []int{0, 7, 9999} {
		if got, ok := blockIndex(sdkBlockID(i)); !ok || got != i {
			t.Errorf("blockIndex(sdkBlockID(%d)) = %d, %v", i, got, ok)
		}
	}
	bad := []string{
		"YmxvY2stMDE=",
		sdkBlockID(10000),
		base64.StdEncoding.EncodeToString([]byte("0f8fad5b-d9cb-469f-a165-70867728950e00000000000x")),
		base64.StdEncoding.EncodeToString([]byte("0f8fad5b-d9cb-469f-a165-70867728950e-00000000001")),
		"not base64!",
	}
	for _, id := range bad {
		if _, ok := blockIndex(id); ok {
			t.Errorf("blockIndex(%q) parsed, want rejected", id)
		}
	}
}

func TestMultipartStreamsBlocksWithoutStaging(t *testing.T) {
	t.Parallel()

	h, s3, up, orch, staging := setupMultipart(t)
	var stored int64
	h.onBytes = func(n int64) { stored += n }
	blob := uploadURL(up, "go-build")

	for i, body := range []string{"aaa", "bbb", "c"} {
		if code := stage(t, h, blob, sdkBlockID(i), body); code != http.StatusCreated {
			t.Fatalf("stage %d: status %d", i, code)
		}
	}
	if n := stagedFiles(t, staging); n != 0 {
		t.Errorf("%d block files on disk, want none", n)
	}
	if code := commit(t, h, blob, sdkBlockID(0), sdkBlockID(1), sdkBlockID(2)); code != http.StatusCreated {
		t.Fatalf("commit: status %d", code)
	}
	if got := string(s3.get("/go-build")); got != "aaabbbc" {
		t.Errorf("object = %q, want aaabbbc", got)
	}
	if len(orch.completed) != 3 || orch.completed[0].PartNumber != 1 || orch.completed[2].PartNumber != 3 || orch.completed[0].ETag == "" {
		t.Errorf("completed parts = %+v", orch.completed)
	}
	if orch.size != 7 || stored != 7 {
		t.Errorf("size reported = %d, metered = %d, want 7", orch.size, stored)
	}
}

func TestMultipartRetriedBlockReplacesPart(t *testing.T) {
	t.Parallel()

	h, s3, up, _, _ := setupMultipart(t)
	blob := uploadURL(up, "retry")
	stage(t, h, blob, sdkBlockID(0), "old")
	stage(t, h, blob, sdkBlockID(0), "new")
	stage(t, h, blob, sdkBlockID(1), "!")
	if code := commit(t, h, blob, sdkBlockID(0), sdkBlockID(1)); code != http.StatusCreated {
		t.Fatalf("commit: status %d", code)
	}
	if got := string(s3.get("/retry")); got != "new!" {
		t.Errorf("object = %q, want new!", got)
	}
}

func TestMultipartCommitOutOfOrderAborts(t *testing.T) {
	t.Parallel()

	h, s3, up, orch, _ := setupMultipart(t)
	blob := uploadURL(up, "unordered")
	stage(t, h, blob, sdkBlockID(0), "a")
	stage(t, h, blob, sdkBlockID(1), "b")
	if code := commit(t, h, blob, sdkBlockID(1), sdkBlockID(0)); code != http.StatusBadRequest {
		t.Errorf("commit: status %d, want 400", code)
	}
	if len(orch.aborted) != 1 {
		t.Errorf("aborted = %v, want the upload aborted", orch.aborted)
	}
	if s3.get("/unordered") != nil {
		t.Error("object written despite a reordered block list")
	}
}

func TestMultipartCommitUnknownBlockAborts(t *testing.T) {
	t.Parallel()

	h, _, up, orch, _ := setupMultipart(t)
	blob := uploadURL(up, "missing")
	stage(t, h, blob, sdkBlockID(0), "a")
	if code := commit(t, h, blob, sdkBlockID(0), sdkBlockID(1)); code != http.StatusBadRequest {
		t.Errorf("commit: status %d, want 400", code)
	}
	if len(orch.aborted) != 1 {
		t.Errorf("aborted = %v, want the upload aborted", orch.aborted)
	}
}

func TestMultipartPartUploadFailure(t *testing.T) {
	t.Parallel()

	h, s3, up, _, _ := setupMultipart(t)
	s3.putStatus.Store(http.StatusInternalServerError)
	if code := stage(t, h, uploadURL(up, "fails"), sdkBlockID(0), "a"); code != http.StatusBadGateway {
		t.Errorf("stage: status %d, want 502", code)
	}
}

func TestMultipartFallsBackToStaging(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		prepare func(h *Handler, orch *fakeOrchestrator)
		blob    func(up *httptest.Server) string
		ids     []string
	}{
		{
			name:    "orchestrator without multipart",
			prepare: func(_ *Handler, orch *fakeOrchestrator) { orch.unsupported = true },
			blob:    func(up *httptest.Server) string { return uploadURL(up, "obj") },
			ids:     []string{sdkBlockID(0), sdkBlockID(1)},
		},
		{
			name:    "block IDs not from the Azure SDK",
			prepare: func(*Handler, *fakeOrchestrator) {},
			blob:    func(up *httptest.Server) string { return uploadURL(up, "obj") },
			ids:     []string{"YmxvY2stMDE=", "YmxvY2stMDI="},
		},
		{
			name:    "target without a cache key",
			prepare: func(*Handler, *fakeOrchestrator) {},
			blob:    func(up *httptest.Server) string { return blobURL(up, "obj") },
			ids:     []string{sdkBlockID(0), sdkBlockID(1)},
		},
		{
			name:    "first block below the S3 part minimum",
			prepare: func(h *Handler, _ *fakeOrchestrator) { h.minPartSize = 1 << 20 },
			blob:    func(up *httptest.Server) string { return uploadURL(up, "obj") },
			ids:     []string{sdkBlockID(0), sdkBlockID(1)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, s3, up, orch, _ := setupMultipart(t)
			tc.prepare(h, orch)
			blob := tc.blob(up)
			for i, id := range tc.ids {
				if code := stage(t, h, blob, id, fmt.Sprintf("part%d", i)); code != http.StatusCreated {
					t.Fatalf("stage %d: status %d", i, code)
				}
			}
			if code := commit(t, h, blob, tc.ids...); code != http.StatusCreated {
				t.Fatalf("commit: status %d", code)
			}
			if got := string(s3.get("/obj")); got != "part0part1" {
				t.Errorf("object = %q, want part0part1", got)
			}
			if orch.completed != nil {
				t.Error("multipart complete called on the staging path")
			}
		})
	}
}

type failingReader struct{ sent bool }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.sent {
		return 0, errors.New("client went away")
	}
	f.sent = true
	return copy(p, "partial"), nil
}

func TestFailedStageRemovesPartialBlock(t *testing.T) {
	t.Parallel()

	h, _, up := setup(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, blobURL(up, "obj")+"?comp=block&blockid=YmxvY2stMDE=", io.NopCloser(&failingReader{}))
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("stage: status %d, want 500", w.Code)
	}
	if n := stagedFiles(t, h.staging); n != 0 {
		t.Errorf("%d partial block files left on disk", n)
	}
}

func TestMultipartConcurrentBlocksShareOneUpload(t *testing.T) {
	t.Parallel()

	h, s3, up, orch, _ := setupMultipart(t)
	blob := uploadURL(up, "concurrent")
	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = stage(t, h, blob, sdkBlockID(i), fmt.Sprintf("%02d", i))
		}()
	}
	wg.Wait()
	ids := make([]string, n)
	var want strings.Builder
	for i := range n {
		if codes[i] != http.StatusCreated {
			t.Fatalf("stage %d: status %d", i, codes[i])
		}
		ids[i] = sdkBlockID(i)
		fmt.Fprintf(&want, "%02d", i)
	}
	if code := commit(t, h, blob, ids...); code != http.StatusCreated {
		t.Fatalf("commit: status %d", code)
	}
	if got := string(s3.get("/concurrent")); got != want.String() {
		t.Errorf("object = %q, want %q", got, want.String())
	}
	orch.mu.Lock()
	defer orch.mu.Unlock()
	if orch.creates != 1 {
		t.Errorf("multipart uploads created = %d, want 1", orch.creates)
	}
}

type loopbackOrchestrator struct{ *fakeOrchestrator }

func (loopbackOrchestrator) PresignPart(context.Context, string, string, string, int32) (string, error) {
	return "http://169.254.169.254/latest/meta-data", nil
}

func TestMultipartRejectsDisallowedPartURL(t *testing.T) {
	t.Parallel()

	h, _, _, orch, _ := setupMultipart(t)
	h.SetMultipart(loopbackOrchestrator{orch})
	h.allowTarget = allowedTarget
	blob := PathPrefix + EncodeUploadTarget("https://s3.example/obj", "k", "v1")
	if code := stage(t, h, blob, sdkBlockID(0), "a"); code != http.StatusBadGateway {
		t.Errorf("stage with a metadata-endpoint part URL: status %d, want 502", code)
	}
}

func TestMultipartCommitRejectsStaleBlockID(t *testing.T) {
	t.Parallel()

	h, _, up, orch, _ := setupMultipart(t)
	blob := uploadURL(up, "stale")
	stage(t, h, blob, sdkBlockID(0), "a")
	other := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("11111111-2222-3333-4444-555555555555%012d", 0)))
	if code := commit(t, h, blob, other); code != http.StatusBadRequest {
		t.Errorf("commit with a different block ID at the same index: status %d, want 400", code)
	}
	if len(orch.aborted) != 1 {
		t.Errorf("aborted = %v, want the upload aborted", orch.aborted)
	}
}
