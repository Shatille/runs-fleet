package blobshim

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is an in-memory stand-in for the presigned S3 endpoint: it stores PUT
// bodies and serves GETs with Range support, like S3 honoring a presigned URL.
type fakeS3 struct {
	mu        sync.Mutex
	objs      map[string][]byte
	putStatus int // when non-zero, PUTs return this status instead of storing
}

func newFakeS3() *fakeS3 { return &fakeS3{objs: map[string][]byte{}} }

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Path
	switch r.Method {
	case http.MethodPut:
		if f.putStatus != 0 {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(f.putStatus)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.objs[key] = body
		f.mu.Unlock()
		w.Header().Set("ETag", `"deadbeef"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		f.mu.Lock()
		data, ok := f.objs[key]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "no such key", http.StatusNotFound)
			return
		}
		total := int64(len(data))
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		start, end := parseTestRange(rng, total)
		if start >= total { // unsatisfiable, e.g. 0-0 on a zero-byte object
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= total {
			end = total - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func parseTestRange(h string, total int64) (int64, int64) {
	spec := strings.TrimPrefix(h, "bytes=")
	lo, hi, _ := strings.Cut(spec, "-")
	start, _ := strconv.ParseInt(lo, 10, 64)
	end := total - 1
	if hi != "" {
		end, _ = strconv.ParseInt(hi, 10, 64)
	}
	return start, end
}

func setup(t *testing.T) (*Handler, *fakeS3, *httptest.Server) {
	t.Helper()
	s3 := newFakeS3()
	upstream := httptest.NewServer(s3)
	t.Cleanup(upstream.Close)
	return NewHandler(upstream.Client(), t.TempDir()), s3, upstream
}

// blobURL builds the shim path for a target object on the fake S3.
func blobURL(upstream *httptest.Server, object string) string {
	return PathPrefix + EncodeTarget(upstream.URL+"/"+object)
}

func blockListXML(ids ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><BlockList>`)
	for _, id := range ids {
		fmt.Fprintf(&b, "<Latest>%s</Latest>", id)
	}
	b.WriteString("</BlockList>")
	return b.String()
}

func TestPutBlobForwardsToPresignedURL(t *testing.T) {
	t.Parallel()

	h, s3, up := setup(t)
	req := httptest.NewRequest(http.MethodPut, blobURL(up, "obj"), strings.NewReader("payload"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if got := string(s3.objs["/obj"]); got != "payload" {
		t.Errorf("forwarded body = %q, want payload", got)
	}
}

func TestStageAndCommitReassemblesInOrder(t *testing.T) {
	t.Parallel()

	h, s3, up := setup(t)
	target := blobURL(up, "obj")
	stage := func(id, body string) {
		req := httptest.NewRequest(http.MethodPut, target+"?comp=block&blockid="+id, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("stage %s status = %d", id, rec.Code)
		}
	}
	stage("AAAA", "hello")
	stage("BBBB", " world")

	req := httptest.NewRequest(http.MethodPut, target+"?comp=blocklist", strings.NewReader(blockListXML("AAAA", "BBBB")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("commit status = %d, want 201", rec.Code)
	}
	if got := string(s3.objs["/obj"]); got != "hello world" {
		t.Errorf("reassembled = %q, want %q", got, "hello world")
	}
}

func TestCommitHonorsBlockListOrder(t *testing.T) {
	t.Parallel()

	h, s3, up := setup(t)
	target := blobURL(up, "obj")
	for _, b := range []struct{ id, body string }{{"one", "AAA"}, {"two", "BBB"}} {
		req := httptest.NewRequest(http.MethodPut, target+"?comp=block&blockid="+b.id, strings.NewReader(b.body))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	req := httptest.NewRequest(http.MethodPut, target+"?comp=blocklist", strings.NewReader(blockListXML("two", "one")))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := string(s3.objs["/obj"]); got != "BBBAAA" {
		t.Errorf("reassembled = %q, want BBBAAA", got)
	}
}

func TestCommitUnknownBlockIsBadRequest(t *testing.T) {
	t.Parallel()

	h, _, up := setup(t)
	req := httptest.NewRequest(http.MethodPut, blobURL(up, "obj")+"?comp=blocklist", strings.NewReader(blockListXML("missing")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestGetFullAndRange(t *testing.T) {
	t.Parallel()

	h, s3, up := setup(t)
	s3.objs["/obj"] = []byte("0123456789")
	target := blobURL(up, "obj")

	full := httptest.NewRecorder()
	h.ServeHTTP(full, httptest.NewRequest(http.MethodGet, target, nil))
	if full.Code != http.StatusOK {
		t.Fatalf("full status = %d", full.Code)
	}
	if body, _ := io.ReadAll(full.Body); string(body) != "0123456789" {
		t.Errorf("full body = %q", body)
	}

	rr := httptest.NewRequest(http.MethodGet, target, nil)
	rr.Header.Set("Range", "bytes=2-4")
	rrec := httptest.NewRecorder()
	h.ServeHTTP(rrec, rr)
	if rrec.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", rrec.Code)
	}
	if body, _ := io.ReadAll(rrec.Body); string(body) != "234" {
		t.Errorf("range body = %q, want 234", body)
	}
	if cr := rrec.Header().Get("Content-Range"); cr != "bytes 2-4/10" {
		t.Errorf("Content-Range = %q", cr)
	}
}

func TestFailedCommitCleansStaging(t *testing.T) {
	t.Parallel()

	h, s3, up := setup(t)
	target := up.URL + "/obj"
	token := EncodeTarget(target)
	blobPath := PathPrefix + token

	stage := httptest.NewRequest(http.MethodPut, blobPath+"?comp=block&blockid=AAAA", strings.NewReader("hello"))
	h.ServeHTTP(httptest.NewRecorder(), stage)

	s3.putStatus = http.StatusInternalServerError // upstream PUT fails
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, blobPath+"?comp=blocklist", strings.NewReader(blockListXML("AAAA"))))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if _, err := os.Stat(h.blockDir(token)); !os.IsNotExist(err) {
		t.Errorf("staging dir not cleaned after failed commit: %v", err)
	}
}

func TestHeadZeroByteObject(t *testing.T) {
	t.Parallel()

	h, s3, up := setup(t)
	s3.objs["/empty"] = []byte{} // zero-byte cache; S3 answers 0-0 range with 416
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, blobURL(up, "empty"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "0" {
		t.Errorf("Content-Length = %q, want 0", cl)
	}
}

func TestHeadSynthesizesSizeFromRange(t *testing.T) {
	t.Parallel()

	h, s3, up := setup(t)
	s3.objs["/obj"] = []byte("0123456789")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, blobURL(up, "obj"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "10" {
		t.Errorf("Content-Length = %q, want 10", cl)
	}
}

func TestBadTokenIsBadRequest(t *testing.T) {
	t.Parallel()

	h, _, _ := setup(t)
	req := httptest.NewRequest(http.MethodGet, PathPrefix+"not*base64*", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	t.Parallel()

	h, _, _ := setup(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/elsewhere", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
