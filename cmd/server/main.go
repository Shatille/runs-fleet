// Package main implements the runs-fleet orchestrator server that processes GitHub webhook events.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cache"
	"github.com/Shavakan/runs-fleet/pkg/circuit"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/coordinator"
	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	gh "github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/housekeeping"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	"github.com/Shavakan/runs-fleet/pkg/provider/k8s"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
	"github.com/Shavakan/runs-fleet/pkg/termination"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/google/go-github/v57/github"
)

const (
	maxDeleteRetries      = 3
	maxFleetCreateRetries = 3
	maxJobRetries         = 2 // Max re-queue attempts for failed jobs (total attempts = maxJobRetries + 1)

	runnerNamePrefix = "runs-fleet-" // Prefix for runner names to extract instance ID
)

// Retry delays - variables to allow testing with shorter durations.
var (
	retryDelay          = 1 * time.Second
	fleetRetryBaseDelay = 2 * time.Second
)

type stdLogger struct{}

func (l *stdLogger) Printf(format string, v ...interface{}) {
	log.Printf(format, v...)
}

func (l *stdLogger) Println(v ...interface{}) {
	log.Println(v...)
}

func initCoordinator(_ context.Context, awsCfg aws.Config, cfg *config.Config, logger coordinator.Logger) coordinator.Coordinator {
	if cfg.CoordinatorEnabled && cfg.InstanceID != "" && cfg.LocksTableName != "" {
		dynamoClient := dynamodb.NewFromConfig(awsCfg)
		coordCfg := coordinator.DefaultConfig(cfg.InstanceID)
		coordCfg.LockTableName = cfg.LocksTableName
		coord := coordinator.NewDynamoDBCoordinator(coordCfg, dynamoClient, logger)
		log.Printf("Distributed coordinator enabled: instance_id=%s, table=%s", cfg.InstanceID, cfg.LocksTableName)
		return coord
	}
	log.Println("Distributed coordinator disabled (no-op coordinator)")
	return coordinator.NewNoOpCoordinator(logger)
}

func initJobQueue(awsCfg aws.Config, cfg *config.Config) queue.Queue {
	if cfg.IsK8sBackend() {
		valkeyClient, err := queue.NewValkeyClient(queue.ValkeyConfig{
			Addr:       cfg.ValkeyAddr,
			Password:   cfg.ValkeyPassword,
			DB:         cfg.ValkeyDB,
			Stream:     "runs-fleet:jobs",
			Group:      "orchestrator",
			ConsumerID: cfg.InstanceID,
		})
		if err != nil {
			log.Fatalf("Failed to create Valkey client: %v", err)
		}
		log.Printf("Using Valkey queue at %s", cfg.ValkeyAddr)
		return valkeyClient
	}
	log.Printf("Using SQS queue at %s", cfg.QueueURL)
	return queue.NewSQSClient(awsCfg, cfg.QueueURL)
}

func initRunnerManager(awsCfg aws.Config, cfg *config.Config) *runner.Manager {
	if cfg.GitHubAppID == "" || cfg.GitHubAppPrivateKey == "" {
		log.Println("WARNING: Runner manager not initialized - GitHub App credentials not configured")
		return nil
	}
	githubClient, err := runner.NewGitHubClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKey)
	if err != nil {
		log.Fatalf("Failed to create GitHub client: %v", err)
	}
	rm := runner.NewManager(awsCfg, githubClient, runner.ManagerConfig{
		CacheSecret:         cfg.CacheSecret,
		CacheURL:            cfg.CacheURL,
		TerminationQueueURL: cfg.TerminationQueueURL,
	})
	log.Println("Runner manager initialized for SSM configuration")
	return rm
}

func initCircuitBreaker(ctx context.Context, awsCfg aws.Config, cfg *config.Config, fm *fleet.Manager, eh *events.Handler) *circuit.Breaker {
	if cfg.CircuitBreakerTable == "" || fm == nil {
		return nil
	}
	cb := circuit.NewBreaker(awsCfg, cfg.CircuitBreakerTable)
	cb.StartCacheCleanup(ctx)
	fm.SetCircuitBreaker(cb)
	if eh != nil {
		eh.SetCircuitBreaker(cb)
	}
	log.Printf("Circuit breaker initialized with table: %s", cfg.CircuitBreakerTable)
	return cb
}

