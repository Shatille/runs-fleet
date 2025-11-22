package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3API interface {
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

type PresignAPI interface {
	PresignPutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
	PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

type CacheServer struct {
	s3Client        S3API
	presignClient   PresignAPI
	cacheBucketName string
}

func NewServer(cfg aws.Config, bucketName string) *CacheServer {
	client := s3.NewFromConfig(cfg)
	return &CacheServer{
		s3Client:        client,
		presignClient:   s3.NewPresignClient(client),
		cacheBucketName: bucketName,
	}
}

// GeneratePresignedURL generates a pre-signed URL for uploading or downloading cache artifacts
func (s *CacheServer) GeneratePresignedURL(ctx context.Context, key string, method string) (string, error) {
	var req *v4.PresignedHTTPRequest
	var err error

	expiration := 15 * time.Minute

	if method == "PUT" {
		req, err = s.presignClient.PresignPutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.cacheBucketName),
			Key:    aws.String(key),
		}, s3.WithPresignExpires(expiration))
	} else if method == "GET" {
		req, err = s.presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.cacheBucketName),
			Key:    aws.String(key),
		}, s3.WithPresignExpires(expiration))
	} else {
		return "", fmt.Errorf("unsupported method: %s", method)
	}

	if err != nil {
		return "", fmt.Errorf("failed to presign request: %w", err)
	}

	return req.URL, nil
}

// GetCacheEntry checks if a cache entry exists (metadata lookup)
// In a real implementation, this would check S3 metadata or a DynamoDB index
func (s *CacheServer) GetCacheEntry(ctx context.Context, keys []string, version string) (string, bool, error) {
	// Placeholder: Check if the first key exists in S3
	// Real implementation needs to handle restore keys and version matching
	if len(keys) == 0 {
		return "", false, nil
	}

	key := fmt.Sprintf("caches/%s/%s", version, keys[0]) // Simplified path structure

	_, err := s.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cacheBucketName),
		Key:    aws.String(key),
	})

	if err != nil {
		// If not found, return false
		return "", false, nil
	}

	return key, true, nil
}

// CreateCacheEntry prepares for a new cache upload
func (s *CacheServer) CreateCacheEntry(ctx context.Context, key string, version string) (string, error) {
	// Return the S3 key where the file should be uploaded
	return fmt.Sprintf("caches/%s/%s", version, key), nil
}

// CommitCacheEntry finalizes the cache entry (if needed)
func (s *CacheServer) CommitCacheEntry(ctx context.Context, id string) error {
	// S3 upload is direct, so commit might just be metadata update
	return nil
}
