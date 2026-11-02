package gocacheprog

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type fakeObject struct {
	body []byte
	meta map[string]string
}

type fakeS3 struct {
	mu         sync.Mutex
	objects    map[string]fakeObject
	getErr     error
	putErr     error
	gets       atomic.Int64
	puts       atomic.Int64
	bodyCloses atomic.Int64
}

type countingBody struct {
	io.Reader
	closes *atomic.Int64
}

func (b countingBody) Close() error {
	b.closes.Add(1)
	return nil
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string]fakeObject{}}
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.gets.Add(1)
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	obj, ok := f.objects[aws.ToString(in.Key)]
	f.mu.Unlock()
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{
		Body:          countingBody{Reader: bytes.NewReader(obj.body), closes: &f.bodyCloses},
		ContentLength: aws.Int64(int64(len(obj.body))),
		Metadata:      obj.meta,
	}, nil
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.puts.Add(1)
	if f.putErr != nil {
		return nil, f.putErr
	}
	if _, seekable := in.Body.(io.Seeker); !seekable {
		return nil, errors.New("body must be seekable so the SDK can compute checksums")
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	if in.ContentLength == nil || *in.ContentLength != int64(len(body)) {
		return nil, errors.New("content length does not match body")
	}
	f.mu.Lock()
	f.objects[aws.ToString(in.Key)] = fakeObject{body: body, meta: in.Metadata}
	f.mu.Unlock()
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) object(key string) (fakeObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[key]
	return obj, ok
}

func TestS3Remote_PutThenGetRoundTrip(t *testing.T) {
	t.Parallel()

	client := newFakeS3()
	r := NewS3Remote(client, "bucket", "gocache/org/repo")
	body := []byte("compiled output")
	if err := r.Put(context.Background(), testID(1), testID(2), int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	key := "gocache/org/repo/" + hex.EncodeToString(testID(1))
	stored, ok := client.object(key)
	if !ok {
		t.Fatalf("object not stored under %s; have %v", key, client.objects)
	}
	if stored.meta[metaOutputID] != hex.EncodeToString(testID(2)) {
		t.Fatalf("metadata = %v", stored.meta)
	}
	if _, err := time.Parse(time.RFC3339Nano, stored.meta[metaPutTime]); err != nil {
		t.Fatalf("put-time %q: %v", stored.meta[metaPutTime], err)
	}

	obj, err := r.Get(context.Background(), testID(1))
	if err != nil {
		t.Fatal(err)
	}
	got, rerr := io.ReadAll(obj.Body)
	if cerr := obj.Body.Close(); rerr == nil {
		rerr = cerr
	}
	if rerr != nil || !bytes.Equal(got, body) {
		t.Fatalf("body = %q, err %v", got, rerr)
	}
	if !bytes.Equal(obj.OutputID, testID(2)) || obj.Size != int64(len(body)) || obj.Time.IsZero() {
		t.Fatalf("object = %+v", obj)
	}
}

func TestS3Remote_EmptyPrefixKeysOnActionID(t *testing.T) {
	t.Parallel()

	client := newFakeS3()
	r := NewS3Remote(client, "bucket", "")
	if err := r.Put(context.Background(), testID(1), testID(2), 1, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.object(hex.EncodeToString(testID(1))); !ok {
		t.Fatalf("keys = %v", client.objects)
	}
}

func TestS3Remote_GetMissIsErrNotFound(t *testing.T) {
	t.Parallel()

	for name, cause := range map[string]error{
		"NoSuchKey": &types.NoSuchKey{},
		"NotFound":  &types.NotFound{},
	} {
		client := newFakeS3()
		client.getErr = cause
		_, err := NewS3Remote(client, "b", "p").Get(context.Background(), testID(1))
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestS3Remote_GetErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("access denied")
	cases := map[string]func(*fakeS3){
		"transport error": func(f *fakeS3) { f.getErr = boom },
		"missing output id": func(f *fakeS3) {
			f.objects["p/"+hex.EncodeToString(testID(1))] = fakeObject{body: []byte("x"), meta: map[string]string{}}
		},
		"short output id": func(f *fakeS3) {
			f.objects["p/"+hex.EncodeToString(testID(1))] = fakeObject{body: []byte("x"), meta: map[string]string{metaOutputID: "abcd"}}
		},
		"unparseable output id": func(f *fakeS3) {
			f.objects["p/"+hex.EncodeToString(testID(1))] = fakeObject{body: []byte("x"), meta: map[string]string{metaOutputID: "zz"}}
		},
	}
	for name, arrange := range cases {
		client := newFakeS3()
		arrange(client)
		_, err := NewS3Remote(client, "b", "p").Get(context.Background(), testID(1))
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: err = %v, want a non-NotFound error", name, err)
		}
		if name == "transport error" && !errors.Is(err, boom) {
			t.Fatalf("cause not wrapped: %v", err)
		}
		if name != "transport error" && client.bodyCloses.Load() != 1 {
			t.Fatalf("%s: body closed %d times, want 1", name, client.bodyCloses.Load())
		}
	}
}

func TestS3Remote_PrefixSlashesAreTrimmed(t *testing.T) {
	t.Parallel()

	client := newFakeS3()
	r := NewS3Remote(client, "bucket", "/gocache/org/repo/")
	if err := r.Put(context.Background(), testID(1), testID(2), 1, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.object("gocache/org/repo/" + hex.EncodeToString(testID(1))); !ok {
		t.Fatalf("keys = %v", client.objects)
	}
}

type nilLengthS3 struct{ *fakeS3 }

func (n nilLengthS3) GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	out, err := n.fakeS3.GetObject(ctx, in, opts...)
	if err == nil {
		out.ContentLength = nil
	}
	return out, err
}

func TestS3Remote_GetRejectsUnknownContentLength(t *testing.T) {
	t.Parallel()

	client := newFakeS3()
	client.objects["p/"+hex.EncodeToString(testID(1))] = fakeObject{body: []byte("x"), meta: map[string]string{metaOutputID: hex.EncodeToString(testID(2))}}
	_, err := NewS3Remote(nilLengthS3{client}, "b", "p").Get(context.Background(), testID(1))
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestLazyRemote_BuildsOnceOnFirstUse(t *testing.T) {
	t.Parallel()

	client := newFakeS3()
	var builds atomic.Int64
	r := NewLazyRemote(func() (Remote, error) {
		builds.Add(1)
		return NewS3Remote(client, "b", "p"), nil
	})
	if builds.Load() != 0 {
		t.Fatal("factory ran at construction")
	}
	if err := r.Put(context.Background(), testID(1), testID(2), 1, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(context.Background(), testID(1)); err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 1 {
		t.Fatalf("factory ran %d times", builds.Load())
	}
}

func TestLazyRemote_BuildFailureIsPermanent(t *testing.T) {
	t.Parallel()

	var builds atomic.Int64
	cause := errors.New("no credentials")
	r := NewLazyRemote(func() (Remote, error) {
		builds.Add(1)
		return nil, cause
	})
	for range 3 {
		_, err := r.Get(context.Background(), testID(1))
		if !errors.Is(err, errRemoteUnavailable) || !errors.Is(err, cause) {
			t.Fatalf("err = %v", err)
		}
	}
	if err := r.Put(context.Background(), testID(1), testID(2), 0, bytes.NewReader(nil)); !errors.Is(err, errRemoteUnavailable) {
		t.Fatalf("put err = %v", err)
	}
	if builds.Load() != 1 {
		t.Fatalf("factory ran %d times", builds.Load())
	}
}