func initHousekeeping(awsCfg aws.Config, cfg *config.Config, metricsPublisher metrics.Publisher) (*housekeeping.Handler, *housekeeping.Scheduler) {
	if cfg.HousekeepingQueueURL == "" {
		return nil, nil
	}

	housekeepingQueueClient := queue.NewClient(awsCfg, cfg.HousekeepingQueueURL)

	var costReporter *cost.Reporter
	if cfg.CostReportSNSTopic != "" || cfg.CostReportBucket != "" {
		costReporter = cost.NewReporter(awsCfg, cfg, cfg.CostReportSNSTopic, cfg.CostReportBucket)
	}

	housekeepingMetrics := &housekeepingMetricsAdapter{publisher: metricsPublisher}
	tasksExecutor := housekeeping.NewTasks(awsCfg, cfg, housekeepingMetrics, costReporter)
	handler := housekeeping.NewHandler(housekeepingQueueClient, tasksExecutor, cfg)

	schedulerCfg := housekeeping.DefaultSchedulerConfig()
	scheduler := housekeeping.NewSchedulerFromConfig(awsCfg, cfg.HousekeepingQueueURL, schedulerCfg)
	scheduler.SetMetrics(metricsPublisher)

	log.Printf("Housekeeping handler and scheduler initialized with queue: %s", cfg.HousekeepingQueueURL)
	return handler, scheduler
}

func initMetrics(awsCfg aws.Config, cfg *config.Config) (metrics.Publisher, http.Handler) {
	var publishers []metrics.Publisher
	var prometheusHandler http.Handler

	if cfg.MetricsCloudWatchEnabled {
		namespace := cfg.MetricsNamespace
		if namespace == "" {
			namespace = "RunsFleet"
		}
		publishers = append(publishers, metrics.NewCloudWatchPublisherWithNamespace(awsCfg, namespace))
		log.Println("CloudWatch metrics enabled")
	}

	if cfg.MetricsPrometheusEnabled {
		namespace := cfg.MetricsNamespace
		if namespace == "" {
			namespace = "runs_fleet"
		}
		prom := metrics.NewPrometheusPublisher(metrics.PrometheusConfig{Namespace: namespace})
		publishers = append(publishers, prom)
		prometheusHandler = prom.Handler()
		log.Println("Prometheus metrics enabled")
	}

	if cfg.MetricsDatadogEnabled {
		namespace := cfg.MetricsNamespace
		if namespace == "" {
			namespace = "runs_fleet"
		}
		dd, err := metrics.NewDatadogPublisher(metrics.DatadogConfig{
			Address:   cfg.MetricsDatadogAddr,
			Namespace: namespace,
			Tags:      cfg.MetricsDatadogTags,
		})
		if err != nil {
			log.Printf("WARNING: Failed to create Datadog metrics publisher: %v (continuing without Datadog)", err)
		} else {
			publishers = append(publishers, dd)
			log.Printf("Datadog metrics enabled (addr: %s)", cfg.MetricsDatadogAddr)
		}
	}

	if len(publishers) == 0 {
		log.Println("No metrics backends enabled")
		return metrics.NoopPublisher{}, nil
	}

	if len(publishers) == 1 {
		return publishers[0], prometheusHandler
	}

	return metrics.NewMultiPublisher(publishers...), prometheusHandler
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Println("Starting runs-fleet server...")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	// Initialize queue based on backend mode
	jobQueue := initJobQueue(awsCfg, cfg)

	dbClient := db.NewClient(awsCfg, cfg.PoolsTableName, cfg.JobsTableName)
	cacheServer := cache.NewServer(awsCfg, cfg.CacheBucketName)

	metricsPublisher, prometheusHandler := initMetrics(awsCfg, cfg)

	// Backend-specific initialization
	var fleetManager *fleet.Manager
	var poolManager *pools.Manager
	var eventHandler *events.Handler
	var k8sProvider *k8s.Provider
	var k8sPoolProvider *k8s.PoolProvider

	if cfg.IsK8sBackend() {
		log.Printf("Initializing K8s backend (namespace: %s)", cfg.KubeNamespace)
		var err error
		k8sProvider, err = k8s.NewProvider(cfg)
		if err != nil {
			log.Fatalf("Failed to create K8s provider: %v", err)
		}
		k8sPoolProvider = k8s.NewPoolProvider(k8sProvider.Clientset(), cfg)
		log.Println("K8s provider and pool provider initialized")
	} else {
		log.Println("Initializing EC2 backend")
		eventsQueueClient := queue.NewClient(awsCfg, cfg.EventsQueueURL)
		fleetManager = fleet.NewManager(awsCfg, cfg)
		poolManager = pools.NewManager(dbClient, fleetManager, cfg)
		eventHandler = events.NewHandler(eventsQueueClient, dbClient, metricsPublisher, cfg)
	}

	logger := &stdLogger{}
	coord := initCoordinator(ctx, awsCfg, cfg, logger)
	if err := coord.Start(ctx); err != nil {
		log.Fatalf("Failed to start coordinator: %v", err)
	}

	// Backend-specific coordinator setup
	if poolManager != nil {
		poolManager.SetCoordinator(coord)
		ec2Client := ec2.NewFromConfig(awsCfg)
		poolManager.SetEC2Client(ec2Client)
		log.Println("Pool manager initialized with EC2 client for reconciliation")
	}
	if k8sPoolProvider != nil {
		k8sPoolProvider.SetCoordinator(coord)
	}

	initCircuitBreaker(ctx, awsCfg, cfg, fleetManager, eventHandler)

	// EC2-specific handlers (SSM-based termination and EC2 housekeeping)
	var terminationHandler *termination.Handler
	var housekeepingHandler *housekeeping.Handler
	var housekeepingScheduler *housekeeping.Scheduler
	if cfg.IsEC2Backend() {
		if cfg.TerminationQueueURL != "" {
			terminationQueueClient := queue.NewClient(awsCfg, cfg.TerminationQueueURL)
			ssmClient := ssm.NewFromConfig(awsCfg)
			terminationHandler = termination.NewHandler(terminationQueueClient, dbClient, metricsPublisher, ssmClient, cfg)
		}
		housekeepingHandler, housekeepingScheduler = initHousekeeping(awsCfg, cfg, metricsPublisher)
	}

	runnerManager := initRunnerManager(awsCfg, cfg)

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "OK\n")
	})

	mux.HandleFunc("/ready", makeReadinessHandler(jobQueue))

	if prometheusHandler != nil {
		mux.Handle(cfg.MetricsPrometheusPath, prometheusHandler)
		log.Printf("Prometheus metrics enabled at %s", cfg.MetricsPrometheusPath)
	}

	cacheHandler := cache.NewHandlerWithAuth(cacheServer, metricsPublisher, cfg.CacheSecret)
	cacheHandler.RegisterRoutes(mux)

	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		payload, err := gh.ParseWebhook(r, cfg.GitHubWebhookSecret)
		if err != nil {
			log.Printf("Webhook parsing failed: %v", err)
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		processed := false
		var response string
		switch event := payload.(type) {
		case *github.WorkflowJobEvent:
			switch event.GetAction() {
			case "queued":
				if err := handleWorkflowJob(r.Context(), event, jobQueue, metricsPublisher); err == nil {
					processed = true
					response = "Job queued"
				}
			case "completed":
				if event.GetWorkflowJob().GetConclusion() == "failure" {
					requeued, err := handleJobFailure(r.Context(), event, jobQueue, dbClient, metricsPublisher)
					if err != nil {
						log.Printf("Failed to handle job failure: %v", err)
					}
					processed = requeued
					if requeued {
						response = "Job requeued"
					}
				}
			}
		}

		w.WriteHeader(http.StatusOK)
		if processed {
			_, _ = fmt.Fprintf(w, "%s\n", response)
		} else {
			_, _ = fmt.Fprintf(w, "Event acknowledged\n")
		}
	})

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	var subnetIndex uint64
	if cfg.IsK8sBackend() {
		go runK8sWorker(ctx, jobQueue, k8sProvider, k8sPoolProvider, metricsPublisher, runnerManager, dbClient, cfg)
		go k8sPoolProvider.ReconcileLoop(ctx)
	} else {
		go runWorker(ctx, jobQueue, fleetManager, poolManager, metricsPublisher, runnerManager, dbClient, cfg, &subnetIndex)
		go poolManager.ReconcileLoop(ctx)
		go eventHandler.Run(ctx)
	}

	if terminationHandler != nil {
		go terminationHandler.Run(ctx)
	}

	if housekeepingHandler != nil {
		go housekeepingHandler.Run(ctx)
	}

	if housekeepingScheduler != nil {
		go housekeepingScheduler.Run(ctx)
	}

	go func() {
		log.Printf("Server listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutdown signal received, gracefully stopping...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), config.MessageProcessTimeout)
	defer shutdownCancel()

	if err := coord.Stop(shutdownCtx); err != nil {
		log.Printf("Coordinator shutdown failed: %v", err)
	}

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}

	if err := metricsPublisher.Close(); err != nil {
		log.Printf("Metrics publisher close failed: %v", err)
	}

	log.Println("Server stopped")
}

