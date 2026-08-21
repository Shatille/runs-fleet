---
topic: Housekeeping (Cleanup Tasks)
last_compiled: 2026-08-21
sources_count: 12
---

# Housekeeping (Cleanup Tasks)

## Purpose [coverage: high -- 12 sources]

`pkg/housekeeping` is the second half of the consistency model: the orchestrator
makes forward progress with retries and conditional writes, and housekeeping
reconciles whatever the happy path dropped. It reaps EC2 instances nothing owns,
retires job records nothing will ever finish, re-dispatches jobs whose runner
never took the work, deletes GitHub runner registrations GitHub itself never
collects, retires stopped pool members frozen on an outdated AMI, samples what
the whole fleet costs, and drains the main-queue DLQ.

The package has two halves. A `Runner`
([pkg/housekeeping/runner.go](../../pkg/housekeeping/runner.go)) owns scheduling:
one in-process timer loop per task, deduplicated across Fargate replicas by a
DynamoDB task lock. A `Tasks` executor
([pkg/housekeeping/tasks.go](../../pkg/housekeeping/tasks.go) plus the
per-sweep files) does the work, and exports enough of its internals
(`SweepOrphanedInstances`, `FindOrphanedJobCandidates`, `RequeueHungJobs`,
`RequeueJob`, `ReconcileJob`, `CancelSpotRequestForInstance`) for `pkg/admin` to
drive the same sweeps from the console on demand.

**The SQS scheduler/handler pair is gone.** `scheduler.go` and `handler.go` no
longer exist; there is no `housekeeping.Message`, no `TaskExecutor` dispatch
switch, and nothing publishes to or consumes from the housekeeping SQS queue.
`RUNS_FLEET_HOUSEKEEPING_QUEUE_URL` survives only so the admin queues view can
still show that queue's depth
([cmd/server/main.go:604](../../cmd/server/main.go)). The stated reason for the
change is in `Runner`'s own doc comment: the work is clock-derived and
idempotent, so a queue bought nothing a timer plus a lock does not.

## Architecture [coverage: high -- 12 sources]

```
Runner.Run                      (cmd/server initHousekeeping, main.go:411)
  └─ one goroutine per taskSpec, each: time.NewTicker(interval)
       │
       ├─ tryRunTask
       │    ├─ AcquireTaskLock(taskType, instanceID, 6m)   → ErrTaskLockHeld ⇒ skip tick
       │    ├─ context.WithTimeout(WithoutCancel(ctx), 4m) ── survives SIGTERM
       │    ├─ spec.execute(taskCtx)
       │    ├─ PublishMessageProcessingSeconds("housekeeping", ok|error, s)
       │    └─ ReleaseTaskLock (deferred, on a fresh context)
       │
       └─ tasks (DefaultSchedulerConfig intervals):
            orphaned_instances        5m   initial   tasks.go  SweepOrphanedInstances
            dlq_redrive               1m   initial   tasks.go  ExecuteDLQRedrive
            unconfirmed_runners       2m             unconfirmed.go
            stale_jobs                5m             tasks.go  (+ auto-requeue)
            stale_secrets            15m             tasks.go  (+ dead-assignment reap)
            orphaned_jobs            15m             tasks.go / orphans.go
            orphaned_packer_instances 15m            packer.go
            expired_instance_claims  15m             tasks.go
            old_jobs                  1h             tasks.go  (HARD DELETE)
            ephemeral_pool_cleanup    1h             tasks.go
            pool_hot_tuner            1h             tasks.go / hot_tuner.go
            orphaned_runners          1h             orphaned_runners.go
            stale_ami_instances       1h             stale_ami.go
            cost_report              24h             tasks.go → pkg/cost.Reporter
            fleet_cost_sample        60s             fleet_cost.go
```

Key structural facts:

