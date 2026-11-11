package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Shavakan/runs-fleet/pkg/cache/wire"
)

const (
	testUploadID  = "upload-1"
	testScopedKey = "caches/org/repo/v1/go-build-x"
	testPartURL   = "https://example.com/presigned-part"
)

// fakeMultipart records the multipart calls the server makes against S3.
type fakeMultipart struct {
	createKey   string
	completeIn  *s3.CompleteMultipartUploadInput
	abortKey    string
	abortErr    error
	presignIn   *s3.UploadPartInput
	completeErr error
}

func (f *fakeMultipart) CreateMultipartUpload(_ context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.createKey = *in.Key
	id := testUploadID
	return &s3.CreateMultipartUploadOutput{UploadId: &id}, nil
}

func (f *fakeMultipart) CompleteMultipartUpload(_ context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	f.completeIn = in
	return &s3.CompleteMultipartUploadOutput{}, f.completeErr
}

func (f *fakeMultipart) AbortMultipartUpload(_ context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.abortKey = *in.Key
	return &s3.AbortMultipartUploadOutput{}, f.abortErr
}

func (f *fakeMultipart) PresignUploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	f.presignIn = in
	return &v4.PresignedHTTPRequest{URL: testPartURL}, nil
}

func scopedMultipartServer(f *fakeMultipart) *Server {
	s := NewServerWithClientsAndScope(&MockS3API{}, &MockPresignAPI{}, "bucket", "org/repo")
	s.SetMultipartClients(f, f)
	return s
}

func TestMultipartDisabledWithoutClients(t *testing.T) {
	t.Parallel()

	s := NewServerWithClients(&MockS3API{}, &MockPresignAPI{}, "bucket")
	if s.MultipartEnabled() {
		t.Fatal("MultipartEnabled() = true without multipart clients")
	}
	if _, err := s.CreateMultipartUpload(context.Background(), "k", "v1"); !errors.Is(err, ErrMultipartDisabled) {
		t.Errorf("CreateMultipartUpload err = %v, want ErrMultipartDisabled", err)
	}
}

func TestMultipartUsesScopedCacheKey(t *testing.T) {
	t.Parallel()

	f := &fakeMultipart{}
	s := scopedMultipartServer(f)
	ctx := context.Background()

	id, err := s.CreateMultipartUpload(ctx, "go-build-x", "v1")
	if err != nil || id != testUploadID {
		t.Fatalf("CreateMultipartUpload = %q, %v", id, err)
	}
	if f.createKey != testScopedKey {
		t.Errorf("create key = %q, want the scoped cache key", f.createKey)
	}

	url, err := s.PresignUploadPart(ctx, "go-build-x", "v1", id, 3)
	if err != nil || url != testPartURL {
		t.Fatalf("PresignUploadPart = %q, %v", url, err)
	}
	if *f.presignIn.Key != testScopedKey || *f.presignIn.UploadId != id || *f.presignIn.PartNumber != 3 {
		t.Errorf("presign input = key %q upload %q part %d", *f.presignIn.Key, *f.presignIn.UploadId, *f.presignIn.PartNumber)
	}

	parts := []wire.MultipartPart{{PartNumber: 1, ETag: `"a"`}, {PartNumber: 2, ETag: `"b"`}}
	if err := s.CompleteMultipartUpload(ctx, "go-build-x", "v1", id, parts); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	got := f.completeIn.MultipartUpload.Parts
	if len(got) != 2 || *got[0].PartNumber != 1 || *got[1].ETag != `"b"` {
		t.Errorf("completed parts = %+v", got)
	}

	if err := s.AbortMultipartUpload(ctx, "go-build-x", "v1", id); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	if f.abortKey != testScopedKey {
		t.Errorf("abort key = %q", f.abortKey)
	}
}

