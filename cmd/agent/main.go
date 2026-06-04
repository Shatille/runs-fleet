// Package main implements the runs-fleet agent that runs on EC2 instances
// to execute GitHub Actions jobs.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/agent"
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
		log.Fatalf("Failed to initialize agent: %v", err)
	}

	// Resolve run ID: env var takes precedence, then secrets store config.
	// In SSM mode the bootstrap only sets INSTANCE_ID and REGION — the run_id
	// comes from the config fetched by initAgent.
	runID := os.Getenv("RUNS_FLEET_RUN_ID")
	if runID == "" && ac.runnerConfig != nil {
		runID = ac.runnerConfig.RunID
	}
	if runID == "" {
		log.Fatal("RUNS_FLEET_RUN_ID not set and not found in runner config")
	}

	// Resolve job ID: env var takes precedence, then secrets store config.
	// In SSM mode the bootstrap does not export RUNS_FLEET_JOB_ID, so the
	// value comes from the config fetched by initAgent.
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

// resolveJobID resolves the GitHub job_id used to key the job record, returning
// an empty string when no source is available.
func resolveJobID(ac *agentConfig) string {
	jobID := os.Getenv("RUNS_FLEET_JOB_ID")
	if jobID == "" && ac.runnerConfig != nil {
		jobID = ac.runnerConfig.JobID
	}
	return jobID
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

	runnerConfig, err := secretsStore.Get(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch config from secrets store: %w", err)
	}
	ac.runnerConfig = runnerConfig

	terminationQueueURL := os.Getenv("RUNS_FLEET_TERMINATION_QUEUE_URL")
	if terminationQueueURL == "" {
		terminationQueueURL = runnerConfig.TerminationQueueURL
	}
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

	jobStatus := agent.JobStatus{
		InstanceID:      instanceID,
		JobID:           jobID,
		ExitCode:        result.ExitCode,
		StartedAt:       result.StartedAt,
		CompletedAt:     result.CompletedAt,
		DurationSeconds: int(result.Duration.Seconds()),
		InterruptedBy:   result.InterruptedBy,
	}

	if result.Error != nil {
		jobStatus.Error = result.Error.Error()
	}

	if termErr := ac.terminator.TerminateInstance(ctx, instanceID, jobStatus); termErr != nil {
		logger.Printf("Failed to terminate instance: %v", termErr)
	}

	logger.Println("Agent completed successfully")
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
