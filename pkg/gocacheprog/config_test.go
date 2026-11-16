package gocacheprog

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestConfigFromEnv(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		EnvDir:        "/var/cache/rf",
		EnvOutcome:    "/tmp/outcome",
		EnvGetWorkers: "3",
		EnvBucket:     "cache-bucket",
		EnvRegion:     "ap-northeast-1",
		EnvPrefix:     "gocache/org/repo",
	}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dir != env[EnvDir] || cfg.OutcomeFile != env[EnvOutcome] || cfg.GetWorkers != 3 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Bucket != env[EnvBucket] || cfg.Region != env[EnvRegion] || cfg.Prefix != env[EnvPrefix] {
		t.Fatalf("remote cfg = %+v", cfg)
	}

	bad, err := ConfigFromEnv(func(k string) string {
		switch k {
		case EnvDir:
			return "/x"
		case EnvGetWorkers:
			return "-2"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if bad.GetWorkers != defaultGetWorkers || bad.OutcomeFile != "" || bad.Bucket != "" {
		t.Fatalf("defaults = %+v", bad)
	}
}

func TestConfigFromEnv_CacheDirFollowsGo(t *testing.T) {
	t.Parallel()

	userCache := func() (string, error) { return "/home/runner/.cache", nil }
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"override wins":            {map[string]string{EnvDir: "/override", "GOCACHE": "/from/go"}, "/override"},
		"GOCACHE from cmd/go":      {map[string]string{"GOCACHE": "/from/go"}, "/from/go"},
		"GOCACHE=off is not a dir": {map[string]string{"GOCACHE": "off"}, filepath.Join("/home/runner/.cache", "go-build")},
		"go's default":             {nil, filepath.Join("/home/runner/.cache", "go-build")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg, err := configFromEnv(func(k string) string { return tc.env[k] }, userCache)
			if err != nil || cfg.Dir != tc.want {
				t.Fatalf("Dir = %q, err %v; want %q", cfg.Dir, err, tc.want)
			}
		})
	}

	noHome := func() (string, error) { return "", errors.New("$HOME is not defined") }
	if _, err := configFromEnv(func(string) string { return "" }, noHome); err == nil {
		t.Fatal("no cache dir resolvable, yet no error")
	}
}
