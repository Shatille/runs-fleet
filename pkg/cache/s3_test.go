package cache

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// MockS3API implements S3API interface
type MockS3API struct {
	HeadObjectFunc    func(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	ListObjectsV2Func func(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

func (m *MockS3API) HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if m.HeadObjectFunc != nil {
		return m.HeadObjectFunc(ctx, params, optFns...)
	}
	return &s3.HeadObjectOutput{}, nil
}

func (m *MockS3API) ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if m.ListObjectsV2Func != nil {
		return m.ListObjectsV2Func(ctx, params, optFns...)
	}
	return &s3.ListObjectsV2Output{Contents: []types.Object{}}, nil
}

// MockPresignAPI implements PresignAPI interface
type MockPresignAPI struct {
	PresignPutObjectFunc func(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
	PresignGetObjectFunc func(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

func (m *MockPresignAPI) PresignPutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	if m.PresignPutObjectFunc != nil {
		return m.PresignPutObjectFunc(ctx, params, optFns...)
	}
	return &v4.PresignedHTTPRequest{URL: "https://example.com/presigned-put"}, nil
}

func (m *MockPresignAPI) PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	if m.PresignGetObjectFunc != nil {
		return m.PresignGetObjectFunc(ctx, params, optFns...)
	}
	return &v4.PresignedHTTPRequest{URL: "https://example.com/presigned-get"}, nil
}

func TestGeneratePresignedURL(t *testing.T) {
	mockPresign := &MockPresignAPI{
		PresignPutObjectFunc: func(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
			if *params.Bucket != "test-bucket" {
				t.Errorf("Bucket = %s, want test-bucket", *params.Bucket)
			}
			if *params.Key != "test-key" {
				t.Errorf("Key = %s, want test-key", *params.Key)
			}
			return &v4.PresignedHTTPRequest{URL: "https://s3.amazonaws.com/test-bucket/test-key?signature=xyz"}, nil
		},
		PresignGetObjectFunc: func(_ context.Context, params *s3.GetObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
			if *params.Bucket != "test-bucket" {
				t.Errorf("Bucket = %s, want test-bucket", *params.Bucket)
			}
			if *params.Key != "test-key" {
				t.Errorf("Key = %s, want test-key", *params.Key)
			}
			return &v4.PresignedHTTPRequest{URL: "https://s3.amazonaws.com/test-bucket/test-key?signature=abc"}, nil
		},
	}

	server := &Server{
		presignClient:   mockPresign,
		cacheBucketName: "test-bucket",
	}

	tests := []struct {
		name    string
		key     string
		method  string
		wantURL string
		wantErr bool
	}{
		{
			name:    "PUT request",
			key:     "test-key",
			method:  http.MethodPut,
			wantURL: "https://s3.amazonaws.com/test-bucket/test-key?signature=xyz",
			wantErr: false,
		},
		{
			name:    "GET request",
			key:     "test-key",
			method:  http.MethodGet,
			wantURL: "https://s3.amazonaws.com/test-bucket/test-key?signature=abc",
			wantErr: false,
		},
		{
			name:    "Unsupported method",
			key:     "test-key",
			method:  "DELETE",
			wantURL: "",
			wantErr: true,
		},
		{
			name:    "Empty key",
			key:     "",
			method:  http.MethodPut,
			wantURL: "",
			wantErr: true,
		},
		{
			name:    "Path traversal attack",
			key:     "../../etc/passwd",
			method:  http.MethodGet,
			wantURL: "",
			wantErr: true,
		},
		{
			name:    "Backslash in key",
			key:     "test\\key",
			method:  http.MethodPut,
			wantURL: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := server.GeneratePresignedURL(context.Background(), tt.key, tt.method)
			if (err != nil) != tt.wantErr {
				t.Errorf("GeneratePresignedURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.wantURL {
				t.Errorf("GeneratePresignedURL() = %v, want %v", got, tt.wantURL)
			}
		})
	}
}

func TestGeneratePresignedURLPresignFailure(t *testing.T) {
	mockPresign := &MockPresignAPI{
		PresignPutObjectFunc: func(_ context.Context, _ *s3.PutObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
			return nil, fmt.Errorf("presign error")
		},
	}

	server := &Server{
		presignClient:   mockPresign,
		cacheBucketName: "test-bucket",
	}

	_, err := server.GeneratePresignedURL(context.Background(), "test-key", http.MethodPut)
	if err == nil {
		t.Error("GeneratePresignedURL() expected error on presign failure, got nil")
	}
}

func TestGetCacheEntry(t *testing.T) {
	tests := []struct {
		name    string
		keys    []string
		version string
		mockS3  *MockS3API
		wantKey string
		found   bool
		wantErr bool
	}{
		{
			name:    "Cache hit",
			keys:    []string{"cache-key-1"},
			version: "v1",
			mockS3: &MockS3API{
				HeadObjectFunc: func(_ context.Context, params *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					expectedKey := "caches/v1/cache-key-1"
					if *params.Key != expectedKey {
						t.Errorf("Key = %s, want %s", *params.Key, expectedKey)
					}
					return &s3.HeadObjectOutput{}, nil
				},
			},
			wantKey: "caches/v1/cache-key-1",
			found:   true,
			wantErr: false,
		},
		{
			name:    "Cache miss - NoSuchKey",
			keys:    []string{"cache-key-2"},
			version: "v1",
			mockS3: &MockS3API{
				HeadObjectFunc: func(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					return nil, &types.NoSuchKey{Message: aws.String("not found")}
				},
			},
			wantKey: "",
			found:   false,
			wantErr: false,
		},
		{
			name:    "S3 API error",
			keys:    []string{"cache-key-3"},
			version: "v1",
			mockS3: &MockS3API{
				HeadObjectFunc: func(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					return nil, fmt.Errorf("permission denied")
				},
			},
			wantKey: "",
			found:   false,
			wantErr: true,
		},
		{
			name:    "No keys provided",
			keys:    []string{},
			version: "v1",
			mockS3:  &MockS3API{},
			wantKey: "",
			found:   false,
			wantErr: false,
		},
		{
			name:    "Path traversal in key",
			keys:    []string{"../../etc/passwd"},
			version: "v1",
			mockS3:  &MockS3API{},
			wantKey: "",
			found:   false,
			wantErr: true,
		},
		{
			name:    "Path traversal in version",
			keys:    []string{"valid-key"},
			version: "../",
			mockS3:  &MockS3API{},
			wantKey: "",
			found:   false,
			wantErr: true,
		},
		{
			name:    "Empty key in array",
			keys:    []string{""},
			version: "v1",
			mockS3:  &MockS3API{},
			wantKey: "",
			found:   false,
			wantErr: true,
		},
		{
			name:    "Empty version",
			keys:    []string{"valid-key"},
			version: "",
			mockS3:  &MockS3API{},
			wantKey: "",
			found:   false,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				s3Client:        tt.mockS3,
				cacheBucketName: "test-bucket",
			}

			key, found, err := server.GetCacheEntry(context.Background(), tt.keys, tt.version)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetCacheEntry() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if found != tt.found {
				t.Errorf("GetCacheEntry() found = %v, want %v", found, tt.found)
			}
			if key != tt.wantKey {
				t.Errorf("GetCacheEntry() key = %v, want %v", key, tt.wantKey)
			}
		})
	}
}

