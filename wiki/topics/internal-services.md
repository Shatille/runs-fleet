---
topic: Internal Services (handlers, validation, workers)
last_compiled: 2026-07-09
sources_count: 9
---

# Internal Services (handlers, validation, workers)

## Purpose [coverage: high -- 9 sources]

The `internal/` tree holds orchestrator-only helpers that are intentionally not part of the public `pkg/` API. Go's `internal/` rule prevents downstream importers from depending on these types, so the orchestrator can refactor them freely. Four subpackages live here:

- `internal/handler` — HTTP-level webhook processing (parsing GitHub `workflow_job` events, building queue messages, bootstrapping ephemeral pools, and deriving the job-startup latency observation from `in_progress` events).
- `internal/validation` — small input-validation utilities used at config time (currently only the Vault Kubernetes JWT path guard).
- `internal/worker` — the queue worker loops that dispatch jobs to compute backends. There is one entry point per dispatch path: `direct` (in-process) and `ec2` (spot/on-demand fleet creation), both of which can short-circuit through `warmpool` (assignment to pre-warmed instances). `naming.go` provides the runner-conditions string used in instance/runner identifiers. The K8s dispatch path (`worker/k8s.go`) was removed along with the K8s runner backend; see Key Decisions.
- `internal/awsobs` — smithy-go middleware attached to the shared AWS SDK config. It times every AWS operation for latency/failure metrics and enforces a per-operation timeout, giving the orchestrator a diagnostic signal for a wedged AWS connection instead of an opaque `context deadline exceeded`.

Together these packages form the "control glue" between the GitHub webhook on one side and the compute provider (`pkg/fleet`) and AWS SDK on the other.

## Architecture [coverage: high -- 9 sources]

**Webhook intake (`internal/handler/webhook.go`)**

`HandleWorkflowJobQueued` is the canonical entry point. It calls `gh.ParseLabelsWithAliases`, parses the run ID from the webhook payload (not the label), and (for `pool=` jobs) lazily creates an ephemeral pool via `EnsureEphemeralPool` before constructing a `queue.JobMessage` and enqueuing it. Enqueue metrics are deliberately deferred: `PublishJobQueuedMetrics` runs *after* the webhook handler returns so the durable work (label parse, pool ensure, SQS send) is on the response's critical path but observability is not. Failure-path handling — `HandleJobFailure` — re-queues failed jobs (capped at `maxJobRetries = 2`), forcing on-demand on retry.

