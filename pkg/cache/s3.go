// Package cache implements GitHub Actions cache protocol backed by S3.
package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/Shavakan/runs-fleet/pkg/logging"
)

var s3Log = logging.WithComponent(logging.LogTypeCache, "s3")

// S3API defines S3 operations required for cache server.
type S3API interface {
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
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
	defaultScope    string // Optional default scope (org/repo) for cache isolation
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

// SetScope sets the cache scope for repository isolation.
// Returns an error if the scope is invalid.
func (s *Server) SetScope(scope string) error {
	if err := validateScope(scope); err != nil {
		return fmt.Errorf("invalid scope: %w", err)
	}
	s.defaultScope = scope
	return nil
}

// WithScope returns a shallow copy of the server with the specified scope.
// This is useful for per-request scoping without modifying the original server.
// If the scope is invalid, returns the original server unchanged.
func (s *Server) WithScope(scope string) *Server {
	if err := validateScope(scope); err != nil {
		return s
	}
	return &Server{
		s3Client:        s.s3Client,
		presignClient:   s.presignClient,
		cacheBucketName: s.cacheBucketName,
		defaultScope:    scope,
	}
}

// buildCacheKey constructs the full S3 key with optional scope prefix.
func (s *Server) buildCacheKey(version, key string) string {
	if s.defaultScope != "" {
		return fmt.Sprintf("caches/%s/%s/%s", s.defaultScope, version, key)
	}
	return fmt.Sprintf("caches/%s/%s", version, key)
}

// buildCachePrefix constructs a prefix for listing cache entries.
func (s *Server) buildCachePrefix(version, keyPrefix string) string {
	if s.defaultScope != "" {
		return fmt.Sprintf("caches/%s/%s/%s", s.defaultScope, version, keyPrefix)
	}
	return fmt.Sprintf("caches/%s/%s", version, keyPrefix)
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

func validateScope(s string) error {
	if s == "" {
		return nil // Empty scope is valid (no isolation)
	}
	if strings.Contains(s, "..") || strings.Contains(s, "\\") || strings.ContainsRune(s, 0) {
		return fmt.Errorf("scope contains invalid characters")
	}
	if strings.HasPrefix(s, "/") {
		return fmt.Errorf("scope cannot start with /")
	}
	// Scope should be "org" or "org/repo" format
	parts := strings.Split(s, "/")
	if len(parts) > 2 {
		return fmt.Errorf("scope must be 'org' or 'org/repo' format")
	}
	// Reject empty parts (e.g., "org/" or "org//repo")
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("scope cannot contain empty parts")
		}
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

// Match describes a cache lookup hit: the S3 object, the key as the client
// compares it, whether the primary key matched, and when the object was written.
type Match struct {
	Key      string
	BareKey  string
	Exact    bool
	Modified time.Time
}

// GetCacheEntry resolves keys against the cache: keys[0] must match exactly,
// keys[1:] are restore-key prefixes resolved to their newest object.
func (s *Server) GetCacheEntry(ctx context.Context, keys []string, version string) (Match, bool, error) {
	if len(keys) == 0 {
		return Match{}, false, nil
	}

	if err := validateVersion(version); err != nil {
		return Match{}, false, fmt.Errorf("invalid version: %w", err)
	}

	for _, k := range keys {
		if err := validateKey(k); err != nil {
			return Match{}, false, fmt.Errorf("invalid key: %w", err)
		}
	}

	primaryKey := s.buildCacheKey(version, keys[0])
	head, err := s.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cacheBucketName),
		Key:    aws.String(primaryKey),
	})
	if err == nil {
		return Match{Key: primaryKey, BareKey: keys[0], Exact: true, Modified: aws.ToTime(head.LastModified)}, true, nil
	}

	var notFound *types.NoSuchKey
	var notFoundErr *types.NotFound
	if !errors.As(err, &notFound) && !errors.As(err, &notFoundErr) {
		if !strings.Contains(err.Error(), "NotFound") && !strings.Contains(err.Error(), "404") {
			return Match{}, false, fmt.Errorf("failed to check cache entry: %w", err)
		}
	}

	for _, restoreKey := range keys[1:] {
		obj, found, err := s.findCacheByPrefix(ctx, version, restoreKey)
		if err != nil {
			return Match{}, false, err
		}
		if found {
			return Match{
				Key:      *obj.Key,
				BareKey:  strings.TrimPrefix(*obj.Key, s.buildCacheKey(version, "")),
				Modified: aws.ToTime(obj.LastModified),
			}, true, nil
		}
	}

	return Match{}, false, nil
}

const (
	prefixListPageSize int32 = 1000
	prefixListMaxPages int   = 10
)

// findCacheByPrefix returns the most recently modified object under the prefix,
// scanning up to prefixListMaxPages pages.
func (s *Server) findCacheByPrefix(ctx context.Context, version, keyPrefix string) (*types.Object, bool, error) {
	prefix := s.buildCachePrefix(version, keyPrefix)
	pages := s3.NewListObjectsV2Paginator(s.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cacheBucketName),
		Prefix: aws.String(prefix),
	}, func(o *s3.ListObjectsV2PaginatorOptions) { o.Limit = prefixListPageSize })

	var best *types.Object
	for page := 1; pages.HasMorePages(); page++ {
		if page > prefixListMaxPages {
			s3Log.Warn(ctx, "restore-key search truncated",
				slog.String("prefix", prefix),
				slog.Int("pages", prefixListMaxPages))
			break
		}
		output, err := pages.NextPage(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("failed to list cache entries: %w", err)
		}
		for i := range output.Contents {
			if obj := &output.Contents[i]; obj.Key != nil && newerThan(obj, best) {
				best = obj
			}
		}
	}

	return best, best != nil, nil
}

func newerThan(obj, best *types.Object) bool {
	if best == nil {
		return true
	}
	if obj.LastModified == nil {
		return false
	}
	return best.LastModified == nil || obj.LastModified.After(*best.LastModified)
}

// CreateCacheEntry prepares for a new cache upload
func (s *Server) CreateCacheEntry(_ context.Context, key string, version string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", fmt.Errorf("invalid key: %w", err)
	}
	if err := validateVersion(version); err != nil {
		return "", fmt.Errorf("invalid version: %w", err)
	}
	return s.buildCacheKey(version, key), nil
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

// NewServerWithClientsAndScope creates a cache server with custom clients and scope for testing.
// If the scope is invalid, the server is created with no scope (empty string).
func NewServerWithClientsAndScope(s3Client S3API, presignClient PresignAPI, bucketName, scope string) *Server {
	validScope := scope
	if err := validateScope(scope); err != nil {
		validScope = ""
	}
	return &Server{
		s3Client:        s3Client,
		presignClient:   presignClient,
		cacheBucketName: bucketName,
		defaultScope:    validScope,
	}
}
