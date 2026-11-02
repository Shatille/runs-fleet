package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/gocacheprog"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

func installFakeGoCacheHelper(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runs-fleet-gocacheprog")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	prev := GoCacheHelperPath
	GoCacheHelperPath = path
	t.Cleanup(func() { GoCacheHelperPath = prev })
	return path
}

func goCacheConfig() *secrets.RunnerConfig {
	return &secrets.RunnerConfig{
		GoCacheBucket: "runs-fleet-cache",
		GoCacheRegion: "ap-northeast-1",
		GoCachePrefix: "gocache/acme/widgets",
	}
}

func readEnvLines(t *testing.T, runnerPath string) []string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(runnerPath, ".env"))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(content)), "\n")
}

func TestWriteGoCacheEnv_WritesEveryVarWithHelperLast(t *testing.T) {
	helper := installFakeGoCacheHelper(t, 0o755)
	runnerPath := t.TempDir()
	r := &Registrar{logger: &mockLogger{}}

	setup := r.WriteGoCacheEnv(runnerPath, goCacheConfig())
	if setup.Status != "" || setup.OutcomeFile != filepath.Join(runnerPath, "_rf_gocache_outcome") {
		t.Fatalf("setup = %+v", setup)
	}
	lines := readEnvLines(t, runnerPath)
	want := []string{
		"RUNS_FLEET_GOCACHE_DIR=" + filepath.Join(runnerPath, "_rf_gocache"),
		"RUNS_FLEET_GOCACHE_OUTCOME=" + setup.OutcomeFile,
		"RUNS_FLEET_GOCACHE_BUCKET=runs-fleet-cache",
		"RUNS_FLEET_GOCACHE_REGION=ap-northeast-1",
		"RUNS_FLEET_GOCACHE_PREFIX=gocache/acme/widgets",
		"GOCACHEPROG=" + helper,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf(".env lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if _, err := os.Stat(filepath.Join(runnerPath, "_rf_gocache")); !os.IsNotExist(err) {
		t.Fatalf("cache dir pre-created by the agent (err=%v); the helper must create it as the runner user", err)
	}
}

func TestWriteGoCacheEnv_DisabledWhenBucketAbsent(t *testing.T) {
	installFakeGoCacheHelper(t, 0o755)
	runnerPath := t.TempDir()
	r := &Registrar{logger: &mockLogger{}}

	for _, cfg := range []*secrets.RunnerConfig{nil, {}} {
		setup := r.WriteGoCacheEnv(runnerPath, cfg)
		if setup.Status != GoCacheDisabled || setup.OutcomeFile != "" {
			t.Fatalf("setup = %+v, want disabled", setup)
		}
	}
	if _, err := os.Stat(filepath.Join(runnerPath, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env written although the cache is disabled (err=%v)", err)
	}
}

func TestWriteGoCacheEnv_DegradedWithoutHelperAndWritesNothing(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"missing": func(t *testing.T) {
			prev := GoCacheHelperPath
			GoCacheHelperPath = filepath.Join(t.TempDir(), "absent")
			t.Cleanup(func() { GoCacheHelperPath = prev })
		},
		"not executable":        func(t *testing.T) { installFakeGoCacheHelper(t, 0o644) },
		"owner-only executable": func(t *testing.T) { installFakeGoCacheHelper(t, 0o750) },
		"directory": func(t *testing.T) {
			prev := GoCacheHelperPath
			GoCacheHelperPath = t.TempDir()
			t.Cleanup(func() { GoCacheHelperPath = prev })
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			arrange(t)
			runnerPath := t.TempDir()
			logger := &mockLogger{}
			r := &Registrar{logger: logger}
			setup := r.WriteGoCacheEnv(runnerPath, goCacheConfig())
			if setup.Status != GoCacheDegraded || setup.OutcomeFile != "" {
				t.Fatalf("setup = %+v, want degraded", setup)
			}
			if _, err := os.Stat(filepath.Join(runnerPath, ".env")); !os.IsNotExist(err) {
				t.Fatalf(".env written although the helper is unusable (err=%v)", err)
			}
			if len(logger.messages) == 0 {
				t.Fatal("no warning logged")
			}
		})
	}
}

