package gocacheprog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Entry describes a cached output. Remote marks one that was just fetched
// from the remote tier rather than found on disk.
type Entry struct {
	OutputID []byte
	Size     int64
	Time     time.Time
	DiskPath string
	Remote   bool
}

// Local is the on-disk cache tier, kept in cmd/go's own GOCACHE layout so go
// without the helper reads and writes the same entries.
type Local struct {
	dir string
	now func() time.Time
}

const (
	hexSize   = HashSize * 2
	entrySize = 2 + 1 + hexSize + 1 + hexSize + 1 + 20 + 1 + 20 + 1

	mtimeInterval = time.Hour
	trimInterval  = 24 * time.Hour
	trimLimit     = 5 * 24 * time.Hour

	tmpDirName   = ".rf-tmp"
	sideDirName  = ".rf-side"
	trimFile     = "trim.txt"
	staleTempAge = time.Hour
)

// NewLocal opens or creates the cache directory and sweeps temp files left
// by a helper that was killed mid-write.
func NewLocal(dir string) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	tmp := filepath.Join(abs, tmpDirName)
	if merr := os.MkdirAll(tmp, 0o755); merr != nil {
		return nil, merr
	}
	probe, perr := os.CreateTemp(tmp, "probe-*")
	if perr != nil {
		return nil, perr
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	sweepStaleTemp(tmp, time.Now().Add(-staleTempAge))
	return &Local{dir: abs, now: time.Now}, nil
}

// OpenLocal is NewLocal that falls back to a per-user temp directory when dir
// cannot be written: cmd/go treats a helper that exits as fatal, and dir is
// the user's own GOCACHE.
func OpenLocal(dir string, logw io.Writer) (*Local, error) {
	local, err := NewLocal(dir)
	if err == nil {
		return local, nil
	}
	fallback := filepath.Join(os.TempDir(), fmt.Sprintf("runs-fleet-gocache-%d", os.Getuid()))
	_, _ = fmt.Fprintf(logw, "runs-fleet-gocacheprog: cache dir %s unusable (%v); using %s\n", dir, err, fallback)
	local, ferr := NewLocal(fallback)
	if ferr != nil {
		return nil, fmt.Errorf("open cache dir %s: %w; fallback %s: %w", dir, err, fallback, ferr)
	}
	return local, nil
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
// A record or body cmd/go would not accept is a miss, not an error.
func (l *Local) Get(actionID []byte) (Entry, bool, error) {
	if len(actionID) != HashSize {
		return Entry{}, false, fmt.Errorf("action id has %d bytes, want %d", len(actionID), HashSize)
	}
	recordPath := l.recordPath(actionID)
	raw, err := os.ReadFile(recordPath)
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("read record %x: %w", actionID, err)
	}
	outputID, size, at, ok := parseRecord(raw, actionID)
	if !ok {
		return Entry{}, false, nil
	}
	path, bodySize, ok, err := l.body(outputID)
	if err != nil || !ok || bodySize != size {
		return Entry{}, false, err
	}
	l.markUsed(recordPath)
	l.markUsed(l.outputPath(outputID))
	return Entry{OutputID: outputID, Size: size, Time: at, DiskPath: path}, true, nil
}

func parseRecord(raw, actionID []byte) (outputID []byte, size int64, at time.Time, ok bool) {
	if len(raw) != entrySize || raw[0] != 'v' || raw[1] != '1' || raw[2] != ' ' ||
		raw[3+hexSize] != ' ' || raw[3+hexSize+1+hexSize] != ' ' ||
		raw[3+hexSize+1+hexSize+1+20] != ' ' || raw[entrySize-1] != '\n' {
		return nil, 0, time.Time{}, false
	}
	fields := raw[3:]
	recorded, derr := hex.DecodeString(string(fields[:hexSize]))
	if derr != nil || !bytes.Equal(recorded, actionID) {
		return nil, 0, time.Time{}, false
	}
	fields = fields[hexSize+1:]
	outputID, derr = hex.DecodeString(string(fields[:hexSize]))
	if derr != nil {
		return nil, 0, time.Time{}, false
	}
	fields = fields[hexSize+1:]
	size, serr := strconv.ParseInt(strings.TrimLeft(string(fields[:20]), " "), 10, 64)
	nanos, terr := strconv.ParseInt(strings.TrimLeft(string(fields[21:41]), " "), 10, 64)
	if serr != nil || terr != nil || size < 0 || nanos < 0 {
		return nil, 0, time.Time{}, false
	}
	return outputID, size, time.Unix(0, nanos), true
}

