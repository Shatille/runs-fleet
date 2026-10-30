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
	Gets             atomic.Int64
	HitsLocal        atomic.Int64
	HitsRemote       atomic.Int64
	Misses           atomic.Int64
	Puts             atomic.Int64
	Errors           atomic.Int64
	GetBytes         atomic.Int64
	PutBytes         atomic.Int64
	DownloadBytes    atomic.Int64
	Uploads          atomic.Int64
	UploadBytes      atomic.Int64
	UploadsSkipped   atomic.Int64
	UploadsDropped   atomic.Int64
	UploadsFailed    atomic.Int64
	UploadsAbandoned atomic.Int64
	RemoteErrors     atomic.Int64
}

// Summary is the JSON form of Stats, one line per helper process. Errors
// counts helper-side faults; RemoteErrors counts remote-tier failures that
// were degraded to misses.
type Summary struct {
	Gets             int64 `json:"gets"`
	HitsLocal        int64 `json:"hits_local"`
	HitsRemote       int64 `json:"hits_remote"`
	Misses           int64 `json:"misses"`
	Puts             int64 `json:"puts"`
	Errors           int64 `json:"errors"`
	GetBytes         int64 `json:"get_bytes"`
	PutBytes         int64 `json:"put_bytes"`
	DownloadBytes    int64 `json:"download_bytes"`
	Uploads          int64 `json:"uploads"`
	UploadBytes      int64 `json:"upload_bytes"`
	UploadsSkipped   int64 `json:"uploads_skipped"`
	UploadsDropped   int64 `json:"uploads_dropped"`
	UploadsFailed    int64 `json:"uploads_failed"`
	UploadsAbandoned int64 `json:"uploads_abandoned"`
	RemoteErrors     int64 `json:"remote_errors"`
}

// Summary snapshots the counters.
func (s *Stats) Summary() Summary {
	return Summary{
		Gets:             s.Gets.Load(),
		HitsLocal:        s.HitsLocal.Load(),
		HitsRemote:       s.HitsRemote.Load(),
		Misses:           s.Misses.Load(),
		Puts:             s.Puts.Load(),
		Errors:           s.Errors.Load(),
		GetBytes:         s.GetBytes.Load(),
		PutBytes:         s.PutBytes.Load(),
		DownloadBytes:    s.DownloadBytes.Load(),
		Uploads:          s.Uploads.Load(),
		UploadBytes:      s.UploadBytes.Load(),
		UploadsSkipped:   s.UploadsSkipped.Load(),
		UploadsDropped:   s.UploadsDropped.Load(),
		UploadsFailed:    s.UploadsFailed.Load(),
		UploadsAbandoned: s.UploadsAbandoned.Load(),
		RemoteErrors:     s.RemoteErrors.Load(),
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