func TestMultipartRejectsBadInput(t *testing.T) {
	t.Parallel()

	s := scopedMultipartServer(&fakeMultipart{})
	ctx := context.Background()

	if _, err := s.CreateMultipartUpload(ctx, "../escape", "v1"); err == nil {
		t.Error("CreateMultipartUpload accepted a traversal key")
	}
	if _, err := s.PresignUploadPart(ctx, "k", "v1", "", 1); err == nil {
		t.Error("PresignUploadPart accepted an empty upload ID")
	}
	for _, n := range []int32{0, 10001} {
		if _, err := s.PresignUploadPart(ctx, "k", "v1", "u", n); err == nil {
			t.Errorf("PresignUploadPart accepted part number %d", n)
		}
	}
	if err := s.CompleteMultipartUpload(ctx, "k", "v1", "u", nil); err == nil {
		t.Error("CompleteMultipartUpload accepted no parts")
	}
	unordered := []wire.MultipartPart{{PartNumber: 2, ETag: "b"}, {PartNumber: 1, ETag: "a"}}
	if err := s.CompleteMultipartUpload(ctx, "k", "v1", "u", unordered); err == nil {
		t.Error("CompleteMultipartUpload accepted descending part numbers")
	}
	missingETag := []wire.MultipartPart{{PartNumber: 1}}
	if err := s.CompleteMultipartUpload(ctx, "k", "v1", "u", missingETag); err == nil {
		t.Error("CompleteMultipartUpload accepted a part without an ETag")
	}
}

func multipartHandler(t *testing.T, s *Server) (*httptest.Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	NewHandlerWithAuth(s, nil, "secret").RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, GenerateCacheToken("secret", "job-1", "i-1", "org/repo")
}

func postMultipart(t *testing.T, ts *httptest.Server, token, path string, req wire.MultipartRequest) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	hr, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		hr.Header.Set(wire.CacheTokenHeader, token)
	}
	resp, err := ts.Client().Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestMultipartRoutesUseTokenScope(t *testing.T) {
	t.Parallel()

	f := &fakeMultipart{}
	s := NewServerWithClients(&MockS3API{}, &MockPresignAPI{}, "bucket")
	s.SetMultipartClients(f, f)
	ts, token := multipartHandler(t, s)

	status, body := postMultipart(t, ts, token, wire.MultipartCreatePath, wire.MultipartRequest{Key: "go-build-x", Version: "v1"})
	var created wire.MultipartCreateResponse
	if status != http.StatusOK || json.Unmarshal(body, &created) != nil || created.UploadID != testUploadID {
		t.Fatalf("create: status %d body %s", status, body)
	}
	if f.createKey != testScopedKey {
		t.Errorf("create key = %q, want the token's scope", f.createKey)
	}

	status, body = postMultipart(t, ts, token, wire.MultipartPartPath, wire.MultipartRequest{Key: "go-build-x", Version: "v1", UploadID: created.UploadID, PartNumber: 2})
	var part wire.MultipartPartResponse
	if status != http.StatusOK || json.Unmarshal(body, &part) != nil || part.URL != testPartURL {
		t.Fatalf("part: status %d body %s", status, body)
	}
	if *f.presignIn.Key != testScopedKey {
		t.Errorf("part key = %q", *f.presignIn.Key)
	}

	status, body = postMultipart(t, ts, token, wire.MultipartCompletePath, wire.MultipartRequest{
		Key: "go-build-x", Version: "v1", UploadID: created.UploadID, Size: 42,
		Parts: []wire.MultipartPart{{PartNumber: 1, ETag: `"a"`}},
	})
	if status != http.StatusOK {
		t.Fatalf("complete: status %d body %s", status, body)
	}

	status, body = postMultipart(t, ts, token, wire.MultipartAbortPath, wire.MultipartRequest{Key: "go-build-x", Version: "v1", UploadID: created.UploadID})
	if status != http.StatusNoContent {
		t.Fatalf("abort: status %d body %s", status, body)
	}
	if f.abortKey != testScopedKey {
		t.Errorf("abort key = %q", f.abortKey)
	}
}

func TestMultipartRoutesRequireToken(t *testing.T) {
	t.Parallel()

	f := &fakeMultipart{}
	s := NewServerWithClients(&MockS3API{}, &MockPresignAPI{}, "bucket")
	s.SetMultipartClients(f, f)
	ts, _ := multipartHandler(t, s)

	status, _ := postMultipart(t, ts, "", wire.MultipartCreatePath, wire.MultipartRequest{Key: "k", Version: "v1"})
	if status != http.StatusUnauthorized {
		t.Errorf("create without token: status %d, want 401", status)
	}
	if f.createKey != "" {
		t.Error("an unauthenticated create reached S3")
	}
}

