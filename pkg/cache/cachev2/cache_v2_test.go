package cachev2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/cache/blobshim"
)

const testBlobBase = "https://127.0.0.1:9123"

type mockPresigner struct {
	uploadURL   string
	downloadURL string
	matchedKey  string
	found       bool
	uploadErr   error
	downloadErr error
	gotKeys     []string
}

func (m *mockPresigner) PresignUpload(context.Context, string, string) (string, error) {
	if m.uploadErr != nil {
		return "", m.uploadErr
	}
	return m.uploadURL, nil
}

func (m *mockPresigner) PresignDownload(_ context.Context, keys []string, _ string) (string, string, bool, error) {
	m.gotKeys = keys
	if m.downloadErr != nil {
		return "", "", false, m.downloadErr
	}
	return m.downloadURL, m.matchedKey, m.found, nil
}

func postJSON(t *testing.T, svc *ServiceV2, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, cacheServiceV2Prefix+method, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeBlobTarget(t *testing.T, blobURL string) string {
	t.Helper()
	tok := strings.TrimPrefix(blobURL, testBlobBase+blobshim.PathPrefix)
	if tok == blobURL {
		t.Fatalf("not a blob shim url: %q", blobURL)
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatalf("decode blob target: %v", err)
	}
	return string(raw)
}

func TestCreateCacheEntryEmbedsPresignedUploadURL(t *testing.T) {
	t.Parallel()

	p := &mockPresigner{uploadURL: "https://s3.example/put?sig=abc"}
	rec := postJSON(t, NewServiceV2(p, testBlobBase, nil), "CreateCacheEntry", `{"key":"k","version":"v1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp createCacheEntryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Error("ok = false")
	}
	if got := decodeBlobTarget(t, resp.SignedUploadURL); got != p.uploadURL {
		t.Errorf("embedded target = %q, want %q", got, p.uploadURL)
	}
}

func TestGetDownloadURLHit(t *testing.T) {
	t.Parallel()

	// Orchestrator returns the full S3 key; ServiceV2 must strip it to the bare
	// key the client sent.
	p := &mockPresigner{downloadURL: "https://s3.example/get?sig=xyz", matchedKey: "caches/org/repo/v1/k", found: true}
	rec := postJSON(t, NewServiceV2(p, testBlobBase, nil), "GetCacheEntryDownloadURL", `{"key":"k","restoreKeys":["pfx-"],"version":"v1"}`)
	var resp getCacheEntryDownloadURLResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.MatchedKey != "k" {
		t.Fatalf("ok=%v matchedKey=%q, want hit with bare key k", resp.OK, resp.MatchedKey)
	}
	if got := decodeBlobTarget(t, resp.SignedDownloadURL); got != p.downloadURL {
		t.Errorf("embedded target = %q, want %q", got, p.downloadURL)
	}
	if len(p.gotKeys) != 2 || p.gotKeys[0] != "k" || p.gotKeys[1] != "pfx-" {
		t.Errorf("presigner keys = %v, want [k pfx-]", p.gotKeys)
	}
}

func TestGetDownloadURLAcceptsSnakeCaseRestoreKeys(t *testing.T) {
	t.Parallel()

	// The real cache@v5 Twirp client (protobuf-ts) serializes the proto field
	// restore_keys in snake_case on the wire, not camelCase. If we only bind
	// "restoreKeys", restore-keys are silently dropped and the orchestrator does
	// only the exact HeadObject — never the prefix fallback. Both casings must work.
	p := &mockPresigner{found: false}
	postJSON(t, NewServiceV2(p, testBlobBase, nil), "GetCacheEntryDownloadURL",
		`{"key":"primary","restore_keys":["pfx-a","pfx-b"],"version":"v1"}`)
	if len(p.gotKeys) != 3 || p.gotKeys[0] != "primary" || p.gotKeys[1] != "pfx-a" || p.gotKeys[2] != "pfx-b" {
		t.Errorf("presigner keys = %v, want [primary pfx-a pfx-b] (snake_case restore_keys dropped)", p.gotKeys)
	}
}

func TestGetDownloadURLSnakeCaseRestoreKeyHit(t *testing.T) {
	t.Parallel()

	// A snake_case restore-key must round-trip a hit, not just reach the
	// presigner: the matched full key is stripped to the bare key the client
	// compares against.
	p := &mockPresigner{downloadURL: "https://s3.example/get?sig=xyz", matchedKey: "caches/org/repo/v1/pfx-abc", found: true}
	rec := postJSON(t, NewServiceV2(p, testBlobBase, nil), "GetCacheEntryDownloadURL",
		`{"key":"primary","restore_keys":["pfx-"],"version":"v1"}`)
	var resp getCacheEntryDownloadURLResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.MatchedKey != "pfx-abc" {
		t.Fatalf("ok=%v matchedKey=%q, want hit with bare key pfx-abc", resp.OK, resp.MatchedKey)
	}
	if len(p.gotKeys) != 2 || p.gotKeys[0] != "primary" || p.gotKeys[1] != "pfx-" {
		t.Errorf("presigner keys = %v, want [primary pfx-]", p.gotKeys)
	}
}

func TestGetDownloadURLMiss(t *testing.T) {
	t.Parallel()

	p := &mockPresigner{found: false}
	rec := postJSON(t, NewServiceV2(p, testBlobBase, nil), "GetCacheEntryDownloadURL", `{"key":"absent","version":"v1"}`)
	var resp getCacheEntryDownloadURLResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.OK || resp.SignedDownloadURL != "" {
		t.Errorf("want miss, got ok=%v url=%q", resp.OK, resp.SignedDownloadURL)
	}
}

func TestGetDownloadURLErrorFailsOpenAsMiss(t *testing.T) {
	t.Parallel()

	p := &mockPresigner{downloadErr: errors.New("orchestrator down")}
	rec := postJSON(t, NewServiceV2(p, testBlobBase, nil), "GetCacheEntryDownloadURL", `{"key":"k","version":"v1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open miss, not error)", rec.Code)
	}
	var resp getCacheEntryDownloadURLResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.OK {
		t.Error("lookup error should surface as miss")
	}
}

func TestCreateCacheEntryPresignErrorIsBadGateway(t *testing.T) {
	t.Parallel()

	p := &mockPresigner{uploadErr: errors.New("orchestrator down")}
	rec := postJSON(t, NewServiceV2(p, testBlobBase, nil), "CreateCacheEntry", `{"key":"k","version":"v1"}`)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestV2RejectsBadRequests(t *testing.T) {
	t.Parallel()

	svc := NewServiceV2(&mockPresigner{}, testBlobBase, nil)
	cases := []struct {
		method, body string
	}{
		{"CreateCacheEntry", "not json"},
		{"CreateCacheEntry", `{"key":"","version":"v1"}`},
		{"CreateCacheEntry", `{"key":"k","version":""}`},
		{"GetCacheEntryDownloadURL", `{"key":"","version":"v1"}`},
		{"GetCacheEntryDownloadURL", `{"key":"k","version":""}`},
		{"FinalizeCacheEntryUpload", `{"key":"","version":"v1"}`},
	}
	for _, tc := range cases {
		if rec := postJSON(t, svc, tc.method, tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %q: status = %d, want 400", tc.method, tc.body, rec.Code)
		}
	}
}

func TestBareKeyForVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, full, version, want string
	}{
		{"scoped", "caches/org/repo/v1/Linux-go-abc", "v1", "Linux-go-abc"},
		{"no scope", "caches/v1/Linux-go-abc", "v1", "Linux-go-abc"},
		{"key contains slash", "caches/org/repo/v1/a/b/c", "v1", "a/b/c"},
		{"version substring of key", "caches/org/repo/abc123/abc123-linux", "abc123", "abc123-linux"},
		{"no marker returns input", "weird-key", "v1", "weird-key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bareKeyForVersion(tc.full, tc.version); got != tc.want {
				t.Errorf("bareKeyForVersion(%q, %q) = %q, want %q", tc.full, tc.version, got, tc.want)
			}
		})
	}
}

func TestFinalizeAcks(t *testing.T) {
	t.Parallel()

	rec := postJSON(t, NewServiceV2(&mockPresigner{}, testBlobBase, nil), "FinalizeCacheEntryUpload", `{"key":"k","version":"v1"}`)
	var resp finalizeCacheEntryUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Error("ok = false, want true")
	}
}

func TestV2NonPostRejected(t *testing.T) {
	t.Parallel()

	svc := NewServiceV2(&mockPresigner{}, testBlobBase, nil)
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, cacheServiceV2Prefix+"CreateCacheEntry", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
