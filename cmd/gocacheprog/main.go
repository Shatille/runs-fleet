// Command gocacheprog is the GOCACHEPROG helper runs-fleet runners point
// cmd/go at. It is started once per go invocation and exits when go closes it.
package main

import (
	"fmt"
	"os"

	"github.com/Shavakan/runs-fleet/pkg/gocacheprog"
)

func main() {
	cfg, err := gocacheprog.ConfigFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs-fleet-gocacheprog: %v\n", err)
		os.Exit(1)
	}
	store, err := gocacheprog.NewLocal(cfg.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs-fleet-gocacheprog: open cache dir %s: %v\n", cfg.Dir, err)
		os.Exit(1)
	}
	stats := &gocacheprog.Stats{}
	runErr := gocacheprog.NewServer(store, stats, cfg.GetWorkers).Run(os.Stdin, os.Stdout)
	if cfg.OutcomeFile != "" {
		if aerr := gocacheprog.AppendOutcome(cfg.OutcomeFile, stats.Summary()); aerr != nil {
			fmt.Fprintf(os.Stderr, "runs-fleet-gocacheprog: write outcome: %v\n", aerr)
		}
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "runs-fleet-gocacheprog: %v\n", runErr)
		os.Exit(1)
	}
}
