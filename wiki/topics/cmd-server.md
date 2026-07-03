---
topic: Server (Orchestrator Entry Point)
last_compiled: 2026-07-03
sources_count: 6
---

# Server (Orchestrator Entry Point)

## Purpose [coverage: medium -- 2 sources]
The `cmd/server` binary is the long-running Fargate orchestrator daemon. It receives GitHub webhooks at `/webhook`, enqueues jobs onto SQS, starts EC2 fleets, reconciles warm pools, handles spot interruption events, runs housekeeping tasks, and exposes the admin API plus the GitHub Actions cache protocol. The backend is EC2-only — there is no runtime branch on a Kubernetes provider. Multiple Fargate replicas may run concurrently; coordination uses per-pool DynamoDB locks rather than global leader election.

## Architecture [coverage: medium -- 3 sources]
Boot sequence in `main()` ([cmd/server/main.go](../../cmd/server/main.go)):

1. `logging.Init()` and create a SIGTERM/SIGINT-cancelled root context via `signal.NotifyContext`.
2. `config.Load()` reads env vars; `gh.ParseAliasRules(cfg.LabelAliasesJSON)` builds the label-alias resolver.
3. `tracing.Setup(ctx, cfg.Tracing)` installs the OpenTelemetry tracer provider (noop when tracing is disabled).
4. `awsobs.Middlewares()` builds AWS SDK observability middleware; `awsconfig.LoadDefaultConfig` creates the shared `aws.Config` with a custom HTTP client, retryer, and a per-operation timeout that exempts SQS `ReceiveMessage` (its 20s long-poll exceeds `AWSPerOpTimeout`). A second `sqsCfg` clones `awsCfg` with a longer response-header timeout (`AWSSQSResponseHeaderTimeout`) so long-polling doesn't trip the header-timeout on the shared client.
5. Initialize core deps: `initJobQueue` (SQS), `db.NewClient` (plus optional GSIs), `cache.NewServer`, `initMetrics` (installs the publisher into the AWS observability recorder).
6. Build EC2-backend deps unconditionally: `fleet.NewManager`, `pools.NewManager`, `events.NewHandler(eventsQueueClient, ...)`, and an `ec2.NewFromConfig` client wired into the pool manager.
7. `initCircuitBreaker` (only if `cfg.CircuitBreakerTable` is set and the fleet manager exists) wires `circuit.Breaker` into both fleet and event handler.
8. `initSecretsStore` builds the secrets backend (SSM or Vault); optional `termination.NewHandler` if `cfg.TerminationQueueURL` is set.
9. `initGitHubClient(cfg)` builds a single `*gh.Client` (nil if GitHub App credentials are absent) and is threaded into both `initHousekeeping` and `initRunnerManager`.
10. `initHousekeeping` builds the `housekeeping.Runner` (nil if no pools table); `initRunnerManager` builds `runner.Manager` from the same shared GitHub client (nil if the client is nil).
11. Build `worker.DirectProcessor` and a 10-slot semaphore (`directProcessorSem`) for synchronous fast-path processing.
12. Construct the `webhookServer` struct and call `setupHTTPRoutes` to register handlers on a `http.ServeMux`.
13. Start `http.Server` on `:8080` (15s read/write, 60s idle timeout).
14. Spawn goroutines via a shared `sync.WaitGroup`: the EC2 worker (`worker.RunEC2Worker`), `poolManager.ReconcileLoop`, `eventHandler.Run`, optional `terminationHandler.Run`, optional `housekeepingRunner.Run`, and the HTTP listener (outside the WaitGroup).
15. Block on `<-ctx.Done()`, then: `server.Shutdown` (stop accepting new HTTP/webhook traffic), `waitForWorkers` (drains the WaitGroup, bounded by `workerDrainTimeout = MessageProcessTimeout + 10s`), `tracing.Shutdown`, `metricsPublisher.Close`.

