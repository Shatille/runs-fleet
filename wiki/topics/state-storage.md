---
topic: State Storage (DynamoDB + Circuit + Secrets)
last_compiled: 2026-07-09
sources_count: 12
---

# State Storage (DynamoDB + Circuit + Secrets)

## Purpose [coverage: high -- 12 sources]

runs-fleet persists four kinds of state across orchestrator restarts and concurrent
Fargate tasks:

1. **Job and pool state** in DynamoDB (`runs-fleet-jobs`, `runs-fleet-pools`) — job
   lifecycle, pool configuration, and three flavors of distributed lock
   (reconcile locks, housekeeping task locks, instance claims) for
   multi-instance Fargate.
2. **Circuit breaker state** in DynamoDB (`runs-fleet-circuit-state`) — per
   instance type spot interruption tracking, used to flip new launches to
   on-demand when a type is unstable.
3. **Per-runner secrets** (registration tokens, cache tokens, full
   `RunnerConfig` JSON) in either AWS SSM Parameter Store (default) or HashiCorp
   Vault, fronted by a single `Store` interface so callers don't care which
   backend is wired up.
4. **Admin audit log** in DynamoDB (`runs-fleet-audit`, added 2026-07, PR #383) —
   append-only record of admin API actions with a 90-day TTL, read back by the
   admin UI's audit viewer.

State is intentionally split: hot operational state (jobs, claims, circuit
counts) lives in DynamoDB where conditional writes give us atomicity; sensitive
ephemeral material (registration tokens, cache HMAC) lives behind a secrets
backend with encryption at rest; the audit trail is a separate append-only
table so compliance data never contends with the job hot path.

## Architecture [coverage: high -- 12 sources]

**DynamoDB (`pkg/db`):** A single `Client` (`pkg/db/dynamo.go`) wraps the AWS
SDK v2 DynamoDB client and holds three table names — `poolsTable`, `jobsTable`,
and `auditTable` — plus two optional GSI names for the jobs table
(`jobsPoolStatusGSI`, `jobsInstanceIDGSI`). The client is constructed via
`NewClient(cfg, poolsTable, jobsTable)` (or `NewClientWithAPI` for tests); the
audit table and GSIs are wired post-construction with setters
(`SetAuditTable`, `SetJobsPoolStatusGSI`, `SetJobsInstanceIDGSI`). All DynamoDB
calls go through a small `DynamoDBAPI` interface (`GetItem`, `UpdateItem`,
`Scan`, `Query`, `PutItem`, `DeleteItem`) so tests can swap in a mock.

File split within `pkg/db`:

- `jobs.go` — job lifecycle transitions and queries (statuses typed in
  `job_status.go`).
- `pool_config.go` — pool configuration CRUD, ephemeral pool creation,
  reconcile-result stamping, and the `IsReservedPoolKey` guard.
- `locks.go` — per-pool reconcile locks and housekeeping task locks
  (`__task_lock:` rows in the pools table).
- `instance_claims.go` — instance claim locking (`__instance_claim:` rows in
  the pools table).
- `audit.go` — audit log writes and filtered reads.

**Circuit breaker (`pkg/circuit`):** `Breaker` (`pkg/circuit/breaker.go`) owns
its own DynamoDB client targeting `runs-fleet-circuit-state`. State is keyed by
`instance_type`. The breaker also keeps an in-process cache (`map[string]*CachedState`)
with a 1-minute TTL to absorb the read traffic from each fleet launch decision,
plus a periodic cleanup goroutine started via `StartCacheCleanup`.

**Secrets (`pkg/secrets`):** A `Store` interface (`pkg/secrets/store.go`) with
four methods — `Put`, `Get`, `Delete`, `List` — operates over a canonical
`RunnerConfig` struct. Two implementations:

- `SSMStore` (`pkg/secrets/ssm.go`) — writes JSON-serialised configs into SSM
  parameters as `SecureString`, tags every parameter with
  `runs-fleet:managed=true` and `runs-fleet:job-id=<id>`.
- `VaultStore` (`pkg/secrets/vault.go`) — uses HashiCorp Vault KV (auto-detects
  v1 vs v2), supports AWS IAM / Kubernetes / AppRole / token auth, and runs a
  background goroutine to renew its own Vault token.

Both stores satisfy `Store` via compile-time assertions (`var _ Store =
(*SSMStore)(nil)`, same for Vault).

## Talks To [coverage: high -- 12 sources]

- **Amazon DynamoDB** — `runs-fleet-jobs`, `runs-fleet-pools`,
  `runs-fleet-circuit-state`, `runs-fleet-audit` tables (reference Terraform in
  [deploy/terraform/dynamodb.tf](../../deploy/terraform/dynamodb.tf)).
  Conditional writes for atomicity; DynamoDB TTL attributes on circuit rows and
  audit rows.
- **`pkg/admin`** — the audit store's only producer/consumer: admin handlers
  call `RecordAudit` after mutating actions and `ListAuditLogs` behind
  `GET /api/audit-logs`. Enabled by `RUNS_FLEET_AUDIT_TABLE`
  (`cmd/server/main.go` calls `SetAuditTable`); gated at call sites via
  `HasAuditTable()`, not a nil check.
- **AWS SSM Parameter Store** — `SecureString` parameters under a configurable
  prefix (default `DefaultSSMPrefix`), one per runner. `GetParametersByPath`
  used for `List`.
- **HashiCorp Vault** — KV v1 or v2 mount (default `DefaultVaultKVMount`,
  `DefaultVaultPath`). Talks via `github.com/hashicorp/vault/api` over HTTPS
  with a 30s HTTP timeout; uses `Auth().Token().LookupSelfWithContext` and
  `RenewSelfWithContext` for token lifecycle.

## API Surface [coverage: high -- 12 sources]

### `pkg/db` construction and feature gates

- `NewClient(cfg aws.Config, poolsTable, jobsTable string) *Client` /
  `NewClientWithAPI(api DynamoDBAPI, ...)` for tests.
- `SetJobsPoolStatusGSI(name)` / `SetJobsInstanceIDGSI(name)` — flip the
  corresponding queries from Scan fallback to GSI Query.
- `SetAuditTable(name)` / `HasAuditTable() bool` — audit methods return errors
  when the table is unconfigured; `HasAuditTable` is the presence check.
- `HasJobsTable() bool` — guard for callers when the jobs table is optional.

### Job lifecycle (`pkg/db/jobs.go`, statuses in `pkg/db/job_status.go`)

Statuses are the typed `JobStatus` constants: `launched`, `running`,
`claiming`, `terminating`, `requeued`, `completed`, `success`, `failed`,
`error`, `orphaned`. Transition mechanics live in the
[job-state-machine](job-state-machine.md) article; the storage-facing surface:

- `SaveJob(ctx, *JobRecord) error` — PutItem with status `launched`, stamping
  `created_at` with the assignment time.
- `ClaimJob(ctx, jobID, runID int64, repo string) error` — self-expiring
  claim lease (compare-and-swap on observed `created_at`); returns
  `ErrJobAlreadyClaimed` or `ErrJobClaimExhausted` (after `claimMaxAttempts`
  = 3 stale re-claims).
- `FailExhaustedClaim(ctx, jobID) error` — `claiming → error`, conditional;
  treats a lost race as success.
- `MarkJobStarted(ctx, jobID, startedAt) (*events.JobInfo, error)` —
  `launched → running`, conditional; returns the post-update record via
  `ReturnValueAllNew`, or `(nil, nil)` for a late message.
- `MarkJobComplete(ctx, jobID, status, exitCode, duration) (*events.JobInfo, error)`
  — terminal write, also `ReturnValueAllNew`.
- `UpdateJobMetrics(ctx, jobID, startedAt, completedAt) error`
- `MarkInstanceTerminating(ctx, instanceID) error`
- `MarkJobRequeuedByJobID(ctx, jobID) (bool, error)` — conditional update from
  `running`/`launched` → `requeued`. Returns false (not error) on conditional
  check failure.
- `DeleteJobClaim(ctx, jobID) error`
- `GetJobByInstance(ctx, instanceID) (*events.JobInfo, error)` — Query on the
  instance-id GSI when configured, Scan fallback otherwise (see Gotchas).
- `GetJobByJobID(ctx, jobID) (*events.JobInfo, error)` — O(1) primary-key
  GetItem.
- `QueryPoolJobHistory(ctx, poolName, since) ([]JobHistoryEntry, error)` —
  Query on the required `pool-created-at-index` GSI, paginated-Scan fallback.
- `GetPoolP90Concurrency(ctx, poolName, windowHours) (int, error)` — samples
  concurrency at 1-minute intervals, returns `samples[floor(0.9 * (N-1))]`;
  active records older than `maxConcurrencyRuntime` (2h) are excluded as
  abandoned.
- `GetPoolBusyInstanceIDs(ctx, poolName) ([]string, error)` — pool-status GSI
  when configured (one Query per status, `launched` and `running`, unioned),
  Scan fallback.
- Admin API helpers: `ListJobsForAdmin`, `GetJobForAdmin`,
  `GetJobStatsForAdmin` returning `AdminJobEntry`, `AdminJobFilter`,
  `AdminJobStats`.

Removed since the 2026-04 compile: `GetPoolPeakConcurrency` and the deprecated
instance-keyed `MarkJobRequeued`.

Errors: `ErrJobAlreadyClaimed`, `ErrJobClaimExhausted`,
`ErrInstanceAlreadyClaimed`.

### Pool configuration (`pkg/db/pool_config.go`)

- `GetPoolConfig(ctx, poolName) (*PoolConfig, error)` — reserved keys (task
  locks, instance claims) resolve to `(nil, nil)` rather than phantom pools.
- `ListPools(ctx) ([]string, error)` — Scan with `IsReservedPoolKey` filtering.
- `UpdatePoolState(ctx, poolName, running, stopped) error` — conditional on
  pool existence.
- `SavePoolConfig(ctx, *PoolConfig) error` — targeted UpdateItem with an
  explicit SET list, so pool CRUD never clobbers lock attributes or
  reconcile-result attributes on the same row.
- `CreateEphemeralPool(ctx, *PoolConfig) error` — conditional put
  (`attribute_not_exists(pool_name)`); returns `ErrPoolAlreadyExists` so
  concurrent jobs racing to create the same ephemeral pool are safe.
- `TouchPoolActivity(ctx, poolName) error` — bumps `last_job_time`.
- `UpdatePoolReconcileResult(ctx, poolName, result, at) error` — added 2026-07
  (PR #384): stamps `last_reconcile_at`/`last_reconcile_result` for admin
  observability; returns `ErrPoolNotFound` if the pool was deleted
  mid-reconcile so callers can ignore the benign race.
- `DeletePoolConfig(ctx, poolName) error` — conditioned on `ephemeral = true`
  so persistent pools cannot be deleted by this path.
- `IsReservedPoolKey(poolName) bool` — the shared guard for the sentinel
  prefixes.

### Locks and claims (`pkg/db/locks.go`, `pkg/db/instance_claims.go`)

- `AcquirePoolReconcileLock(ctx, poolName, owner, ttl)` /
  `ReleasePoolReconcileLock(ctx, poolName, owner)` — lock attributes on the
  pool's own row; `ErrPoolReconcileLockHeld`, `ErrPoolNotFound`.
- `AcquireTaskLock(ctx, taskType, owner, ttl)` / `ReleaseTaskLock(...)` —
  housekeeping singletons via `__task_lock:<taskType>` rows;
  `ErrTaskLockHeld`.
- `ClaimInstanceForJob(ctx, instanceID, jobID, ttl) error` — conditional
  UpdateItem on the **pools** table using a synthetic key
  `pool_name = "__instance_claim:<instanceID>"`. Acquires if no claim exists,
  if the existing claim has expired (`claim_expiry < now`), or if this job
  already owns it. Returns `ErrInstanceAlreadyClaimed` on conflict.
- `ReleaseInstanceClaim(ctx, instanceID, jobID) error` — DeleteItem with
  `attribute_exists(pool_name) AND job_id = :job_id`. Silently no-ops if the
  claim is held by another job (treats conditional check failure as success).

See the [per-resource-locking](../concepts/per-resource-locking.md) concept for
how these three lock flavors compose.

### Audit log (`pkg/db/audit.go`)

- `RecordAudit(ctx, AuditEntry) error` — PutItem; the `id` (ULID) and
  `timestamp` are generated internally so callers never fabricate them; an
  empty `User` is stored as `"anonymous"`.
- `ListAuditLogs(ctx, AuditFilter) ([]AuditEntry, error)` — Query on the
  `user-index` GSI when `filter.User` is set (with Scan fallback on
  ValidationException), full paginated Scan otherwise. `Offset`/`Limit` are
  applied in memory.
- Types: `AuditEntry { ID, User, Action, Target, Result, Details map[string]any,
  ClientIP, Timestamp }`, `AuditFilter { User, Action, Since, Until, Limit,
  Offset }`.

### `pkg/circuit`

- `NewBreaker(cfg aws.Config, tableName string) *Breaker`
- `(*Breaker).RecordInterruption(ctx, instanceType) error` — increments the
  counter, opens the circuit when it crosses `InterruptionThreshold` within
  `TimeWindow`.
- `(*Breaker).CheckCircuit(ctx, instanceType) (State, error)` — cache-first
  read. Auto-resets state in DynamoDB if `auto_reset_at` is in the past.
- `(*Breaker).ResetCircuit(ctx, instanceType) error` — manual reset.
- `(*Breaker).StartCacheCleanup(ctx) <-chan struct{}` — kicks off a
  5-minute-interval background cleanup of expired in-process cache entries.

Types: `State` (`StateClosed`, `StateOpen`, `StateHalfOpen`), `Record`,
`CachedState`. Constants: `InterruptionThreshold = 3`, `TimeWindow = 15 *
time.Minute`, `CooldownPeriod = 30 * time.Minute`.

### `pkg/secrets`

The `Store` interface:

```go
type Store interface {
    Put(ctx, runnerID, *RunnerConfig) error
    Get(ctx, runnerID) (*RunnerConfig, error)
    Delete(ctx, runnerID) error
    List(ctx) ([]string, error)
}
```

`RunnerConfig` carries `Org`, `Repo`, `RunID`, `JITToken`, `Labels`,
`RunnerGroup`, `RunnerName`, `JobID`, `CacheToken`, `CacheURL`,
`TerminationQueueURL`, `IsOrg`.

- `NewSSMStore(awsCfg, prefix) *SSMStore` /
  `NewSSMStoreWithClient(client SSMAPI, prefix)` for tests. Empty prefix falls
  back to `DefaultSSMPrefix`.
- `NewVaultStore(ctx, VaultConfig) (*VaultStore, error)` —
  authenticates, auto-detects KV version, starts token renewal.
  `NewVaultStoreWithClient(client, kvMount, basePath, kvVersion)` is the test
  hook.
- `(*VaultStore).Close()` cancels the renewal goroutine and waits for it.

## Data [coverage: high -- 12 sources]

### `runs-fleet-jobs` table

Partition key: `job_id` (Number). Three GSIs
(`deploy/terraform/dynamodb.tf`):

- `pool-created-at-index` (`pool`, `created_at`) — **required**; backs
  `QueryPoolJobHistory` for autoscaling. Missing it means a Scan fallback and
  a WARN every reconcile loop.
- `instance-id-index` (`instance_id`) — optional; wired via
  `RUNS_FLEET_JOBS_INSTANCE_ID_GSI`. Backs `GetJobByInstance`.
- `pool-status-index` (`pool`, `status`) — optional; wired via
  `RUNS_FLEET_JOBS_POOL_STATUS_GSI`. Backs `GetPoolBusyInstanceIDs`.

Attributes (from `jobRecord` and admin parsing):

- `instance_id` (S, omitempty), `job_id` (N), `run_id` (N), `repo` (S),
  `instance_type` (S), `pool` (S, omitempty), `spot` (BOOL), `retry_count` (N),
  `warm_pool_hit` (BOOL), `status` (S, omitempty — empty status is dropped
  rather than written, guarding the status-keyed GSI), `created_at` (S,
  RFC3339), `spot_request_id` (S, omitempty), `persistent_spot` (BOOL,
  omitempty), `trace_id` (S, omitempty — extracted from the W3C traceparent).
- Mutated by lifecycle methods: `started_at`, `completed_at`, `requeued_at`,
  `exit_code`, `duration_seconds`.

**`created_at` semantics:** during the `claiming` phase it is the lease
timestamp written by `ClaimJob` (staleness is judged against it, and it is the
compare-and-swap pin for re-claims). `SaveJob`'s PutItem then replaces the
whole record at assignment, re-stamping `created_at` — so on any `launched`
-or-later record, `created_at` means *assignment time*, and that is what
consumers read.

**Surfaced read model:** `unmarshalJobInfo` maps records into
`events.JobInfo`; since PR #387 that includes `WarmPoolHit` and `CreatedAt`
(RFC3339-parsed; a malformed `created_at` parses to the zero time rather than
failing the unmarshal, and consumers self-guard on zero). No schema change was
involved — both attributes were already written (`warm_pool_hit` at record
creation, `created_at` as above); they simply weren't surfaced to readers
before.

Status values: `launched`, `claiming`, `running`, `requeued`, `terminating`,
`completed`, `success`, `failed`, `error`, `orphaned` (typed constants in
`pkg/db/job_status.go`).

### `runs-fleet-pools` table

Partition key: `pool_name` (S). One physical table, three logical concerns:

1. **Pool config rows** (`PoolConfig` in `pool_config.go`): `instance_type`,
   `desired_running`, `desired_stopped`, `current_running`, `current_stopped`,
   `idle_timeout_minutes`, `schedules` (list of `PoolSchedule`), `ephemeral`,
   `last_job_time`, flexible spec fields (`arch`, `cpu_min`/`cpu_max`,
   `ram_min`/`ram_max`, `families`, `multi_spec`), reconcile observability
   (`last_reconcile_at`, `last_reconcile_result` — written only by
   `UpdatePoolReconcileResult`, deliberately outside `SavePoolConfig`'s SET
   list), and reconcile-lock attributes (`reconcile_lock_owner`,
   `reconcile_lock_expires`) written only by the lock methods.
2. **Task lock rows** — `pool_name = "__task_lock:<taskType>"`
   (`taskLockPrefix`), one per housekeeping singleton.
3. **Instance claim rows** — `pool_name = "__instance_claim:<instanceID>"`
   (`instanceClaimPrefix`). Attributes: `job_id` (N),
   `claimed_at` (N, unix), `claim_expiry` (N, unix). Conditional acquire is:
   `attribute_not_exists(pool_name) OR attribute_not_exists(claim_expiry) OR
   claim_expiry < :now OR job_id = :job_id`.

Every path that enumerates pools must exclude the sentinel-prefixed rows via
`IsReservedPoolKey` — otherwise they get reconciled as phantom pools and
inflate per-pool metric cardinality.

### `runs-fleet-circuit-state` table

Partition key: `instance_type` (S). Schema from `circuit.Record`:

- `instance_type` (S)
- `state` (S) — `closed | open | half-open`
- `interruption_count` (N)
- `first_interruption_at`, `last_interruption_at`, `opened_at`,
  `auto_reset_at` (S, RFC3339)
- `ttl` (N, unix) — set to `auto_reset_at + 1h` for DynamoDB TTL cleanup of
  resolved entries.

### `runs-fleet-audit` table (added 2026-07, PR #383)

Partition key: `id` (S, ULID — time-sortable). GSI `user-index` on
(`user`, `timestamp`), **required by schema** (no setter to configure the GSI
name, unlike the jobs table's optional GSIs). Schema from `auditRecord`:

- `id` (S, ULID), `user` (S — `"anonymous"` when unauthenticated),
  `action` (S), `target` (S, omitempty), `result` (S),
  `details` (M, omitempty — free-form `map[string]any`),
  `client_ip` (S, omitempty), `timestamp` (S, RFC3339)
- `ttl` (N, unix) — write time + `auditRetention` (90 days); the first table
  in this deployment to use DynamoDB TTL for whole-row retention, so entries
  age out with no cleanup job.

### SSM parameter naming (`SSMStore.parameterPath`)

```
{prefix}/{runner-id}/config
```

Type is `SecureString`. Tags: `runs-fleet:managed=true`,
`runs-fleet:job-id=<id>`. `List` uses `GetParametersByPath` recursive=true and
extracts `runner-id` from the parsed path (`extractRunnerID`).

### Vault paths (`VaultStore.secretPath` / KV v2)

- KV v1: `<kvMount>/<basePath>/<runnerID>` (logical write/read).
- KV v2: `client.KVv2(kvMount).Put(ctx, basePath+"/"+runnerID, data)`. List
  uses `<kvMount>/metadata/<basePath>` for v2 vs `<kvMount>/<basePath>` for v1.
- Stored fields mirror `RunnerConfig` but written as a flat `map[string]
  interface{}` (lower-snake-case keys).

## Key Decisions [coverage: high -- 12 sources]

- **DynamoDB conditional writes for distributed locks.** `ClaimJob` is a
  compare-and-swap lease; `ClaimInstanceForJob` uses a four-clause condition
  that bakes in TTL-based stealing; reconcile and task locks use the same
  conditional-write shape. There is no separate lock table — claims, task
  locks, and pool config share `runs-fleet-pools`, disambiguated by sentinel
  key prefixes (`__instance_claim:`, `__task_lock:`). This avoids extra tables
  and lets a crashed orchestrator's locks expire on their own.
- **DB record as cross-instance rendezvous.** The job record is the join point
  between processes on different machines with no direct channel: the
  orchestrator stamps `created_at` at assignment (`SaveJob`), the on-instance
  agent reports its `StartedAt` via the termination queue, and the termination
  handler joins the two — `MarkJobStarted`'s `ReturnValueAllNew` response
  hands back `CreatedAt`/`WarmPoolHit` so `InstanceProvisionSeconds` is
  computed without a second read (PR #387). Zero or negative spans are
  dropped, so a malformed timestamp or cross-host clock skew can't emit bogus
  latency. See [job-state-machine](job-state-machine.md) and
  [events-and-termination](events-and-termination.md).
- **GSI-or-Scan fallback everywhere.** Every GSI-backed query
  (`GetJobByInstance`, `QueryPoolJobHistory`, `GetPoolBusyInstanceIDs`,
  `ListAuditLogs`) catches `ValidationException` and falls back to a paginated
  Scan with a WARN. This decouples deploy ordering: code referencing a new GSI
  can ship before the matching Terraform lands, degrading to correct-but-slow
  instead of erroring.
- **`MarkJobRequeuedByJobID` returns `(false, nil)` on conditional failure**,
  not an error. Callers can treat "already requeued / terminating" as a
  benign no-op rather than an alarm.
- **Audit is config-gated, not nil-gated.** `SetAuditTable` +
  `HasAuditTable()` is the feature flag; `pkg/admin` always receives a
  concrete `*db.Client` and checks `HasAuditTable`, so a nil-interface bug
  can't silently disable auditing. Unset table means audit methods error and
  admin actions are logged via slog only.
- **Audit IDs and timestamps are server-generated.** `RecordAudit` mints the
  ULID and RFC3339 timestamp internally so callers can never fabricate or
  reorder history; ULIDs make the primary key time-sortable.
- **Audit retention via DynamoDB TTL, pagination in memory.** 90-day TTL
  (`auditRetention`) bounds table growth with no cleanup job, which is also
  what makes `ListAuditLogs`' in-memory `Offset`/`Limit` acceptable — DynamoDB
  has no native offset and the table stays small by construction.
- **Reconcile-result stamps bypass `SavePoolConfig`.**
  `UpdatePoolReconcileResult` (like `TouchPoolActivity`) is a targeted
  UpdateItem, and `SavePoolConfig`'s SET list deliberately omits those
  attributes — pool CRUD, lock traffic, and reconcile observability never
  clobber each other despite sharing a row.
- **Circuit breaker keyed per instance type, not per pool.** Spot capacity is a
  property of the EC2 instance type, so a c7g.large interruption shouldn't
  block on-demand or m7g launches in the same pool. The breaker stores one row
  per type in `runs-fleet-circuit-state`.
- **Auto-reset baked into reads.** `CheckCircuit` rewrites the row to
  `closed` when `auto_reset_at` is in the past, then writes back through
  `putRecord`. No background reconciler is needed; the next
  `CheckCircuit` at launch time does it.
- **Two-tier caching for the breaker.** A 1-minute in-process cache absorbs
  the per-launch read traffic; a periodic cleaner (`StartCacheCleanup`,
  5-minute tick) prunes entries older than `2 * CacheTTL` to bound memory.
- **TTL attribute for transient state.** Circuit rows set `ttl =
  auto_reset_at + 1h` and audit rows set `ttl = write + 90d` so DynamoDB GC
  reclaims them automatically. Instance claims rely on application-level
  expiry rather than DynamoDB TTL because they need to be stealable
  mid-flight.
- **Secrets backend abstraction.** `Store` is the only thing called from the
  rest of the codebase. SSM is the default (zero ops, IAM-native); Vault is
  opt-in for orgs that already run it. Both implementations are asserted to
  satisfy `Store` at compile time.
- **Vault KV version auto-detection.** `NewVaultStore` probes the mount's
  `config` endpoint, falls back to `LIST` probes against v1 and v2 path
  styles when permission denied, and defaults to v1 only when truly
  ambiguous. Operators don't have to set `KVVersion` explicitly.
- **Vault token self-renewal.** `startTokenRenewal` runs a 5-minute ticker
  that calls `LookupSelf` and renews when remaining TTL drops below 3600s.
  Failures are logged-and-skip rather than hard failures so a transient
  Vault blip doesn't crash the orchestrator.

## Gotchas [coverage: high -- 12 sources]

- **Scan fallbacks are silent except for a WARN.** `GetJobByInstance` and
  `GetPoolBusyInstanceIDs` only use their GSIs when the corresponding env vars
  (`RUNS_FLEET_JOBS_INSTANCE_ID_GSI`, `RUNS_FLEET_JOBS_POOL_STATUS_GSI`) are
  set — unset means every call is a paginated Scan, correct but slow and
  RCU-expensive at scale. `QueryPoolJobHistory`'s GSI is required and
  hard-named (`pool-created-at-index`).
- **`QueryPoolJobHistory`'s GSI path reads a single page.** The Scan fallback
  paginates fully, but the Query path stops at DynamoDB's ~1 MB page —
  sufficient at ~100 jobs/day, but a pool sustaining tens of thousands of jobs
  per hour would compute p90 concurrency from a truncated dataset (reads
  under the true value).
- **`created_at` changes meaning across the claim boundary.** On a `claiming`
  record it's the lease timestamp; `SaveJob` re-stamps it at assignment. Code
  that reads `created_at` off an arbitrary record must know which phase it's
  looking at — `events.JobInfo.CreatedAt` is documented as assignment time and
  is only meaningful on `launched`-or-later records.
- **Stale code comment on `jobRecord`.** The struct comment in `jobs.go` still
  says "Primary key is instance_id"; the table's actual hash key is `job_id`
  (see Terraform), and every lifecycle method keys on `job_id`. `instance_id`
  is just an attribute (plus the optional GSI hash key).
- **Reads are eventually consistent by default.** None of the DynamoDB calls
  in these files set `ConsistentRead: true`. Conditional writes on the same
  partition key are still atomic, but read-then-write patterns can race
  across orchestrators — which is exactly why mutations use conditional
  expressions instead of read-modify-write.
- **Instance-claim release is best-effort.** `ReleaseInstanceClaim` swallows
  `ConditionalCheckFailedException` (claim already gone or owned by someone
  else) as a debug log. Callers shouldn't rely on the claim being present
  after the call.
- **`ListAuditLogs` ordering is not guaranteed.** Only the user-filtered GSI
  path returns timestamp-sorted results; the unfiltered path is a full table
  Scan with no ordering. Callers needing strict order should filter by user —
  or sort client-side, as the admin UI does. `Offset`-based pagination over a
  Scan is also O(full result set) per request.
- **Audit failures don't block admin actions.** `pkg/admin` records audit
  entries best-effort (log-and-continue on `RecordAudit` error), so the audit
  trail is high-fidelity but not transactional with the action it describes.
- **Circuit breaker has no `half-open` state machine.** `StateHalfOpen` is
  defined as a constant but the only transitions implemented are
  `closed → open` (on threshold) and `open → closed` (on auto-reset or manual
  reset). There is no probationary launch path today.
- **Manual `ResetCircuit` clears history.** It zeroes `interruption_count`
  and `first_interruption_at`, so a flaky type can't be reset-spammed without
  losing the data needed to spot the pattern.
- **SSM rate limits and parameter quota.** SSM Parameter Store has a default
  quota of 10,000 standard parameters per account and per-API throttling on
  `Put/Get`. Each runner gets one parameter; long retention or high
  concurrency can hit either limit. `Delete` swallows `ParameterNotFound` so
  duplicate cleanup is safe.
- **SSM `List` is paginated.** `GetParametersByPath` follows `NextToken` to
  completion; on a large fleet this means many round trips and counts against
  the API rate limit.
- **Vault token lifecycle is owned by the store.** The renewal goroutine is
  started by `NewVaultStore` and is only stopped by `Close()`. Leaking a
  `VaultStore` leaks a goroutine; tests using `NewVaultStoreWithClient` skip
  renewal entirely, which is intentional.
- **Vault `Get` for a missing secret returns an error**, not `(nil, nil)`,
  because the implementation explicitly returns `fmt.Errorf("secret not
  found")`. SSM's `Get` returns the SDK's not-found error from
  `GetParameter` — error shapes differ between backends. Callers needing a
  uniform "missing" signal should normalize at the call site.
- **Vault KV v2 delete uses metadata delete.** `Delete` calls
  `DeleteMetadata` on KV v2 to fully remove the secret (rather than soft-
  delete the latest version). Re-creating the same `runner-id` immediately
  is fine; relying on previous-version recovery is not.
- **DynamoDB TTL is opportunistic.** AWS deletes TTL-expired items "within
  a few days," not immediately. Don't treat a stale `runs-fleet-circuit-state`
  row as authoritative for current capacity — the auto-reset logic in
  `CheckCircuit` is what actually transitions state. Same for audit rows:
  entries may linger somewhat past 90 days.

## Sources [coverage: high]

- [pkg/db/dynamo.go](../../pkg/db/dynamo.go)
- [pkg/db/jobs.go](../../pkg/db/jobs.go)
- [pkg/db/job_status.go](../../pkg/db/job_status.go)
- [pkg/db/pool_config.go](../../pkg/db/pool_config.go)
- [pkg/db/locks.go](../../pkg/db/locks.go)
- [pkg/db/instance_claims.go](../../pkg/db/instance_claims.go)
- [pkg/db/audit.go](../../pkg/db/audit.go)
- [pkg/circuit/breaker.go](../../pkg/circuit/breaker.go)
- [pkg/secrets/store.go](../../pkg/secrets/store.go)
- [pkg/secrets/ssm.go](../../pkg/secrets/ssm.go)
- [pkg/secrets/vault.go](../../pkg/secrets/vault.go)
- [deploy/terraform/dynamodb.tf](../../deploy/terraform/dynamodb.tf)
