package gocacheprog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sync/atomic"
	"time"
)

const (
	defaultMaxRemoteObject = 64 << 20
	remoteGetTimeout       = 10 * time.Second
	remoteFailureLimit     = 5
	uploadWorkers          = 4
	uploadQueueDepth       = 256
)

// Cache is the two-tier policy behind the protocol server: every output lands
// on local disk, remote misses fill the local tier, and puts are mirrored to
// the remote tier asynchronously. Remote trouble degrades to misses, and a
// remote that keeps failing is abandoned for the rest of the process.
type Cache struct {
	local           *Local
	remote          Remote
	up              *uploader
	stats           *Stats
	warn            *warner
	maxRemoteObject int64
	remoteDown      atomic.Bool
	remoteFailures  atomic.Int64
}

// NewCache wires a local tier to an optional remote one; a nil remote keeps
// the cache local-only.
func NewCache(local *Local, remote Remote, stats *Stats) *Cache {
	return newCache(local, remote, stats, os.Stderr)
}

func newCache(local *Local, remote Remote, stats *Stats, logw io.Writer) *Cache {
	c := &Cache{local: local, remote: remote, stats: stats, warn: newWarner(logw), maxRemoteObject: defaultMaxRemoteObject}
	if remote != nil {
		c.up = newUploader(remote, stats, c.warn, uploadWorkers, uploadQueueDepth)
		c.up.onUnavailable = c.markRemoteDown
	}
	return c
}

func (c *Cache) hasRemote() bool {
	return c.remote != nil && !c.remoteDown.Load()
}

func (c *Cache) markRemoteDown() {
	c.remoteDown.Store(true)
}

func (c *Cache) remoteFailed(class string, err error) {
	c.stats.RemoteErrors.Add(1)
	c.warn.warn(class, err)
	if errors.Is(err, errRemoteUnavailable) || c.remoteFailures.Add(1) >= remoteFailureLimit {
		c.markRemoteDown()
	}
}

// Get serves from local disk, then fills from the remote tier. Remote
// failures of any kind are reported as misses.
func (c *Cache) Get(actionID []byte) (Entry, bool, error) {
	e, ok, err := c.local.Get(actionID)
	if err != nil || ok || !c.hasRemote() {
		return e, ok, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteGetTimeout)
	defer cancel()
	obj, gerr := c.remote.Get(ctx, actionID)
	if errors.Is(gerr, ErrNotFound) {
		c.remoteFailures.Store(0)
		return Entry{}, false, nil
	}
	if gerr != nil {
		c.remoteFailed("remote-get", gerr)
		return Entry{}, false, nil
	}
	path, perr := c.local.Put(actionID, obj.OutputID, obj.Size, &digestReader{r: obj.Body, h: sha256.New(), want: obj.OutputID})
	_ = obj.Body.Close()
	if perr != nil {
		c.remoteFailed("remote-fill", perr)
		return Entry{}, false, nil
	}
	c.remoteFailures.Store(0)
	c.stats.DownloadBytes.Add(obj.Size)
	return Entry{OutputID: obj.OutputID, Size: obj.Size, Time: obj.Time, DiskPath: path, Remote: true}, true, nil
}

type digestReader struct {
	r    io.Reader
	h    hash.Hash
	want []byte
}

func (d *digestReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	d.h.Write(p[:n])
	if errors.Is(err, io.EOF) && !bytes.Equal(d.h.Sum(nil), d.want) {
		return n, fmt.Errorf("body digest %x does not match output id %x", d.h.Sum(nil), d.want)
	}
	return n, err
}

// Put stores body locally and queues it for upload when it fits the remote
// cap and the queue has room.
func (c *Cache) Put(actionID, outputID []byte, size int64, body io.Reader) (string, error) {
	path, err := c.local.Put(actionID, outputID, size, body)
	if err != nil || !c.hasRemote() {
		return path, err
	}
	if size > c.maxRemoteObject {
		c.stats.UploadsSkipped.Add(1)
		return path, nil
	}
	if !c.up.enqueue(upload{actionID: actionID, outputID: outputID, size: size, path: path}) {
		c.stats.UploadsDropped.Add(1)
	}
	return path, nil
}

// Close lets queued uploads finish until ctx expires, then abandons the rest.
func (c *Cache) Close(ctx context.Context) {
	if c.up != nil {
		c.up.drain(ctx)
	}
}