- `taskSpecs()` ([runner.go:237](../../pkg/housekeeping/runner.go)) is the single
  schedule table. A task whose interval is `<= 0` is **dropped with a warning
  rather than scheduled** — a zero duration reaching `time.NewTicker` panics and
  would take every other task's loop down with the process.
- `orphaned_instances` and `dlq_redrive` carry `initial: true` and run once at
  startup, so a fresh deploy does not wait a full interval.
- Task bodies run on `context.WithoutCancel(ctx)`, so an in-flight sweep drains
  through a shutdown instead of being cut mid-terminate; `taskTimeout = 4m`
  bounds it either way.
- `SetTaskLocker`, `SetMetrics`, `SetPoolDB`, `SetGitHubJobChecker`,
  `SetJobRequeuer`, `SetAMIReference`, `SetRunnerRegistry`, `SetFleetCostStore`
  are all optional wiring. Every sweep whose dependency is missing returns `nil`
  immediately — deliberate, because each of them terminates instances or deletes
  registrations and none may act on an unknown.

**Removed in this window: the pool-audit task.** `TaskPoolAudit` and
`ExecutePoolAudit` no longer exist anywhere in the Go source. It was a 10-minute
`Scan` of the pools table that published `PoolInstances`/`PoolDesired` under the
same metric names *and* the same dimension tuples that pool reconciliation
already writes from `pkg/pools/manager.go` — reading back out of DynamoDB the
values reconcile had just written via `UpdatePoolState`. It wrote no state and
cleaned nothing up, so the only effect was a second, staler writer into one time
series: on any minute reconcile missed, the 10-minute-old sample became the
datapoint. Two writers at different cadences into one series is a correctness
defect, and it paid for a non-paginated Scan every 10 minutes to be wrong.

## Talks To [coverage: high -- 12 sources]

- **DynamoDB (jobs table)** — `Scan` with `FilterExpression` for every job sweep;
  `UpdateItem` with `ConditionExpression` on `status` for every transition;
  `BatchWriteItem` (25 per batch) for the old-jobs delete; `GetItem` for the
  pre-terminate re-read.
- **DynamoDB (pools table)** — via `PoolDBAPI`: `ListPools`, `GetPoolConfig`,
  `DeletePoolConfig`, `ListJobsForAdmin`, `UpdatePoolAutoTune`,
  `DeleteExpiredInstanceClaims`, `HasLiveInstanceClaim`,
  `HasActiveJobForInstance`, `LastJobCompletionForInstance`. Also the task lock
  itself (`TaskLocker.AcquireTaskLock`/`ReleaseTaskLock` → `db.ErrTaskLockHeld`),
  the fleet-cost day rows, and the runner-offline sightings.
- **EC2** — `DescribeInstances` (tag, IAM-profile, state and pool filters, all
  paginated), `TerminateInstances`, `DescribeSpotInstanceRequests`,
  `CancelSpotInstanceRequests`. Spot cancellation always precedes termination.
- **SQS** — `GetQueueAttributes` (DLQ depth + both ARNs),
  `ListMessageMoveTasks`, `StartMessageMoveTask` for the redrive; and
  `JobRequeuer.SendMessage` onto the **main** job queue for every requeue path.
- **GitHub** — `GitHubJobChecker.GetWorkflowJobStatus` (stale-job reconciliation
  and the pre-requeue queued re-confirmation) and `RunnerRegistry.ListRunners` /
  `DeleteRunner` (orphaned-runner deregistration), both adapted from
  `*gh.Client` in [cmd/server/main.go](../../cmd/server/main.go).
- **SSM / Vault** — `secrets.Store.List` / `Delete` for runner configs.
- **`pkg/cost`** — `CostReporter.GenerateDailyReport` for the 24h report, and
  `cost.NewFleetPricer` for the per-tick fleet sample.
- **`pkg/admin`** — inbound: the console calls `SweepOrphanedInstances`,
  `FindOrphanedJobCandidates`, `RequeueHungJobs`, `RequeueJob`, `ReconcileJob`,
  `MarkJobOrphaned`, `BatchCheckInstanceExistence` and
  `CancelSpotRequestForInstance` directly. See
  [admin-ui](admin-ui.md).

