---
topic: Housekeeping (Cleanup Tasks)
last_compiled: 2026-04-30
sources_count: 3
---

# Housekeeping (Cleanup Tasks)

## Purpose [coverage: high -- 3 sources]

The `pkg/housekeeping` package runs scheduled cleanup tasks that prevent
resource leakage and inflate-the-bill failure modes in long-running fleet
operation. It is split into three components: a `Scheduler` that fires task
messages on cron-like intervals, a `Handler` that consumes those messages
from SQS, and a `Tasks` executor that performs the actual cleanup work
(orphaned EC2 instances, stale runner secrets, old job records, DLQ
redrives, ephemeral pool TTL, stale GitHub jobs, and more).

The split between scheduler and handler is deliberate: scheduling is
trivially idempotent (worst case: duplicate task messages), but task
execution must be coordinated across multiple Fargate instances. By
dispatching tasks via the housekeeping queue plus a DynamoDB task lock,
multiple replicas can run safely without leader election.

## Architecture [coverage: high -- 3 sources]

```
Scheduler (in-process tickers)
   ├─ orphanedTicker     (5m)  → SendMessage(TaskOrphanedInstances)
   ├─ ssmTicker          (15m) → SendMessage(TaskStaleSecrets)
   ├─ jobsTicker         (1h)  → SendMessage(TaskOldJobs)
   ├─ orphanedJobsTicker (15m) → SendMessage(TaskOrphanedJobs)
   ├─ poolTicker         (10m) → SendMessage(TaskPoolAudit)
   ├─ costTicker         (24h) → SendMessage(TaskCostReport)
   ├─ dlqTicker          (1m)  → SendMessage(TaskDLQRedrive)
   ├─ ephemeralTicker    (1h)  → SendMessage(TaskEphemeralPoolCleanup)
   └─ staleJobsTicker    (5m)  → SendMessage(TaskStaleJobs)
                                       │
                                       ▼
                          Housekeeping SQS queue
                                       │
                                       ▼
                          Handler.Run loop
                          ├─ ReceiveMessages (long-poll 20s)
                          ├─ Acquire task lock (6m TTL)
                          ├─ Dispatch by TaskType
                          └─ DeleteMessage on success
                                       │
                                       ▼
                          Tasks.Execute<TaskName>
                          ├─ EC2 Describe/Terminate/CancelSpot
                          ├─ DynamoDB Scan/BatchWrite/UpdateItem
                          ├─ SSM/Vault via secrets.Store
                          ├─ SQS StartMessageMoveTask (DLQ redrive)
                          └─ GitHub API (stale job reconciliation)
```

The scheduler kicks off `TaskOrphanedInstances` and `TaskDLQRedrive`
immediately on `Run` so a freshly deployed orchestrator does not need to
wait a full interval before the first cleanup runs.

## Talks To [coverage: high -- 3 sources]

- **SQS** — `SchedulerSQSAPI.SendMessage` to enqueue task messages on the
  housekeeping queue (`config.Config.QueueURL` for the main queue is also
  consulted by `ExecuteDLQRedrive`); `QueueAPI.ReceiveMessages` /
  `DeleteMessage` for consumption; `SQSAPI.GetQueueAttributes` and
  `StartMessageMoveTask` for DLQ redrive.
- **EC2** — `EC2API.DescribeInstances` (tag-based and IAM-profile-based
  filters), `TerminateInstances`, `DescribeSpotInstanceRequests`,
  `CancelSpotInstanceRequests`. Tag filter
  `tag:runs-fleet:managed=true` plus a fallback filter on
  `iam-instance-profile.arn` catches both tagged and untagged zombies.
- **DynamoDB** — `DynamoDBAPI.Scan` against the jobs table for old/orphaned
  /stale job detection; `BatchWriteItem` for delete batches of 25;
  `UpdateItem` with `ConditionExpression` on status for atomic transitions
  (`running|claiming → orphaned`, `running|claiming → completed`); a Scan
  on the pools table for utilization audits.
