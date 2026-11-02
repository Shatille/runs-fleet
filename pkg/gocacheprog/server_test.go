package gocacheprog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClient struct {
	t     *testing.T
	w     io.WriteCloser
	dec   *json.Decoder
	mu    sync.Mutex
	inbox map[int64]Response
	order []int64
	ready chan struct{}
}

func startClient(t *testing.T, srv *Server) (*fakeClient, <-chan error) {
	t.Helper()
	toServer, fromClient, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	toClient, fromServer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, f := range []*os.File{toServer, fromClient, toClient, fromServer} {
			_ = f.Close()
		}
	})
	done := make(chan error, 1)
	go func() {
		runErr := srv.Run(toServer, fromServer)
		_ = fromServer.Close()
		done <- runErr
	}()
	c := &fakeClient{t: t, w: fromClient, dec: json.NewDecoder(toClient), inbox: map[int64]Response{}, ready: make(chan struct{}, 64)}
	go c.readLoop()
	return c, done
}

func (c *fakeClient) readLoop() {
	for {
		var resp Response
		if err := c.dec.Decode(&resp); err != nil {
			return
		}
		c.mu.Lock()
		if _, dup := c.inbox[resp.ID]; dup {
			c.t.Errorf("duplicate response for ID %d", resp.ID)
		}
		c.inbox[resp.ID] = resp
		c.order = append(c.order, resp.ID)
		c.mu.Unlock()
		c.ready <- struct{}{}
	}
}

func (c *fakeClient) send(frames ...any) {
	c.t.Helper()
	mustWriteFrames(c.t, c.w, frames...)
}

func (c *fakeClient) wait(id int64) Response {
	c.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		resp, ok := c.inbox[id]
		c.mu.Unlock()
		if ok {
			return resp
		}
		select {
		case <-c.ready:
		case <-deadline:
			c.t.Fatalf("no response for ID %d", id)
		}
	}
}

func newTestServer(t *testing.T, store Store) (*Server, *Stats) {
	t.Helper()
	if store == nil {
		store = newTestLocal(t)
	}
	stats := &Stats{}
	srv := NewServer(store, stats, 4)
	srv.warn.logger.SetOutput(io.Discard)
	return srv, stats
}

