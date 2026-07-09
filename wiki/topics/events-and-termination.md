---
topic: Events + Termination Handlers
last_compiled: 2026-07-09
sources_count: 5
---

# Events + Termination Handlers

## Purpose [coverage: high -- 5 sources]

`pkg/events` and `pkg/termination` host two independent SQS queue processors
that react to instance lifecycle signals.

- `pkg/events/handler.go` consumes EventBridge events forwarded into the
  events SQS queue: spot interruption warnings (`EC2 Spot Instance
  Interruption Warning`) and instance state-change notifications (`EC2
  Instance State-change Notification`). Spot warnings are turned into a
  `MarkInstanceTerminating` write plus a job re-queue with
  `ForceOnDemand=true`; state changes are now informational only.
- `pkg/termination/handler.go` consumes telemetry produced by the agent (and
  the boot shim) over the job lifecycle: `started` confirms the runner
  registered and transitions the job launched → running (emitting the
  `RunnerConfirmed`, `InstanceProvisionSeconds`, and `AgentBootstrapSeconds`
  metrics); `bootstrap_failed` recovers a job whose instance never started
  the agent; completion statuses (`success`/`failure`/`timeout`/
  `interrupted`) mark the job complete in DynamoDB, publish
  completion/duration/billing metrics, and delete the runner config from the
  secrets store.

The termination handler is also the landing zone for agent-observed metrics
that ride the telemetry (tool-cache misses, cache-interceptor outcome, v2
cache-write bytes, bootstrap phase timings) — the agent has no metrics client
of its own; see [observability](observability.md).

## Architecture [coverage: high -- 5 sources]

Both packages follow the same pattern: a `Handler` struct constructed via
`NewHandler`, exposing a single `Run(ctx context.Context)` loop that polls SQS
on a 1-second ticker (`eventsTickInterval`, `handlerTickInterval` — vars, for
test override) and fans out work over a bounded semaphore
(`maxConcurrency = 5`).

Events handler (`pkg/events/handler.go`):

- `Handler` holds `queueClient QueueAPI`, `dbClient DBAPI`, `metrics
  MetricsAPI`, `config *config.Config`, plus a `jobQueue JobRequeuer` and
  `circuitBreaker CircuitBreakerAPI` installed post-construction via
  `SetJobQueue` / `SetCircuitBreaker`.
- `Run` calls `ReceiveMessages(ctx, 10, 20)` (10 messages, 20s long poll)
  under a 25s timeout (or the remaining deadline, whichever is shorter), and
  publishes a `QueueReceive` outcome (`messages`/`empty`/`error`) per poll.
  Each message gets a goroutine bounded by the semaphore; the loop
  `wg.Wait()`s the batch before polling again. Processing runs under
  `context.WithTimeout(context.WithoutCancel(ctx), config.MessageProcessTimeout)`
  — detached from the parent SIGTERM context (keeping log/trace values) so an
  in-flight spot-interruption requeue completes on shutdown instead of
  aborting with "context canceled". Panics are recovered;
  `MessageProcessingSeconds` (queue `events`, result `ok`/`error`) is timed
  from slot acquisition. Empty polls sleep a 160–240ms jitter.
- `processEvent` always defers `DeleteMessage(handle)` and emits
  `PublishMessageDeletionFailure` if the delete fails. Body is unmarshaled
  into `EventBridgeEvent`, dispatched on `DetailType`.
- `handleSpotInterruption` opens an `events.spot_interruption` trace span,
  marks the instance terminating, resolves the job, records the interruption
  in the circuit breaker (and publishes `CircuitBreakerOpen`/`Trip` from its
  state), re-queues the job on-demand, and publishes
  `JobRequeued{reason=spot_interruption}`. The `SpotInterruptions{Family}`
  counter is published from a **deferred** best-effort block with a detached
  context so a throttled CloudWatch call can never abort the critical work —
  and still fires when the spot reclaim tore down the handler's own context.

Termination handler (`pkg/termination/handler.go`):

- `Handler` holds `queueClient QueueAPI`, `dbClient DBAPI`, `metrics
  MetricsAPI`, `secretsStore secrets.Store`, `jobQueue JobQueueAPI` (nilable
  — bootstrap-failure re-queue is then skipped), and `config *config.Config`.
- `Run` polls identically; each message gets a 30s timeout on a
  `context.WithoutCancel` context, and `MessageProcessingSeconds` (queue
  `termination`) is published per message. On `ctx.Done()` it `wg.Wait()`s
  in-flight work before returning.
