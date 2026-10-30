package gocacheprog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type memRemote struct {
	mu      sync.Mutex
	objects map[string][]byte
	block   chan struct{}
	putErr  error
	puts    int
	getErr  error
	gets    int
}

func newMemRemote() *memRemote {
	return &memRemote{objects: map[string][]byte{}}
}

func (m *memRemote) Get(ctx context.Context, actionID []byte) (RemoteObject, error) {
	m.mu.Lock()
	m.gets++
	body, ok := m.objects[string(actionID)]
	getErr := m.getErr
	m.mu.Unlock()
	if getErr != nil {
		return RemoteObject{}, getErr
	}
	if !ok {
		return RemoteObject{}, ErrNotFound
	}
	if err := m.wait(ctx); err != nil {
		return RemoteObject{}, err
	}
	sum := sha256.Sum256(body)
	return RemoteObject{OutputID: sum[:], Size: int64(len(body)), Time: time.Unix(1700000000, 0), Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (m *memRemote) Put(ctx context.Context, actionID, _ []byte, _ int64, body io.Reader) error {
	if err := m.wait(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if m.putErr != nil {
		return m.putErr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.objects[string(actionID)] = data
	return nil
}

func (m *memRemote) wait(ctx context.Context) error {
	if m.block == nil {
		return nil
	}
	select {
	case <-m.block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *memRemote) object(actionID []byte) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[string(actionID)]
	return b, ok
}

func (m *memRemote) putCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts
}

func memOpen(files map[string][]byte) func(string) (io.ReadCloser, error) {
	return func(path string) (io.ReadCloser, error) {
		b, ok := files[path]
		if !ok {
			return nil, errors.New("no such file: " + path)
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

func newTestUploader(remote Remote, workers, depth int, files map[string][]byte) (*uploader, *Stats) {
	stats := &Stats{}
	u := newUploader(remote, stats, newWarner(io.Discard), workers, depth)
	u.open = memOpen(files)
	return u, stats
}

func TestUploader_UploadsQueuedBodies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		remote := newMemRemote()
		files := map[string][]byte{"/l1/o/one": []byte("first"), "/l1/o/two": []byte("second")}
		u, stats := newTestUploader(remote, 2, 8, files)

		if !u.enqueue(upload{actionID: testID(1), outputID: testID(2), size: 5, path: "/l1/o/one"}) {
			t.Fatal("first enqueue refused")
		}
		if !u.enqueue(upload{actionID: testID(3), outputID: testID(4), size: 6, path: "/l1/o/two"}) {
			t.Fatal("second enqueue refused")
		}
		if abandoned := u.drain(context.Background()); abandoned != 0 {
			t.Fatalf("abandoned = %d", abandoned)
		}
		for id, want := range map[byte]string{1: "first", 3: "second"} {
			if got, ok := remote.object(testID(id)); !ok || string(got) != want {
				t.Fatalf("remote %d = %q (%v)", id, got, ok)
			}
		}
		sum := stats.Summary()
		if sum.Uploads != 2 || sum.UploadBytes != 11 || sum.UploadsFailed != 0 || sum.UploadsAbandoned != 0 {
			t.Fatalf("stats = %+v", sum)
		}
	})
}

func TestUploader_EnqueueNeverBlocksWhenQueueIsFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		remote := newMemRemote()
		remote.block = make(chan struct{})
		files := map[string][]byte{"/o": []byte("x")}
		u, stats := newTestUploader(remote, 1, 1, files)

		synctest.Wait()
		accepted := 0
		for range 3 {
			if u.enqueue(upload{actionID: testID(1), outputID: testID(2), size: 1, path: "/o"}) {
				accepted++
			}
			synctest.Wait()
		}
		if accepted != 2 {
			t.Fatalf("accepted %d uploads with one worker and depth 1, want 2", accepted)
		}
		close(remote.block)
		if abandoned := u.drain(context.Background()); abandoned != 0 {
			t.Fatalf("abandoned = %d", abandoned)
		}
		if sum := stats.Summary(); sum.Uploads != 2 {
			t.Fatalf("stats = %+v", sum)
		}
	})
}

func TestUploader_DrainDeadlineAbandonsInFlightAndQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		remote := newMemRemote()
		remote.block = make(chan struct{})
		files := map[string][]byte{"/o": []byte("x")}
		u, stats := newTestUploader(remote, 1, 4, files)

		for range 3 {
			if !u.enqueue(upload{actionID: testID(1), outputID: testID(2), size: 1, path: "/o"}) {
				t.Fatal("enqueue refused")
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		start := time.Now()
		abandoned := u.drain(ctx)
		if elapsed := time.Since(start); elapsed != 20*time.Second {
			t.Fatalf("drain returned after %v, want the 20s deadline", elapsed)
		}
		if abandoned != 3 {
			t.Fatalf("abandoned = %d, want 3", abandoned)
		}
		if remote.putCount() != 0 {
			t.Fatalf("remote recorded %d completed puts", remote.putCount())
		}
		sum := stats.Summary()
		if sum.UploadsAbandoned != 3 || sum.Uploads != 0 || sum.UploadsFailed != 0 {
			t.Fatalf("stats = %+v", sum)
		}
		if u.enqueue(upload{actionID: testID(1), outputID: testID(2), size: 1, path: "/o"}) {
			t.Fatal("enqueue accepted after drain")
		}
	})
}

func TestUploader_FailedUploadIsCountedAndWorkersKeepGoing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		remote := newMemRemote()
		files := map[string][]byte{"/ok": []byte("x")}
		u, stats := newTestUploader(remote, 1, 4, files)

		u.enqueue(upload{actionID: testID(1), outputID: testID(2), size: 1, path: "/missing"})
		remote.putErr = errors.New("throttled")
		u.enqueue(upload{actionID: testID(3), outputID: testID(4), size: 1, path: "/ok"})
		synctest.Wait()
		remote.mu.Lock()
		remote.putErr = nil
		remote.mu.Unlock()
		u.enqueue(upload{actionID: testID(5), outputID: testID(6), size: 1, path: "/ok"})
		if abandoned := u.drain(context.Background()); abandoned != 0 {
			t.Fatalf("abandoned = %d", abandoned)
		}
		sum := stats.Summary()
		if sum.UploadsFailed != 2 || sum.Uploads != 1 || sum.UploadBytes != 1 {
			t.Fatalf("stats = %+v", sum)
		}
		if _, ok := remote.object(testID(5)); !ok {
			t.Fatal("upload after failures did not land")
		}
	})
}
