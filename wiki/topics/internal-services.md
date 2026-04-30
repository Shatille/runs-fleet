---
topic: Internal Services (handlers, validation, workers)
last_compiled: 2026-04-30
sources_count: 8
---

# Internal Services (handlers, validation, workers)

## Purpose [coverage: high -- 8 sources]

The `internal/` tree holds orchestrator-only helpers that are intentionally not part of the public `pkg/` API. Go's `internal/` rule prevents downstream importers from depending on these types, so the orchestrator can refactor them freely. Three subpackages live here:

- `internal/handler` — HTTP-level webhook processing (parsing GitHub `workflow_job` events, building queue messages, and bootstrapping ephemeral pools).
- `internal/validation` — small input-validation utilities used at config time (currently only the Vault Kubernetes JWT path guard).
- `internal/worker` — the queue worker loops that dispatch jobs to compute backends. There is one entry point per dispatch path: `direct` (in-process), `ec2` (spot/on-demand fleet creation), `k8s` (pod creation), and `warmpool` (assignment to pre-warmed instances). `naming.go` provides the runner-conditions string used in instance/runner identifiers.

Together these packages form the "control glue" between the GitHub webhook on one side and the compute providers (`pkg/fleet`, `pkg/provider/k8s`) on the other.

## Architecture [coverage: high -- 8 sources]

**Webhook intake (`internal/handler/webhook.go`)**

`HandleWorkflowJobQueued` is the canonical entry point. It calls `gh.ParseLabels`, parses the run ID, and (for `pool=` jobs) lazily creates an ephemeral pool via `EnsureEphemeralPool` before constructing a `queue.JobMessage` and enqueuing it. Failure-path handling — `HandleJobFailure` — re-queues failed jobs (capped at `maxJobRetries = 2`), forcing on-demand on retry.

**Worker loop pattern (`internal/worker/common.go`)**

`RunWorkerLoop` is the single concurrency primitive shared by every dispatch path. Each worker:

- Polls the queue every 25 seconds via a `time.Ticker`.
- Caps in-flight processing with a buffered semaphore (`maxConcurrency = 5`).
- Tracks active goroutines with a `sync.WaitGroup` and waits for them in a `defer` so shutdown is clean.
- Wraps each message handler in `recover()` so a panic in one job does not kill the worker.
- Bounds per-message work with `config.MessageProcessTimeout`.

`RunWorkerLoopWithTicker` exposes the same logic with an injectable tick channel for tests.

**Three dispatch paths**

```
queue.Message ──► RunWorkerLoop ──► processor closure
                                       ├── processEC2Message  (worker/ec2.go)
                                       ├── processK8sMessage  (worker/k8s.go)
                                       └── DirectProcessor    (worker/direct.go, no queue)
```

`RunEC2Worker` and `RunK8sWorker` both call `RunWorkerLoop`, differing only in which `*WorkerDeps` struct they bind. The direct path skips the queue entirely: `TryDirectProcessing` is called from the webhook handler with a separate semaphore so a hot job can be launched before SQS round-trip.

**Warm pool short-circuit (`internal/worker/warmpool.go`)**

Both EC2 and direct paths consult `WarmPoolAssigner.TryAssignToWarmPool` before falling through to a fresh fleet. Assignment uses `PoolManager.ClaimAndStartPoolInstance` which performs the DynamoDB conditional write that makes pool reuse safe across multiple Fargate tasks.

**Naming (`internal/worker/naming.go`)**

`BuildRunnerConditions` flattens the requested resource labels (`arch`, `cpu`, `ram`, `disk`, `families`, `gen`) into a single dash-joined string used inside runner names and EC2 `Name` tags.

## Talks To [coverage: high -- 8 sources]

