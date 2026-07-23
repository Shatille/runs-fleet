package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

func TestWriteBuildkitCacheEnv_WritesLinesWhenConfigPresent(t *testing.T) {
	tmpDir := t.TempDir()
	r := &Registrar{logger: &mockLogger{}}

	cfg := &secrets.RunnerConfig{
		BuildkitCacheBucket: "runs-fleet-cache",
		BuildkitCacheRegion: "ap-northeast-1",
		BuildkitCachePrefix: "buildkit/acme/widgets/",
	}
	outcomeFile := r.WriteBuildkitCacheEnv(tmpDir, cfg)
	if outcomeFile == "" {
		t.Fatal("expected non-empty outcome file path when config present")
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".env"))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	s := string(content)
	for _, want := range []string{
		"RUNS_FLEET_BUILDKIT_CACHE_BUCKET=runs-fleet-cache",
		"RUNS_FLEET_BUILDKIT_CACHE_REGION=ap-northeast-1",
		"RUNS_FLEET_BUILDKIT_CACHE_PREFIX=buildkit/acme/widgets/",
		"RUNS_FLEET_BUILDKIT_CACHE_OUTCOME=" + outcomeFile,
	} {
		if !contains(s, want) {
			t.Errorf("expected %q in .env, got:\n%s", want, s)
		}
	}
}

func TestWriteBuildkitCacheEnv_NoopWhenBucketAbsent(t *testing.T) {
	tmpDir := t.TempDir()
	r := &Registrar{logger: &mockLogger{}}

	// Bucket empty → feature disabled → nothing written, empty outcome path.
	outcomeFile := r.WriteBuildkitCacheEnv(tmpDir, &secrets.RunnerConfig{})
	if outcomeFile != "" {
		t.Errorf("expected empty outcome path when bucket absent, got %q", outcomeFile)
	}

	// .env may or may not exist; if it does it must carry no buildkit lines.
	if content, err := os.ReadFile(filepath.Join(tmpDir, ".env")); err == nil {
		if contains(string(content), "RUNS_FLEET_BUILDKIT_CACHE_BUCKET") {
			t.Error("must not write buildkit env when bucket absent")
		}
	}
}

func TestReadBuildCacheOutcome_ReadsLastLine(t *testing.T) {
	tmpDir := t.TempDir()
	outcomeFile := filepath.Join(tmpDir, "_rf_buildkit_cache_outcome")
	if err := os.WriteFile(outcomeFile, []byte("skipped:default-builder\nengaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The most recent build's outcome is the last non-empty line.
	if got := ReadBuildCacheOutcome(outcomeFile); got != "engaged" {
		t.Errorf("ReadBuildCacheOutcome = %q, want engaged", got)
	}
}

func TestReadBuildCacheOutcome_EmptyPathIsDisabled(t *testing.T) {
	if got := ReadBuildCacheOutcome(""); got != BuildCacheDisabled {
		t.Errorf("ReadBuildCacheOutcome(\"\") = %q, want %q", got, BuildCacheDisabled)
	}
}

func TestReadBuildCacheOutcome_MissingFileIsDisabled(t *testing.T) {
	// The shim was never invoked (no docker build in the job) → no file → disabled.
	if got := ReadBuildCacheOutcome("/no/such/outcome-file"); got != BuildCacheDisabled {
		t.Errorf("ReadBuildCacheOutcome(missing) = %q, want %q", got, BuildCacheDisabled)
	}
}

func TestReadBuildCacheOutcome_NormalizesSkippedAndFailedPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	tests := []struct {
		raw  string
		want string
	}{
		{"engaged", "engaged"},
		{"skipped:user-cache-flags", "skipped"},
		{"failed:imds", "failed"},
		{"disabled", "disabled"},
	}
	for _, tt := range tests {
		f := filepath.Join(tmpDir, "out-"+tt.want)
		if err := os.WriteFile(f, []byte(tt.raw+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ReadBuildCacheOutcome(f); got != tt.want {
			t.Errorf("ReadBuildCacheOutcome(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
