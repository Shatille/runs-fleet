---
topic: Queue Processing (SQS / Valkey)
last_compiled: 2026-04-30
sources_count: 3
---

# Queue Processing (SQS / Valkey)

## Purpose [coverage: medium -- 3 sources]

`pkg/queue` decouples webhook ingestion from instance provisioning by abstracting
two backends behind a single `Queue` interface: AWS SQS FIFO (EC2 mode) and
Valkey/Redis Streams (K8s mode). Producers (webhook handler, pool manager,
events router) call `SendMessage`; consumers (orchestrator processors) call
`ReceiveMessages` / `DeleteMessage` to claim and acknowledge work. The package
preserves FIFO semantics per message group and carries OpenTelemetry trace
context across the queue boundary so spans stitch back together on the
consumer side.

## Architecture [coverage: medium -- 3 sources]

The interface is intentionally minimal — three methods plus an optional
`Pinger` for health checks. Provider-specific concerns (deduplication IDs,
consumer groups, stream offsets) are hidden inside the implementations.

- `interface.go` defines `Queue`, `Message`, `JobMessage`, and the optional
  `Pinger` interface.
- `sqs.go` implements `SQSClient` against `github.com/aws/aws-sdk-go-v2/service/sqs`.
  One client wraps a single queue URL — runs-fleet instantiates it five times
  (main, pool, events, termination, housekeeping).
- `valkey.go` implements `ValkeyClient` against `github.com/redis/go-redis/v9`,
  using Redis Streams with consumer groups (`XADD` / `XREADGROUP` / `XACK`)
  rather than plain lists, so multiple consumers can share work and track
  pending messages.

Both implementations have a compile-time interface check
(`var _ Queue = (*SQSClient)(nil)` and `var _ Queue = (*ValkeyClient)(nil)`).

## Talks To [coverage: medium -- 3 sources]

- **AWS SQS** via `aws-sdk-go-v2/service/sqs` (`SendMessage`, `ReceiveMessage`,
  `DeleteMessage`). The `SQSAPI` interface narrows the SDK surface to those
  three operations for testability.
- **Valkey/Redis** via `go-redis/v9` (`XAdd`, `XReadGroup`, `XAck`,
  `XGroupCreateMkStream`, `Ping`).
- **`pkg/config`** (implicit) supplies the `aws.Config` passed to
  `NewSQSClient` and the `ValkeyConfig` (addr, password, DB, stream, group,
  consumer ID) for `NewValkeyClient`.
- **OpenTelemetry** consumers via `ExtractTraceContext`, which pulls
  `TraceID` / `SpanID` / `ParentID` from `Message.Attributes`.

## API Surface [coverage: medium -- 3 sources]

The `Queue` interface (`interface.go`):

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

`Message` is the provider-agnostic envelope:

```go
type Message struct {
    ID         string            // SQS MessageId or Valkey stream ID
    Body       string            // JSON-encoded JobMessage
    Handle     string            // SQS ReceiptHandle or Valkey stream ID
    Attributes map[string]string // trace context, etc.
}
```

`ExtractTraceContext(msg)` returns `(traceID, spanID, parentID)` from
`msg.Attributes`.

Constructors:

- `NewSQSClient(cfg aws.Config, queueURL string) *SQSClient`
- `NewClient(cfg aws.Config, queueURL string) *SQSClient` — backward-compat alias
- `NewSQSClientWithAPI(api SQSAPI, queueURL string) *SQSClient` — test seam
- `NewValkeyClient(cfg ValkeyConfig) (*ValkeyClient, error)` — also calls
  `ensureConsumerGroup` (idempotent: `BUSYGROUP ... already exists` is swallowed)
- `NewValkeyClientWithRedis(client *redis.Client, stream, group, consumerID string) *ValkeyClient`
- `(*ValkeyClient).Close() error`, `(*ValkeyClient).Ping(ctx) error`

## Data [coverage: medium -- 3 sources]

`JobMessage` is the canonical job envelope, JSON-marshaled into `Message.Body`
on send and unmarshaled by the consumer. Notable fields:

- Identity: `JobID`, `RunID`, `Repo`, `OriginalLabel`
- Compute spec: `InstanceType`, `InstanceTypes[]`, `Pool`, `Spot`,
  `ForceOnDemand`, `OS`, `Arch`, `StorageGiB`, `PublicIP`
- Flexible matching: `CPUMin`, `CPUMax`, `RAMMin`, `RAMMax`, `Families[]`, `Gen`
- Routing: `Region`, `Environment`
- Retry: `RetryCount`
- Tracing: `TraceID`, `SpanID`, `ParentID`

