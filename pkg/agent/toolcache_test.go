package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeMarker creates <dir>/<tool>/<version>/<platform>.complete (and the version dir).
func writeMarker(t *testing.T, dir, tool, version, platform string) {
	t.Helper()
	vdir := filepath.Join(dir, tool, version)
	if err := os.MkdirAll(filepath.Join(vdir, platform), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vdir, platform+".complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotToolCache(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Python", "3.12.13", "x64")
	writeMarker(t, dir, "node", "22.13.1", "x64")
	// A version dir WITHOUT a .complete marker must not be counted (incomplete install).
	if err := os.MkdirAll(filepath.Join(dir, "Ruby", "3.4.8", "x64"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A .complete nested deeper than <Tool>/<version>/<platform> must be ignored
	// (depth guard: exactly 2 separators in the key).
	deep := filepath.Join(dir, "Weird", "1.0.0", "x64", "extra")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "x64.complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := SnapshotToolCache(dir)
	want := map[string]struct{}{
		"Python/3.12.13/x64": {},
		"node/22.13.1/x64":   {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot = %v, want %v", got, want)
	}
}

func TestSnapshotToolCache_MissingDir(t *testing.T) {
	// A nonexistent tool cache must yield an empty set, not panic/error.
	if got := SnapshotToolCache(filepath.Join(t.TempDir(), "nope")); len(got) != 0 {
		t.Errorf("expected empty snapshot for missing dir, got %v", got)
	}
}

func TestDiffToolCache(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Python", "3.12.13", "x64") // pre-baked / already present
	before := SnapshotToolCache(dir)

	// Simulate the job downloading two more versions on-demand.
	writeMarker(t, dir, "Python", "3.10.14", "x64")
	writeMarker(t, dir, "node", "18.20.4", "arm64")
	after := SnapshotToolCache(dir)

	got := DiffToolCache(before, after)
	want := []string{"Python/3.10.14/x64", "node/18.20.4/arm64"} // sorted, new-only
	if !reflect.DeepEqual(got, want) {
		t.Errorf("diff = %v, want %v", got, want)
	}

	// No new entries → no misses.
	if misses := DiffToolCache(after, after); len(misses) != 0 {
		t.Errorf("expected no misses for identical snapshots, got %v", misses)
	}
}
