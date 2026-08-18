// Command mirror-proxy serves the local Docker Hub mirror on runs-fleet
// runners (see pkg/mirrorproxy), configured by ECR_PULL_THROUGH_ENDPOINT.
// Loopback only: the mirror is a per-host convenience for dockerd and
// BuildKit, never a network service.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ecr"

	"github.com/Shavakan/runs-fleet/pkg/mirrorproxy"
)

// resolveRegion returns cfgRegion when the ambient config already resolved one,
// otherwise the region reported by fromIMDS. A systemd unit starts with a
// near-empty environment, so AWS_REGION is usually absent here and the SDK's
// own IMDS lookup stays dormant unless explicitly opted into; without a region
// every ECR call fails and the mirror can only answer 502.
func resolveRegion(ctx context.Context, cfgRegion string, fromIMDS func(context.Context) (string, error)) (string, error) {
	if cfgRegion != "" {
		return cfgRegion, nil
	}
	region, err := fromIMDS(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve region from IMDS: %w", err)
	}
	if region == "" {
		return "", fmt.Errorf("IMDS returned no region")
	}
	return region, nil
}

func imdsRegion(awsCfg aws.Config) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		out, err := imds.NewFromConfig(awsCfg).GetRegion(ctx, &imds.GetRegionInput{})
		if err != nil {
			return "", err
		}
		return out.Region, nil
	}
}

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

	region, err := resolveRegion(context.Background(), awsCfg.Region, imdsRegion(awsCfg))
	if err != nil {
		logger.Error("no AWS region available; every ECR call would fail and the mirror could only serve 502", "error", err)
		os.Exit(1)
	}
	awsCfg.Region = region

	ecrClient := ecr.NewFromConfig(awsCfg)
	handler, err := mirrorproxy.New(endpoint, mirrorproxy.NewECRTokenSource(ecrClient))
	if err != nil {
		logger.Error("invalid mirror configuration", "endpoint", endpoint, "error", err)
		os.Exit(1)
	}
	rules, err := mirrorproxy.DiscoverRules(context.Background(), ecrClient)
	if err != nil {
		logger.Error("pull-through rule discovery failed; refusing to serve a mirror that would 502 every pull", "error", err)
		os.Exit(1)
	}
	handler.AddRules(rules)
	logger.Info("mirror routing discovered", "rules", rules)

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
