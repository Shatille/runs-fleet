package blobshim

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// memStore is an in-memory ObjectStore for handler tests.
type memStore struct {
	mu     sync.Mutex
	objs   map[string][]byte
	putErr error
	getErr error
}

func newMemStore() *memStore { return &memStore{objs: map[string][]byte{}} }

func (m *memStore) Put(_ context.Context, key string, body io.Reader) error {
	if m.putErr != nil {
		return m.putErr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = data
	return nil
}

func (m *memStore) Get(_ context.Context, key, rangeHeader string) (*GetResult, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	m.mu.Lock()
	data, ok := m.objs[key]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	total := int64(len(data))
	if rangeHeader == "" {
		return &GetResult{Body: io.NopCloser(strings.NewReader(string(data))), Info: ObjectInfo{Size: total}}, nil
	}
	start, end := parseTestRange(rangeHeader, total)
	chunk := data[start : end+1]
	return &GetResult{
		Body:         io.NopCloser(strings.NewReader(string(chunk))),
		Info:         ObjectInfo{Size: int64(len(chunk))},
		ContentRange: fmt.Sprintf("bytes %d-%d/%d", start, end, total),
		Partial:      true,
	}, nil
}

func (m *memStore) Head(_ context.Context, key string) (*ObjectInfo, error) {
	m.mu.Lock()
	data, ok := m.objs[key]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return &ObjectInfo{Size: int64(len(data))}, nil
}

// parseTestRange handles "bytes=start-end" and "bytes=start-".
func parseTestRange(h string, total int64) (int64, int64) {
	spec := strings.TrimPrefix(h, "bytes=")
	lo, hi, _ := strings.Cut(spec, "-")
	start, _ := strconv.ParseInt(lo, 10, 64)
	end := total - 1
	if hi != "" {
		end, _ = strconv.ParseInt(hi, 10, 64)
	}
	return start, end
}
