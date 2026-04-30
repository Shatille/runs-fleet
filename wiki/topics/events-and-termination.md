---
topic: Events + Termination Handlers
last_compiled: 2026-04-30
sources_count: 2
---

# Events + Termination Handlers

## Purpose [coverage: medium -- 2 sources]

`pkg/events` and `pkg/termination` host two independent SQS queue processors that
react to instance lifecycle signals.

- `pkg/events/handler.go` consumes EventBridge events forwarded into the events
  SQS queue: spot interruption warnings (`EC2 Spot Instance Interruption
  Warning`) and instance state-change notifications (`EC2 Instance State-change
  Notification`). Spot warnings are turned into a `MarkInstanceTerminating` write
  plus a job re-queue with `ForceOnDemand=true`.
- `pkg/termination/handler.go` consumes notifications produced by the agent
  itself when a job finishes (or fails, times out, or is interrupted). It marks
  the job complete in DynamoDB, publishes duration/success/failure metrics, and
  deletes the runner config from the secrets store.

## Architecture [coverage: medium -- 2 sources]

Both packages follow the same pattern: a `Handler` struct constructed via
`NewHandler`, exposing a single `Run(ctx context.Context)` loop that polls SQS
on a 1-second ticker (`eventsTickInterval`, `handlerTickInterval`) and fans out
work over a bounded semaphore (`maxConcurrency = 5`).

Events handler (`pkg/events/handler.go`):

- `Handler` holds `queueClient QueueAPI`, `dbClient DBAPI`, `metrics MetricsAPI`,
  `config *config.Config`, and an optional `circuitBreaker CircuitBreakerAPI`
  installed post-construction via `SetCircuitBreaker`.
- `Run` calls `ReceiveMessages(ctx, 10, 20)` (10 messages, 20s long poll). For
  each message it spawns a goroutine that takes a slot in `sem`, runs
  `processEvent` under a `config.MessageProcessTimeout` context, and recovers
  panics. The receive itself runs under a 25s timeout (or the remaining
  deadline, whichever is shorter). When no messages arrive, it sleeps a 160-240ms
  jitter before the next tick.
- `processEvent` always defers `DeleteMessage(handle)` and emits
  `PublishMessageDeletionFailure` if the delete fails. Body is unmarshaled into
  `EventBridgeEvent`, dispatched on `DetailType`.

Termination handler (`pkg/termination/handler.go`):

- `Handler` holds `queueClient QueueAPI`, `dbClient DBAPI`, `metrics MetricsAPI`,
  `secretsStore secrets.Store`, and `config *config.Config`.
- `Run` polls SQS (`ReceiveMessages(ctx, 10, 20)`) and dispatches each message
  to a goroutine bounded by the same 5-slot semaphore, with a per-message 30s
  timeout. On `ctx.Done()` it `wg.Wait()`s in-flight work before returning.
- `processMessage` -> `validateMessage` -> `processTermination` ->
  `DeleteMessage` only after success.

## Talks To [coverage: medium -- 2 sources]

Events handler:

- SQS events queue via `QueueAPI.ReceiveMessages` / `DeleteMessage`.
- SQS main queue via `QueueAPI.SendMessage` to re-queue interrupted jobs as
  `*queue.JobMessage`.
- DynamoDB jobs table via `DBAPI.MarkInstanceTerminating` and
  `DBAPI.GetJobByInstance`.
- `MetricsAPI` for `PublishSpotInterruption`, `PublishFleetSizeIncrement`,
  `PublishFleetSizeDecrement`, `PublishMessageDeletionFailure`.
- `CircuitBreakerAPI.RecordInterruption(ctx, instanceType)` (best effort; warns
  on error).

Termination handler:

- SQS termination queue via `QueueAPI.ReceiveMessages` / `DeleteMessage`.
- DynamoDB jobs table via `DBAPI.MarkJobComplete` and `DBAPI.UpdateJobMetrics`.
- `MetricsAPI` for `PublishJobDuration`, `PublishJobSuccess`,
  `PublishJobFailure`.
- `secrets.Store.Delete(ctx, instanceID)` to remove runner config (SSM or Vault
  backend, see `pkg/secrets`).

## API Surface [coverage: medium -- 2 sources]

Events:

- `events.Handler`, `events.NewHandler(q QueueAPI, db DBAPI, m MetricsAPI, cfg *config.Config) *Handler`
- `(*Handler).SetCircuitBreaker(cb CircuitBreakerAPI)`
- `(*Handler).Run(ctx context.Context)`
- Interfaces: `QueueAPI`, `DBAPI`, `MetricsAPI`, `CircuitBreakerAPI`
- Types: `EventBridgeEvent`, `SpotInterruptionDetail`, `StateChangeDetail`,
  `JobInfo`

Termination:

- `termination.Handler`, `termination.NewHandler(q QueueAPI, db DBAPI, m MetricsAPI, secretsStore secrets.Store, cfg *config.Config) *Handler`
- `(*Handler).Run(ctx context.Context)`
- Interfaces: `QueueAPI`, `DBAPI`, `MetricsAPI`
- Type: `Message`

