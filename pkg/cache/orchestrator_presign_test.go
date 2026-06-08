package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// v1Server stands up the real v1 cache handler (auth disabled) over the given
// S3 mock, so HTTPPresigner is tested against the actual orchestrator contract.
func v1Server(t *testing.T, s3api S3API) *httptest.Server {
	t.Helper()
	server := NewServerWithClientsAndScope(s3api, &MockPresignAPI{}, "bucket", "org/repo")
	mux := http.NewServeMux()
	NewHandlerWithAuth(server, nil, "").RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestHTTPPresignerUpload(t *testing.T) {
	t.Parallel()

	ts := v1Server(t, &MockS3API{})
	p := NewHTTPPresigner(ts.Client(), ts.URL, "")
	url, err := p.PresignUpload(context.Background(), "k", "v1")
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	if url != "https://example.com/presigned-put" {
		t.Errorf("upload url = %q, want the orchestrator-presigned PUT", url)
	}
}

func TestHTTPPresignerDownloadHit(t *testing.T) {
	t.Parallel()

	// Default MockS3API.HeadObject succeeds => primary key found.
	ts := v1Server(t, &MockS3API{})
	p := NewHTTPPresigner(ts.Client(), ts.URL, "")
	url, matchedKey, found, err := p.PresignDownload(context.Background(), []string{"k"}, "v1")
	if err != nil {
		t.Fatalf("PresignDownload: %v", err)
	}
	if !found {
		t.Fatal("found = false, want hit")
	}
	if url != "https://example.com/presigned-get" {
		t.Errorf("download url = %q", url)
	}
	if matchedKey != "caches/org/repo/v1/k" {
		t.Errorf("matchedKey = %q, want full S3 key", matchedKey)
	}
}

func TestHTTPPresignerDownloadMiss(t *testing.T) {
	t.Parallel()

	miss := &MockS3API{
		HeadObjectFunc: func(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			return nil, &types.NotFound{}
		},
		ListObjectsV2Func: func(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
			return &s3.ListObjectsV2Output{Contents: []types.Object{}}, nil
		},
	}
	ts := v1Server(t, miss)
	p := NewHTTPPresigner(ts.Client(), ts.URL, "")
	_, _, found, err := p.PresignDownload(context.Background(), []string{"absent"}, "v1")
	if err != nil {
		t.Fatalf("PresignDownload: %v", err)
	}
	if found {
		t.Error("found = true, want miss")
	}
}

func TestHTTPPresignerSendsToken(t *testing.T) {
	t.Parallel()

	var gotToken string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get(CacheTokenHeader)
		w.Header().Set("Location", "https://example.com/presigned-put")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	p := NewHTTPPresigner(ts.Client(), ts.URL, "tok-123")
	if _, err := p.PresignUpload(context.Background(), "k", "v1"); err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	if gotToken != "tok-123" {
		t.Errorf("X-Cache-Token = %q, want tok-123", gotToken)
	}
}
