package gocacheprog

import "testing"

func TestConfigFromEnv(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		EnvDir:        "/var/cache/rf",
		EnvOutcome:    "/tmp/outcome",
		EnvGetWorkers: "3",
	}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dir != env[EnvDir] || cfg.OutcomeFile != env[EnvOutcome] || cfg.GetWorkers != 3 {
		t.Fatalf("cfg = %+v", cfg)
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
	if bad.GetWorkers != defaultGetWorkers || bad.OutcomeFile != "" {
		t.Fatalf("defaults = %+v", bad)
	}
}