- `pkg/queue` — `queue.Queue` interface, `queue.JobMessage`, `queue.Message`. Used by every worker for receive/delete/send and by the webhook for enqueue.
- `pkg/fleet` — `fleet.Manager`, `fleet.LaunchSpec`, `fleet.FlexibleSpec` for EC2 fleet creation and warm-pool spec matching.
- `pkg/pools` — `pools.Manager`, `pools.AvailableInstance`, `pools.ErrNoAvailableInstance` for warm pool claim/start/stop.
- `pkg/db` — `db.Client`, `db.JobRecord`, `db.PoolConfig`, sentinel errors `db.ErrJobAlreadyClaimed` and `db.ErrPoolAlreadyExists`. Used for job claims, ephemeral pool creation, and instance-claim release.
- `pkg/github` — `gh.ParseLabels`, `gh.JobConfig` for webhook label parsing.
- `pkg/provider` and `pkg/provider/k8s` — `k8s.Provider`, `k8s.PoolProvider`, `provider.RunnerSpec`, `provider.RunnerResult` for the K8s dispatch path.
- `pkg/runner` — `runner.Manager`, `runner.PrepareRunnerRequest`. Both EC2 and warm-pool paths call `PrepareRunner` to write SSM parameters before the agent boots.
- `pkg/metrics` — `metrics.Publisher` for queue depth, fleet size, warm-pool hits, scheduling failures, claim failures, message-deletion failures, job duration.
- `pkg/config` — `config.Config`, `config.MessageProcessTimeout`, `config.CleanupTimeout`, `config.ShortTimeout`, plus subnet lists.
- `pkg/logging` — `logging.WithComponent` for namespaced slog loggers (`webhook`, `worker`, `direct`, `ec2-worker`, `k8s-worker`, `warmpool-assigner`).

The `internal/handler` package is also imported by `internal/worker` (for `BuildRunnerLabel`), which is the only cross-internal dependency.

## API Surface [coverage: high -- 8 sources]

**`internal/handler`**

- `HandleWorkflowJobQueued(ctx, *github.WorkflowJobEvent, queue.Queue, *db.Client, metrics.Publisher) (*queue.JobMessage, error)`
- `HandleJobFailure(ctx, *github.WorkflowJobEvent, queue.Queue, *db.Client, metrics.Publisher) (bool, error)`
- `EnsureEphemeralPool(ctx, PoolDBClient, *gh.JobConfig) error`
- `BuildRunnerLabel(*queue.JobMessage) string`
- `PoolDBClient` interface (`GetPoolConfig`, `CreateEphemeralPool`, `TouchPoolActivity`)
- Constants: `maxJobRetries = 2`, `runnerNamePrefix = "runs-fleet-"`

**`internal/validation`**

- `ValidateK8sJWTPath(path string) error` — rejects empty, non-absolute, or paths not under `/var/run/secrets/`. Configuration-time check only; does not resolve symlinks or stat the file.

**`internal/worker` — common**

- `MessageProcessor` (function type)
- `RunWorkerLoop(ctx, name, queue.Queue, MessageProcessor)`
- `RunWorkerLoopWithTicker(ctx, name, queue.Queue, MessageProcessor, <-chan time.Time)`

**`internal/worker` — direct**

- `DirectProcessor` struct (Fleet, Pool, Metrics, Runner, DB, Config, SubnetIndex)
- `(*DirectProcessor).ProcessJobDirect(ctx, *queue.JobMessage) bool`
- `TryDirectProcessing(_, *DirectProcessor, chan struct{}, *queue.JobMessage)`

**`internal/worker` — ec2**

- `EC2WorkerDeps` struct (Queue, Fleet, Pool, Metrics, Runner, DB, Config, SubnetIndex, WarmPoolAssigner)
- `WarmPoolAssignerInterface` (for test injection)
- `RunEC2Worker(ctx, EC2WorkerDeps)`
- `SelectSubnet(*config.Config, *uint64, publicIP bool) string`
- `CreateFleetWithRetry(ctx, *fleet.Manager, *fleet.LaunchSpec) ([]string, error)`
- `SaveJobRecords(ctx, *db.Client, *queue.JobMessage, []string)`
- `PrepareRunners(ctx, *runner.Manager, *queue.JobMessage, []string)`
- Tunables: `RetryDelay = 1s`, `FleetRetryBaseDelay = 2s`, `maxDeleteRetries = 3`, `maxFleetCreateRetries = 3`, `maxJobRetries = 2`

