package gocacheprog

import "testing"

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

	if _, merr := ConfigFromEnv(func(string) string { return "" }); merr == nil {
		t.Fatal("missing cache dir accepted")
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
