---
topic: Queue Processing (SQS)
last_compiled: 2026-07-03
sources_count: 6
---

# Queue Processing (SQS)

## Purpose [coverage: high -- 6 sources]

`pkg/queue` decouples webhook ingestion from instance provisioning behind a
single `Queue` interface backed by AWS SQS FIFO. Producers (webhook handler,
events handler, housekeeping requeuer) call `SendMessage`; consumers
(`internal/worker` loops, `pkg/events`, `pkg/termination`) call
`ReceiveMessages` / `DeleteMessage` to claim and acknowledge work. The
package preserves FIFO semantics per message group and carries
OpenTelemetry trace context across the queue boundary (as an SQS message
attribute) so spans stitch back together on the consumer side.

As of 2026-06 this is the only backend: the K8s compute provider and its
Valkey Streams queue implementation were removed upstream, and `pkg/queue`
now contains a single `Queue` implementation (`SQSClient`). See Key
Decisions for the removal.

## Architecture [coverage: high -- 6 sources]

The interface is intentionally minimal — three methods plus an optional
`Pinger` for health checks.

- [pkg/queue/interface.go](../../pkg/queue/interface.go) defines `Queue`,
  `Message`, `JobMessage`, and the optional `Pinger` interface.
- [pkg/queue/sqs.go](../../pkg/queue/sqs.go) implements `SQSClient` against
  `github.com/aws/aws-sdk-go-v2/service/sqs`. One client wraps a single
  queue URL. `cmd/server/main.go` constructs three `Queue`-typed clients:
  main job queue (`jobQueue`), events queue, and termination queue.

`SQSClient` has a compile-time interface check
(`var _ Queue = (*SQSClient)(nil)`).

Not everything that touches SQS goes through this package: `pkg/housekeeping`
talks to the raw AWS SDK `sqs` package directly for operator-level queue
management (DLQ redrive via `StartMessageMoveTask`, queue depth via
`GetQueueAttributes`) rather than through `queue.Queue`. The admin UI's
queue-monitoring handler (`pkg/admin`) similarly calls `sqs.NewFromConfig`
directly to read queue attributes for the pool and housekeeping queue URLs.
`pkg/queue` is the transport for job messages specifically, not a wrapper
for every SQS interaction in the codebase.

## Talks To [coverage: high -- 6 sources]

- **AWS SQS** via `aws-sdk-go-v2/service/sqs` (`SendMessage`,
  `ReceiveMessage`, `DeleteMessage`). The `SQSAPI` interface narrows the SDK
  surface to those three operations for testability.
- **`pkg/config`** supplies the queue URLs consumed by `queue.NewClient` /
  `queue.NewSQSClient`: `RUNS_FLEET_QUEUE_URL` (main),
  `RUNS_FLEET_EVENTS_QUEUE_URL`, `RUNS_FLEET_TERMINATION_QUEUE_URL`. The
  pool queue (`RUNS_FLEET_POOL_QUEUE_URL`) and housekeeping queue
  (`RUNS_FLEET_HOUSEKEEPING_QUEUE_URL`) URLs are also loaded into
  `config.Config` but are read by the admin monitoring handler and pool
  batch processing rather than instantiated as `pkg/queue.Queue` clients.
- **OpenTelemetry** via `pkg/tracing.InjectTraceContext` (producer side,
  sets the `Traceparent` message attribute) and the consumer's trace
  extraction from `Message.Attributes`.
- **Producers onto the main queue**: `internal/handler/webhook.go`
  (`HandleWorkflowJobQueued`, `HandleJobFailure`); `internal/worker/ec2.go`
  (spot-interruption / on-demand-fallback requeue); `pkg/events` and
  `pkg/termination` (requeue on spot warning / termination, via the
  `jobQueue` injected into each handler); `pkg/housekeeping`
  (`requeue.go`, `unconfirmed.go`) via the `JobRequeuer` interface
  (`SendMessage`), satisfied by `*SQSClient`. All of these share the single
  `jobQueue` client — there is no separate producer client per caller.
- **Consumers**: `internal/worker` (`RunWorkerLoop` and variants) drive
  `ReceiveMessages`/`DeleteMessage` against `jobQueue` for the EC2, direct,
  and warm-pool worker loops; `pkg/events.Handler` and
  `pkg/termination.Handler` consume their own queues (events, termination)
  through the same `Queue`-shaped interfaces (declared locally in each
  package, satisfied structurally by `*SQSClient`), and also act as
  producers back onto the main queue for requeues.
- **Bypasses `pkg/queue` entirely**: `pkg/agent/telemetry.go` (the EC2 agent
  binary) calls the raw AWS SDK `sqs.Client.SendMessage` directly to post
  job telemetry, rather than going through `queue.Queue` — the agent binary
  doesn't depend on `pkg/queue` at all.