func handleWorkflowJob(ctx context.Context, event *github.WorkflowJobEvent, q queue.Queue, m metrics.Publisher) error {
	log.Printf("Received workflow_job queued: %s", event.GetWorkflowJob().GetName())

	jobConfig, err := gh.ParseLabels(event.GetWorkflowJob().Labels)
	if err != nil {
		log.Printf("Skipping job (no runs-fleet labels): %v", err)
		return nil
	}

	msg := &queue.JobMessage{
		JobID:         fmt.Sprintf("%d", event.GetWorkflowJob().GetID()),
		RunID:         jobConfig.RunID,
		Repo:          event.GetRepo().GetFullName(), // owner/repo for repo-level registration
		InstanceType:  jobConfig.InstanceType,
		Pool:          jobConfig.Pool,
		Private:       jobConfig.Private,
		Spot:          jobConfig.Spot,
		RunnerSpec:    jobConfig.RunnerSpec,
		OriginalLabel: jobConfig.OriginalLabel,
		// Sprint 4 features
		Region:      jobConfig.Region,      // Phase 3: Multi-region support
		Environment: jobConfig.Environment, // Phase 6: Per-stack environments
		OS:          jobConfig.OS,          // Phase 4: Windows support
		Arch:        jobConfig.Arch,        // Phase 4: Architecture support
		// Flexible instance selection (Phase 10)
		InstanceTypes: jobConfig.InstanceTypes,
		// Storage configuration
		StorageGiB: jobConfig.StorageGiB,
	}

	if err := q.SendMessage(ctx, msg); err != nil {
		log.Printf("Failed to enqueue job: %v", err)
		return fmt.Errorf("failed to enqueue job: %w", err)
	}

	if err := m.PublishJobQueued(ctx); err != nil {
		log.Printf("Failed to publish job queued metric: %v", err)
	}
	if err := m.PublishQueueDepth(ctx, 1); err != nil {
		log.Printf("Failed to publish queue depth metric: %v", err)
	}
	if len(jobConfig.InstanceTypes) > 1 {
		log.Printf("Enqueued job for run %s (os=%s, arch=%s, region=%s, env=%s, instanceTypes=%d)",
			jobConfig.RunID, jobConfig.OS, jobConfig.Arch, jobConfig.Region, jobConfig.Environment, len(jobConfig.InstanceTypes))
	} else {
		log.Printf("Enqueued job for run %s (os=%s, arch=%s, region=%s, env=%s)",
			jobConfig.RunID, jobConfig.OS, jobConfig.Arch, jobConfig.Region, jobConfig.Environment)
	}
	return nil
}

