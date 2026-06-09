# Operator-triggered requeue of runner-less / hung jobs

## Problem

The orchestrator launches an EC2 instance for a queued GitHub Actions job, but the
runner agent on that instance sometimes fails to register (broken AMI, agent dies on
init, Vault/STS auth failure, etc.). The job then sits forever: GitHub does **not**
auto-cancel queued self-hosted jobs, and the instance is dead.

The in-process watchdog `housekeeping.Tasks.ExecuteUnconfirmedRunners`
(`pkg/housekeeping/unconfirmed.go`) already recovers the common case — jobs stuck in
`launched` past a fixed 5-minute threshold. This feature adds the **manual,
operator-triggered** version of that same recovery, for when the watchdog was broken or
disabled, or when an operator wants to force a sweep immediately (with a chosen
threshold) rather than wait for the next 2-minute tick. It targets the same scope as the
watchdog — `launched` jobs only (see "Selection criteria").

## Hard constraint: do not touch the GitHub job

The requeue re-dispatches a fresh **runner** by re-enqueuing into runs-fleet's own SQS
main queue, so a new healthy instance picks up the still-queued GitHub job. It MUST NOT
cancel, re-create, re-run, or dispatch the GitHub Actions job/run. These hangs are
runner-flawed; the GitHub job itself is valid and still queued, so we preserve it and
fix only the runner side. No GitHub API call is made on this path.

## Selection criteria ("requeue-able")

A job is requeue-able when **all** hold:

1. **Status** is `launched` (instance was provisioned but the runner never confirmed).
   `running` is **excluded**: it means the runner confirmed and is executing a real job,
   and this action terminates the instance — requeuing a `running` job would kill live
   work. A runner that died mid-job is recovered by the spot-interruption path and the
   orphan reaper, not here. `claiming` is excluded too — a transient pre-launch state
   with no instance to reason about.
2. **Staleness**: `created_at` older than a threshold (default 15 min, min 10 min,
   operator-overridable via `threshold_minutes`). Younger jobs are likely still
   legitimately booting/registering.
3. **Dead/missing instance**: the recorded `instance_id` either no longer exists
   (terminated / `InvalidInstanceID.NotFound`) **or** still exists but the runner never
   confirmed. A live instance is terminated before requeue so we never run two runners
   for one job; a missing instance needs no termination. A job with no `instance_id` is
   still requeue-able (provisioning never produced an instance).
4. **Retries remaining**: `retry_count < maxLaunchRecoveryRetries` (2, mirroring the
   worker's on-demand cap). Exhausted jobs are skipped by this operator action — they
   are left for the watchdog to mark terminal, so the operator endpoint never destroys a
   job, it only re-dispatches.
5. **run_id present**: a job with no `run_id` cannot be rebuilt into a launch message and
   is skipped.

## Requeue mechanism (reuses the existing path)

Reuses `pkg/housekeeping`:

- `FindRequeueableJobs` — single DynamoDB scan over the requested statuses past threshold
  (the operator action and the watchdog both pass `launched` only), projecting the fields
  needed to rebuild a launch message (shared by the watchdog).
- `BuildRequeueMessage` — builds the `queue.JobMessage` with `RetryCount+1`,
  `ForceOnDemand: true`, `Spot: false` (on-demand for reliability, same as the watchdog).
- `RequeueHungJobs` — the operator core: existence check (batched `DescribeInstances`
  with per-instance fallback, same helper as the orphan sweep), terminate alive-but-dead
  instances + cancel their spot requests, send the fresh message via the `JobRequeuer`
  (SQS), then flip the record to `requeued` with a conditional write guarded on the
  record still being `launched`.

The watchdog's launched-only recovery is refactored onto these shared helpers so there
is exactly one definition of "what a requeue-able job is" and "how a requeue message is
built".

## Safety

- **Idempotency / no double-runner**: the requeue message dedup id is
  `(job_id, retry_count)` (FIFO dedup in `pkg/queue`), and the status flip to `requeued`
  is a conditional write (`status = launched`), so a concurrent normal completion,
  watchdog requeue, a runner confirming into `running`, or a second operator click cannot
  double-dispatch or clobber a job that advanced. A `ConditionalCheckFailedException` is treated as success
  (the job already moved on). A live instance is terminated before the message is sent.
- **Don't touch healthy in-flight jobs**: the staleness threshold + the
  instance-existence/confirmation check exclude jobs whose runner is alive and working.
  An EC2 `DescribeInstances` error makes the instance "assumed alive" (safe default), so a
  transient AWS outage never causes a spurious requeue.
- **Bounded retries**: capped at `maxLaunchRecoveryRetries`; exhausted jobs are skipped
  (never requeued, never destroyed by this path).
- **Dry run**: `dry_run=true` reports the candidate set without terminating, sending, or
  mutating any record.

## Admin API surface

`POST /api/housekeeping/requeue-hung-jobs` (auth via the existing admin
`AuthMiddleware` / `RUNS_FLEET_ADMIN_SECRET`, behind the admin rate limiter, audit
logged like other admin actions).

Query params:
- `threshold_minutes` — min job age (default 15, min 10).
- `dry_run` — `true` to preview only.

Response:
```json
{ "requeued": 2, "candidates": 5, "skipped_exhausted": 1, "job_ids": [42, 43], "message": "..." }
```

## UI hook

The Jobs page (`pkg/admin/ui/app/jobs/page.tsx`) already has an "Orphaned Jobs Cleanup"
card with Dry-Run / action buttons hitting `POST /api/housekeeping/orphaned-jobs`. The
intended UI hook is a sibling "Requeue Hung Jobs" card that POSTs to
`/api/housekeeping/requeue-hung-jobs` with the same dry-run-then-confirm flow. The
backend endpoint is the load-bearing, fully tested part; the UI card is a thin wrapper
over `apiFetch` identical in shape to the existing cleanup card.
