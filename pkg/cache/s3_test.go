package cache

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// MockS3API implements S3API interface
type MockS3API struct {
	HeadObjectFunc func(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

func (m *MockS3API) HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if m.HeadObjectFunc != nil {
		return m.HeadObjectFunc(ctx, params, optFns...)
	}
	return &s3.HeadObjectOutput{}, nil
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
		{
			name:    "Fallback key hit - primary misses, secondary hits",
			keys:    []string{"primary-key", "fallback-key"},
			version: "v1",
			mockS3: &MockS3API{
				HeadObjectFunc: func(_ context.Context, params *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					if *params.Key == "caches/v1/primary-key" {
						return nil, &types.NoSuchKey{Message: aws.String("not found")}
					}
					if *params.Key == "caches/v1/fallback-key" {
						return &s3.HeadObjectOutput{}, nil
					}
					return nil, fmt.Errorf("unexpected key: %s", *params.Key)
				},
			},
			wantKey: "caches/v1/fallback-key",
			found:   true,
			wantErr: false,
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
