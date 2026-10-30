package gocacheprog

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"
)

// Stats counts what one helper process did.
type Stats struct {
	Gets      atomic.Int64
	HitsLocal atomic.Int64
	Misses    atomic.Int64
	Puts      atomic.Int64
	Errors    atomic.Int64
	GetBytes  atomic.Int64
	PutBytes  atomic.Int64
}

// Summary is the JSON form of Stats, one line per helper process.
type Summary struct {
	Gets      int64 `json:"gets"`
	HitsLocal int64 `json:"hits_local"`
	Misses    int64 `json:"misses"`
	Puts      int64 `json:"puts"`
	Errors    int64 `json:"errors"`
	GetBytes  int64 `json:"get_bytes"`
	PutBytes  int64 `json:"put_bytes"`
}

// Summary snapshots the counters.
func (s *Stats) Summary() Summary {
	return Summary{
		Gets:      s.Gets.Load(),
		HitsLocal: s.HitsLocal.Load(),
		Misses:    s.Misses.Load(),
		Puts:      s.Puts.Load(),
		Errors:    s.Errors.Load(),
		GetBytes:  s.GetBytes.Load(),
		PutBytes:  s.PutBytes.Load(),
	}
}

// AppendOutcome appends one JSON line to path, creating the file if needed.
func AppendOutcome(path string, sum Summary) error {
	line, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// ReadOutcomes parses every JSON line AppendOutcome wrote to path. A missing
// file yields no summaries and no error.
func ReadOutcomes(path string) ([]Summary, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Summary
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var sum Summary
		if uerr := json.Unmarshal([]byte(line), &sum); uerr != nil {
			return nil, uerr
		}
		out = append(out, sum)
	}
	return out, sc.Err()
}
