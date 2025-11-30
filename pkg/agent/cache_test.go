package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type mockS3API struct {
	headObjectFunc func(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	getObjectFunc  func(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	putObjectFunc  func(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

func (m *mockS3API) HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if m.headObjectFunc != nil {
		return m.headObjectFunc(ctx, params, optFns...)
	}
	return &s3.HeadObjectOutput{}, nil
}

func (m *mockS3API) GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if m.getObjectFunc != nil {
		return m.getObjectFunc(ctx, params, optFns...)
	}
	return &s3.GetObjectOutput{
		Body: io.NopCloser(bytes.NewReader([]byte("test content"))),
	}, nil
}

func (m *mockS3API) PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if m.putObjectFunc != nil {
		return m.putObjectFunc(ctx, params, optFns...)
	}
	return &s3.PutObjectOutput{}, nil
}

func TestCache_CacheKey(t *testing.T) {
	cache := &Cache{bucketName: "test-bucket"}

	key := cache.cacheKey("2.300.0", "linux-arm64")
	expected := "runners/2.300.0/linux-arm64/actions-runner-2.300.0-linux-arm64.tar.gz"

	if key != expected {
		t.Errorf("expected key %q, got %q", expected, key)
	}
}

func TestCache_CheckCache_Hit(t *testing.T) {
	mock := &mockS3API{
		headObjectFunc: func(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			return &s3.HeadObjectOutput{}, nil
		},
	}

	cache := &Cache{
		s3Client:   mock,
		bucketName: "test-bucket",
	}

	exists, key, err := cache.CheckCache(context.Background(), "2.300.0", "linux-arm64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Error("expected cache hit")
	}
	if key == "" {
		t.Error("expected non-empty key")
	}
}

func TestCache_CheckCache_Miss(t *testing.T) {
	mock := &mockS3API{
		headObjectFunc: func(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			return nil, &types.NotFound{}
		},
	}

	cache := &Cache{
		s3Client:   mock,
		bucketName: "test-bucket",
	}

	exists, key, err := cache.CheckCache(context.Background(), "2.300.0", "linux-arm64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected cache miss")
	}
	if key == "" {
		t.Error("expected non-empty key")
	}
}

func TestCache_CheckCache_Error(t *testing.T) {
	mock := &mockS3API{
		headObjectFunc: func(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			return nil, errors.New("network error")
		},
	}

	cache := &Cache{
		s3Client:   mock,
		bucketName: "test-bucket",
	}

	_, _, err := cache.CheckCache(context.Background(), "2.300.0", "linux-arm64")
	if err == nil {
		t.Error("expected error")
	}
}

func TestCache_GetCachedRunner(t *testing.T) {
	testContent := []byte("test runner binary content")
	mock := &mockS3API{
		getObjectFunc: func(_ context.Context, _ *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{
				Body: io.NopCloser(bytes.NewReader(testContent)),
			}, nil
		},
	}

	cache := &Cache{
		s3Client:   mock,
		bucketName: "test-bucket",
	}

	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "runner.tar.gz")

	err := cache.GetCachedRunner(context.Background(), "2.300.0", "linux-arm64", destPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	if !bytes.Equal(content, testContent) {
		t.Errorf("content mismatch: expected %q, got %q", testContent, content)
	}
}

func TestCache_GetCachedRunner_Error(t *testing.T) {
	mock := &mockS3API{
		getObjectFunc: func(_ context.Context, _ *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
			return nil, errors.New("access denied")
		},
	}

	cache := &Cache{
		s3Client:   mock,
		bucketName: "test-bucket",
	}

	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "runner.tar.gz")

	err := cache.GetCachedRunner(context.Background(), "2.300.0", "linux-arm64", destPath)
	if err == nil {
		t.Error("expected error")
	}
}

func TestCache_CacheRunner(t *testing.T) {
	var uploadedKey string
	mock := &mockS3API{
		putObjectFunc: func(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			uploadedKey = *params.Key
			return &s3.PutObjectOutput{}, nil
		},
	}

	cache := &Cache{
		s3Client:   mock,
		bucketName: "test-bucket",
	}

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "runner.tar.gz")
	if err := os.WriteFile(localPath, []byte("test content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	err := cache.CacheRunner(context.Background(), "2.300.0", "linux-arm64", localPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedKey := "runners/2.300.0/linux-arm64/actions-runner-2.300.0-linux-arm64.tar.gz"
	if uploadedKey != expectedKey {
		t.Errorf("expected key %q, got %q", expectedKey, uploadedKey)
	}
}

func TestCache_CacheRunner_FileNotFound(t *testing.T) {
	mock := &mockS3API{}

	cache := &Cache{
		s3Client:   mock,
		bucketName: "test-bucket",
	}

	err := cache.CacheRunner(context.Background(), "2.300.0", "linux-arm64", "/nonexistent/file.tar.gz")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestCache_DownloadFromCache(t *testing.T) {
	mock := &mockS3API{
		getObjectFunc: func(_ context.Context, _ *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{
				Body: io.NopCloser(bytes.NewReader([]byte("test"))),
			}, nil
		},
	}

	cache := &Cache{
		s3Client:   mock,
		bucketName: "test-bucket",
	}

	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "runner.tar.gz")

	err := cache.DownloadFromCache(context.Background(), "2.300.0", "linux-arm64", destPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCache_UploadToCache(t *testing.T) {
	mock := &mockS3API{
		putObjectFunc: func(_ context.Context, _ *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			return &s3.PutObjectOutput{}, nil
		},
	}

	cache := &Cache{
		s3Client:   mock,
		bucketName: "test-bucket",
	}

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "runner.tar.gz")
	if err := os.WriteFile(localPath, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	err := cache.UploadToCache(context.Background(), "2.300.0", "linux-arm64", localPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
