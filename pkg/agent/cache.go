package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3API defines S3 operations for runner caching.
type S3API interface {
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// Cache provides S3 caching for GitHub Actions runner binaries.
type Cache struct {
	s3Client   S3API
	bucketName string
}

// NewCache creates a new S3 cache client.
func NewCache(cfg aws.Config, bucketName string) *Cache {
	return &Cache{
		s3Client:   s3.NewFromConfig(cfg),
		bucketName: bucketName,
	}
}

// cacheKey returns the S3 key for a cached runner binary.
func (c *Cache) cacheKey(version, arch string) string {
	return fmt.Sprintf("runners/%s/%s/actions-runner-%s-%s.tar.gz", version, arch, version, arch)
}

// CheckCache checks if a runner binary is cached in S3.
// Returns whether it exists, the S3 key, and any error.
func (c *Cache) CheckCache(ctx context.Context, version, arch string) (bool, string, error) {
	key := c.cacheKey(version, arch)

	_, err := c.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		// Check if it's a not found error (cache miss)
		var notFoundErr *types.NotFound
		if errors.As(err, &notFoundErr) {
			return false, key, nil
		}
		// Return actual errors (network issues, permission denied, etc.)
		return false, key, fmt.Errorf("failed to check cache: %w", err)
	}

	return true, key, nil
}

// GetCachedRunner downloads a cached runner binary from S3.
func (c *Cache) GetCachedRunner(ctx context.Context, version, arch, destPath string) error {
	key := c.cacheKey(version, arch)

	output, err := c.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to get cached runner: %w", err)
	}
	defer func() {
		_ = output.Body.Close()
	}()

	// Create destination directory
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Create destination file
	file, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	// Copy content
	if _, err := io.Copy(file, output.Body); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// CacheRunner uploads a runner binary to S3 cache.
func (c *Cache) CacheRunner(ctx context.Context, version, arch, localPath string) error {
	key := c.cacheKey(version, arch)

	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	_, err = c.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucketName),
		Key:         aws.String(key),
		Body:        file,
		ContentType: aws.String("application/gzip"),
	})
	if err != nil {
		return fmt.Errorf("failed to upload runner: %w", err)
	}

	return nil
}

// DownloadFromCache wraps GetCachedRunner for CacheClient interface compatibility.
func (c *Cache) DownloadFromCache(ctx context.Context, version, arch, destPath string) error {
	return c.GetCachedRunner(ctx, version, arch, destPath)
}

// UploadToCache wraps CacheRunner for CacheClient interface compatibility.
func (c *Cache) UploadToCache(ctx context.Context, version, arch, sourcePath string) error {
	return c.CacheRunner(ctx, version, arch, sourcePath)
}
