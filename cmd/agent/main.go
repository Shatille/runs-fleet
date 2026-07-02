// Package main implements the runs-fleet agent that runs on EC2 instances
// to execute GitHub Actions jobs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/agent"
	"github.com/Shavakan/runs-fleet/pkg/agent/cacheproxy"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
)

type stdLogger struct{}

func (l *stdLogger) Printf(format string, v ...interface{}) {
	log.Printf(format, v...)
}

func (l *stdLogger) Println(v ...interface{}) {
	log.Println(v...)
}

// agentConfig holds initialized components for the agent.
type agentConfig struct {
	telemetry    agent.TelemetryClient
	terminator   agent.InstanceTerminator
	runnerConfig *secrets.RunnerConfig
	cwLogger     *agent.CloudWatchLogger
	awsCfg       aws.Config
	secretsStore secrets.Store
}

func main() {
	logger := &stdLogger{}
	logger.Println("Starting runs-fleet agent...")

	defer func() {
		if r := recover(); r != nil {
			logger.Printf("PANIC: Agent crashed: %v", r)
			time.Sleep(2 * time.Second)
			os.Exit(1)
		}
	}()

	ctx := context.Background()

	instanceID := os.Getenv("RUNS_FLEET_INSTANCE_ID")
	if instanceID == "" {
		log.Fatal("RUNS_FLEET_INSTANCE_ID environment variable is required")
	}
	maxRuntimeMinutes := getEnvInt("RUNS_FLEET_MAX_RUNTIME_MINUTES", 360)

	ac, err := initAgent(ctx, instanceID, logger)
	if err != nil {
		if errors.Is(err, secrets.ErrConfigNotFound) {
			logger.Println("No job config found — instance in pool standby")
			os.Exit(0)
		}
		log.Fatalf("Failed to initialize agent: %v", err)
	}
	if closer, ok := ac.secretsStore.(interface{ Close() }); ok {
		defer closer.Close()
	}

	runID := ac.runnerConfig.RunID
	if runID == "" {
		log.Fatal("run_id missing from runner config")
	}

	jobID := resolveJobID(ac)

	logger.Printf("Agent configuration: run_id=%s, job_id=%s, instance_id=%s, max_runtime=%dm",
		runID, jobID, instanceID, maxRuntimeMinutes)

	downloader := agent.NewDownloader()
	safetyMonitor := agent.NewSafetyMonitor(time.Duration(maxRuntimeMinutes)*time.Minute, logger)
	executor := agent.NewExecutor(logger, safetyMonitor)
	cleanup := agent.NewCleanup(logger)

	if ac.cwLogger != nil {
		executor.SetCloudWatchLogger(ac.cwLogger)
	}

	runAgent(ctx, ac, downloader, executor, cleanup, instanceID, jobID, logger)
}

// resolveJobID returns the GitHub job_id (used to key the job record) from the
// runner config, or "" when unset.
func resolveJobID(ac *agentConfig) string {
	if ac.runnerConfig == nil {
		return ""
	}
	return ac.runnerConfig.JobID
}

const configFetchAttempts = 5

var configFetchRetryDelay = 2 * time.Second

// fetchRunnerConfig reads the runner config from the backend, retrying transient
// failures and the cold-start window before the orchestrator writes the config.
func fetchRunnerConfig(ctx context.Context, store secrets.Store, instanceID string, logger *stdLogger) (*secrets.RunnerConfig, error) {
	var lastErr error
	for attempt := 1; attempt <= configFetchAttempts; attempt++ {
		config, err := store.Get(ctx, instanceID)
		if err == nil {
			return config, nil
		}
		lastErr = err
		if attempt < configFetchAttempts {
			logger.Printf("config fetch attempt %d/%d failed: %v; retrying in %s",
				attempt, configFetchAttempts, err, configFetchRetryDelay)
			time.Sleep(configFetchRetryDelay)
		}
	}
	return nil, fmt.Errorf("config fetch failed after %d attempts: %w", configFetchAttempts, lastErr)
}

