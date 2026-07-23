// Package main implements the runs-fleet agent that runs on EC2 instances
// to execute GitHub Actions jobs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

	timings := bootstrapTimings{boot: readUptime(), start: time.Now()}

	instanceID := os.Getenv("RUNS_FLEET_INSTANCE_ID")
	if instanceID == "" {
		log.Fatal("RUNS_FLEET_INSTANCE_ID environment variable is required")
	}
	maxRuntimeMinutes := getEnvInt("RUNS_FLEET_MAX_RUNTIME_MINUTES", 360)
	standbyDeadline := time.Now().Add(time.Duration(getEnvInt("RUNS_FLEET_STANDBY_DEADLINE_MINUTES", 120)) * time.Minute)

	ac, err := initStore(ctx)
	if err != nil {
		log.Fatalf("Failed to initialize agent store: %v", err)
	}
	if closer, ok := ac.secretsStore.(interface{ Close() }); ok {
		defer closer.Close()
	}

	// Uniform standby poll: wait for this instance's job config. A cold-start
	// instance's config appears within the fast-poll window; a hot-pool spare
	// waits (bounded) until it is assigned. Either sentinel exit is clean (0):
	// the instance was never given a job, so there is nothing to fail.
	//
	// The poll runs under a SIGTERM-aware context so an instance stop (the
	// reconciler banking an idle spare) cancels the wait and the agent exits 0
	// promptly instead of being killed mid-sleep. Signal handling is scoped to
	// standby only: once a job config is found, stop() restores default signal
	// behavior and the job runs under the plain background context, exactly as
	// before this feature — a SIGTERM must never abort a job in flight.
	standbyCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	runnerConfig, err := standbyWaitForConfig(standbyCtx, ac.secretsStore, instanceID, standbyDeadline, logger)
	stop()
	if err != nil {
		if errors.Is(err, errStandbyDeadline) {
			logger.Println("standby deadline reached without a job — exiting for reconciler to reclaim")
		} else {
			logger.Printf("standby ended without a job (%v) — exiting", err)
		}
		os.Exit(0)
	}

	// Config found: the meaningful acquisition latency starts here, not at
	// process start. Rebasing keeps the standby wait (and a hot spare's long-ago
	// boot) out of the bootstrap total, so the metric cohort is clean.
	configFoundAt := time.Now()
	timings.boot = 0
	timings.config = 0
	timings.start = configFoundAt

	if err := completeInit(ctx, ac, instanceID, runnerConfig, logger); err != nil {
		log.Fatalf("Failed to initialize agent: %v", err)
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

	runAgent(ctx, ac, downloader, executor, cleanup, instanceID, jobID, logger, &timings)
}

// resolveJobID returns the GitHub job_id (used to key the job record) from the
// runner config, or "" when unset.
func resolveJobID(ac *agentConfig) string {
	if ac.runnerConfig == nil {
		return ""
	}
	return ac.runnerConfig.JobID
}

// initStore initializes the AWS config and secrets store — the components needed
// to poll for job config during standby. The config-dependent components
// (telemetry, terminator, CloudWatch logger) are built later by completeInit,
// once a job config has actually been acquired, so an unassigned standby spare
// never spins them up.
func initStore(ctx context.Context) (*agentConfig, error) {
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

	return ac, nil
}

// completeInit builds the config-dependent agent components (telemetry,
// terminator, CloudWatch logger) once a job config has been acquired. Split from
// initStore so a standby spare that is never assigned a job does not create SQS/
// CloudWatch clients it will never use.
func completeInit(ctx context.Context, ac *agentConfig, instanceID string, runnerConfig *secrets.RunnerConfig, logger *stdLogger) error {
	ac.runnerConfig = runnerConfig

	terminationQueueURL := runnerConfig.TerminationQueueURL
	if terminationQueueURL != "" {
		ac.telemetry = agent.NewSQSTelemetry(ac.awsCfg, terminationQueueURL, logger)
	}

	ac.terminator = agent.NewEC2Terminator(ac.awsCfg, ac.telemetry, logger)

	logGroup := os.Getenv("RUNS_FLEET_LOG_GROUP")
	if logGroup != "" {
		logStream := fmt.Sprintf("%s/%s", instanceID, runnerConfig.RunID)
		cwLogger := agent.NewCloudWatchLogger(ac.awsCfg, logGroup, logStream, logger)
		if startErr := cwLogger.Start(ctx); startErr != nil {
			logger.Printf("Warning: failed to start CloudWatch logger: %v", startErr)
		} else {
			ac.cwLogger = cwLogger
			logger.Printf("CloudWatch logging enabled: %s/%s", logGroup, logStream)
		}
	}

	return nil
}