- `processMessage` is a dispatch tree with explicit ack semantics
  (`ackMessage` = `DeleteMessage`, no-op on empty handle):
  - empty body / unparseable JSON → warn + **ack** (non-retryable; the
    termination queue has no DLQ, so returning an error would redeliver
    garbage for the full retention window);
  - `status == "bootstrap_failed"` → `processBootstrapFailure` (instance-scoped:
    the boot shim `scripts/cloud-init-boot.sh` has no job_id) then ack;
    transient DB/queue errors are returned so SQS redelivers;
  - `status == "started"` → validate, `confirmRunnerStarted`, ack; a DB error
    is returned for redelivery so the unconfirmed-runner watchdog does not
    later mistake a healthy job for a never-confirmed one;
  - otherwise → validate (instance_id, job_id, status required; invalid →
    warn + ack), `processTermination`, ack only after success.
- `confirmRunnerStarted` calls `MarkJobStarted(jobID, msg.StartedAt)` — a
  conditional launched → running transition with `ReturnValueAllNew`
  ([pkg/db/jobs.go](../../pkg/db/jobs.go)), so a late "started" after the job
  completed or was recovered is a `(nil, nil)` no-op. On a real transition it
  publishes `RunnerConfirmed{Pool}`, then `publishProvisionSeconds` (agent
  `StartedAt` − record `CreatedAt`; source `warm_pool`/`cold_start` from
  `JobInfo.WarmPoolHit`; family from the instance type) and
  `publishBootstrapSeconds` (one `AgentBootstrapSeconds{Pool,Phase}` per
  positive `bootstrap_*` segment; phase enum fixed orchestrator-side).
- `processBootstrapFailure` resolves the job by instance, and if
  `RetryCount < maxBootstrapRequeues (= 2)` marks it requeued via the
  idempotent `MarkJobRequeuedByJobID` (a redelivery or the watchdog cannot
  double-enqueue) and sends a `Spot=false, ForceOnDemand=true` re-queue,
  publishing `JobsRequeued{reason=bootstrap_failed}`. On exhaustion it gives
  up loudly: `SchedulingFailure{task_type=bootstrap_failed}` — a systemic
  bootstrap failure (bad AMI/agent rollout) must fail fast rather than loop
  forever burning instances. Runner-config cleanup is best-effort.
- `processTermination` opens a `termination.process` span, calls
  `MarkJobComplete` (`ReturnValueAllNew` echoes run_id/repo/pool/instance
  type for log enrichment and metric labels), `UpdateJobMetrics` (timestamps,
  best-effort), then publishes: `JobsCompleted{pool, result, repo}` with
  `result = jobResult(status)`; `JobExecutionSeconds` and
  `RunnerExecutionSeconds{Arch,Vcpu,Spot,Result}` (via
  `fleet.GetInstanceSpec`) only when `DurationSeconds > 0`;
  `RunnerToolCacheMiss` per parsed `"<Tool>/<version>/<platform>"` key
  (version normalized to major.minor, OS prefix stripped from arch);
  `RunnerCacheInterception{status}`; and `CacheBytesStored` for v2
  agent-metered blob writes. Finally it deletes the runner config from the
  secrets store (`not found` swallowed).

## Talks To [coverage: high -- 5 sources]

Events handler:

- SQS events queue via `QueueAPI.ReceiveMessages` / `DeleteMessage`.
- SQS main queue via `JobRequeuer.SendMessage` (`SetJobQueue`) to re-queue
  interrupted jobs as `*queue.JobMessage`. A nil jobQueue is a hard error at
  requeue time, not a silent skip.
- DynamoDB jobs table via `DBAPI.MarkInstanceTerminating` and
  `DBAPI.GetJobByInstance`.
- `MetricsAPI`: `PublishSpotInterruption(family)`, `PublishJobRequeued`,
  `PublishCircuitBreakerTrip/Open`, `PublishQueueReceive`,
  `PublishMessageProcessingSeconds`, `PublishMessageDeletionFailure`.
- `CircuitBreakerAPI.RecordInterruption` + `IsOpen` per instance type (best
  effort; warns on error).
- `tracing.Tracer()` — `events.spot_interruption` span.

Termination handler:

- SQS termination queue via `QueueAPI.ReceiveMessages` / `DeleteMessage`.
- DynamoDB jobs table via `DBAPI`: `MarkJobComplete`, `MarkJobStarted`,
  `UpdateJobMetrics`, `GetJobByInstance`, `MarkJobRequeuedByJobID`.
