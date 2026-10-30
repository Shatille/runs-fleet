package gocacheprog

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestLocal(t *testing.T) *Local {
	t.Helper()
	l, err := NewLocal(filepath.Join(t.TempDir(), "l1"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLocal_PutThenGetRoundTrip(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	body := []byte("compiled object")
	outputID := sha256.Sum256(body)
	before := time.Now().Add(-time.Second)

	path, err := l.Put(testID(1), outputID[:], int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("DiskPath %q is not absolute", path)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(onDisk, body) {
		t.Fatalf("body on disk = %q, err %v", onDisk, err)
	}
	if fi, serr := os.Stat(path); serr != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("body mode = %v, err %v; want 0644", fi.Mode().Perm(), serr)
	}

	e, ok, err := l.Get(testID(1))
	if err != nil || !ok {
		t.Fatalf("get: ok %v, err %v", ok, err)
	}
	if !bytes.Equal(e.OutputID, outputID[:]) || e.Size != int64(len(body)) || e.DiskPath != path {
		t.Fatalf("entry = %+v", e)
	}
	if e.Time.Before(before) {
		t.Fatalf("put time %v predates the put", e.Time)
	}
}

func TestLocal_GetMissIsNotAnError(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	_, ok, err := l.Get(testID(9))
	if err != nil || ok {
		t.Fatalf("miss: ok %v, err %v", ok, err)
	}
}

func TestLocal_GetRejectsBodyWhoseSizeDriftedFromIndex(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	body := []byte("abc")
	outputID := sha256.Sum256(body)
	path, err := l.Put(testID(1), outputID[:], 3, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if werr := os.WriteFile(path, []byte("abcd"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	_, ok, err := l.Get(testID(1))
	if ok || err == nil {
		t.Fatalf("drifted body: ok %v, err %v; want miss with error", ok, err)
	}
}

func TestLocal_GetMissesWhenBodyFileIsGone(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	outputID := sha256.Sum256([]byte("x"))
	path, err := l.Put(testID(1), outputID[:], 1, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if rerr := os.Remove(path); rerr != nil {
		t.Fatal(rerr)
	}
	_, ok, err := l.Get(testID(1))
	if ok || err != nil {
		t.Fatalf("body gone: ok %v, err %v; want plain miss", ok, err)
	}
}

func TestLocal_PutShortBodyLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	outputID := sha256.Sum256([]byte("short"))
	if _, err := l.Put(testID(1), outputID[:], 10, strings.NewReader("short")); err == nil {
		t.Fatal("short body accepted")
	}
	if _, ok, _ := l.Get(testID(1)); ok {
		t.Fatal("index written for a failed put")
	}
	if _, err := os.Stat(l.outputPath(outputID[:])); err == nil {
		t.Fatal("body file published for a failed put")
	}
	leftovers, _ := filepath.Glob(filepath.Join(l.dir, "tmp", "*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestLocal_PutRejectsWrongLengthOutputID(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	if _, err := l.Put(testID(1), []byte{1, 2, 3}, 0, nil); err == nil {
		t.Fatal("3-byte output id accepted")
	}
}

func TestLocal_PutZeroLengthBody(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	outputID := sha256.Sum256(nil)
	path, err := l.Put(testID(1), outputID[:], 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() != 0 {
		t.Fatalf("empty body: %v, size %d", err, fi.Size())
	}
	if _, ok, gerr := l.Get(testID(1)); !ok || gerr != nil {
		t.Fatalf("get empty: ok %v, err %v", ok, gerr)
	}
}

func TestNewLocal_SweepsStaleTempFilesOnly(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "l1")
	tmpDir := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(tmpDir, "w-stale")
	fresh := filepath.Join(tmpDir, "w-fresh")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleTempAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := NewLocal(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("stale temp file survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh temp file removed: %v", err)
	}
}