// cmd/go stores a cached executable as a -d directory holding the one binary.
func (l *Local) body(outputID []byte) (path string, size int64, ok bool, err error) {
	path = l.outputPath(outputID)
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	if !fi.IsDir() {
		return path, fi.Size(), true, nil
	}
	entries, rerr := os.ReadDir(path)
	if rerr != nil || len(entries) != 1 {
		return "", 0, false, nil
	}
	path = filepath.Join(path, entries[0].Name())
	if fi, err = os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return "", 0, false, nil
	}
	return path, fi.Size(), true, nil
}

func (l *Local) markUsed(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if now := l.now(); now.Sub(fi.ModTime()) >= mtimeInterval {
		_ = os.Chtimes(path, now, now)
	}
}

// Put stores body as the output for actionID and returns the body's path.
func (l *Local) Put(actionID, outputID []byte, size int64, body io.Reader) (string, error) {
	if len(actionID) != HashSize {
		return "", fmt.Errorf("action id has %d bytes, want %d", len(actionID), HashSize)
	}
	if len(outputID) != HashSize {
		return "", fmt.Errorf("output id has %d bytes, want %d", len(outputID), HashSize)
	}
	if body == nil {
		body = bytes.NewReader(nil)
	}
	path, stored := l.storedBody(outputID, size)
	recorded := true
	if !stored {
		path = l.outputPath(outputID)
		// A -d directory is go's executable entry, possibly mid-write by a go
		// process without the helper: leave it and serve this put from a side file.
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			path, recorded = filepath.Join(l.dir, sideDirName, hex.EncodeToString(outputID)), false
		}
		if err := l.writeAtomic(path, size, body); err != nil {
			return "", err
		}
	}
	if !recorded {
		return path, nil
	}
	record := fmt.Sprintf("v1 %x %x %20d %20d\n", actionID, outputID, size, l.now().UnixNano())
	if err := l.writeAtomic(l.recordPath(actionID), int64(len(record)), strings.NewReader(record)); err != nil {
		return "", err
	}
	return path, nil
}

// Verified by digest, as cmd/go does before skipping a write.
func (l *Local) storedBody(outputID []byte, size int64) (string, bool) {
	path, bodySize, ok, err := l.body(outputID)
	if err != nil || !ok || bodySize != size || !fileDigestIs(path, outputID) {
		return "", false
	}
	l.markUsed(l.outputPath(outputID))
	return path, true
}

func fileDigestIs(path string, want []byte) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, cerr := io.Copy(h, f); cerr != nil {
		return false
	}
	return bytes.Equal(h.Sum(nil), want)
}

// Trim applies cmd/go's expiry, which go skips while a GOCACHEPROG helper is
// set: at most once a day, drop entries unused for five days.
func (l *Local) Trim() {
	now := l.now()
	stampPath := filepath.Join(l.dir, trimFile)
	if raw, err := os.ReadFile(stampPath); err == nil {
		if sec, perr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); perr == nil {
			if since := now.Sub(time.Unix(sec, 0)); since < trimInterval && since > -mtimeInterval {
				return
			}
		}
	}
	stamp := strconv.FormatInt(now.Unix(), 10)
	_ = l.writeAtomic(stampPath, int64(len(stamp)), strings.NewReader(stamp))
	cutoff := now.Add(-trimLimit - mtimeInterval)
	for i := range 256 {
		trimSubdir(filepath.Join(l.dir, fmt.Sprintf("%02x", i)), cutoff)
	}
}

func trimSubdir(subdir string, cutoff time.Time) {
	entries, err := os.ReadDir(subdir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, "-a") && !strings.HasSuffix(name, "-d") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(subdir, name))
	}
}

// Close is a no-op: the local tier has nothing in flight.
func (l *Local) Close(context.Context) {}

func (l *Local) outputPath(outputID []byte) string {
	return l.entryPath(outputID, "d")
}

func (l *Local) recordPath(actionID []byte) string {
	return l.entryPath(actionID, "a")
}

func (l *Local) entryPath(id []byte, kind string) string {
	return filepath.Join(l.dir, fmt.Sprintf("%02x", id[0]), fmt.Sprintf("%x-%s", id, kind))
}

func (l *Local) writeAtomic(path string, size int64, body io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create cache subdirectory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Join(l.dir, tmpDirName), "w-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
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
