package gocacheprog

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Environment variables the helper reads; the agent writes them into the
// runner .env and tests set them directly.
const (
	EnvDir        = "RUNS_FLEET_GOCACHE_DIR"
	EnvOutcome    = "RUNS_FLEET_GOCACHE_OUTCOME"
	EnvGetWorkers = "RUNS_FLEET_GOCACHE_GET_WORKERS"
	EnvBucket     = "RUNS_FLEET_GOCACHE_BUCKET"
	EnvRegion     = "RUNS_FLEET_GOCACHE_REGION"
	EnvPrefix     = "RUNS_FLEET_GOCACHE_PREFIX"
)

const (
	defaultGetWorkers = 8
	envGoCache        = "GOCACHE"
)

// Config is the helper's runtime configuration. An empty Bucket keeps the
// helper local-only.
type Config struct {
	Dir         string
	OutcomeFile string
	GetWorkers  int
	Bucket      string
	Region      string
	Prefix      string
}

// ConfigFromEnv builds a Config from lookup (normally os.Getenv). The cache
// directory is go's own: EnvDir overrides it, otherwise the GOCACHE cmd/go
// exports to the helper, otherwise go's default. An invalid worker count
// falls back to the default.
func ConfigFromEnv(lookup func(string) string) (Config, error) {
	return configFromEnv(lookup, os.UserCacheDir)
}

func configFromEnv(lookup func(string) string, userCacheDir func() (string, error)) (Config, error) {
	dir, err := cacheDir(lookup, userCacheDir)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Dir:         dir,
		OutcomeFile: lookup(EnvOutcome),
		GetWorkers:  defaultGetWorkers,
		Bucket:      lookup(EnvBucket),
		Region:      lookup(EnvRegion),
		Prefix:      lookup(EnvPrefix),
	}
	if n, err := strconv.Atoi(lookup(EnvGetWorkers)); err == nil && n > 0 {
		cfg.GetWorkers = n
	}
	return cfg, nil
}

func cacheDir(lookup func(string) string, userCacheDir func() (string, error)) (string, error) {
	if dir := lookup(EnvDir); dir != "" {
		return dir, nil
	}
	if dir := lookup(envGoCache); dir != "" && dir != "off" {
		return dir, nil
	}
	base, err := userCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate go build cache: %w", err)
	}
	return filepath.Join(base, "go-build"), nil
}
