// Package main implements the runs-fleet orchestrator server that processes GitHub webhook events.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/Shavakan/runs-fleet/internal/awsobs"
	"github.com/Shavakan/runs-fleet/internal/handler"
	"github.com/Shavakan/runs-fleet/internal/worker"
	"github.com/Shavakan/runs-fleet/pkg/admin"
	"github.com/Shavakan/runs-fleet/pkg/cache"
	"github.com/Shavakan/runs-fleet/pkg/circuit"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	gh "github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/housekeeping"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
	"github.com/Shavakan/runs-fleet/pkg/termination"
	"github.com/Shavakan/runs-fleet/pkg/tracing"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/go-github/v57/github"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
)

var (
	serverLog  = logging.WithComponent(logging.LogTypeServer, "health")
	webhookLog = logging.WithComponent(logging.LogTypeWebhook, "main")
)

func main() {
	logging.Init()
	log := logging.WithComponent(logging.LogTypeServer, "main")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Info(ctx, "server starting")

	cfg, err := config.Load()
	if err != nil {
		log.Error(ctx, "config load failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	tp, err := tracing.Setup(ctx, cfg.Tracing)
	if err != nil {
		log.Error(ctx, "tracing setup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	otel.SetTracerProvider(tp)

	awsObsLog := logging.WithComponent(logging.LogTypeAWS, "observability")
	// SQS ReceiveMessage long-polls with WaitTimeSeconds=20, which exceeds
	// AWSPerOpTimeout (15s); bounding it would abort every empty poll, so it is
	// exempt from the per-operation timeout. Other SQS ops (SendMessage,
	// DeleteMessage) are fast and stay bounded.
	perOpTimeout := awsobs.PerOperationTimeout(config.AWSPerOpTimeout, map[string]bool{"ReceiveMessage": true})
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.AWSRegion),
		awsconfig.WithHTTPClient(newAWSHTTPClient(config.AWSResponseHeaderTimeout)),
		awsconfig.WithRetryer(awsRetryer),
		// Observability is registered first so it stays the outermost Initialize
		// middleware and records the full operation duration, including a
		// per-operation-timeout abort; the timeout middleware inserts itself just
		// inside it.
		awsconfig.WithAPIOptions(append(awsobs.Middlewares(awsObsLog), perOpTimeout)),
	)
	if err != nil {
		log.Error(ctx, "aws config load failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// SQS long polling withholds response headers for up to the 20s long-poll
	// wait, which exceeds awsCfg's 10s ResponseHeaderTimeout and aborts every
	// empty poll. SQS clients use a dedicated config whose header timeout clears
	// the long-poll wait; all other services keep the fast-abort awsCfg.
	sqsCfg := awsCfg
	sqsCfg.HTTPClient = newAWSHTTPClient(config.AWSSQSResponseHeaderTimeout)

	jobQueue := initJobQueue(sqsCfg, cfg)
	dbClient := db.NewClient(awsCfg, cfg.PoolsTableName, cfg.JobsTableName)
	if cfg.JobsPoolStatusGSI != "" {
		dbClient.SetJobsPoolStatusGSI(cfg.JobsPoolStatusGSI)
	}
	if cfg.JobsInstanceIDGSI != "" {
		dbClient.SetJobsInstanceIDGSI(cfg.JobsInstanceIDGSI)
	}
	cacheServer := cache.NewServer(awsCfg, cfg.CacheBucketName)
	metricsPublisher, prometheusHandler := initMetrics(awsCfg, cfg)

	eventsQueueClient := queue.NewClient(sqsCfg, cfg.EventsQueueURL)
	fleetManager := fleet.NewManager(awsCfg, cfg)
	poolManager := pools.NewManager(dbClient, fleetManager, cfg)
	eventHandler := events.NewHandler(eventsQueueClient, dbClient, metricsPublisher, cfg)
	eventHandler.SetJobQueue(jobQueue)

	ec2Client := ec2.NewFromConfig(awsCfg)
	poolManager.SetEC2Client(ec2Client)

	initCircuitBreaker(ctx, awsCfg, cfg, fleetManager, eventHandler)

	secretsStore := initSecretsStore(ctx, awsCfg, cfg)
	var terminationHandler *termination.Handler
	if cfg.TerminationQueueURL != "" {
		terminationQueueClient := queue.NewClient(sqsCfg, cfg.TerminationQueueURL)
		terminationHandler = termination.NewHandler(terminationQueueClient, dbClient, metricsPublisher, secretsStore, cfg)
	}
	housekeepingHandler, housekeepingScheduler := initHousekeeping(awsCfg, sqsCfg, cfg, secretsStore, metricsPublisher, dbClient)

	runnerManager := initRunnerManager(secretsStore, cfg)

	var subnetIndex uint64
	directProcessor := &worker.DirectProcessor{
		Fleet:       fleetManager,
		Pool:        poolManager,
		Metrics:     metricsPublisher,
		Runner:      runnerManager,
		DB:          dbClient,
		Config:      cfg,
		SubnetIndex: &subnetIndex,
	}
	directProcessorSem := make(chan struct{}, 10)

	ws := &webhookServer{
		cfg:                cfg,
		awsCfg:             awsCfg,
		sqsCfg:             sqsCfg,
		jobQueue:           jobQueue,
		dbClient:           dbClient,
		metricsPublisher:   metricsPublisher,
		directProcessor:    directProcessor,
		directProcessorSem: directProcessorSem,
		poolNotifier:       poolManager,
	}
	mux := ws.setupHTTPRoutes(cacheServer, prometheusHandler)

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	var workers sync.WaitGroup
	runWorker := func(fn func(context.Context)) {
		workers.Go(func() {
			fn(ctx)
		})
	}

	runWorker(func(c context.Context) {
		worker.RunEC2Worker(c, worker.EC2WorkerDeps{
			Queue:       jobQueue,
			Fleet:       fleetManager,
			Pool:        poolManager,
			Metrics:     metricsPublisher,
			Runner:      runnerManager,
			DB:          dbClient,
			Config:      cfg,
			SubnetIndex: &subnetIndex,
		})
	})
	runWorker(poolManager.ReconcileLoop)
	runWorker(eventHandler.Run)

	if terminationHandler != nil {
		runWorker(terminationHandler.Run)
	}
	if housekeepingHandler != nil {
		runWorker(housekeepingHandler.Run)
	}
	if housekeepingScheduler != nil {
		runWorker(housekeepingScheduler.Run)
	}

	go func() {
		log.Info(ctx, "http server listening", slog.String("addr", server.Addr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error(ctx, "http server failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info(ctx, "shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), config.MessageProcessTimeout)
	defer shutdownCancel()

	// Stop accepting new HTTP/webhook traffic before draining workers.
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error(ctx, "server shutdown failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Workers share the signal context and are already cancelling; wait for them
	// to return (bounded) before flushing telemetry so their final spans and
	// metrics are captured, rather than abandoning them mid-iteration.
	if waitForWorkers(&workers, workerDrainTimeout) {
		log.Info(ctx, "background workers stopped")
	} else {
		log.Warn(ctx, "background workers did not stop within grace period",
			slog.Duration("timeout", workerDrainTimeout))
	}

	if err := tracing.Shutdown(shutdownCtx, tp); err != nil {
		log.Warn(ctx, "tracing shutdown failed", slog.String("error", err.Error()))
	}

	if err := metricsPublisher.Close(); err != nil {
		log.Warn(ctx, "metrics publisher close failed", slog.String("error", err.Error()))
	}

	log.Info(ctx, "server stopped")
}

// workerDrainTimeout bounds how long shutdown waits for background workers to
// observe the cancelled context and return.
const workerDrainTimeout = 15 * time.Second

// waitForWorkers waits for the worker WaitGroup to drain, returning false if the
// timeout elapses first.
func waitForWorkers(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func initJobQueue(sqsCfg aws.Config, cfg *config.Config) queue.Queue {
	log := logging.WithComponent(logging.LogTypeQueue, "init")
	log.Info(context.Background(), "queue initialized", slog.String("type", "sqs"), slog.String(logging.KeyQueueURL, cfg.QueueURL))
	return queue.NewSQSClient(sqsCfg, cfg.QueueURL)
}

func initRunnerManager(secretsStore secrets.Store, cfg *config.Config) *runner.Manager {
	log := logging.WithComponent(logging.LogTypeServer, "runner")
	if cfg.GitHubAppID == "" || cfg.GitHubAppPrivateKey == "" {
		log.Warn(context.Background(), "runner manager not initialized", slog.String(logging.KeyReason, "github app credentials not configured"))
		return nil
	}
	githubClient, err := runner.NewGitHubClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKey)
	if err != nil {
		log.Error(context.Background(), "github client creation failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	return runner.NewManager(githubClient, secretsStore, runner.ManagerConfig{
		CacheSecret:         cfg.CacheSecret,
		CacheURL:            cfg.CacheURL,
		TerminationQueueURL: cfg.TerminationQueueURL,
	})
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
	return cb
}

func initSecretsStore(ctx context.Context, awsCfg aws.Config, cfg *config.Config) secrets.Store {
	log := logging.WithComponent(logging.LogTypeServer, "secrets")
	secretsCfg := secrets.Config{
		Backend: cfg.SecretsBackend,
		SSM: secrets.SSMConfig{
			Prefix: cfg.SecretsPathPrefix,
		},
		Vault: secrets.VaultConfig{
			Address:      cfg.VaultAddr,
			KVMount:      cfg.VaultKVMount,
			KVVersion:    cfg.VaultKVVersion,
			BasePath:     cfg.VaultBasePath,
			AuthMethod:   cfg.VaultAuthMethod,
			AWSRole:      cfg.VaultAWSRole,
			K8sAuthMount: cfg.VaultK8sAuthMount,
			K8sRole:      cfg.VaultK8sRole,
			K8sJWTPath:   cfg.VaultK8sJWTPath,
		},
	}
	store, err := secrets.NewStore(ctx, secretsCfg, awsCfg)
	if err != nil {
		log.Error(ctx, "secrets store creation failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	log.Info(ctx, "secrets store initialized", slog.String(logging.KeyBackend, cfg.SecretsBackend))
	return store
}

func initHousekeeping(awsCfg, sqsCfg aws.Config, cfg *config.Config, secretsStore secrets.Store, metricsPublisher metrics.Publisher, dbClient *db.Client) (*housekeeping.Handler, *housekeeping.Scheduler) {
	if cfg.HousekeepingQueueURL == "" {
		return nil, nil
	}

	housekeepingQueueClient := queue.NewClient(sqsCfg, cfg.HousekeepingQueueURL)

	var costReporter housekeeping.CostReporter
	if cfg.CostReportSNSTopic != "" || cfg.CostReportBucket != "" {
		costReporter = cost.NewReporter(awsCfg, cfg, cfg.CostReportSNSTopic, cfg.CostReportBucket)
	}

	housekeepingMetrics := &housekeepingMetricsAdapter{publisher: metricsPublisher}
	tasksExecutor := housekeeping.NewTasks(awsCfg, cfg, secretsStore, housekeepingMetrics, costReporter)

	if dbClient != nil {
		tasksExecutor.SetPoolDB(&poolDBAdapter{client: dbClient})
	}

	if cfg.GitHubAppID != "" && cfg.GitHubAppPrivateKey != "" {
		githubClient, err := runner.NewGitHubClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKey)
		if err != nil {
			hkLog := logging.WithComponent(logging.LogTypeServer, "housekeeping")
			hkLog.Error(context.Background(), "failed to initialize GitHub client for stale job detection", slog.String("error", err.Error()))
		} else {
			tasksExecutor.SetGitHubJobChecker(&githubJobCheckerAdapter{client: githubClient})
		}
	}

	h := housekeeping.NewHandler(housekeepingQueueClient, tasksExecutor, cfg)

	if dbClient != nil {
		instanceID := uuid.NewString()
		h.SetTaskLocker(dbClient, instanceID)
	}

	schedulerCfg := housekeeping.DefaultSchedulerConfig()
	scheduler := housekeeping.NewSchedulerFromConfig(sqsCfg, cfg.HousekeepingQueueURL, schedulerCfg)
	scheduler.SetMetrics(metricsPublisher)

	return h, scheduler
}

func initMetrics(awsCfg aws.Config, cfg *config.Config) (metrics.Publisher, http.Handler) {
	log := logging.WithComponent(logging.LogTypeServer, "metrics")
	var publishers []metrics.Publisher
	var prometheusHandler http.Handler
	var backends []string

	if cfg.MetricsCloudWatchEnabled {
		namespace := cfg.MetricsNamespace
		if namespace == "" {
			namespace = "RunsFleet"
		}
		publishers = append(publishers, metrics.NewCloudWatchPublisherWithNamespace(awsCfg, namespace))
		backends = append(backends, "cloudwatch")
	}

	if cfg.MetricsPrometheusEnabled {
		namespace := cfg.MetricsNamespace
		if namespace == "" {
			namespace = "runs_fleet"
		}
		prom := metrics.NewPrometheusPublisher(metrics.PrometheusConfig{Namespace: namespace})
		publishers = append(publishers, prom)
		prometheusHandler = prom.Handler()
		backends = append(backends, "prometheus")
	}

	if cfg.MetricsDatadogEnabled {
		namespace := cfg.MetricsNamespace
		if namespace == "" {
			namespace = "runs_fleet"
		}
		dd, err := metrics.NewDatadogPublisher(metrics.DatadogConfig{
			Address:               cfg.MetricsDatadogAddr,
			Namespace:             namespace,
			Tags:                  cfg.MetricsDatadogTags,
			SampleRate:            cfg.MetricsDatadogSampleRate,
			BufferPoolSize:        cfg.MetricsDatadogBufferPoolSize,
			WorkersCount:          cfg.MetricsDatadogWorkersCount,
			MaxMessagesPerPayload: cfg.MetricsDatadogMaxMsgsPerPayload,
		})
		if err != nil {
			log.Warn(context.Background(), "datadog metrics init failed", slog.String("error", err.Error()))
		} else {
			publishers = append(publishers, dd)
			backends = append(backends, "datadog")
		}
	}

	if len(publishers) == 0 {
		return metrics.NoopPublisher{}, nil
	}

	log.Info(context.Background(), "metrics initialized", slog.Any("backends", backends))

	if len(publishers) == 1 {
		return publishers[0], prometheusHandler
	}

	return metrics.NewMultiPublisher(publishers...), prometheusHandler
}

// PoolReconcileNotifier allows the webhook handler to trigger immediate pool reconciliation.
type PoolReconcileNotifier interface {
	NotifyPoolDemand(poolName string)
}

var validPoolName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

type webhookServer struct {
	cfg                *config.Config
	awsCfg             aws.Config
	sqsCfg             aws.Config
	jobQueue           queue.Queue
	dbClient           *db.Client
	metricsPublisher   metrics.Publisher
	directProcessor    *worker.DirectProcessor
	directProcessorSem chan struct{}
	poolNotifier       PoolReconcileNotifier
}

func (ws *webhookServer) setupHTTPRoutes(cacheServer *cache.Server, prometheusHandler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "OK\n")
	})

	mux.HandleFunc("/ready", ws.handleReadiness)

	if prometheusHandler != nil {
		mux.Handle(ws.cfg.MetricsPrometheusPath, prometheusHandler)
	}

	cacheHandler := cache.NewHandlerWithAuth(cacheServer, ws.metricsPublisher, ws.cfg.CacheSecret)
	cacheHandler.RegisterRoutes(mux)

	// Admin API handlers
	adminAuth := admin.NewAuthMiddleware(ws.cfg.AdminSecret)
	adminRateLimiter := admin.NewRateLimiter(ws.cfg.AdminRateLimit)

	adminMux := http.NewServeMux()

	adminHandler := admin.NewHandler(ws.dbClient, ws.cfg.AdminSecret)
	adminHandler.RegisterRoutes(adminMux)

	jobsHandler := admin.NewJobsHandler(ws.dbClient, adminAuth, ws.cfg.TraceUIURL)
	jobsHandler.RegisterRoutes(adminMux)

	ec2Client := ec2.NewFromConfig(ws.awsCfg)
	instancesHandler := admin.NewInstancesHandler(ec2Client, ws.dbClient, adminAuth)
	instancesHandler.RegisterRoutes(adminMux)

	sqsClient := sqs.NewFromConfig(ws.sqsCfg)
	queuesHandler := admin.NewQueuesHandler(sqsClient, admin.QueueConfig{
		MainQueue:         ws.cfg.QueueURL,
		MainQueueDLQ:      ws.cfg.QueueDLQURL,
		PoolQueue:         ws.cfg.PoolQueueURL,
		EventsQueue:       ws.cfg.EventsQueueURL,
		TerminationQueue:  ws.cfg.TerminationQueueURL,
		HousekeepingQueue: ws.cfg.HousekeepingQueueURL,
	}, adminAuth)
	queuesHandler.RegisterRoutes(adminMux)

	dynamoClient := dynamodb.NewFromConfig(ws.awsCfg)
	circuitHandler := admin.NewCircuitHandler(dynamoClient, ws.cfg.CircuitBreakerTable, adminAuth)
	circuitHandler.RegisterRoutes(adminMux)

	housekeepingHandler := admin.NewHousekeepingHandler(ec2Client, dynamoClient, ws.cfg.JobsTableName, adminAuth)
	housekeepingHandler.RegisterRoutes(adminMux)

	costAdminHandler := admin.NewCostHandler(ws.dbClient, adminAuth)
	costAdminHandler.RegisterRoutes(adminMux)

	mux.Handle("/api/", adminRateLimiter.Wrap(adminMux))
	mux.Handle("/admin/", admin.UIHandler())

	mux.HandleFunc("/webhook", ws.handleWebhook)

	return mux
}

func (ws *webhookServer) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if pinger, ok := ws.jobQueue.(queue.Pinger); ok {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pinger.Ping(pingCtx); err != nil {
			serverLog.Warn(pingCtx, "readiness check failed", slog.String("error", err.Error()))
			http.Error(w, "Queue not ready", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "OK\n")
}

func (ws *webhookServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	log := logging.WithComponent(logging.LogTypeWebhook, "handler")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	payload, err := gh.ParseWebhook(r, ws.cfg.GitHubWebhookSecret)
	if err != nil {
		log.Warn(r.Context(), "webhook parse failed", slog.String("error", err.Error()))
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	processed, response := ws.processWebhookEvent(r.Context(), payload)

	w.WriteHeader(http.StatusOK)
	if processed {
		_, _ = fmt.Fprintf(w, "%s\n", response)
	} else {
		_, _ = fmt.Fprintf(w, "Event acknowledged\n")
	}
}

func (ws *webhookServer) processWebhookEvent(ctx context.Context, payload interface{}) (bool, string) {
	event, ok := payload.(*github.WorkflowJobEvent)
	if !ok {
		return false, ""
	}

	switch event.GetAction() {
	case "queued":
		jobMsg, err := handler.HandleWorkflowJobQueued(ctx, event, ws.jobQueue, ws.dbClient, ws.metricsPublisher)
		if err != nil || jobMsg == nil {
			return false, ""
		}
		if jobMsg.Pool != "" && len(jobMsg.Pool) <= 63 && validPoolName.MatchString(jobMsg.Pool) && ws.poolNotifier != nil {
			ws.poolNotifier.NotifyPoolDemand(jobMsg.Pool)
		}
		worker.TryDirectProcessing(ctx, ws.directProcessor, ws.directProcessorSem, jobMsg)
		return true, "Job queued"

	case "completed":
		if event.GetWorkflowJob().GetConclusion() != "failure" {
			return false, ""
		}
		requeued, err := handler.HandleJobFailure(ctx, event, ws.jobQueue, ws.dbClient, ws.metricsPublisher)
		if err != nil {
			failCtx := logging.ContextWithJob(ctx, event.GetWorkflowJob().GetID(), 0, event.GetRepo().GetFullName())
			webhookLog.Error(failCtx, "job failure handling failed",
				slog.String("error", err.Error()))
		}
		if requeued {
			return true, "Job requeued"
		}
	}
	return false, ""
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

func (h *housekeepingMetricsAdapter) PublishOrphanedJobsCleanedUp(ctx context.Context, count int) error {
	return h.publisher.PublishOrphanedJobsCleanedUp(ctx, count)
}

func (h *housekeepingMetricsAdapter) PublishStaleJobsReconciled(ctx context.Context, count int) error {
	return h.publisher.PublishStaleJobsReconciled(ctx, count)
}

func (h *housekeepingMetricsAdapter) PublishPoolUtilization(ctx context.Context, poolName string, utilization float64) error {
	return h.publisher.PublishPoolUtilization(ctx, poolName, utilization)
}

// poolDBAdapter adapts db.Client to housekeeping.PoolDBAPI.
type poolDBAdapter struct {
	client *db.Client
}

func (p *poolDBAdapter) ListPools(ctx context.Context) ([]string, error) {
	return p.client.ListPools(ctx)
}

func (p *poolDBAdapter) GetPoolConfig(ctx context.Context, poolName string) (*housekeeping.PoolConfig, error) {
	cfg, err := p.client.GetPoolConfig(ctx, poolName)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}
	return &housekeeping.PoolConfig{
		PoolName:    cfg.PoolName,
		Ephemeral:   cfg.Ephemeral,
		LastJobTime: cfg.LastJobTime,
	}, nil
}

func (p *poolDBAdapter) DeletePoolConfig(ctx context.Context, poolName string) error {
	return p.client.DeletePoolConfig(ctx, poolName)
}

// githubJobCheckerAdapter adapts runner.GitHubClient to housekeeping.GitHubJobChecker.
type githubJobCheckerAdapter struct {
	client *runner.GitHubClient
}

func (g *githubJobCheckerAdapter) GetWorkflowJobStatus(ctx context.Context, _ string, repo string, jobID int64) (*housekeeping.GitHubJobStatus, error) {
	info, err := g.client.GetWorkflowJobByID(ctx, repo, jobID)
	if err != nil {
		return nil, err
	}
	return &housekeeping.GitHubJobStatus{
		Completed:  info.Status == "completed",
		Conclusion: info.Conclusion,
	}, nil
}
