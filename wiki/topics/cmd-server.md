---
topic: Server (Orchestrator Entry Point)
last_compiled: 2026-04-30
sources_count: 3
---

# Server (Orchestrator Entry Point)

## Purpose [coverage: medium -- 3 sources]
The `cmd/server` binary is the long-running Fargate orchestrator daemon. It receives GitHub webhooks at `/webhook`, processes the job queue (SQS for EC2 backend, Valkey streams for K8s), starts EC2 fleets, reconciles warm pools, handles spot interruption events, runs housekeeping tasks, and exposes the admin API plus the GitHub Actions cache protocol. Multiple Fargate replicas may run concurrently; coordination uses per-pool DynamoDB locks rather than global leader election.

## Architecture [coverage: high -- 3 sources]
Boot sequence in `main()` (cmd/server/main.go):

1. `logging.Init()` and create a SIGTERM/SIGINT-cancelled root context via `signal.NotifyContext`.
2. `config.Load()` reads env vars; `awsconfig.LoadDefaultConfig` creates the shared `aws.Config`.
3. Initialize core deps: `initJobQueue` (SQS or Valkey), `db.NewClient`, `cache.NewServer`, `initMetrics`.
4. Branch on backend:
   - `cfg.IsK8sBackend()`: build `k8s.Provider` and `k8s.PoolProvider`.
   - else (EC2): build `fleet.NewManager`, `pools.NewManager`, `events.NewHandler`, plus EC2 client wired into the pool manager.
5. `initCircuitBreaker` (if `CircuitBreakerTable` set) wires `circuit.Breaker` into both fleet and event handler.
6. EC2 only: `initSecretsStore` (`secrets.NewStore`), optional `termination.NewHandler`, and `initHousekeeping` (handler + scheduler).
7. `initRunnerManager` builds `runner.Manager` from a GitHub App.
8. EC2 only: build `worker.DirectProcessor` and a 10-slot semaphore (`directProcessorSem`) for synchronous fast-path processing.
9. Construct `webhookServer` struct and call `setupHTTPRoutes` to register handlers on a `http.ServeMux`.
10. Start `http.Server` on `:8080` (15s read/write, 60s idle timeout).
11. Spawn goroutines: backend worker (`worker.RunK8sWorker` or `worker.RunEC2Worker`), pool reconciler (`poolManager.ReconcileLoop` or `k8sPoolProvider.ReconcileLoop`), `eventHandler.Run`, optional `terminationHandler.Run`, `housekeepingHandler.Run`, `housekeepingScheduler.Run`, and the HTTP listener.
12. Block on `<-ctx.Done()`, then run `server.Shutdown` with `config.MessageProcessTimeout` and `metricsPublisher.Close`.

`internal/worker/common.go` provides the shared polling primitive `RunWorkerLoop`/`RunWorkerLoopWithTicker`: a 25-second ticker drives `q.ReceiveMessages(ctx, 10, 20)` (batch 10, 20s long-poll), and each message is dispatched to a `MessageProcessor` goroutine bounded by a 5-slot semaphore (`maxConcurrency = 5`) and protected by a `sync.WaitGroup` so shutdown waits for in-flight work. Each processor runs under `context.WithTimeout(ctx, config.MessageProcessTimeout)` and a panic recover.

## Talks To [coverage: high -- 1 source]
Internal packages imported by `main.go`: `internal/handler`, `internal/worker`, `pkg/admin`, `pkg/cache`, `pkg/circuit`, `pkg/config`, `pkg/cost`, `pkg/db`, `pkg/events`, `pkg/fleet`, `pkg/github` (aliased `gh`), `pkg/housekeeping`, `pkg/logging`, `pkg/metrics`, `pkg/pools`, `pkg/provider/k8s`, `pkg/queue`, `pkg/runner`, `pkg/secrets`, `pkg/termination`.

AWS SDK clients constructed: `dynamodb.NewFromConfig`, `ec2.NewFromConfig`, `sqs.NewFromConfig` (used by admin handlers and queue clients).

External: GitHub API via `github.com/google/go-github/v57/github` and `runner.NewGitHubClient` (App auth using `cfg.GitHubAppID` / `cfg.GitHubAppPrivateKey`); Valkey/Redis via `queue.NewValkeyClient` for the K8s backend; Vault optionally via the secrets backend.

## API Surface [coverage: high -- 2 sources]
HTTP routes registered in `webhookServer.setupHTTPRoutes` on port `:8080`:

