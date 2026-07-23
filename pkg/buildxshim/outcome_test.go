package buildxshim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteOutcome_AppendsLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "_rf_buildkit_cache_outcome")

	WriteOutcome(path, "engaged")
	WriteOutcome(path, "skipped:default-builder")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read outcome file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || lines[0] != "engaged" || lines[1] != "skipped:default-builder" {
		t.Errorf("unexpected outcome contents: %q", string(b))
	}
}

func TestWriteOutcome_EmptyPathIsNoop(_ *testing.T) {
	// Must never panic or error when the path is unknown.
	WriteOutcome("", "engaged")
}

func TestWriteOutcome_UnwritablePathIsSilent(_ *testing.T) {
	// A path under a nonexistent directory must be swallowed, never fatal.
	WriteOutcome("/nonexistent-dir-xyz/_rf_buildkit_cache_outcome", "engaged")
}