## API Surface [coverage: high -- 12 sources]

`TaskType` constants ([runner.go:18](../../pkg/housekeeping/runner.go)):
`orphaned_instances`, `stale_secrets`, `old_jobs`, `cost_report`, `dlq_redrive`,
`ephemeral_pool_cleanup`, `orphaned_jobs`, `stale_jobs`, `unconfirmed_runners`,
`orphaned_packer_instances`, `pool_hot_tuner`, `stale_ami_instances`,
`expired_instance_claims`, `orphaned_runners`, `fleet_cost_sample`. (No
`pool_audit`.)

Scheduling:

- `SchedulerConfig` — one `time.Duration` field per task;
  `DefaultSchedulerConfig()` supplies the intervals in the diagram above.
- `Runner` — `NewRunner(executor, schedulerCfg)`, `SetTaskLocker(locker,
  instanceID)`, `SetMetrics(RunnerMetricsAPI)`, `Run(ctx)` (blocks until ctx is
  cancelled and every loop has drained). Constants: `taskLockTTL = 6m`,
  `taskTimeout = 4m`.
- `TaskExecutor` — the 15-method interface `*Tasks` satisfies.

Execution (`*Tasks`, `NewTasks(awsCfg, appConfig, secretsStore, metrics,
costReporter)`), one `Execute*` per task plus:

- `SweepOrphanedInstances(ctx, dryRun) (OrphanInstanceSweep, error)` — the
  five-phase reaper, with a dry run that detects but cancels/terminates/publishes
  nothing. `ExecuteOrphanedInstances` is the non-dry wrapper.
- `ExecuteOrphanedSpotRequests` — cancels spot requests whose instances are gone.
  Not scheduled; callable ad hoc.
- Setters: `SetPoolDB`, `SetGitHubJobChecker`, `SetJobRequeuer`,
  `SetAMIReference`, `SetRunnerRegistry(registry, ActiveReposFunc, sightings)`,
  `SetFleetCostStore`.

Shared helpers other packages consume (mostly `orphans.go` / `requeue.go`):

- `FindOrphanedJobCandidates(ctx, scan, table, threshold, ...ScanOption)
  ([]OrphanedJobCandidate, truncated bool, error)` and
  `FindRequeueableJobs(..., statuses []db.JobStatus, ...ScanOption)`.
- `ScanOption` / `WithMaxItems(n)` — caps a sweep at *n matched candidates* and
  reports `truncated`. Applied to matches, not `ScanInput.Limit`, which bounds
  items *read* and so cannot express "stop after n matches" behind a filter.
- `SeparateOrphanedJobs`, `BatchCheckInstanceExistence` (100 IDs per describe,
  with a per-ID fallback), `MarkJobOrphaned(…, observedStatus)`,
  `ReconcileJob` → `ReconcileOutcome`, `RequeueHungJobs(deps, opts)` →
  `RequeueResult`, `RequeueJob(deps, jobID, opts)` → `SingleRequeueResult` /
  `RequeueOutcome`, `GetRequeueableJob`, `BuildRequeueMessage`,
  `CancelSpotRequestForInstance`.
- `MaxRequeueRetries = 2` — the shared cap for the watchdog, the stale-queued
  auto-requeue and the operator action.

Interfaces consumed: `TaskLocker`, `TaskExecutor`, `RunnerMetricsAPI`, `EC2API`,
`OrphanEC2API`, `DynamoDBAPI`, `OrphanScanAPI`, `SQSAPI`, `MetricsAPI`,
`CostReporter`, `GitHubJobChecker`, `JobQueuedChecker`, `JobRequeuer`,
`PoolDBAPI`, `AMIReference`, `RunnerRegistry`, `RunnerSightingStore`,
`ActiveReposFunc`, `FleetCostStore`.