func TestGetCacheEntry_RestoreKeyPrefixMatching(t *testing.T) {
	now := aws.Time(timeNow())

	tests := []struct {
		name    string
		keys    []string
		version string
		mockS3  *MockS3API
		wantKey string
		found   bool
	}{
		{
			name:    "Restore key prefix matching - finds most recent",
			keys:    []string{"node-modules-abc123", "node-modules-"},
			version: "v1",
			mockS3: &MockS3API{
				HeadObjectFunc: func(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					return nil, &types.NotFound{Message: aws.String("not found")}
				},
				ListObjectsV2Func: func(_ context.Context, params *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
					if *params.Prefix == "caches/v1/node-modules-" {
						return &s3.ListObjectsV2Output{
							Contents: []types.Object{
								{Key: aws.String("caches/v1/node-modules-old"), LastModified: aws.Time(now.Add(-24 * 60 * 60 * 1e9))},
								{Key: aws.String("caches/v1/node-modules-recent"), LastModified: now},
							},
						}, nil
					}
					return &s3.ListObjectsV2Output{}, nil
				},
			},
			wantKey: "caches/v1/node-modules-recent",
			found:   true,
		},
		{
			name:    "Restore key prefix matching - no matches",
			keys:    []string{"node-modules-abc123", "node-modules-"},
			version: "v1",
			mockS3: &MockS3API{
				HeadObjectFunc: func(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					return nil, &types.NotFound{Message: aws.String("not found")}
				},
				ListObjectsV2Func: func(_ context.Context, _ *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
					return &s3.ListObjectsV2Output{Contents: []types.Object{}}, nil
				},
			},
			wantKey: "",
			found:   false,
		},
		{
			name:    "Primary key hit - no prefix matching needed",
			keys:    []string{"node-modules-exact", "node-modules-"},
			version: "v1",
			mockS3: &MockS3API{
				HeadObjectFunc: func(_ context.Context, params *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					if *params.Key == "caches/v1/node-modules-exact" {
						return &s3.HeadObjectOutput{}, nil
					}
					return nil, &types.NotFound{Message: aws.String("not found")}
				},
			},
			wantKey: "caches/v1/node-modules-exact",
			found:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				s3Client:        tt.mockS3,
				cacheBucketName: "test-bucket",
			}

			key, found, err := server.GetCacheEntry(context.Background(), tt.keys, tt.version)
			if err != nil {
				t.Errorf("GetCacheEntry() unexpected error = %v", err)
				return
			}
			if found != tt.found {
				t.Errorf("GetCacheEntry() found = %v, want %v", found, tt.found)
			}
			if key != tt.wantKey {
				t.Errorf("GetCacheEntry() key = %v, want %v", key, tt.wantKey)
			}
		})
	}
}

