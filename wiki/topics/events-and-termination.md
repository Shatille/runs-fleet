---
topic: Events + Termination Handlers
last_compiled: 2026-08-21
sources_count: 6
---

# Events + Termination Handlers

## Purpose [coverage: high -- 6 sources]

`pkg/events` and `pkg/termination` host two independent SQS queue processors
that react to instance lifecycle signals.

- [pkg/events/handler.go](../../pkg/events/handler.go) consumes EventBridge
  events forwarded into the events SQS queue: spot interruption warnings
  (`EC2 Spot Instance Interruption Warning`) and instance state-change
  notifications (`EC2 Instance State-change Notification`). Spot warnings
  become a `MarkInstanceTerminating` write plus a job re-queue with
  `ForceOnDemand=true`, and — since PR #454 — a GitHub job **re-run** when
  the re-queue cannot possibly rescue the job. State changes are logged and
  otherwise discarded.
- [pkg/events/rerun.go](../../pkg/events/rerun.go) (new in PR #454, commit
  `d74efec`) is the recovery half: it waits for GitHub to conclude an
  interrupted job and, if the conclusion is `failure`, asks GitHub to re-run
  it.
- [pkg/termination/handler.go](../../pkg/termination/handler.go) consumes
  telemetry produced by the agent (and the boot shim) over the job lifecycle:
  `started` confirms the runner registered and transitions the job launched →
  running; `bootstrap_failed` recovers a job whose instance never started the
  agent; completion statuses (`success`/`failure`/`timeout`/`interrupted`)
  mark the job complete in DynamoDB, publish a broad set of completion and
  runner-health metrics, and delete the runner config from the secrets store.

The termination handler is the landing zone for every agent-observed signal
that rides the telemetry — tool-cache misses, cache-interception outcome,
buildx build-cache interception outcome, runner-log upload outcome, v2
cache-write bytes, and bootstrap phase timings. The agent has no metrics
client of its own; see [observability](observability.md).

## Architecture [coverage: high -- 6 sources]

Both packages follow the same pattern: a `Handler` struct constructed via
`NewHandler`, exposing a single `Run(ctx context.Context)` loop that polls SQS
on a 1-second ticker (`eventsTickInterval`, `handlerTickInterval` — vars, for
test override) and fans out work over a bounded semaphore
(`maxConcurrency = 5`).

**Events handler** ([pkg/events/handler.go](../../pkg/events/handler.go)):

- `Handler` holds `queueClient QueueAPI`, `dbClient DBAPI`,
  `metrics MetricsAPI`, `config *config.Config`, plus three dependencies
  installed post-construction: `jobQueue JobRequeuer` (`SetJobQueue`),
  `circuitBreaker CircuitBreakerAPI` (`SetCircuitBreaker`), and
  `gitHub JobRerunner` (`SetGitHub`, defined in `rerun.go`).
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
  from slot acquisition. An empty poll sleeps only a 160–240ms jitter
  ([handler.go:195](../../pkg/events/handler.go)).
- `processEvent` always defers `DeleteMessage(handle)` and emits
  `PublishMessageDeletionFailure` if the delete fails. The body is
  unmarshaled into `EventBridgeEvent` and dispatched on `DetailType`; an
  unrecognised `detail-type` warns and returns (still deleted, via the
  defer).
- `handleSpotInterruption` opens an `events.spot_interruption` trace span,
  marks the instance terminating, resolves the job, records the interruption
  in the circuit breaker (publishing `CircuitBreakerOpen`/`Trip` from its
  state), re-queues the job on-demand, publishes
  `JobRequeued{reason=spot_interruption}`, and finally calls
  `recoverInterruptedJob`. The `SpotInterruptions{Family}` counter is
  published from a **deferred** best-effort block with a detached context so a
  throttled CloudWatch call can never abort the critical work — and still
  fires when the spot reclaim tore down the handler's own context.
- `handleStateChange` unmarshals `StateChangeDetail`, stashes the instance id
  on the log context, logs the state, and **does nothing else**: known states
  (`running`, `stopped`, `terminated`, `pending`, `stopping`,
  `shutting-down`) fall through an empty switch case; anything else warns.