- **SSM / Vault** — `secrets.Store.List` and `Delete` via the
  `secretsStore` field, used by `ExecuteStaleSecrets`.
- **GitHub API** — `GitHubJobChecker.GetWorkflowJobStatus` resolves stale
  jobs by querying authoritative job state.
- **DynamoDB pool table (separate locker)** — `TaskLocker.AcquireTaskLock` /
  `ReleaseTaskLock`, returning `db.ErrTaskLockHeld` when another replica
  owns the task.

## API Surface [coverage: high -- 3 sources]

Types and functions exported from `pkg/housekeeping`:

- `TaskType` constants: `TaskOrphanedInstances`, `TaskStaleSecrets`,
  `TaskOldJobs`, `TaskPoolAudit`, `TaskCostReport`, `TaskDLQRedrive`,
  `TaskEphemeralPoolCleanup`, `TaskOrphanedJobs`, `TaskStaleJobs`.
- `Message{TaskType, Timestamp, Params}` — the JSON payload on the
  housekeeping queue.
- Interfaces consumed: `QueueAPI`, `TaskLocker`, `TaskExecutor`,
  `SchedulerSQSAPI`, `SchedulerMetrics`, `EC2API`, `DynamoDBAPI`,
  `MetricsAPI`, `CostReporter`, `GitHubJobChecker`, `SQSAPI`, `PoolDBAPI`.
- `Handler` — `NewHandler(q, executor, cfg)`, `SetTaskLocker(locker, instanceID)`,
  `Run(ctx)`, internal `processMessage`. Per-task timeout: 4 minutes.
  Lock TTL: 6 minutes (`taskLockTTL`).
- `Scheduler` — `SchedulerConfig` (one `time.Duration` per task),
  `DefaultSchedulerConfig()`, `NewScheduler`, `NewSchedulerFromConfig`,
  `SetMetrics`, `Run(ctx)`. Retry policy: `maxScheduleRetries = 3` with
  `schedulerBaseRetryDelay = 1s` exponential backoff.
- `Tasks` — `NewTasks(cfg, appConfig, secretsStore, metrics, costReporter)`,
  `SetPoolDB`, `SetGitHubJobChecker`, plus the
  `Execute<TaskName>` methods that satisfy `TaskExecutor`. Also
  `ExecuteOrphanedSpotRequests` (not currently scheduled, callable for
  ad-hoc cleanup).

## Data [coverage: high -- 3 sources]

**Task message format** (`Message`):

```json
{
  "task_type": "orphaned_instances",
  "timestamp": "2026-04-30T12:00:00Z",
  "params": null
}
```

**Targeted resources and key thresholds:**

- Orphaned EC2 instances — running/pending instances older than
  `MaxRuntimeMinutes + 10`. Two detection passes: tag-based
  (`runs-fleet:managed=true`) and IAM-profile-based (catches untagged
  zombies; also includes `stopped` state because untagged stopped
  instances are invisible to pool reconciliation). Spot requests are
  cancelled before termination to prevent zombie resurrection.
- Stale runner secrets — `secrets.Store.List()` ∖ existing instances,
  with a 10-minute `instanceTerminationGracePeriod` to avoid deleting
  parameters for very recently terminated instances.
- Old jobs — `completed_at < now - 7 days`, batch-deleted in groups of 25.
- Orphaned jobs — status in `{running, claiming}` and `created_at` older
  than `orphanedJobThreshold = 2h`. Marked `orphaned` via conditional
  `UpdateItem`. Jobs in `claiming` without `instance_id` are auto-orphaned.
- Stale jobs — same DynamoDB filter but `staleJobThreshold = 10m`,
  capped at `maxStaleJobChecks = 30` GitHub API calls per cycle (~360/h
  vs the 5000/h limit). Resolves to actual GitHub conclusion.
