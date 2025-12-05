// Package main implements the runs-fleet agent that runs on EC2 instances or K8s pods
// to execute GitHub Actions jobs.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/agent"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
	runnerConfig *agent.RunnerConfig
	cacheClient  agent.CacheClient
	cwLogger     *agent.CloudWatchLogger
	awsCfg       aws.Config
	cleanup      func() // Cleanup function for deferred resources
}

func main() {
	logger := &stdLogger{}
	logger.Println("Starting runs-fleet agent...")

	isK8s := agent.IsK8sEnvironment()
	if isK8s {
		logger.Println("Running in Kubernetes mode")
	} else {
		logger.Println("Running in EC2 mode")
	}

	defer func() {
		if r := recover(); r != nil {
			logger.Printf("PANIC: Agent crashed: %v", r)
			time.Sleep(2 * time.Second)
			os.Exit(1)
		}
	}()

	ctx := context.Background()

	runID := os.Getenv("RUNS_FLEET_RUN_ID")
	if runID == "" {
		log.Fatal("RUNS_FLEET_RUN_ID environment variable is required")
	}

	instanceID := getInstanceID(isK8s)
	maxRuntimeMinutes := getEnvInt("RUNS_FLEET_MAX_RUNTIME_MINUTES", 360)

	logger.Printf("Agent configuration: run_id=%s, instance_id=%s, k8s=%v, max_runtime=%dm",
		runID, instanceID, isK8s, maxRuntimeMinutes)

	// Initialize components based on backend mode
	var ac *agentConfig
	var err error
	if isK8s {
		ac, err = initK8sMode(ctx, logger)
	} else {
		ac, err = initEC2Mode(ctx, instanceID, runID, logger)
	}
	if err != nil {
		log.Fatalf("Failed to initialize agent: %v", err)
	}
	if ac.cleanup != nil {
		defer ac.cleanup()
	}

	// Common initialization
	downloader := agent.NewDownloader(ac.cacheClient)
	safetyMonitor := agent.NewSafetyMonitor(time.Duration(maxRuntimeMinutes)*time.Minute, logger)
	executor := agent.NewExecutor(logger, safetyMonitor)
	cleanup := agent.NewCleanup(logger)

	if ac.cwLogger != nil {
		executor.SetCloudWatchLogger(ac.cwLogger)
	}

	// Run agent phases
	runAgent(ctx, ac, downloader, executor, cleanup, instanceID, runID, isK8s, logger)
}

// getInstanceID returns the instance/pod ID based on mode.
func getInstanceID(isK8s bool) string {
	instanceID := os.Getenv("RUNS_FLEET_INSTANCE_ID")
	if instanceID == "" {
		if isK8s {
			hostname, err := os.Hostname()
			if err != nil {
				log.Fatal("Failed to get hostname for K8s pod")
			}
			return hostname
		}
		log.Fatal("RUNS_FLEET_INSTANCE_ID environment variable is required")
	}
	return instanceID
}

// initK8sMode initializes components for Kubernetes mode.
func initK8sMode(ctx context.Context, logger *stdLogger) (*agentConfig, error) {
	ac := &agentConfig{}

	runnerConfig, err := agent.FetchK8sConfig(ctx, agent.DefaultK8sConfigPaths(), logger)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch K8s config: %w", err)
	}
	ac.runnerConfig = runnerConfig

	// Create Kubernetes clientset for cleanup operations
	var clientset kubernetes.Interface
	namespace := getK8sNamespace()

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		logger.Printf("Warning: failed to create in-cluster config: %v (cleanup will be skipped)", err)
	} else {
		clientset, err = kubernetes.NewForConfig(restConfig)
		if err != nil {
			logger.Printf("Warning: failed to create Kubernetes clientset: %v (cleanup will be skipped)", err)
			clientset = nil
		} else {
			logger.Printf("Kubernetes client initialized for namespace: %s", namespace)
		}
	}

	// Valkey telemetry (required when configured)
	valkeyAddr := os.Getenv("RUNS_FLEET_VALKEY_ADDR")
	if valkeyAddr != "" {
		valkeyPassword := os.Getenv("RUNS_FLEET_VALKEY_PASSWORD")
		valkeyDB := getEnvInt("RUNS_FLEET_VALKEY_DB", 0)
		valkeyTelemetry, valkeyErr := agent.NewValkeyTelemetry(
			valkeyAddr, valkeyPassword, valkeyDB,
			"runs-fleet:termination", logger,
		)
		if valkeyErr != nil {
			return nil, fmt.Errorf("failed to connect to Valkey at %s: %w", valkeyAddr, valkeyErr)
		}
		ac.telemetry = valkeyTelemetry
		ac.cleanup = func() {
			if closeErr := valkeyTelemetry.Close(); closeErr != nil {
				logger.Printf("Warning: failed to close Valkey connection: %v", closeErr)
			}
		}
	}

	ac.terminator = agent.NewK8sTerminator(clientset, namespace, ac.telemetry, logger)
	return ac, nil
}

