---
topic: Internal Services (handlers, validation, workers)
last_compiled: 2026-08-21
sources_count: 9
---

# Internal Services (handlers, validation, workers)

## Purpose [coverage: high -- 9 sources]

The `internal/` tree holds orchestrator-only helpers that are intentionally not part of the public `pkg/` API. Go's `internal/` rule prevents downstream importers from depending on these types, so the orchestrator can refactor them freely. Four subpackages live here:

- `internal/handler` — HTTP-level webhook processing: parsing GitHub `workflow_job` events, building queue messages, bootstrapping ephemeral pools, deriving the job-startup latency observation from `in_progress`, requeueing on `failure`, and cleaning up on `cancelled`.
- `internal/validation` — small input-validation utilities used at config time (currently only the Vault Kubernetes JWT path guard).
- `internal/worker` — the queue worker loops that dispatch jobs. There is one entry point per dispatch path: `direct` (in-process, webhook-triggered) and `ec2` (SQS-driven fleet creation), both of which can short-circuit through `warmpool` (assignment to pre-warmed instances). `naming.go` provides the runner-conditions string used in instance/runner identifiers. The K8s dispatch path (`worker/k8s.go`) was removed along with the K8s runner backend; see Key Decisions.
- `internal/awsobs` — smithy-go middleware attached to the shared AWS SDK config. It times every AWS operation for latency/failure metrics and enforces a per-operation timeout, giving the orchestrator a diagnostic signal for a wedged AWS connection instead of an opaque `context deadline exceeded`.

Together these packages form the "control glue" between the GitHub webhook on one side and EC2 (`pkg/fleet`) plus the AWS SDK on the other.

## Architecture [coverage: high -- 9 sources]

**Webhook intake ([internal/handler/webhook.go](../../internal/handler/webhook.go))**

The package splits every webhook action into a *durable* half that must complete before GitHub is acked and a *best-effort* half that runs after (see [cmd-server](cmd-server.md) for the `processWebhookEvent` / `runPostAck` mechanics).