- **Pool and housekeeping queues are not consumed anywhere.**
  `RUNS_FLEET_POOL_QUEUE_URL` and `RUNS_FLEET_HOUSEKEEPING_QUEUE_URL` are
  loaded into `config.Config` and surfaced read-only to the admin
  queue-depth dashboard (via a raw `sqs.Client`), but no `queue.Queue`
  client is ever constructed for either. Pool-labeled jobs route through
  the same main `jobQueue` (see `HandleWorkflowJobQueued`), and pool
  reconciliation runs on a 60s timer rather than draining a queue — the
  pool/housekeeping queue infrastructure exists in config and Terraform but
  is effectively vestigial or reserved for future use as far as `pkg/queue`
  is concerned.

## API Surface [coverage: high -- 6 sources]

The `Queue` interface ([pkg/queue/interface.go](../../pkg/queue/interface.go)):

```go
type Queue interface {
    SendMessage(ctx context.Context, job *JobMessage) error
    ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]Message, error)
    DeleteMessage(ctx context.Context, handle string) error
}

type Pinger interface {
    Ping(ctx context.Context) error
}
```

`Message` is the transport envelope:

```go
type Message struct {
    ID         string            // SQS MessageId (may be empty)
    Body       string            // JSON-encoded JobMessage
    Handle     string            // SQS ReceiptHandle
    Attributes map[string]string // trace context, etc.
    SentAt     time.Time         // from SQS SentTimestamp; zero if absent
}
```

Constructors and helpers ([pkg/queue/sqs.go](../../pkg/queue/sqs.go)):

- `NewSQSClient(cfg aws.Config, queueURL string) *SQSClient`
- `NewClient(cfg aws.Config, queueURL string) *SQSClient` — alias for
  `NewSQSClient`, kept for backward compatibility (used in `cmd/server/main.go`)
- `NewSQSClientWithAPI(api SQSAPI, queueURL string) *SQSClient` — test seam
  over the narrowed `SQSAPI` interface

`SQSClient` has no `Close()` — it holds no long-lived connection beyond the
SDK's HTTP client.

## Data [coverage: high -- 6 sources]

`JobMessage` ([pkg/queue/interface.go](../../pkg/queue/interface.go)) is the
canonical job envelope, JSON-marshaled into `Message.Body` on send and
unmarshaled by the consumer:

- Identity: `JobID`, `RunID`, `Repo`, `OriginalLabel`
- Compute spec: `InstanceType`, `InstanceTypes[]`, `Pool`, `Spot`,
  `ForceOnDemand`, `Arch`, `StorageGiB`
- Flexible matching: `CPUMin`, `CPUMax`, `RAMMin`, `RAMMax`, `Families[]`, `Gen`
- Retry: `RetryCount`
- Tracing: `Traceparent` (W3C traceparent string, not split trace/span/parent
  fields)

`SendMessage` requires `JobID != 0`, returning an error otherwise.

**SQS FIFO message group / dedup** (only applied when `queueURL` ends in
`.fifo`):

- `MessageGroupId = fmt.Sprintf("%d", job.JobID)` — grouped by job, not run.
  See Key Decisions for why.
- `MessageDeduplicationId = sha256(fmt.Sprintf("%d-%d", JobID, RetryCount))`
  hex-encoded — deterministic per `(job_id, retry_count)` pair, unlike the
  old run-scoped random-suffixed key.
- Trace context is sent as a native `MessageAttribute` (`Traceparent`), not
  stuffed into the JSON body.

`ReceiveMessages` requests the `SentTimestamp` system attribute and all
message attributes (`MessageAttributeNames: []string{"All"}`), used to
populate `Message.SentAt` for job-wait-latency metrics
([internal/worker/ec2.go](../../internal/worker/ec2.go)'s `publishJobWait`).

`internal/worker/ec2.go` wraps `SendMessage`/`DeleteMessage` with its own
retry helpers (`sendMessageWithRetry`, `deleteMessageWithRetry` — fixed
attempt count, `RetryDelay` between tries) on top of whatever the AWS SDK
already retries internally; `pkg/queue` itself does not retry.

## Key Decisions [coverage: high -- 6 sources]

- **2026-06: Valkey Streams (K8s queue backend) removed.** The K8s compute
  provider was removed upstream in its entirety; `pkg/queue/valkey.go` (the
  `ValkeyClient` implementation using Redis Streams / consumer groups) was
  deleted along with it. `pkg/queue/interface.go` now describes a single
  SQS implementation — the "provider-agnostic" framing in `Message` and
  `Queue` is a holdover from the two-backend era but no longer models a
  real second backend.
- **FIFO grouping by `JobID`, not `RunID`.** A run's jobs (matrix legs,
  otherwise-independent jobs) are enqueued together but have no per-run
  ordering requirement — stepped jobs (`needs:`) are gated by GitHub and
  never enqueued concurrently. Grouping by `RunID` would serialize all of a
  run's jobs through one FIFO lane and let one stuck/poison job
  head-of-line-block its siblings; grouping by `JobID` keeps them parallel
  and isolated. `RunID` still travels in the message body for other uses.
