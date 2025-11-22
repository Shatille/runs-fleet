// Package cache implements GitHub Actions cache protocol backed by S3.
package cache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3API defines S3 operations required for cache server.
type S3API interface {
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

// PresignAPI defines pre-signing operations for S3 URLs.
type PresignAPI interface {
	PresignPutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
	PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// Server implements GitHub Actions cache protocol using S3 backend.
type Server struct {
	s3Client        S3API
	presignClient   PresignAPI
	cacheBucketName string
}

// NewServer creates a new cache server backed by S3.
func NewServer(cfg aws.Config, bucketName string) *Server {
	client := s3.NewFromConfig(cfg)
	return &Server{
		s3Client:        client,
		presignClient:   s3.NewPresignClient(client),
		cacheBucketName: bucketName,
	}
}

func validateKey(s string) error {
	if s == "" {
		return fmt.Errorf("key cannot be empty")
	}
	// GitHub Actions cache API spec allows up to 512 characters for cache keys.
	// We use 504 bytes to allow for potential encoding overhead or additional metadata.
	// See: https://docs.github.com/en/rest/actions/cache?apiVersion=2022-11-28#create-a-cache
	if len(s) > 504 {
		return fmt.Errorf("key exceeds maximum length of 504 bytes")
	}
	if strings.Contains(s, "..") || strings.Contains(s, "\\") || strings.ContainsRune(s, 0) {
		return fmt.Errorf("key contains invalid characters")
	}
	if strings.HasPrefix(s, "/") {
		return fmt.Errorf("key cannot start with /")
	}
	return nil
}

func validateVersion(s string) error {
	if s == "" {
		return fmt.Errorf("version cannot be empty")
	}
	// GitHub Actions cache API spec allows up to 512 bytes for version strings.
	// Version is typically a hash of dependencies (e.g., package-lock.json hash).
	if len(s) > 512 {
		return fmt.Errorf("version exceeds maximum length of 512 bytes")
	}
	if strings.Contains(s, "..") || strings.Contains(s, "\\") || strings.ContainsRune(s, 0) {
		return fmt.Errorf("version contains invalid characters")
	}
	if strings.HasPrefix(s, "/") {
		return fmt.Errorf("version cannot start with /")
	}
	return nil
}

// GeneratePresignedURL generates a pre-signed URL for uploading or downloading cache artifacts
func (s *Server) GeneratePresignedURL(ctx context.Context, key string, method string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", fmt.Errorf("invalid key: %w", err)
	}
	var req *v4.PresignedHTTPRequest
	var err error

	expiration := 15 * time.Minute

	switch method {
	case http.MethodPut:
		req, err = s.presignClient.PresignPutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.cacheBucketName),
			Key:    aws.String(key),
		}, s3.WithPresignExpires(expiration))
	case http.MethodGet:
		req, err = s.presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.cacheBucketName),
			Key:    aws.String(key),
		}, s3.WithPresignExpires(expiration))
	default:
		return "", fmt.Errorf("unsupported method: %s", method)
	}

	if err != nil {
		return "", fmt.Errorf("failed to presign request: %w", err)
	}

	return req.URL, nil
}

// GetCacheEntry checks if a cache entry exists (metadata lookup)
// GitHub Actions cache protocol uses keys array as primary + restore-keys fallbacks
func (s *Server) GetCacheEntry(ctx context.Context, keys []string, version string) (string, bool, error) {
	if len(keys) == 0 {
		return "", false, nil
	}

	if err := validateVersion(version); err != nil {
		return "", false, fmt.Errorf("invalid version: %w", err)
	}

	for _, k := range keys {
		if err := validateKey(k); err != nil {
			return "", false, fmt.Errorf("invalid key: %w", err)
		}
	}

	for _, k := range keys {
		key := fmt.Sprintf("caches/%s/%s", version, k)

		_, err := s.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(s.cacheBucketName),
			Key:    aws.String(key),
		})

		if err != nil {
			var notFound *types.NoSuchKey
			if errors.As(err, &notFound) {
				continue
			}
			return "", false, fmt.Errorf("failed to check cache entry: %w", err)
		}

		return key, true, nil
	}

	return "", false, nil
}

// CreateCacheEntry prepares for a new cache upload
func (s *Server) CreateCacheEntry(_ context.Context, key string, version string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", fmt.Errorf("invalid key: %w", err)
	}
	if err := validateVersion(version); err != nil {
		return "", fmt.Errorf("invalid version: %w", err)
	}
	return fmt.Sprintf("caches/%s/%s", version, key), nil
}

// CommitCacheEntry finalizes the cache entry
// No-op for S3 backend since uploads are atomic via pre-signed URLs
func (s *Server) CommitCacheEntry(_ context.Context, _ string) error {
	return nil
}

// NewServerWithClients creates a cache server with custom clients for testing.
func NewServerWithClients(s3Client S3API, presignClient PresignAPI, bucketName string) *Server {
	return &Server{
		s3Client:        s3Client,
		presignClient:   presignClient,
		cacheBucketName: bucketName,
	}
}