// runAgent executes the agent phases.
func runAgent(ctx context.Context, ac *agentConfig, downloader *agent.Downloader,
	executor *agent.Executor, cleanup *agent.Cleanup,
	instanceID, jobID string, logger *stdLogger, timings *bootstrapTimings) {

	if ac.cwLogger != nil {
		defer ac.cwLogger.Stop()
	}

	logger.Println("Phase 1: Downloading GitHub Actions runner...")
	runnerStart := time.Now()
	runnerPath, err := downloader.DownloadRunner(ctx)
	if err != nil {
		logger.Printf("Failed to download runner: %v", err)
		terminateWithError(ctx, ac.terminator, instanceID, jobID, "download_failed", err)
		return
	}
	timings.runner = time.Since(runnerStart)
	logger.Printf("Runner downloaded successfully to %s", runnerPath)

	logger.Println("Phase 2: Registering runner with GitHub...")
	registerStart := time.Now()
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

	// Transparent Docker layer cache: write the buildkit-cache env vars into the
	// runner .env so the on-host buildx shim can inject S3 cache flags. Inert
	// when the orchestrator did not configure a bucket (empty outcome file).
	buildCacheOutcomeFile := registrar.WriteBuildkitCacheEnv(runnerPath, ac.runnerConfig)

	// Engage the transparent v2 cache interceptor. Best effort and fail-open:
	// any failure leaves the runner talking to GitHub's cache directly.
	cacheProxy, cacheInterception, cacheStop := engageCache(ctx, ac, registrar, runnerPath, logger)
	if cacheStop != nil {
		defer cacheStop()
	}
	timings.register = time.Since(registerStart)

	jobStartedAt := time.Now()

	if ac.telemetry != nil {
		jobStatus := agent.JobStatus{
			InstanceID: instanceID,
			JobID:      jobID,
			StartedAt:  jobStartedAt,
		}
		timings.applyTo(&jobStatus, jobStartedAt)
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
	toolCacheBefore, beforeErr := agent.SnapshotToolCache(toolCacheDir)
	if beforeErr != nil {
		logger.Printf("Warning: tool cache snapshot (pre-job) failed: %v", beforeErr)
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

	toolCacheAfter, afterErr := agent.SnapshotToolCache(toolCacheDir)
	if afterErr != nil {
		logger.Printf("Warning: tool cache snapshot (post-job) failed: %v", afterErr)
	}
	// Only report misses when BOTH snapshots succeeded: a failed pre-job snapshot
	// leaves an empty baseline, which would mis-report every pre-baked tool as a miss.
	var toolCacheMisses []string
	if beforeErr == nil && afterErr == nil {
		toolCacheMisses = agent.DiffToolCache(toolCacheBefore, toolCacheAfter)
	}

	jobStatus := agent.JobStatus{
		InstanceID:             instanceID,
		JobID:                  jobID,
		ExitCode:               result.ExitCode,
		StartedAt:              result.StartedAt,
		CompletedAt:            result.CompletedAt,
		DurationSeconds:        int(result.Duration.Seconds()),
		InterruptedBy:          result.InterruptedBy,
		ToolCacheMisses:        toolCacheMisses,
		CacheInterception:      cacheInterception,
		BuildCacheInterception: agent.ReadBuildCacheOutcome(buildCacheOutcomeFile),
	}
	if cacheProxy != nil {
		jobStatus.CacheBytesWritten = cacheProxy.BytesWritten()
	}

	if result.Error != nil {
		jobStatus.Error = result.Error.Error()
	}

	if termErr := ac.terminator.TerminateInstance(ctx, instanceID, jobStatus); termErr != nil {
		logger.Printf("Failed to terminate instance: %v", termErr)
	}

	logger.Println("Agent completed successfully")
}

// Cache interception outcomes reported to the orchestrator as a metric.
const (
	cacheInterceptionEngaged  = "engaged"  // proxy + CA trust + pin all succeeded
	cacheInterceptionFailed   = "failed"   // a fail-open branch — traffic goes to GitHub
	cacheInterceptionDisabled = "disabled" // no cache URL configured for this deployment
)

// engageCache starts the on-host cache interceptor and redirects the runner's
// cache traffic to it. Every step is fail-open: on any error the function logs
// and leaves the runner pointed at GitHub's own cache. It returns the running
// proxy (nil unless engaged, for reading write-byte stats), the outcome status,
// and a teardown closure (nil unless engaged). The pin is installed LAST, only
// after the CA is trusted, so traffic is never redirected to an untrusted listener.
func engageCache(ctx context.Context, ac *agentConfig, registrar *agent.Registrar, runnerPath string, logger *stdLogger) (*cacheproxy.Proxy, string, func()) {
	if ac.runnerConfig.CacheURL == "" {
		return nil, cacheInterceptionDisabled, nil
	}
	cp, err := cacheproxy.New(cacheproxy.Config{
		OrchestratorBaseURL: ac.runnerConfig.CacheURL,
		CacheToken:          ac.runnerConfig.CacheToken,
		StagingDir:          filepath.Join(runnerPath, "_rf_cache_staging"),
	})
	if err != nil {
		logger.Printf("cache interceptor disabled (fail open): %v", err)
		return nil, cacheInterceptionFailed, nil
	}
	if err := cp.Start(ctx); err != nil {
		logger.Printf("cache interceptor start failed (fail open): %v", err)
		return nil, cacheInterceptionFailed, nil
	}
	caPath := filepath.Join(runnerPath, "runs-fleet-cache-ca.pem")
	if err := provisionCacheTrust(cp, registrar, runnerPath, caPath); err != nil {
		logger.Printf("cache interceptor not engaged (fail open): %v", err)
		_ = cp.Stop(context.Background())
		return nil, cacheInterceptionFailed, nil
	}
	if err := cacheproxy.EngageCacheTrustAndPin(cacheproxy.DefaultResultsHost, cp.CACertPEM()); err != nil {
		logger.Printf("cache interceptor engage failed (fail open): %v", err)
		_ = cp.Stop(context.Background())
		return nil, cacheInterceptionFailed, nil
	}
	logger.Printf("cache interceptor engaged: %s -> %s", cacheproxy.DefaultResultsHost, cp.Addr())
	return cp, cacheInterceptionEngaged, func() {
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

// bootstrapTimings accumulates the agent-side startup segments. start is the
// monotonic process-start reference used for the total; boot is the /proc/uptime
// reading (kernel + cloud-init + bootstrap scripts) that precedes the process.
type bootstrapTimings struct {
	boot     float64
	config   time.Duration
	runner   time.Duration
	register time.Duration
	start    time.Time
}

// applyTo copies the captured segments onto a JobStatus in seconds. The total is
// the start-to-jobStartedAt span (a monotonic delta) and is left zero when start
// was never captured, so an unset reference can't fabricate a bogus value.
func (bt bootstrapTimings) applyTo(js *agent.JobStatus, jobStartedAt time.Time) {
	js.BootstrapBootSeconds = bt.boot
	js.BootstrapConfigSeconds = bt.config.Seconds()
	js.BootstrapRunnerSeconds = bt.runner.Seconds()
	js.BootstrapRegisterSeconds = bt.register.Seconds()
	if !bt.start.IsZero() {
		js.BootstrapTotalSeconds = jobStartedAt.Sub(bt.start).Seconds()
	}
}

// readUptime returns the seconds-since-boot from /proc/uptime, or 0 on any error
// (the file is absent off Linux, e.g. darwin test hosts; production is AL2023).
func readUptime() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	return parseUptime(data)
}

// parseUptime extracts the first (uptime) field from /proc/uptime contents,
// returning 0 when the field is missing or unparseable.
func parseUptime(data []byte) float64 {
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return v
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