**`internal/worker` — k8s**

- `K8sWorkerDeps` struct (Queue, Provider, PoolProvider, Metrics, Runner, DB, Config)
- `RunK8sWorker(ctx, K8sWorkerDeps)`
- `CreateK8sRunnerWithRetry(ctx, *k8s.Provider, *provider.RunnerSpec) (*provider.RunnerResult, error)`

**`internal/worker` — warmpool**

- `WarmPoolAssigner` struct (Pool, Runner, DB)
- `WarmPoolResult` struct (Assigned, InstanceID)
- `(*WarmPoolAssigner).TryAssignToWarmPool(ctx, *queue.JobMessage) (*WarmPoolResult, error)`
- Interfaces: `PoolManager`, `RunnerPreparer`, `JobDBClient`

**`internal/worker` — naming**

- `BuildRunnerConditions(*queue.JobMessage) string` — emits `arch-cpuN-ramN-diskN-families-genN`, omitting any unset field.

## Data [coverage: high -- 8 sources]

**Runner naming**

- `runnerNamePrefix = "runs-fleet-"` (declared in `handler/webhook.go`). `HandleJobFailure` uses this prefix to recognise jobs it owns when filtering completed-with-failure events.
- `BuildRunnerConditions` produces the suffix portion. Format: `arch-cpu<min>-ram<min>-disk<gib>-<family1>-<family2>-gen<n>`. Only set fields are included; the assembled string is dash-joined with no leading separator. Empty job specs produce an empty string.
- `BuildRunnerLabel` returns the original webhook label when present; otherwise reconstructs `runs-fleet=<run_id>[/pool=<name>][/spot=false]`. This is what GitHub sees during JIT registration.

**Runner-name length**

The runner name is assembled by upstream callers as `runs-fleet-<conditions>-<short-job-id>`. Recent commits (`4988e4b`) note the GitHub registration limit of 64 characters; the conditions string from `BuildRunnerConditions` is the largest variable component, so it deliberately uses short tokens (`cpu4`, `ram8`, `gen8`) instead of human-readable forms.

**Job message construction**

`HandleWorkflowJobQueued` copies every flexible-spec field from `gh.JobConfig` into `queue.JobMessage`: `InstanceType`, `InstanceTypes`, `Pool`, `Spot`, `OriginalLabel`, `Region`, `Environment`, `OS`, `Arch`, `StorageGiB`, `PublicIP`, `CPUMin`/`CPUMax`, `RAMMin`/`RAMMax`, `Families`, `Gen`. The same shape is round-tripped through SQS and decoded by `processEC2Message` / `processK8sMessage`.

**Subnet round-robin**

`SelectSubnet` uses `atomic.AddUint64(subnetIndex, 1) - 1` to advance a shared counter, then takes `idx % len(subnets)`. Private subnets are preferred unless `PublicIP=true` is requested. Returns empty string when public is requested but no public subnets exist (caller must handle).

## Key Decisions [coverage: high -- 8 sources]

- **`internal/` over `pkg/`.** These packages are not part of the orchestrator's contract; placing them in `internal/` enforces that no external consumer can import them and gives the team freedom to refactor without API-stability concerns.

- **One worker per dispatch path.** Rather than a single dispatcher with branching, EC2 and K8s have independent workers. Each owns its own queue subscription and concurrency budget, so a stuck K8s API does not block EC2 fleet creation.

- **Direct processing optimisation.** `TryDirectProcessing` lets the webhook handler launch a job in-process when capacity permits, skipping SQS entirely for the common-case latency win. Capacity is bounded by an external semaphore; on overflow the message still goes via SQS, so no job is dropped. The direct path uses `context.Background()` (not the request context) because the goroutine outlives the HTTP handler.

- **Single concurrency primitive.** `RunWorkerLoop` is shared by both backends. Tunables (`maxConcurrency = 5`, 25-second ticker, `MessageProcessTimeout`) live in one file.

