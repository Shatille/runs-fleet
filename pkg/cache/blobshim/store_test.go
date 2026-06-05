package blobshim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type mockS3 struct {
	putBody   []byte
	putCalls  int
	parts     map[int32][]byte
	createN   int
	completeN int
	abortN    int
	getRange  string
	getOut    *s3.GetObjectOutput
	getErr    error
	headOut   *s3.HeadObjectOutput
	headErr   error
}

func newMockS3() *mockS3 { return &mockS3{parts: map[int32][]byte{}} }

func (m *mockS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	b, _ := io.ReadAll(in.Body)
	m.putBody = b
	m.putCalls++
	return &s3.PutObjectOutput{}, nil
}

func (m *mockS3) CreateMultipartUpload(_ context.Context, _ *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	m.createN++
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("u1")}, nil
}

func (m *mockS3) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	b, _ := io.ReadAll(in.Body)
	m.parts[*in.PartNumber] = b
	return &s3.UploadPartOutput{ETag: aws.String(fmt.Sprintf("etag-%d", *in.PartNumber))}, nil
}

func (m *mockS3) CompleteMultipartUpload(_ context.Context, _ *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	m.completeN++
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (m *mockS3) AbortMultipartUpload(_ context.Context, _ *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	m.abortN++
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (m *mockS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if in.Range != nil {
		m.getRange = *in.Range
	}
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.getOut, nil
}

func (m *mockS3) HeadObject(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if m.headErr != nil {
		return nil, m.headErr
	}
	return m.headOut, nil
}

func (m *mockS3) assembledParts() []byte {
	nums := make([]int32, 0, len(m.parts))
	for n := range m.parts {
		nums = append(nums, n)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	var out []byte
	for _, n := range nums {
		out = append(out, m.parts[n]...)
	}
	return out
}

func TestS3StorePutSmallUsesSinglePut(t *testing.T) {
	t.Parallel()

	m := newMockS3()
	store := NewS3Store(m, "bucket")
	if err := store.Put(context.Background(), "k", strings.NewReader("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if m.putCalls != 1 {
		t.Errorf("PutObject calls = %d, want 1", m.putCalls)
	}
	if m.createN != 0 {
		t.Errorf("multipart used for small object (createN=%d)", m.createN)
	}
	if string(m.putBody) != "hello" {
		t.Errorf("put body = %q, want hello", m.putBody)
	}
}

func TestS3StorePutLargeUsesMultipart(t *testing.T) {
	t.Parallel()

	m := newMockS3()
	store := NewS3Store(m, "bucket")
	data := bytes.Repeat([]byte("x"), partSize+100)
	if err := store.Put(context.Background(), "k", bytes.NewReader(data)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if m.putCalls != 0 {
		t.Errorf("single PutObject used for large object (putCalls=%d)", m.putCalls)
	}
	if m.createN != 1 || m.completeN != 1 {
		t.Errorf("create=%d complete=%d, want 1/1", m.createN, m.completeN)
	}
	if len(m.parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(m.parts))
	}
	if len(m.parts[1]) != partSize || len(m.parts[2]) != 100 {
		t.Errorf("part sizes = %d/%d, want %d/100", len(m.parts[1]), len(m.parts[2]), partSize)
	}
	if !bytes.Equal(m.assembledParts(), data) {
		t.Error("reassembled multipart bytes differ from input")
	}
	if m.abortN != 0 {
		t.Errorf("abort called %d times on success", m.abortN)
	}
}

func TestS3StoreGetPassesRangeAndMapsNotFound(t *testing.T) {
	t.Parallel()

	m := newMockS3()
	m.getOut = &s3.GetObjectOutput{
		Body:          io.NopCloser(strings.NewReader("par")),
		ContentLength: aws.Int64(3),
		ContentRange:  aws.String("bytes 0-2/10"),
		ETag:          aws.String("\"abc\""),
	}
	store := NewS3Store(m, "bucket")
	res, err := store.Get(context.Background(), "k", "bytes=0-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.getRange != "bytes=0-2" {
		t.Errorf("range passed = %q, want bytes=0-2", m.getRange)
	}
	if !res.Partial || res.ContentRange != "bytes 0-2/10" {
		t.Errorf("partial=%v range=%q", res.Partial, res.ContentRange)
	}

	m.getErr = &types.NoSuchKey{}
	if _, err := store.Get(context.Background(), "k", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get error = %v, want ErrNotFound", err)
	}
}

func TestS3StoreGetMapsNotFoundVariants(t *testing.T) {
	t.Parallel()

	variants := map[string]error{
		"NoSuchKey":    &types.NoSuchKey{},
		"NotFound":     &types.NotFound{},
		"404 fallback": errors.New("operation error S3: GetObject, https response error StatusCode: 404"),
	}
	for name, e := range variants {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMockS3()
			m.getErr = e
			store := NewS3Store(m, "bucket")
			if _, err := store.Get(context.Background(), "k", ""); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get error = %v, want ErrNotFound", err)
			}
		})
	}
}
