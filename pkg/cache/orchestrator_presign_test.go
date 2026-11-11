package cache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/cache/blobshim"
	"github.com/Shavakan/runs-fleet/pkg/cache/cachev2"
	"github.com/Shavakan/runs-fleet/pkg/cache/wire"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// This contract test for cachev2.HTTPPresigner lives in package cache (not
// cachev2) because it drives the presigner against the REAL v1 server using
// package-internal helpers (NewServerWithClientsAndScope, MockS3API); cachev2
// imports nothing from cache, so this stays acyclic.

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
	p := cachev2.NewHTTPPresigner(ts.Client(), ts.URL, "")
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
	p := cachev2.NewHTTPPresigner(ts.Client(), ts.URL, "")
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
	if matchedKey != "k" {
		t.Errorf("matchedKey = %q, want the bare key the client sent", matchedKey)
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
	p := cachev2.NewHTTPPresigner(ts.Client(), ts.URL, "")
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
		gotToken = r.Header.Get(wire.CacheTokenHeader)
		w.Header().Set("Location", "https://example.com/presigned-put")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	p := cachev2.NewHTTPPresigner(ts.Client(), ts.URL, "tok-123")
	if _, err := p.PresignUpload(context.Background(), "k", "v1"); err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	if gotToken != "tok-123" {
		t.Errorf("X-Cache-Token = %q, want tok-123", gotToken)
	}
}

func TestHTTPPresignerMultipartRoundTrip(t *testing.T) {
	t.Parallel()

	f := &fakeMultipart{}
	server := NewServerWithClientsAndScope(&MockS3API{}, &MockPresignAPI{}, "bucket", "org/repo")
	server.SetMultipartClients(f, f)
	mux := http.NewServeMux()
	NewHandlerWithAuth(server, nil, "").RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	p := cachev2.NewHTTPPresigner(ts.Client(), ts.URL, "")
	ctx := context.Background()
	id, err := p.CreateMultipart(ctx, "k", "v1")
	if err != nil || id != testUploadID {
		t.Fatalf("CreateMultipart = %q, %v", id, err)
	}
	url, err := p.PresignPart(ctx, "k", "v1", id, 4)
	if err != nil || url != testPartURL {
		t.Fatalf("PresignPart = %q, %v", url, err)
	}
	if *f.presignIn.PartNumber != 4 {
		t.Errorf("part number = %d, want 4", *f.presignIn.PartNumber)
	}
	if err := p.CompleteMultipart(ctx, "k", "v1", id, []wire.MultipartPart{{PartNumber: 1, ETag: "a"}}, 9); err != nil {
		t.Fatalf("CompleteMultipart: %v", err)
	}
	if err := p.AbortMultipart(ctx, "k", "v1", id); err != nil {
		t.Fatalf("AbortMultipart: %v", err)
	}
	if f.createKey != "caches/org/repo/v1/k" || f.abortKey != "caches/org/repo/v1/k" {
		t.Errorf("keys = create %q abort %q", f.createKey, f.abortKey)
	}
}

func TestHTTPPresignerMultipartUnsupported(t *testing.T) {
	t.Parallel()

	ts := v1Server(t, &MockS3API{})
	p := cachev2.NewHTTPPresigner(ts.Client(), ts.URL, "")
	if _, err := p.CreateMultipart(context.Background(), "k", "v1"); !errors.Is(err, blobshim.ErrMultipartUnsupported) {
		t.Errorf("CreateMultipart err = %v, want ErrMultipartUnsupported", err)
	}
}
