package gocacheprog

import (
	"io"
	"log"
	"sync"
)

type warner struct {
	mu     sync.Mutex
	logger *log.Logger
	seen   map[string]bool
}

func newWarner(w io.Writer) *warner {
	return &warner{logger: log.New(w, "runs-fleet-gocacheprog: ", 0), seen: map[string]bool{}}
}

func (w *warner) warn(class string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[class] {
		return
	}
	w.seen[class] = true
	w.logger.Printf("%s: %v (further %s errors suppressed)", class, err, class)
}