// handleJobFailure processes workflow_job completed events with failure conclusion.
// If the job failed due to runner death (not a normal job failure), it re-queues the job.
// Returns (true, nil) if job was re-queued, (false, nil) if skipped, or (false, error) on error.
func handleJobFailure(ctx context.Context, event *github.WorkflowJobEvent, q queue.Queue, dbc *db.Client, m metrics.Publisher) (bool, error) {
	job := event.GetWorkflowJob()
	runnerName := job.GetRunnerName()

	// Skip if no runner was assigned (job failed before runner picked it up)
	if runnerName == "" {
		log.Printf("Job %d failed with no runner assigned, skipping re-queue", job.GetID())
		return false, nil
	}

	// Extract instance ID from runner name (format: runs-fleet-{instance-id})
	if !strings.HasPrefix(runnerName, runnerNamePrefix) {
		log.Printf("Job %d failed on non-runs-fleet runner %q, skipping", job.GetID(), runnerName)
		return false, nil
	}
	instanceID := strings.TrimPrefix(runnerName, runnerNamePrefix)

	// Check if this is a runs-fleet managed job by parsing labels
	_, err := gh.ParseLabels(job.Labels)
	if err != nil {
		log.Printf("Job %d not a runs-fleet job, skipping: %v", job.GetID(), err)
		return false, nil
	}

	// Skip if jobs table not configured
	if !dbc.HasJobsTable() {
		log.Printf("Jobs table not configured, cannot check job state for instance %s", instanceID)
		return false, nil
	}

	// Look up job in DynamoDB
	jobInfo, err := dbc.GetJobByInstance(ctx, instanceID)
	if err != nil {
		return false, fmt.Errorf("failed to get job for instance %s: %w", instanceID, err)
	}

	// If no job record found, it was already cleaned up or never tracked
	if jobInfo == nil {
		log.Printf("No job record for instance %s, skipping re-queue", instanceID)
		return false, nil
	}

	// Check retry limit
	if jobInfo.RetryCount >= maxJobRetries {
		log.Printf("Job %s exceeded max retries (%d), not re-queuing", jobInfo.JobID, maxJobRetries)
		return false, nil
	}

	// Validate required fields before re-queuing
	if jobInfo.RunID == "" || jobInfo.Repo == "" {
		log.Printf("Invalid job data for instance %s (RunID=%q, Repo=%q), skipping re-queue",
			instanceID, jobInfo.RunID, jobInfo.Repo)
		return false, nil
	}

	// Atomically mark job as requeued (only succeeds if status is "running")
	// This prevents duplicate re-queuing from concurrent webhooks or EventBridge
	marked, err := dbc.MarkJobRequeued(ctx, instanceID)
	if err != nil {
		return false, fmt.Errorf("failed to mark job requeued: %w", err)
	}
	if !marked {
		// Job was already in terminating/requeued state - EventBridge or another handler got it first
		log.Printf("Job %s for instance %s already handled (not in running state), skipping", jobInfo.JobID, instanceID)
		return false, nil
	}

	// Re-queue with ForceOnDemand to avoid another spot interruption
	// Note: Some fields (OriginalLabel, Region, etc.) are not stored in JobRecord.
	// Re-queued jobs use basic config. Full field storage is a future enhancement.
	requeueMsg := &queue.JobMessage{
		JobID:         jobInfo.JobID,
		RunID:         jobInfo.RunID,
		Repo:          jobInfo.Repo,
		InstanceType:  jobInfo.InstanceType,
		Pool:          jobInfo.Pool,
		Private:       jobInfo.Private,
		Spot:          false, // ForceOnDemand uses on-demand instances
		RunnerSpec:    jobInfo.RunnerSpec,
		RetryCount:    jobInfo.RetryCount + 1,
		ForceOnDemand: true,
	}

	if err := q.SendMessage(ctx, requeueMsg); err != nil {
		return false, fmt.Errorf("failed to re-queue job %s: %w", jobInfo.JobID, err)
	}

	log.Printf("Re-queued failed job %s from instance %s (RetryCount=%d, ForceOnDemand=true)",
		jobInfo.JobID, instanceID, requeueMsg.RetryCount)

	// Publish metric for job retry
	if err := m.PublishJobQueued(ctx); err != nil {
		log.Printf("Failed to publish job queued metric: %v", err)
	}

	return true, nil
}

