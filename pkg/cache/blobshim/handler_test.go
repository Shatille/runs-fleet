package blobshim

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestHandler(t *testing.T, store ObjectStore) (*Handler, *Signer) {
	t.Helper()
	signer := NewSigner([]byte("test-secret"))
	h := NewHandler(store, signer, t.TempDir())
	h.now = func() time.Time { return time.Unix(1_000_000, 0) }
	return h, signer
}

func writeToken(t *testing.T, s *Signer, op Operation, key string) string {
	t.Helper()
	return s.Sign(op, key, time.Unix(1_000_000, 0).Add(time.Hour))
}

func blockListXML(ids ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for _, id := range ids {
		fmt.Fprintf(&b, "<Latest>%s</Latest>", id)
	}
	b.WriteString("</BlockList>")
	return b.String()
}

func TestStageAndCommitReassemblesInOrder(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	h, signer := newTestHandler(t, store)
	const s3Key = "caches/org/repo/v1/key"
	tok := writeToken(t, signer, OpWrite, s3Key)

	stage := func(blockID, body string) {
		req := httptest.NewRequest(http.MethodPut, PathPrefix+tok+"?comp=block&blockid="+blockID, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("stage %s: status = %d, want 201", blockID, rec.Code)
		}
	}
	stage("AAAA", "hello")
	stage("BBBB", " world")

	req := httptest.NewRequest(http.MethodPut, PathPrefix+tok+"?comp=blocklist", strings.NewReader(blockListXML("AAAA", "BBBB")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("commit: status = %d, want 201", rec.Code)
	}
	if got := string(store.objs[s3Key]); got != "hello world" {
		t.Errorf("stored = %q, want %q", got, "hello world")
	}
}

func TestCommitHonorsBlockListOrder(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	h, signer := newTestHandler(t, store)
	const s3Key = "caches/k"
	tok := writeToken(t, signer, OpWrite, s3Key)

	for _, b := range []struct{ id, body string }{{"one", "AAA"}, {"two", "BBB"}} {
		req := httptest.NewRequest(http.MethodPut, PathPrefix+tok+"?comp=block&blockid="+b.id, strings.NewReader(b.body))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	// Commit in reverse order; the stored object must follow the block list.
	req := httptest.NewRequest(http.MethodPut, PathPrefix+tok+"?comp=blocklist", strings.NewReader(blockListXML("two", "one")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := string(store.objs[s3Key]); got != "BBBAAA" {
		t.Errorf("stored = %q, want BBBAAA", got)
	}
}

func TestCommitUnknownBlockIsBadRequest(t *testing.T) {
	t.Parallel()

	h, signer := newTestHandler(t, newMemStore())
	tok := writeToken(t, signer, OpWrite, "caches/k")
	req := httptest.NewRequest(http.MethodPut, PathPrefix+tok+"?comp=blocklist", strings.NewReader(blockListXML("never-staged")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestPutBlobSingleShot(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	h, signer := newTestHandler(t, store)
	const s3Key = "caches/single"
	tok := writeToken(t, signer, OpWrite, s3Key)

	req := httptest.NewRequest(http.MethodPut, PathPrefix+tok, strings.NewReader("payload"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if got := string(store.objs[s3Key]); got != "payload" {
		t.Errorf("stored = %q, want payload", got)
	}
}

func TestGetFullAndRange(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.objs["caches/obj"] = []byte("0123456789")
	h, signer := newTestHandler(t, store)
	tok := writeToken(t, signer, OpRead, "caches/obj")

	full := httptest.NewRequest(http.MethodGet, PathPrefix+tok, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, full)
	if rec.Code != http.StatusOK {
		t.Fatalf("full get status = %d, want 200", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Body); string(body) != "0123456789" {
		t.Errorf("full body = %q", body)
	}

	ranged := httptest.NewRequest(http.MethodGet, PathPrefix+tok, nil)
	ranged.Header.Set("Range", "bytes=2-4")
	rrec := httptest.NewRecorder()
	h.ServeHTTP(rrec, ranged)
	if rrec.Code != http.StatusPartialContent {
		t.Fatalf("range get status = %d, want 206", rrec.Code)
	}
	if body, _ := io.ReadAll(rrec.Body); string(body) != "234" {
		t.Errorf("range body = %q, want 234", body)
	}
	if cr := rrec.Header().Get("Content-Range"); cr != "bytes 2-4/10" {
		t.Errorf("Content-Range = %q, want bytes 2-4/10", cr)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	t.Parallel()

	h, signer := newTestHandler(t, newMemStore())
	tok := writeToken(t, signer, OpRead, "caches/absent")
	req := httptest.NewRequest(http.MethodGet, PathPrefix+tok, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestReadTokenCannotWrite(t *testing.T) {
	t.Parallel()

	h, signer := newTestHandler(t, newMemStore())
	tok := writeToken(t, signer, OpRead, "caches/k")
	req := httptest.NewRequest(http.MethodPut, PathPrefix+tok, strings.NewReader("x"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestWriteTokenCannotRead(t *testing.T) {
	t.Parallel()

	h, signer := newTestHandler(t, newMemStore())
	tok := writeToken(t, signer, OpWrite, "caches/k")
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, PathPrefix+tok, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with write token: status = %d, want 403", method, rec.Code)
		}
	}
}

func TestExpiredTokenForbidden(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	signer := NewSigner([]byte("test-secret"))
	h := NewHandler(store, signer, t.TempDir())
	h.now = func() time.Time { return time.Unix(2_000_000, 0) }

	tok := signer.Sign(OpRead, "caches/k", time.Unix(1_000_000, 0))
	req := httptest.NewRequest(http.MethodGet, PathPrefix+tok, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	t.Parallel()

	h, _ := newTestHandler(t, newMemStore())
	req := httptest.NewRequest(http.MethodGet, "/elsewhere", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