// initAgent initializes the agent components (AWS config, secrets, telemetry, terminator).
func initAgent(ctx context.Context, instanceID string, logger *stdLogger) (*agentConfig, error) {
	ac := &agentConfig{}

	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "ap-northeast-1"
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	ac.awsCfg = awsCfg

	secretsCfg := secrets.LoadConfig()
	secretsStore, err := secrets.NewStore(ctx, secretsCfg, awsCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create secrets store: %w", err)
	}
	ac.secretsStore = secretsStore

	runnerConfig, err := fetchRunnerConfig(ctx, secretsStore, instanceID, logger)
	if err != nil {
		return nil, err
	}
	ac.runnerConfig = runnerConfig

	terminationQueueURL := runnerConfig.TerminationQueueURL
	if terminationQueueURL != "" {
		ac.telemetry = agent.NewSQSTelemetry(awsCfg, terminationQueueURL, logger)
	}

	ac.terminator = agent.NewEC2Terminator(awsCfg, ac.telemetry, logger)

	logGroup := os.Getenv("RUNS_FLEET_LOG_GROUP")
	if logGroup != "" {
		logStream := fmt.Sprintf("%s/%s", instanceID, runnerConfig.RunID)
		cwLogger := agent.NewCloudWatchLogger(awsCfg, logGroup, logStream, logger)
		if startErr := cwLogger.Start(ctx); startErr != nil {
			logger.Printf("Warning: failed to start CloudWatch logger: %v", startErr)
		} else {
			ac.cwLogger = cwLogger
			logger.Printf("CloudWatch logging enabled: %s/%s", logGroup, logStream)
		}
	}

	return ac, nil
}

// runAgent executes the agent phases.
func runAgent(ctx context.Context, ac *agentConfig, downloader *agent.Downloader,
	executor *agent.Executor, cleanup *agent.Cleanup,
	instanceID, jobID string, logger *stdLogger) {

	if ac.cwLogger != nil {
		defer ac.cwLogger.Stop()
	}

	logger.Println("Phase 1: Downloading GitHub Actions runner...")
	runnerPath, err := downloader.DownloadRunner(ctx)
	if err != nil {
		logger.Printf("Failed to download runner: %v", err)
		terminateWithError(ctx, ac.terminator, instanceID, jobID, "download_failed", err)
		return
	}
	logger.Printf("Runner downloaded successfully to %s", runnerPath)

	logger.Println("Phase 2: Registering runner with GitHub...")
	registrar := agent.NewRegistrar(ac.secretsStore, logger)

	if regErr := registrar.RegisterRunner(ctx, ac.runnerConfig, runnerPath); regErr != nil {
		logger.Printf("Failed to register runner: %v", regErr)
		terminateWithError(ctx, ac.terminator, instanceID, jobID, "registration_failed", regErr)
		return
	}
	logger.Println("Runner registered successfully")

	cacheURL := ac.runnerConfig.CacheURL
	if envErr := registrar.SetRunnerEnvironment(runnerPath, cacheURL, ac.runnerConfig.CacheToken); envErr != nil {
		logger.Printf("Warning: failed to set runner environment: %v", envErr)
	}

	// Engage the transparent v2 cache interceptor. Best effort and fail-open:
	// any failure leaves the runner talking to GitHub's cache directly.
	if stop := engageCache(ctx, ac, registrar, runnerPath, logger); stop != nil {
		defer stop()
	}

	jobStartedAt := time.Now()

	if ac.telemetry != nil {
		jobStatus := agent.JobStatus{
			InstanceID: instanceID,
			JobID:      jobID,
			StartedAt:  jobStartedAt,
		}
		if sendErr := ac.telemetry.SendJobStarted(ctx, jobStatus); sendErr != nil {
			logger.Printf("Warning: failed to send job started notification: %v", sendErr)
		}
	}

	// Snapshot the Actions tool cache so we can report which tool versions the job
	// downloaded on-demand (not pre-baked). Best-effort telemetry; never gates the job.
	toolCacheDir := os.Getenv("AGENT_TOOLSDIRECTORY")
	if toolCacheDir == "" {
		toolCacheDir = agent.DefaultToolCacheDir
	}
	toolCacheBefore, tcErr := agent.SnapshotToolCache(toolCacheDir)
	if tcErr != nil {
		logger.Printf("Warning: tool cache snapshot (pre-job) failed: %v", tcErr)
	}

	logger.Println("Phase 3: Executing job...")
	result, err := executor.ExecuteJob(ctx, runnerPath)
	if err != nil {
		logger.Printf("Job execution error: %v", err)
	}

	if result != nil {
		logger.Printf("Job result: exit_code=%d, duration=%s, interrupted_by=%s",
			result.ExitCode, result.Duration, result.InterruptedBy)
		if result.StartedAt.IsZero() {
			result.StartedAt = jobStartedAt
		}
	} else {
		logger.Println("No job result available")
		completedAt := time.Now()
		result = &agent.JobResult{
			ExitCode:    -1,
			StartedAt:   jobStartedAt,
			CompletedAt: completedAt,
			Duration:    completedAt.Sub(jobStartedAt),
			Error:       err,
		}
	}

	logger.Println("Phase 4: Cleaning up and terminating...")

	if cleanErr := cleanup.CleanupRunner(ctx, runnerPath); cleanErr != nil {
		logger.Printf("Warning: cleanup failed: %v", cleanErr)
	}

	toolCacheAfter, tcErr := agent.SnapshotToolCache(toolCacheDir)
	if tcErr != nil {
		logger.Printf("Warning: tool cache snapshot (post-job) failed: %v", tcErr)
	}

	jobStatus := agent.JobStatus{
		InstanceID:      instanceID,
		JobID:           jobID,
		ExitCode:        result.ExitCode,
		StartedAt:       result.StartedAt,
		CompletedAt:     result.CompletedAt,
		DurationSeconds: int(result.Duration.Seconds()),
		InterruptedBy:   result.InterruptedBy,
		ToolCacheMisses: agent.DiffToolCache(toolCacheBefore, toolCacheAfter),
	}

	if result.Error != nil {
		jobStatus.Error = result.Error.Error()
	}

	if termErr := ac.terminator.TerminateInstance(ctx, instanceID, jobStatus); termErr != nil {
		logger.Printf("Failed to terminate instance: %v", termErr)
	}

	logger.Println("Agent completed successfully")
}

