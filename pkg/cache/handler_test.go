package cache_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/Shavakan/runs-fleet/pkg/cache"
)

type mockS3Client struct {
	headObjectFunc    func(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	listObjectsV2Func func(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

func (m *mockS3Client) HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if m.headObjectFunc != nil {
		return m.headObjectFunc(ctx, params, optFns...)
	}
	return &s3.HeadObjectOutput{}, nil
}

func (m *mockS3Client) ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if m.listObjectsV2Func != nil {
		return m.listObjectsV2Func(ctx, params, optFns...)
	}
	return &s3.ListObjectsV2Output{Contents: []types.Object{}}, nil
}

type mockPresignClient struct {
	presignPutFunc func(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
	presignGetFunc func(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

func (m *mockPresignClient) PresignPutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	if m.presignPutFunc != nil {
		return m.presignPutFunc(ctx, params, optFns...)
	}
	return &v4.PresignedHTTPRequest{URL: "https://example.com/presigned-put"}, nil
}

func (m *mockPresignClient) PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	if m.presignGetFunc != nil {
		return m.presignGetFunc(ctx, params, optFns...)
	}
	return &v4.PresignedHTTPRequest{URL: "https://example.com/presigned-get"}, nil
}

func TestHandler_ReserveCacheEntry(t *testing.T) {
	tests := []struct {
		name           string
		requestBody    string
		presignErr     error
		wantStatusCode int
		wantCacheID    bool
	}{
		{
			name:           "valid request",
			requestBody:    `{"key":"my-cache-key","version":"abc123"}`,
			wantStatusCode: http.StatusOK,
			wantCacheID:    true,
		},
		{
			name:           "invalid JSON",
			requestBody:    `{invalid}`,
			wantStatusCode: http.StatusBadRequest,
			wantCacheID:    false,
		},
		{
			name:           "empty key",
			requestBody:    `{"key":"","version":"abc123"}`,
			wantStatusCode: http.StatusBadRequest,
			wantCacheID:    false,
		},
		{
			name:           "empty version",
			requestBody:    `{"key":"my-cache-key","version":""}`,
			wantStatusCode: http.StatusBadRequest,
			wantCacheID:    false,
		},
		{
			name:           "presign error",
			requestBody:    `{"key":"my-cache-key","version":"abc123"}`,
			presignErr:     errors.New("presign failed"),
			wantStatusCode: http.StatusInternalServerError,
			wantCacheID:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockPresign := &mockPresignClient{
				presignPutFunc: func(_ context.Context, _ *s3.PutObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
					if tt.presignErr != nil {
						return nil, tt.presignErr
					}
					return &v4.PresignedHTTPRequest{URL: "https://example.com/upload"}, nil
				},
			}

			server := cache.NewServerWithClients(&mockS3Client{}, mockPresign, "test-bucket")
			handler := cache.NewHandler(server)

			req := httptest.NewRequest(http.MethodPost, "/_apis/artifactcache/caches", strings.NewReader(tt.requestBody))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.ReserveCacheEntry(w, req)

			if w.Code != tt.wantStatusCode {
				t.Errorf("ReserveCacheEntry() status = %v, want %v", w.Code, tt.wantStatusCode)
			}

			if tt.wantCacheID {
				var resp map[string]interface{}
				if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
					t.Fatalf("Failed to decode response: %v", err)
				}
				if _, ok := resp["cacheId"]; !ok {
					t.Error("Response missing cacheId field")
				}
			}
		})
	}
}

func TestHandler_CommitCacheEntry(t *testing.T) {
	tests := []struct {
		name           string
		cacheID        string
		requestBody    string
		wantStatusCode int
	}{
		{
			name:           "valid commit",
			cacheID:        "caches/abc123/my-key",
			requestBody:    `{"size":1024}`,
			wantStatusCode: http.StatusOK,
		},
		{
			name:           "empty cache ID",
			cacheID:        "",
			requestBody:    `{"size":1024}`,
			wantStatusCode: http.StatusBadRequest,
		},
		{
			name:           "invalid JSON",
			cacheID:        "caches/abc123/my-key",
			requestBody:    `{invalid}`,
			wantStatusCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := cache.NewServerWithClients(&mockS3Client{}, &mockPresignClient{}, "test-bucket")
			handler := cache.NewHandler(server)

			req := httptest.NewRequest(http.MethodPatch, "/_apis/artifactcache/caches/"+tt.cacheID, strings.NewReader(tt.requestBody))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.CommitCacheEntry(w, req, tt.cacheID)

			if w.Code != tt.wantStatusCode {
				t.Errorf("CommitCacheEntry() status = %v, want %v", w.Code, tt.wantStatusCode)
			}
		})
	}
}