func TestWriteGoCacheEnv_DegradedWhenEnvWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	installFakeGoCacheHelper(t, 0o755)
	runnerPath := t.TempDir()
	if err := os.Chmod(runnerPath, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(runnerPath, 0o700) })
	r := &Registrar{logger: &mockLogger{}}

	setup := r.WriteGoCacheEnv(runnerPath, goCacheConfig())
	if setup.Status != GoCacheDegraded {
		t.Fatalf("setup = %+v, want degraded", setup)
	}
}

func TestReadGoCacheOutcome_SetupStatusPassesThrough(t *testing.T) {
	for _, status := range []string{GoCacheDisabled, GoCacheDegraded} {
		got, stats := ReadGoCacheOutcome(GoCacheSetup{Status: status})
		if got != status || stats != nil {
			t.Fatalf("ReadGoCacheOutcome(%s) = %q, %v", status, got, stats)
		}
	}
}

func TestReadGoCacheOutcome_MissingFileIsUnused(t *testing.T) {
	got, stats := ReadGoCacheOutcome(GoCacheSetup{OutcomeFile: filepath.Join(t.TempDir(), "absent")})
	if got != GoCacheUnused || stats != nil {
		t.Fatalf("got %q, %v", got, stats)
	}
}

func TestReadGoCacheOutcome_SumsEveryHelperInvocation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "outcome")
	for _, s := range []gocacheprog.Summary{
		{Gets: 10, HitsLocal: 2, HitsRemote: 3, Misses: 5, Puts: 5, DownloadBytes: 300, Uploads: 5, UploadBytes: 500},
		{Gets: 4, HitsLocal: 4, PutBytes: 7},
	} {
		if err := gocacheprog.AppendOutcome(file, s); err != nil {
			t.Fatal(err)
		}
	}
	got, stats := ReadGoCacheOutcome(GoCacheSetup{OutcomeFile: file})
	if got != GoCacheEngaged || stats == nil {
		t.Fatalf("got %q, %v", got, stats)
	}
	want := gocacheprog.Summary{Gets: 14, HitsLocal: 6, HitsRemote: 3, Misses: 5, Puts: 5, DownloadBytes: 300, Uploads: 5, UploadBytes: 500, PutBytes: 7}
	if *stats != want {
		t.Fatalf("stats = %+v, want %+v", *stats, want)
	}
}

func TestReadGoCacheOutcome_Status(t *testing.T) {
	cases := map[string]struct {
		lines []gocacheprog.Summary
		want  string
	}{
		"helper fault":    {[]gocacheprog.Summary{{Gets: 1, Errors: 1}}, GoCacheDegraded},
		"remote errors":   {[]gocacheprog.Summary{{Gets: 9, HitsRemote: 1}, {Gets: 1, RemoteErrors: 5}}, GoCacheDegraded},
		"failed uploads":  {[]gocacheprog.Summary{{Puts: 3, UploadsFailed: 1}}, GoCacheDegraded},
		"cold bucket":     {[]gocacheprog.Summary{{Gets: 50, Misses: 50, Puts: 50, Uploads: 50}}, GoCacheEngaged},
		"only go version": {[]gocacheprog.Summary{{}, {}}, GoCacheUnused},
		"dropped uploads": {[]gocacheprog.Summary{{Puts: 3, UploadsDropped: 2, UploadsSkipped: 1, UploadsAbandoned: 1}}, GoCacheEngaged},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "outcome")
			for _, s := range tc.lines {
				if err := gocacheprog.AppendOutcome(file, s); err != nil {
					t.Fatal(err)
				}
			}
			got, stats := ReadGoCacheOutcome(GoCacheSetup{OutcomeFile: file})
			if got != tc.want || stats == nil {
				t.Fatalf("got %q, stats %v; want %q with stats", got, stats, tc.want)
			}
		})
	}
}

func TestReadGoCacheOutcome_DamagedFileIsDegradedButKeepsIntactLines(t *testing.T) {
	file := filepath.Join(t.TempDir(), "outcome")
	if err := os.WriteFile(file, []byte("{\"gets\":7,\"puts\":2}\n{\"ge"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, stats := ReadGoCacheOutcome(GoCacheSetup{OutcomeFile: file})
	if got != GoCacheDegraded || stats == nil || stats.Gets != 7 || stats.Puts != 2 {
		t.Fatalf("got %q, %+v", got, stats)
	}

	if err := os.WriteFile(file, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, stats = ReadGoCacheOutcome(GoCacheSetup{OutcomeFile: file})
	if got != GoCacheDegraded || stats != nil {
		t.Fatalf("got %q, %v", got, stats)
	}
}
