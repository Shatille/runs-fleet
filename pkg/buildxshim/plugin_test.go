package buildxshim

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverRealPlugin_PrefersRecordedPath(t *testing.T) {
	dir := t.TempDir()
	recorded := filepath.Join(dir, "real-buildx")
	if err := os.WriteFile(recorded, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordFile := filepath.Join(dir, "buildx-real-path")
	if err := os.WriteFile(recordFile, []byte(recorded+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := DiscoverRealPlugin(recordFile, []string{"/nonexistent/docker-buildx"})
	if got != recorded {
		t.Errorf("got %q, want recorded path %q", got, recorded)
	}
}

func TestDiscoverRealPlugin_FallsBackToSearchList(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "docker-buildx")
	if err := os.WriteFile(candidate, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Recorded path missing; search list has a real file.
	got := DiscoverRealPlugin(filepath.Join(dir, "missing"), []string{
		"/nonexistent/docker-buildx",
		candidate,
	})
	if got != candidate {
		t.Errorf("got %q, want fallback %q", got, candidate)
	}
}

func TestDiscoverRealPlugin_RecordedPathMustExist(t *testing.T) {
	dir := t.TempDir()
	recordFile := filepath.Join(dir, "buildx-real-path")
	// Record a path that does not exist; discovery must skip it.
	if err := os.WriteFile(recordFile, []byte("/gone/docker-buildx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "docker-buildx")
	if err := os.WriteFile(candidate, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := DiscoverRealPlugin(recordFile, []string{candidate})
	if got != candidate {
		t.Errorf("got %q, want search-list fallback %q when recorded path is dead", got, candidate)
	}
}

func TestDiscoverRealPlugin_ReturnsEmptyWhenNothingFound(t *testing.T) {
	got := DiscoverRealPlugin("/no/record", []string{"/no/a", "/no/b"})
	if got != "" {
		t.Errorf("expected empty string when nothing found, got %q", got)
	}
}