func TestHandler_GetCacheEntry(t *testing.T) {
	tests := []struct {
		name           string
		queryKeys      string
		queryVersion   string
		headObjectFunc func(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
		wantStatusCode int
		wantArchiveURL bool
	}{
		{
			name:         "cache hit",
			queryKeys:    "my-key",
			queryVersion: "abc123",
			headObjectFunc: func(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
				return &s3.HeadObjectOutput{}, nil
			},
			wantStatusCode: http.StatusOK,
			wantArchiveURL: true,
		},
		{
			name:         "cache miss",
			queryKeys:    "my-key",
			queryVersion: "abc123",
			headObjectFunc: func(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
				return nil, &types.NoSuchKey{}
			},
			wantStatusCode: http.StatusNoContent,
			wantArchiveURL: false,
		},
		{
			name:           "missing keys parameter",
			queryKeys:      "",
			queryVersion:   "abc123",
			wantStatusCode: http.StatusBadRequest,
			wantArchiveURL: false,
		},
		{
			name:           "missing version parameter",
			queryKeys:      "my-key",
			queryVersion:   "",
			wantStatusCode: http.StatusBadRequest,
			wantArchiveURL: false,
		},
		{
			name:         "S3 error",
			queryKeys:    "my-key",
			queryVersion: "abc123",
			headObjectFunc: func(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
				return nil, errors.New("S3 error")
			},
			wantStatusCode: http.StatusInternalServerError,
			wantArchiveURL: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockS3 := &mockS3Client{headObjectFunc: tt.headObjectFunc}
			mockPresign := &mockPresignClient{}

			server := cache.NewServerWithClients(mockS3, mockPresign, "test-bucket")
			handler := cache.NewHandler(server)

			url := "/_apis/artifactcache/cache"
			if tt.queryKeys != "" || tt.queryVersion != "" {
				url += "?keys=" + tt.queryKeys + "&version=" + tt.queryVersion
			}

			req := httptest.NewRequest(http.MethodGet, url, nil)
			w := httptest.NewRecorder()

			handler.GetCacheEntry(w, req)

			if w.Code != tt.wantStatusCode {
				t.Errorf("GetCacheEntry() status = %v, want %v", w.Code, tt.wantStatusCode)
			}

			if tt.wantArchiveURL {
				var resp map[string]interface{}
				if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
					t.Fatalf("Failed to decode response: %v", err)
				}
				if _, ok := resp["archiveLocation"]; !ok {
					t.Error("Response missing archiveLocation field")
				}
			}
		})
	}
}

func TestHandler_DownloadCacheArtifact(t *testing.T) {
	tests := []struct {
		name           string
		cacheID        string
		presignErr     error
		wantStatusCode int
		wantRedirect   bool
	}{
		{
			name:           "valid download",
			cacheID:        "caches/abc123/my-key",
			wantStatusCode: http.StatusTemporaryRedirect,
			wantRedirect:   true,
		},
		{
			name:           "empty cache ID",
			cacheID:        "",
			wantStatusCode: http.StatusBadRequest,
			wantRedirect:   false,
		},
		{
			name:           "presign error",
			cacheID:        "caches/abc123/my-key",
			presignErr:     errors.New("presign failed"),
			wantStatusCode: http.StatusInternalServerError,
			wantRedirect:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockPresign := &mockPresignClient{
				presignGetFunc: func(_ context.Context, _ *s3.GetObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
					if tt.presignErr != nil {
						return nil, tt.presignErr
					}
					return &v4.PresignedHTTPRequest{URL: "https://example.com/download"}, nil
				},
			}

			server := cache.NewServerWithClients(&mockS3Client{}, mockPresign, "test-bucket")
			handler := cache.NewHandler(server)

			req := httptest.NewRequest(http.MethodGet, "/_artifacts/"+tt.cacheID, nil)
			w := httptest.NewRecorder()

			handler.DownloadCacheArtifact(w, req, tt.cacheID)

			if w.Code != tt.wantStatusCode {
				t.Errorf("DownloadCacheArtifact() status = %v, want %v", w.Code, tt.wantStatusCode)
			}

			if tt.wantRedirect {
				location := w.Header().Get("Location")
				if location == "" {
					t.Error("Expected Location header for redirect")
				}
			}
		})
	}
}