// engageCache starts the on-host cache interceptor and redirects the runner's
// cache traffic to it. Every step is fail-open: on any error the function logs
// and returns nil, leaving the runner pointed at GitHub's own cache. On success
// it returns a teardown closure. The pin is installed LAST, only after the CA
// is trusted, so traffic is never redirected to an untrusted listener.
func engageCache(ctx context.Context, ac *agentConfig, registrar *agent.Registrar, runnerPath string, logger *stdLogger) func() {
	if ac.runnerConfig.CacheURL == "" {
		return nil
	}
	cp, err := cacheproxy.New(cacheproxy.Config{
		OrchestratorBaseURL: ac.runnerConfig.CacheURL,
		CacheToken:          ac.runnerConfig.CacheToken,
		StagingDir:          filepath.Join(runnerPath, "_rf_cache_staging"),
	})
	if err != nil {
		logger.Printf("cache interceptor disabled (fail open): %v", err)
		return nil
	}
	if err := cp.Start(ctx); err != nil {
		logger.Printf("cache interceptor start failed (fail open): %v", err)
		return nil
	}
	caPath := filepath.Join(runnerPath, "runs-fleet-cache-ca.pem")
	if err := provisionCacheTrust(cp, registrar, runnerPath, caPath); err != nil {
		logger.Printf("cache interceptor not engaged (fail open): %v", err)
		_ = cp.Stop(context.Background())
		return nil
	}
	if err := cacheproxy.EngageCacheTrustAndPin(cacheproxy.DefaultResultsHost, cp.CACertPEM()); err != nil {
		logger.Printf("cache interceptor engage failed (fail open): %v", err)
		_ = cp.Stop(context.Background())
		return nil
	}
	logger.Printf("cache interceptor engaged: %s -> %s", cacheproxy.DefaultResultsHost, cp.Addr())
	return func() {
		if err := cacheproxy.DisengageCache(cacheproxy.DefaultResultsHost); err != nil {
			logger.Printf("cache disengage failed: %v", err)
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cp.Stop(stopCtx)
	}
}

// provisionCacheTrust makes the per-instance CA trusted for the runner's
// Node-based cache/artifact clients before any pin: it writes the CA file and
// points NODE_EXTRA_CA_CERTS at it. Both writes land under the runner dir, so
// this needs no privilege. The system trust store (for any non-Node client) is
// handled best-effort by the engage helper, since NODE_EXTRA_CA_CERTS already
// covers the v2 cache client.
func provisionCacheTrust(cp *cacheproxy.Proxy, registrar *agent.Registrar, runnerPath, caPath string) error {
	if err := cacheproxy.WriteCACert(caPath, cp.CACertPEM()); err != nil {
		return err
	}
	return registrar.AppendRunnerEnv(runnerPath, "NODE_EXTRA_CA_CERTS", caPath)
}

// terminateWithError terminates the instance after an error.
func terminateWithError(ctx context.Context, terminator agent.InstanceTerminator, instanceID, jobID, errorType string, err error) {
	jobStatus := agent.JobStatus{
		InstanceID:  instanceID,
		JobID:       jobID,
		Status:      "failure",
		ExitCode:    -1,
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
		Error:       errorType + ": " + err.Error(),
	}

	if termErr := terminator.TerminateInstance(ctx, instanceID, jobStatus); termErr != nil {
		log.Printf("Failed to terminate instance: %v", termErr)
	}
}

// getEnvInt gets an integer environment variable with a default value.
func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		var result int
		if _, err := fmt.Sscanf(value, "%d", &result); err == nil {
			return result
		}
	}
	return defaultValue
}
