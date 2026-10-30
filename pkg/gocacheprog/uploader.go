package gocacheprog

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

const uploadTimeout = 60 * time.Second

type upload struct {
	actionID []byte
	outputID []byte
	size     int64
	path     string
}

type uploader struct {
	remote        Remote
	stats         *Stats
	warn          *warner
	open          func(string) (io.ReadCloser, error)
	onUnavailable func()
	queue         chan upload
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	mu            sync.Mutex
	closed        bool
}

func newUploader(remote Remote, stats *Stats, warn *warner, workers, depth int) *uploader {
	ctx, cancel := context.WithCancel(context.Background())
	u := &uploader{
		remote:        remote,
		stats:         stats,
		warn:          warn,
		open:          func(p string) (io.ReadCloser, error) { return os.Open(p) },
		onUnavailable: func() {},
		queue:         make(chan upload, depth),
		ctx:           ctx,
		cancel:        cancel,
	}
	for range workers {
		u.wg.Add(1)
		go u.work()
	}
	return u
}

func (u *uploader) enqueue(up upload) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return false
	}
	select {
	case u.queue <- up:
		return true
	default:
		return false
	}
}

func (u *uploader) drain(ctx context.Context) int64 {
	u.mu.Lock()
	if !u.closed {
		u.closed = true
		close(u.queue)
	}
	u.mu.Unlock()
	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		u.cancel()
		<-done
	}
	u.cancel()
	return u.stats.UploadsAbandoned.Load()
}

func (u *uploader) work() {
	defer u.wg.Done()
	for up := range u.queue {
		u.run(up)
	}
}

func (u *uploader) run(up upload) {
	if u.ctx.Err() != nil {
		u.stats.UploadsAbandoned.Add(1)
		return
	}
	body, err := u.open(up.path)
	if err != nil {
		u.stats.UploadsFailed.Add(1)
		u.warn.warn("upload-open", err)
		return
	}
	ctx, cancel := context.WithTimeout(u.ctx, uploadTimeout)
	err = u.remote.Put(ctx, up.actionID, up.outputID, up.size, body)
	cancel()
	_ = body.Close()
	switch {
	case err == nil:
		u.stats.Uploads.Add(1)
		u.stats.UploadBytes.Add(up.size)
	case u.ctx.Err() != nil:
		u.stats.UploadsAbandoned.Add(1)
	default:
		u.stats.UploadsFailed.Add(1)
		u.warn.warn("upload", err)
		if errors.Is(err, errRemoteUnavailable) {
			u.onUnavailable()
		}
	}
}