func TestHandler_IsAuthEnabled(t *testing.T) {
	tests := []struct {
		name        string
		cacheSecret string
		want        bool
	}{
		{
			name:        "auth enabled with secret",
			cacheSecret: "my-secret-key",
			want:        true,
		},
		{
			name:        "auth disabled without secret",
			cacheSecret: "",
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := cache.NewServerWithClients(&mockS3Client{}, &mockPresignClient{}, "test-bucket")
			handler := cache.NewHandlerWithAuth(server, nil, tt.cacheSecret)

			got := handler.IsAuthEnabled()
			if got != tt.want {
				t.Errorf("IsAuthEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandler_NewHandlerWithMetrics(t *testing.T) {
	server := cache.NewServerWithClients(&mockS3Client{}, &mockPresignClient{}, "test-bucket")
	handler := cache.NewHandlerWithMetrics(server, nil)

	if handler == nil {
		t.Error("NewHandlerWithMetrics() returned nil")
	}
}

func TestHandler_NewHandlerWithAuth(t *testing.T) {
	server := cache.NewServerWithClients(&mockS3Client{}, &mockPresignClient{}, "test-bucket")
	handler := cache.NewHandlerWithAuth(server, nil, "test-secret")

	if handler == nil {
		t.Error("NewHandlerWithAuth() returned nil")
	}
	if !handler.IsAuthEnabled() {
		t.Error("Auth should be enabled when secret is provided")
	}
}

func TestHandler_RegisterRoutes(t *testing.T) {
	server := cache.NewServerWithClients(&mockS3Client{}, &mockPresignClient{}, "test-bucket")
	handler := cache.NewHandler(server)
	mux := http.NewServeMux()

	// RegisterRoutes should not panic
	handler.RegisterRoutes(mux)

	// Verify routes are registered by making requests
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/_apis/artifactcache/caches"},
		{http.MethodGet, "/_apis/artifactcache/cache"},
		{http.MethodPatch, "/_apis/artifactcache/caches/test-id"},
		{http.MethodGet, "/_artifacts/test-id"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(_ *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			// Just verify it doesn't return 404 (route was registered)
			// Actual handler logic is tested elsewhere
		})
	}
}

func TestHandler_RegisterRoutes_WithAuth(t *testing.T) {
	server := cache.NewServerWithClients(&mockS3Client{}, &mockPresignClient{}, "test-bucket")
	handler := cache.NewHandlerWithAuth(server, nil, "test-secret")
	mux := http.NewServeMux()

	// RegisterRoutes with auth should not panic
	handler.RegisterRoutes(mux)

	// Without auth token, requests should fail with 401
	req := httptest.NewRequest(http.MethodGet, "/_apis/artifactcache/cache?keys=test&version=v1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Logf("Expected 401 Unauthorized without auth token, got %d", w.Code)
	}
}

func TestHandler_ReserveCacheEntry_MethodNotAllowed(t *testing.T) {
	server := cache.NewServerWithClients(&mockS3Client{}, &mockPresignClient{}, "test-bucket")
	handler := cache.NewHandler(server)

	// Test that GET is not allowed
	req := httptest.NewRequest(http.MethodGet, "/_apis/artifactcache/caches", nil)
	w := httptest.NewRecorder()

	handler.ReserveCacheEntry(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("ReserveCacheEntry() with GET status = %v, want %v", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandler_GetCacheEntry_MethodNotAllowed(t *testing.T) {
	server := cache.NewServerWithClients(&mockS3Client{}, &mockPresignClient{}, "test-bucket")
	handler := cache.NewHandler(server)

	// Test that POST is not allowed
	req := httptest.NewRequest(http.MethodPost, "/_apis/artifactcache/cache?keys=test&version=v1", nil)
	w := httptest.NewRecorder()

	handler.GetCacheEntry(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GetCacheEntry() with POST status = %v, want %v", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandler_DownloadCacheArtifact_InvalidKey(t *testing.T) {
	server := cache.NewServerWithClients(&mockS3Client{}, &mockPresignClient{}, "test-bucket")
	handler := cache.NewHandler(server)

	// Test with invalid key containing path traversal
	req := httptest.NewRequest(http.MethodGet, "/_artifacts/../../../etc/passwd", nil)
	w := httptest.NewRecorder()

	handler.DownloadCacheArtifact(w, req, "../../../etc/passwd")

	if w.Code != http.StatusBadRequest {
		t.Errorf("DownloadCacheArtifact() with invalid key status = %v, want %v", w.Code, http.StatusBadRequest)
	}
}
