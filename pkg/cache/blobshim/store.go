package blobshim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ErrNotFound is returned by Get and Head when the object is absent.
var ErrNotFound = errors.New("blob not found")

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Size        int64
	ETag        string
	ContentType string
}

// GetResult is the outcome of a (possibly ranged) read. Body must be closed.
type GetResult struct {
	Body         io.ReadCloser
	Info         ObjectInfo
	ContentRange string // HTTP Content-Range value, set only when Partial
	Partial      bool   // true => the read was satisfied as a 206 range
}

// ObjectStore is the backing blob store. The shim writes the reassembled cache
// archive with Put and serves reads with Get/Head.
type ObjectStore interface {
	Put(ctx context.Context, key string, body io.Reader) error
	Get(ctx context.Context, key, rangeHeader string) (*GetResult, error)
	Head(ctx context.Context, key string) (*ObjectInfo, error)
}

// s3BlobAPI is the subset of the S3 client the store needs.
type s3BlobAPI interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(ctx context.Context, in *s3.UploadPartInput, opts ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, opts ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// partSize is the S3 multipart part size. 8 MiB clears the 5 MiB minimum for
// all-but-last parts and keeps a 10 GiB cache archive well under 10000 parts.
const partSize = 8 << 20

// S3Store implements ObjectStore over an S3 bucket. Put streams the body,
// switching to multipart only when it exceeds one part, so small caches take a
// single PutObject and large caches never buffer the whole archive in memory.
type S3Store struct {
	client s3BlobAPI
	bucket string
}

// NewS3Store returns a store backed by the given S3 client and bucket.
func NewS3Store(client s3BlobAPI, bucket string) *S3Store {
	return &S3Store{client: client, bucket: bucket}
}

// Put streams body to key, using a single PutObject when it fits one part and
// multipart otherwise. A failed multipart upload is aborted.
func (s *S3Store) Put(ctx context.Context, key string, body io.Reader) error {
	first := make([]byte, partSize)
	n, err := io.ReadFull(body, first)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		_, perr := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(first[:n]),
			ContentLength: aws.Int64(int64(n)),
		})
		if perr != nil {
			return fmt.Errorf("put object: %w", perr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	return s.putMultipart(ctx, key, first, body)
}

func (s *S3Store) putMultipart(ctx context.Context, key string, first []byte, rest io.Reader) (err error) {
	created, cerr := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if cerr != nil {
		return fmt.Errorf("create multipart upload: %w", cerr)
	}
	uploadID := aws.ToString(created.UploadId)
	defer func() {
		if err != nil {
			// Detach from the request context: if the upload failed because the
			// client disconnected (ctx cancelled), the abort must still go
			// through or the incomplete multipart upload lingers in S3.
			_, _ = s.client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(s.bucket),
				Key:      aws.String(key),
				UploadId: aws.String(uploadID),
			})
		}
	}()

	var completed []types.CompletedPart
	part := first
	for partNum := int32(1); ; partNum++ {
		done, cp, uerr := s.uploadPart(ctx, key, uploadID, partNum, part, rest)
		if uerr != nil {
			return uerr
		}
		completed = append(completed, cp.part)
		if done {
			break
		}
		part = cp.next
	}

	_, cerr = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if cerr != nil {
		return fmt.Errorf("complete multipart upload: %w", cerr)
	}
	return nil
}

type partResult struct {
	part types.CompletedPart
	next []byte
}

// uploadPart uploads one part, then reads the following part from rest. It
// returns done=true once rest is exhausted (this part was the last).
func (s *S3Store) uploadPart(ctx context.Context, key, uploadID string, num int32, data []byte, rest io.Reader) (done bool, res partResult, err error) {
	out, uerr := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		UploadId:      aws.String(uploadID),
		PartNumber:    aws.Int32(num),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if uerr != nil {
		return false, res, fmt.Errorf("upload part %d: %w", num, uerr)
	}
	res.part = types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(num)}

	next := make([]byte, partSize)
	n, rerr := io.ReadFull(rest, next)
	if n > 0 {
		res.next = next[:n]
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) || rerr == nil {
			return false, res, nil
		}
		return false, res, fmt.Errorf("read body: %w", rerr)
	}
	if errors.Is(rerr, io.EOF) {
		return true, res, nil
	}
	if rerr != nil {
		return false, res, fmt.Errorf("read body: %w", rerr)
	}
	return true, res, nil
}

// Get reads key, honoring an optional HTTP Range header value.
func (s *S3Store) Get(ctx context.Context, key, rangeHeader string) (*GetResult, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if rangeHeader != "" {
		in.Range = aws.String(rangeHeader)
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get object: %w", err)
	}
	res := &GetResult{
		Body: out.Body,
		Info: ObjectInfo{Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag), ContentType: aws.ToString(out.ContentType)},
	}
	if out.ContentRange != nil {
		res.ContentRange = *out.ContentRange
		res.Partial = true
	}
	return res, nil
}

// Head returns metadata for key.
func (s *S3Store) Head(ctx context.Context, key string) (*ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("head object: %w", err)
	}
	return &ObjectInfo{Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag), ContentType: aws.ToString(out.ContentType)}, nil
}

func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return true
	}
	return strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404")
}
