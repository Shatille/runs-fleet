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
	"strings"
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

	labelAliasResolver, err := gh.ParseAliasRules(cfg.LabelAliasesJSON)
	if err != nil {
		log.Error(ctx, "label alias config invalid", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if n := labelAliasResolver.Len(); n > 0 {
		log.Info(ctx, "label aliases loaded", slog.Int("rules", n))
	}

	tp, err := tracing.Setup(ctx, cfg.Tracing)
	if err != nil {
		log.Error(ctx, "tracing setup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	otel.SetTracerProvider(tp)

	// SQS ReceiveMessage long-polls with WaitTimeSeconds=20, which exceeds
	// AWSPerOpTimeout (15s); bounding it would abort every empty poll, so it is
	// exempt from the per-operation timeout. Other SQS ops (SendMessage,
	// DeleteMessage) are fast and stay bounded.
	perOpTimeout := awsobs.PerOperationTimeout(config.AWSPerOpTimeout, map[string]bool{"ReceiveMessage": true})
	// The observability middleware emits AWS-call latency and failure metrics; its
	// publisher is installed after initMetrics builds it (the CloudWatch backend
	// depends on awsCfg, which carries this middleware).
	awsObsMiddlewares, awsObsRecorder := awsobs.Middlewares()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.AWSRegion),
		awsconfig.WithHTTPClient(newAWSHTTPClient(config.AWSResponseHeaderTimeout)),
		awsconfig.WithRetryer(awsRetryer),
		// Observability is registered first so it stays the outermost Initialize
		// middleware and records the full operation duration, including a
		// per-operation-timeout abort; the timeout middleware inserts itself just
		// inside it.
		awsconfig.WithAPIOptions(append(awsObsMiddlewares, perOpTimeout)),
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
	if cfg.AuditTableName != "" {
		dbClient.SetAuditTable(cfg.AuditTableName)
	}
	cacheServer := cache.NewServer(awsCfg, cfg.CacheBucketName)
	metricsPublisher, prometheusHandler := initMetrics(awsCfg, cfg)
	awsObsRecorder.SetPublisher(metricsPublisher)

	eventsQueueClient := queue.NewClient(sqsCfg, cfg.EventsQueueURL)
	fleetManager := fleet.NewManager(awsCfg, cfg)
	fleetManager.SetMetrics(metricsPublisher)
	poolManager := pools.NewManager(dbClient, fleetManager, cfg)
	poolManager.SetMetrics(metricsPublisher)
	eventHandler := events.NewHandler(eventsQueueClient, dbClient, metricsPublisher, cfg)
	eventHandler.SetJobQueue(jobQueue)

	ec2Client := ec2.NewFromConfig(awsCfg)
	poolManager.SetEC2Client(ec2Client)

	initCircuitBreaker(ctx, awsCfg, cfg, fleetManager, eventHandler)

	secretsStore := initSecretsStore(ctx, awsCfg, cfg)
	var terminationHandler *termination.Handler
	if cfg.TerminationQueueURL != "" {
		terminationQueueClient := queue.NewClient(sqsCfg, cfg.TerminationQueueURL)
		terminationHandler = termination.NewHandler(terminationQueueClient, dbClient, metricsPublisher, secretsStore, jobQueue, cfg)
	}
	githubClient := initGitHubClient(cfg)
	housekeepingRunner := initHousekeeping(awsCfg, cfg, secretsStore, metricsPublisher, dbClient, jobQueue, githubClient)

	runnerManager := initRunnerManager(githubClient, secretsStore, cfg)

	var subnetIndex uint64
	directProcessor := &worker.DirectProcessor{
		Fleet:       fleetManager,
		Pool:        poolManager,
		Metrics:     metricsPublisher,
		Runner:      runnerManager,
		DB:          dbClient,
		Config:      cfg,
		SubnetIndex: &subnetIndex,
		Queue:       jobQueue,
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
		labelAliasResolver: labelAliasResolver,
		fleetManager:       fleetManager,
	}
	mux, err := ws.setupHTTPRoutes(ctx, cacheServer, prometheusHandler)
	if err != nil {
		log.Error(ctx, "http route setup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

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
	if housekeepingRunner != nil {
		runWorker(housekeepingRunner.Run)
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

	// Workers share the signal context: receiving has already stopped, but each
	// has detached its in-flight processing from the signal so it can finish.
	// Wait for that drain (bounded by workerDrainTimeout) before flushing
	// telemetry so in-flight jobs complete and their final spans and metrics are
	// captured, rather than aborting them mid-flight.
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
// drain in-flight message processing and return. On SIGTERM each worker stops
// accepting new work immediately but lets an already-dispatched processor run to
// completion (detached from the signal, bounded by config.MessageProcessTimeout);
// this must cover that processing window plus a small buffer so the drain is not
// abandoned while an in-flight job is still finishing. It must stay below the
// pod's terminationGracePeriodSeconds (see deploy/) or k8s SIGKILLs mid-drain.
const workerDrainTimeout = config.MessageProcessTimeout + 10*time.Second

// oidcDiscoveryTimeout bounds the startup call to the OIDC issuer's
// well-known discovery endpoint. A fast HTTP error already fails startup via
// the returned error; this covers the case where the issuer is unreachable
// (e.g. black-holed) and never responds at all -- without a bound, that
// hangs main() forever instead of failing loudly.
const oidcDiscoveryTimeout = 15 * time.Second

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

// initGitHubClient builds the single GitHub App client shared by the runner
// manager and housekeeping's stale-job checker, so both talk to GitHub
// through one installation-token cache instead of minting their own.
func initGitHubClient(cfg *config.Config) *gh.Client {
	log := logging.WithComponent(logging.LogTypeServer, "github")
	if cfg.GitHubAppID == "" || cfg.GitHubAppPrivateKey == "" {
		log.Warn(context.Background(), "github client not initialized", slog.String(logging.KeyReason, "github app credentials not configured"))
		return nil
	}
	client, err := gh.NewClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKey)
	if err != nil {
		log.Error(context.Background(), "github client creation failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	return client
}

func initRunnerManager(githubClient *gh.Client, secretsStore secrets.Store, cfg *config.Config) *runner.Manager {
	if githubClient == nil {
		return nil
	}
	return runner.NewManager(githubClient, secretsStore, runner.ManagerConfig{
		CacheSecret:         cfg.CacheSecret,
		BaseURL:             cfg.BaseURL,
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

func initHousekeeping(awsCfg aws.Config, cfg *config.Config, secretsStore secrets.Store, metricsPublisher metrics.Publisher, dbClient *db.Client, jobQueue queue.Queue, githubClient *gh.Client) *housekeeping.Runner {
	if dbClient == nil || cfg.PoolsTableName == "" {
		logging.WithComponent(logging.LogTypeServer, "housekeeping").Warn(context.Background(),
			"housekeeping disabled: no pools table configured for task locking")
		return nil
	}

	var costReporter housekeeping.CostReporter
	if cfg.CostReportSNSTopic != "" || cfg.CostReportBucket != "" {
		costReporter = cost.NewReporter(awsCfg, cfg, cfg.CostReportSNSTopic, cfg.CostReportBucket)
	}

	tasksExecutor := housekeeping.NewTasks(awsCfg, cfg, secretsStore, metricsPublisher, costReporter)
	tasksExecutor.SetPoolDB(&poolDBAdapter{client: dbClient})
	tasksExecutor.SetJobRequeuer(jobQueue)

	if githubClient != nil {
		tasksExecutor.SetGitHubJobChecker(&githubJobCheckerAdapter{client: githubClient})
	}

	r := housekeeping.NewRunner(tasksExecutor, housekeeping.DefaultSchedulerConfig())
	r.SetMetrics(metricsPublisher)
	r.SetTaskLocker(dbClient, uuid.NewString())
	return r
}

func initMetrics(awsCfg aws.Config, cfg *config.Config) (metrics.Publisher, http.Handler) {
	log := logging.WithComponent(logging.LogTypeServer, "metrics")
	var publishers []metrics.Publisher
	var prometheusHandler http.Handler
	var backends []string

	if cfg.MetricsCloudWatchEnabled {
		publishers = append(publishers, metrics.NewCloudWatchPublisher(awsCfg))
		backends = append(backends, "cloudwatch")
	}

	if cfg.MetricsPrometheusEnabled {
		prom := metrics.NewPrometheusPublisher(metrics.PrometheusConfig{})
		publishers = append(publishers, prom)
		prometheusHandler = prom.Handler()
		backends = append(backends, "prometheus")
	}

	if cfg.MetricsDatadogEnabled {
		dd, err := metrics.NewDatadogPublisher(metrics.DatadogConfig{
			Address:               cfg.MetricsDatadogAddr,
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
	labelAliasResolver *gh.AliasResolver
	fleetManager       *fleet.Manager
}

func (ws *webhookServer) setupHTTPRoutes(ctx context.Context, cacheServer *cache.Server, prometheusHandler http.Handler) (*http.ServeMux, error) {
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
	var oidcClient *admin.OIDCClient
	if ws.cfg.OIDCIssuerURL != "" {
		// Bounded so an unreachable/black-holed issuer fails startup fast
		// instead of hanging main() forever (a slow HTTP error already
		// propagates via err below; a connection that never responds would
		// not, without this).
		discoverCtx, cancel := context.WithTimeout(ctx, oidcDiscoveryTimeout)
		defer cancel()

		var err error
		oidcClient, err = admin.NewOIDCClient(discoverCtx, admin.OIDCClientConfig{
			IssuerURL:    ws.cfg.OIDCIssuerURL,
			ClientID:     ws.cfg.OIDCClientID,
			ClientSecret: ws.cfg.OIDCClientSecret,
			RedirectURL:  oidcRedirectURL(ws.cfg),
			Scopes:       ws.cfg.OIDCScopes,
			GroupsClaim:  ws.cfg.OIDCGroupsClaim,
		})
		if err != nil {
			return nil, fmt.Errorf("admin OIDC client setup: %w", err)
		}
	}

	adminAuth := admin.NewAuthMiddleware(ws.cfg.AdminSessionSecret)
	adminRateLimiter := admin.NewRateLimiter(ws.cfg.AdminRateLimit)

	adminMux := http.NewServeMux()

	authHandler := admin.NewAuthHandler(oidcClient, ws.cfg.AdminSessionSecret, time.Duration(ws.cfg.AdminSessionTTLMinutes)*time.Minute)
	authHandler.RegisterRoutes(adminMux)

	adminHandler := admin.NewHandler(ws.dbClient, ws.dbClient, adminAuth)
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

	housekeepingHandler := admin.NewHousekeepingHandler(ec2Client, dynamoClient, ws.cfg.JobsTableName, ws.dbClient, adminAuth)
	housekeepingHandler.RegisterRoutes(adminMux)

	requeueHandler := admin.NewRequeueHandler(ec2Client, dynamoClient, ws.jobQueue, ws.metricsPublisher, ws.cfg.JobsTableName, adminAuth)
	requeueHandler.RegisterRoutes(adminMux)

	costPriceFetcher := cost.NewPriceFetcher(ws.awsCfg, ws.awsCfg.Region)
	costAdminHandler := admin.NewCostHandler(ws.dbClient, adminAuth, costPriceFetcher, ws.fleetManager)
	costAdminHandler.RegisterRoutes(adminMux)

	mux.Handle("/api/", adminRateLimiter.Wrap(adminMux))
	mux.Handle("/admin/", admin.UIHandler())

	mux.HandleFunc("/webhook", ws.handleWebhook)

	return mux, nil
}

// oidcRedirectURL returns the configured OIDC redirect URL, defaulting to
// the callback path under the orchestrator's own base URL.
func oidcRedirectURL(cfg *config.Config) string {
	if cfg.OIDCRedirectURL != "" {
		return cfg.OIDCRedirectURL
	}
	return strings.TrimSuffix(cfg.BaseURL, "/") + "/api/auth/callback"
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

	processed, response, postAck := ws.processWebhookEvent(r.Context(), payload)

	w.WriteHeader(http.StatusOK)
	if processed {
		_, _ = fmt.Fprintf(w, "%s\n", response)
	} else {
		_, _ = fmt.Fprintf(w, "Event acknowledged\n")
	}

	ws.runPostAck(postAck)
}

// runPostAck executes best-effort work that must not count against GitHub's 10s
// delivery budget. It runs on a background context because the request context
// is canceled once the handler returns, and recovers panics so a failed
// observability call never crashes the server.
func (ws *webhookServer) runPostAck(postAck func(context.Context)) {
	if postAck == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				webhookLog.Error(context.Background(), "panic in post-ack webhook work",
					slog.Any("panic", r))
			}
		}()
		postAck(context.Background())
	}()
}

// processWebhookEvent performs the durable work for a webhook event (the SQS
// enqueue that must complete before acking GitHub) and returns a closure for the
// best-effort work (metrics, pool-demand notification, direct processing) to run
// after the ack. The closure is nil when there is nothing more to do.
func (ws *webhookServer) processWebhookEvent(ctx context.Context, payload interface{}) (bool, string, func(context.Context)) {
	event, ok := payload.(*github.WorkflowJobEvent)
	if !ok {
		return false, "", nil
	}

	switch event.GetAction() {
	case "queued":
		jobMsg, err := handler.HandleWorkflowJobQueued(ctx, event, ws.jobQueue, ws.dbClient, ws.labelAliasResolver)
		if err != nil || jobMsg == nil {
			return false, "", nil
		}
		postAck := func(bgCtx context.Context) {
			handler.PublishJobQueuedMetrics(bgCtx, ws.metricsPublisher, jobMsg)
			if jobMsg.Pool != "" && len(jobMsg.Pool) <= 63 && validPoolName.MatchString(jobMsg.Pool) && ws.poolNotifier != nil {
				ws.poolNotifier.NotifyPoolDemand(jobMsg.Pool)
			}
			worker.TryDirectProcessing(bgCtx, ws.directProcessor, ws.directProcessorSem, jobMsg)
		}
		return true, "Job queued", postAck

	case "completed":
		if event.GetWorkflowJob().GetConclusion() != "failure" {
			return false, "", nil
		}
		requeued, err := handler.HandleJobFailure(ctx, event, ws.jobQueue, ws.dbClient, ws.labelAliasResolver)
		if err != nil {
			failCtx := logging.ContextWithJob(ctx, event.GetWorkflowJob().GetID(), 0, event.GetRepo().GetFullName())
			webhookLog.Error(failCtx, "job failure handling failed",
				slog.String("error", err.Error()))
		}
		if requeued {
			return true, "Job requeued", func(bgCtx context.Context) {
				if err := ws.metricsPublisher.PublishJobRequeued(bgCtx, "job_failure"); err != nil {
					webhookLog.Error(bgCtx, "job requeued metric failed", slog.String("error", err.Error()))
				}
				if err := ws.metricsPublisher.PublishQueueDepth(bgCtx, "main", 1); err != nil {
					webhookLog.Error(bgCtx, "queue depth metric failed", slog.String("error", err.Error()))
				}
			}
		}
	}
	return false, "", nil
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

// githubJobCheckerAdapter adapts *gh.Client to housekeeping.GitHubJobChecker.
type githubJobCheckerAdapter struct {
	client *gh.Client
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