func runWorker(ctx context.Context, q queue.Queue, f *fleet.Manager, pm *pools.Manager, m metrics.Publisher, rm *runner.Manager, dbc *db.Client, cfg *config.Config, subnetIndex *uint64) {
	log.Println("Starting worker loop...")
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)
	var activeWork sync.WaitGroup

	defer func() {
		log.Println("Waiting for in-flight work to complete...")
		activeWork.Wait()
		log.Println("Worker shutdown complete")
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			timeout := 25 * time.Second
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline)
				if remaining < timeout {
					timeout = remaining
				}
			}
			recvCtx, cancel := context.WithTimeout(ctx, timeout)
			messages, err := q.ReceiveMessages(recvCtx, 10, 20)
			cancel()
			if err != nil {
				log.Printf("Failed to receive messages: %v", err)
				continue
			}

			if len(messages) == 0 {
				continue
			}

			for _, msg := range messages {
				msg := msg
				activeWork.Add(1)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("panic in processMessage: %v", r)
						}
					}()
					defer activeWork.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					processCtx, processCancel := context.WithTimeout(ctx, config.MessageProcessTimeout)
					defer processCancel()
					processMessage(processCtx, q, f, pm, m, rm, dbc, msg, cfg, subnetIndex)
				}()
			}
		}
	}
}

func deleteMessageWithRetry(ctx context.Context, q queue.Queue, receiptHandle string) error {
	err := q.DeleteMessage(ctx, receiptHandle)
	for attempts := 1; attempts < maxDeleteRetries && err != nil; attempts++ {
		time.Sleep(retryDelay)
		err = q.DeleteMessage(ctx, receiptHandle)
	}
	return err
}

func selectSubnet(job *queue.JobMessage, cfg *config.Config, subnetIndex *uint64) string {
	if job.Private && len(cfg.PrivateSubnetIDs) > 0 {
		idx := atomic.AddUint64(subnetIndex, 1) - 1
		return cfg.PrivateSubnetIDs[idx%uint64(len(cfg.PrivateSubnetIDs))]
	}
	if len(cfg.PublicSubnetIDs) > 0 {
		idx := atomic.AddUint64(subnetIndex, 1) - 1
		return cfg.PublicSubnetIDs[idx%uint64(len(cfg.PublicSubnetIDs))]
	}
	return ""
}

func createFleetWithRetry(ctx context.Context, f *fleet.Manager, spec *fleet.LaunchSpec) ([]string, error) {
	var instanceIDs []string
	var err error
	for attempt := 0; attempt < maxFleetCreateRetries; attempt++ {
		if attempt > 0 {
			backoff := fleetRetryBaseDelay * time.Duration(1<<uint(attempt-1))
			log.Printf("Retrying fleet creation (attempt %d/%d) after %v", attempt+1, maxFleetCreateRetries, backoff)
			time.Sleep(backoff)
		}

		instanceIDs, err = f.CreateFleet(ctx, spec)
		if err == nil {
			return instanceIDs, nil
		}

		log.Printf("Fleet creation attempt %d/%d failed: %v", attempt+1, maxFleetCreateRetries, err)
	}
	return nil, fmt.Errorf("failed after %d attempts: %w", maxFleetCreateRetries, err)
}

func createK8sRunnerWithRetry(ctx context.Context, p *k8s.Provider, spec *provider.RunnerSpec) (*provider.RunnerResult, error) {
	var result *provider.RunnerResult
	var err error
	for attempt := 0; attempt < maxFleetCreateRetries; attempt++ {
		if attempt > 0 {
			backoff := fleetRetryBaseDelay * time.Duration(1<<uint(attempt-1))
			log.Printf("Retrying K8s pod creation (attempt %d/%d) after %v", attempt+1, maxFleetCreateRetries, backoff)
			time.Sleep(backoff)
		}

		result, err = p.CreateRunner(ctx, spec)
		if err == nil {
			return result, nil
		}

		log.Printf("K8s pod creation attempt %d/%d failed: %v", attempt+1, maxFleetCreateRetries, err)
	}
	return nil, fmt.Errorf("failed after %d attempts: %w", maxFleetCreateRetries, err)
}