## Data [coverage: medium -- 2 sources]

EventBridge envelope (`EventBridgeEvent`):
`version`, `id`, `detail-type`, `source`, `account`, `time`, `region`,
`resources []string`, `detail json.RawMessage`. The handler dispatches on
`detail-type`:

- `EC2 Spot Instance Interruption Warning` -> `SpotInterruptionDetail{InstanceID, InstanceAction}`
- `EC2 Instance State-change Notification` -> `StateChangeDetail{InstanceID, State}`

`StateChangeDetail.State` mapping:

- `running` -> `PublishFleetSizeIncrement`
- `stopped` / `terminated` -> `PublishFleetSizeDecrement`
- `pending` / `stopping` / `shutting-down` -> intermediate, no action
- anything else -> warn `unknown instance state`

`JobInfo` (read from DynamoDB by `GetJobByInstance`): `JobID int64`,
`RunID int64`, `Repo`, `InstanceType`, `Pool`, `Spot bool`, `RetryCount int`.
The re-queue message is a `*queue.JobMessage` with `Spot=false`,
`RetryCount=job.RetryCount+1`, `ForceOnDemand=true`.

Termination `Message` (sent by the agent):

- `instance_id`, `job_id` (string, parsed via `strconv.ParseInt`)
- `status`: `started`, `success`, `failure`, `timeout`, `interrupted`
- `exit_code int`, `duration_seconds int`
- `started_at`, `completed_at` (`time.Time`)
- `error string` (optional), `interrupted_by string` (optional)

## Key Decisions [coverage: medium -- 2 sources]

- 2-minute spot warning is treated as enough lead time to mark the instance
  terminating and re-queue the job; no in-place retry is attempted.
- Re-queued jobs always go on-demand: `Spot: false, ForceOnDemand: true`. This
  is a deliberate trade-off documented inline ("Force on-demand for retries
  after spot interruption") to avoid losing the job to a second interruption.
- `RetryCount` is incremented per re-queue so downstream consumers can cap
  retries.
- Spot interruptions feed the circuit breaker
  (`CircuitBreakerAPI.RecordInterruption`) per-`InstanceType`, so a flapping
  family can be temporarily steered around.
- Termination queue is a separate SQS queue from the events queue: agents post
  here directly, EventBridge does not, and message lifecycles differ (the
  termination handler deletes only on full success, while the events handler
  deletes on every code path via defer).
- Metric publishes are wrapped in `retryWithBackoff` (3 attempts, backoffs
  100ms / 500ms / 1s) for the events handler; transient CloudWatch errors do not
  drop spot-interruption signals.
- `processTermination` short-circuits on `status == "started"` and returns nil
  (these are progress beacons, not completions).
- Secrets cleanup is best-effort: `not found` / `NotFound` errors are swallowed
  so a redelivered termination message does not fail.

## Gotchas [coverage: medium -- 2 sources]

- Race between spot interruption and natural job completion: both handlers can
  fire for the same instance. The events handler will still re-queue if
  `GetJobByInstance` returns a non-nil job with non-zero `JobID`/`RunID`, so the
  job DB record must be cleared promptly by termination processing to avoid
  spurious re-queues. `JobInfo` with `JobID == 0 || RunID == 0` is treated as
  "nothing to re-queue".
- Re-queued jobs lose context: the inline comment notes "Some fields
  (OriginalLabel, Region, etc.) are not stored in JobInfo. Re-queued jobs use
  basic config." Region-pinned or label-specific jobs may land on the wrong
  pool after a spot interruption.
- Duplicate termination messages: SQS at-least-once delivery means the same
  termination can arrive twice. `MarkJobComplete` and secrets `Delete` must be
  idempotent; the handler hides "not found" errors from secrets but DB-level
  idempotency lives in `pkg/db`.
- The termination handler only calls `DeleteMessage` after `processTermination`
  succeeds, so a transient DynamoDB failure will redeliver the message after
  visibility timeout. The events handler always deletes (via defer) regardless
  of processing result, so a failed spot-interruption handle will not retry via
  SQS - it relies on the inner `retryWithBackoff` and on EventBridge not
  re-delivering.
- EventBridge delivery delays can compress the 2-minute window: by the time the
  warning hits the events queue and is dequeued, less than 2 minutes may remain
  to drain.
- Receive in the events handler uses a 25s timeout (or remaining deadline) but
  long-polls for 20s, leaving a narrow 5s slack for the AWS SDK's HTTP layer.
- Concurrency cap is hard-coded at 5 in both handlers; bursts of interruptions
  during a regional spot reclamation will queue behind the semaphore.
- Empty `msg.Handle` short-circuits `processEvent` early (logs warning, no
  delete) and skips the `DeleteMessage` call in `processMessage`; useful for
  in-process testing but means malformed messages never get removed if they
  somehow lack a handle in production.

## Sources [coverage: high]

- [pkg/events/handler.go](../../pkg/events/handler.go)
- [pkg/termination/handler.go](../../pkg/termination/handler.go)