- **Deterministic dedup key `(JobID, RetryCount)`.** Replaces an earlier
  design that hashed in a timestamp and UUID slice to force distinct dedup
  IDs on every send. The current key is stable per retry attempt: sending
  the same `(JobID, RetryCount)` twice naturally collides with SQS's
  5-minute dedup window, while a genuine retry (which bumps `RetryCount`)
  produces a new key.
- **Hardcoded 120s visibility timeout.** `ReceiveMessages` sets
  `VisibilityTimeout: 120` to exceed the ~90s `MessageProcessTimeout`
  elsewhere in job processing.
- **Caller-controlled long polling.** `waitTimeSeconds` is a parameter, not
  a constant — callers pick (typically 20s for long polling, 0 for drain).
- **Trace propagation via a single `Traceparent` attribute.** Uses the
  standard W3C traceparent string rather than separate `TraceID`/`SpanID`/
  `ParentID` fields, avoiding re-marshaling and keeping trace context
  inspectable without parsing JSON.
- **`pkg/queue` scoped to job transport, not general SQS access.**
  Operator-facing queue management (DLQ redrive, depth checks) in
  `pkg/housekeeping` and `pkg/admin` goes through the raw AWS SDK `sqs`
  package instead of `queue.Queue`, since those operations (`StartMessageMoveTask`,
  `GetQueueAttributes`) aren't part of the job send/receive/ack lifecycle.

## Gotchas [coverage: high -- 6 sources]

- **Visibility vs. job duration.** The 120s SQS visibility timeout covers
  job *claim* time, not job *execution*. If a processor stalls past 120s
  the message becomes visible again and another consumer may pick it up.
  After repeated receive attempts SQS routes the message to the DLQ
  (configured at the queue, not in this package).
- **`Message.ID` may be empty.** The code guards `sqsMsg.MessageId` with a
  nil check — only `Handle` and `Body` are guaranteed populated.
- **FIFO throughput is per `JobID` group, not per run.** Since each job gets
  its own `MessageGroupId`, FIFO ordering only matters within retries of the
  same job; this maximizes parallelism but means there is no ordering
  guarantee across jobs in the same run (by design — see Key Decisions).
- **No `Close()` on `SQSClient`.** Unlike the removed Valkey client (which
  wrapped a `redis.Client` needing explicit shutdown), the SQS client holds
  no connection to release — nothing to change in shutdown paths after the
  Valkey removal, but any code that previously type-asserted for a
  `Closer`-like interface across both backends should be reviewed for dead
  branches.
- **Non-FIFO queue URLs skip grouping/dedup entirely.** `SendMessage` only
  sets `MessageGroupId`/`MessageDeduplicationId` when `queueURL` ends in
  `.fifo`; a misconfigured non-FIFO queue URL silently loses ordering and
  dedup guarantees rather than erroring.
- **`pkg/queue`'s `Message`/`Queue` naming still reads as backend-agnostic**
  post-Valkey-removal (e.g. doc comments referencing "provider-agnostic
  format"). Treat that framing as historical; there is currently exactly
  one implementation to satisfy.
- **The agent binary doesn't use `pkg/queue` at all.** `pkg/agent/telemetry.go`
  sends telemetry via a raw `sqs.Client`, so changes to `Queue`'s contract
  (message shape, attribute names) don't automatically apply to what the
  agent posts back — they have to be kept in sync by hand.
- **`RUNS_FLEET_POOL_QUEUE_URL` / `RUNS_FLEET_HOUSEKEEPING_QUEUE_URL` look
  wired up but aren't.** They pass config validation and appear in the
  admin dashboard, which can give the impression of an active pool/
  housekeeping queue consumer. There isn't one — don't assume a message
  sent to either URL will be picked up by anything.

## Sources [coverage: high]

- [pkg/queue/interface.go](../../pkg/queue/interface.go)
- [pkg/queue/sqs.go](../../pkg/queue/sqs.go)
- [pkg/config/config.go](../../pkg/config/config.go)
- [cmd/server/main.go](../../cmd/server/main.go)
- [internal/handler/webhook.go](../../internal/handler/webhook.go)
- [internal/worker/ec2.go](../../internal/worker/ec2.go)
