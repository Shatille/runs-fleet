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
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
	"github.com/Shavakan/runs-fleet/pkg/termination"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/google/go-github/v57/github"
)

const (
	maxDeleteRetries      = 3
	retryDelay            = 1 * time.Second
	maxFleetCreateRetries = 3
	fleetRetryBaseDelay   = 2 * time.Second
)

type stdLogger struct{}

func (l *stdLogger) Printf(format string, v ...interface{}) {
	log.Printf(format, v...)
}

func (l *stdLogger) Println(v ...interface{}) {
	log.Println(v...)
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

	sqsClient := queue.NewClient(awsCfg, cfg.QueueURL)
	eventsQueueClient := queue.NewClient(awsCfg, cfg.EventsQueueURL)
	fleetManager := fleet.NewManager(awsCfg, cfg)
	dbClient := db.NewClient(awsCfg, cfg.PoolsTableName, cfg.JobsTableName)
	poolManager := pools.NewManager(dbClient, fleetManager, cfg)
	cacheServer := cache.NewServer(awsCfg, cfg.CacheBucketName)
	metricsPublisher := metrics.NewPublisher(awsCfg)
	eventHandler := events.NewHandler(eventsQueueClient, dbClient, metricsPublisher, cfg)

	var coord coordinator.Coordinator
	logger := &stdLogger{}
	if cfg.CoordinatorEnabled && cfg.InstanceID != "" && cfg.LocksTableName != "" {
		dynamoClient := dynamodb.NewFromConfig(awsCfg)
		coordCfg := coordinator.DefaultConfig(cfg.InstanceID)
		coordCfg.LockTableName = cfg.LocksTableName
		coord = coordinator.NewDynamoDBCoordinator(coordCfg, dynamoClient, logger)
		log.Printf("Distributed coordinator enabled: instance_id=%s, table=%s", cfg.InstanceID, cfg.LocksTableName)
	} else {
		coord = coordinator.NewNoOpCoordinator(logger)
		log.Println("Distributed coordinator disabled (no-op coordinator)")
	}

	if err := coord.Start(ctx); err != nil {
		log.Fatalf("Failed to start coordinator: %v", err)
	}

	poolManager.SetCoordinator(coord)

	ec2Client := ec2.NewFromConfig(awsCfg)
	poolManager.SetEC2Client(ec2Client)
	log.Println("Pool manager initialized with EC2 client for reconciliation")

	var circuitBreaker *circuit.Breaker
	if cfg.CircuitBreakerTable != "" {
		circuitBreaker = circuit.NewBreaker(awsCfg, cfg.CircuitBreakerTable)
		circuitBreaker.StartCacheCleanup(ctx)
		fleetManager.SetCircuitBreaker(circuitBreaker)
		eventHandler.SetCircuitBreaker(circuitBreaker)
		log.Printf("Circuit breaker initialized with table: %s", cfg.CircuitBreakerTable)
	}

	var terminationHandler *termination.Handler
	if cfg.TerminationQueueURL != "" {
		terminationQueueClient := queue.NewClient(awsCfg, cfg.TerminationQueueURL)
		ssmClient := ssm.NewFromConfig(awsCfg)
		terminationHandler = termination.NewHandler(terminationQueueClient, dbClient, metricsPublisher, ssmClient, cfg)
	}

	var housekeepingHandler *housekeeping.Handler
	if cfg.HousekeepingQueueURL != "" {
		housekeepingQueueClient := queue.NewClient(awsCfg, cfg.HousekeepingQueueURL)

		var costReporter *cost.Reporter
		if cfg.CostReportSNSTopic != "" || cfg.CostReportBucket != "" {
			costReporter = cost.NewReporter(awsCfg, cfg, cfg.CostReportSNSTopic, cfg.CostReportBucket)
		}

		housekeepingMetrics := &housekeepingMetricsAdapter{publisher: metricsPublisher}
		tasksExecutor := housekeeping.NewTasks(awsCfg, cfg, housekeepingMetrics, costReporter)
		housekeepingHandler = housekeeping.NewHandler(housekeepingQueueClient, tasksExecutor, cfg)
		log.Printf("Housekeeping handler initialized with queue: %s", cfg.HousekeepingQueueURL)
	}

	// Initialize runner manager for SSM configuration
	var runnerManager *runner.Manager
	if cfg.GitHubAppID != "" && cfg.GitHubAppPrivateKey != "" {
		githubClient, err := runner.NewGitHubClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKey)
		if err != nil {
			log.Fatalf("Failed to create GitHub client: %v", err)
		}
		runnerManager = runner.NewManager(awsCfg, githubClient, runner.ManagerConfig{
			CacheSecret: cfg.CacheSecret,
			CacheURL:    cfg.CacheURL,
		})
		log.Println("Runner manager initialized for SSM configuration")
	} else {
		log.Println("WARNING: Runner manager not initialized - GitHub App credentials not configured")
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "OK\n")
	})

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
		switch event := payload.(type) {
		case *github.WorkflowJobEvent:
			if event.GetAction() == "queued" {
				if err := handleWorkflowJob(r.Context(), event, sqsClient, metricsPublisher); err == nil {
					processed = true
				}
			}
		}

		w.WriteHeader(http.StatusOK)
		if processed {
			_, _ = fmt.Fprintf(w, "Job queued\n")
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
	go runWorker(ctx, sqsClient, fleetManager, poolManager, metricsPublisher, runnerManager, cfg, &subnetIndex)

	go poolManager.ReconcileLoop(ctx)

	go eventHandler.Run(ctx)

	if terminationHandler != nil {
		go terminationHandler.Run(ctx)
	}

	if housekeepingHandler != nil {
		go housekeepingHandler.Run(ctx)
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

	log.Println("Server stopped")
}

func handleWorkflowJob(ctx context.Context, event *github.WorkflowJobEvent, q *queue.Client, m *metrics.Publisher) error {
	log.Printf("Received workflow_job queued: %s", event.GetWorkflowJob().GetName())

	jobConfig, err := gh.ParseLabels(event.GetWorkflowJob().Labels)
	if err != nil {
		log.Printf("Skipping job (no runs-fleet labels): %v", err)
		return nil
	}

	msg := &queue.JobMessage{
		JobID:        fmt.Sprintf("%d", event.GetWorkflowJob().GetID()),
		RunID:        jobConfig.RunID,
		Repo:         event.GetRepo().GetFullName(), // owner/repo for repo-level registration
		InstanceType: jobConfig.InstanceType,
		Pool:         jobConfig.Pool,
		Private:      jobConfig.Private,
		Spot:         jobConfig.Spot,
		RunnerSpec:   jobConfig.RunnerSpec,
		// Sprint 4 features
		Region:      jobConfig.Region,      // Phase 3: Multi-region support
		Environment: jobConfig.Environment, // Phase 6: Per-stack environments
		OS:          jobConfig.OS,          // Phase 4: Windows support
		Arch:        jobConfig.Arch,        // Phase 4: Architecture support
		// Flexible instance selection (Phase 10)
		InstanceTypes: jobConfig.InstanceTypes,
	}

	if err := q.SendMessage(ctx, msg); err != nil {
		log.Printf("Failed to enqueue job: %v", err)
		return fmt.Errorf("failed to enqueue job: %w", err)
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

func runWorker(ctx context.Context, q *queue.Client, f *fleet.Manager, pm *pools.Manager, m *metrics.Publisher, rm *runner.Manager, cfg *config.Config, subnetIndex *uint64) {
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
					processMessage(processCtx, q, f, pm, m, rm, msg, cfg, subnetIndex)
				}()
			}
		}
	}
}