The `in_progress` path (PR #387) follows the same split. `HandleWorkflowJobInProgress` is a pure filter — no I/O, no context — that returns a `StartupObservation` (job ID, pool, GitHub-clock created-to-started seconds) only when the runner name carries the `runs-fleet-` prefix AND the labels parse; it returns nil on a zero timestamp or non-positive span. `PublishJobStartupMetrics` then runs post-ack: it resolves the source (`warm_pool` / `cold_start` from `events.JobInfo.WarmPoolHit`) via one `GetJobByJobID` read through the `JobStartupDB` interface and publishes `JobStartupSeconds`.

**Worker loop pattern (`internal/worker/common.go`)**

`RunWorkerLoop` (and its variants `RunWorkerLoopWithObserver`, `RunWorkerLoopWithObservers`, `RunWorkerLoopWithTicker`) is the single concurrency primitive shared by every dispatch path. Each worker:

- Polls the queue on a 25-second idle ticker (`idlePollInterval`), but keeps draining without waiting for the next tick as long as `ReceiveMessages` returns full batches (`drainQueue`), so a deep backlog is bounded by processing throughput, not by tick cadence.
- Caps in-flight processing with a buffered semaphore (`maxConcurrency = 5`).
- Tracks active goroutines with a `sync.WaitGroup` and waits for them in a `defer` so shutdown is clean.
- Wraps each message handler in `recover()` so a panic in one job does not kill the worker, and reports "ok"/"error" per message via an optional `ProcessObserver`.
- Bounds per-message work with `config.MessageProcessTimeout`, applied on a context that is deliberately detached from the loop's parent (`context.WithoutCancel`) — see Key Decisions.
- Reports each receive outcome ("messages"/"empty"/"error") via an optional `ReceiveObserver`, used to drive queue-receive metrics.

`RunWorkerLoopWithTicker` exposes the same logic with an injectable tick channel for tests.

**Two dispatch paths, one short-circuit**

```
queue.Message ──► RunWorkerLoop ──► processor closure
                                       ├── processEC2Message  (worker/ec2.go)
                                       └── DirectProcessor    (worker/direct.go, no queue)
                                              │
                                              ▼ (job.Pool != "")
                                       WarmPoolAssigner.TryAssignToWarmPool (worker/warmpool.go)
```

`RunEC2Worker` calls `RunWorkerLoopWithObservers` bound to `EC2WorkerDeps`. The direct path skips the queue entirely: `TryDirectProcessing` is called from the webhook handler with a separate semaphore so a hot job can be launched before the SQS round-trip; the always-enqueued SQS copy still exists as the fallback if direct processing loses the race or fails.

**Warm pool short-circuit (`internal/worker/warmpool.go`)**

Both EC2 and direct paths consult `WarmPoolAssigner.TryAssignToWarmPool` before falling through to a fresh fleet. Assignment uses `PoolManager.ClaimAndStartPoolInstance`, which performs the DynamoDB conditional write that makes pool reuse safe across multiple Fargate tasks.

**Naming (`internal/worker/naming.go`)**

`BuildRunnerConditions` flattens the requested resource labels (`arch`, `cpu`, `ram`, `disk`, `families`, `gen`) into a single dash-joined string used inside runner names and EC2 `Name` tags.

**AWS SDK observability (`internal/awsobs/middleware.go`, `internal/awsobs/timeout.go`)**

Both files register `smithy-go` `InitializeMiddleware` on the shared `aws.Config` via `awsconfig.WithAPIOptions`, so every AWS client built from that config carries them:

- `Middlewares()` returns the API option for the observability middleware plus a `*Recorder`. The middleware (`observe.HandleInitialize`) times `next.HandleInitialize` end-to-end (spanning serialize, retry, signing, and the HTTP round-trip) and publishes `PublishAWSCallDuration` for every call, plus `PublishAWSCallFailure` (classified `"timeout"` or `"error"`) when the call fails. `Recorder` holds the `metrics.Publisher` behind an `atomic.Pointer` so the middleware can be registered before the publisher exists (during `aws.Config` construction) and later wired up via `SetPublisher`; until then it emits to `metrics.NoopPublisher{}`.
- `PerOperationTimeout(d, exempt)` returns a second API option (`perOpTimeout.HandleInitialize`) that derives a `context.WithTimeout(ctx, d)` sub-context per AWS operation, bounding any single wedged call so it cannot consume a whole per-message processing budget. Operations named in `exempt` bypass the timeout — the sole current use is SQS `ReceiveMessage`, which long-polls for `WaitTimeSeconds=20` and would otherwise be aborted every empty poll.

Both middlewares anchor themselves relative to the SDK's own `RegisterServiceMetadata` middleware (which seeds service/operation name into the context) and to each other: observability inserts itself immediately after `RegisterServiceMetadata`, and the timeout middleware inserts itself immediately after observability. This ordering keeps observability outermost (so it times a timeout abort too) while keeping both anchored after the point where `GetServiceID`/`GetOperationName` become readable. Each falls back to `middleware.Before` at the head of the Initialize step if its expected anchor is absent (e.g. a bare test stack).

## Talks To [coverage: high -- 9 sources]

- `pkg/queue` — `queue.Queue` interface, `queue.JobMessage`, `queue.Message`. Used by every worker for receive/delete/send and by the webhook for enqueue.
- `pkg/fleet` — `fleet.Manager`, `fleet.LaunchSpec`, `fleet.FlexibleSpec` for EC2 fleet creation and warm-pool spec matching.
- `pkg/pools` — `pools.Manager`, `pools.AvailableInstance`, `pools.ErrNoAvailableInstance` for warm pool claim/start/stop.
- `pkg/db` — `db.Client`, `db.JobRecord`, `db.PoolConfig`, sentinel errors `db.ErrJobAlreadyClaimed`, `db.ErrJobClaimExhausted`, and `db.ErrPoolAlreadyExists`. Used for job claims, ephemeral pool creation, and instance-claim release.
- `pkg/events` — `events.JobInfo` (carrying `WarmPoolHit`), the return type of `GetJobByJobID` consumed by `PublishJobStartupMetrics` to resolve the startup-metric source.
- `pkg/github` — `gh.ParseLabelsWithAliases`, `gh.JobConfig`, `gh.AliasResolver` for webhook label parsing.
- `pkg/runner` — `runner.Manager`, `runner.PrepareRunnerRequest`. Both EC2 and warm-pool paths call `PrepareRunner` to write SSM parameters before the agent boots.
- `pkg/metrics` — `metrics.Publisher` for queue depth, fleet size, warm-pool hits, scheduling failures, claim failures, message-deletion failures, job duration, job-wait latency, job-startup latency (`JobStartupSeconds`), and (via `internal/awsobs`) per-operation AWS call duration/failure.
- `pkg/config` — `config.Config`, `config.MessageProcessTimeout`, `config.MessageReceiveTimeout`, `config.CleanupTimeout`, `config.ShortTimeout`, plus subnet lists.
- `pkg/logging` — `logging.WithComponent` / `logging.ContextWithJob` for namespaced slog loggers (`webhook`, `worker`, `direct`, `ec2-worker`, `warmpool-assigner`) and job-identity propagation into deep AWS call logs.
- `pkg/tracing` — `tracing.Tracer()`, `tracing.ExtractTraceContext` / `InjectTraceContext` for W3C TraceContext propagation across the webhook → SQS → worker boundary.
- AWS SDK (`aws-sdk-go-v2/aws/middleware`, `smithy-go/middleware`) — `internal/awsobs` registers directly against the SDK's own Initialize step and reads `awsmiddleware.GetServiceID`/`GetOperationName`.

The `internal/handler` package is also imported by `internal/worker` (for `BuildRunnerLabel`), which is the only cross-internal dependency. `internal/awsobs` has no dependency on `internal/handler` or `internal/worker`; it is wired in wherever the shared `aws.Config` is constructed (`pkg/config`) and consumed transitively by every AWS client, including the ones `internal/worker` calls.

## API Surface [coverage: high -- 9 sources]

**`internal/handler`**

- `HandleWorkflowJobQueued(ctx, *github.WorkflowJobEvent, queue.Queue, *db.Client, *gh.AliasResolver) (*queue.JobMessage, error)`
- `PublishJobQueuedMetrics(ctx, metrics.Publisher, *queue.JobMessage)`
- `CapacityLabel(cpu int) string`
- `HandleJobFailure(ctx, *github.WorkflowJobEvent, queue.Queue, *db.Client, *gh.AliasResolver) (bool, error)`
- `HandleWorkflowJobInProgress(*github.WorkflowJobEvent, *gh.AliasResolver) *StartupObservation` — pure filter, no ctx/I/O
- `PublishJobStartupMetrics(ctx, metrics.Publisher, JobStartupDB, *StartupObservation)`
- `StartupObservation` struct (JobID, Pool, Seconds)
- `JobStartupDB` interface (`HasJobsTable`, `GetJobByJobID` returning `*events.JobInfo`); satisfied by `*db.Client`
- `EnsureEphemeralPool(ctx, PoolDBClient, *gh.JobConfig) error`
- `BuildRunnerLabel(*queue.JobMessage) string`
- `PoolDBClient` interface (`GetPoolConfig`, `CreateEphemeralPool`, `TouchPoolActivity`)
- Constants: `maxJobRetries = 2`, `runnerNamePrefix = "runs-fleet-"`

**`internal/validation`**

- `ValidateK8sJWTPath(path string) error` — rejects empty, non-absolute, or paths not under `/var/run/secrets/`. Configuration-time check only; does not resolve symlinks or stat the file.

**`internal/worker` — common**

- `MessageProcessor`, `ReceiveObserver`, `ProcessObserver` (function types)
- `RunWorkerLoop(ctx, name, queue.Queue, MessageProcessor)`
- `RunWorkerLoopWithObserver(ctx, name, queue.Queue, MessageProcessor, ReceiveObserver)`
- `RunWorkerLoopWithObservers(ctx, name, queue.Queue, MessageProcessor, ReceiveObserver, ProcessObserver)`
- `RunWorkerLoopWithTicker(ctx, name, queue.Queue, MessageProcessor, ReceiveObserver, ProcessObserver, <-chan time.Time)`

**`internal/worker` — direct**

- `DirectProcessor` struct (Fleet, Pool, Metrics, Runner, DB, Config, SubnetIndex, Queue, WarmPoolAssigner, CreateFleetFn, PrepareRunnersFn)
- `(*DirectProcessor).ProcessJobDirect(ctx, *queue.JobMessage) bool`
- `TryDirectProcessing(_, *DirectProcessor, chan struct{}, *queue.JobMessage)`
- `BuildOnDemandFallbackJob(*queue.JobMessage) *queue.JobMessage` (shared with `ec2.go`)

**`internal/worker` — ec2**

- `EC2WorkerDeps` struct (Queue, Fleet, Pool, Metrics, Runner, DB, Config, SubnetIndex, WarmPoolAssigner, CreateFleetFn, PrepareRunnersFn)
- `WarmPoolAssignerInterface` (for test injection)
- `RunEC2Worker(ctx, EC2WorkerDeps)`
- `SelectSubnet(*config.Config, *uint64) string`
- `CreateFleetWithRetry(ctx, fleetManagerInterface, *fleet.LaunchSpec) ([]string, error)`
- `SaveJobRecords(ctx, *db.Client, *queue.JobMessage, []string)`
- `PrepareRunners(ctx, RunnerPreparer, *queue.JobMessage, []string) []string`
- Tunables: `RetryDelay = 1s`, `FleetRetryBaseDelay = 2s`, `maxDeleteRetries = 3`, `maxFleetCreateRetries = 3`, `maxJobRetries = 2`

**`internal/worker` — warmpool**

- `WarmPoolAssigner` struct (Pool, Runner, DB)
- `WarmPoolResult` struct (Assigned, InstanceID)
- `(*WarmPoolAssigner).TryAssignToWarmPool(ctx, *queue.JobMessage) (*WarmPoolResult, error)`
- Interfaces: `PoolManager`, `RunnerPreparer`, `JobDBClient`

**`internal/worker` — naming**

- `BuildRunnerConditions(*queue.JobMessage) string` — emits `arch-cpuN-ramN-diskN-families-genN`, omitting any unset field.

**`internal/awsobs`**

- `Middlewares() ([]func(*middleware.Stack) error, *Recorder)` — the API option to pass to `awsconfig.WithAPIOptions`, plus the `Recorder` to wire up once the metrics publisher exists.
- `(*Recorder).SetPublisher(metrics.Publisher)` — installs the real publisher; safe to call after the middleware is already attached.
- `NewRecorder() *Recorder` — recorder that emits to a no-op publisher until `SetPublisher` is called.
- `PerOperationTimeout(d time.Duration, exempt map[string]bool) func(*middleware.Stack) error` — second API option enforcing a per-call timeout, bypassed for operation names in `exempt`.

## Data [coverage: high -- 9 sources]

**Runner naming**

- `runnerNamePrefix = "runs-fleet-"` (declared in `handler/webhook.go`). Both `HandleJobFailure` and `HandleWorkflowJobInProgress` use this prefix to recognise jobs this system owns when filtering webhook events that fire for every runner in a repo.
- `BuildRunnerConditions` produces the suffix portion. Format: `arch-cpu<min>-ram<min>-disk<gib>-<family1>-<family2>-gen<n>`. Only set fields are included; the assembled string is dash-joined with no leading separator. Empty job specs produce an empty string.
- `BuildRunnerLabel` returns the original webhook label when present; otherwise reconstructs `runs-fleet=<run_id>[/pool=<name>][/spot=false]`. This is what GitHub sees during JIT registration.

**Runner-name length**

The runner name is assembled by upstream callers as `runs-fleet-<conditions>-<short-job-id>`. GitHub's registration limit of 64 characters constrains this; the conditions string from `BuildRunnerConditions` is the largest variable component, so it deliberately uses short tokens (`cpu4`, `ram8`, `gen8`) instead of human-readable forms.

**Job message construction**

`HandleWorkflowJobQueued` copies every flexible-spec field from `gh.JobConfig` into `queue.JobMessage`: `InstanceType`, `InstanceTypes`, `Pool`, `Spot`, `OriginalLabel`, `Arch`, `StorageGiB`, `Traceparent`, `CPUMin`/`CPUMax`, `RAMMin`/`RAMMax`, `Families`, `Gen`. The same shape is round-tripped through SQS and decoded by `processEC2Message`.

**Subnet round-robin**

`SelectSubnet` uses `atomic.AddUint64(subnetIndex, 1) - 1` to advance a shared counter, then takes `idx % len(subnets)`.

**AWS call metrics**

`internal/awsobs` emits per-call duration (`PublishAWSCallDuration`, dimensioned by AWS service ID and operation name) and a failure counter (`PublishAWSCallFailure`, classified `"timeout"` or `"error"`) for every AWS SDK call made through the shared `aws.Config`, with the single exception of CloudWatch calls (`cloudWatchServiceID`), which are skipped to avoid the CloudWatch publisher's own `PutMetricData` call recursively triggering another `AWSCallDuration` emission.

## Key Decisions [coverage: high -- 9 sources]

- **`internal/` over `pkg/`.** These packages are not part of the orchestrator's contract; placing them in `internal/` enforces that no external consumer can import them and gives the team freedom to refactor without API-stability concerns.

- **2026-06: K8s worker path removed.** `internal/worker/k8s.go` (and the `K8sWorkerDeps`/`RunK8sWorker`/`processK8sMessage`/`CreateK8sRunnerWithRetry` API it exposed) was deleted along with the K8s runner backend. Only `direct`, `ec2`, and `warmpool` dispatch paths remain in `internal/worker`. `internal/validation.ValidateK8sJWTPath` still exists (Vault Kubernetes-auth JWT path guard is unrelated to the runner backend) but is no longer paired with a K8s worker in this tree.

- **Direct processing optimisation.** `TryDirectProcessing` lets the webhook handler launch a job in-process when capacity permits, skipping SQS entirely for the common-case latency win. Capacity is bounded by an external semaphore; on overflow the message still goes via SQS, so no job is dropped. The direct path uses `context.Background()` (not the request context) because the goroutine outlives the HTTP handler.

- **Single concurrency primitive.** `RunWorkerLoop` (and its observer variants) is shared by the remaining dispatch path. Tunables (`maxConcurrency = 5`, `idlePollInterval = 25s`, `config.MessageProcessTimeout`) live in one file.

- **Per-message context is detached from the loop's parent.** `drainQueue` runs each processor on `context.WithoutCancel(ctx)` bounded by a fresh `config.MessageProcessTimeout`, rather than the loop's own (typically SIGTERM-driven) context. This is deliberate: on shutdown, the receive path still stops accepting new work via `ctx.Done()`, but an already-dispatched processor is allowed to run to completion instead of aborting mid-flight with "context canceled" and producing a rollout error burst. The deferred `activeWork.Wait()` still drains these processors before the worker returns.

- **DB-backed job claims.** Both `ProcessJobDirect` and `processEC2Message` call `db.ClaimJob` early. `db.ErrJobAlreadyClaimed` signals another orchestrator already took the job; `db.ErrJobClaimExhausted` signals the claim lease was re-acquired past its retry cap and the job is marked terminal instead of retried forever. On the "already claimed" error the EC2 path still deletes the SQS message (so the queue does not redrive forever) and counts it as a dedup, not a scheduling failure.

- **Path-traversal validation at config time.** `ValidateK8sJWTPath` runs during boot, before any Vault auth attempt. The comment explicitly documents that this is *not* runtime security: it is config hygiene meant to catch typos and prevent obviously-bad paths from being passed to Vault. Production trust still comes from kubelet-managed mounts.

- **Ephemeral pools created on demand.** `EnsureEphemeralPool` creates a 0-running / 1-stopped pool with a 30-minute idle timeout when a `pool=` label arrives for an unknown pool name. `db.ErrPoolAlreadyExists` is treated as a benign race (touch activity instead).

- **Webhook metrics deferred past the ack.** `HandleWorkflowJobQueued` does only durable work (label parse, ephemeral pool ensure, SQS send); `PublishJobQueuedMetrics` is called separately, after the webhook response, so a slow or failing metrics backend cannot delay GitHub's webhook delivery budget.

- **2026-07: startup latency measured on GitHub's clock (PR #387).** `HandleWorkflowJobInProgress` computes `JobStartupSeconds` from the webhook payload's own `created_at`/`started_at` — both stamped by GitHub — so orchestrator/GitHub clock skew cannot corrupt the span, and no local timestamp bookkeeping is needed. Two design constraints follow: (1) `in_progress` fires for *every* runner in a repo, including foreign ones, so the filter demands two-sided proof of ownership — the `runs-fleet-` runner-name prefix AND a successful `ParseLabelsWithAliases` — the same pair `HandleJobFailure` uses; (2) the filter is pure and the DB read + publish run post-ack via `PublishJobStartupMetrics`, keeping the webhook's durable path free of new I/O. On any source-lookup miss (nil db, no jobs table, error, missing record) the observation is published with an empty source rather than dropped, so dashboard totals stay complete. The counterpart instance-provision latency (launch → runner-ready) is deliberately *not* published from `internal/worker/ec2.go` — its old TODO was removed in the same PR; the launch and runner-ready timestamps live on different instances, so the metric is emitted from `pkg/termination`'s runner-confirmation path using the jobs DB record as the cross-instance rendezvous.

- **AWS SDK middleware as a diagnostic safety net, not a semantic layer.** `internal/awsobs` does not change AWS call behavior beyond enforcing a timeout; it purely times and classifies calls. It replaces an earlier per-call WARN log (which flooded logs on SQS `ReceiveMessage` long-polls) with a metric, trading per-call log noise for a queryable duration/failure signal dimensioned by service and operation.

- **Middleware self-anchoring instead of fixed stack position.** Both `awsobs` middlewares locate themselves relative to named middleware IDs (`RegisterServiceMetadata`, then each other) rather than assuming a fixed index in the Initialize step, so they remain correctly ordered even if the SDK's own middleware set changes, with a `middleware.Before` fallback if the expected anchor is missing.

## Gotchas [coverage: high -- 9 sources]

- **Spot fallback path is delicate.** When `CreateFleetWithRetry` fails on spot and retries are available, `handleOnDemandFallback` sends the on-demand requeue *before* deleting the original SQS message, so a failed requeue leaves the original for redelivery instead of losing the job. The direct path's symmetric `recoverFleetFailure` follows the same send-before-cleanup ordering, and marks the claim terminal (not silently dropped) when the job is no longer eligible for a fallback.

- **`ProcessJobDirect` returns `false` on "already claimed" or "claim exhausted".** Neither is a processing failure in the direct-path sense — the first means another worker won the race, the second means the queue path already marked the job terminal. Callers must not log either as an error. The semaphore in `TryDirectProcessing` is released regardless.

- **Receive-timeout math has to leave room for retries.** `drainQueue` bounds each `ReceiveMessages` call to the smaller of `config.MessageReceiveTimeout` and the remaining context deadline, long-polling up to 20 seconds (`receiveWaitSeconds`) for up to 10 messages (`maxMessagesPerReceive`). Combined with `config.MessageProcessTimeout` and the SQS visibility timeout configured in `pkg/queue`, tightening any one of these can cause duplicate processing.

- **Warm-pool failures stop the instance.** If `PrepareRunner` or `SaveJob` fails *after* `ClaimAndStartPoolInstance` succeeded, the instance is already booting. The assigner retries `StopPoolInstance` up to three times, then logs `manual cleanup required`. The DB claim is also released so a future job can pick the instance up again.

- **Subnet index pointer is shared.** `SelectSubnet` writes to a `*uint64` passed by reference. Workers and the direct processor must share the *same* pointer, or each path will round-robin independently and bias toward subnet zero.

- **Matrix jobs share `RunID`.** `BuildRunnerLabel` keys off `RunID` only, so two matrix jobs in the same workflow run produce identical labels. Disambiguation comes from the runner-name suffix (job ID), not the label.

- **`HandleJobFailure` over-filters intentionally.** It returns `false, nil` when the runner name does not start with `runs-fleet-`, when labels can't be parsed, or when no jobs table exists. None of these are errors — they are signals that the event was for a different system or that we have nothing to do.

- **`JobStartupDB` is nil-check-guarded but interface-typed.** `PublishJobStartupMetrics` tolerates a nil `dbc`, but callers holding a concrete `*db.Client` must guard the typed-nil trap before assigning it to the interface (a nil pointer in a non-nil interface passes `dbc != nil`); `cmd/server`'s `in_progress` case does exactly this. Also, an empty `source` dimension on `JobStartupSeconds` is not a bug — it is the deliberate degraded output when the job record cannot be resolved.

- **`maxJobRetries = 2` exists in two places.** Both `internal/handler/webhook.go` and `internal/worker/ec2.go` declare it. The intent is the same (cap retries at 2) but the constants are not shared; updating one without the other will silently desync the spot-fallback path from the failure-requeue path.

- **CloudWatch calls are invisible to `AWSCallDuration`.** `internal/awsobs` explicitly skips recording when the AWS service ID is `CloudWatch`, because the CloudWatch metrics backend publishes over the same instrumented `aws.Config`; recording it would make each `PutMetricData` call emit another metric about itself. Anyone debugging "why don't I see CloudWatch API latency in the AWS-call metrics" should check this exclusion first, not assume the middleware is broken.

- **The per-operation timeout exemption is name-based and order-sensitive.** `PerOperationTimeout`'s `exempt` map matches on `awsmiddleware.GetOperationName(ctx)`, which is only populated once `RegisterServiceMetadata` has run. If the timeout middleware were ever inserted ahead of that anchor, the operation name would read empty and the SQS `ReceiveMessage` exemption would silently stop matching, causing every long-poll to be aborted after `d`.

## Sources [coverage: high]

- [internal/handler/webhook.go](../../internal/handler/webhook.go)
- [internal/validation/path.go](../../internal/validation/path.go)
- [internal/worker/common.go](../../internal/worker/common.go)
- [internal/worker/direct.go](../../internal/worker/direct.go)
- [internal/worker/ec2.go](../../internal/worker/ec2.go)
- [internal/worker/naming.go](../../internal/worker/naming.go)
- [internal/worker/warmpool.go](../../internal/worker/warmpool.go)
- [internal/awsobs/middleware.go](../../internal/awsobs/middleware.go)
- [internal/awsobs/timeout.go](../../internal/awsobs/timeout.go)
