package cache

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/Shavakan/runs-fleet/pkg/cache/blobshim"
)

const testBlobBase = "https://127.0.0.1:9123"

func newV2(t *testing.T, s3api S3API) (*ServiceV2, *blobshim.Signer) {
	t.Helper()
	signer := blobshim.NewSigner([]byte("v2-secret"))
	server := NewServerWithClients(s3api, &MockPresignAPI{}, "bucket")
	svc, err := NewServiceV2(server, "org/repo", testBlobBase, signer, nil)
	if err != nil {
		t.Fatalf("NewServiceV2: %v", err)
	}
	return svc, signer
}

func TestNewServiceV2RequiresScope(t *testing.T) {
	t.Parallel()

	server := NewServerWithClients(&MockS3API{}, &MockPresignAPI{}, "bucket")
	signer := blobshim.NewSigner([]byte("v2-secret"))
	if _, err := NewServiceV2(server, "", testBlobBase, signer, nil); err == nil {
		t.Error("NewServiceV2 with empty scope = nil error, want error (fleet-wide bucket needs isolation)")
	}
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

func decodeBlobToken(t *testing.T, signer *blobshim.Signer, url string) (blobshim.Operation, string) {
	t.Helper()
	tok := strings.TrimPrefix(url, testBlobBase+blobshim.PathPrefix)
	if tok == url {
		t.Fatalf("url %q does not point at the blob shim", url)
	}
	op, key, err := signer.Verify(tok, time.Now())
	if err != nil {
		t.Fatalf("blob token verify: %v", err)
	}
	return op, key
}

func TestCreateCacheEntryReturnsSignedWriteURL(t *testing.T) {
	t.Parallel()

	svc, signer := newV2(t, &MockS3API{})
	rec := postJSON(t, svc, "CreateCacheEntry", `{"key":"Linux-go-abc","version":"v1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp createCacheEntryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Error("ok = false, want true")
	}
	op, key := decodeBlobToken(t, signer, resp.SignedUploadURL)
	if op != blobshim.OpWrite {
		t.Errorf("op = %q, want write", op)
	}
	if key != "caches/org/repo/v1/Linux-go-abc" {
		t.Errorf("s3 key = %q", key)
	}
}

func TestGetDownloadURLHit(t *testing.T) {
	t.Parallel()

	// Default MockS3API.HeadObject returns success => the primary key is found.
	svc, signer := newV2(t, &MockS3API{})
	rec := postJSON(t, svc, "GetCacheEntryDownloadURL", `{"key":"Linux-go-abc","version":"v1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp getCacheEntryDownloadURLResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Fatal("ok = false, want hit")
	}
	if resp.MatchedKey != "Linux-go-abc" {
		t.Errorf("matchedKey = %q, want Linux-go-abc", resp.MatchedKey)
	}
	op, key := decodeBlobToken(t, signer, resp.SignedDownloadURL)
	if op != blobshim.OpRead {
		t.Errorf("op = %q, want read", op)
	}
	if key != "caches/org/repo/v1/Linux-go-abc" {
		t.Errorf("s3 key = %q", key)
	}
}

func TestGetDownloadURLMiss(t *testing.T) {
	t.Parallel()

	miss := &MockS3API{
		HeadObjectFunc: func(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			return nil, &types.NotFound{}
		},
		ListObjectsV2Func: func(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
			return &s3.ListObjectsV2Output{Contents: []types.Object{}}, nil
		},
	}
	svc, _ := newV2(t, miss)
	rec := postJSON(t, svc, "GetCacheEntryDownloadURL", `{"key":"absent","restoreKeys":["pfx-"],"version":"v1"}`)
	var resp getCacheEntryDownloadURLResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("ok = true, want miss")
	}
	if resp.SignedDownloadURL != "" {
		t.Errorf("download url = %q, want empty on miss", resp.SignedDownloadURL)
	}
}

func TestFinalizeConfirmsUpload(t *testing.T) {
	t.Parallel()

	svc, _ := newV2(t, &MockS3API{}) // default Head success => present
	rec := postJSON(t, svc, "FinalizeCacheEntryUpload", `{"key":"Linux-go-abc","version":"v1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp finalizeCacheEntryUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Error("ok = false, want true")
	}
}

func TestCreateCacheEntryRejectsBadRequest(t *testing.T) {
	t.Parallel()

	svc, _ := newV2(t, &MockS3API{})
	if rec := postJSON(t, svc, "CreateCacheEntry", "not json"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body status = %d, want 400", rec.Code)
	}
	if rec := postJSON(t, svc, "CreateCacheEntry", `{"key":"","version":"v1"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty key status = %d, want 400", rec.Code)
	}
}

func TestNonPostIsRejected(t *testing.T) {
	t.Parallel()

	svc, _ := newV2(t, &MockS3API{})
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, cacheServiceV2Prefix+"CreateCacheEntry", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
