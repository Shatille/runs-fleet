package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Shavakan/runs-fleet/pkg/gocacheprog"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

// Go build cache outcome values reported to the orchestrator.
const (
	GoCacheEngaged  = "engaged"
	GoCacheUnused   = "unused"
	GoCacheDegraded = "degraded"
	GoCacheDisabled = "disabled"
)

// GoCacheHelperPath is where the runner AMI installs the GOCACHEPROG helper.
// Overridable so tests can point it at a temp file.
var GoCacheHelperPath = "/usr/local/bin/runs-fleet-gocacheprog"

const (
	goCacheDir         = "_rf_gocache"
	goCacheOutcomeFile = "_rf_gocache_outcome"
	goCacheProgEnv     = "GOCACHEPROG"
)

// GoCacheSetup is WriteGoCacheEnv's decision for a job: a final Status when
// the cache could not be engaged, otherwise the outcome file the helper
// appends one JSON line per go invocation to.
type GoCacheSetup struct {
	Status      string
	OutcomeFile string
}

// WriteGoCacheEnv points every go invocation in the job at the GOCACHEPROG
// helper by appending its configuration to the runner .env. It engages only
// when the config carries a bucket and the helper is an executable file,
// because cmd/go treats a missing or crashing helper as fatal; GOCACHEPROG is
// written last so a partial .env never names a helper that lacks its config.
func (r *Registrar) WriteGoCacheEnv(runnerPath string, cfg *secrets.RunnerConfig) GoCacheSetup {
	if cfg == nil || cfg.GoCacheBucket == "" {
		return GoCacheSetup{Status: GoCacheDisabled}
	}
	if err := checkExecutable(GoCacheHelperPath); err != nil {
		r.logger.Printf("Warning: Go build cache not engaged: %v", err)
		return GoCacheSetup{Status: GoCacheDegraded}
	}
	outcomeFile := filepath.Join(runnerPath, goCacheOutcomeFile)
	entries := [][2]string{
		{gocacheprog.EnvDir, filepath.Join(runnerPath, goCacheDir)},
		{gocacheprog.EnvOutcome, outcomeFile},
		{gocacheprog.EnvBucket, cfg.GoCacheBucket},
		{gocacheprog.EnvRegion, cfg.GoCacheRegion},
		{gocacheprog.EnvPrefix, cfg.GoCachePrefix},
		{goCacheProgEnv, GoCacheHelperPath},
	}
	for _, e := range entries {
		if err := r.AppendRunnerEnv(runnerPath, e[0], e[1]); err != nil {
			r.logger.Printf("Warning: failed to write %s to .env: %v", e[0], err)
			return GoCacheSetup{Status: GoCacheDegraded}
		}
	}
	return GoCacheSetup{OutcomeFile: outcomeFile}
}

func checkExecutable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o001 == 0 {
		return fmt.Errorf("%s is not a file every user can execute (mode %s)", path, fi.Mode())
	}
	return nil
}

// ReadGoCacheOutcome sums what every helper invocation in the job recorded
// and classifies it: unused when no go command ran, degraded when any helper
// or remote fault occurred or the file is damaged, engaged otherwise. The
// stats are nil when no invocation was recorded.
func ReadGoCacheOutcome(setup GoCacheSetup) (string, *gocacheprog.Summary) {
	if setup.Status != "" {
		return setup.Status, nil
	}
	lines, err := gocacheprog.ReadOutcomes(setup.OutcomeFile)
	if len(lines) == 0 {
		if err != nil {
			return GoCacheDegraded, nil
		}
		return GoCacheUnused, nil
	}
	var total gocacheprog.Summary
	for _, line := range lines {
		total = total.Add(line)
	}
	switch {
	case err != nil || total.Errors+total.RemoteErrors+total.UploadsFailed > 0:
		return GoCacheDegraded, &total
	case total.Gets+total.Puts == 0:
		return GoCacheUnused, &total
	}
	return GoCacheEngaged, &total
}
