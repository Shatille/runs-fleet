package gocacheprog

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ErrNotFound reports that the remote tier holds nothing for an action ID.
var ErrNotFound = errors.New("gocacheprog: object not found")

var errRemoteUnavailable = errors.New("gocacheprog: remote tier unavailable")

// RemoteObject is a cached output fetched from the remote tier. The caller
// must close Body.
type RemoteObject struct {
	OutputID []byte
	Size     int64
	Time     time.Time
	Body     io.ReadCloser
}

// Remote is the shared cache tier behind the local disk.
type Remote interface {
	Get(ctx context.Context, actionID []byte) (RemoteObject, error)
	Put(ctx context.Context, actionID, outputID []byte, size int64, body io.Reader) error
}

// S3API is the subset of the S3 client the remote tier uses.
type S3API interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

const (
	metaOutputID = "output-id"
	metaPutTime  = "put-time"
)

type s3Remote struct {
	client S3API
	bucket string
	prefix string
}

// NewS3Remote stores one object per action ID under prefix in bucket.
func NewS3Remote(client S3API, bucket, prefix string) Remote {
	return &s3Remote{client: client, bucket: bucket, prefix: strings.Trim(prefix, "/")}
}

func (r *s3Remote) key(actionID []byte) string {
	return path.Join(r.prefix, hex.EncodeToString(actionID))
}

func (r *s3Remote) Get(ctx context.Context, actionID []byte) (RemoteObject, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(r.key(actionID))})
	if err != nil {
		var noKey *types.NoSuchKey
		var notFound *types.NotFound
		if errors.As(err, &noKey) || errors.As(err, &notFound) {
			return RemoteObject{}, ErrNotFound
		}
		return RemoteObject{}, fmt.Errorf("get %s: %w", r.key(actionID), err)
	}
	obj, perr := parseRemoteObject(out)
	if perr != nil {
		_ = out.Body.Close()
		return RemoteObject{}, fmt.Errorf("get %s: %w", r.key(actionID), perr)
	}
	return obj, nil
}

func parseRemoteObject(out *s3.GetObjectOutput) (RemoteObject, error) {
	outputID, err := hex.DecodeString(out.Metadata[metaOutputID])
	if err != nil || len(outputID) != HashSize {
		return RemoteObject{}, fmt.Errorf("bad %s metadata %q", metaOutputID, out.Metadata[metaOutputID])
	}
	if out.ContentLength == nil {
		return RemoteObject{}, errors.New("missing content length")
	}
	putTime, terr := time.Parse(time.RFC3339Nano, out.Metadata[metaPutTime])
	if terr != nil {
		putTime = time.Now().UTC()
	}
	return RemoteObject{OutputID: outputID, Size: *out.ContentLength, Time: putTime, Body: out.Body}, nil
}

func (r *s3Remote) Put(ctx context.Context, actionID, outputID []byte, size int64, body io.Reader) error {
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(r.bucket),
		Key:           aws.String(r.key(actionID)),
		Body:          body,
		ContentLength: aws.Int64(size),
		Metadata: map[string]string{
			metaOutputID: hex.EncodeToString(outputID),
			metaPutTime:  time.Now().UTC().Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		return fmt.Errorf("put %s: %w", r.key(actionID), err)
	}
	return nil
}

type lazyRemote struct {
	once  sync.Once
	build func() (Remote, error)
	r     Remote
	err   error
}

// NewLazyRemote defers build to the first Get or Put so the helper can answer
// cmd/go's handshake before any AWS client exists. A build failure is final.
func NewLazyRemote(build func() (Remote, error)) Remote {
	return &lazyRemote{build: build}
}

func (l *lazyRemote) resolve() (Remote, error) {
	l.once.Do(func() {
		r, err := l.build()
		if err != nil {
			l.err = fmt.Errorf("%w: %w", errRemoteUnavailable, err)
			return
		}
		l.r = r
	})
	return l.r, l.err
}

func (l *lazyRemote) Get(ctx context.Context, actionID []byte) (RemoteObject, error) {
	r, err := l.resolve()
	if err != nil {
		return RemoteObject{}, err
	}
	return r.Get(ctx, actionID)
}

func (l *lazyRemote) Put(ctx context.Context, actionID, outputID []byte, size int64, body io.Reader) error {
	r, err := l.resolve()
	if err != nil {
		return err
	}
	return r.Put(ctx, actionID, outputID, size, body)
}
