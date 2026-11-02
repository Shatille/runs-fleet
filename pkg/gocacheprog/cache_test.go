package gocacheprog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func newTestCache(t *testing.T, remote Remote) (*Cache, *Stats) {
	t.Helper()
	stats := &Stats{}
	return newCache(newTestLocal(t), remote, stats, io.Discard), stats
}

func TestCache_LocalHitDoesNotConsultRemote(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	c, stats := newTestCache(t, remote)
	body := []byte("local")
	if _, err := c.local.Put(testID(1), testID(2), int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	e, ok, err := c.Get(testID(1))
	if err != nil || !ok || e.Remote {
		t.Fatalf("entry = %+v, ok %v, err %v", e, ok, err)
	}
	if remote.gets != 0 || stats.Summary().HitsRemote != 0 {
		t.Fatalf("remote consulted on a local hit: gets=%d", remote.gets)
	}
}

func TestCache_RemoteHitIsWrittenThroughToLocal(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	remote.objects[string(testID(1))] = []byte("remote body")
	c, stats := newTestCache(t, remote)

	e, ok, err := c.Get(testID(1))
	if err != nil || !ok || !e.Remote {
		t.Fatalf("entry = %+v, ok %v, err %v", e, ok, err)
	}
	want := sha256.Sum256([]byte("remote body"))
	if !bytes.Equal(e.OutputID, want[:]) || e.Size != 11 || e.Time.IsZero() {
		t.Fatalf("entry = %+v", e)
	}
	if onDisk, rerr := os.ReadFile(e.DiskPath); rerr != nil || string(onDisk) != "remote body" {
		t.Fatalf("DiskPath content = %q, err %v", onDisk, rerr)
	}
	if sum := stats.Summary(); sum.DownloadBytes != 11 {
		t.Fatalf("stats = %+v", sum)
	}

	again, ok, err := c.Get(testID(1))
	if err != nil || !ok || again.Remote || again.DiskPath != e.DiskPath {
		t.Fatalf("second get = %+v, ok %v, err %v", again, ok, err)
	}
	if remote.gets != 1 {
		t.Fatalf("remote consulted %d times, want 1", remote.gets)
	}
}

func TestCache_RemoteMissIsAMiss(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	c, stats := newTestCache(t, remote)
	_, ok, err := c.Get(testID(1))
	if err != nil || ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if sum := stats.Summary(); sum.RemoteErrors != 0 {
		t.Fatalf("stats = %+v", sum)
	}
}

func TestCache_RemoteFailureIsAMissNotAnError(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	remote.getErr = errors.New("503")
	c, stats := newTestCache(t, remote)
	for range 2 {
		_, ok, err := c.Get(testID(1))
		if err != nil || ok {
			t.Fatalf("ok %v, err %v", ok, err)
		}
	}
	if sum := stats.Summary(); sum.RemoteErrors != 2 || sum.Errors != 0 {
		t.Fatalf("stats = %+v", sum)
	}
	if remote.gets != 2 {
		t.Fatalf("remote consulted %d times; transient failures must not disable it", remote.gets)
	}
}

type truncatingRemote struct{ *memRemote }

func (r truncatingRemote) Get(ctx context.Context, actionID []byte) (RemoteObject, error) {
	obj, err := r.memRemote.Get(ctx, actionID)
	if err == nil {
		obj.Size++
	}
	return obj, err
}

func TestCache_TruncatedRemoteBodyIsAMissAndLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	remote.objects[string(testID(1))] = []byte("partial")
	c, stats := newTestCache(t, truncatingRemote{remote})
	_, ok, err := c.Get(testID(1))
	if err != nil || ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if sum := stats.Summary(); sum.RemoteErrors != 1 {
		t.Fatalf("stats = %+v", sum)
	}
	if _, lok, lerr := c.local.Get(testID(1)); lok || lerr != nil {
		t.Fatalf("local has entry %v, err %v after a truncated fill", lok, lerr)
	}
}

type wrongDigestRemote struct{ *memRemote }

func (r wrongDigestRemote) Get(ctx context.Context, actionID []byte) (RemoteObject, error) {
	obj, err := r.memRemote.Get(ctx, actionID)
	if err == nil {
		obj.OutputID = testID(0xB0)
	}
	return obj, err
}

