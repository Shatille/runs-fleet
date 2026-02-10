// Package main implements the runs-fleet orchestrator server that processes GitHub webhook events.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	"github.com/Shavakan/runs-fleet/pkg/provider/k8s"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
	"github.com/Shavakan/runs-fleet/pkg/termination"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/go-github/v57/github"
	"github.com/google/uuid"
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

	log.Info("server starting")

	cfg, err := config.Load()
	if err != nil {
		log.Error("config load failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		log.Error("aws config load failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	jobQueue := initJobQueue(awsCfg, cfg)
	dbClient := db.NewClient(awsCfg, cfg.PoolsTableName, cfg.JobsTableName)
	cacheServer := cache.NewServer(awsCfg, cfg.CacheBucketName)
	metricsPublisher, prometheusHandler := initMetrics(awsCfg, cfg)

	var fleetManager *fleet.Manager
	var poolManager *pools.Manager
	var eventHandler *events.Handler
	var k8sProvider *k8s.Provider
	var k8sPoolProvider *k8s.PoolProvider

	if cfg.IsK8sBackend() {
		log.Info("initializing backend", slog.String(logging.KeyBackend, "k8s"), slog.String(logging.KeyNamespace, cfg.KubeNamespace))
		k8sProvider, err = k8s.NewProvider(cfg)
		if err != nil {
			log.Error("k8s provider creation failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
		k8sPoolProvider = k8s.NewPoolProvider(k8sProvider.Clientset(), cfg)
	} else {
		log.Info("initializing backend", slog.String(logging.KeyBackend, "ec2"))
		eventsQueueClient := queue.NewClient(awsCfg, cfg.EventsQueueURL)
		fleetManager = fleet.NewManager(awsCfg, cfg)
		poolManager = pools.NewManager(dbClient, fleetManager, cfg)
		eventHandler = events.NewHandler(eventsQueueClient, dbClient, metricsPublisher, cfg)
	}

	if poolManager != nil {
		ec2Client := ec2.NewFromConfig(awsCfg)
		poolManager.SetEC2Client(ec2Client)
	}

	initCircuitBreaker(ctx, awsCfg, cfg, fleetManager, eventHandler)

	var secretsStore secrets.Store
	var terminationHandler *termination.Handler
	var housekeepingHandler *housekeeping.Handler
	var housekeepingScheduler *housekeeping.Scheduler
	if cfg.IsEC2Backend() {
		secretsStore = initSecretsStore(ctx, awsCfg, cfg)
		if cfg.TerminationQueueURL != "" {
			terminationQueueClient := queue.NewClient(awsCfg, cfg.TerminationQueueURL)
			terminationHandler = termination.NewHandler(terminationQueueClient, dbClient, metricsPublisher, secretsStore, cfg)
		}
		housekeepingHandler, housekeepingScheduler = initHousekeeping(awsCfg, cfg, secretsStore, metricsPublisher, dbClient)
	}

	runnerManager := initRunnerManager(secretsStore, cfg)

	var subnetIndex uint64
	var directProcessor *worker.DirectProcessor
	var directProcessorSem chan struct{}
	if cfg.IsEC2Backend() {
		directProcessor = &worker.DirectProcessor{
			Fleet:       fleetManager,
			Pool:        poolManager,
			Metrics:     metricsPublisher,
			Runner:      runnerManager,
			DB:          dbClient,
			Config:      cfg,
			SubnetIndex: &subnetIndex,
		}
		directProcessorSem = make(chan struct{}, 10)
	}

	ws := &webhookServer{
		cfg:                cfg,
		awsCfg:             awsCfg,
		jobQueue:           jobQueue,
		dbClient:           dbClient,
		metricsPublisher:   metricsPublisher,
		directProcessor:    directProcessor,
		directProcessorSem: directProcessorSem,
	}
	mux := ws.setupHTTPRoutes(cacheServer, prometheusHandler)

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	if cfg.IsK8sBackend() {
		go worker.RunK8sWorker(ctx, worker.K8sWorkerDeps{
			Queue:        jobQueue,
			Provider:     k8sProvider,
			PoolProvider: k8sPoolProvider,
			Metrics:      metricsPublisher,
			Runner:       runnerManager,
			DB:           dbClient,
			Config:       cfg,
		})
		go k8sPoolProvider.ReconcileLoop(ctx)
	} else {
		go worker.RunEC2Worker(ctx, worker.EC2WorkerDeps{
			Queue:       jobQueue,
			Fleet:       fleetManager,
			Pool:        poolManager,
			Metrics:     metricsPublisher,
			Runner:      runnerManager,
			DB:          dbClient,
			Config:      cfg,
			SubnetIndex: &subnetIndex,
		})
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
		log.Info("http server listening", slog.String("addr", server.Addr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), config.MessageProcessTimeout)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("server shutdown failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	if err := metricsPublisher.Close(); err != nil {
		log.Warn("metrics publisher close failed", slog.String("error", err.Error()))
	}

	log.Info("server stopped")
}

func initJobQueue(awsCfg aws.Config, cfg *config.Config) queue.Queue {
	log := logging.WithComponent(logging.LogTypeQueue, "init")
	if cfg.IsK8sBackend() {
		consumerID := os.Getenv("HOSTNAME")
		if consumerID == "" {
			consumerID = uuid.NewString()
		}
		valkeyClient, err := queue.NewValkeyClient(queue.ValkeyConfig{
			Addr:       cfg.ValkeyAddr,
			Password:   cfg.ValkeyPassword,
			DB:         cfg.ValkeyDB,
			Stream:     "runs-fleet:jobs",
			Group:      "orchestrator",
			ConsumerID: consumerID,
		})
		if err != nil {
			log.Error("valkey client creation failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
		log.Info("queue initialized", slog.String("type", "valkey"), slog.String("addr", cfg.ValkeyAddr))
		return valkeyClient
	}
	log.Info("queue initialized", slog.String("type", "sqs"), slog.String(logging.KeyQueueURL, cfg.QueueURL))
	return queue.NewSQSClient(awsCfg, cfg.QueueURL)
}

func initRunnerManager(secretsStore secrets.Store, cfg *config.Config) *runner.Manager {
	log := logging.WithComponent(logging.LogTypeServer, "runner")
	if cfg.GitHubAppID == "" || cfg.GitHubAppPrivateKey == "" {
		log.Warn("runner manager not initialized", slog.String(logging.KeyReason, "github app credentials not configured"))
		return nil
	}
	githubClient, err := runner.NewGitHubClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKey)
	if err != nil {
		log.Error("github client creation failed", slog.String("error", err.Error()))
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
		log.Error("secrets store creation failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	log.Info("secrets store initialized", slog.String(logging.KeyBackend, cfg.SecretsBackend))
	return store
}

func initHousekeeping(awsCfg aws.Config, cfg *config.Config, secretsStore secrets.Store, metricsPublisher metrics.Publisher, dbClient *db.Client) (*housekeeping.Handler, *housekeeping.Scheduler) {
	if cfg.HousekeepingQueueURL == "" {
		return nil, nil
	}

	housekeepingQueueClient := queue.NewClient(awsCfg, cfg.HousekeepingQueueURL)

	var costReporter housekeeping.CostReporter
	if cfg.CostReportSNSTopic != "" || cfg.CostReportBucket != "" {
		costReporter = cost.NewReporter(awsCfg, cfg, cfg.CostReportSNSTopic, cfg.CostReportBucket)
	}

	housekeepingMetrics := &housekeepingMetricsAdapter{publisher: metricsPublisher}
	tasksExecutor := housekeeping.NewTasks(awsCfg, cfg, secretsStore, housekeepingMetrics, costReporter)

	if dbClient != nil {
		tasksExecutor.SetPoolDB(&poolDBAdapter{client: dbClient})
	}

	h := housekeeping.NewHandler(housekeepingQueueClient, tasksExecutor, cfg)

	if dbClient != nil {
		instanceID := uuid.NewString()
		h.SetTaskLocker(dbClient, instanceID)
	}

	schedulerCfg := housekeeping.DefaultSchedulerConfig()
	scheduler := housekeeping.NewSchedulerFromConfig(awsCfg, cfg.HousekeepingQueueURL, schedulerCfg)
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
			log.Warn("datadog metrics init failed", slog.String("error", err.Error()))
		} else {
			publishers = append(publishers, dd)
			backends = append(backends, "datadog")
		}
	}

	if len(publishers) == 0 {
		return metrics.NoopPublisher{}, nil
	}

	log.Info("metrics initialized", slog.Any("backends", backends))

	if len(publishers) == 1 {
		return publishers[0], prometheusHandler
	}

	return metrics.NewMultiPublisher(publishers...), prometheusHandler
}

type webhookServer struct {
	cfg                *config.Config
	awsCfg             aws.Config
	jobQueue           queue.Queue
	dbClient           *db.Client
	metricsPublisher   metrics.Publisher
	directProcessor    *worker.DirectProcessor
	directProcessorSem chan struct{}
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

	adminHandler := admin.NewHandler(ws.dbClient, ws.cfg.AdminSecret)
	adminHandler.RegisterRoutes(mux)

	jobsHandler := admin.NewJobsHandler(ws.dbClient, adminAuth)
	jobsHandler.RegisterRoutes(mux)

	ec2Client := ec2.NewFromConfig(ws.awsCfg)
	instancesHandler := admin.NewInstancesHandler(ec2Client, ws.dbClient, adminAuth)
	instancesHandler.RegisterRoutes(mux)

	sqsClient := sqs.NewFromConfig(ws.awsCfg)
	queuesHandler := admin.NewQueuesHandler(sqsClient, admin.QueueConfig{
		MainQueue:         ws.cfg.QueueURL,
		MainQueueDLQ:      ws.cfg.QueueDLQURL,
		PoolQueue:         ws.cfg.PoolQueueURL,
		EventsQueue:       ws.cfg.EventsQueueURL,
		TerminationQueue:  ws.cfg.TerminationQueueURL,
		HousekeepingQueue: ws.cfg.HousekeepingQueueURL,
	}, adminAuth)
	queuesHandler.RegisterRoutes(mux)

	dynamoClient := dynamodb.NewFromConfig(ws.awsCfg)
	circuitHandler := admin.NewCircuitHandler(dynamoClient, ws.cfg.CircuitBreakerTable, adminAuth)
	circuitHandler.RegisterRoutes(mux)

	mux.Handle("/admin/", admin.UIHandler())

	mux.HandleFunc("/webhook", ws.handleWebhook)

	return mux
}

func (ws *webhookServer) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if pinger, ok := ws.jobQueue.(queue.Pinger); ok {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pinger.Ping(pingCtx); err != nil {
			serverLog.Warn("readiness check failed", slog.String("error", err.Error()))
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
		log.Warn("webhook parse failed", slog.String("error", err.Error()))
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
		worker.TryDirectProcessing(ctx, ws.directProcessor, ws.directProcessorSem, jobMsg)
		return true, "Job queued"

	case "completed":
		if event.GetWorkflowJob().GetConclusion() != "failure" {
			return false, ""
		}
		requeued, err := handler.HandleJobFailure(ctx, event, ws.jobQueue, ws.dbClient, ws.metricsPublisher)
		if err != nil {
			webhookLog.Error("job failure handling failed",
				slog.Int64(logging.KeyJobID, event.GetWorkflowJob().GetID()),
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
