package gocacheprog

import (
	"fmt"
	"strconv"
)

// Environment variables the helper reads; the agent writes them into the
// runner .env and tests set them directly.
const (
	EnvDir        = "RUNS_FLEET_GOCACHE_DIR"
	EnvOutcome    = "RUNS_FLEET_GOCACHE_OUTCOME"
	EnvGetWorkers = "RUNS_FLEET_GOCACHE_GET_WORKERS"
)

const defaultGetWorkers = 8

// Config is the helper's runtime configuration.
type Config struct {
	Dir         string
	OutcomeFile string
	GetWorkers  int
}

// ConfigFromEnv builds a Config from lookup (normally os.Getenv). The cache
// directory is required; an invalid worker count falls back to the default.
func ConfigFromEnv(lookup func(string) string) (Config, error) {
	cfg := Config{
		Dir:         lookup(EnvDir),
		OutcomeFile: lookup(EnvOutcome),
		GetWorkers:  defaultGetWorkers,
	}
	if cfg.Dir == "" {
		return Config{}, fmt.Errorf("%s is not set", EnvDir)
	}
	if n, err := strconv.Atoi(lookup(EnvGetWorkers)); err == nil && n > 0 {
		cfg.GetWorkers = n
	}
	return cfg, nil
}