- Ephemeral pools — `EphemeralPoolTTL = 4h` since `LastJobTime`. Pool
  instances are terminated before the pool config is deleted.
- DLQ redrive — uses `StartMessageMoveTask` with source = DLQ ARN,
  destination = main queue ARN. No-op when DLQ is empty.

## Key Decisions [coverage: high -- 3 sources]

- **Async via SQS, not in-process cron.** Multi-instance Fargate
  deployments can all run the scheduler without duplicating *task
  execution* — only one consumer wins the SQS message and only one
  acquires the DynamoDB task lock.
- **Task lock TTL > task timeout.** `taskLockTTL = 6m` covers the 4-minute
  per-task `context.WithTimeout` plus buffer; expired locks self-heal
  after a crash.
- **Dual orphan detection (tag + IAM profile).** Persistent spot tag
  propagation failures leave untagged zombies that the tag-based pass
  cannot see. The profile-based pass catches them and explicitly logs
  the discrepancy.
- **Conditional writes for status transitions.** `markJobOrphaned` and
  `markJobCompleted` both gate on `status = running OR status = claiming`,
  so a normal completion racing with the cleanup never gets clobbered;
  `ConditionalCheckFailedException` is treated as a no-op success.
- **Cost-bounded GitHub reconciliation.** `maxStaleJobChecks = 30` per
  cycle keeps the worst case at 360 calls/hour even under heavy stale-job
  load.
- **TTL/age cutoffs over hard counts.** Old jobs (7d), orphaned-job
  threshold (2h, exceeds `MaxRuntimeMinutes + 10` plus spot interruption
  re-queue slack), ephemeral pools (4h), termination grace period (10m).

## Gotchas [coverage: medium -- 3 sources]

- **Race between job completion and orphan detection.** The 2h orphan
  threshold is intentionally larger than `MaxRuntimeMinutes + 10` to
  prevent prematurely orphaning legitimate long jobs; the conditional
  write is the second line of defense.
- **DynamoDB `Scan` cost.** `ExecuteOrphanedJobs`, `ExecuteOldJobs`,
  `ExecuteStaleJobs`, and `ExecutePoolAudit` all use full-table `Scan`.
  The package itself notes that >100k items would justify a GSI on
  `(status, created_at)`.
- **Stopped instances in profile-based pass only.** Tag-based pass
  excludes `stopped` (those are legitimate warm-pool members); profile-
  based pass includes `stopped` to catch untagged zombies that pool
  reconciliation cannot see. Mis-tuning these filters can either leak
  EBS or terminate warm pool members.
- **Spot request cancellation needs to happen before termination.**
  Otherwise persistent spot requests resurrect the instance.
  `cancelSpotRequestsForInstances` runs first, batched at 200 IDs per
  describe and 100 per cancel, with a single 2s retry on failure.
- **`SchedulerMetrics` and `TaskLocker` are optional.** A nil locker
  means no cross-replica coordination — safe only for single-instance
  deployments. A nil metrics publisher silently drops scheduling-failure
  signals.
- **`ExecuteCostReport` and `ExecuteEphemeralPoolCleanup` no-op on nil
  dependencies.** `costReporter == nil` (checked via `isNilInterface`
  reflection guard against typed-nil pointers) and `poolDB == nil`
  return cleanly without error, which means a misconfigured deployment
  fails silently rather than loudly.
- **Task type dispatch is open-coded.** `processMessage`'s switch logs
  `"unknown task type"` and still deletes the message — adding a new
  `TaskType` requires updating both the scheduler tickers and the
  handler switch.

## Sources [coverage: high]

- [pkg/housekeeping/handler.go](../../pkg/housekeeping/handler.go)
- [pkg/housekeeping/scheduler.go](../../pkg/housekeeping/scheduler.go)
- [pkg/housekeeping/tasks.go](../../pkg/housekeeping/tasks.go)