- **DB-backed job claims.** Both `ProcessJobDirect` and `processEC2Message` call `db.ClaimJob` early. `db.ErrJobAlreadyClaimed` is the only signal that another orchestrator already took the job; on that error the EC2 path still deletes the SQS message (so the queue does not redrive forever).

- **Path-traversal validation at config time.** `ValidateK8sJWTPath` runs during boot, before any Vault auth attempt. The comment explicitly documents that this is *not* runtime security: it is config hygiene meant to catch typos and prevent obviously-bad paths from being passed to Vault. Production trust still comes from kubelet-managed mounts.

- **Ephemeral pools created on demand.** `EnsureEphemeralPool` creates a 0-running / 1-stopped pool with a 30-minute idle timeout when a `pool=` label arrives for an unknown pool name. `db.ErrPoolAlreadyExists` is treated as a benign race (touch activity instead).

## Gotchas [coverage: high -- 8 sources]

- **Spot fallback path is delicate.** When `CreateFleetWithRetry` fails on spot and retries are available, `handleOnDemandFallback` deletes the SQS message *before* re-queueing as on-demand. If the delete succeeds but the re-queue fails, the job is lost — there is an explicit `ec2Log.Error("job lost - fallback requeue failed", ...)` log line for this. The cleanup uses `context.Background()` with `config.CleanupTimeout` so it survives even if the parent context was already cancelled.

- **`ProcessJobDirect` returns `false` on "already claimed".** This is *not* a failure — it means another worker beat us to it. Callers must not log this as an error. The semaphore in `TryDirectProcessing` is released regardless.

- **Visibility-timeout coupling.** The worker calls `q.ReceiveMessages(recvCtx, 10, 20)` (max 10 messages, 20-second long-poll). The receive timeout is the smaller of 25 seconds and the remaining context deadline. Combined with `MessageProcessTimeout` and the SQS visibility timeout configured in `pkg/queue`, the math has to leave room for retries; tightening any one of these can cause duplicate processing.

- **Warm-pool failures stop the instance.** If `PrepareRunner` or `SaveJob` fails *after* `ClaimAndStartPoolInstance` succeeded, the instance is already booting. The assigner retries `StopPoolInstance` up to three times, then logs `manual cleanup required`. The DB claim is also released so a future job can pick the instance up again.

- **K8s worker is more brittle by design.** `processK8sMessage` treats a missing `Runner` manager, a `GetRegistrationToken` failure, or any unmarshal error as a poison message — the message is deleted and never retried. The EC2 path is more forgiving because spot fallback exists.

- **Subnet index pointer is shared.** `SelectSubnet` writes to a `*uint64` passed by reference. Workers and the direct processor must share the *same* pointer, or each path will round-robin independently and bias toward subnet zero.

- **Matrix jobs share `RunID`.** `BuildRunnerLabel` keys off `RunID` only, so two matrix jobs in the same workflow run produce identical labels. Disambiguation comes from the runner-name suffix (job ID), not the label — see commit `4988e4b`.

- **`HandleJobFailure` over-filters intentionally.** It returns `false, nil` when the runner name does not start with `runs-fleet-`, when labels can't be parsed, or when no jobs table exists. None of these are errors — they are signals that the event was for a different system or that we have nothing to do.

- **`maxJobRetries = 2` exists in two places.** Both `internal/handler/webhook.go` and `internal/worker/ec2.go` declare it. The intent is the same (cap retries at 2) but the constants are not shared; updating one without the other will silently desync the spot-fallback path from the failure-requeue path.

## Sources [coverage: high]

- [internal/handler/webhook.go](../../internal/handler/webhook.go)
- [internal/validation/path.go](../../internal/validation/path.go)
- [internal/worker/common.go](../../internal/worker/common.go)
- [internal/worker/direct.go](../../internal/worker/direct.go)
- [internal/worker/ec2.go](../../internal/worker/ec2.go)
- [internal/worker/k8s.go](../../internal/worker/k8s.go)
- [internal/worker/naming.go](../../internal/worker/naming.go)
- [internal/worker/warmpool.go](../../internal/worker/warmpool.go)