**Re-run recovery** ([pkg/events/rerun.go](../../pkg/events/rerun.go)):

- `JobRerunner` is a two-method interface — `GetWorkflowJobState(ctx, repo,
  jobID) (WorkflowJobState, error)` and `RerunJob(ctx, repo, jobID) error` —
  satisfied in `cmd/server` by `eventsRerunAdapter` over `*gh.Client`
  (`GetWorkflowJobByID` + `RerunJob`, the latter wrapping GitHub's
  `RerunJobByID`).
- `recoverInterruptedJob` gates hard before doing anything: nil client, nil
  job, zero `JobID`, or empty `Repo` → no-op; `job.RetryCount > 0` → no-op
  ("one recovery per job: a reclaim during the re-run must not start
  another").
- `awaitConcludedJob` polls `GetWorkflowJobState` every
  `rerunPollInterval = 5s` until `Status == "completed"` or
  `rerunWaitBudget = 45s` elapses. The budget is deliberately short: GitHub
  concluded a reclaimed job a median of 29s after job start but p90 at 169s,
  and the events worker is blocked for the duration, so it must leave room
  inside `config.MessageProcessTimeout` (90s). A job still running when the
  budget expires is left to the re-queue.
- Only `Conclusion == "failure"` triggers `RerunJob`. A `success` means the
  job finished despite the reclaim; a `cancelled` is somebody's deliberate
  stop. Errors are returned for logging only — the interruption path must
  never fail over a recovery attempt.

**Termination handler**
([pkg/termination/handler.go](../../pkg/termination/handler.go)):

- `Handler` holds `queueClient QueueAPI`, `dbClient DBAPI`,
  `metrics MetricsAPI`, `secretsStore secrets.Store`, `jobQueue JobQueueAPI`
  (nilable — bootstrap-failure re-queue is then skipped),
  `githubChecker GitHubJobStatusChecker` (`SetGitHubJobChecker`; nil disables
  the still-queued check), and `config *config.Config`.
- `Run` polls identically; each message gets a 30s timeout on a
  `context.WithoutCancel` context, and `MessageProcessingSeconds` (queue
  `termination`) is published per message. On `ctx.Done()` it `wg.Wait()`s
  in-flight work before returning.
- `processMessage` is a dispatch tree with explicit ack semantics
  (`ackMessage` = `DeleteMessage`, no-op on empty handle):
  - empty body / unparseable JSON → warn + **ack** (non-retryable; the
    termination queue has no DLQ, so returning an error would redeliver
    garbage for the full retention window);
  - `status == "bootstrap_failed"` → `processBootstrapFailure`
    (instance-scoped: the boot shim `scripts/cloud-init-boot.sh` has no
    job_id) then ack; transient DB/queue errors are returned so SQS
    redelivers;
  - `status == "started"` → validate, `confirmRunnerStarted`, ack; a DB error
    is returned for redelivery so the unconfirmed-runner watchdog does not
    later mistake a healthy job for a never-confirmed one;
  - otherwise → validate (instance_id, job_id, status required; invalid →
    warn + ack), `processTermination`, ack only after success.
- `confirmRunnerStarted` calls `MarkJobStarted(jobID, msg.StartedAt)` — a
  conditional launched → running transition with `ReturnValueAllNew`
  ([pkg/db/jobs.go](../../pkg/db/jobs.go)), so a late "started" after the job
  completed or was recovered is a `(nil, nil)` no-op. On a real transition it
  publishes `RunnerConfirmed{Pool}`, then `publishProvisionSeconds` and
  `publishBootstrapSeconds`.
- `publishProvisionSeconds` computes `StartedAt − record.CreatedAt` and
  labels the source `hot_pool` (a running spare, no boot — `HotPoolHit`),
  else `warm_pool` (`WarmPoolHit`, i.e. a stopped-instance resume), else
  `cold_start`. It publishes only when both timestamps are non-zero and the
  span is positive.
- `publishBootstrapSeconds` emits one `AgentBootstrapSeconds{Pool,Phase}` per
  positive `bootstrap_*` segment, over an orchestrator-fixed phase enum
  (`boot`, `config`, `runner_download`, `registration`, `total`).
- `processBootstrapFailure` resolves the job by instance, and if
  `RetryCount < maxBootstrapRequeues (= 2)` marks it requeued via the
  idempotent `MarkJobRequeuedByJobID` (a redelivery or the watchdog cannot
  double-enqueue) and sends a `Spot=false, ForceOnDemand=true` re-queue,
  publishing `JobsRequeued{reason=bootstrap_failed}`. On exhaustion it gives
  up loudly with `SchedulingFailure{task_type=bootstrap_failed}`.
  Runner-config cleanup is best-effort.
- `redispatchIfStillQueued` runs **before** `MarkJobComplete`: the agent's
  `job_id` is its boot-time config, not necessarily the job it ran (GitHub
  hands queued jobs to any label-matched runner). If GitHub still reports the
  configured job `queued`, closing the record here would orphan it. It flips
  the record to `requeued` **before** sending the message (a long-polling
  worker beats a follow-up write), sends regardless of who won the
  transition, deletes this instance's config, and reports `handled=true`.
  Everything inconclusive — nil checker, nil queue, DB pre-read failure,
  unknown record, GitHub API error, status ≠ `queued` — fails open to the
  normal completion path. On budget exhaustion it deliberately does **not**
  complete the record (that would corrupt telemetry and hide the job from
  every reaper) and instead emits
  `SchedulingFailure{task_type=job_still_queued}`, leaving the record for the
  orphan sweep.
- `processTermination` opens a `termination.process` span, calls
  `MarkJobComplete` (`ReturnValueAllNew` echoes run_id/repo/pool/instance
  type for log enrichment and metric labels — a `nil` return means this
  instance no longer owns the record, so the completion is skipped and only
  the config is deleted), `UpdateJobMetrics` (timestamps, best-effort), then
  publishes the metric set below and finally deletes the runner config.

Completion metrics published by `processTermination`, in order:

| Metric | Condition |
|---|---|
| `JobsCompleted{pool, result, repo}` | always (`result = jobResult(status)`) |
| `JobExecutionSeconds{pool, result}` | `DurationSeconds > 0` |
| `RunnerExecutionSeconds{Arch,Vcpu,Spot,Result}` | `DurationSeconds > 0` and `fleet.GetInstanceSpec` knows the type |
| `RunnerToolCacheMiss{tool, version, arch}` | one per parseable `tool_cache_misses` key |
| `RunnerCacheInterception{status}` | `cache_interception != ""` |
| `RunnerBuildCacheInterception{status}` | `build_cache_interception != ""` |
| `RunnerLogUpload{status}` | `log_upload != ""` |
| `CacheBytesStored{bytes}` | `cache_bytes_written > 0` |

`parseToolCacheMiss(key)` splits an agent key `"<Tool>/<version>/<platform>"`
on `/` and requires **exactly** three non-empty segments (agent
`SnapshotToolCache` only emits two-separator paths, so this is an exact
accept/reject and a tampered key with extra slashes is dropped). It then:
strips a `linux-` / `darwin-` / `windows-` prefix from the platform segment so
the `Arch` dimension is consistently `x64`/`arm64`; truncates the version at
the first `-` or `+` (dropping any build suffix, e.g. `21.0.4-7` → `21.0.4`);
and keeps only `major.minor` (`3.10.14` → `3.10`, `21.0.4` → `21.0`), falling
back to the single segment when there is no dot. This is a cardinality guard
([handler.go](../../pkg/termination/handler.go), `parseToolCacheMiss`).

## Talks To [coverage: high -- 6 sources]

Events handler:

- SQS events queue via `QueueAPI.ReceiveMessages` / `DeleteMessage`.
- SQS main queue via `JobRequeuer.SendMessage` (`SetJobQueue`) to re-queue
  interrupted jobs as `*queue.JobMessage`. A nil jobQueue is a hard error at
  requeue time, not a silent skip.
- DynamoDB jobs table via `DBAPI.MarkInstanceTerminating` and
  `DBAPI.GetJobByInstance`.
- **GitHub** via `JobRerunner` (`SetGitHub`) — `GetWorkflowJobState` and
  `RerunJob`, wired in `cmd/server` as `eventsRerunAdapter` over the shared
  `*gh.Client`. Nil when GitHub App credentials are unset, in which case
  re-run recovery is silently skipped.
- `MetricsAPI`: `PublishSpotInterruption(family)`, `PublishJobRequeued`,
  `PublishCircuitBreakerTrip/Open`, `PublishQueueReceive`,
  `PublishMessageProcessingSeconds`, `PublishMessageDeletionFailure`.
- `CircuitBreakerAPI.RecordInterruption` + `IsOpen` per instance type (best
  effort; warns on error).
- `tracing.Tracer()` — `events.spot_interruption` span.

Termination handler:

- SQS termination queue via `QueueAPI.ReceiveMessages` / `DeleteMessage`.
- DynamoDB jobs table via `DBAPI`: `MarkJobComplete`, `MarkJobStarted`,
  `UpdateJobMetrics`, `GetJobByInstance`, `GetJobByJobID`,
  `MarkJobRequeuedByJobID`.
- SQS main queue via `JobQueueAPI.SendMessage` (bootstrap-failure and
  still-queued re-dispatch).
- **GitHub** via `GitHubJobStatusChecker.GetWorkflowJobStatus` — deliberately
  narrower than housekeeping's checker, which collapses the
  `queued`/`in_progress` distinction this handler depends on.
- `MetricsAPI` (14 methods): `PublishJobCompleted`, `PublishRunnerConfirmed`,
  `PublishInstanceProvisionSeconds`, `PublishAgentBootstrapSeconds`,
  `PublishJobExecutionSeconds`, `PublishMessageProcessingSeconds`,
  `PublishJobRequeued`, `PublishSchedulingFailure`,
  `PublishRunnerExecutionSeconds`, `PublishRunnerToolCacheMiss`,
  `PublishRunnerCacheInterception`, `PublishRunnerBuildCacheInterception`,
  `PublishRunnerLogUpload`, `PublishCacheBytesStored`.
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
- `(*Handler).SetGitHub(gh JobRerunner)` — enables re-run recovery
- `(*Handler).Run(ctx context.Context)`
- Interfaces: `QueueAPI`, `JobRequeuer`, `DBAPI`, `MetricsAPI`,
  `CircuitBreakerAPI`, `JobRerunner`
- Types: `EventBridgeEvent`, `SpotInterruptionDetail`, `StateChangeDetail`,
  `JobInfo`, `WorkflowJobState{Status, Conclusion}`
- Tunables (vars, test-overridable): `eventsTickInterval = 1s`,
  `rerunWaitBudget = 45s`, `rerunPollInterval = 5s`

Termination:

- `termination.Handler`, `termination.NewHandler(q QueueAPI, db DBAPI, m MetricsAPI, secretsStore secrets.Store, jobQueue JobQueueAPI, cfg *config.Config) *Handler`
- `(*Handler).SetGitHubJobChecker(c GitHubJobStatusChecker)`
- `(*Handler).Run(ctx context.Context)`
- Interfaces: `QueueAPI`, `DBAPI`, `JobQueueAPI`, `MetricsAPI`,
  `GitHubJobStatusChecker`
- Type: `Message`
- Constants: `maxBootstrapRequeues = 2` (mirrors the webhook's
  `maxJobRetries` and housekeeping's `MaxRequeueRetries`); requeue reasons
  `bootstrap_failed` / `job_still_queued`; provision sources
  `warm_pool` / `cold_start` / `hot_pool`; result enum
  `served`/`interrupted`/`timeout`/`error`

## Data [coverage: high -- 5 sources]

EventBridge envelope (`EventBridgeEvent`):
`version`, `id`, `detail-type`, `source`, `account`, `time`, `region`,
`resources []string`, `detail json.RawMessage`. The handler dispatches on
`detail-type`:

- `EC2 Spot Instance Interruption Warning` → `SpotInterruptionDetail{InstanceID, InstanceAction}`
- `EC2 Instance State-change Notification` → `StateChangeDetail{InstanceID, State}`

`JobInfo` (read from DynamoDB by `GetJobByInstance` / `GetJobByJobID`,
defined in `pkg/events`, shared with `pkg/termination` and the webhook):
`JobID int64`, `RunID int64`, `Repo`, `InstanceID` (the instance the record
is currently bound to; empty mid-reclaim), `InstanceType`, `Pool`,
`Spot bool`, `RetryCount int`, `WarmPoolHit bool`, `HotPoolHit bool`
(served by a RUNNING hot-pool spare, no boot), `CreatedAt time.Time`
(assignment-time `SaveJob` stamp; zero if unparseable), `OriginalLabel`
(the `runs-on` label the workflow asked for — a requeue must re-register
under it, since GitHub matches runners by exact label set). The re-queue
message is a `*queue.JobMessage` with `Spot=false`,
`RetryCount=job.RetryCount+1`, `ForceOnDemand=true`, and the carried
`OriginalLabel`.

Termination `Message` (sent by the agent; JSON mirror of `agent.JobStatus`):

- `instance_id`, `job_id` (string, parsed via `strconv.ParseInt`)
- `status`: `started`, `success`, `failure`, `timeout`, `interrupted`
  (plus boot-shim `bootstrap_failed`, which carries only `instance_id`)
- `exit_code int`, `duration_seconds int`
- `started_at`, `completed_at` (`time.Time`)
- `error string`, `interrupted_by string` (optional)
- `tool_cache_misses []string` — `"<Tool>/<version>/<platform>"` keys
- `cache_interception string` — `engaged|failed|disabled`
- `build_cache_interception string` — `engaged|skipped|failed|disabled`
  (buildx layer-cache shim; absent from a pre-rollout agent)
- `log_upload string` — runner-log upload outcome (absent from a
  pre-rollout agent)
- `cache_bytes_written int64` — v2 blob bytes metered on the runner (the
  blob PUT bypasses the orchestrator, so it cannot be counted server-side)
- `bootstrap_boot_seconds`, `bootstrap_config_seconds`,
  `bootstrap_runner_seconds`, `bootstrap_register_seconds`,
  `bootstrap_total_seconds` (`float64`, all `omitempty`) —
  **additive**: an old agent omits them (decoded as zero → no publish),
  a new agent's extra JSON is ignored by an old orchestrator. Backward
  decode is pinned by a raw-JSON compatibility test in `handler_test.go`.

`jobResult(status)` maps the agent status onto the operational result enum:
`success → served`, `timeout → timeout`, `interrupted → interrupted`,
anything else → `error`. `served` means *our runner* ran the job to
completion and exited cleanly — reported even when the client's workflow
steps failed (the ephemeral actions-runner exits 0 whenever it operated
correctly, because `ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED` is unset).

## Key Decisions [coverage: high -- 6 sources]

- **2026-08 (PR #454): a re-queue is not enough, so re-run.** Registration
  binds a runner to a *label*, not a job. By the time a replacement runner
  registers, GitHub has often already concluded the reclaimed job failed —
  and a concluded job is dispatchable to nobody. The interruption path
  therefore does both: re-queue first (which rescues the job GitHub has not
  yet given up on) and then watch for the failure conclusion and re-run.
  Safety comes from the call site, not from a check: only the
  spot-interruption path calls it, so the job was killed mid-flight and never
  succeeded — a re-run repeats an aborted attempt rather than duplicating
  accepted work. `RetryCount > 0` caps it at one recovery per job.
- **The re-run wait deliberately skips the slow tail.** `rerunWaitBudget`
  is 45s against a p90-of-169s conclusion latency, because the events worker
  is blocked for the whole wait and must leave room inside the 90s
  `MessageProcessTimeout`. Spending the full budget would cascade deadline
  errors across the interruption path; a job still running at expiry is left
  to the re-queue, which was already the mechanism for that case.
- **Re-run targets one job, not the run.** `pkg/github/rerun.go` uses
  `RerunJobByID` rather than the run-level rerun-failed-jobs endpoint, which
  would also re-run jobs that failed for real. Dependent jobs still come
  along, which is what recovers the gate jobs a reclaim cascades into.
- **Termination checks GitHub before closing a record.** A runner's
  configured `job_id` is not proof it ran that job. `redispatchIfStillQueued`
  makes the still-queued case a re-dispatch instead of a completion, and
  flips the DB record claimable *before* the message is sent so a
  long-polling worker cannot receive a message whose record still reads
  `running` (which `ClaimJob` would reject as already-claimed, dropping the
  message without a trace).
- **On re-dispatch budget exhaustion the record is left alone.** Completing a
  never-run job with this agent's status would corrupt telemetry *and* hide
  the job from every reaper. Instead the record stays running/launched with a
  gone instance, which the orphan sweep marks orphaned, and
  `SchedulingFailure{job_still_queued}` fires for alerting.
- 2-minute spot warning is treated as enough lead time to mark the instance
  terminating and re-queue the job; no in-place retry is attempted.
- Re-queued jobs always go on-demand: `Spot: false, ForceOnDemand: true` — in
  the spot-interruption, bootstrap-failure, and still-queued paths alike — to
  avoid losing the job to a second interruption. `RetryCount` is incremented
  per re-queue so downstream consumers can cap retries (see the
  [two-track-reliability](../concepts/two-track-reliability.md) concept).
- Spot interruptions feed the circuit breaker per-`InstanceType`, and the
  handler surfaces the resulting breaker state as metrics
  (`CircuitBreakerOpen`/`Trip`) at the moment of interruption.
- **The spot-interruption metric is deferred and detached**: published
  after the critical work, best-effort, on a `context.WithoutCancel`
  context. A throttled CloudWatch call must never abort marking the instance
  terminating or re-queuing the job (the SQS message is deleted
  unconditionally, so an aborted handler would lose the job).
- Termination queue is a separate SQS queue from the events queue, with
  opposite delete semantics: the events handler deletes on every code path
  (via defer — EventBridge is not expected to redeliver), while the
  termination handler acks non-retryable garbage explicitly (no DLQ; an
  error return would redeliver for the full retention window) and returns
  errors only for transient DB/queue failures so SQS redelivers real work.
- `"started"` is a first-class state transition, not a progress beacon: it
  drives the launched → running conditional write, the `RunnerConfirmed`
  metric (whose flatline against `JobsAssigned` is the leading indicator of
  fleet-wide registration failure), and the provision/bootstrap latency
  metrics. The conditional write makes a late/duplicate "started" a no-op
  rather than resurrecting a terminal record.
- `InstanceProvisionSeconds` is measured at the **cross-instance
  rendezvous**: the jobs DB record joins the orchestrator-stamped assignment
  time (`created_at`) with the agent-stamped `StartedAt`, surfaced in one
  round trip via `MarkJobStarted`'s `ReturnValueAllNew`. `hot_pool` is the
  more specific label for a warm-pool hit served by a *running* spare, so
  the sub-10s no-boot cohort is observable on its own.
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

## Gotchas [coverage: high -- 6 sources]

- **EC2 state-change events arrive and are thrown away.** `handleStateChange`
  logs the state and returns; the known-state switch case is empty by design
  (the `Instances` gauge comes from the pool reconcile loop's authoritative
  per-pool counts, because per-event deltas drift and lose state on restart).
  But these events carry the one thing cost attribution lacks: near-exact
  instance start/stop spans, independent of any job record. Nothing in this
  repo consumes them, and the EventBridge rule that routes them lives in a
  separate IaC repository — so the fix is cross-repo, not a local edit. Until
  then, fleet cost is sampled rather than derived (see
  [observability](observability.md)).
- **`QueueReceive{empty}` fires on every empty poll, ~4x/second-of-idle.**
  The events loop backs off only a 160–240ms jitter after an empty receive
  ([handler.go:195](../../pkg/events/handler.go)), so an idle events queue
  produces a steady stream of counter increments — the single highest-volume
  emitter in the events path, and one reason CloudWatch metrics now ship
  disabled (see [project-overview](project-overview.md) Key Decisions).
- **Re-run recovery is silently absent without GitHub App credentials.**
  `SetGitHub` is only called in `cmd/server` when `initGitHubClient` returned
  non-nil; otherwise `recoverInterruptedJob` no-ops on its nil check and a
  reclaim-killed job is left to the re-queue alone.
- **`recoverInterruptedJob` blocks the events worker for up to 45s.** It runs
  inline at the end of `handleSpotInterruption`, inside one of five
  semaphore slots. A regional spot reclamation storm therefore serialises
  worse than the raw concurrency cap suggests, because each slot may sit in
  the re-run poll loop.
- **A re-run is not a re-queue and does not update the DB record.**
  `RerunJob` asks GitHub to create a fresh attempt; the runs-fleet job record
  for the reclaimed attempt stays wherever the re-queue left it. Job-count
  and cost telemetry will see both attempts.
- Race between spot interruption and natural job completion: both handlers
  can fire for the same instance. The events handler will still re-queue if
  `GetJobByInstance` returns a non-nil job with non-zero `JobID`/`RunID`, so
  the job DB record must be cleared promptly by termination processing to
  avoid spurious re-queues. `JobInfo` with `JobID == 0 || RunID == 0` is
  treated as "nothing to re-queue".
- **`Region` is still not carried on a re-queue.** `JobInfo` has no region
  field, so a re-queued job uses basic config
  ([handler.go:401](../../pkg/events/handler.go)). `OriginalLabel` *is*
  carried now (PR #435), so the label-mismatch starvation bug is fixed, but
  region pinning remains unrepresentable.
- **Cross-clock guard silently drops observations.**
  `publishProvisionSeconds` compares an orchestrator clock (`created_at`)
  against an EC2 instance clock (`StartedAt`); skew is chrony-bounded but
  non-zero, so it publishes only when both timestamps are non-zero and the
  span is positive. A skewed or zero pair emits nothing — no error, no
  metric — so very fast hot-pool confirmations can be undercounted.
- **Two-halves deploy for the agent-reported metrics.** The `bootstrap_*`,
  `build_cache_interception`, and `log_upload` fields are populated by the
  agent, which ships via the AMI cascade (`build-runner.yml` →
  `build-amis.yml`); the publishing side ships with the orchestrator image.
  Order-independent (absent-as-zero / empty-string both ways), but the
  corresponding metrics flatline until *both* halves are live — an empty
  panel right after an orchestrator deploy is expected, not a bug.
- A late `"started"` after completion is a `(nil, nil)` no-op — correct for
  state, but it also means `RunnerConfirmed`, `InstanceProvisionSeconds`,
  and `AgentBootstrapSeconds` are never emitted for that job.
- **A malformed tool-cache key is dropped without a trace.**
  `parseToolCacheMiss` returns `ok=false` for anything that is not exactly
  three non-empty slash-separated segments, and the caller `continue`s with
  no log line. A future agent that changes the key shape would silently zero
  the tool-cache-miss metric.
- **The tool-cache metric collapses the patch version.** `3.10.14` and
  `3.10.2` are one series (`3.10`), so it tells you *which minor* to bake,
  not which exact release. Cache keys must be read from each `setup-*`
  action's source, not from release artifacts.
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
- Concurrency cap is hard-coded at 5 in both handlers; the events handler
  additionally waits for each batch to finish before polling again.
- Empty `msg.Handle` short-circuits `processEvent` early (logs warning, no
  delete) and makes `ackMessage` a no-op in the termination handler; useful
  for in-process testing but means messages that somehow lack a handle in
  production are never removed.
- The `SpotInterruptions{Family}` label is resolved from the interrupted
  job's instance type and stays empty when no job was found (the backend
  drops the empty dimension) — the dimensionless series and the
  family-dimensioned series are distinct in CloudWatch.

## Sources [coverage: high]

- [pkg/events/handler.go](../../pkg/events/handler.go)
- [pkg/events/rerun.go](../../pkg/events/rerun.go)
- [pkg/termination/handler.go](../../pkg/termination/handler.go)
- [pkg/github/rerun.go](../../pkg/github/rerun.go)
- [pkg/db/jobs.go](../../pkg/db/jobs.go)
- [docs/METRICS.md](../../docs/METRICS.md)