func processMessage(ctx context.Context, q queue.Queue, f *fleet.Manager, pm *pools.Manager, m metrics.Publisher, rm *runner.Manager, dbc *db.Client, msg queue.Message, cfg *config.Config, subnetIndex *uint64) {
	startTime := time.Now()
	fleetCreated := false
	poisonMessage := false

	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
		defer cleanupCancel()

		if fleetCreated {
			if err := deleteMessageWithRetry(cleanupCtx, q, msg.Handle); err != nil {
				log.Printf("Failed to delete message after %d attempts: %v", maxDeleteRetries, err)
			}

			if err := m.PublishQueueDepth(cleanupCtx, -1); err != nil {
				log.Printf("Failed to publish queue depth metric: %v", err)
			}

			if metricErr := m.PublishFleetSizeIncrement(cleanupCtx); metricErr != nil {
				log.Printf("Failed to publish fleet size increment metric: %v", metricErr)
			}
		}

		if poisonMessage {
			if err := m.PublishQueueDepth(cleanupCtx, -1); err != nil {
				log.Printf("Failed to publish queue depth metric: %v", err)
			}
		}

		if err := m.PublishJobDuration(cleanupCtx, int(time.Since(startTime).Seconds())); err != nil {
			log.Printf("Failed to publish job duration metric: %v", err)
		}
	}()

	var job queue.JobMessage
	if err := json.Unmarshal([]byte(msg.Body), &job); err != nil {
		log.Printf("Failed to unmarshal message: %v", err)
		poisonMessage = true

		if err := deleteMessageWithRetry(ctx, q, msg.Handle); err != nil {
			log.Printf("Failed to delete poison message after %d attempts: %v", maxDeleteRetries, err)
		}

		metricCtx, metricCancel := context.WithTimeout(ctx, config.ShortTimeout)
		defer metricCancel()
		if metricErr := m.PublishMessageDeletionFailure(metricCtx); metricErr != nil {
			log.Printf("Failed to publish poison message metric: %v", metricErr)
		}
		return
	}

	log.Printf("Processing job for run %s (retry=%d, forceOnDemand=%v, os=%s, region=%s, env=%s)",
		job.RunID, job.RetryCount, job.ForceOnDemand, job.OS, job.Region, job.Environment)

	spec := &fleet.LaunchSpec{
		RunID:         job.RunID,
		InstanceType:  job.InstanceType,
		InstanceTypes: job.InstanceTypes, // Flexible instance selection (Phase 10)
		SubnetID:      selectSubnet(&job, cfg, subnetIndex),
		Spot:          job.Spot,
		Pool:          job.Pool,
		ForceOnDemand: job.ForceOnDemand,
		RetryCount:    job.RetryCount,
		// Sprint 4 features
		Region:      job.Region,      // Phase 3: Multi-region support
		Environment: job.Environment, // Phase 6: Per-stack environments
		OS:          job.OS,          // Phase 4: Windows support
		Arch:        job.Arch,        // Phase 4: Architecture support
		// Storage configuration
		StorageGiB: job.StorageGiB,
	}

	instanceIDs, err := createFleetWithRetry(ctx, f, spec)
	if err != nil {
		log.Printf("Failed to create fleet: %v", err)
		return
	}

	fleetCreated = true

	// Save job records in DynamoDB for spot interruption tracking (only if jobs table is configured)
	if dbc != nil && dbc.HasJobsTable() {
		for _, instanceID := range instanceIDs {
			jobRecord := &db.JobRecord{
				JobID:        job.JobID,
				RunID:        job.RunID,
				Repo:         job.Repo,
				InstanceID:   instanceID,
				InstanceType: job.InstanceType,
				Pool:         job.Pool,
				Private:      job.Private,
				Spot:         job.Spot,
				RunnerSpec:   job.RunnerSpec,
				RetryCount:   job.RetryCount,
			}
			// Retry SaveJob with exponential backoff (spot tracking is critical for job re-queueing)
			var saveErr error
			for attempt := 0; attempt < 3; attempt++ {
				if saveErr = dbc.SaveJob(ctx, jobRecord); saveErr == nil {
					break
				}
				if attempt < 2 {
					time.Sleep(time.Duration(1<<attempt) * 100 * time.Millisecond) // 100ms, 200ms
				}
			}
			if saveErr != nil {
				log.Printf("ERROR: Failed to save job record after retries for instance %s: %v (spot interruption tracking disabled)", instanceID, saveErr)
			}
		}
	}

	// Prepare runner config in SSM for each instance
	if rm != nil {
		for _, instanceID := range instanceIDs {
			label := buildRunnerLabel(&job)
			prepareReq := runner.PrepareRunnerRequest{
				InstanceID: instanceID,
				JobID:      job.JobID,
				RunID:      job.RunID,
				Repo:       job.Repo,
				Labels:     []string{label},
			}
			if err := rm.PrepareRunner(ctx, prepareReq); err != nil {
				log.Printf("Failed to prepare runner config for instance %s: %v", instanceID, err)
				// Continue anyway - the instance might still work if manually configured
			}
		}
	}

	// Mark instances as busy for idle timeout tracking
	for _, instanceID := range instanceIDs {
		pm.MarkInstanceBusy(instanceID)
	}

	log.Printf("Successfully launched %d instance(s) for run %s", len(instanceIDs), job.RunID)
}