Both backends require `JobID != 0` and `RunID != 0` and return an error
otherwise.

**SQS message group / dedup:**

- `MessageGroupId = fmt.Sprintf("%d", job.RunID)` — FIFO ordering is per run.
- `MessageDeduplicationId = sha256(fmt.Sprintf("%d-%d-%s", JobID, time.Now().UnixNano(), uuid[:8]))`
  hex-encoded — combines JobID, nanosecond timestamp, and a UUID slice so
  deliberate retries are not silently de-duped.
- Trace context is sent as native `MessageAttributes` (`TraceID`, `SpanID`,
  `ParentID`), not stuffed into the body.

**Valkey stream entry** (`XADD` values):

```
body     = JSON(JobMessage)
run_id   = JobMessage.RunID
job_id   = JobMessage.JobID
trace_id = JobMessage.TraceID
span_id  = JobMessage.SpanID
```

The stream name and consumer group come from `ValkeyConfig`. The `Handle`
returned to consumers is the Redis stream message ID.

## Key Decisions [coverage: medium -- 3 sources]

- **Per-run FIFO grouping (SQS).** `MessageGroupId = RunID` lets different
  workflow runs proceed in parallel while preserving order within a single run.
- **Hardcoded 120s visibility timeout (SQS).** `ReceiveMessages` sets
  `VisibilityTimeout: 120`, with the comment "to exceed
  MessageProcessTimeout (90s)" — the project-level "5 min visibility timeout"
  is set on other queues, but the in-code value here is 120s.
- **Caller-controlled long polling.** `waitTimeSeconds` is a parameter, not a
  constant — callers pick (typically 20s for long polling, 0 for drain).
- **Trace propagation via attributes, not body.** Avoids re-marshaling and
  keeps trace context inspectable without parsing JSON.
- **Streams over Lists for Valkey.** Uses `XREADGROUP` consumer groups so
  multiple orchestrator instances can share a stream with at-least-once
  delivery and explicit `XACK`, mirroring SQS semantics more faithfully than
  `LPUSH`/`BRPOP`.
- **Idempotent group creation.** `ensureConsumerGroup` calls
  `XGroupCreateMkStream` and treats `BUSYGROUP ... already exists` as success.
- **Auto-generated consumer ID.** If `ValkeyConfig.ConsumerID` is empty,
  `uuid.New().String()` is used so each process gets a distinct consumer name
  inside the group.

## Gotchas [coverage: medium -- 3 sources]

- **Visibility vs. job duration.** The 120s SQS visibility timeout covers job
  *claim* time, not job *execution*. If a processor stalls past 120s the
  message becomes visible again and another consumer may pick it up. After
  three receive attempts SQS routes the message to the DLQ (configured at the
  queue, not in this package).
- **Dedup ID includes a random suffix.** Re-sending the *same* `JobMessage`
  intentionally produces a different `MessageDeduplicationId` because of
  `time.Now().UnixNano()` + UUID slice, so SQS's native 5-minute deduplication
  window does not protect against accidental duplicate sends from the producer
  side — callers must dedupe upstream if they need that.
- **`MessageGroupId` is `RunID`, not `JobID`.** All jobs in one run serialize
  through one FIFO group. SQS FIFO throughput is 300 msg/s per group (3000
  with high-throughput mode), so a single very-bursty run can become a
  bottleneck.
- **Valkey persistence is operator-controlled.** `pkg/queue` does not
  configure AOF/RDB; if the Valkey instance is non-persistent, in-flight
  messages can be lost on failover. The orchestrator relies on whatever the
  operator wired up via `RUNS_FLEET_VALKEY_ADDR`.
- **`Message.ID` may be empty for SQS.** The code guards `sqsMsg.MessageId`
  with a nil check (`if sqsMsg.MessageId != nil`) — only `Handle` and `Body`
  are guaranteed populated.
- **Attribute string-only.** Valkey `XADD` values that are not `string` (or
  whose type assertion fails) silently drop on the consumer; both `body` and
  trace fields use `string` assertions and skip empty trace IDs.
- **`Close()` exists on Valkey but not SQS.** The SQS client holds no
  long-lived connection; the Valkey client wraps a `redis.Client` that should
  be closed on shutdown.

## Sources [coverage: high]

- [pkg/queue/interface.go](../../pkg/queue/interface.go)
- [pkg/queue/sqs.go](../../pkg/queue/sqs.go)
- [pkg/queue/valkey.go](../../pkg/queue/valkey.go)
