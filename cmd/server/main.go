// Package main implements the runs-fleet orchestrator server that processes GitHub webhook events.
package main

import (
	"context"
	"fmt"
	"log"
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
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/provider/k8s"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
	"github.com/Shavakan/runs-fleet/pkg/termination"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/google/go-github/v57/github"
	"github.com/google/uuid"
)

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
		log.Printf("Initializing K8s backend (namespace: %s)", cfg.KubeNamespace)
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

	if poolManager != nil {
		ec2Client := ec2.NewFromConfig(awsCfg)
		poolManager.SetEC2Client(ec2Client)
		log.Println("Pool manager initialized with EC2 client for reconciliation")
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

	mux := setupHTTPRoutes(cfg, jobQueue, dbClient, cacheServer, metricsPublisher, prometheusHandler, directProcessor, directProcessorSem)

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
		log.Printf("Server listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutdown signal received, gracefully stopping...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), config.MessageProcessTimeout)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}

	if err := metricsPublisher.Close(); err != nil {
		log.Printf("Metrics publisher close failed: %v", err)
	}

	log.Println("Server stopped")
}

func initJobQueue(awsCfg aws.Config, cfg *config.Config) queue.Queue {
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
			log.Fatalf("Failed to create Valkey client: %v", err)
		}
		log.Printf("Using Valkey queue at %s", cfg.ValkeyAddr)
		return valkeyClient
	}
	log.Printf("Using SQS queue at %s", cfg.QueueURL)
	return queue.NewSQSClient(awsCfg, cfg.QueueURL)
}

func initRunnerManager(secretsStore secrets.Store, cfg *config.Config) *runner.Manager {
	if cfg.GitHubAppID == "" || cfg.GitHubAppPrivateKey == "" {
		log.Println("WARNING: Runner manager not initialized - GitHub App credentials not configured")
		return nil
	}
	githubClient, err := runner.NewGitHubClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKey)
	if err != nil {
		log.Fatalf("Failed to create GitHub client: %v", err)
	}
	rm := runner.NewManager(githubClient, secretsStore, runner.ManagerConfig{
		CacheSecret:         cfg.CacheSecret,
		CacheURL:            cfg.CacheURL,
		TerminationQueueURL: cfg.TerminationQueueURL,
	})
	log.Println("Runner manager initialized for secrets store")
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

func initSecretsStore(ctx context.Context, awsCfg aws.Config, cfg *config.Config) secrets.Store {
	secretsCfg := secrets.Config{
		Backend: cfg.SecretsBackend,
		SSM: secrets.SSMConfig{
			Prefix: cfg.SecretsPathPrefix,
		},
		Vault: secrets.VaultConfig{
			Address:  cfg.VaultAddr,
			KVMount:  cfg.VaultKVMount,
			BasePath: cfg.VaultKVPath,
			AWSRole:  cfg.VaultAWSRole,
		},
	}
	store, err := secrets.NewStore(ctx, secretsCfg, awsCfg)
	if err != nil {
		log.Fatalf("Failed to create secrets store: %v", err)
	}
	log.Printf("Secrets store initialized (backend: %s)", cfg.SecretsBackend)
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

	schedulerCfg := housekeeping.DefaultSchedulerConfig()
	scheduler := housekeeping.NewSchedulerFromConfig(awsCfg, cfg.HousekeepingQueueURL, schedulerCfg)
	scheduler.SetMetrics(metricsPublisher)

	log.Printf("Housekeeping handler and scheduler initialized with queue: %s", cfg.HousekeepingQueueURL)
	return h, scheduler
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

func setupHTTPRoutes(cfg *config.Config, jobQueue queue.Queue, dbClient *db.Client, cacheServer *cache.Server, metricsPublisher metrics.Publisher, prometheusHandler http.Handler, directProcessor *worker.DirectProcessor, directProcessorSem chan struct{}) *http.ServeMux {
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

	adminHandler := admin.NewHandler(dbClient, cfg.AdminSecret)
	adminHandler.RegisterRoutes(mux)
	mux.Handle("/admin/", admin.UIHandler())
	if cfg.AdminSecret != "" {
		log.Println("Admin API enabled at /api/pools with authentication, UI at /admin/")
	} else {
		log.Println("Admin API enabled at /api/pools (no authentication), UI at /admin/")
	}

	mux.HandleFunc("/webhook", makeWebhookHandler(cfg, jobQueue, dbClient, metricsPublisher, directProcessor, directProcessorSem))

	return mux
}

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

func makeWebhookHandler(cfg *config.Config, jobQueue queue.Queue, dbClient *db.Client, metricsPublisher metrics.Publisher, directProcessor *worker.DirectProcessor, directProcessorSem chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		processed, response := processWebhookEvent(r.Context(), payload, jobQueue, dbClient, metricsPublisher, directProcessor, directProcessorSem)

		w.WriteHeader(http.StatusOK)
		if processed {
			_, _ = fmt.Fprintf(w, "%s\n", response)
		} else {
			_, _ = fmt.Fprintf(w, "Event acknowledged\n")
		}
	}
}

func processWebhookEvent(ctx context.Context, payload interface{}, jobQueue queue.Queue, dbClient *db.Client, metricsPublisher metrics.Publisher, directProcessor *worker.DirectProcessor, directProcessorSem chan struct{}) (bool, string) {
	event, ok := payload.(*github.WorkflowJobEvent)
	if !ok {
		return false, ""
	}

	switch event.GetAction() {
	case "queued":
		jobMsg, err := handler.HandleWorkflowJobQueued(ctx, event, jobQueue, dbClient, metricsPublisher)
		if err != nil || jobMsg == nil {
			return false, ""
		}
		worker.TryDirectProcessing(ctx, directProcessor, directProcessorSem, jobMsg)
		return true, "Job queued"

	case "completed":
		if event.GetWorkflowJob().GetConclusion() != "failure" {
			return false, ""
		}
		requeued, err := handler.HandleJobFailure(ctx, event, jobQueue, dbClient, metricsPublisher)
		if err != nil {
			log.Printf("Failed to handle job failure: %v", err)
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
