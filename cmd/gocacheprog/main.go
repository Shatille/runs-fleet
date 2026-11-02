// Command gocacheprog is the GOCACHEPROG helper runs-fleet runners point
// cmd/go at. It is started once per go invocation and exits when go closes it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Shavakan/runs-fleet/pkg/gocacheprog"
)

func main() {
	cfg, err := gocacheprog.ConfigFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs-fleet-gocacheprog: %v\n", err)
		os.Exit(1)
	}
	local, err := gocacheprog.NewLocal(cfg.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs-fleet-gocacheprog: open cache dir %s: %v\n", cfg.Dir, err)
		os.Exit(1)
	}
	stats := &gocacheprog.Stats{}
	store := gocacheprog.NewCache(local, newRemote(cfg), stats)
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

func newRemote(cfg gocacheprog.Config) gocacheprog.Remote {
	if cfg.Bucket == "" {
		return nil
	}
	return gocacheprog.NewLazyRemote(func() (gocacheprog.Remote, error) {
		if cfg.Region == "" {
			return nil, errors.New(gocacheprog.EnvRegion + " is not set")
		}
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(cfg.Region))
		if err != nil {
			return nil, err
		}
		return gocacheprog.NewS3Remote(s3.NewFromConfig(awsCfg), cfg.Bucket, cfg.Prefix), nil
	})
}