## Data [coverage: high -- 12 sources]

Thresholds and caps, by sweep:

| Sweep | Selection | Bound |
|---|---|---|
| `orphaned_instances` | 5 phases (below) | terminates everything merged |
| `stale_secrets` | config with no instance, or config older than `deadAssignmentAge()` | — |
| `old_jobs` | `completed_at < now-7d` | 25 deletes per `BatchWriteItem` |
| `orphaned_jobs` | running/claiming/launched `created_at < now-2h`, **or** requeued `requeued_at < now-2h` | `WithMaxItems` when driven by the console |
| `stale_jobs` | running/claiming `created_at < now-10m` | 30 GitHub checks + 5 requeues per cycle |
| `unconfirmed_runners` | launched, `created_at < now-5m`, has `instance_id` | `MaxRequeueRetries = 2` |
| `ephemeral_pool_cleanup` | `Ephemeral && now-LastJobTime > 4h` (`EphemeralPoolTTL`) | terminates pool instances before deleting config |
| `orphaned_packer_instances` | `tag:created-by=runs-fleet-packer`, age > 1h, pending/running/stopping/stopped | — |
| `stale_ami_instances` | stopped, pool-tagged, `ImageId != reference[arch]` | **1 per pool per cycle**, oldest launch first |
| `orphaned_runners` | name prefix `runs-fleet-runner-`, status `offline`, not busy, offline for `minOfflineAge()` | 200 deletes per cycle |
| `expired_instance_claims` | `__instance_claim:` rows with `claim_expiry < now` | conditional `DeleteItem` per row |
| `fleet_cost_sample` | managed instances in pending/running/stopping/stopped | elapsed clamped at 15m |

Derived thresholds (both computed from config rather than hardcoded, because the
config validator permits a runtime ceiling up to 24h and a hardcoded constant
below it would reap live work):

- `deadAssignmentAge() = max(MaxRuntimeMinutes, agentStandbyAllowance 2h) +
  deadAssignmentSlack 2h`. Used by the stale-secrets dead-assignment reap.