func TestCache_RemoteBodyNotMatchingOutputIDIsAMissAndLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	remote.objects[string(testID(1))] = []byte("bit-rotted")
	c, stats := newTestCache(t, wrongDigestRemote{remote})
	_, ok, err := c.Get(testID(1))
	if err != nil || ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if sum := stats.Summary(); sum.RemoteErrors != 1 || sum.DownloadBytes != 0 {
		t.Fatalf("stats = %+v", sum)
	}
	if _, lok, lerr := c.local.Get(testID(1)); lok || lerr != nil {
		t.Fatalf("local has entry %v, err %v after a digest mismatch", lok, lerr)
	}
	if _, serr := os.Stat(c.local.outputPath(testID(0xB0))); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("output file left behind: %v", serr)
	}
}

func TestCache_RepeatedRemoteFailuresTripTheBreaker(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	remote.getErr = errors.New("timeout")
	c, stats := newTestCache(t, remote)
	for i := range remoteFailureLimit + 3 {
		if _, ok, err := c.Get(testID(byte(i))); ok || err != nil {
			t.Fatalf("get %d: ok %v, err %v", i, ok, err)
		}
	}
	if c.hasRemote() {
		t.Fatal("remote still consulted after repeated failures")
	}
	if remote.gets != remoteFailureLimit {
		t.Fatalf("remote consulted %d times, want %d", remote.gets, remoteFailureLimit)
	}
	if sum := stats.Summary(); sum.RemoteErrors != remoteFailureLimit || sum.Errors != 0 {
		t.Fatalf("stats = %+v", sum)
	}
}

func TestCache_SuccessResetsTheFailureCount(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	c, _ := newTestCache(t, remote)
	flaky := errors.New("flaky")
	for i := range remoteFailureLimit * 2 {
		remote.mu.Lock()
		if i%(remoteFailureLimit-1) == 0 {
			remote.getErr = nil
		} else {
			remote.getErr = flaky
		}
		remote.mu.Unlock()
		if _, _, err := c.Get(testID(byte(i))); err != nil {
			t.Fatal(err)
		}
	}
	if !c.hasRemote() {
		t.Fatal("breaker tripped although failures never ran consecutively to the limit")
	}
}