// makeReadinessHandler returns a handler that checks queue connectivity.
// Pinger is optional: K8s/Valkey needs verification, EC2/SQS is AWS-managed.
func makeReadinessHandler(q queue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pinger, ok := q.(queue.Pinger); ok {
			pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := pinger.Ping(pingCtx); err != nil {
				log.Printf("Readiness check failed: %v", err)
				http.Error(w, "Queue not ready", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "OK\n")
	}
}

// buildRunnerLabel returns the runs-fleet label for GitHub runner registration.
// Uses the original label from the webhook to ensure exact matching with workflow runs-on.
func buildRunnerLabel(job *queue.JobMessage) string {
	if job.OriginalLabel != "" {
		return job.OriginalLabel
	}
	// Fallback for backwards compatibility with queued messages without OriginalLabel
	label := fmt.Sprintf("runs-fleet=%s/runner=%s", job.RunID, job.RunnerSpec)
	if job.Pool != "" {
		label += fmt.Sprintf("/pool=%s", job.Pool)
	}
	if job.Private {
		label += "/private=true"
	}
	if !job.Spot {
		label += "/spot=false"
	}
	return label
}

// housekeepingMetricsAdapter adapts metrics.Publisher to housekeeping.MetricsAPI.
type housekeepingMetricsAdapter struct {
	publisher metrics.Publisher
}

func (h *housekeepingMetricsAdapter) PublishOrphanedInstancesTerminated(ctx context.Context, count int) error {
	return h.publisher.PublishOrphanedInstancesTerminated(ctx, count)
}

func (h *housekeepingMetricsAdapter) PublishSSMParametersDeleted(ctx context.Context, count int) error {
	return h.publisher.PublishSSMParametersDeleted(ctx, count)
}

func (h *housekeepingMetricsAdapter) PublishJobRecordsArchived(ctx context.Context, count int) error {
	return h.publisher.PublishJobRecordsArchived(ctx, count)
}

func (h *housekeepingMetricsAdapter) PublishPoolUtilization(ctx context.Context, poolName string, utilization float64) error {
	return h.publisher.PublishPoolUtilization(ctx, poolName, utilization)
}

// K8s backend functions

func runK8sWorker(ctx context.Context, q queue.Queue, p *k8s.Provider, pp *k8s.PoolProvider, m metrics.Publisher, rm *runner.Manager, dbc *db.Client, cfg *config.Config) {
	log.Println("Starting K8s worker loop...")
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)
	var activeWork sync.WaitGroup

	defer func() {
		log.Println("Waiting for in-flight K8s work to complete...")
		activeWork.Wait()
		log.Println("K8s worker shutdown complete")
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			timeout := 25 * time.Second
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline)
				if remaining < timeout {
					timeout = remaining
				}
			}
			recvCtx, cancel := context.WithTimeout(ctx, timeout)
			messages, err := q.ReceiveMessages(recvCtx, 10, 20)
			cancel()
			if err != nil {
				log.Printf("Failed to receive messages: %v", err)
				continue
			}

			if len(messages) == 0 {
				continue
			}

			for _, msg := range messages {
				msg := msg
				activeWork.Add(1)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("panic in processK8sMessage: %v", r)
						}
					}()
					defer activeWork.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					processCtx, processCancel := context.WithTimeout(ctx, config.MessageProcessTimeout)
					defer processCancel()
					processK8sMessage(processCtx, q, p, pp, m, rm, dbc, msg, cfg)
				}()
			}
		}
	}
}

