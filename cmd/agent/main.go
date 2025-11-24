// Package main implements the runs-fleet agent that runs on EC2 instances to execute GitHub Actions jobs.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/agent"
	"github.com/aws/aws-sdk-go-v2/config"
)

type stdLogger struct{}

func (l *stdLogger) Printf(format string, v ...interface{}) {
	log.Printf(format, v...)
}

func (l *stdLogger) Println(v ...interface{}) {
	log.Println(v...)
}

func main() {
	logger := &stdLogger{}
	logger.Println("Starting runs-fleet agent...")

	// Set up panic recovery
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("PANIC: Agent crashed: %v", r)
			// Allow time for logs to flush
			time.Sleep(2 * time.Second)
			os.Exit(1)
		}
	}()

	ctx := context.Background()

	// Get required environment variables
	runID := os.Getenv("RUNS_FLEET_RUN_ID")
	if runID == "" {
		log.Fatal("RUNS_FLEET_RUN_ID environment variable is required")
	}

	instanceID := os.Getenv("RUNS_FLEET_INSTANCE_ID")
	if instanceID == "" {
		log.Fatal("RUNS_FLEET_INSTANCE_ID environment variable is required")
	}

	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "ap-northeast-1"
	}

	configBucket := os.Getenv("RUNS_FLEET_CONFIG_BUCKET")
	terminationQueueURL := os.Getenv("RUNS_FLEET_TERMINATION_QUEUE_URL")
	ssmParameterPath := os.Getenv("RUNS_FLEET_SSM_PARAMETER")
	maxRuntimeMinutes := getEnvInt("RUNS_FLEET_MAX_RUNTIME_MINUTES", 360)

	logger.Printf("Agent configuration: run_id=%s, instance_id=%s, region=%s, max_runtime=%dm",
		runID, instanceID, region, maxRuntimeMinutes)

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	// Initialize components
	var cacheClient agent.CacheClient
	if configBucket != "" {
		cacheClient = agent.NewCache(cfg, configBucket)
	}

	downloader := agent.NewDownloader(cacheClient)
	registrar := agent.NewRegistrar(cfg, logger)
	safetyMonitor := agent.NewSafetyMonitor(time.Duration(maxRuntimeMinutes)*time.Minute, logger)
	executor := agent.NewExecutor(logger, safetyMonitor)
	cleanup := agent.NewCleanup(logger)

	var telemetry *agent.Telemetry
	if terminationQueueURL != "" {
		telemetry = agent.NewTelemetry(cfg, terminationQueueURL, logger)
	}

	terminator := agent.NewTerminator(cfg, telemetry, logger)

	// Phase 1: Download runner
	logger.Println("Phase 1: Downloading GitHub Actions runner...")
	runnerPath, err := downloader.DownloadRunner(ctx)
	if err != nil {
		logger.Printf("Failed to download runner: %v", err)
		terminateWithError(ctx, terminator, instanceID, runID, "download_failed", err)
		return
	}
	logger.Printf("Runner downloaded successfully to %s", runnerPath)

	// Phase 2: Register runner
	logger.Println("Phase 2: Registering runner with GitHub...")
	if ssmParameterPath == "" {
		ssmParameterPath = "/runs-fleet/runners/" + instanceID + "/config"
	}

	runnerConfig, err := registrar.FetchConfig(ctx, ssmParameterPath)
	if err != nil {
		logger.Printf("Failed to fetch runner config: %v", err)
		terminateWithError(ctx, terminator, instanceID, runID, "config_failed", err)
		return
	}

	if err := registrar.RegisterRunner(ctx, runnerConfig, runnerPath); err != nil {
		logger.Printf("Failed to register runner: %v", err)
		terminateWithError(ctx, terminator, instanceID, runID, "registration_failed", err)
		return
	}
	logger.Println("Runner registered successfully")

	// Track job start time
	jobStartedAt := time.Now()

	// Send job started notification
	if telemetry != nil {
		jobStatus := agent.JobStatus{
			InstanceID: instanceID,
			JobID:      runID,
			StartedAt:  jobStartedAt,
		}
		if err := telemetry.SendJobStarted(ctx, jobStatus); err != nil {
			logger.Printf("Warning: failed to send job started notification: %v", err)
		}
	}

	// Phase 3: Execute job
	logger.Println("Phase 3: Executing job...")
	result, err := executor.ExecuteJob(ctx, runnerPath)
	if err != nil {
		logger.Printf("Job execution error: %v", err)
	}

	if result != nil {
		logger.Printf("Job result: exit_code=%d, duration=%s, interrupted_by=%s",
			result.ExitCode, result.Duration, result.InterruptedBy)
		// Use saved start time if result doesn't have one
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

	// Phase 4: Cleanup and termination
	logger.Println("Phase 4: Cleaning up and terminating...")

	// Clean up runner directory
	if err := cleanup.CleanupRunner(ctx, runnerPath); err != nil {
		logger.Printf("Warning: cleanup failed: %v", err)
	}

	// Prepare job status for termination notification
	jobStatus := agent.JobStatus{
		InstanceID:      instanceID,
		JobID:           runID,
		ExitCode:        result.ExitCode,
		StartedAt:       result.StartedAt,
		CompletedAt:     result.CompletedAt,
		DurationSeconds: int(result.Duration.Seconds()),
		InterruptedBy:   result.InterruptedBy,
	}

	if result.Error != nil {
		jobStatus.Error = result.Error.Error()
	}

	// Terminate instance
	if err := terminator.TerminateInstance(ctx, instanceID, jobStatus); err != nil {
		logger.Printf("Failed to terminate instance: %v", err)
		// If termination fails, rely on max runtime timeout
	}

	logger.Println("Agent completed successfully")
}

// terminateWithError terminates the instance after an error.
func terminateWithError(ctx context.Context, terminator *agent.Terminator, instanceID, runID, errorType string, err error) {
	jobStatus := agent.JobStatus{
		InstanceID:  instanceID,
		JobID:       runID,
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