// getK8sNamespace returns the namespace the pod is running in.
// Reads from the downward API file or falls back to "default".
func getK8sNamespace() string {
	// Read from service account namespace file (standard K8s location)
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil {
		return strings.TrimSpace(string(data))
	}

	return "default"
}

// initEC2Mode initializes components for EC2 mode.
func initEC2Mode(ctx context.Context, instanceID, runID string, logger *stdLogger) (*agentConfig, error) {
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

	// Config bucket for runner download cache
	configBucket := os.Getenv("RUNS_FLEET_CONFIG_BUCKET")
	if configBucket != "" {
		ac.cacheClient = agent.NewCache(awsCfg, configBucket)
	}

	// SQS telemetry
	terminationQueueURL := os.Getenv("RUNS_FLEET_TERMINATION_QUEUE_URL")
	if terminationQueueURL != "" {
		ac.telemetry = agent.NewSQSTelemetry(awsCfg, terminationQueueURL, logger)
	}

	ac.terminator = agent.NewEC2Terminator(awsCfg, ac.telemetry, logger)

	// Fetch config from SSM
	ssmParameterPath := os.Getenv("RUNS_FLEET_SSM_PARAMETER")
	if ssmParameterPath == "" {
		ssmParameterPath = "/runs-fleet/runners/" + instanceID + "/config"
	}

	registrar := agent.NewRegistrar(awsCfg, logger)
	runnerConfig, err := registrar.FetchConfig(ctx, ssmParameterPath)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch config from SSM: %w", err)
	}
	ac.runnerConfig = runnerConfig

	// CloudWatch logging (EC2 only)
	logGroup := os.Getenv("RUNS_FLEET_LOG_GROUP")
	if logGroup != "" {
		logStream := fmt.Sprintf("%s/%s", instanceID, runID)
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
	instanceID, runID string, isK8s bool, logger *stdLogger) {

	if ac.cwLogger != nil {
		defer ac.cwLogger.Stop()
	}

	// Phase 1: Download runner
	logger.Println("Phase 1: Downloading GitHub Actions runner...")
	runnerPath, err := downloader.DownloadRunner(ctx)
	if err != nil {
		logger.Printf("Failed to download runner: %v", err)
		terminateWithError(ctx, ac.terminator, instanceID, runID, "download_failed", err)
		return
	}
	logger.Printf("Runner downloaded successfully to %s", runnerPath)

	// Phase 2: Register runner
	logger.Println("Phase 2: Registering runner with GitHub...")

	var registrar *agent.Registrar
	if isK8s {
		registrar = agent.NewRegistrarWithoutAWS(logger)
	} else {
		registrar = agent.NewRegistrar(ac.awsCfg, logger)
	}

	if regErr := registrar.RegisterRunner(ctx, ac.runnerConfig, runnerPath); regErr != nil {
		logger.Printf("Failed to register runner: %v", regErr)
		terminateWithError(ctx, ac.terminator, instanceID, runID, "registration_failed", regErr)
		return
	}
	logger.Println("Runner registered successfully")

	// Set runner environment variables
	cacheURL := os.Getenv("RUNS_FLEET_CACHE_URL")
	if envErr := registrar.SetRunnerEnvironment(runnerPath, cacheURL, ac.runnerConfig.CacheToken); envErr != nil {
		logger.Printf("Warning: failed to set runner environment: %v", envErr)
	}

	jobStartedAt := time.Now()

	if ac.telemetry != nil {
		jobStatus := agent.JobStatus{
			InstanceID: instanceID,
			JobID:      runID,
			StartedAt:  jobStartedAt,
		}
		if sendErr := ac.telemetry.SendJobStarted(ctx, jobStatus); sendErr != nil {
			logger.Printf("Warning: failed to send job started notification: %v", sendErr)
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

	if cleanErr := cleanup.CleanupRunner(ctx, runnerPath); cleanErr != nil {
		logger.Printf("Warning: cleanup failed: %v", cleanErr)
	}

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

	if termErr := ac.terminator.TerminateInstance(ctx, instanceID, jobStatus); termErr != nil {
		logger.Printf("Failed to terminate instance: %v", termErr)
	}

	logger.Println("Agent completed successfully")
}

// terminateWithError terminates the instance after an error.
func terminateWithError(ctx context.Context, terminator agent.InstanceTerminator, instanceID, runID, errorType string, err error) {
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