func timeNow() time.Time {
	return time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
}

func TestGetCacheEntry_WithScope(t *testing.T) {
	tests := []struct {
		name    string
		scope   string
		keys    []string
		version string
		mockS3  *MockS3API
		wantKey string
		found   bool
	}{
		{
			name:    "Cache hit with scope",
			scope:   "myorg/myrepo",
			keys:    []string{"cache-key"},
			version: "v1",
			mockS3: &MockS3API{
				HeadObjectFunc: func(_ context.Context, params *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					expectedKey := "caches/myorg/myrepo/v1/cache-key"
					if *params.Key != expectedKey {
						t.Errorf("Key = %s, want %s", *params.Key, expectedKey)
					}
					return &s3.HeadObjectOutput{}, nil
				},
			},
			wantKey: "caches/myorg/myrepo/v1/cache-key",
			found:   true,
		},
		{
			name:    "Cache hit with org-only scope",
			scope:   "myorg",
			keys:    []string{"shared-cache"},
			version: "v1",
			mockS3: &MockS3API{
				HeadObjectFunc: func(_ context.Context, params *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					expectedKey := "caches/myorg/v1/shared-cache"
					if *params.Key != expectedKey {
						t.Errorf("Key = %s, want %s", *params.Key, expectedKey)
					}
					return &s3.HeadObjectOutput{}, nil
				},
			},
			wantKey: "caches/myorg/v1/shared-cache",
			found:   true,
		},
		{
			name:    "Cache miss with scope",
			scope:   "myorg/myrepo",
			keys:    []string{"missing-key"},
			version: "v1",
			mockS3: &MockS3API{
				HeadObjectFunc: func(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					return nil, &types.NotFound{Message: aws.String("not found")}
				},
			},
			wantKey: "",
			found:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				s3Client:        tt.mockS3,
				cacheBucketName: "test-bucket",
				defaultScope:    tt.scope,
			}

			key, found, err := server.GetCacheEntry(context.Background(), tt.keys, tt.version)
			if err != nil {
				t.Errorf("GetCacheEntry() unexpected error = %v", err)
				return
			}
			if found != tt.found {
				t.Errorf("GetCacheEntry() found = %v, want %v", found, tt.found)
			}
			if key != tt.wantKey {
				t.Errorf("GetCacheEntry() key = %v, want %v", key, tt.wantKey)
			}
		})
	}
}