func TestCache_PutBeforeAnyGetMarksAnUnavailableRemoteDown(t *testing.T) {
	t.Parallel()

	var builds atomic.Int64
	lazy := NewLazyRemote(func() (Remote, error) {
		builds.Add(1)
		return nil, errors.New("no credentials")
	})
	c, stats := newTestCache(t, lazy)
	if _, err := c.Put(testID(1), testID(2), 1, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	c.Close(context.Background())
	if c.hasRemote() {
		t.Fatal("remote still considered available after the upload path hit the failed factory")
	}
	if sum := stats.Summary(); sum.UploadsFailed != 1 || sum.Uploads != 0 {
		t.Fatalf("stats = %+v", sum)
	}
	if builds.Load() != 1 {
		t.Fatalf("factory ran %d times", builds.Load())
	}
	if _, ok, err := c.Get(testID(9)); ok || err != nil {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if sum := stats.Summary(); sum.RemoteErrors != 0 {
		t.Fatalf("remote consulted after being marked down: %+v", sum)
	}
}

func TestCache_UploadsReachS3WithASeekableBody(t *testing.T) {
	t.Parallel()

	client := newFakeS3()
	c, stats := newTestCache(t, NewS3Remote(client, "bucket", "gocache/org/repo"))
	body := []byte("compiled")
	outputID := sha256.Sum256(body)
	if _, err := c.Put(testID(1), outputID[:], int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	c.Close(context.Background())
	if sum := stats.Summary(); sum.Uploads != 1 || sum.UploadsFailed != 0 {
		t.Fatalf("stats = %+v", sum)
	}
	fresh, _ := newTestCache(t, NewS3Remote(client, "bucket", "gocache/org/repo"))
	e, ok, err := fresh.Get(testID(1))
	if err != nil || !ok || !e.Remote {
		t.Fatalf("entry = %+v, ok %v, err %v", e, ok, err)
	}
	if onDisk, rerr := os.ReadFile(e.DiskPath); rerr != nil || !bytes.Equal(onDisk, body) {
		t.Fatalf("filled body = %q, err %v", onDisk, rerr)
	}
}

func TestCache_UnavailableRemoteIsConsultedOnce(t *testing.T) {
	t.Parallel()

	var builds atomic.Int64
	lazy := NewLazyRemote(func() (Remote, error) {
		builds.Add(1)
		return nil, errors.New("no region")
	})
	c, stats := newTestCache(t, lazy)
	for range 3 {
		if _, ok, err := c.Get(testID(1)); ok || err != nil {
			t.Fatalf("ok %v, err %v", ok, err)
		}
	}
	if c.hasRemote() {
		t.Fatal("remote still considered available")
	}
	if sum := stats.Summary(); sum.RemoteErrors != 1 {
		t.Fatalf("stats = %+v", sum)
	}
	if builds.Load() != 1 {
		t.Fatalf("factory ran %d times", builds.Load())
	}
	path, err := c.Put(testID(2), testID(3), 1, bytes.NewReader([]byte("x")))
	if err != nil || path == "" {
		t.Fatalf("put with remote down: %q, %v", path, err)
	}
	c.Close(context.Background())
	if sum := stats.Summary(); sum.Uploads != 0 || sum.UploadsFailed != 0 || sum.UploadsDropped != 0 {
		t.Fatalf("stats after put with remote down = %+v", sum)
	}
}

func TestCache_PutStoresLocallyAndUploadsAsync(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	c, stats := newTestCache(t, remote)
	body := []byte("fresh output")
	path, err := c.Put(testID(1), testID(2), int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if onDisk, rerr := os.ReadFile(path); rerr != nil || !bytes.Equal(onDisk, body) {
		t.Fatalf("local body = %q, err %v", onDisk, rerr)
	}
	c.Close(context.Background())
	if got, ok := remote.object(testID(1)); !ok || !bytes.Equal(got, body) {
		t.Fatalf("remote body = %q (%v)", got, ok)
	}
	if sum := stats.Summary(); sum.Uploads != 1 || sum.UploadBytes != int64(len(body)) {
		t.Fatalf("stats = %+v", sum)
	}
}

func TestCache_PutLargerThanCapStaysLocalOnly(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	c, stats := newTestCache(t, remote)
	c.maxRemoteObject = 4
	if _, err := c.Put(testID(1), testID(2), 5, bytes.NewReader([]byte("12345"))); err != nil {
		t.Fatal(err)
	}
	c.Close(context.Background())
	if _, ok := remote.object(testID(1)); ok {
		t.Fatal("oversized output was uploaded")
	}
	if sum := stats.Summary(); sum.UploadsSkipped != 1 || sum.Uploads != 0 || sum.UploadsDropped != 0 {
		t.Fatalf("stats = %+v", sum)
	}
}

func TestCache_PutDropsUploadWhenQueueIsFull(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	remote.block = make(chan struct{})
	c, stats := newTestCache(t, nil)
	c.remote = remote
	c.up = newUploader(remote, stats, newWarner(io.Discard), 1, 1)
	for i := byte(1); i <= 3; i++ {
		if _, err := c.Put(testID(i), testID(i+10), 1, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatal(err)
		}
	}
	close(remote.block)
	c.Close(context.Background())
	sum := stats.Summary()
	if sum.UploadsDropped+sum.Uploads != 3 || sum.UploadsDropped < 1 {
		t.Fatalf("stats = %+v, want every put either uploaded or dropped with at least one drop", sum)
	}
}

func TestCache_ConcurrentGetsOfOneMissingActionBothHit(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	remote.objects[string(testID(1))] = []byte("shared")
	c, _ := newTestCache(t, remote)
	var wg sync.WaitGroup
	results := make([]Entry, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, ok, err := c.Get(testID(1))
			if err == nil && !ok {
				err = errors.New("miss")
			}
			results[i], errs[i] = e, err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if results[i].DiskPath != results[0].DiskPath {
			t.Fatalf("get %d served %s, get 0 served %s", i, results[i].DiskPath, results[0].DiskPath)
		}
	}
}

func TestCache_WithoutRemoteIsLocalOnly(t *testing.T) {
	t.Parallel()

	c, stats := newTestCache(t, nil)
	if c.hasRemote() {
		t.Fatal("nil remote reported available")
	}
	if _, ok, err := c.Get(testID(1)); ok || err != nil {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if _, err := c.Put(testID(1), testID(2), 1, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if e, ok, err := c.Get(testID(1)); err != nil || !ok || e.Remote {
		t.Fatalf("entry = %+v, ok %v, err %v", e, ok, err)
	}
	c.Close(context.Background())
	if sum := stats.Summary(); sum != (Summary{}) {
		t.Fatalf("local-only cache touched remote counters: %+v", sum)
	}
}
