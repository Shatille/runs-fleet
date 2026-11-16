package gocacheprog

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestLocal_GetMissesWhenBodySizeDriftedFromRecord(t *testing.T) {
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
	if ok || err != nil {
		t.Fatalf("drifted body: ok %v, err %v; want plain miss", ok, err)
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
	leftovers, _ := filepath.Glob(filepath.Join(l.dir, tmpDirName, "*"))
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
	tmpDir := filepath.Join(root, tmpDirName)
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

func goRecord(actionID, outputID []byte, size int64, at time.Time) string {
	return fmt.Sprintf("v1 %x %x %20d %20d\n", actionID, outputID, size, at.UnixNano())
}

func goEntryPath(dir string, id []byte, kind string) string {
	return filepath.Join(dir, fmt.Sprintf("%02x", id[0]), fmt.Sprintf("%x-%s", id, kind))
}

func writeGoEntry(t *testing.T, dir string, actionID []byte, body []byte, at time.Time) []byte {
	t.Helper()
	outputID := sha256.Sum256(body)
	for _, f := range []struct {
		path string
		data []byte
	}{
		{goEntryPath(dir, outputID[:], "d"), body},
		{goEntryPath(dir, actionID, "a"), []byte(goRecord(actionID, outputID[:], int64(len(body)), at))},
	} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.path, f.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return outputID[:]
}

func TestLocal_PutWritesTheLayoutCmdGoReads(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	at := time.Unix(1_800_000_000, 123)
	l.now = func() time.Time { return at }
	body := []byte("compiled object")
	outputID := sha256.Sum256(body)

	path, err := l.Put(testID(0x20), outputID[:], int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if want := goEntryPath(l.dir, outputID[:], "d"); path != want {
		t.Fatalf("body path = %s, want %s", path, want)
	}
	record, err := os.ReadFile(goEntryPath(l.dir, testID(0x20), "a"))
	if err != nil {
		t.Fatal(err)
	}
	if want := goRecord(testID(0x20), outputID[:], int64(len(body)), at); string(record) != want {
		t.Fatalf("record = %q, want %q", record, want)
	}
	if len(record) != entrySize {
		t.Fatalf("record is %d bytes, cmd/go expects %d", len(record), entrySize)
	}
}

func TestLocal_GetServesAnEntryCmdGoWrote(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	at := time.Unix(1_700_000_000, 42)
	body := []byte("from a restored go-build cache")
	outputID := writeGoEntry(t, l.dir, testID(0x30), body, at)

	e, ok, err := l.Get(testID(0x30))
	if err != nil || !ok {
		t.Fatalf("get: ok %v, err %v", ok, err)
	}
	if !bytes.Equal(e.OutputID, outputID) || e.Size != int64(len(body)) || !e.Time.Equal(at) {
		t.Fatalf("entry = %+v", e)
	}
	if e.DiskPath != goEntryPath(l.dir, outputID, "d") || e.Remote {
		t.Fatalf("entry = %+v", e)
	}
}

func TestLocal_GetMissesOnRecordsCmdGoWouldReject(t *testing.T) {
	t.Parallel()

	body := []byte("x")
	outputID := sha256.Sum256(body)
	good := goRecord(testID(0x40), outputID[:], 1, time.Unix(1_700_000_000, 0))
	cases := map[string]string{
		"empty":            "",
		"truncated":        good[:entrySize-1],
		"too long":         good + "x",
		"wrong version":    "v2" + good[2:],
		"other action id":  goRecord(testID(0x41), outputID[:], 1, time.Unix(1_700_000_000, 0)),
		"non-hex output":   good[:3+hexSize+1] + strings.Repeat("z", hexSize) + good[3+hexSize+1+hexSize:],
		"negative size":    fmt.Sprintf("v1 %x %x %20d %20d\n", testID(0x40), outputID[:], -1, 1),
		"unparseable size": fmt.Sprintf("v1 %x %x %20s %20d\n", testID(0x40), outputID[:], "abc", 1),
	}
	for name, record := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l := newTestLocal(t)
			writeGoEntry(t, l.dir, testID(0x40), body, time.Unix(1_700_000_000, 0))
			if err := os.WriteFile(goEntryPath(l.dir, testID(0x40), "a"), []byte(record), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := l.Get(testID(0x40)); ok || err != nil {
				t.Fatalf("ok %v, err %v; want plain miss", ok, err)
			}
		})
	}
}

func TestLocal_GetResolvesAnExecutableEntryDirectory(t *testing.T) {
	t.Parallel()

	body := []byte("binary")
	for name, tc := range map[string]struct {
		files []string
		hit   bool
	}{
		"one file":  {[]string{"tool"}, true},
		"no file":   {nil, false},
		"two files": {[]string{"tool", "other"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l := newTestLocal(t)
			outputID := writeGoEntry(t, l.dir, testID(0x50), body, time.Unix(1_700_000_000, 0))
			entryDir := goEntryPath(l.dir, outputID, "d")
			if err := os.Remove(entryDir); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(entryDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range tc.files {
				if err := os.WriteFile(filepath.Join(entryDir, f), body, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			e, ok, err := l.Get(testID(0x50))
			if err != nil || ok != tc.hit {
				t.Fatalf("ok %v, err %v; want hit %v", ok, err, tc.hit)
			}
			if tc.hit && e.DiskPath != filepath.Join(entryDir, "tool") {
				t.Fatalf("DiskPath = %s", e.DiskPath)
			}
		})
	}
}

func TestLocal_GetMarksOnlyStaleEntriesUsed(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	for name, tc := range map[string]struct {
		age    time.Duration
		bumped bool
	}{
		"older than the interval": {2 * time.Hour, true},
		"recent":                  {10 * time.Minute, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l := newTestLocal(t)
			l.now = func() time.Time { return now }
			outputID := writeGoEntry(t, l.dir, testID(0x60), []byte("x"), now)
			was := now.Add(-tc.age)
			paths := []string{goEntryPath(l.dir, testID(0x60), "a"), goEntryPath(l.dir, outputID, "d")}
			for _, p := range paths {
				if err := os.Chtimes(p, was, was); err != nil {
					t.Fatal(err)
				}
			}

			if _, ok, err := l.Get(testID(0x60)); !ok || err != nil {
				t.Fatalf("get: ok %v, err %v", ok, err)
			}
			want := was
			if tc.bumped {
				want = now
			}
			for _, p := range paths {
				fi, err := os.Stat(p)
				if err != nil || !fi.ModTime().Equal(want) {
					t.Fatalf("%s mtime = %v (err %v), want %v", filepath.Base(p), fi.ModTime(), err, want)
				}
			}
		})
	}
}

func TestLocal_PutReusesAnExecutableEntryDirectory(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	body := []byte("binary")
	outputID := sha256.Sum256(body)
	entryDir := goEntryPath(l.dir, outputID[:], "d")
	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(entryDir, "tool")
	if err := os.WriteFile(inner, body, 0o755); err != nil {
		t.Fatal(err)
	}

	path, err := l.Put(testID(0x70), outputID[:], int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if path != inner {
		t.Fatalf("path = %s, want the existing binary %s", path, inner)
	}
	if e, ok, gerr := l.Get(testID(0x70)); !ok || gerr != nil || e.DiskPath != inner {
		t.Fatalf("get: %+v, ok %v, err %v", e, ok, gerr)
	}
}

func trimFixture(t *testing.T, now time.Time) (l *Local, old, oldDir, recent, foreign string) {
	t.Helper()
	l = newTestLocal(t)
	l.now = func() time.Time { return now }
	sub := filepath.Join(l.dir, "ab")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	old = filepath.Join(sub, "old-a")
	oldDir = filepath.Join(sub, "olddir-d")
	recent = filepath.Join(sub, "recent-d")
	foreign = filepath.Join(sub, "notes.txt")
	for _, p := range []string{old, recent, foreign} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "tool"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	expired := now.Add(-trimLimit - 2*mtimeInterval)
	kept := now.Add(-trimLimit + time.Hour)
	for p, at := range map[string]time.Time{old: expired, oldDir: expired, foreign: expired, recent: kept} {
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	return l, old, oldDir, recent, foreign
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestLocal_TrimRemovesOnlyExpiredCacheEntries(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	l, old, oldDir, recent, foreign := trimFixture(t, now)

	l.Trim()

	if exists(old) || exists(oldDir) {
		t.Fatalf("expired entries survived: file %v, dir %v", exists(old), exists(oldDir))
	}
	if !exists(recent) || !exists(foreign) {
		t.Fatalf("kept entries removed: recent %v, foreign %v", exists(recent), exists(foreign))
	}
	stamp, err := os.ReadFile(filepath.Join(l.dir, trimFile))
	if err != nil || string(stamp) != fmt.Sprintf("%d", now.Unix()) {
		t.Fatalf("trim.txt = %q, err %v", stamp, err)
	}
}

func TestLocal_TrimSkipsWhenTrimmedWithinADay(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	for name, tc := range map[string]struct {
		stamp   string
		trimmed bool
	}{
		"an hour ago":    {fmt.Sprintf("%d", now.Add(-time.Hour).Unix()), false},
		"two days ago":   {fmt.Sprintf("%d", now.Add(-48*time.Hour).Unix()), true},
		"far in future":  {fmt.Sprintf("%d", now.Add(48*time.Hour).Unix()), true},
		"corrupt":        {"not a number", true},
		"padded by a nl": {fmt.Sprintf("%d\n", now.Add(-time.Hour).Unix()), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l, old, _, _, _ := trimFixture(t, now)
			if err := os.WriteFile(filepath.Join(l.dir, trimFile), []byte(tc.stamp), 0o644); err != nil {
				t.Fatal(err)
			}

			l.Trim()

			if exists(old) == tc.trimmed {
				t.Fatalf("expired entry present = %v, want trimmed = %v", exists(old), tc.trimmed)
			}
		})
	}
}

func TestLocal_PutLeavesAnExecutableEntryItCannotUseAlone(t *testing.T) {
	t.Parallel()

	body := []byte("binary")
	outputID := sha256.Sum256(body)
	for name, files := range map[string]map[string]string{
		"empty directory":  {},
		"two files":        {"tool": "binary", "other": "binary"},
		"other content":    {"tool": "BINARY"},
		"still being made": {"tool": "bin"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l := newTestLocal(t)
			entryDir := goEntryPath(l.dir, outputID[:], "d")
			if err := os.MkdirAll(entryDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for f, content := range files {
				if err := os.WriteFile(filepath.Join(entryDir, f), []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			path, err := l.Put(testID(0x71), outputID[:], int64(len(body)), bytes.NewReader(body))
			if err != nil {
				t.Fatalf("put failed, which cmd/go would turn into a failed build: %v", err)
			}
			if onDisk, rerr := os.ReadFile(path); rerr != nil || !bytes.Equal(onDisk, body) {
				t.Fatalf("returned path holds %q, err %v", onDisk, rerr)
			}
			if strings.HasPrefix(path, entryDir) {
				t.Fatalf("body written inside go's executable entry: %s", path)
			}
			for f, content := range files {
				if onDisk, rerr := os.ReadFile(filepath.Join(entryDir, f)); rerr != nil || string(onDisk) != content {
					t.Fatalf("%s changed to %q, err %v", f, onDisk, rerr)
				}
			}
			if left, _ := os.ReadDir(entryDir); len(left) != len(files) {
				t.Fatalf("entry directory now holds %d files, want %d", len(left), len(files))
			}
		})
	}
}

type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

func TestLocal_PutKeepsABodyAlreadyOnDisk(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	body := []byte("shared by two actions")
	outputID := writeGoEntry(t, l.dir, testID(0x80), body, time.Unix(1_700_000_000, 0))
	bodyPath := goEntryPath(l.dir, outputID, "d")
	before, err := os.Stat(bodyPath)
	if err != nil {
		t.Fatal(err)
	}

	src := &countingReader{r: bytes.NewReader(body)}
	path, err := l.Put(testID(0x81), outputID, int64(len(body)), src)
	if err != nil || path != bodyPath {
		t.Fatalf("path = %s, err %v", path, err)
	}
	after, err := os.Stat(bodyPath)
	if err != nil || !os.SameFile(before, after) || src.read != 0 {
		t.Fatalf("body rewritten: same file %v, read %d bytes of the new body, err %v", os.SameFile(before, after), src.read, err)
	}
	if _, ok, gerr := l.Get(testID(0x81)); !ok || gerr != nil {
		t.Fatalf("second action not recorded: ok %v, err %v", ok, gerr)
	}
}

func TestLocal_PutReplacesACorruptBodyOfTheRightSize(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	body := []byte("good")
	outputID := writeGoEntry(t, l.dir, testID(0x82), body, time.Unix(1_700_000_000, 0))
	bodyPath := goEntryPath(l.dir, outputID, "d")
	if err := os.WriteFile(bodyPath, []byte("evil"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Put(testID(0x82), outputID, int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if onDisk, err := os.ReadFile(bodyPath); err != nil || !bytes.Equal(onDisk, body) {
		t.Fatalf("body on disk = %q, err %v", onDisk, err)
	}
}

func TestLocal_ConcurrentPutsAndGetsOfOneOutput(t *testing.T) {
	t.Parallel()

	l := newTestLocal(t)
	body := bytes.Repeat([]byte("object "), 4096)
	outputID := sha256.Sum256(body)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			action := testID(byte(0x90 + i%4))
			if _, err := l.Put(action, outputID[:], int64(len(body)), bytes.NewReader(body)); err != nil {
				t.Errorf("put: %v", err)
			}
			e, ok, err := l.Get(action)
			if err != nil || !ok {
				t.Errorf("get: ok %v, err %v", ok, err)
				return
			}
			if onDisk, rerr := os.ReadFile(e.DiskPath); rerr != nil || !bytes.Equal(onDisk, body) {
				t.Errorf("served a body that is not the output (err %v)", rerr)
			}
		})
	}
	wg.Wait()
}

func TestOpenLocal_FallsBackWhenTheCacheDirIsUnusable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	var log bytes.Buffer

	first, err := OpenLocal(filepath.Join(blocker, "go-build"), &log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(first.dir, blocker) || !strings.Contains(log.String(), "unusable") {
		t.Fatalf("dir = %s, log = %q; want a fallback directory and a warning", first.dir, log.String())
	}
	outputID := sha256.Sum256([]byte("x"))
	if _, perr := first.Put(testID(1), outputID[:], 1, strings.NewReader("x")); perr != nil {
		t.Fatalf("fallback tier unusable: %v", perr)
	}

	second, err := OpenLocal(filepath.Join(blocker, "go-build"), &log)
	if err != nil {
		t.Fatal(err)
	}
	if second.dir != first.dir {
		t.Fatalf("each helper start got its own fallback (%s, %s); they must share one", first.dir, second.dir)
	}
	if _, ok, gerr := second.Get(testID(1)); !ok || gerr != nil {
		t.Fatalf("second helper misses what the first stored: ok %v, err %v", ok, gerr)
	}
}

func TestNewLocal_RejectsADirectoryItCannotWriteTo(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}

	root := filepath.Join(t.TempDir(), "go-build")
	tmp := filepath.Join(root, tmpDirName)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o755) })

	if _, err := NewLocal(root); err == nil {
		t.Fatal("opened a cache directory whose temp dir is read-only; every put would fail the build")
	}
}
