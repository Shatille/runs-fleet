package gocacheprog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const closeDrainTimeout = 20 * time.Second

// Store is the cache policy behind the protocol server. Close runs after the
// last request and before the close reply, bounded by ctx.
type Store interface {
	Get(actionID []byte) (Entry, bool, error)
	Put(actionID, outputID []byte, size int64, body io.Reader) (string, error)
	Close(ctx context.Context)
}

// Server speaks the GOCACHEPROG protocol over a reader/writer pair.
type Server struct {
	store      Store
	stats      *Stats
	getWorkers int
	warn       *warner
}

// NewServer wires a Store to the protocol. getWorkers bounds concurrent gets.
func NewServer(store Store, stats *Stats, getWorkers int) *Server {
	if getWorkers <= 0 {
		getWorkers = defaultGetWorkers
	}
	return &Server{store: store, stats: stats, getWorkers: getWorkers, warn: newWarner(os.Stderr)}
}

// Run advertises capabilities, then serves requests until close or EOF.
// It returns nil on a clean close or EOF and an error only when the stream
// cannot be parsed or responses cannot be written.
func (s *Server) Run(stdin io.Reader, stdout io.Writer) error {
	w := newFrameWriter(stdout)
	if err := w.Send(Response{ID: 0, KnownCommands: knownCommands}); err != nil {
		return err
	}
	r := newFrameReader(stdin)
	sem := make(chan struct{}, s.getWorkers)
	var wg sync.WaitGroup
	var once sync.Once
	finish := func() { once.Do(func() { s.finish(&wg) }) }
	defer finish()
	for {
		req, body, err := r.Next()
		if err != nil {
			finish()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch req.Command {
		case CmdPut:
			if serr := w.Send(s.handlePut(req, body)); serr != nil {
				return serr
			}
		case CmdGet:
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				_ = w.Send(s.handleGet(req))
			}()
		case CmdClose:
			finish()
			return w.Send(Response{ID: req.ID})
		default:
			if serr := w.Send(Response{ID: req.ID, Err: fmt.Sprintf("gocacheprog: unknown command %q", req.Command)}); serr != nil {
				return serr
			}
		}
	}
}

func (s *Server) finish(wg *sync.WaitGroup) {
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), closeDrainTimeout)
	defer cancel()
	s.store.Close(ctx)
}

func (s *Server) handlePut(req Request, body io.Reader) (resp Response) {
	defer func() {
		if r := recover(); r != nil {
			s.stats.Errors.Add(1)
			s.warn.warn("put-panic", fmt.Errorf("%v", r))
			resp = Response{ID: req.ID, Err: fmt.Sprintf("gocacheprog: put panicked: %v", r)}
		}
	}()
	s.stats.Puts.Add(1)
	path, err := s.store.Put(req.ActionID, req.OutputID, req.BodySize, body)
	if err != nil {
		s.stats.Errors.Add(1)
		s.warn.warn("put", err)
		return Response{ID: req.ID, Err: "gocacheprog: " + err.Error()}
	}
	s.stats.PutBytes.Add(req.BodySize)
	return Response{ID: req.ID, DiskPath: path}
}

func (s *Server) handleGet(req Request) (resp Response) {
	defer func() {
		if r := recover(); r != nil {
			s.stats.Errors.Add(1)
			s.warn.warn("get-panic", fmt.Errorf("%v", r))
			resp = Response{ID: req.ID, Miss: true}
		}
	}()
	s.stats.Gets.Add(1)
	e, ok, err := s.store.Get(req.ActionID)
	if err != nil {
		s.stats.Errors.Add(1)
		s.warn.warn("get", err)
		return Response{ID: req.ID, Miss: true}
	}
	if !ok {
		s.stats.Misses.Add(1)
		return Response{ID: req.ID, Miss: true}
	}
	if e.Remote {
		s.stats.HitsRemote.Add(1)
	} else {
		s.stats.HitsLocal.Add(1)
	}
	s.stats.GetBytes.Add(e.Size)
	t := e.Time
	return Response{ID: req.ID, OutputID: e.OutputID, Size: e.Size, Time: &t, DiskPath: e.DiskPath}
}