func processK8sMessage(ctx context.Context, q queue.Queue, p *k8s.Provider, pp *k8s.PoolProvider, m metrics.Publisher, rm *runner.Manager, dbc *db.Client, msg queue.Message, _ *config.Config) {
	startTime := time.Now()
	podCreated := false
	poisonMessage := false

	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
		defer cleanupCancel()

		if podCreated {
			if err := deleteMessageWithRetry(cleanupCtx, q, msg.Handle); err != nil {
				log.Printf("Failed to delete message after %d attempts: %v", maxDeleteRetries, err)
			}

			if err := m.PublishQueueDepth(cleanupCtx, -1); err != nil {
				log.Printf("Failed to publish queue depth metric: %v", err)
			}
		}

		if poisonMessage {
			if err := m.PublishQueueDepth(cleanupCtx, -1); err != nil {
				log.Printf("Failed to publish queue depth metric: %v", err)
			}
		}

		if err := m.PublishJobDuration(cleanupCtx, int(time.Since(startTime).Seconds())); err != nil {
			log.Printf("Failed to publish job duration metric: %v", err)
		}
	}()

	if msg.Body == "" {
		log.Printf("Received message with empty body")
		poisonMessage = true
		return
	}

	var job queue.JobMessage
	if err := json.Unmarshal([]byte(msg.Body), &job); err != nil {
		log.Printf("Failed to unmarshal message: %v", err)
		poisonMessage = true

		if err := deleteMessageWithRetry(ctx, q, msg.Handle); err != nil {
			log.Printf("Failed to delete poison message after %d attempts: %v", maxDeleteRetries, err)
		}

		metricCtx, metricCancel := context.WithTimeout(ctx, config.ShortTimeout)
		defer metricCancel()
		if metricErr := m.PublishMessageDeletionFailure(metricCtx); metricErr != nil {
			log.Printf("Failed to publish poison message metric: %v", metricErr)
		}
		return
	}

	log.Printf("Processing K8s job for run %s (os=%s, arch=%s, pool=%s)",
		job.RunID, job.OS, job.Arch, job.Pool)

	// Get registration token from GitHub before creating pod
	var jitToken string
	if rm == nil {
		log.Printf("Runner manager not initialized for K8s job %s", job.RunID)
		poisonMessage = true
		if metricErr := m.PublishSchedulingFailure(ctx, "k8s-runner-manager-missing"); metricErr != nil {
			log.Printf("Failed to publish runner manager missing metric: %v", metricErr)
		}
		return
	}
	regResult, err := rm.GetRegistrationToken(ctx, job.Repo)
	if err != nil {
		log.Printf("Failed to get registration token for K8s job %s: %v", job.RunID, err)
		poisonMessage = true
		if metricErr := m.PublishSchedulingFailure(ctx, "k8s-registration-token"); metricErr != nil {
			log.Printf("Failed to publish registration token failure metric: %v", metricErr)
		}
		return
	}
	jitToken = regResult.Token

	spec := &provider.RunnerSpec{
		RunID:        job.RunID,
		JobID:        job.JobID,
		Repo:         job.Repo,
		Labels:       []string{buildRunnerLabel(&job)},
		Pool:         job.Pool,
		OS:           job.OS,
		Arch:         job.Arch,
		InstanceType: job.InstanceType,
		Spot:         job.Spot,
		Private:      job.Private,
		Environment:  job.Environment,
		RetryCount:   job.RetryCount,
		StorageGiB:   job.StorageGiB,
		JITToken:     jitToken,
	}

	result, err := createK8sRunnerWithRetry(ctx, p, spec)
	if err != nil {
		log.Printf("Failed to create K8s runner: %v", err)
		if metricErr := m.PublishSchedulingFailure(ctx, "k8s-pod-creation"); metricErr != nil {
			log.Printf("Failed to publish pod creation failure metric: %v", metricErr)
		}
		return
	}

	podCreated = true

	// Save job records in DynamoDB with retry (only if jobs table is configured)
	if dbc != nil && dbc.HasJobsTable() {
		for _, runnerID := range result.RunnerIDs {
			jobRecord := &db.JobRecord{
				JobID:        job.JobID,
				RunID:        job.RunID,
				Repo:         job.Repo,
				InstanceID:   runnerID,
				InstanceType: job.InstanceType,
				Pool:         job.Pool,
				Private:      job.Private,
				Spot:         job.Spot,
				RunnerSpec:   job.RunnerSpec,
				RetryCount:   job.RetryCount,
			}
			var saveErr error
			for attempt := 0; attempt < 3; attempt++ {
				if saveErr = dbc.SaveJob(ctx, jobRecord); saveErr == nil {
					break
				}
				if attempt < 2 {
					time.Sleep(time.Duration(1<<attempt) * 100 * time.Millisecond)
				}
			}
			if saveErr != nil {
				log.Printf("ERROR: Failed to save job record after retries for pod %s: %v", runnerID, saveErr)
				if metricErr := m.PublishSchedulingFailure(ctx, "k8s-job-record-save"); metricErr != nil {
					log.Printf("Failed to publish job record save failure metric: %v", metricErr)
				}
			}
		}
	}

	// Mark pods as busy for idle timeout tracking
	for _, runnerID := range result.RunnerIDs {
		pp.MarkRunnerBusy(runnerID)
	}

	log.Printf("Successfully created %d K8s pod(s) for run %s", len(result.RunnerIDs), job.RunID)
}