- `minOfflineAge()` = the same value, reused for orphaned-runner deregistration
  (#437). A registration exists from the moment its JIT config is minted —
  before the instance even boots — so a live runner reads `offline` for its whole
  startup, and a standby agent can sit offline for the standby deadline.

The five orphaned-instance phases ([tasks.go:212](../../pkg/housekeeping/tasks.go)):

1. **Tag** — `runs-fleet:managed=true`, running/pending, launched before
   `now - (MaxRuntimeMinutes + 10m)`.
2. **IAM profile** — any instance on the runner instance profile past the same
   cutoff, *including stopped*, to catch untagged zombies from spot tag
   propagation failures that pool reconciliation cannot see.
3. **Unclaimed** — cold-start instances (no pool tag) past
   `UnclaimedInstanceGraceMinutes` holding no active job.
4. **Completed** — instances whose every job row is terminal and whose latest
   completion predates `CompletedInstanceGraceMinutes`, with a `launchedAt` guard
   so a restarted pool member's stale row does not reap it.
5. **Abandoned stopped** — stopped, *untagged-by-pool* instances past
   `StoppedInstanceGraceHours`. Opt-in (disabled at zero) because terminating a
   stopped instance destroys its EBS volume.

Rows this package writes into the **pools** table alongside pool configs:

- `__instance_claim:<id>` — reaped by `expired_instance_claims` on `claim_expiry`.
- `__runner_offline:<repo>:<id>` — first-offline sighting for the orphaned-runner
  sweep, durable because consecutive sweeps land on different replicas so a
  per-process counter would never accumulate. Reaped by
  `DeleteStaleRunnerSightings` (7d) rather than DynamoDB TTL: the table's single
  TTL attribute is already spent on `claim_expiry`, so the `ttl` attribute on
  these rows is inert.

Fleet cost sample (`db.FleetCostDelta`, one row per local day): `TotalCost`,
`ComputeCost`, `EBSCost`, `InstanceSeconds`, `AttributedSeconds` (seconds where
the instance was both billable and in the busy set), `SampledAt`, `Partial`. Days
are bucketed in `config.ReportLocation()`, not UTC, and an empty fleet still
writes a checkpoint so the next tick does not over-attribute the gap.

## Key Decisions [coverage: high -- 12 sources]

- **In-process timers + a distributed task lock, not SQS.** Scheduling is
  clock-derived and every task is idempotent, so the queue was pure overhead. The
  lock (65s-style pattern, here `taskLockTTL = 6m` > `taskTimeout = 4m`) is what
  keeps N replicas from doing the same work at once. Note the honest limit: the
  lock is **released on completion**, so it prevents *concurrency* but not two
  runs inside one interval — two replicas whose tickers are offset can each get
  the lock in the same minute. That is safe precisely because every task is
  idempotent, and it is why the design tolerates dropping the queue.
- **Every dependency-less sweep no-ops instead of guessing.** `stale_ami` with no
  `AMIReference`, `orphaned_runners` with no registry/repos/sightings,
  `unconfirmed_runners` with no requeuer, `pool_hot_tuner` / ephemeral cleanup
  with no `poolDB`, `fleet_cost_sample` with no store. Each one terminates,
  deletes or destroys, so an unknown must read as "do nothing", never as
  "everything qualifies". `ExecuteCostReport` uses an `isNilInterface` reflection
  guard for the same reason — a typed-nil `*cost.Reporter` in the interface would
  otherwise be non-nil.
- **Confirm immediately before every irreversible act.** The scan snapshot is
  minutes old, so `stale_ami` re-reads EC2 state *and* checks
  `HasLiveInstanceClaim` + `HasActiveJobForInstance` per candidate (#433);
  `unconfirmed_runners` re-reads the job's status before terminating; the requeue
  path re-confirms with GitHub that the job is still `queued`. `stale_ami`
  terminates candidate-by-candidate rather than batching at the end, so batching
  cannot stretch the gap between "confirmed stopped" and "destroyed" across every
  other candidate's round trips.
- **Every uncertainty resolves toward "leave it alive."** A DynamoDB or EC2
  lookup failure counts as claimed/alive/busy; `instancePromisedToJob` returns
  `true` when `poolDB` is nil; `instanceIDGone` trusts the API error *code*, not
  the message, so a throttling error quoting `InvalidInstanceID` cannot read as a
  vanished instance. The cost of a false positive is one instance left on an old
  AMI for an hour; the cost of a false negative is a killed build.
- **Conditional writes for every status transition.** `MarkJobOrphaned`,
  `markJobCompleted`, `markLaunchedJobRequeued`, `markLaunchedJobError` all gate
  on the status the scan actually observed, and treat
  `ConditionalCheckFailedException` as "another actor owns this record" — a
  no-op, not an error. `markLaunchedJobRequeued` returns whether *this* call did
  the write, because a caller that assumed success on a lost race would send a
  message another actor's rollback then strands.
- **Claimable before enqueue, with rollback.** `requeueUnconfirmed` flips the
  record to `requeued` *before* sending the SQS message, because `ClaimJob`
  rejects a record still reading `launched` and a long-polling worker beats the
  follow-up write. If the send then fails, the flip is rolled back — this sweep
  only scans `launched`, so it would otherwise never see the job again.
- **`requeued` had to be added to the orphan sweep (#423).** Nothing else could
  see it: stale-jobs scans running/claiming, the requeue sweep scans launched,
  and the old-jobs GC keys off `completed_at`, which a requeued record never
  gets. It is aged on `requeued_at`, not `created_at`, so a re-dispatch seconds
  old is not retired for its first attempt's age.
- **GitHub-queued means hung, and hung is now auto-recovered (#430).** A record
  reading `running` while GitHub still has the job `queued` is a runner that was
  handed someone else's work (or none). The stale-jobs sweep re-dispatches those
  through the *same* `requeueCandidate` path the operator button uses — re-read,
  re-confirm queued, terminate, guarded flip, send — so the sweep can never be
  less careful than a human. Bounded at 5 per cycle so a mass hang drains
  gradually instead of thundering.
- **Ephemeral runners leak registrations, so the fleet must clean them up
  (#436).** GitHub removes an ephemeral runner only once it *completes* a job, so
  every instance the watchdog kills before it takes work leaves its registration
  behind forever — observed at 360 of 369 in one repo. Only `offline`,
  non-`busy`, name-prefixed registrations are removed, capped at 200 per cycle.
- **Stopped pool members never re-image, so they need retiring (#431–#434).** EC2
  does not re-image on `StartInstances`, so a stopped instance holds its
  creation-time AMI for as long as it exists. Running instances are deliberately
  left alone — they pick up the new image when they cycle after their next job,
  and one that never cycles is a hung instance, a different problem. The drip is
  one per pool per cycle so a pool never dips more than one below target, and it
  runs unconditionally (#434) rather than behind a flag.
- **Bounded batches the console drains (#443).** `WithMaxItems` exists so an
  operator-triggered sweep does not grow with the table and outlive the browser's
  fetch timeout; `truncated` is the signal to send another batch. The scheduled
  sweeps pass no option and keep draining the whole table.
- **The fleet sampler is statistical by construction.** Crediting the elapsed
  interval per observed instance is unbiased in aggregate at any interval — the
  interval controls variance, not bias. 60s buys ~3% daily aggregate error for
  1440 `DescribeInstances` calls a day, and halving it would buy under a point.
  Which is exactly why the output feeds a fleet total and is never surfaced as a
  per-job number.

## Gotchas [coverage: high -- 12 sources]

- **`ExecuteOldJobs` HARD-DELETES job rows older than 7 days, with no archive.**
  The doc comment says "archives or deletes"; the code only deletes
  ([tasks.go:955](../../pkg/housekeeping/tasks.go)). Anything computed *from the
  jobs table* is therefore silently truncated at 7 days — including any
  "month-to-date" figure. This is exactly why the admin cost page's fleet block
  derives its attributed share from the sampler's own busy-vs-total
  instance-seconds rather than from dividing the job-priced total by the fleet
  total: that ratio would decay across a month for calendar reasons.
- **Full-table `Scan` is still the access pattern.** `ExecuteOldJobs`,
  `ExecuteStaleJobs`, `FindOrphanedJobCandidates` and `FindRequeueableJobs` all
  scan the jobs table behind a `FilterExpression`; the code itself notes a GSI on
  `(status, created_at)` would be justified past ~100k items. `ListPools` scans
  the **pools** table, which also accumulates `__instance_claim:` and
  `__runner_offline:` rows — that accumulation is what `expired_instance_claims`
  and `DeleteStaleRunnerSightings` exist to bound, and what previously bloated
  the table past a Scan page and hid real pools.
- **The task lock does not make a task run exactly once per interval.** It is
  released on completion, not held for the interval, so two replicas with offset
  tickers can both run a task within one interval. Fine for idempotent sweeps;
  not something to rely on for anything that counts.
- **A misconfigured interval silently disables a task.** `taskSpecs()` drops any
  spec with `interval <= 0`, logging `"task disabled: interval not configured"`
  at WARN. That is deliberate (a zero duration panics `time.NewTicker` and would
  kill the process), but a task can vanish from a deployment with only a startup
  warning to show it.
- **A nil `TaskLocker` means no cross-replica coordination at all.** Safe only
  for a single replica. A nil `RunnerMetricsAPI` silently drops per-task latency.
- **Stopped instances appear in the profile-based orphan pass but not the
  tag-based one.** Tagged stopped instances are legitimate warm-pool members;
  untagged stopped ones are invisible to pool reconciliation and leak EBS.
  Mis-tuning these filters either leaks storage or terminates warm capacity.
- **Spot request cancellation must precede termination**, or a persistent request
  resurrects the instance. `cancelSpotRequestsForInstances` batches at 200 IDs
  per describe and 100 per cancel with a single 2s retry. `stale_ami`
  deliberately skips it — warm pools are on-demand only, so a pool member has no
  persistent request.
- **`ExecuteEphemeralPoolCleanup` returns `nil` even when deletes failed.** Pool
  instance-termination and `DeletePoolConfig` errors are logged and the loop
  continues; the function's only error path is `ListPools`. The task therefore
  reports success on a partly failed cleanup.
- **`ExecuteOldJobs` swallows `BatchWriteItem` failures too** — a failed batch
  `continue`s and is simply not counted, so a persistently failing delete is
  invisible except in the count.
- **The stale-jobs GitHub budget is a floor, not a ceiling.** `maxStaleJobChecks
  = 30` bounds the *status* calls, but each of the up-to-5 requeues spends one
  more to re-confirm queued, so a cycle's real ceiling is 35.
- **`ExecuteOrphanedRunners` returns `nil` on almost every failure.** A repo
  whose listing fails is skipped, a failed sighting write leaves the
  registration, and a failed sighting reap returns `nil` early. Only
  `activeRepos` failing surfaces as a task error.
- **Fleet EBS cost is an assumed 100 GiB per instance.** `DescribeInstances`
  reports volume IDs but not sizes (`EbsInstanceBlockDevice` carries no
  `VolumeSize`), so real sizes would need a `DescribeVolumes` fan-out every tick.
  The API flags the component as estimated; the number is not billing-accurate.
- **A multi-hour outage understates fleet cost rather than spiking it.** Ticks
  credit elapsed-since-checkpoint, clamped at `fleetCostMaxElapsed = 15m`, and a
  clamped tick marks its day `Partial`.
- **`pool_hot_tuner` is gated on the master hot-pools toggle** and returns
  immediately when off, so a fleet that never enables hot pools pays no
  steady-state DynamoDB load — but it also means a stale `auto_tune` recommendation
  can sit in the pool record from before the toggle was flipped off.
- **`launchedConfirmThreshold` (5m) is a package `var`, not a const** — it exists
  as a test seam, and it is the one threshold in the package that is *not*
  derived from config, so a fleet whose cold starts genuinely take longer than
  5 minutes would see the watchdog killing healthy boots.

## Sources [coverage: high]

- [pkg/housekeeping/runner.go](../../pkg/housekeeping/runner.go)
- [pkg/housekeeping/tasks.go](../../pkg/housekeeping/tasks.go)
- [pkg/housekeeping/orphans.go](../../pkg/housekeeping/orphans.go)
- [pkg/housekeeping/orphaned_runners.go](../../pkg/housekeeping/orphaned_runners.go)
- [pkg/housekeeping/requeue.go](../../pkg/housekeeping/requeue.go)
- [pkg/housekeeping/stale_ami.go](../../pkg/housekeeping/stale_ami.go)
- [pkg/housekeeping/unconfirmed.go](../../pkg/housekeeping/unconfirmed.go)
- [pkg/housekeeping/hot_tuner.go](../../pkg/housekeeping/hot_tuner.go)
- [pkg/housekeeping/fleet_cost.go](../../pkg/housekeeping/fleet_cost.go)
- [pkg/housekeeping/packer.go](../../pkg/housekeeping/packer.go)
- [cmd/server/main.go](../../cmd/server/main.go)
- [pkg/db/runner_sightings.go](../../pkg/db/runner_sightings.go)
- [pkg/db/instance_claims.go](../../pkg/db/instance_claims.go)
- [pkg/db/pool_config.go](../../pkg/db/pool_config.go)
