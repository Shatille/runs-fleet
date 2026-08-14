// Command mirror-proxy serves the local Docker Hub mirror on runs-fleet
// runners (see pkg/mirrorproxy), configured by ECR_PULL_THROUGH_ENDPOINT.
// Loopback only: the mirror is a per-host convenience for dockerd and
// BuildKit, never a network service.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"

	"github.com/Shavakan/runs-fleet/pkg/mirrorproxy"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8989", "loopback address to serve the mirror on")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	endpoint := os.Getenv("ECR_PULL_THROUGH_ENDPOINT")
	if endpoint == "" {
		logger.Error("ECR_PULL_THROUGH_ENDPOINT is not set; nothing to mirror onto")
		os.Exit(1)
	}

	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		logger.Error("failed to load AWS config", "error", err)
		os.Exit(1)
	}

	ecrClient := ecr.NewFromConfig(awsCfg)
	handler, err := mirrorproxy.New(endpoint, mirrorproxy.NewECRTokenSource(ecrClient))
	if err != nil {
		logger.Error("invalid mirror configuration", "endpoint", endpoint, "error", err)
		os.Exit(1)
	}
	if rules, err := mirrorproxy.DiscoverRules(context.Background(), ecrClient); err != nil {
		logger.Warn("pull-through rule discovery failed; serving docker.io only", "error", err)
	} else {
		handler.AddRules(rules)
		logger.Info("mirror routing discovered", "rules", rules)
	}

	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Info("mirror proxy serving", "listen", *listen, "endpoint", endpoint)
	if err := server.ListenAndServe(); err != nil {
		logger.Error("mirror proxy exited", "error", err)
		os.Exit(1)
	}
}