func TestServer_CapabilitiesComeFirst(t *testing.T) {
	t.Parallel()

	srv, _ := newTestServer(t, nil)
	c, done := startClient(t, srv)
	capab := c.wait(0)
	if len(capab.KnownCommands) != 3 || capab.Err != "" {
		t.Fatalf("capability response = %+v", capab)
	}
	c.send(Request{ID: 1, Command: CmdClose})
	if resp := c.wait(1); resp.Err != "" {
		t.Fatalf("close response = %+v", resp)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestServer_PutThenGet(t *testing.T) {
	t.Parallel()

	srv, stats := newTestServer(t, nil)
	c, done := startClient(t, srv)
	c.wait(0)

	body := []byte("object bytes")
	outputID := sha256.Sum256(body)
	c.send(Request{ID: 1, Command: CmdPut, ActionID: testID(1), OutputID: outputID[:], BodySize: int64(len(body))}, body)
	put := c.wait(1)
	if put.Err != "" || put.DiskPath == "" {
		t.Fatalf("put response = %+v", put)
	}
	if onDisk, err := os.ReadFile(put.DiskPath); err != nil || !bytes.Equal(onDisk, body) {
		t.Fatalf("DiskPath content = %q, err %v", onDisk, err)
	}

	c.send(Request{ID: 2, Command: CmdGet, ActionID: testID(1)})
	hit := c.wait(2)
	if hit.Miss || hit.Err != "" || hit.DiskPath != put.DiskPath || hit.Size != int64(len(body)) || hit.Time == nil {
		t.Fatalf("hit response = %+v", hit)
	}
	if len(hit.OutputID) != HashSize || !bytes.Equal(hit.OutputID, outputID[:]) {
		t.Fatalf("hit OutputID = %x", hit.OutputID)
	}

	c.send(Request{ID: 3, Command: CmdGet, ActionID: testID(7)})
	miss := c.wait(3)
	if !miss.Miss || miss.Err != "" || miss.DiskPath != "" {
		t.Fatalf("miss response = %+v", miss)
	}

	c.send(Request{ID: 4, Command: CmdClose})
	c.wait(4)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sum := stats.Summary()
	if sum.Puts != 1 || sum.Gets != 2 || sum.HitsLocal != 1 || sum.Misses != 1 || sum.PutBytes != int64(len(body)) || sum.GetBytes != int64(len(body)) {
		t.Fatalf("stats = %+v", sum)
	}
}

type blockingStore struct {
	*Local
	release chan struct{}
}

func (b *blockingStore) Get(actionID []byte) (Entry, bool, error) {
	if actionID[0] == 1 {
		<-b.release
	}
	return b.Local.Get(actionID)
}

func TestServer_GetsAreAnsweredOutOfOrder(t *testing.T) {
	t.Parallel()

	store := &blockingStore{Local: newTestLocal(t), release: make(chan struct{})}
	srv, _ := newTestServer(t, store)
	c, done := startClient(t, srv)
	c.wait(0)

	c.send(Request{ID: 1, Command: CmdGet, ActionID: testID(1)})
	c.send(Request{ID: 2, Command: CmdGet, ActionID: testID(2)})
	if resp := c.wait(2); !resp.Miss {
		t.Fatalf("second get = %+v", resp)
	}
	c.mu.Lock()
	_, firstAnswered := c.inbox[1]
	c.mu.Unlock()
	if firstAnswered {
		t.Fatal("blocked get answered before release")
	}
	close(store.release)
	if resp := c.wait(1); !resp.Miss {
		t.Fatalf("first get = %+v", resp)
	}
	c.send(Request{ID: 3, Command: CmdClose})
	c.wait(3)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServer_CloseWaitsForInFlightGets(t *testing.T) {
	t.Parallel()

	store := &blockingStore{Local: newTestLocal(t), release: make(chan struct{})}
	srv, _ := newTestServer(t, store)
	c, done := startClient(t, srv)
	c.wait(0)

	c.send(Request{ID: 1, Command: CmdGet, ActionID: testID(1)})
	c.send(Request{ID: 2, Command: CmdClose})
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) while a get was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	c.mu.Lock()
	_, closed := c.inbox[2]
	c.mu.Unlock()
	if closed {
		t.Fatal("close answered before the in-flight get")
	}
	close(store.release)
	c.wait(2)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	order := append([]int64(nil), c.order...)
	c.mu.Unlock()
	if len(order) != 3 || order[1] != 1 || order[2] != 2 {
		t.Fatalf("response order = %v, want [0 1 2]", order)
	}
}

func TestServer_EOFWithoutCloseIsClean(t *testing.T) {
	t.Parallel()

	srv, _ := newTestServer(t, nil)
	c, done := startClient(t, srv)
	c.wait(0)
	c.send(Request{ID: 1, Command: CmdGet, ActionID: testID(1)})
	c.wait(1)
	if err := c.w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v on EOF", err)
	}
}

func TestServer_FramingErrorIsFatal(t *testing.T) {
	t.Parallel()

	srv, _ := newTestServer(t, nil)
	c, done := startClient(t, srv)
	c.wait(0)
	if _, err := io.WriteString(c.w, "this is not a frame\n"); err != nil {
		t.Fatal(err)
	}
	if err := c.w.Close(); err != nil {
		t.Fatal(err)
	}
	err := <-done
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("Run returned %v, want a decode error", err)
	}
}

type panickingStore struct{ *Local }

func (panickingStore) Get([]byte) (Entry, bool, error) { panic("boom") }
func (panickingStore) Put([]byte, []byte, int64, io.Reader) (string, error) {
	panic("boom")
}

func TestServer_HandlerPanicYieldsExactlyOneResponse(t *testing.T) {
	t.Parallel()

	srv, stats := newTestServer(t, panickingStore{})
	c, done := startClient(t, srv)
	c.wait(0)

	c.send(Request{ID: 1, Command: CmdGet, ActionID: testID(1)})
	if resp := c.wait(1); !resp.Miss || resp.Err != "" {
		t.Fatalf("get after panic = %+v, want a plain miss", resp)
	}
	c.send(Request{ID: 2, Command: CmdPut, ActionID: testID(1), OutputID: testID(2), BodySize: 1}, []byte("x"))
	if resp := c.wait(2); resp.Err == "" || resp.DiskPath != "" {
		t.Fatalf("put after panic = %+v, want Err", resp)
	}
	c.send(Request{ID: 3, Command: CmdGet, ActionID: testID(3)})
	c.wait(3)
	c.send(Request{ID: 4, Command: CmdClose})
	c.wait(4)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stats.Errors.Load() != 3 {
		t.Fatalf("errors = %d, want 3", stats.Errors.Load())
	}
}