- SQS main queue via `JobQueueAPI.SendMessage` (bootstrap-failure re-queue).
- `MetricsAPI`: `PublishJobCompleted`, `PublishRunnerConfirmed`,
  `PublishInstanceProvisionSeconds`, `PublishAgentBootstrapSeconds`,
  `PublishJobExecutionSeconds`, `PublishRunnerExecutionSeconds`,
  `PublishMessageProcessingSeconds`, `PublishJobRequeued`,
  `PublishSchedulingFailure`, `PublishRunnerToolCacheMiss`,
  `PublishRunnerCacheInterception`, `PublishCacheBytesStored`.
- `secrets.Store.Delete(ctx, instanceID)` to remove runner config (SSM or
  Vault backend, see `pkg/secrets`).
- `fleet.GetInstanceSpec` (instance catalog) for the billing dimensions.
- `tracing.Tracer()` — `termination.process`, `termination.bootstrap_failed`
  spans.

The message producer is the agent's telemetry
([pkg/agent/telemetry.go](../../pkg/agent/telemetry.go)): `JobStatus` fields
mirror `termination.Message` one-for-one, including the five `bootstrap_*`
segments carried on the started (not completion) signal.

## API Surface [coverage: medium -- 4 sources]

Events:

- `events.Handler`, `events.NewHandler(q QueueAPI, db DBAPI, m MetricsAPI, cfg *config.Config) *Handler`
- `(*Handler).SetJobQueue(q JobRequeuer)` — must be set before events flow
- `(*Handler).SetCircuitBreaker(cb CircuitBreakerAPI)`
- `(*Handler).Run(ctx context.Context)`
- Interfaces: `QueueAPI`, `JobRequeuer`, `DBAPI`, `MetricsAPI`, `CircuitBreakerAPI`
- Types: `EventBridgeEvent`, `SpotInterruptionDetail`, `StateChangeDetail`,
  `JobInfo`

Termination:

