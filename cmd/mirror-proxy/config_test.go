package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The rendered config must name the address the proxy actually bound. Any
// second, independent lookup of the bridge could disagree with the bind, and
// BuildKit answers an address nothing listens on by pulling from Docker Hub —
// silently, which is the whole failure this file exists to prevent.
func TestBuildkitdConfigMirrorsEveryDiscoveredUpstream(t *testing.T) {
	got := buildkitdConfig("172.18.0.1:8989", map[string]string{
		"docker.io":       "docker-hub",
		"quay.io":         "quay",
		"registry.k8s.io": "k8s",
	})

	for _, upstream := range []string{"docker.io", "quay.io", "registry.k8s.io"} {
		block := "[registry.\"" + upstream + "\"]\n  mirrors = [\"172.18.0.1:8989\"]\n"
		if !strings.Contains(got, block) {
			t.Errorf("config missing mirror block for %s:\n%s", upstream, got)
		}
	}
	if !strings.Contains(got, "[registry.\"172.18.0.1:8989\"]\n  http = true\n") {
		t.Errorf("config missing the http=true block for the mirror itself:\n%s", got)
	}
}

// Deterministic order keeps the file byte-stable across restarts, so a reader
// diffing it does not see phantom churn from Go's random map iteration.
func TestBuildkitdConfigIsDeterministicallyOrdered(t *testing.T) {
	rules := map[string]string{"quay.io": "quay", "docker.io": "docker-hub", "registry.k8s.io": "k8s"}
	first := buildkitdConfig("172.17.0.1:8989", rules)
	for i := 0; i < 20; i++ {
		if got := buildkitdConfig("172.17.0.1:8989", rules); got != first {
			t.Fatalf("buildkitdConfig() is not deterministic:\n%s\n---\n%s", first, got)
		}
	}
	if idx := strings.Index(first, "docker.io"); idx == -1 || idx > strings.Index(first, "quay.io") {
		t.Errorf("expected upstreams in sorted order:\n%s", first)
	}
}

func TestWriteBuildkitdConfigReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "buildkitd.toml")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeBuildkitdConfig(path, "fresh contents\n"); err != nil {
		t.Fatalf("writeBuildkitdConfig() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh contents\n" {
		t.Errorf("config = %q, want %q", got, "fresh contents\n")
	}

	// A leftover temp file would accumulate on every restart.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the config", len(entries))
	}
}

// When the bridge is unreachable the config must go away rather than linger:
// a stale file names an address BuildKit cannot reach, which reads as a
// working mirror and silently falls back to Docker Hub. Removing it makes the
// buildx shim skip cleanly instead.
func TestRemoveBuildkitdConfigIsQuietWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buildkitd.toml")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeBuildkitdConfig(path); err != nil {
		t.Fatalf("removeBuildkitdConfig() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("config still present after removal: %v", err)
	}
	if err := removeBuildkitdConfig(path); err != nil {
		t.Errorf("removeBuildkitdConfig() on an absent file error = %v, want nil", err)
	}
}