`HandleWorkflowJobQueued` is the canonical entry point: it opens a `webhook.process` span, calls `gh.ParseLabelsWithAliases`, reads `run_id` **from the webhook payload** (the legacy label's run_id segment is ignored — [webhook.go:66](../../internal/handler/webhook.go)), lazily creates an ephemeral pool via `EnsureEphemeralPool` for `pool=` jobs, then constructs the `queue.JobMessage` (copying every flexible-spec field plus `Traceparent` from `tracing.InjectTraceContext`) and enqueues it. Enqueue metrics live in the separate `PublishJobQueuedMetrics`, called post-ack.

The post-ack chain for a `queued` job is three steps, in order: `PublishJobQueuedMetrics` → **`NotifyPoolDemand`** → `worker.TryDirectProcessing`. `NotifyPoolDemand` is *not* implemented here — `cmd/server` declares the one-method `PoolReconcileNotifier` interface and satisfies it with `*pools.Manager`, calling it only when the pool name is non-empty, ≤63 chars, and matches `^[a-zA-Z0-9][a-zA-Z0-9_-]*$`. It pokes the pool reconciler so a warm-pool job does not wait out the 60s tick.

Three more actions are handled:

- `in_progress` → `HandleWorkflowJobInProgress` is a pure filter (no ctx, no I/O) returning a `StartupObservation` (job id, pool, GitHub-clock created→started seconds) only when the runner name carries the `runs-fleet-` prefix AND the labels parse; nil on a zero timestamp or non-positive span. `PublishJobStartupMetrics` then resolves the source (`warm_pool`/`cold_start` from `events.JobInfo.WarmPoolHit`) with one `GetJobByJobID` read through the `JobStartupDB` interface and publishes `JobStartupSeconds`.
- `completed` + `failure` → `HandleJobFailure` re-queues (capped at `maxJobRetries = 2`), gated on the `runs-fleet-` runner-name prefix, a parseable label set, a jobs table, a non-nil record, and a non-zero `RunID`/`Repo`. The `MarkJobRequeuedByJobID` conditional write decides ownership; the requeue message carries `Spot=false`, `ForceOnDemand=true`, `RetryCount+1`, and `OriginalLabel`.
- `completed` + `cancelled` → `HandleJobCancelled` retires a job GitHub cancelled before its runner ever confirmed (a superseded PR pipeline, or an operator cancelling a queued job) — otherwise the record stays `launched` forever and the unconfirmed-runner watchdog eventually provisions a fresh runner for work that no longer exists. Only the `launched` case is claimed: the conditional `MarkJobCancelled` flip decides ownership, and losing it means the runner confirmed concurrently and owns its own shutdown. It deletes the runner config and releases the instance claim but deliberately **does not terminate the instance**.

**Worker loop pattern ([internal/worker/common.go](../../internal/worker/common.go))**

`RunWorkerLoop` (and its variants `RunWorkerLoopWithObserver`, `RunWorkerLoopWithObservers`, `RunWorkerLoopWithTicker`) is the single concurrency primitive shared by every dispatch path. Each worker:

- Polls the queue on a 25-second idle ticker (`idlePollInterval`), but keeps draining without waiting for the next tick as long as `ReceiveMessages` returns a non-empty batch (`drainQueue`), so a deep backlog is bounded by processing throughput, not tick cadence.
- Caps in-flight processing with a buffered semaphore (`maxConcurrency = 5`), acquired **before** spawning so the number of goroutines — and the SQS visibility leases they hold — stays bounded even while draining.
- Tracks active goroutines with a `sync.WaitGroup` and waits for them in a `defer` so shutdown is clean.
- Wraps each handler in `recover()` so a panic in one job does not kill the worker, and reports "ok"/"error" per message via an optional `ProcessObserver`.
- Bounds per-message work with `config.MessageProcessTimeout` on a context deliberately detached from the loop's parent (`context.WithoutCancel`) — see Key Decisions.
- Reports each receive outcome ("messages"/"empty"/"error") via an optional `ReceiveObserver`, driving `QueueReceive`.
- Bounds each `ReceiveMessages` call by the smaller of `config.MessageReceiveTimeout` and the remaining context deadline.

`RunWorkerLoopWithTicker` exposes the same logic with an injectable tick channel for tests. A `TODO(metrics)` in `drainQueue` notes `PublishWorkerInflight` is not yet wired.

**Two dispatch paths, one short-circuit**

```
queue.Message ──► RunWorkerLoop ──► processor closure
                                       ├── processEC2Message  (worker/ec2.go)
                                       └── DirectProcessor    (worker/direct.go, no queue)
                                              │
                                              ▼ (job.Pool != "")
                                       WarmPoolAssigner.TryAssignToWarmPool (worker/warmpool.go)
```

`RunEC2Worker` calls `RunWorkerLoopWithObservers` bound to `EC2WorkerDeps`. The direct path skips the queue entirely: `TryDirectProcessing` is called from the webhook post-ack closure with its own 10-slot semaphore, so a hot job can launch before the SQS round trip; the always-enqueued SQS copy is the fallback if direct processing loses the claim race or fails.

**Dual-path dedup.** Both paths call `db.ClaimJob` first, so exactly one wins. `processEC2Message`'s deferred cleanup distinguishes four outcomes, and the distinction is load-bearing:

| Flag | Message | Metrics |
|---|---|---|
| `jobProcessed` | deleted | `QueueDepth -1` |
| `alreadyClaimed` | deleted | **`JobsDeduplicated{path=queue}`** + `QueueDepth -1` |
| `claimExhausted` | deleted | `QueueDepth -1` (the `SchedulingFailure` was already emitted by `failJobClaimExhausted`) |
| `poisonMessage` | deleted inline | `MessageDeletionFailure` + `QueueDepth -1` |

`JobsDeduplicated` counts the **correct** outcome of the dual-path race: the job *did* get a runner, via the winning path, so it must never deflate the fulfillment SLA and is deliberately not a `SchedulingFailure`. One nuance in the same block: a discarded message whose `RetryCount > 0` is logged at **info**, not debug — that message existed to *recover* a job with no runner, so dropping it means the recovery silently failed and the job starves. Leaving that case at debug hid exactly such a regression in production.

**Warm pool short-circuit ([internal/worker/warmpool.go](../../internal/worker/warmpool.go))**

Both paths consult `WarmPoolAssigner.TryAssignToWarmPool` before falling through to a fresh fleet. It early-returns on an empty pool, a nil Pool/Runner, or an already-dead context (returning a retryable error rather than issuing AWS calls that would fail immediately and cascade). Assignment goes through `PoolManager.ClaimAndStartPoolInstance`, whose DynamoDB conditional write makes reuse safe across replicas; `ErrNoAvailableInstance` is a clean "not assigned". On success it writes the job record with `WarmPoolHit: true` and `HotPoolHit: instance.IsFromRunningSpare()`. `releaseFailedAssignment` unwinds a partial assignment in a strict order — delete the runner config first (the agent reads it once at boot and never revalidates, so a leftover config would make the instance's *next* assignment register for this job), then stop the instance (up to 3 attempts), and only then release the claim, because releasing a still-running instance lets a concurrent orchestrator claim it as a hot spare that the pending stop would kill mid-flight.

**Naming ([internal/worker/naming.go](../../internal/worker/naming.go))**

`BuildRunnerConditions` flattens the requested resource labels (`arch`, `cpu`, `ram`, `disk`, `families`, `gen`) into a single dash-joined string used inside runner names and EC2 `Name` tags.

**AWS SDK observability ([internal/awsobs/middleware.go](../../internal/awsobs/middleware.go), [internal/awsobs/timeout.go](../../internal/awsobs/timeout.go))**

Both files register `smithy-go` `InitializeMiddleware` on the shared `aws.Config` via `awsconfig.WithAPIOptions`, so every AWS client built from that config carries them:

- `Middlewares()` returns the API option plus a `*Recorder`. `observe.HandleInitialize` times `next.HandleInitialize` end-to-end (spanning serialize, retry, signing, and the HTTP round trip), reads `awsmiddleware.GetServiceID` / `GetOperationName`, and publishes `PublishAWSCallDuration(service, operation, seconds)` on every call plus `PublishAWSCallFailure(service, operation, result)` on failure. `failureResult` classifies `"timeout"` for `context.DeadlineExceeded`, `context.Canceled`, or any `net.Error` reporting `Timeout()`, and `"error"` otherwise. `Recorder` holds the `metrics.Publisher` behind an `atomic.Pointer` so the middleware can be registered before the publisher exists (the CloudWatch backend is built *from* the config that carries the middleware) and wired later via `SetPublisher`; until then it emits to `metrics.NoopPublisher{}`. `SetPublisher(nil)` normalises back to the no-op.
- `PerOperationTimeout(d, exempt)` derives a `context.WithTimeout(ctx, d)` sub-context per operation, bounding a wedged call so it cannot consume a whole per-message budget. `d <= 0` makes it a pass-through. Operations named in `exempt` bypass it — the sole current use is SQS `ReceiveMessage`, whose `WaitTimeSeconds=20` long poll exceeds `AWSPerOpTimeout = 15s` and would otherwise be aborted on every empty poll.

Both middlewares self-anchor: observability inserts `After` the SDK's `RegisterServiceMetadata` (which seeds service/operation into the context — context changes propagate downward only, so anything registered *ahead* of it reads empty), and the timeout inserts `After` observability so observability stays outermost and times a timeout abort too. Each falls back through the other anchor and finally to `middleware.Before` at the head of the Initialize step (e.g. a bare test stack).

## Talks To [coverage: high -- 9 sources]

- `pkg/queue` — `queue.Queue`, `queue.JobMessage`, `queue.Message` (including `SentAt`, the SQS `SentTimestamp` behind `publishJobWait`). Used by every worker for receive/delete/send and by the webhook for enqueue.
- `pkg/fleet` — `fleet.Manager`, `fleet.LaunchSpec`, `fleet.FlexibleSpec` for EC2 fleet creation and warm-pool spec matching.
- `pkg/pools` — `pools.Manager`, `pools.AvailableInstance`, `pools.ErrNoAvailableInstance` for warm pool claim/start/stop and `MarkInstanceBusy`.
- `pkg/db` — `db.Client`, `db.JobRecord`, `db.PoolConfig`, and sentinels `db.ErrJobAlreadyClaimed`, `db.ErrJobClaimExhausted`, `db.ErrPoolAlreadyExists`. Used for claims (`ClaimJob`, `DeleteJobClaim`, `FailExhaustedClaim`), job records, cancellation (`MarkJobCancelled`), requeues (`MarkJobRequeuedByJobID`), ephemeral pools, and instance-claim release.
- `pkg/events` — `events.JobInfo`, the return type of `GetJobByJobID` consumed by `PublishJobStartupMetrics`.
- `pkg/github` — `gh.ParseLabelsWithAliases`, `gh.JobConfig`, `gh.AliasResolver`.
- `pkg/runner` — `runner.Manager`, `runner.PrepareRunnerRequest`. Both cold-start and warm-pool paths call `PrepareRunner` to write SSM parameters before the agent boots.
- `pkg/secrets` — via the `RunnerConfigDeleter` interface in `HandleJobCancelled`.
- `pkg/metrics` — `metrics.Publisher` for queue depth/receive, job assigned/requeued/deduplicated, scheduling failures, message-deletion failures, job-wait and job-startup latency, and (via `internal/awsobs`) per-operation AWS call duration/failure.
- `pkg/config` — `config.Config`, `MessageProcessTimeout`, `MessageReceiveTimeout`, `CleanupTimeout`, `ShortTimeout`, `AWSPerOpTimeout`, plus subnet lists.
- `pkg/logging` — `logging.WithComponent` / `ContextWith` / `ContextWithJob` for namespaced slog loggers (`webhook`, `worker`, `direct`, `ec2-worker`, `warmpool-assigner`) and job-identity propagation into deep AWS call logs.
- `pkg/tracing` — `tracing.Tracer()`, `ExtractTraceContext` / `InjectTraceContext` for W3C TraceContext propagation across webhook → SQS → worker.
- AWS SDK (`aws-sdk-go-v2/aws/middleware`, `smithy-go/middleware`) — `internal/awsobs` registers against the SDK's own Initialize step.

`internal/handler` is imported by `internal/worker` (for `BuildRunnerLabel`), the only cross-internal dependency. `internal/awsobs` depends on neither; it is wired where the shared `aws.Config` is constructed (`cmd/server/main.go`) and consumed transitively by every AWS client.

## API Surface [coverage: high -- 9 sources]

**`internal/handler`**

- `HandleWorkflowJobQueued(ctx, *github.WorkflowJobEvent, queue.Queue, *db.Client, *gh.AliasResolver) (*queue.JobMessage, error)`
- `PublishJobQueuedMetrics(ctx, metrics.Publisher, *queue.JobMessage)`
- `CapacityLabel(cpu int) string`
- `HandleJobFailure(ctx, *github.WorkflowJobEvent, queue.Queue, *db.Client, *gh.AliasResolver) (bool, error)`
- `HandleJobCancelled(ctx, *github.WorkflowJobEvent, *db.Client, RunnerConfigDeleter, *gh.AliasResolver) (bool, error)`
- `HandleWorkflowJobInProgress(*github.WorkflowJobEvent, *gh.AliasResolver) *StartupObservation` — pure filter, no ctx/I/O
- `PublishJobStartupMetrics(ctx, metrics.Publisher, JobStartupDB, *StartupObservation)`
- `EnsureEphemeralPool(ctx, PoolDBClient, *gh.JobConfig) error`
- `BuildRunnerLabel(ctx, *queue.JobMessage) string` — takes a ctx (it warns on a label-less re-dispatch)
- Types: `StartupObservation{JobID, Pool, Seconds}`
- Interfaces: `PoolDBClient` (`GetPoolConfig`, `CreateEphemeralPool`, `TouchPoolActivity`), `JobStartupDB` (`HasJobsTable`, `GetJobByJobID`), `RunnerConfigDeleter` (`Delete`)
- Constants: `maxJobRetries = 2`, `runnerNamePrefix = "runs-fleet-"`

**`internal/validation`**

- `ValidateK8sJWTPath(path string) error` — rejects empty, non-absolute, or paths not under `/var/run/secrets/`. Configuration-time check only; does not resolve symlinks or stat the file.

**`internal/worker` — common**

- `MessageProcessor`, `ReceiveObserver`, `ProcessObserver` (function types)
- `RunWorkerLoop`, `RunWorkerLoopWithObserver`, `RunWorkerLoopWithObservers`, `RunWorkerLoopWithTicker`
- Tunables: `maxConcurrency = 5`, `maxMessagesPerReceive = 10`, `receiveWaitSeconds = 20`, `idlePollInterval = 25s`

**`internal/worker` — direct**

- `DirectProcessor` struct (Fleet, Pool, Metrics, Runner, DB, Config, SubnetIndex, Queue, WarmPoolAssigner, CreateFleetFn, PrepareRunnersFn)
- `(*DirectProcessor).ProcessJobDirect(ctx, *queue.JobMessage) bool`
- `TryDirectProcessing(_, *DirectProcessor, chan struct{}, *queue.JobMessage)`

**`internal/worker` — ec2**

- `EC2WorkerDeps` struct (Queue, Fleet, Pool, Metrics, Runner, DB, Config, SubnetIndex, WarmPoolAssigner, CreateFleetFn, PrepareRunnersFn)
- `WarmPoolAssignerInterface` (for test injection)
- `RunEC2Worker(ctx, EC2WorkerDeps)`
- `SelectSubnet(*config.Config, *uint64) string`
- `CreateFleetWithRetry(ctx, fleetManagerInterface, *fleet.LaunchSpec) ([]string, error)`
- `SaveJobRecords(ctx, *db.Client, *queue.JobMessage, []string)`
- `PrepareRunners(ctx, RunnerPreparer, *queue.JobMessage, []string) []string`
- `BuildOnDemandFallbackJob(*queue.JobMessage) *queue.JobMessage` (shared with `direct.go`)
- Tunables: `RetryDelay = 1s`, `FleetRetryBaseDelay = 2s`, `maxDeleteRetries = 3`, `maxFleetCreateRetries = 3`, `maxJobRetries = 2`
- Metric label constants: `queueMain`, `sourceWarmPool`/`sourceColdStart`, `schedulingFailureJobClaim`/`schedulingFailureFleetCreate`, `requeueReasonOnDemandFallback`/`requeueReasonDirectFleetFailure`, `discardReasonAlreadyClaimed`, `dedupPathQueue`

**`internal/worker` — warmpool**

- `WarmPoolAssigner` struct (Pool, Runner, DB); `WarmPoolResult{Assigned, InstanceID}`
- `(*WarmPoolAssigner).TryAssignToWarmPool(ctx, *queue.JobMessage) (*WarmPoolResult, error)`
- Interfaces: `PoolManager`, `RunnerPreparer`, `JobDBClient`

**`internal/worker` — naming**

- `BuildRunnerConditions(*queue.JobMessage) string` — emits `arch-cpuN-ramN-diskN-families-genN`, omitting any unset field.

**`internal/awsobs`**

- `Middlewares() ([]func(*middleware.Stack) error, *Recorder)`
- `NewRecorder() *Recorder`; `(*Recorder).SetPublisher(metrics.Publisher)`
- `PerOperationTimeout(d time.Duration, exempt map[string]bool) func(*middleware.Stack) error`
- Unexported IDs: `middlewareID = "RunsFleetAWSObservability"`, `timeoutMiddlewareID = "RunsFleetAWSPerOpTimeout"`, anchor `registerServiceMetadataID = "RegisterServiceMetadata"`, exclusion `cloudWatchServiceID = "CloudWatch"`

## Data [coverage: high -- 9 sources]

**Runner naming**

- `runnerNamePrefix = "runs-fleet-"` (declared in `handler/webhook.go`). `HandleJobFailure` and `HandleWorkflowJobInProgress` both use it to recognise jobs this system owns when filtering webhook events that fire for every runner in a repo. `HandleJobCancelled` deliberately does *not*: a queued-then-cancelled job never got a runner, so there is no runner name to gate on and the labels are the only signal.
- `BuildRunnerConditions` produces the suffix. Format: `arch-cpu<min>-ram<min>-disk<gib>-<family1>-<family2>-gen<n>`; only set fields are included, dash-joined with no leading separator; an empty spec yields an empty string.
- `BuildRunnerLabel` returns `OriginalLabel` when present, otherwise reconstructs `runs-fleet=<run_id>[/pool=<name>][/spot=false]`.

**Runner-name length**

The runner name is assembled by upstream callers as `runs-fleet-<conditions>-<short-job-id>`. GitHub's 64-character registration limit constrains this, so the conditions string uses short tokens (`cpu4`, `ram8`, `gen8`) rather than human-readable forms.

**Job message construction**

`HandleWorkflowJobQueued` copies every flexible-spec field from `gh.JobConfig` into `queue.JobMessage`: `InstanceType`, `InstanceTypes`, `Pool`, `Spot`, `OriginalLabel`, `Arch`, `StorageGiB`, `Traceparent`, `CPUMin`/`CPUMax`, `RAMMin`/`RAMMax`, `Families`, `Gen`. The same shape round-trips through SQS and is decoded by `processEC2Message`.

**Ephemeral pool shape**

`EnsureEphemeralPool` creates `DesiredRunning: 0`, `DesiredStopped: 1`, `IdleTimeoutMinutes: 30`, carrying the flexible spec (arch, cpu/ram min-max, families) but deliberately **not** `InstanceType`: pinning the smallest resolved match would lock a flexible-label pool to one type, so the pool carries the spec and `resolvePoolInstanceTypes` re-resolves and price-ranks per launch.

**Subnet round-robin**

`SelectSubnet` uses `atomic.AddUint64(subnetIndex, 1) - 1`, then `idx % len(subnets)`.

**FIFO dedup id**

`BuildOnDemandFallbackJob` bumps `RetryCount` specifically so the FIFO dedup id (`job_id-retry_count`) changes and SQS does not drop the requeue as a duplicate of the original.

**AWS call metrics: what is actually instrumented**

`internal/awsobs` is attached to the shared `aws.Config` (and its `sqsCfg` clone) in `cmd/server`, so **every** SDK client the orchestrator builds carries it — there is no per-service opt-in. Service IDs and operations that therefore appear on `AWSCallDuration` / `AWSCallFailure`:

| Service | Operations reaching the middleware |
|---|---|
| SQS | `SendMessage`, `DeleteMessage`, `GetQueueAttributes`, `ListMessageMoveTasks`, `StartMessageMoveTask` — and `ReceiveMessage`, which is **timed but exempt from the per-op timeout** |
| DynamoDB | `GetItem`, `PutItem`, `UpdateItem`, `DeleteItem`, `Query`, `Scan`, `BatchWriteItem` |
| EC2 | `CreateFleet`, `RunInstances`, `CreateTags`, `DescribeInstances`, `DescribeFleetInstances`, `DeleteFleets`, `DescribeInstanceTypeOfferings`, `DescribeSpotPriceHistory`, `DescribeSpotInstanceRequests`, `DescribeSubnets`, `DescribeLaunchTemplateVersions`, `StartInstances`, `StopInstances`, `TerminateInstances` |
| S3 | `HeadObject` (cache), `PutObject` (cost report). Object reads/writes by runners go through pre-signed URLs, and `PresignGetObject` signs locally without issuing an SDK call — so the cache's hot path is invisible to these metrics |
| SSM | `GetParameter`, `GetParametersByPath`, `PutParameter`, `DeleteParameter` |
| SNS | `Publish` (cost report) |
| Pricing | `GetProducts` |
| **CloudWatch** | **none — excluded by service ID** |

CloudWatch's `PutMetricData` (the metrics backend) and `GetMetricData` (the cost reporter's metric reads) are both invisible to these metrics; see Key Decisions for why.

## Key Decisions [coverage: high -- 9 sources]

- **`internal/` over `pkg/`.** These packages are not part of the orchestrator's contract; `internal/` enforces that no external consumer can import them.

- **CloudWatch is deliberately excluded from AWS-call instrumentation.** The exclusion is a recursion guard, not an oversight. The CloudWatch metrics backend publishes via `PutMetricData` on the *same shared `aws.Config`* that carries this middleware, so timing those calls would emit an `AWSCallDuration` metric whose own publish issues another `PutMetricData`, which would be timed, which would publish again — amplifying without bound. `observe.HandleInitialize` therefore returns early when `awsmiddleware.GetServiceID(ctx) == cloudWatchServiceID`, before reading the operation name or touching the publisher. Note the check is by *service*, so the cost reporter's `GetMetricData` reads are collateral: they are also unmeasured, even though they could not recurse.

- **`JobsDeduplicated` is a success counter, not a failure counter.** The dual-path design (direct + always-enqueued SQS copy) guarantees a race, and `db.ClaimJob` resolves it. The losing path dropping its message is the *intended* outcome — the job got a runner via the winner — so it emits `JobsDeduplicated{path=queue}` and explicitly not `SchedulingFailure`. Conflating the two would deflate the fulfillment SLA on every single job. `claimExhausted` is the genuine failure in the same code block and deliberately emits no dedup metric.

- **A discarded *re-dispatch* is logged louder than a discarded first attempt.** `retryCount > 0` on an already-claimed drop means a recovery message was thrown away and the job it was recovering starves; that is logged at info, while the ordinary first-attempt dedup stays at debug. The comment records why: leaving it at debug hid exactly such a regression in production.

- **2026-06: K8s worker path removed.** `internal/worker/k8s.go` (and `K8sWorkerDeps`/`RunK8sWorker`/`processK8sMessage`/`CreateK8sRunnerWithRetry`) was deleted with the K8s runner backend. `internal/validation.ValidateK8sJWTPath` remains — Vault's Kubernetes *auth method* is unrelated to the runner backend.

- **Direct processing optimisation.** `TryDirectProcessing` lets the webhook post-ack closure launch a job in-process when capacity permits, skipping SQS for the common-case latency win. On semaphore overflow the message still goes via SQS, so no job is dropped. It runs on `context.Background()` (not the request context) because the goroutine outlives the HTTP handler, and re-stashes job identity on that fresh context for logging and panic records.

- **Single concurrency primitive.** `RunWorkerLoop` and its observer variants are shared by the remaining dispatch path; the tunables live in one file.

- **Per-message context is detached from the loop's parent.** `drainQueue` runs each processor on `context.WithoutCancel(ctx)` bounded by a fresh `config.MessageProcessTimeout`. On shutdown the receive path still stops accepting new work via `ctx.Done()`, but an already-dispatched processor runs to completion instead of aborting mid-flight with "context canceled" and producing a rollout error burst. The deferred `activeWork.Wait()` drains them before the worker returns.

- **Cleanup always runs on a fresh context.** Claim releases, terminal writes, requeues, and message deletes all build `context.WithTimeout(context.Background(), config.CleanupTimeout)` rather than reusing the job context, which may already be expired on a wedged connection — precisely the situation in which cleanup matters most.

- **DB-backed job claims, with three distinct outcomes.** `db.ErrJobAlreadyClaimed` → another orchestrator won (dedup). `db.ErrJobClaimExhausted` → the lease was re-claimed past its cap without ever provisioning, so `failJobClaimExhausted` marks the record terminal and returns its error, letting the caller keep the SQS message for redelivery rather than deleting it on a failed transition. Any other claim error → `SchedulingFailure{job_claim}` and leave the message. The direct path defers the terminal transition to the queue path rather than duplicating it.

- **`NotifyPoolDemand` fires post-ack and is regex-gated.** Waking the pool reconciler on a `queued` webhook is what closes the gap between a warm-pool job arriving and the 60s reconcile tick, but it is best-effort observability-adjacent work: it runs after GitHub is acked, and only for a pool name that passes the length and character checks in `cmd/server`.

- **Cancellation cleans up without terminating.** `HandleJobCancelled` deletes the runner config and releases the claim but leaves the instance alive. A `launched` record only means no started signal has been *processed* yet — the runner may have registered moments ago and be visible to GitHub as idle capacity for some other queued job, and killing that is the starvation bug this series removed. Deleting the config stops a runner being minted for a job that no longer exists; anything already registered goes on to serve other work, and an instance that never registered exits on its standby deadline.

- **A label-less re-dispatch warns instead of degrading silently.** `BuildRunnerLabel`'s synthesized fallback only matches a workflow using the legacy `runs-fleet=<run-id>/...` form. A re-dispatch (`RetryCount > 0`) reaching it has lost the label its first dispatch carried, so it would register a runner the starving job can never be handed to — burning an instance while the job keeps waiting. That is worth an alert, which is why the function takes a `ctx`.

- **Webhook metrics deferred past the ack.** `HandleWorkflowJobQueued` does only durable work; `PublishJobQueuedMetrics` runs after the response so a slow metrics backend cannot eat GitHub's delivery budget.

- **2026-07: startup latency measured on GitHub's clock (PR #387).** `HandleWorkflowJobInProgress` computes `JobStartupSeconds` from the payload's own `created_at`/`started_at` — both stamped by GitHub — so clock skew cannot corrupt the span. Because `in_progress` fires for *every* runner in a repo, the filter demands two-sided proof of ownership (prefix AND label parse). On any source-lookup miss the observation publishes with an empty `source` rather than being dropped, so dashboard totals stay complete. The counterpart instance-provision latency is emitted from `pkg/termination` instead, because its two timestamps live on different machines.

- **AWS SDK middleware as a diagnostic safety net, not a semantic layer.** `internal/awsobs` changes no call behavior beyond the timeout; it times and classifies. It replaced an earlier per-call WARN log that flooded logs on SQS long polls, trading log noise for a queryable duration/failure signal dimensioned by service and operation.

- **Middleware self-anchoring instead of a fixed stack position.** Both middlewares locate themselves relative to named IDs (`RegisterServiceMetadata`, then each other) rather than assuming an index, with a `middleware.Before` fallback. Anchoring *after* the metadata seeder is mandatory, not cosmetic: context changes propagate downward only, so anything registered ahead of it reads an empty service and operation.

## Gotchas [coverage: high -- 9 sources]

- **CloudWatch API latency is invisible in `AWSCallDuration`, by design.** Anyone debugging "why don't I see CloudWatch call latency" should check the `cloudWatchServiceID` exclusion first, not assume the middleware is broken. With CloudWatch metrics now off by default ([project-overview](project-overview.md)), the exclusion is also mostly moot in practice — but the cost reporter's `GetMetricData` reads remain unmeasured.

- **The per-operation timeout exemption is name-based and order-sensitive.** `PerOperationTimeout`'s `exempt` map matches `awsmiddleware.GetOperationName(ctx)`, populated only once `RegisterServiceMetadata` has run. If the timeout middleware were inserted ahead of that anchor the name would read empty, the `ReceiveMessage` exemption would silently stop matching, and every long poll would abort after 15s. The `stack.Initialize.Get` fallbacks preserve the ordering; a hand-built stack that omits both anchors does not.

- **`AWSCallDuration` and `AWSCallFailure` are missing for the first AWS calls of every boot.** The `Recorder` emits to a no-op until `SetPublisher` runs, which cannot happen until after `awsconfig.LoadDefaultConfig` (the CloudWatch backend depends on that config). Early-boot calls are genuinely unmeasured.

- **Spot fallback path is delicate.** When `CreateFleetWithRetry` fails on spot and retries remain, `handleOnDemandFallback` sends the on-demand requeue *before* deleting the original SQS message, so a failed requeue leaves the original for redelivery. The direct path's symmetric `recoverFleetFailure` follows the same send-before-cleanup ordering, releases the claim *first* so the requeue can re-claim, and marks the claim terminal (via `FailExhaustedClaim`) rather than silently dropping when no fallback is eligible. `onDemandFallbackEligible` (`job.Spot && !job.ForceOnDemand && RetryCount < 2`) is shared by both paths so their log level and their recovery branch stay in lockstep.

- **`ProcessJobDirect` returns `false` on "already claimed" and "claim exhausted".** Neither is a direct-path failure — the first means another worker won, the second means the queue path owns the terminal transition. Callers must not log either as an error. The semaphore is released regardless.

- **`CreateFleetWithRetry` sleeps on the real clock.** It calls `time.Sleep(FleetRetryBaseDelay * 1<<(attempt-1))` without selecting on `ctx.Done()`, so up to ~6s of backoff is unresponsive to cancellation; the same applies to `deleteMessageWithRetry` / `sendMessageWithRetry` (`RetryDelay`) and `SaveJobRecords`' 100/200ms backoff. Tests override the exported vars rather than using `synctest` for this reason.

- **Receive-timeout math has to leave room for retries.** `drainQueue` bounds each `ReceiveMessages` call to the smaller of `config.MessageReceiveTimeout` (25s) and the remaining deadline, long-polling up to 20s for up to 10 messages. Combined with `config.MessageProcessTimeout` and the SQS visibility timeout in `pkg/queue`, tightening any one of these can cause duplicate processing.

- **Warm-pool unwind order is load-bearing and can leave a claim to expire.** If `PrepareRunner` or `SaveJob` fails after `ClaimAndStartPoolInstance` succeeded, `releaseFailedAssignment` must delete the config, then stop the instance, then release the claim. If the stop never succeeds after 3 attempts it deliberately leaves the claim to expire on its TTL rather than releasing a running instance — releasing it would let a concurrent orchestrator claim it as a hot spare that the pending stop would then kill mid-flight. The log says `manual cleanup may be required`.

- **`SaveJobRecords` failures are logged, not fatal.** A cold-start job whose record fails to save still proceeds, which means spot-interruption recovery and cost attribution have no record for that instance.

- **Subnet index pointer is shared.** `SelectSubnet` writes through a `*uint64`. Workers and the direct processor must share the *same* pointer, or each path round-robins independently and biases toward subnet zero.

- **Matrix jobs share `RunID`.** The `BuildRunnerLabel` fallback keys off `RunID` only, so two matrix jobs in one workflow run produce identical labels. Disambiguation comes from the runner-name suffix (job id), not the label.

- **`HandleJobFailure` over-filters intentionally.** It returns `false, nil` for a non-`runs-fleet-` runner name, unparseable labels, a missing jobs table, a missing record, an exhausted retry count, or a zero `RunID`/`Repo`. None are errors — they signal the event was for a different system or there is nothing to do.

- **`JobStartupDB` is nil-check-guarded but interface-typed.** `PublishJobStartupMetrics` tolerates a nil `dbc`, but a caller holding a concrete `*db.Client` must guard the typed-nil trap before assigning it (a nil pointer in a non-nil interface passes `dbc != nil`); `cmd/server`'s `in_progress` case does exactly this. An empty `source` dimension on `JobStartupSeconds` is the deliberate degraded output, not a bug.

- **`maxJobRetries = 2` exists in two places.** Both `internal/handler/webhook.go` and `internal/worker/ec2.go` declare it, and `pkg/termination`'s `maxBootstrapRequeues` is a third copy of the same intent. Updating one without the others silently desyncs the recovery paths.

- **`publishJobWait` silently skips when `SentAt` is zero.** A non-SQS backend or a missing `SentTimestamp` attribute drops the enqueue→assignment latency observation entirely; negative elapsed (clock skew) is clamped to 0 rather than dropped.

- **`ValidateK8sJWTPath` is config hygiene, not runtime security.** Its own comment says so: it does not resolve symlinks or stat the file. Production trust comes from kubelet-managed mounts.

- **`EnsureEphemeralPool` failures are non-fatal.** The webhook logs a warning and enqueues the job anyway, so a pool-table hiccup produces a `pool=` job routed to a pool that may not exist yet — the reconciler creates capacity on its next pass.

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
