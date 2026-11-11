package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/Shavakan/runs-fleet/pkg/cache/wire"
)

// maxMultipartParts is S3's part-number ceiling for one multipart upload.
const maxMultipartParts = 10000

var (
	// ErrMultipartDisabled is returned when the server has no multipart clients.
	ErrMultipartDisabled = errors.New("multipart upload not configured")
	// ErrMultipartS3 wraps a failed S3 call, as opposed to a rejected request.
	ErrMultipartS3 = errors.New("s3 multipart request failed")
)

// MultipartAPI defines the S3 multipart operations behind streamed cache uploads.
type MultipartAPI interface {
	CreateMultipartUpload(ctx context.Context, params *s3.CreateMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	CompleteMultipartUpload(ctx context.Context, params *s3.CompleteMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(ctx context.Context, params *s3.AbortMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// MultipartPresignAPI presigns the UploadPart requests a runner sends directly to S3.
type MultipartPresignAPI interface {
	PresignUploadPart(ctx context.Context, params *s3.UploadPartInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// SetMultipartClients enables streamed multipart uploads.
func (s *Server) SetMultipartClients(api MultipartAPI, presign MultipartPresignAPI) {
	s.multipart = api
	s.multipartPresign = presign
}

// MultipartEnabled reports whether the server can serve multipart uploads.
func (s *Server) MultipartEnabled() bool {
	return s.multipart != nil && s.multipartPresign != nil
}

func (s *Server) multipartKey(ctx context.Context, key, version, uploadID string) (string, error) {
	if !s.MultipartEnabled() {
		return "", ErrMultipartDisabled
	}
	if uploadID == "" {
		return "", fmt.Errorf("upload ID cannot be empty")
	}
	return s.CreateCacheEntry(ctx, key, version)
}

// CreateMultipartUpload starts a multipart upload for the cache entry and returns its upload ID.
func (s *Server) CreateMultipartUpload(ctx context.Context, key, version string) (string, error) {
	if !s.MultipartEnabled() {
		return "", ErrMultipartDisabled
	}
	cacheKey, err := s.CreateCacheEntry(ctx, key, version)
	if err != nil {
		return "", err
	}
	out, err := s.multipart.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.cacheBucketName),
		Key:    aws.String(cacheKey),
	})
	if err != nil {
		return "", fmt.Errorf("%w: create: %w", ErrMultipartS3, err)
	}
	if out.UploadId == nil || *out.UploadId == "" {
		return "", fmt.Errorf("%w: create returned no upload ID", ErrMultipartS3)
	}
	return *out.UploadId, nil
}

// PresignUploadPart returns a presigned URL for uploading one part.
func (s *Server) PresignUploadPart(ctx context.Context, key, version, uploadID string, partNumber int32) (string, error) {
	cacheKey, err := s.multipartKey(ctx, key, version, uploadID)
	if err != nil {
		return "", err
	}
	if partNumber < 1 || partNumber > maxMultipartParts {
		return "", fmt.Errorf("part number %d out of range", partNumber)
	}
	req, err := s.multipartPresign.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.cacheBucketName),
		Key:        aws.String(cacheKey),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(partNumber),
	}, s3.WithPresignExpires(15*time.Minute))
	if err != nil {
		return "", fmt.Errorf("%w: presign part: %w", ErrMultipartS3, err)
	}
	return req.URL, nil
}

// CompleteMultipartUpload assembles the uploaded parts, which must be in ascending part order.
func (s *Server) CompleteMultipartUpload(ctx context.Context, key, version, uploadID string, parts []wire.MultipartPart) error {
	cacheKey, err := s.multipartKey(ctx, key, version, uploadID)
	if err != nil {
		return err
	}
	if len(parts) == 0 || len(parts) > maxMultipartParts {
		return fmt.Errorf("part count %d out of range", len(parts))
	}
	completed := make([]types.CompletedPart, 0, len(parts))
	for i, p := range parts {
		if p.ETag == "" {
			return fmt.Errorf("part %d has no ETag", p.PartNumber)
		}
		if p.PartNumber < 1 || p.PartNumber > maxMultipartParts || (i > 0 && p.PartNumber <= parts[i-1].PartNumber) {
			return fmt.Errorf("part numbers must ascend within 1..%d", maxMultipartParts)
		}
		completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(p.PartNumber), ETag: aws.String(p.ETag)})
	}
	if _, err := s.multipart.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.cacheBucketName),
		Key:             aws.String(cacheKey),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		return fmt.Errorf("%w: complete: %w", ErrMultipartS3, err)
	}
	return nil
}

// AbortMultipartUpload discards an unfinished upload's parts.
func (s *Server) AbortMultipartUpload(ctx context.Context, key, version, uploadID string) error {
	cacheKey, err := s.multipartKey(ctx, key, version, uploadID)
	if err != nil {
		return err
	}
	if _, err := s.multipart.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.cacheBucketName),
		Key:      aws.String(cacheKey),
		UploadId: aws.String(uploadID),
	}); err != nil {
		return fmt.Errorf("%w: abort: %w", ErrMultipartS3, err)
	}
	return nil
}