- `GET /health` — always returns 200 OK.
- `GET /ready` — calls `queue.Pinger.Ping` on the job queue with a 2s timeout, 503 on failure (`handleReadiness`).
- `GET <cfg.MetricsPrometheusPath>` — Prometheus scrape endpoint, only when Prometheus backend is enabled.
- Cache protocol routes via `cache.NewHandlerWithAuth(...).RegisterRoutes(mux)` (HMAC-authenticated with `cfg.CacheSecret`).
- Admin handlers (all gated by `admin.NewAuthMiddleware(cfg.AdminSecret)`): `admin.NewHandler`, `admin.NewJobsHandler`, `admin.NewInstancesHandler`, `admin.NewQueuesHandler` (configured with main / DLQ / pool / events / termination / housekeeping queue URLs), `admin.NewCircuitHandler`, `admin.NewHousekeepingHandler`.
- `GET /admin/` — admin web UI via `admin.UIHandler()`.
- `POST /webhook` — `handleWebhook`: validates HMAC via `gh.ParseWebhook(r, cfg.GitHubWebhookSecret)`, dispatches `*github.WorkflowJobEvent` in `processWebhookEvent`:
  - `action=queued` → `handler.HandleWorkflowJobQueued` enqueues a `queue.JobMessage`, then `worker.TryDirectProcessing` attempts the synchronous fast path under the 10-slot semaphore.
  - `action=completed` with `conclusion=failure` → `handler.HandleJobFailure` may re-queue (capped at `maxJobRetries = 2`, only for runner names prefixed `runs-fleet-`, sets `ForceOnDemand: true`).

## Data [coverage: low -- 0 sources]
Not applicable. The server owns no persistent state directly. All state lives in the DynamoDB tables managed by `pkg/db` (`runs-fleet-jobs`, `runs-fleet-pools`, `runs-fleet-circuit-state`), S3 (`runs-fleet-cache`), and SQS / Valkey queues. See [state-storage](./state-storage.md).

## Key Decisions [coverage: high -- 3 sources]
- Single binary, multiple goroutines: one main worker per backend, plus pool reconciler, events handler, termination handler, housekeeping handler, and housekeeping scheduler — each as a top-level `go ...` in `main`.
- Multi-instance Fargate without leader election: housekeeping and pool reconciliation use DynamoDB locks (`h.SetTaskLocker(dbClient, instanceID)` with a `uuid.NewString()` per replica).
- Backend abstraction: `cfg.IsK8sBackend()` / `cfg.IsEC2Backend()` switch between `worker.RunK8sWorker` (+ `k8sPoolProvider.ReconcileLoop`) and `worker.RunEC2Worker` (+ `poolManager.ReconcileLoop` + `eventHandler.Run`).
- Fast-path direct processing: when the EC2 webhook arrives, `worker.TryDirectProcessing` opportunistically processes the job inline using `worker.DirectProcessor` and a 10-slot semaphore, bypassing SQS round-trip latency for the first job.
- Pluggable metrics: `initMetrics` composes CloudWatch, Prometheus, and Datadog publishers via `metrics.NewMultiPublisher`; falls back to `metrics.NoopPublisher{}` when none enabled. Default namespaces: `RunsFleet` (CloudWatch) / `runs_fleet` (Prometheus, Datadog).
- Worker concurrency model (worker/common.go): batch size 10, long-poll 20s, ticker 25s, `maxConcurrency = 5` per worker, each message scoped to `config.MessageProcessTimeout`.
- Graceful shutdown: signal-driven cancellation propagates to all goroutines; `server.Shutdown` and worker `WaitGroup` ensure in-flight work completes before exit.

## Gotchas [coverage: medium -- 2 sources]
- ECS task draining: SIGTERM cancels the root context but the shutdown timeout is `config.MessageProcessTimeout` — long-running message processors can be cut off if they exceed it.
- Receive errors from a cancelled context are logged at `Warn`, not `Error`, to avoid noisy shutdown logs (`worker/common.go`).
- Direct-processing semaphore is sized to 10; bursts beyond that fall through to normal queue processing (no error, just skipped fast path).
- Webhook handler always responds 200 OK even when the event is ignored (non-`workflow_job`, unrecognized action) — relies on `gh.ParseWebhook` for HMAC rejection at 400.
- Job re-queue path (`HandleJobFailure`) requires the runner name to begin with `runs-fleet-` and the job table to be present (`dbc.HasJobsTable()`); otherwise failures are silently skipped. Max retries is 2.
- Circuit breaker is only wired when `cfg.CircuitBreakerTable` is set and the EC2 fleet manager exists (`initCircuitBreaker` early-returns otherwise).
- Termination, housekeeping handler, and housekeeping scheduler are EC2-only and additionally require the relevant queue URL (`TerminationQueueURL`, `HousekeepingQueueURL`); missing config silently disables them.
- Runner manager is skipped (returns nil) if GitHub App credentials are not configured — downstream callers must tolerate a nil manager.

## Sources [coverage: high]
- [cmd/server/main.go](../../cmd/server/main.go)
- [internal/handler/webhook.go](../../internal/handler/webhook.go)
- [internal/worker/common.go](../../internal/worker/common.go)