func TestMultipartRoutesNotFoundWhenDisabled(t *testing.T) {
	t.Parallel()

	ts, token := multipartHandler(t, NewServerWithClients(&MockS3API{}, &MockPresignAPI{}, "bucket"))
	status, _ := postMultipart(t, ts, token, wire.MultipartCreatePath, wire.MultipartRequest{Key: "k", Version: "v1"})
	if status != http.StatusNotFound {
		t.Errorf("create with multipart disabled: status %d, want 404", status)
	}
}

func TestMultipartRoutesRejectBadRequests(t *testing.T) {
	t.Parallel()

	f := &fakeMultipart{completeErr: errors.New("EntityTooSmall")}
	s := NewServerWithClients(&MockS3API{}, &MockPresignAPI{}, "bucket")
	s.SetMultipartClients(f, f)
	ts, token := multipartHandler(t, s)

	cases := []struct {
		name string
		path string
		req  wire.MultipartRequest
		want int
	}{
		{"part number zero", wire.MultipartPartPath, wire.MultipartRequest{Key: "k", Version: "v1", UploadID: "u"}, http.StatusBadRequest},
		{"traversal key", wire.MultipartCreatePath, wire.MultipartRequest{Key: "../x", Version: "v1"}, http.StatusBadRequest},
		{"no parts", wire.MultipartCompletePath, wire.MultipartRequest{Key: "k", Version: "v1", UploadID: "u"}, http.StatusBadRequest},
		{"s3 rejects complete", wire.MultipartCompletePath, wire.MultipartRequest{Key: "k", Version: "v1", UploadID: "u", Parts: []wire.MultipartPart{{PartNumber: 1, ETag: "a"}}}, http.StatusBadGateway},
	}
	for _, tc := range cases {
		status, body := postMultipart(t, ts, token, tc.path, tc.req)
		if status != tc.want {
			t.Errorf("%s: status %d (%s), want %d", tc.name, status, body, tc.want)
		}
	}
}

type opMetrics struct {
	operations  []string
	bytesStored []int64
}

func (m *opMetrics) PublishCacheRequest(context.Context, string) error { return nil }
func (m *opMetrics) PublishCacheOperation(_ context.Context, op string) error {
	m.operations = append(m.operations, op)
	return nil
}
func (m *opMetrics) PublishCacheBytesStored(_ context.Context, n int64) error {
	m.bytesStored = append(m.bytesStored, n)
	return nil
}
func (m *opMetrics) PublishCacheError(context.Context, string) error        { return nil }
func (m *opMetrics) PublishCacheAuthRejected(context.Context, string) error { return nil }

// The runner meters v2 upload bytes itself (docs/METRICS.md), so the
// orchestrator's complete must not count them again.
func TestMultipartCompleteDoesNotMeterBytes(t *testing.T) {
	t.Parallel()

	f := &fakeMultipart{}
	s := NewServerWithClients(&MockS3API{}, &MockPresignAPI{}, "bucket")
	s.SetMultipartClients(f, f)
	m := &opMetrics{}
	mux := http.NewServeMux()
	NewHandlerWithMetrics(s, m).RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	status, body := postMultipart(t, ts, "", wire.MultipartCompletePath, wire.MultipartRequest{
		Key: "k", Version: "v1", UploadID: "u", Size: 42,
		Parts: []wire.MultipartPart{{PartNumber: 1, ETag: "a"}},
	})
	if status != http.StatusOK {
		t.Fatalf("complete: status %d body %s", status, body)
	}
	if len(m.bytesStored) != 0 {
		t.Errorf("bytes stored = %v, want none (the agent reports v2 bytes)", m.bytesStored)
	}
	if len(m.operations) != 1 || m.operations[0] != "multipart_complete" {
		t.Errorf("operations = %v, want [multipart_complete]", m.operations)
	}
}

func TestMultipartPartIsNotCountedAsOperation(t *testing.T) {
	t.Parallel()

	f := &fakeMultipart{}
	s := NewServerWithClients(&MockS3API{}, &MockPresignAPI{}, "bucket")
	s.SetMultipartClients(f, f)
	m := &opMetrics{}
	mux := http.NewServeMux()
	NewHandlerWithMetrics(s, m).RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	if status, body := postMultipart(t, ts, "", wire.MultipartPartPath, wire.MultipartRequest{Key: "k", Version: "v1", UploadID: "u", PartNumber: 1}); status != http.StatusOK {
		t.Fatalf("part: status %d body %s", status, body)
	}
	if len(m.operations) != 0 {
		t.Errorf("operations = %v, want none for a part presign", m.operations)
	}
}