func deleteMessageWithRetry(ctx context.Context, q *queue.Client, receiptHandle string) error {
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

func processMessage(ctx context.Context, q *queue.Client, f *fleet.Manager, pm *pools.Manager, m *metrics.Publisher, rm *runner.Manager, msg types.Message, cfg *config.Config, subnetIndex *uint64) {
	startTime := time.Now()
	fleetCreated := false
	poisonMessage := false

	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
		defer cleanupCancel()

		if fleetCreated {
			if err := deleteMessageWithRetry(cleanupCtx, q, *msg.ReceiptHandle); err != nil {
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
	if err := json.Unmarshal([]byte(*msg.Body), &job); err != nil {
		log.Printf("Failed to unmarshal message: %v", err)
		poisonMessage = true

		if err := deleteMessageWithRetry(ctx, q, *msg.ReceiptHandle); err != nil {
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
	}

	instanceIDs, err := createFleetWithRetry(ctx, f, spec)
	if err != nil {
		log.Printf("Failed to create fleet: %v", err)
		return
	}

	fleetCreated = true

	// Prepare runner config in SSM for each instance
	if rm != nil {
		for _, instanceID := range instanceIDs {
			prepareReq := runner.PrepareRunnerRequest{
				InstanceID: instanceID,
				JobID:      job.JobID,
				RunID:      job.RunID,
				Repo:       job.Repo,
				Labels:     []string{fmt.Sprintf("runs-fleet=%s", job.RunID), fmt.Sprintf("runner=%s", job.RunnerSpec)},
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

// housekeepingMetricsAdapter adapts metrics.Publisher to housekeeping.MetricsAPI.
type housekeepingMetricsAdapter struct {
	publisher *metrics.Publisher
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
