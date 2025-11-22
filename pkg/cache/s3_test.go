package cache

import (
	"context"
	"fmt"
	"testing"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
		PresignPutObjectFunc: func(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
			if *params.Bucket != "test-bucket" {
				t.Errorf("Bucket = %s, want test-bucket", *params.Bucket)
			}
			if *params.Key != "test-key" {
				t.Errorf("Key = %s, want test-key", *params.Key)
			}
			return &v4.PresignedHTTPRequest{URL: "https://s3.amazonaws.com/test-bucket/test-key?signature=xyz"}, nil
		},
		PresignGetObjectFunc: func(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
			if *params.Bucket != "test-bucket" {
				t.Errorf("Bucket = %s, want test-bucket", *params.Bucket)
			}
			if *params.Key != "test-key" {
				t.Errorf("Key = %s, want test-key", *params.Key)
			}
			return &v4.PresignedHTTPRequest{URL: "https://s3.amazonaws.com/test-bucket/test-key?signature=abc"}, nil
		},
	}

	server := &CacheServer{
		presignClient:   mockPresign,
		cacheBucketName: "test-bucket",
	}

	tests := []struct {
		name    string
		method  string
		wantURL string
		wantErr bool
	}{
		{
			name:    "PUT request",
			method:  "PUT",
			wantURL: "https://s3.amazonaws.com/test-bucket/test-key?signature=xyz",
			wantErr: false,
		},
		{
			name:    "GET request",
			method:  "GET",
			wantURL: "https://s3.amazonaws.com/test-bucket/test-key?signature=abc",
			wantErr: false,
		},
		{
			name:    "Unsupported method",
			method:  "DELETE",
			wantURL: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := server.GeneratePresignedURL(context.Background(), "test-key", tt.method)
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
				HeadObjectFunc: func(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
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
			name:    "Cache miss",
			keys:    []string{"cache-key-2"},
			version: "v1",
			mockS3: &MockS3API{
				HeadObjectFunc: func(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					return nil, fmt.Errorf("NotFound")
				},
			},
			wantKey: "",
			found:   false,
			wantErr: false,
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &CacheServer{
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