`internal/worker/common.go` provides the shared polling primitive `RunWorkerLoopWithTicker` (wrapped by `RunWorkerLoopWithObservers`/`RunWorkerLoop`): an `idlePollInterval` (25s) ticker triggers `drainQueue`, which calls `q.ReceiveMessages(ctx, maxMessagesPerReceive=10, receiveWaitSeconds=20)` and keeps re-receiving while batches come back full (so a backlog isn't throttled to one batch per tick). Each message is dispatched to a goroutine bounded by a `maxConcurrency = 5` semaphore, tracked by a `sync.WaitGroup` so shutdown waits for in-flight work. Each processor runs under `context.WithTimeout(context.WithoutCancel(ctx), config.MessageProcessTimeout)` — detached from the signal context so an already-dispatched processor finishes instead of aborting on SIGTERM — with a panic recover that reports `"error"` to the process observer.

## Talks To [coverage: medium -- 2 sources]
Internal packages imported by `main.go`: `internal/awsobs`, `internal/handler`, `internal/worker`, `pkg/admin`, `pkg/cache`, `pkg/circuit`, `pkg/config`, `pkg/cost`, `pkg/db`, `pkg/events`, `pkg/fleet`, `pkg/github` (aliased `gh`), `pkg/housekeeping`, `pkg/logging`, `pkg/metrics`, `pkg/pools`, `pkg/queue`, `pkg/runner`, `pkg/secrets`, `pkg/termination`, `pkg/tracing`. There is no import of a `pkg/provider` package — the Kubernetes provider abstraction was removed upstream (see Key Decisions).

AWS SDK clients constructed: `dynamodb.NewFromConfig`, `ec2.NewFromConfig` (one in `main`, one in `setupHTTPRoutes` for admin handlers), `sqs.NewFromConfig` (admin queue handlers).

External: GitHub API via `github.com/google/go-github/v57/github` (event types) and `pkg/github.Client` (aliased `gh`) for App-auth installation tokens (`cfg.GitHubAppID` / `cfg.GitHubAppPrivateKey`); Vault optionally via the secrets backend.

## API Surface [coverage: high -- 2 sources]
HTTP routes registered in `webhookServer.setupHTTPRoutes` on port `:8080`:

- `GET /health` — always returns 200 OK.
- `GET /ready` — calls `queue.Pinger.Ping` on the job queue with a 2s timeout, 503 on failure (`handleReadiness`).
- `GET <cfg.MetricsPrometheusPath>` — Prometheus scrape endpoint, only when Prometheus backend is enabled.
- Cache protocol routes via `cache.NewHandlerWithAuth(...).RegisterRoutes(mux)` (HMAC-authenticated with `cfg.CacheSecret`).
- Admin handlers under `/api/` (rate-limited, gated by `admin.NewAuthMiddleware(cfg.AdminSecret)`): `admin.NewHandler`, `admin.NewJobsHandler`, `admin.NewInstancesHandler`, `admin.NewQueuesHandler` (configured with main / DLQ / pool / events / termination / housekeeping queue URLs), `admin.NewCircuitHandler`, `admin.NewHousekeepingHandler`, `admin.NewRequeueHandler`, `admin.NewCostHandler`.
- `GET /admin/` — admin web UI via `admin.UIHandler()`.
- `POST /webhook` — `handleWebhook`: validates HMAC via `gh.ParseWebhook(r, cfg.GitHubWebhookSecret)`, then `processWebhookEvent` performs durable work synchronously and returns a `postAck` closure for best-effort work run after the response is written (`runPostAck`, on a background context, panic-recovered):
  - `action=queued` → `handler.HandleWorkflowJobQueued` enqueues a `queue.JobMessage`; postAck publishes metrics, notifies the pool manager (`NotifyPoolDemand`) if a valid `pool` label is present, and calls `worker.TryDirectProcessing` under the 10-slot semaphore for the synchronous fast path.
  - `action=completed` with `conclusion=failure` → `handler.HandleJobFailure` may re-queue (capped at `maxJobRetries = 2` inside the handler package, only for runner names prefixed `runs-fleet-`, sets `ForceOnDemand: true`); postAck publishes requeue/queue-depth metrics.

## Data [coverage: low -- 0 sources]
Not applicable. The server owns no persistent state directly. All state lives in the DynamoDB tables managed by `pkg/db` (`runs-fleet-jobs`, `runs-fleet-pools`, `runs-fleet-circuit-state`), S3 (`runs-fleet-cache`), and SQS queues. See [state-storage](./state-storage.md).

## Key Decisions [coverage: medium -- 4 sources]
- **2026-07: shared GitHub client.** `initGitHubClient(cfg)` builds a single `*gh.Client` once in `main`, and the same pointer is passed to both `initRunnerManager` and `initHousekeeping` (which wraps it in a `githubJobCheckerAdapter` for the stale-job checker). Previously the runner manager and housekeeping's stale-job checker each constructed their own GitHub client independently, so each maintained its own per-owner installation-token cache and minted its own tokens for the same App installation. Sharing one client means a token cached for an owner by one consumer (e.g. the runner manager registering a runner) is reused by the other (e.g. housekeeping checking a stale job in the same owner) instead of being fetched twice.
- **2026-07: `pkg/github` relocation.** The GitHub App client (`NewClient`, webhook parsing, alias rules) lives in `pkg/github`, imported in `main.go` under the alias `gh`. The old `runner.NewGitHubClient` symbol referenced by earlier versions of this article no longer exists — `pkg/runner` now only builds a `runner.Manager` and takes a `*gh.Client` as a constructor argument.
- **2026-06: Kubernetes backend removed.** `pkg/provider` (and its `k8s.Provider` / `k8s.PoolProvider` implementations) no longer exist. `main.go` no longer branches on `cfg.IsK8sBackend()` / `cfg.IsEC2Backend()`; it unconditionally builds the EC2 path and starts `worker.RunEC2Worker` plus `poolManager.ReconcileLoop`. There is no more `worker.RunK8sWorker` or Valkey-backed queue selection in this file.
- Single binary, multiple goroutines: one EC2 worker, plus pool reconciler, events handler, optional termination handler, and optional housekeeping runner — each launched via a shared `runWorker` closure onto a `sync.WaitGroup`, so shutdown can wait for all of them uniformly.
- Multi-instance Fargate without leader election: housekeeping and pool reconciliation use DynamoDB locks (`r.SetTaskLocker(dbClient, uuid.NewString())` — one random instance ID per replica).
- Fast-path direct processing: when the webhook arrives, `worker.TryDirectProcessing` opportunistically processes the job inline using `worker.DirectProcessor` and a 10-slot semaphore, bypassing SQS round-trip latency for the first job. It runs on `context.Background()` with its own `MessageProcessTimeout`, since the HTTP request context is gone by the time the postAck closure runs.
- Pluggable metrics: `initMetrics` composes CloudWatch, Prometheus, and Datadog publishers via `metrics.NewMultiPublisher`; falls back to `metrics.NoopPublisher{}` when none enabled. Default namespaces: `RunsFleet` (CloudWatch) / `runs_fleet` (Prometheus, Datadog). The publisher is also installed into the AWS SDK observability recorder after construction, since the CloudWatch backend itself depends on the AWS config that carries the observability middleware.
- Worker concurrency model (`internal/worker/common.go`): batch size 10, long-poll 20s, idle re-poll ticker 25s, `maxConcurrency = 5` per worker, each message scoped to `config.MessageProcessTimeout` on a context detached from the shutdown signal so in-flight work survives SIGTERM.
- Graceful shutdown ordering: HTTP server stops first (no new webhook traffic), then workers drain (bounded by `workerDrainTimeout`), then tracing and metrics flush — so in-flight jobs' final spans/metrics are captured rather than the process exiting mid-flight.

## Gotchas [coverage: medium -- 3 sources]
- Worker drain timeout (`workerDrainTimeout = MessageProcessTimeout + 10s`) must stay below the pod's `terminationGracePeriodSeconds` or the platform SIGKILLs mid-drain.
- Receive errors from a cancelled/deadline-exceeded context are logged at `Warn`, not `Error`, to avoid noisy shutdown logs; other receive errors are `Error` (`internal/worker/common.go`).
- Direct-processing semaphore is sized to 10; bursts beyond that fall through to normal queue processing (no error, just skipped fast path).
- Webhook handler always responds 200 OK for any recognized event even when no further action is taken — relies on `gh.ParseWebhook` for HMAC/parse rejection at 400.
- Job re-queue path (`HandleJobFailure`) requires the runner name to begin with `runs-fleet-`; failures for other runner names are silently skipped. Max retries is enforced inside `internal/handler`, not in `main.go`.
- Circuit breaker is only wired when `cfg.CircuitBreakerTable` is set and the EC2 fleet manager exists (`initCircuitBreaker` early-returns otherwise).
- Termination and housekeeping are gated on config: termination requires `cfg.TerminationQueueURL`; housekeeping requires a pools table name (`cfg.PoolsTableName`) since it needs the table for task locking. Missing config silently disables them (nil handler, not started).
- Both the runner manager and housekeeping's GitHub job checker are skipped (nil) if GitHub App credentials (`cfg.GitHubAppID` / `cfg.GitHubAppPrivateKey`) are not configured — downstream callers must tolerate a nil `*runner.Manager`, and `housekeeping.Tasks.SetGitHubJobChecker` is only called when the shared client is non-nil.
- SQS `ReceiveMessage` calls use a dedicated `sqsCfg` (longer response-header timeout) precisely because its 20s long-poll wait would otherwise exceed both `AWSResponseHeaderTimeout` and `AWSPerOpTimeout` on the default `awsCfg`; other SQS operations (`SendMessage`, `DeleteMessage`) are fast enough to stay on the per-operation-timeout-bounded path.

## Sources [coverage: high]
- [cmd/server/main.go](../../cmd/server/main.go)
- [internal/handler/webhook.go](../../internal/handler/webhook.go)
- [internal/worker/common.go](../../internal/worker/common.go)
- [internal/worker/ec2.go](../../internal/worker/ec2.go)
- [internal/worker/direct.go](../../internal/worker/direct.go)
- [pkg/github/client.go](../../pkg/github/client.go)