func TestCreateCacheEntry_WithScope(t *testing.T) {
	tests := []struct {
		name    string
		scope   string
		key     string
		version string
		want    string
	}{
		{
			name:    "Entry with repo scope",
			scope:   "myorg/myrepo",
			key:     "cache-key",
			version: "v1",
			want:    "caches/myorg/myrepo/v1/cache-key",
		},
		{
			name:    "Entry with org scope",
			scope:   "myorg",
			key:     "shared-cache",
			version: "v1",
			want:    "caches/myorg/v1/shared-cache",
		},
		{
			name:    "Entry without scope",
			scope:   "",
			key:     "cache-key",
			version: "v1",
			want:    "caches/v1/cache-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				cacheBucketName: "test-bucket",
				defaultScope:    tt.scope,
			}

			got, err := server.CreateCacheEntry(context.Background(), tt.key, tt.version)
			if err != nil {
				t.Errorf("CreateCacheEntry() unexpected error = %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("CreateCacheEntry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWithScope(t *testing.T) {
	original := &Server{
		cacheBucketName: "test-bucket",
		defaultScope:    "",
	}

	scoped := original.WithScope("myorg/myrepo")

	// Original should be unchanged
	if original.defaultScope != "" {
		t.Errorf("Original server scope should be empty, got %q", original.defaultScope)
	}

	// Scoped should have the scope
	if scoped.defaultScope != "myorg/myrepo" {
		t.Errorf("Scoped server scope should be 'myorg/myrepo', got %q", scoped.defaultScope)
	}

	// Both should share the same bucket name
	if scoped.cacheBucketName != original.cacheBucketName {
		t.Errorf("Scoped server bucket should match original")
	}
}

func TestNewServerWithClientsAndScope_InvalidScope(t *testing.T) {
	mockS3 := &MockS3API{}
	mockPresign := &MockPresignAPI{}

	// Invalid scope should result in empty scope
	server := NewServerWithClientsAndScope(mockS3, mockPresign, "test-bucket", "../etc")
	if server.defaultScope != "" {
		t.Errorf("Invalid scope should be rejected, got %q", server.defaultScope)
	}

	// Valid scope should be accepted
	server = NewServerWithClientsAndScope(mockS3, mockPresign, "test-bucket", "org/repo")
	if server.defaultScope != "org/repo" {
		t.Errorf("Valid scope should be set, got %q", server.defaultScope)
	}
}

func TestSetScope(t *testing.T) {
	server := &Server{cacheBucketName: "test-bucket"}

	// Valid scope should succeed
	if err := server.SetScope("org/repo"); err != nil {
		t.Errorf("SetScope with valid scope failed: %v", err)
	}
	if server.defaultScope != "org/repo" {
		t.Errorf("SetScope should set scope, got %q", server.defaultScope)
	}

	// Empty scope is valid
	if err := server.SetScope(""); err != nil {
		t.Errorf("SetScope with empty scope failed: %v", err)
	}

	// Invalid scope should fail
	if err := server.SetScope("../etc"); err == nil {
		t.Error("SetScope should reject path traversal")
	}

	// Too many parts should fail
	if err := server.SetScope("org/repo/extra"); err == nil {
		t.Error("SetScope should reject too many parts")
	}
}

func TestWithScope_InvalidScope(t *testing.T) {
	original := &Server{
		cacheBucketName: "test-bucket",
		defaultScope:    "original-scope",
	}

	// Path traversal attempt should return original server
	result := original.WithScope("../../../etc")
	if result != original {
		t.Error("WithScope with path traversal should return original server")
	}
	if result.defaultScope != "original-scope" {
		t.Errorf("Scope should be unchanged, got %q", result.defaultScope)
	}

	// Too many parts should return original server
	result = original.WithScope("org/repo/extra")
	if result != original {
		t.Error("WithScope with too many parts should return original server")
	}
}

func TestCreateCacheEntry(t *testing.T) {
	server := &Server{cacheBucketName: "test-bucket"}

	tests := []struct {
		name    string
		key     string
		version string
		want    string
		wantErr bool
	}{
		{
			name:    "Valid entry",
			key:     "valid-key",
			version: "v1",
			want:    "caches/v1/valid-key",
			wantErr: false,
		},
		{
			name:    "Path traversal in key",
			key:     "../../etc/passwd",
			version: "v1",
			want:    "",
			wantErr: true,
		},
		{
			name:    "Path traversal in version",
			key:     "valid-key",
			version: "../etc",
			want:    "",
			wantErr: true,
		},
		{
			name:    "Empty key",
			key:     "",
			version: "v1",
			want:    "",
			wantErr: true,
		},
		{
			name:    "Empty version",
			key:     "valid-key",
			version: "",
			want:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := server.CreateCacheEntry(context.Background(), tt.key, tt.version)
			if (err != nil) != tt.wantErr {
				t.Errorf("CreateCacheEntry() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("CreateCacheEntry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateKey(t *testing.T) {
	maxKeyLen := 504
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"valid simple key", "my-cache-key", false},
		{"valid key with slashes", "npm/cache/key", false},
		{"valid key with dots", "my.cache.key", false},
		{"valid key at max length", strings.Repeat("a", maxKeyLen), false},
		{"empty key", "", true},
		{"key exceeds max length", strings.Repeat("a", maxKeyLen+1), true},
		{"key with double dots", "path/../traversal", true},
		{"key with backslash", "path\\windows", true},
		{"key with null byte", "key\x00null", true},
		{"key starting with slash", "/absolute/path", true},
		{"key with consecutive slashes", "path//to/key", false},
		{"key with trailing slash", "path/to/", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateKey(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			}
		})
	}
}

func TestValidateVersion(t *testing.T) {
	maxVersionLen := 512
	tests := []struct {
		name    string
		version string
		wantErr bool
	}{
		{"valid simple version", "v1.0.0", false},
		{"valid hash version", "abc123def456", false},
		{"valid version with dashes", "sha256-abc123", false},
		{"valid version at max length", strings.Repeat("a", maxVersionLen), false},
		{"empty version", "", true},
		{"version exceeds max length", strings.Repeat("a", maxVersionLen+1), true},
		{"version with double dots", "v1..0", true},
		{"version with backslash", "v1\\0", true},
		{"version with null byte", "v1\x00null", true},
		{"version starting with slash", "/v1.0.0", true},
		{"version with consecutive slashes", "v1//0", false},
		{"version with trailing slash", "v1.0/", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVersion(tt.version)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateVersion(%q) error = %v, wantErr %v", tt.version, err, tt.wantErr)
			}
		})
	}
}

func TestValidateScope(t *testing.T) {
	tests := []struct {
		name    string
		scope   string
		wantErr bool
	}{
		{"empty scope", "", false},
		{"valid org only", "myorg", false},
		{"valid org/repo", "myorg/myrepo", false},
		{"scope with double dots", "../escape", true},
		{"scope with backslash", "org\\repo", true},
		{"scope with null byte", "org\x00repo", true},
		{"scope starting with slash", "/org/repo", true},
		{"scope with too many parts", "org/repo/extra", true},
		{"scope with trailing slash", "org/", true},
		{"scope with consecutive slashes", "org//repo", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateScope(tt.scope)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateScope(%q) error = %v, wantErr %v", tt.scope, err, tt.wantErr)
			}
		})
	}
}

func TestBuildCachePrefix(t *testing.T) {
	tests := []struct {
		name      string
		scope     string
		version   string
		keyPrefix string
		want      string
	}{
		{
			name:      "without scope",
			scope:     "",
			version:   "v1",
			keyPrefix: "npm-",
			want:      "caches/v1/npm-",
		},
		{
			name:      "with scope",
			scope:     "owner/repo",
			version:   "v1",
			keyPrefix: "npm-",
			want:      "caches/owner/repo/v1/npm-",
		},
		{
			name:      "empty key prefix",
			scope:     "",
			version:   "v1",
			keyPrefix: "",
			want:      "caches/v1/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				cacheBucketName: "test-bucket",
				defaultScope:    tt.scope,
			}
			got := server.buildCachePrefix(tt.version, tt.keyPrefix)
			if got != tt.want {
				t.Errorf("buildCachePrefix() = %q, want %q", got, tt.want)
			}
		})
	}
}