func TestServer_PutIntegrityFailureReportsErrAndKeepsServing(t *testing.T) {
	t.Parallel()

	srv, _ := newTestServer(t, nil)
	c, done := startClient(t, srv)
	c.wait(0)

	c.send(Request{ID: 1, Command: CmdPut, ActionID: testID(1), OutputID: testID(2), BodySize: 10}, []byte("short"))
	if resp := c.wait(1); resp.Err == "" {
		t.Fatalf("short put = %+v, want Err", resp)
	}
	c.send(Request{ID: 2, Command: CmdGet, ActionID: testID(1)})
	if resp := c.wait(2); !resp.Miss {
		t.Fatalf("get after failed put = %+v", resp)
	}
	c.send(Request{ID: 3, Command: CmdClose})
	c.wait(3)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type closeRecordingStore struct {
	*Local
	remoteHit bool
	closed    atomic.Bool
}

func (s *closeRecordingStore) Get(actionID []byte) (Entry, bool, error) {
	e, ok, err := s.Local.Get(actionID)
	if ok && s.remoteHit {
		e.Remote = true
	}
	return e, ok, err
}

func (s *closeRecordingStore) Close(context.Context) { s.closed.Store(true) }

func TestServer_RemoteHitsAreCountedSeparately(t *testing.T) {
	t.Parallel()

	store := &closeRecordingStore{Local: newTestLocal(t), remoteHit: true}
	srv, stats := newTestServer(t, store)
	c, done := startClient(t, srv)
	c.wait(0)

	c.send(Request{ID: 1, Command: CmdPut, ActionID: testID(1), OutputID: testID(2), BodySize: 3}, []byte("abc"))
	c.wait(1)
	c.send(Request{ID: 2, Command: CmdGet, ActionID: testID(1)})
	if resp := c.wait(2); resp.Miss || resp.DiskPath == "" {
		t.Fatalf("get = %+v", resp)
	}
	c.send(Request{ID: 3, Command: CmdClose})
	c.wait(3)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sum := stats.Summary()
	if sum.HitsRemote != 1 || sum.HitsLocal != 0 || sum.GetBytes != 3 {
		t.Fatalf("stats = %+v", sum)
	}
}

func TestServer_CloseDrainsStoreBeforeReplying(t *testing.T) {
	t.Parallel()

	store := &closeRecordingStore{Local: newTestLocal(t)}
	srv, _ := newTestServer(t, store)
	c, done := startClient(t, srv)
	c.wait(0)
	c.send(Request{ID: 1, Command: CmdClose})
	c.wait(1)
	if !store.closed.Load() {
		t.Fatal("close reply sent before the store was closed")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServer_EOFDrainsStore(t *testing.T) {
	t.Parallel()

	store := &closeRecordingStore{Local: newTestLocal(t)}
	srv, _ := newTestServer(t, store)
	c, done := startClient(t, srv)
	c.wait(0)
	if err := c.w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !store.closed.Load() {
		t.Fatal("store not closed on EOF")
	}
}

func TestServer_WithCacheOverFailingRemoteNeverAnswersErr(t *testing.T) {
	t.Parallel()

	remote := newMemRemote()
	remote.getErr = errors.New("access denied")
	stats := &Stats{}
	srv := NewServer(newCache(newTestLocal(t), remote, stats, io.Discard), stats, 4)
	srv.warn.logger.SetOutput(io.Discard)
	c, done := startClient(t, srv)
	c.wait(0)

	for i := int64(1); i <= 3; i++ {
		c.send(Request{ID: i, Command: CmdGet, ActionID: testID(byte(i))})
		if resp := c.wait(i); !resp.Miss || resp.Err != "" {
			t.Fatalf("get %d = %+v, want a plain miss", i, resp)
		}
	}
	c.send(Request{ID: 4, Command: CmdPut, ActionID: testID(1), OutputID: testID(2), BodySize: 3}, []byte("abc"))
	if resp := c.wait(4); resp.Err != "" || resp.DiskPath == "" {
		t.Fatalf("put = %+v", resp)
	}
	c.send(Request{ID: 5, Command: CmdClose})
	if resp := c.wait(5); resp.Err != "" {
		t.Fatalf("close = %+v", resp)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sum := stats.Summary()
	if sum.Errors != 0 || sum.RemoteErrors != 3 || sum.Misses != 3 || sum.Puts != 1 {
		t.Fatalf("stats = %+v", sum)
	}
}

type writerFailingAfterHandshake struct {
	writes int
}

func (w *writerFailingAfterHandshake) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("stdout gone")
	}
	return len(p), nil
}

func TestServer_WriteErrorStillDrainsStore(t *testing.T) {
	t.Parallel()

	store := &closeRecordingStore{Local: newTestLocal(t)}
	srv, _ := newTestServer(t, store)
	var in bytes.Buffer
	mustWriteFrames(t, &in, Request{ID: 1, Command: CmdPut, ActionID: testID(1), OutputID: testID(2), BodySize: 1}, []byte("x"))
	err := srv.Run(&in, &writerFailingAfterHandshake{})
	if err == nil {
		t.Fatal("Run returned nil although stdout failed")
	}
	if !store.closed.Load() {
		t.Fatal("store not closed after a write error")
	}
}