- `termination.Handler`, `termination.NewHandler(q QueueAPI, db DBAPI, m MetricsAPI, secretsStore secrets.Store, jobQueue JobQueueAPI, cfg *config.Config) *Handler`
- `(*Handler).Run(ctx context.Context)`
- Interfaces: `QueueAPI`, `DBAPI`, `JobQueueAPI`, `MetricsAPI`
- Type: `Message`
- Constants: `maxBootstrapRequeues = 2` (mirrors the webhook's
  `maxJobRetries` and housekeeping's `MaxRequeueRetries`), result enum
  `served`/`interrupted`/`timeout`/`error`

## Data [coverage: high -- 5 sources]

EventBridge envelope (`EventBridgeEvent`):
`version`, `id`, `detail-type`, `source`, `account`, `time`, `region`,
`resources []string`, `detail json.RawMessage`. The handler dispatches on
`detail-type`:

- `EC2 Spot Instance Interruption Warning` -> `SpotInterruptionDetail{InstanceID, InstanceAction}`
- `EC2 Instance State-change Notification` -> `StateChangeDetail{InstanceID, State}`

State changes no longer drive any metric: the `Instances` gauge is published
from the pool reconcile loop using authoritative per-pool counts (per-event
deltas would drift and lose state on restart). Known states are logged;
unknown states warn.

`JobInfo` (read from DynamoDB by `GetJobByInstance` / `GetJobByJobID`,
defined in `pkg/events`, shared with `pkg/termination` and the webhook):
`JobID int64`, `RunID int64`, `Repo`, `InstanceType`, `Pool`, `Spot bool`,
`RetryCount int`, `WarmPoolHit bool`, `CreatedAt time.Time` (assignment-time
`SaveJob` stamp; zero if unparseable). `WarmPoolHit` and `CreatedAt` were
added in PR #387 (2026-07-09) so runner confirmation can label and compute
`InstanceProvisionSeconds` without a second read. The re-queue message is a
`*queue.JobMessage` with `Spot=false`, `RetryCount=job.RetryCount+1`,
`ForceOnDemand=true`.

Termination `Message` (sent by the agent; JSON mirror of `agent.JobStatus`):

- `instance_id`, `job_id` (string, parsed via `strconv.ParseInt`)
- `status`: `started`, `success`, `failure`, `timeout`, `interrupted`
  (plus boot-shim `bootstrap_failed`, which carries only `instance_id`)
- `exit_code int`, `duration_seconds int`
- `started_at`, `completed_at` (`time.Time`)
- `error string`, `interrupted_by string` (optional)
- `tool_cache_misses []string` — `"<Tool>/<version>/<platform>"` keys
- `cache_interception string` — `engaged|failed|disabled`
- `cache_bytes_written int64` — v2 blob bytes metered on the runner
- `bootstrap_boot_seconds`, `bootstrap_config_seconds`,
  `bootstrap_runner_seconds`, `bootstrap_register_seconds`,
  `bootstrap_total_seconds` (`float64`, all `omitempty`) — added 2026-07-09;
  **additive**: an old agent omits them (decoded as zero → zero publishes),
  a new agent's extra JSON is ignored by an old orchestrator. Backward
  decode is pinned by a raw-JSON compatibility test in `handler_test.go`.

`jobResult(status)` maps the agent status onto the operational result enum:
`success → served`, `timeout → timeout`, `interrupted → interrupted`,
anything else → `error`. `served` means *our runner* ran the job to
completion and exited cleanly — reported even when the client's workflow
steps failed (the ephemeral actions-runner exits 0 whenever it operated
correctly because `ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED` is unset).

## Key Decisions [coverage: high -- 5 sources]

- 2-minute spot warning is treated as enough lead time to mark the instance
  terminating and re-queue the job; no in-place retry is attempted.
- Re-queued jobs always go on-demand: `Spot: false, ForceOnDemand: true` —
  in both the spot-interruption and bootstrap-failure paths — to avoid
  losing the job to a second interruption. `RetryCount` is incremented per
  re-queue so downstream consumers can cap retries (see the
  [two-track-reliability](../concepts/two-track-reliability.md) concept).
- Spot interruptions feed the circuit breaker per-`InstanceType`, and the
  handler surfaces the resulting breaker state as metrics
  (`CircuitBreakerOpen`/`Trip`) at the moment of interruption.
- The spot-interruption **metric is deferred and detached**: publishing
  after the critical work, best-effort, on a `context.WithoutCancel`
  context. A throttled CloudWatch call must never abort marking the
  instance terminating or re-queuing the job (the SQS message is deleted
  unconditionally, so an aborted handler would lose the job). The former
  `retryWithBackoff` wrapper is gone — ordering, not retries, is the
  protection now.
- Termination queue is a separate SQS queue from the events queue, with
  opposite delete semantics: the events handler deletes on every code path
  (via defer — EventBridge is not expected to redeliver), while the
  termination handler acks non-retryable garbage explicitly (no DLQ; an
  error return would redeliver for the full retention window) and returns
  errors only for transient DB/queue failures so SQS redelivers real work.
- `"started"` is a first-class state transition, not just a progress beacon
  (changed from the pre-2026-07 behavior of short-circuiting): it drives the
  launched → running conditional write, the `RunnerConfirmed` metric (whose
  flatline against `JobsAssigned` is the leading indicator of fleet-wide
  registration failure), and — since PR #387 — the provision and bootstrap
  latency metrics. The conditional write makes a late/duplicate "started" a
  no-op rather than resurrecting a terminal record.
- `InstanceProvisionSeconds` is measured at the **cross-instance
  rendezvous**: the jobs DB record joins the orchestrator-stamped assignment
  time (`created_at`) with the agent-stamped `StartedAt`, surfaced in one
  round trip via `MarkJobStarted`'s `ReturnValueAllNew`. `warm_pool` covers
  resume + bootstrap; `cold_start` covers post-CreateFleet through
  registration (CreateFleet itself is `FleetCreateSeconds`). Before #387 the
  metric existed but was never called.
- Bootstrap-failure recovery fails fast: `maxBootstrapRequeues = 2`
  deliberately mirrors the webhook's `maxJobRetries` and housekeeping's
  `MaxRequeueRetries` — a job whose instances keep failing to boot is
  almost always a bad AMI/agent rollout, so it stops re-queuing and emits
  `SchedulingFailure{bootstrap_failed}` for alerting instead of looping
  forever burning instances.
- The `jobResult` enum is *our* operational lifecycle, never derived from
  the GitHub workflow conclusion; the fulfillment SLA lives on
  assignment-based counters (`JobsAssigned` vs `SchedulingFailure`), not on
  completions.
- Both handlers detach in-flight work from the SIGTERM context
  (`context.WithoutCancel` + timeout) and drain via `wg.Wait()`, so
  shutdown completes work instead of abandoning it mid-write.
- Agent-observed signals are published orchestrator-side with
  orchestrator-owned label enums (bootstrap `Phase`, tool-cache
  major.minor normalization) — the agent never supplies metric label
  strings, a deliberate cardinality guard.
- Secrets cleanup is best-effort: `not found` / `NotFound` errors are
  swallowed so a redelivered termination message does not fail.

## Gotchas [coverage: high -- 5 sources]

- Race between spot interruption and natural job completion: both handlers
  can fire for the same instance. The events handler will still re-queue if
  `GetJobByInstance` returns a non-nil job with non-zero `JobID`/`RunID`, so
  the job DB record must be cleared promptly by termination processing to
  avoid spurious re-queues. `JobInfo` with `JobID == 0 || RunID == 0` is
  treated as "nothing to re-queue".
- Re-queued jobs lose context: the inline comment notes "Some fields
  (OriginalLabel, Region, etc.) are not stored in JobInfo. Re-queued jobs
  use basic config." Region-pinned or label-specific jobs may land on the
  wrong pool after a spot interruption.
- **Cross-clock guard silently drops observations.**
  `publishProvisionSeconds` compares an orchestrator clock (`created_at`)
  against an EC2 instance clock (`StartedAt`); skew is chrony-bounded but
  non-zero, so it publishes only when both timestamps are non-zero and the
  span is positive. A skewed or zero pair emits nothing — no error, no
  metric — so very fast warm-pool confirmations can be undercounted.
- **Two-halves deploy for the bootstrap metric.** The `bootstrap_*` fields
  are populated by the agent, which ships via the AMI cascade
  (`build-runner.yml` → `build-amis.yml`); the publishing side ships with
  the orchestrator image. Order-independent (zero-as-absent both ways), but
  `AgentBootstrapSeconds` flatlines until *both* halves are live — an empty
  panel right after an orchestrator deploy is expected, not a bug.
- A late `"started"` after completion is a `(nil, nil)` no-op — correct for
  state, but it also means `RunnerConfirmed`, `InstanceProvisionSeconds`,
  and `AgentBootstrapSeconds` are never emitted for that job. Bootstrap
  timings are only observed for jobs whose started signal wins the race
  with completion processing (in practice: all but pathological cases).
- Duplicate termination messages: SQS at-least-once delivery means the same
  termination can arrive twice. `MarkJobComplete`, `MarkJobStarted`, and
  `MarkJobRequeuedByJobID` are conditional writes (idempotent); secrets
  `Delete` hides "not found".
- The events handler deletes every message via defer regardless of
  processing result, so a failed spot-interruption handle will **not**
  retry via SQS — it relies on ordering (critical work before returns) and
  on EventBridge not re-delivering. The termination handler is the
  opposite: only transient failures redeliver, and there is **no DLQ** on
  the termination queue, which is why malformed messages must be acked, not
  errored.
- `bootstrap_failed` carries no job_id (the boot shim runs before the Go
  agent), so it must be handled before the job_id-requiring validation —
  a validation reorder would silently drop bootstrap recovery.
- EventBridge delivery delays can compress the 2-minute window: by the time
  the warning hits the events queue and is dequeued, less than 2 minutes may
  remain to drain.
- Receive in the events handler uses a 25s timeout (or remaining deadline)
  but long-polls for 20s, leaving a narrow 5s slack for the AWS SDK's HTTP
  layer.
- Concurrency cap is hard-coded at 5 in both handlers; bursts of
  interruptions during a regional spot reclamation will queue behind the
  semaphore, and the events handler additionally waits for each batch to
  finish before polling again.
- Empty `msg.Handle` short-circuits `processEvent` early (logs warning, no
  delete) and makes `ackMessage` a no-op in the termination handler; useful
  for in-process testing but means messages that somehow lack a handle in
  production are never removed.
- The `SpotInterruptions{Family}` label is resolved from the interrupted
  job's instance type and stays empty when no job was found (the backend
  drops the empty dimension) — the dimensionless series and the
  family-dimensioned series are distinct in CloudWatch. This split is also
  why the cost reporter's dimensionless query undercounts; see
  [observability](observability.md) Gotchas.

## Sources [coverage: high]

- [pkg/events/handler.go](../../pkg/events/handler.go)
- [pkg/termination/handler.go](../../pkg/termination/handler.go)
- [pkg/db/jobs.go](../../pkg/db/jobs.go)
- [pkg/agent/telemetry.go](../../pkg/agent/telemetry.go)
- [docs/METRICS.md](../../docs/METRICS.md)
