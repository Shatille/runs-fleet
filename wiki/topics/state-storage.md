---
topic: State Storage (DynamoDB + Circuit + Secrets)
last_compiled: 2026-08-21
sources_count: 22
---

# State Storage (DynamoDB + Circuit + Secrets)

## Purpose [coverage: high -- 22 sources]

runs-fleet persists five kinds of state across orchestrator restarts and
concurrent Fargate tasks:

1. **Job state** in DynamoDB (`runs-fleet-jobs`) — the lifecycle record, plus
   derived read models for the pool autoscaler, the admin console, and the cost
   reports.
2. **Pool state and every non-job durable row** in DynamoDB
   (`runs-fleet-pools`) — pool configuration plus *four* distinct sentinel-keyed
   row kinds sharing the same physical table: reconcile/task locks, instance
   claims, runner offline sightings, and per-day fleet cost rollups.
3. **Circuit breaker state** in DynamoDB (`runs-fleet-circuit-state`) — per
   instance type spot interruption tracking, used to flip new launches to
   on-demand when a type is unstable.
4. **Per-runner secrets** (registration credential, cache token, the full
   `RunnerConfig`) in AWS SSM Parameter Store (default), HashiCorp Vault, or a
   read-only env backend, all behind one `Store` interface.
5. **Admin audit log** in DynamoDB (`runs-fleet-audit`, PR #383) — append-only
   record of admin API actions with a 90-day TTL, read back by the admin UI's
   audit viewer.

State is intentionally split: hot operational state (jobs, claims, circuit
counts) lives in DynamoDB where conditional writes give us atomicity; sensitive
ephemeral material (registration credentials, cache HMAC) lives behind a secrets
backend with encryption at rest; the audit trail is a separate append-only
table so compliance data never contends with the job hot path.

**Durability asymmetry worth knowing up front:** the jobs table is *not* an
archive. `Tasks.ExecuteOldJobs`
([pkg/housekeeping/tasks.go:955](../../pkg/housekeeping/tasks.go)) scans for
`completed_at < now-7d` and `BatchWriteItem`-deletes the rows outright — no S3
archive, no cold copy, despite the doc comment saying "archives or deletes".
Anything computing a month-to-date figure from the jobs table therefore
silently truncates to about seven days. That is exactly why PR #455's fleet
cost rollups needed their own durable rows in the pools table
([pkg/db/fleet_cost.go:15-23](../../pkg/db/fleet_cost.go)).

## Architecture [coverage: high -- 22 sources]

**DynamoDB (`pkg/db`):** A single `Client` ([pkg/db/dynamo.go](../../pkg/db/dynamo.go))
wraps the AWS SDK v2 DynamoDB client and holds three table names — `poolsTable`,
`jobsTable`, `auditTable` — plus two optional GSI names for the jobs table
(`jobsPoolStatusGSI`, `jobsInstanceIDGSI`). Constructed via
`NewClient(cfg, poolsTable, jobsTable)` (or `NewClientWithAPI` for tests); the
audit table and GSIs are wired post-construction with setters. All calls go
through a six-method `DynamoDBAPI` interface (`GetItem`, `UpdateItem`, `Scan`,
`Query`, `PutItem`, `DeleteItem`) so tests can swap in a mock.

File split within `pkg/db`:

- [jobs.go](../../pkg/db/jobs.go) (1702 lines) — job lifecycle transitions,
  concurrency/autoscaling queries, admin read models.
- [job_status.go](../../pkg/db/job_status.go) — typed `JobStatus` constants,
  the *agent-vocabulary* constants, and `IsTerminal()`.
- [pool_config.go](../../pkg/db/pool_config.go) — pool CRUD, ephemeral pool
  creation, auto-tune recommendations, `IsReservedPoolKey`, and the shared
  `reapReservedRows` sweep.
- [locks.go](../../pkg/db/locks.go) — per-pool reconcile locks and housekeeping
  task locks (`__task_lock:` rows).
- [instance_claims.go](../../pkg/db/instance_claims.go) — instance claim
  locking (`__instance_claim:` rows) plus `HasLiveInstanceClaim` and the
  expired-claim reaper.
- [runner_sightings.go](../../pkg/db/runner_sightings.go) — first-offline
  sighting stamps (`__runner_offline:` rows), PR #436.
- [fleet_cost.go](../../pkg/db/fleet_cost.go) — per-UTC-day fleet cost rollups
  (`__fleet_day:` rows) and `ListBusyInstanceIDs`, PR #455.
- [active_repos.go](../../pkg/db/active_repos.go) — `ListActiveRepos`, the repo
  set derived from recent job records, PR #436.
- [audit.go](../../pkg/db/audit.go) — audit log writes and filtered reads.

**Circuit breaker (`pkg/circuit`):** `Breaker`
([pkg/circuit/breaker.go](../../pkg/circuit/breaker.go)) owns its own DynamoDB
client targeting `runs-fleet-circuit-state`, keyed by `instance_type`. It keeps
an in-process cache (`map[string]*CachedState`) with a 1-minute TTL to absorb
the read traffic from each fleet launch decision, plus a periodic cleanup
goroutine started via `StartCacheCleanup`.

**Secrets (`pkg/secrets`):** A `Store` interface
([pkg/secrets/store.go](../../pkg/secrets/store.go)) with four methods — `Put`,
`Get`, `Delete`, `List` — over a canonical `RunnerConfig` struct, plus a shared
`ErrConfigNotFound` sentinel. Three implementations:

- `SSMStore` ([pkg/secrets/ssm.go](../../pkg/secrets/ssm.go)) — writes
  `SecureString` parameters. Since PR #425 the config is written as **two**
  parameters, with the credential-packing helpers factored into
  [credential.go](../../pkg/secrets/credential.go).
- `VaultStore` ([pkg/secrets/vault.go](../../pkg/secrets/vault.go)) — Vault KV
  (auto-detects v1 vs v2), AWS IAM / Kubernetes / AppRole / token auth
  ([vault_auth.go](../../pkg/secrets/vault_auth.go)), background token renewal.
  Put/Get round-trip the whole struct through JSON rather than a hand-written
  field map (the fix in PR #398), so new `RunnerConfig` fields cannot be
  silently dropped.
- `EnvStore` ([pkg/secrets/env.go](../../pkg/secrets/env.go)) — read-only,
  reads `RUNS_FLEET_*` env vars for the case where bootstrap already fetched
  the config. `Put`/`Delete`/`List` all return errors by design.

PR #427 added [backend_parity_test.go](../../pkg/secrets/backend_parity_test.go),
which runs one contract suite against SSM, Vault KV v1, and Vault KV v2, because
the two backends deliberately store *different layouts* (SSM splits, Vault keeps
flat) and one agent binary must read whichever it is pointed at.

## Talks To [coverage: high -- 22 sources]

- **Amazon DynamoDB** — `runs-fleet-jobs`, `runs-fleet-pools`,
  `runs-fleet-circuit-state`, `runs-fleet-audit` (illustrative Terraform in
  [deploy/terraform/dynamodb.tf](../../deploy/terraform/dynamodb.tf); the real
  IaC lives in a separate repository). Conditional writes for atomicity;
  DynamoDB TTL on circuit rows, audit rows, and `claim_expiry` on the pools
  table.
- **`pkg/housekeeping`** — the biggest consumer. It drives every reaper in
  `pkg/db`: `DeleteExpiredInstanceClaims`, `DeleteStaleRunnerSightings`,
  `ExecuteOldJobs`, plus `ListActiveRepos` as the repo set for the
  orphaned-runner sweep and `AddFleetCostSample` / `GetFleetCostDays` /
  `ListBusyInstanceIDs` for the fleet cost sampler
  ([pkg/housekeeping/fleet_cost.go](../../pkg/housekeeping/fleet_cost.go)).
- **`pkg/pools`** — `ListPools`, `GetPoolConfig`, `UpdatePoolState`,
  `UpdatePoolReconcileResult`, `GetPoolBusyInstanceIDs`,
  `GetPoolP90Concurrency`, and the reconcile locks.
- **`pkg/cost`** — [fleetmtd.go](../../pkg/cost/fleetmtd.go) reads
  `GetFleetCostDays` for a fleet-wide MTD figure; the job-priced path reads
  `ListJobsForAdmin` with `CompletedOnly`.
- **`pkg/admin`** — job/pool read models, and the audit store's only
  producer/consumer (`RecordAudit` after mutating actions, `ListAuditLogs`
  behind `GET /api/audit-logs`). Gated via `HasAuditTable()`, not a nil check.
- **AWS SSM Parameter Store** — two `SecureString` parameters per runner under
  a configurable prefix (default `DefaultSSMPrefix`). `GetParametersByPath`
  (recursive, paginated) backs `List`; `AddTagsToResource` is a separate call.
- **HashiCorp Vault** — KV v1 or v2 mount (default `DefaultVaultKVMount`,
  `DefaultVaultPath`) over `github.com/hashicorp/vault/api`, 30s HTTP timeout,
  `LookupSelfWithContext` / `RenewSelfWithContext` for token lifecycle.

## API Surface [coverage: high -- 22 sources]

### `pkg/db` construction and feature gates

- `NewClient(cfg aws.Config, poolsTable, jobsTable string) *Client` /
  `NewClientWithAPI(api DynamoDBAPI, ...)`.
- `SetJobsPoolStatusGSI(name)` / `SetJobsInstanceIDGSI(name)` — flip the
  corresponding queries from Scan fallback to GSI Query.
- `SetAuditTable(name)` / `HasAuditTable() bool`, `HasJobsTable() bool`.

### Job lifecycle and reads ([pkg/db/jobs.go](../../pkg/db/jobs.go))

Transition mechanics live in [job-state-machine](job-state-machine.md); the
storage-facing surface:

- `SaveJob(ctx, *JobRecord) error` — PutItem with status `launched`, stamping
  `created_at` with the assignment time; carries `OriginalLabel`.
- `ClaimJob(ctx, jobID, runID, repo) error` — self-expiring claim lease
  (compare-and-swap on observed `created_at`); `ErrJobAlreadyClaimed`,
  `ErrJobClaimExhausted` (after `claimMaxAttempts` = 3).
- `FailExhaustedClaim(ctx, jobID) error`
- `MarkJobStarted(ctx, jobID, startedAt) (*events.JobInfo, error)`
- `MarkJobComplete(ctx, jobID, instanceID, status string, exitCode, duration) (*events.JobInfo, error)`
  — note the `instanceID` parameter: the write is now **conditioned on
  `instance_id = :iid`** so a stale agent cannot clobber a re-dispatched record.
- `MarkJobCancelled(ctx, jobID) (bool, error)` — `launched → cancelled`
  (PR #417), for a job GitHub cancelled before its runner confirmed.
- `MarkJobRequeuedByJobID(ctx, jobID) (bool, error)` — `running`/`launched` →
  `requeued`; `(false, nil)` on conditional failure.
- `MarkInstanceTerminating`, `UpdateJobMetrics`, `DeleteJobClaim`
- `GetJobByJobID` (O(1) GetItem), `GetJobByInstance` (instance-id GSI or Scan),
  `LastJobCompletionForInstance`
- `QueryPoolJobHistory(ctx, poolName, since)`,
  `GetPoolP90Concurrency(ctx, poolName, windowHours)`,
  `GetPoolBusyInstanceIDs(ctx, poolName)`
- Admin helpers: `ListJobsForAdmin` (returns `([]AdminJobEntry, int, error)`,
  the int being the pre-limit/offset match total), `GetJobForAdmin`,
  `GetJobStatsForAdmin`.
- Errors: `ErrJobAlreadyClaimed`, `ErrJobClaimExhausted`,
  `ErrInstanceAlreadyClaimed`, `ErrPoolAlreadyExists`, `ErrPoolNotFound`,
  `ErrPoolReconcileLockHeld`, `ErrTaskLockHeld`.

`AdminJobFilter` now carries five bounds beyond `Limit`/`Offset`: `Status`,
`Pool`, `Since` (inclusive lower on `created_at`), `Until` (exclusive upper),
and two attribute-existence predicates added because status strings are
unreliable (see Gotchas):

- `CompletedOnly bool` — selects on `attribute_exists(completed_at)` rather
  than any status value.
- `StaleBefore time.Time` — the inverse: records with no `completed_at`,
  created before the cutoff. `AdminJobStats.Stalled` is derived from it and
  deliberately overlaps `Running`/`Requeued`.

### Pool configuration ([pkg/db/pool_config.go](../../pkg/db/pool_config.go))

- `GetPoolConfig(ctx, poolName) (*PoolConfig, error)` — reserved keys resolve
  to `(nil, nil)` rather than phantom pools.
- `ListPools(ctx) ([]string, error)` — **fully paginated** over
  `LastEvaluatedKey` since PR #402, with `IsReservedPoolKey` filtering.
- `UpdatePoolState(ctx, poolName, running, stopped, effectiveDesiredRunning, effectiveDesiredStopped) error`
  — observed counts and resolved targets in one write, so the two can never
  disagree across passes.
- `SavePoolConfig(ctx, *PoolConfig) error` — targeted UpdateItem with an
  explicit SET list, so pool CRUD never clobbers lock attributes,
  reconcile-result attributes, or the tuner's `auto_tune` recommendation.
- `CreateEphemeralPool` (conditional put; `ErrPoolAlreadyExists`),
  `TouchPoolActivity`, `UpdatePoolReconcileResult`,
  `UpdatePoolAutoTune(ctx, poolName, AutoTuneRec)`, `DeletePoolConfig`
  (conditioned on `ephemeral = true`).
- `IsReservedPoolKey(poolName) bool` — the shared sentinel-prefix guard.
- `reapReservedRows(ctx, prefix, attr, cutoff, what) (int, error)` —
  unexported; the one scan-and-conditional-delete sweep both reserved-row
  reapers are built on.

### Locks, claims, and sightings

- `AcquirePoolReconcileLock` / `ReleasePoolReconcileLock` — lock attributes on
  the pool's own row.
- `AcquireTaskLock(ctx, taskType, owner, ttl)` / `ReleaseTaskLock` —
  housekeeping singletons via `__task_lock:<taskType>` rows.
- `ClaimInstanceForJob(ctx, instanceID, jobID, ttl) error` — conditional
  UpdateItem on `pool_name = "__instance_claim:<instanceID>"`;
  `ErrInstanceAlreadyClaimed`.
- `ReleaseInstanceClaim(ctx, instanceID, jobID) error` — conditional DeleteItem,
  no-ops when held by another job.
- `HasLiveInstanceClaim(ctx, instanceID) (bool, error)` — `ConsistentRead`
  GetItem. **An unreadable claim counts as held**: the caller uses it to decide
  whether destroying the instance is safe, and an unanswered question is not a
  yes ([instance_claims.go:144-183](../../pkg/db/instance_claims.go)).
- `DeleteExpiredInstanceClaims(ctx, now) (int, error)` — PR #402's reaper.
- `RecordRunnerOffline(ctx, repo, runnerID, now) (time.Duration, error)` —
  `if_not_exists` stamp of `first_seen_offline`, returning the accumulated
  offline age; `ForgetRunnerOffline`; `DeleteStaleRunnerSightings(ctx, now)`.
- `ListActiveRepos(ctx) ([]string, error)` — distinct `repo` values with
  `created_at` inside `activeRepoWindow` (7 days).

See the [per-resource-locking](../concepts/per-resource-locking.md) concept for
how the lock flavors compose.

### Fleet cost rollups ([pkg/db/fleet_cost.go](../../pkg/db/fleet_cost.go), PR #455)

- `AddFleetCostSample(ctx, day string, d FleetCostDelta) error` — folds one
  sampling tick into its day's row using DynamoDB's atomic `ADD` for every
  monetary and duration field, `SET` for the `last_sample_at` checkpoint, and a
  latching `partial = true` when the tick's window had to be clamped.
- `GetFleetCostDays(ctx, fromDay, toDay) ([]FleetCostDay, error)` — paginated
  Scan with `begins_with(pool_name, "__fleet_day:")` and a `#day BETWEEN`
  filter, **`ConsistentRead: true`**.
- `ListBusyInstanceIDs(ctx) ([]string, error)` — fleet-wide (all pools *and*
  cold-start instances), filtered on `busyJobStatuses` plus
  `created_at >= now-maxConcurrencyRuntime`, deduplicated.
- `FleetDayFormat = "2006-01-02"`; types `FleetCostDelta`, `FleetCostDay`.

### Audit log ([pkg/db/audit.go](../../pkg/db/audit.go))

- `RecordAudit(ctx, AuditEntry) error` — PutItem; ULID `id` and `timestamp`
  minted internally; empty `User` stored as `"anonymous"`.
- `ListAuditLogs(ctx, AuditFilter) ([]AuditEntry, error)` — `user-index` Query
  when `filter.User` is set (Scan fallback on ValidationException), full
  paginated Scan otherwise; `Offset`/`Limit` applied in memory.

### `pkg/circuit`

- `NewBreaker(cfg aws.Config, tableName string) *Breaker`
- `RecordInterruption(ctx, instanceType) error`,
  `CheckCircuit(ctx, instanceType) (State, error)` (cache-first, auto-resets
  when `auto_reset_at` is past), `ResetCircuit(ctx, instanceType) error`,
  `StartCacheCleanup(ctx) <-chan struct{}` (5-minute tick).
- Types: `State` (`StateClosed`, `StateOpen`, `StateHalfOpen`), `Record`,
  `CachedState`. Constants: `InterruptionThreshold = 3`,
  `TimeWindow = 15 * time.Minute`, `CooldownPeriod = 30 * time.Minute`.

### `pkg/secrets`

```go
type Store interface {
    Put(ctx, runnerID string, config *RunnerConfig) error
    Get(ctx, runnerID string) (*RunnerConfig, error)
    Delete(ctx, runnerID string) error
    List(ctx) ([]string, error)
}
var ErrConfigNotFound = errors.New("runner config not found")
```

`RunnerConfig` ([store.go:17-65](../../pkg/secrets/store.go)) now carries two
mutually-exclusive credential fields plus several opt-in feature blocks:

- `RegistrationToken string \`json:"jit_token"\`` — a GitHub *registration*
  token for `config.sh --token`. The field was renamed from `JITToken` because
  it never was a JIT credential, **but the JSON tag stays `jit_token`
  deliberately**: it is a frozen wire contract with agents already in flight.
- `JITConfig string \`json:"jit_config,omitempty"\`` — GitHub's encoded
  just-in-time config, handed to `run.sh` via
  `ACTIONS_RUNNER_INPUT_JITCONFIG`. When set it supersedes `RegistrationToken`.
- `CreatedAt string` (RFC3339) — bounds how long an agent could still be
  acting on this config, which is how housekeeping tells a live assignment from
  an abandoned one.
- `BuildkitCache{Bucket,Region,Prefix}`, `RunnerLogs{Bucket,Prefix}` — all
  `omitempty`, all inert when absent in either direction.
- `Org`, `Repo`, `RunID`, `Labels`, `RunnerGroup`, `RunnerName`, `JobID`,
  `CacheToken`, `CacheURL`, `TerminationQueueURL`, `IsOrg`.

Constructors: `NewSSMStore(awsCfg, prefix)` / `NewSSMStoreWithClient(SSMAPI, prefix)`;
`NewVaultStore(ctx, VaultConfig)` / `NewVaultStoreWithClient(client, kvMount, basePath, kvVersion)`
plus `(*VaultStore).Close()`; `NewEnvStore()`.

Credential packing ([credential.go](../../pkg/secrets/credential.go)):
`marshalConfigHalf`, `packCredential`, `compressJITConfig`, `unpackCredential`,
`storedCredential`, `credentialMaxDecodedBytes = 1 << 20`.

## Data [coverage: high -- 22 sources]

### `runs-fleet-jobs` table

Partition key: `job_id` (Number). Three GSIs:

- `pool-created-at-index` (`pool`, `created_at`) — **required**; backs
  `QueryPoolJobHistory`. Missing it means a Scan fallback and a WARN every
  reconcile loop.
- `instance-id-index` (`instance_id`) — optional, via
  `RUNS_FLEET_JOBS_INSTANCE_ID_GSI`. Backs `GetJobByInstance`.
- `pool-status-index` (`pool`, `status`) — optional, via
  `RUNS_FLEET_JOBS_POOL_STATUS_GSI`. Backs `GetPoolBusyInstanceIDs`.

Attributes: `job_id` (N), `run_id` (N), `repo` (S), `instance_id` (S,
omitempty), `instance_type` (S), `pool` (S, omitempty), `spot` (BOOL),
`retry_count` (N), `warm_pool_hit` (BOOL), `status` (S, omitempty — an empty
status is dropped rather than written, guarding the status-keyed GSI),
`created_at` (S, RFC3339), `spot_request_id` (S, omitempty), `persistent_spot`
(BOOL, omitempty), `trace_id` (S, omitempty — extracted from the W3C
traceparent), and **`original_label`** (S, omitempty — the runs-on label the
workflow actually asked for; see [job-state-machine](job-state-machine.md)).
Mutated by lifecycle methods: `started_at`, `completed_at`, `requeued_at`,
`exit_code`, `duration_seconds`.

**`created_at` semantics:** during `claiming` it is the lease timestamp written
by `ClaimJob` (staleness is judged against it, and it is the compare-and-swap
pin for re-claims). `SaveJob`'s PutItem replaces the whole record at
assignment, re-stamping `created_at` — so on any `launched`-or-later record it
means *assignment time*.

**Status vocabulary.** `pkg/db/job_status.go` defines eleven `JobStatus`
constants (`launched`, `running`, `claiming`, `terminating`, `requeued`,
`completed`, `success`, `failed`, `error`, `orphaned`, `cancelled`) — and then
three more for a *second, different vocabulary*:

```go
// Agent-reported terminal statuses. The agent writes its own vocabulary
// (pkg/agent/telemetry.go) straight through MarkJobComplete, so the table holds
// "failure" rather than JobStatusFailed and carries "timeout"/"interrupted",
// which have no JobStatus constant.
JobStatusAgentFailure     JobStatus = "failure"
JobStatusAgentTimeout     JobStatus = "timeout"
JobStatusAgentInterrupted JobStatus = "interrupted"
```

`IsTerminal()` spans both vocabularies and deliberately excludes `requeued`,
`terminating`, and `orphaned` (the last because `ExecuteOrphanedJobs` stamps it
on a swallowed EC2 lookup error, so a transient API fault must not get a live
instance reaped).

### `runs-fleet-pools` table

Partition key: `pool_name` (S). One physical table, **five** logical concerns:

1. **Pool config rows** (`PoolConfig`): `instance_type`, `desired_running`,
   `desired_stopped`, `current_running`, `current_stopped`,
   `effective_desired_running` / `effective_desired_stopped` (`*int` — nil
   "never reconciled" is distinct from a target of zero),
   `idle_timeout_minutes`, `schedules`, `ephemeral`, `last_job_time`, the
   flexible spec fields (`arch`, `cpu_min`/`cpu_max`, `ram_min`/`ram_max`,
   `families`, `multi_spec`), reconcile observability (`last_reconcile_at`,
   `last_reconcile_result`), hot-pool admin overrides
   (`override_linger_minutes`, `override_max_hot` — three-state `*int`: nil =
   auto, `&0` = force cold, `&N` = fixed), the tuner's `auto_tune`
   (`AutoTuneRec`), and reconcile-lock attributes (`reconcile_lock_owner`,
   `reconcile_lock_expires`).
2. **Task lock rows** — `__task_lock:<taskType>` (`taskLockPrefix`), one per
   housekeeping singleton.
3. **Instance claim rows** — `__instance_claim:<instanceID>`
   (`instanceClaimPrefix`). Attributes `job_id` (N), `claimed_at` (N, unix),
   `claim_expiry` (N, unix). Conditional acquire:
   `attribute_not_exists(pool_name) OR attribute_not_exists(claim_expiry) OR claim_expiry < :now OR job_id = :job_id`.
4. **Runner offline sighting rows** (PR #436) —
   `__runner_offline:<repo>:<runnerID>` (`runnerSightingPrefix`). Attributes
   `first_seen_offline` (N, unix, written with `if_not_exists` so the *original*
   stamp survives every sweep) and an **inert** `ttl` (N). Keyed per repo
   because runner IDs are allocated per repo.
5. **Fleet cost rollup rows** (PR #455) — `__fleet_day:<YYYY-MM-DD>`
   (`fleetDayPrefix`). Attributes `day` (S), `cost_usd`, `compute_usd`,
   `ebs_usd`, `instance_seconds`, `attributed_seconds` (all N, accumulated with
   atomic `ADD`), `partial` (BOOL, latching), `last_sample_at` (N, unix). The
   `day` key's `2006-01-02` layout is lexicographically ordered, which is what
   makes a string range filter a date range filter. **These rows carry no
   expiry and no reaper** — one per day is ~365/year and they are the only
   surviving record of what the fleet cost.

**The single-TTL constraint.** The pools table's one DynamoDB TTL attribute is
spent on `claim_expiry`
([deploy/terraform/dynamodb.tf](../../deploy/terraform/dynamodb.tf)). Any
*second* row kind's own `ttl` attribute is therefore inert — stated explicitly
in [runner_sightings.go:20-25](../../pkg/db/runner_sightings.go) — which is why
sightings are reaped by `DeleteStaleRunnerSightings` instead, and why
`__fleet_day:` rows do not even try.

### `runs-fleet-circuit-state` table

Partition key: `instance_type` (S). From `circuit.Record`: `state`
(`closed | open | half-open`), `interruption_count` (N),
`first_interruption_at`, `last_interruption_at`, `opened_at`, `auto_reset_at`
(S, RFC3339), `ttl` (N, unix — `auto_reset_at + 1h`).

### `runs-fleet-audit` table (PR #383)

Partition key: `id` (S, ULID — time-sortable). GSI `user-index` on
(`user`, `timestamp`), **required by schema** (no setter, unlike the jobs
table's optional GSIs). Attributes: `id`, `user` (`"anonymous"` when
unauthenticated), `action`, `target` (omitempty), `result`, `details` (M,
omitempty), `client_ip` (omitempty), `timestamp` (S, RFC3339), `ttl` (N —
write time + `auditRetention`, 90 days).

### SSM parameter naming — now two parameters per runner (PR #425)

```
{prefix}/{runner-id}/config       # plaintext fields, SecureString, tagged
{prefix}/{runner-id}/credential   # JIT config or registration token, SecureString, UNTAGGED
```

A GitHub JIT config is base64 of a JSON document whose bulk is a 2048-bit RSA
private key; at ~4100 characters it alone exceeds SSM's 4096-character Standard
tier ceiling, and combining both halves would force the whole store onto the
paid Advanced tier. The credential parameter therefore stores the JIT config
**base64-decoded then gzipped then re-base64'd** — decoding GitHub's outer
base64 layer before compressing saves ~29% versus ~8% for compressing the
base64 text directly. `storedCredential.Compressed` records which form was
written rather than inferring it, so a value where compression did not pay off
reads back unambiguously.

Tags (`runs-fleet:managed=true`, `runs-fleet:job-id=<id>`) go on the **config**
parameter only, in a separate `AddTagsToResource` call (PR #426), and never on
the credential parameter — a tag value is readable by anyone with
`DescribeTags`, and that parameter registers a runner.

`extractRunnerID` matches **both** `config` and `credential` suffixes, so
`List` reports a runner holding either half.

### Vault paths

- KV v1: `<kvMount>/<basePath>/<runnerID>` (logical write/read).
- KV v2: `client.KVv2(kvMount).Put(ctx, basePath+"/"+runnerID, data)`. `List`
  uses `<kvMount>/metadata/<basePath>` for v2 vs `<kvMount>/<basePath>` for v1.
- Payload is the whole `RunnerConfig` marshalled to JSON then unmarshalled into
  `map[string]interface{}` — one flat secret, no split, because Vault has no
  4096-character cap.

## Key Decisions [coverage: high -- 22 sources]

- **One pools table, five row kinds, one sentinel-prefix guard.** Claims, task
  locks, sightings, and fleet-day rollups all live in `runs-fleet-pools`,
  disambiguated by `__`-prefixed partition keys. This avoids four extra tables
  and lets a crashed orchestrator's locks expire on their own — at the cost of
  a rule every enumerating path must obey (see Gotchas).
- **DynamoDB conditional writes for distributed locks.** `ClaimJob` is a
  compare-and-swap lease; `ClaimInstanceForJob` uses a four-clause condition
  that bakes in TTL-based stealing; reconcile and task locks use the same shape.
- **Reapers, not TTL, for anything but the first row kind.** With the pools
  table's single TTL attribute committed to `claim_expiry`, every other
  sentinel row needs an application-level sweep.
  `reapReservedRows` is the one implementation both use: paginated Scan on the
  prefix, then a **conditional** `DeleteItem` re-checking the age attribute per
  row. Unconditional or batch deletes were rejected because a row rewritten
  between the scan and the delete is still live — dropping a renewed claim would
  cause double-assignment, and dropping a re-stamped sighting would restart an
  offline clock and delay a deregistration.
- **Fleet cost accumulates with `ADD`, not read-modify-write** (PR #455).
  Several orchestrator replicas may sample concurrently and a lost update would
  undercount the fleet with no external signal. `last_sample_at` is `SET`
  because it is a checkpoint, not an accumulator; `partial` latches true and is
  never cleared, because one clamped tick means the day understates and a later
  clean tick does not undo that.
- **`GetFleetCostDays` uses `ConsistentRead: true`** — the only place in
  `pkg/db` that reads consistently besides `HasLiveInstanceClaim`. The sampler
  reads `last_sample_at` and then prices the window *since* it, so an
  eventually-consistent read would let a replica re-price a window the previous
  tick already counted. The housekeeping task lock serializes executions but
  does not make a stale read fresh.
- **Fleet rollups are durable because job records are not.** `ExecuteOldJobs`
  hard-deletes at 7 days, so a month-to-date figure derived from job records
  decays for calendar reasons rather than attribution ones —
  [pkg/cost/fleetmtd.go:32-34](../../pkg/cost/fleetmtd.go) says so explicitly,
  and that is why `AttributedPercent` comes from the sampler's own busy-vs-total
  instance-seconds rather than from dividing two independently sourced numbers.
- **Attribute existence over status strings for "finished".** `CompletedOnly`
  and `StaleBefore` key on `completed_at`, which only `MarkJobComplete` writes.
  Filtering on `status == "completed"` matched almost nothing because terminal
  rows carry GitHub's raw conclusion (PR #403 — see Gotchas).
- **`ListPools` paginates** (PR #402). A single non-paginated Scan returned only
  DynamoDB's first 1 MB page; with the table bloated by leaked claim rows, real
  pools past that page were invisible to the reconciler, the hot-pool tuner, and
  the admin console.
- **`HasLiveInstanceClaim` fails closed.** An unreadable claim row counts as
  held, because the caller is deciding whether destroying an instance is safe.
- **`SavePoolConfig`'s SET list is deliberately narrow.** Lock attributes,
  `last_reconcile_*`, `effective_desired_*`, and `auto_tune` are all written by
  their own targeted UpdateItems and excluded from the CRUD path, so pool
  saves, lock traffic, reconcile observability, and tuner recommendations never
  clobber each other despite sharing a row. The hot-pool *overrides* are the
  exception — they *are* in the SET list, so a nil pointer clears the override.
- **SSM split for the free tier** (PR #425). Two parameters per runner keeps
  both halves under the 4096-character Standard-tier ceiling. Write order is
  credential-first, delete order is config-first, so either crash point leaves
  at most a lone credential — which `List` still enumerates and the sweep still
  deletes — rather than a config that boots a runner and hangs.
- **Legacy credential layout stays readable.** `SSMStore.loadCredential`
  treats an absent credential parameter as the *old* layout (credential inline
  in the config JSON) rather than an error, because agents on an older AMI and a
  newer orchestrator coexist for the length of a rollout. Only a config with
  neither a credential parameter nor an inline credential is a real failure.
- **`marshalConfigHalf` shadows the credential fields via a struct alias**
  rather than zeroing a copy: `RegistrationToken`'s `jit_token` tag has no
  `omitempty` (frozen wire contract), so a zeroed copy would still emit an empty
  `jit_token` — putting a credential-shaped key in the *plaintext* parameter.
- **One `Store` contract, three backends, one parity suite** (PR #427). SSM is
  the default (zero ops, IAM-native); Vault is opt-in for forked environments;
  `EnvStore` covers pre-fetched bootstrap config. The layouts differ on purpose,
  so the parity tests hold all three to the same behavior.
- **Vault serializes through JSON, not a field map** (PR #398). A
  hand-maintained map silently dropped newly added `RunnerConfig` fields; a
  round-trip cannot.
- **Circuit breaker keyed per instance type, not per pool**, with auto-reset
  baked into reads (`CheckCircuit` rewrites the row to `closed` when
  `auto_reset_at` is past, so no background reconciler is needed) and a
  two-tier cache (1-minute in-process, 5-minute cleanup tick).
- **Audit is config-gated and server-stamped.** `SetAuditTable` +
  `HasAuditTable()` is the flag (not a nil check); `RecordAudit` mints the ULID
  and timestamp internally so callers cannot fabricate or reorder history; a
  90-day TTL bounds growth with no cleanup job, which is also what makes
  in-memory `Offset`/`Limit` acceptable.

## Gotchas [coverage: high -- 22 sources]

- **A new sentinel prefix that skips `IsReservedPoolKey` becomes a phantom
  pool. This has now happened twice.** Prefixes are declared in per-feature
  files (`taskLockPrefix` in `locks.go`, `instanceClaimPrefix` in
  `instance_claims.go`, `runnerSightingPrefix` in `runner_sightings.go`,
  `fleetDayPrefix` in `fleet_cost.go`) while the predicate lives in
  `pool_config.go`. When PR #436's `__runner_offline:` rows were not added to
  it, they surfaced as zero-count pools in the admin Pools tab and were walked
  by the reconciler, the ephemeral-pool cleanup, and the hot tuner (fixed in
  PR #438, 724dc71). Every such row also inflates per-pool CloudWatch metric
  cardinality — one zero-valued series per ephemeral instance ID.

  There is now a guard: `TestIsReservedPoolKeyCoversEverySentinelPrefix`
  ([pkg/db/pool_config_reserved_test.go](../../pkg/db/pool_config_reserved_test.go))
  parses every non-test file in `pkg/db`, collects every `__`-prefixed string
  constant, and fails if `IsReservedPoolKey` does not filter it. **If you add a
  row kind to the pools table, add its prefix to `IsReservedPoolKey` — the test
  will tell you, but only if the prefix is a plain string literal constant in
  `pkg/db`.**
- **A second row kind's `ttl` attribute does nothing.** DynamoDB allows one TTL
  attribute per table, and the pools table's is `claim_expiry`. The `ttl`
  written on every `__runner_offline:` row is inert
  ([runner_sightings.go:20-25](../../pkg/db/runner_sightings.go)); those rows
  survive only because `DeleteStaleRunnerSightings` reaps them. Do not add a new
  row kind with a `ttl` attribute and assume it will be collected.
- **Reserved-row reapers only cover repos/instances something still revisits.**
  The orphaned-runner sweep writes sightings only for repos `ListActiveRepos`
  reports, so a repo that stops using runs-fleet strands its rows; the reaper is
  the only thing that collects them. `ListActiveRepos` itself only looks back
  `activeRepoWindow` (7 days) — and since `ExecuteOldJobs` deletes job rows at
  7 days, that window is effectively the whole table.
- **`ExecuteOldJobs` hard-deletes with no archive.** Despite the comment
  "archives or deletes old job records", the implementation
  ([tasks.go:955](../../pkg/housekeeping/tasks.go)) only ever
  `BatchWriteItem`-deletes. Any metric, report, or console view computing
  month-to-date from the jobs table truncates to ~7 days.
- **Stored status is often GitHub's raw conclusion, not a `JobStatus`
  constant.** `MarkJobComplete` takes a bare `status string` and writes whatever
  it is given; the agent's own vocabulary (`success`/`failure`/`timeout`/
  `interrupted`, from [pkg/agent/telemetry.go](../../pkg/agent/telemetry.go))
  goes straight through. Filtering on `status == "completed"` therefore returns
  near-zero: live July data matched 1 job / 0.0 hours instead of 8,102 / 432.3
  hours, which broke the admin Cost tab, the Metrics tab's cost figure, and the
  daily cost report (PR #403). Use `CompletedOnly` / `StaleBefore`
  (attribute-existence on `completed_at`), never a status string.
- **`MarkJobComplete` is conditioned on `instance_id`.** An agent booting from
  a stale config can report a `job_id` whose record has since been
  re-dispatched; the condition makes that a `(nil, nil)` no-op instead of a
  clobbered live retry. Callers must handle the nil.
- **Scan fallbacks are silent except for a WARN.** `GetJobByInstance` and
  `GetPoolBusyInstanceIDs` use their GSIs only when
  `RUNS_FLEET_JOBS_INSTANCE_ID_GSI` / `RUNS_FLEET_JOBS_POOL_STATUS_GSI` are
  set; unset means a paginated Scan every call — correct but RCU-expensive.
- **`QueryPoolJobHistory`'s GSI path reads a single page.** The Scan fallback
  paginates fully; the Query path stops at DynamoDB's ~1 MB page, so a pool
  sustaining tens of thousands of jobs an hour would compute p90 concurrency
  from a truncated dataset (reading *under* the true value).
- **DynamoDB filters run after the read.** `ListActiveRepos`,
  `ListBusyInstanceIDs`, and `reapReservedRows` all say so in comments: their
  filters trim the response, not the RCU. `ListBusyInstanceIDs`' age bound is a
  correctness bound (a leaked stale row would otherwise mark an instance busy
  forever and inflate the attributed share), not a cost one.
- **`created_at` changes meaning across the claim boundary.** On a `claiming`
  record it is the lease timestamp; `SaveJob` re-stamps it at assignment.
  `events.JobInfo.CreatedAt` documents assignment time and is only meaningful
  on `launched`-or-later records.
- **Stale code comment on `jobRecord`.** [jobs.go:46](../../pkg/db/jobs.go)
  still says "Primary key is instance_id"; the table's hash key is `job_id`,
  and every lifecycle method keys on it. `instance_id` is an attribute plus the
  optional GSI hash key.
- **Reads are eventually consistent almost everywhere.** Only
  `GetFleetCostDays` and `HasLiveInstanceClaim` set `ConsistentRead: true`.
  Conditional writes on the same partition key are still atomic, which is why
  mutations use conditional expressions rather than read-modify-write.
- **Instance-claim release is best-effort.** `ReleaseInstanceClaim` swallows
  `ConditionalCheckFailedException` as a debug log; don't rely on the claim's
  state after the call.
- **`ListAuditLogs` ordering is not guaranteed.** Only the user-filtered GSI
  path returns timestamp-sorted results; the unfiltered path is an unordered
  Scan, and `Offset` pagination over it is O(full result set) per request.
  Audit failures also don't block admin actions (`pkg/admin` logs and
  continues), so the trail is high-fidelity but not transactional.
- **Circuit breaker has no half-open state machine.** `StateHalfOpen` is a
  defined constant, but only `closed → open` (threshold) and `open → closed`
  (auto/manual reset) are implemented. Manual `ResetCircuit` also zeroes
  `interruption_count` and `first_interruption_at`, so reset-spamming a flaky
  type destroys the evidence.
- **SSM now consumes two parameters and two writes per runner.** The default
  quota is 10,000 standard parameters per account with per-API throttling; the
  split halves the effective headroom. `Delete` swallows `ParameterNotFound`
  (safe duplicate cleanup) and deletes both halves even if the first fails,
  because a surviving credential is a live registration credential outliving
  its instance.
- **Credential decompression is capped at 1 MiB** and reads one byte past the
  cap to distinguish "filled it" from "had more to give" — an oversized
  credential fails at `unpackCredential` rather than reaching the agent
  silently truncated. `unpackCredential` also assigns only after every fallible
  step passes, so a rejected credential leaves the config untouched rather than
  half-applied.
- **A non-base64 JIT config is stored uncompressed rather than rejected.**
  GitHub's format is not a contract this package controls, and a credential that
  cannot be stored means a job that never runs.
- **Backend error shapes are now unified — mostly.** Both SSM and Vault wrap
  missing configs in `ErrConfigNotFound`, so `errors.Is` works across backends
  (PR #427's parity suite enforces this). But `EnvStore` returns plain
  `fmt.Errorf` for its missing-variable cases, and `Put`/`Delete`/`List` on it
  always error by design.
- **Vault KV v2 delete uses metadata delete.** `Delete` calls `DeleteMetadata`
  to fully remove the secret rather than soft-deleting the latest version;
  previous-version recovery is not available.
- **Vault token lifecycle is owned by the store.** The renewal goroutine starts
  in `NewVaultStore` and stops only on `Close()`; leaking a `VaultStore` leaks
  a goroutine. `NewVaultStoreWithClient` skips renewal entirely, intentionally.
- **DynamoDB TTL is opportunistic.** AWS deletes expired items "within a few
  days", not immediately. A stale `runs-fleet-circuit-state` row is not
  authoritative — `CheckCircuit`'s auto-reset is what actually transitions
  state. Audit rows may likewise linger past 90 days.

## Sources [coverage: high]

- [pkg/db/dynamo.go](../../pkg/db/dynamo.go)
- [pkg/db/jobs.go](../../pkg/db/jobs.go)
- [pkg/db/job_status.go](../../pkg/db/job_status.go)
- [pkg/db/pool_config.go](../../pkg/db/pool_config.go)
- [pkg/db/pool_config_reserved_test.go](../../pkg/db/pool_config_reserved_test.go)
- [pkg/db/locks.go](../../pkg/db/locks.go)
- [pkg/db/instance_claims.go](../../pkg/db/instance_claims.go)
- [pkg/db/runner_sightings.go](../../pkg/db/runner_sightings.go)
- [pkg/db/fleet_cost.go](../../pkg/db/fleet_cost.go)
- [pkg/db/active_repos.go](../../pkg/db/active_repos.go)
- [pkg/db/audit.go](../../pkg/db/audit.go)
- [pkg/circuit/breaker.go](../../pkg/circuit/breaker.go)
- [pkg/secrets/store.go](../../pkg/secrets/store.go)
- [pkg/secrets/ssm.go](../../pkg/secrets/ssm.go)
- [pkg/secrets/credential.go](../../pkg/secrets/credential.go)
- [pkg/secrets/vault.go](../../pkg/secrets/vault.go)
- [pkg/secrets/env.go](../../pkg/secrets/env.go)
- [pkg/secrets/backend_parity_test.go](../../pkg/secrets/backend_parity_test.go)
- [pkg/housekeeping/tasks.go](../../pkg/housekeeping/tasks.go)
- [pkg/housekeeping/fleet_cost.go](../../pkg/housekeeping/fleet_cost.go)
- [pkg/cost/fleetmtd.go](../../pkg/cost/fleetmtd.go)
- [deploy/terraform/dynamodb.tf](../../deploy/terraform/dynamodb.tf)
