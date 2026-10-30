package gocacheprog

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Entry describes a cached output.
type Entry struct {
	OutputID []byte
	Size     int64
	Time     time.Time
	DiskPath string
}

// Local is the on-disk cache tier: an index file per action ID pointing at a
// body file per output ID.
type Local struct {
	dir string
}

type indexRecord struct {
	OutputID string    `json:"output_id"`
	Size     int64     `json:"size"`
	Time     time.Time `json:"time"`
}

const staleTempAge = time.Hour

// NewLocal opens or creates the cache directory tree under dir and sweeps
// temp files left by a helper that was killed mid-write.
func NewLocal(dir string) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"a", "o", "tmp"} {
		if merr := os.MkdirAll(filepath.Join(abs, sub), 0o755); merr != nil {
			return nil, merr
		}
	}
	sweepStaleTemp(filepath.Join(abs, "tmp"), time.Now().Add(-staleTempAge))
	return &Local{dir: abs}, nil
}

func sweepStaleTemp(tmpDir string, olderThan time.Time) {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(olderThan) {
			continue
		}
		_ = os.Remove(filepath.Join(tmpDir, e.Name()))
	}
}

// Get looks an action ID up and reports whether a usable output is on disk.
func (l *Local) Get(actionID []byte) (Entry, bool, error) {
	raw, err := os.ReadFile(l.indexPath(actionID))
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	var rec indexRecord
	if uerr := json.Unmarshal(raw, &rec); uerr != nil {
		return Entry{}, false, fmt.Errorf("index %x: %w", actionID, uerr)
	}
	outputID, derr := hex.DecodeString(rec.OutputID)
	if derr != nil || len(outputID) != HashSize {
		return Entry{}, false, fmt.Errorf("index %x: bad output id %q", actionID, rec.OutputID)
	}
	path := l.outputPath(outputID)
	fi, serr := os.Stat(path)
	if errors.Is(serr, fs.ErrNotExist) {
		return Entry{}, false, nil
	}
	if serr != nil {
		return Entry{}, false, serr
	}
	if fi.Size() != rec.Size {
		return Entry{}, false, fmt.Errorf("output %s: size %d, index says %d", rec.OutputID, fi.Size(), rec.Size)
	}
	return Entry{OutputID: outputID, Size: rec.Size, Time: rec.Time, DiskPath: path}, true, nil
}

// Put stores body as the output for actionID and returns the body's path.
func (l *Local) Put(actionID, outputID []byte, size int64, body io.Reader) (string, error) {
	if len(outputID) != HashSize {
		return "", fmt.Errorf("output id has %d bytes, want %d", len(outputID), HashSize)
	}
	if body == nil {
		body = bytes.NewReader(nil)
	}
	path := l.outputPath(outputID)
	if err := l.writeAtomic(path, size, body); err != nil {
		return "", err
	}
	rec, err := json.Marshal(indexRecord{OutputID: hex.EncodeToString(outputID), Size: size, Time: time.Now().UTC()})
	if err != nil {
		return "", err
	}
	if werr := l.writeAtomic(l.indexPath(actionID), int64(len(rec)), bytes.NewReader(rec)); werr != nil {
		return "", werr
	}
	return path, nil
}

func (l *Local) outputPath(outputID []byte) string {
	return filepath.Join(l.dir, "o", hex.EncodeToString(outputID))
}

func (l *Local) indexPath(actionID []byte) string {
	return filepath.Join(l.dir, "a", hex.EncodeToString(actionID))
}

func (l *Local) writeAtomic(path string, size int64, body io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Join(l.dir, "tmp"), "w-*")
	if err != nil {
		return err
	}
	n, cerr := io.Copy(tmp, body)
	if cerr == nil && n != size {
		cerr = fmt.Errorf("wrote %d bytes, want %d", n, size)
	}
	if cerr == nil {
		cerr = tmp.Chmod(0o644)
	}
	if clerr := tmp.Close(); cerr == nil {
		cerr = clerr
	}
	if cerr != nil {
		_ = os.Remove(tmp.Name())
		return cerr
	}
	if rerr := os.Rename(tmp.Name(), path); rerr != nil {
		_ = os.Remove(tmp.Name())
		return rerr
	}
	return nil
}
