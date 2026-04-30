---
topic: State Storage (DynamoDB + Circuit + Secrets)
last_compiled: 2026-04-30
sources_count: 7
---

# State Storage (DynamoDB + Circuit + Secrets)

## Purpose [coverage: high -- 7 sources]

runs-fleet persists three kinds of state across orchestrator restarts and concurrent
Fargate tasks:

1. **Job and pool state** in DynamoDB (`runs-fleet-jobs`, `runs-fleet-pools`) — job
   lifecycle, instance claims for distributed locking across multi-instance Fargate.
2. **Circuit breaker state** in DynamoDB (`runs-fleet-circuit-state`) — per
   instance type spot interruption tracking, used to flip new launches to
   on-demand when a type is unstable.
3. **Per-runner secrets** (JIT registration tokens, cache tokens, full
   `RunnerConfig` JSON) in either AWS SSM Parameter Store (default) or HashiCorp
   Vault, fronted by a single `Store` interface so callers don't care which
   backend is wired up.

State is intentionally split: hot operational state (jobs, claims, circuit
counts) lives in DynamoDB where conditional writes give us atomicity; sensitive
ephemeral material (JIT tokens, cache HMAC) lives behind a secrets backend with
encryption at rest.

## Architecture [coverage: high -- 7 sources]

**DynamoDB (`pkg/db`):** A single `Client` (`pkg/db/dynamo.go`) wraps the AWS
SDK v2 DynamoDB client and holds two table names — `poolsTable` and
`jobsTable`. The client is constructed once via `NewClient(cfg, poolsTable,
jobsTable)`. All DynamoDB calls go through a small `DynamoDBAPI` interface
(`GetItem`, `UpdateItem`, `Scan`, `Query`, `PutItem`, `DeleteItem`) so tests can
swap in a mock. Job operations are in `pkg/db/jobs.go`; instance-claim locking
is in `pkg/db/instance_claims.go` and reuses the pools table.

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

## Talks To [coverage: high -- 7 sources]

- **Amazon DynamoDB** — `runs-fleet-jobs`, `runs-fleet-pools`,
  `runs-fleet-circuit-state` tables. Conditional writes for atomicity, TTL
  attribute for circuit-state cleanup.
- **AWS SSM Parameter Store** — `SecureString` parameters under a configurable
  prefix (default `DefaultSSMPrefix`), one per runner. `GetParametersByPath`
  used for `List`.
- **HashiCorp Vault** — KV v1 or v2 mount (default `DefaultVaultKVMount`,
  `DefaultVaultPath`). Talks via `github.com/hashicorp/vault/api` over HTTPS
  with a 30s HTTP timeout; uses `Auth().Token().LookupSelfWithContext` and
  `RenewSelfWithContext` for token lifecycle.

## API Surface [coverage: high -- 7 sources]

### `pkg/db` (jobs and instance claims)

Construction:

- `NewClient(cfg aws.Config, poolsTable, jobsTable string) *Client`
- `(*Client).HasJobsTable() bool` — guard for callers when the jobs table is
  optional.

Job lifecycle (`pkg/db/jobs.go`):

- `SaveJob(ctx, *JobRecord) error` — inserts a record with status
  `"running"` keyed by `instance_id`.
- `ClaimJob(ctx, jobID int64) error` — atomic put with
  `attribute_not_exists(job_id)`. Returns `ErrJobAlreadyClaimed` on conflict.
- `DeleteJobClaim(ctx, jobID) error` — used to clean up a claim on processing
  failure.
- `MarkJobComplete(ctx, jobID, status, exitCode, duration) error` — sets
  `status`, `exit_code`, `duration_seconds`, `completed_at`.
- `UpdateJobMetrics(ctx, jobID, startedAt, completedAt) error`
- `MarkInstanceTerminating(ctx, instanceID) error` — looks up the running job
  for an instance and flips its status to `"terminating"`.
- `MarkJobRequeuedByJobID(ctx, jobID) (bool, error)` — conditional update from
  `"running"` → `"requeued"`. Returns false (not error) on conditional check
  failure.
- `MarkJobRequeued(ctx, instanceID)` — deprecated, uses the wrong key.
- `GetJobByInstance(ctx, instanceID) (*events.JobInfo, error)` — Scan with
  filter (no GSI today; see Gotchas).
- `GetJobByJobID(ctx, jobID) (*events.JobInfo, error)` — O(1) primary-key
  GetItem.
- `QueryPoolJobHistory(ctx, poolName, since) ([]JobHistoryEntry, error)`
- `GetPoolPeakConcurrency(ctx, poolName, windowHours) (int, error)`
- `GetPoolP90Concurrency(ctx, poolName, windowHours) (int, error)` — samples
  concurrency at 1-minute intervals, returns `samples[floor(0.9 * (N-1))]`.
- `GetPoolBusyInstanceIDs(ctx, poolName) ([]string, error)`
- Admin API helpers: `ListJobsForAdmin`, `GetJobForAdmin`,
  `GetJobStatsForAdmin` returning `AdminJobEntry`, `AdminJobFilter`,
  `AdminJobStats`.

Instance claim locking (`pkg/db/instance_claims.go`):

- `ClaimInstanceForJob(ctx, instanceID, jobID, ttl) error` — conditional
  UpdateItem on the **pools** table using a synthetic key
  `pool_name = "__instance_claim:<instanceID>"`. Acquires if no claim exists,
  if the existing claim has expired (`claim_expiry < now`), or if this job
  already owns it. Returns `ErrInstanceAlreadyClaimed` on conflict.
- `ReleaseInstanceClaim(ctx, instanceID, jobID) error` — DeleteItem with
  `attribute_exists(pool_name) AND job_id = :job_id`. Silently no-ops if the
  claim is held by another job (treats conditional check failure as success).

Errors: `ErrJobAlreadyClaimed`, `ErrInstanceAlreadyClaimed`.

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

## Data [coverage: high -- 7 sources]

### `runs-fleet-jobs` table

Partition key: `job_id` (Number). The `instance_id`-keyed `SaveJob` path is
historical — see Gotchas.

Attributes (from `jobRecord` and admin parsing):

- `instance_id` (S), `job_id` (N), `run_id` (N), `repo` (S),
  `instance_type` (S), `pool` (S), `spot` (BOOL), `retry_count` (N),
  `warm_pool_hit` (BOOL), `status` (S), `created_at` (S, RFC3339),
  `spot_request_id` (S, omitempty), `persistent_spot` (BOOL, omitempty).
- Mutated by lifecycle methods: `started_at`, `completed_at`, `requeued_at`,
  `exit_code`, `duration_seconds`.

Status values observed: `claiming`, `running`, `requeued`, `terminating`,
`completed`, `success`, `failed`, `error`, `orphaned`.

### `runs-fleet-pools` table

Partition key: `pool_name` (S). Reused for two distinct concerns:

1. Real pool config rows (out of scope for these sources).
2. **Instance claim rows** — `pool_name = "__instance_claim:<instanceID>"`
   (prefix constant `instanceClaimPrefix`). Attributes: `job_id` (N),
   `claimed_at` (N, unix), `claim_expiry` (N, unix). Conditional acquire is:
   `attribute_not_exists(pool_name) OR attribute_not_exists(claim_expiry) OR
   claim_expiry < :now OR job_id = :job_id`.

### `runs-fleet-circuit-state` table

Partition key: `instance_type` (S). Schema from `circuit.Record`:

- `instance_type` (S)
- `state` (S) — `closed | open | half-open`
- `interruption_count` (N)
- `first_interruption_at`, `last_interruption_at`, `opened_at`,
  `auto_reset_at` (S, RFC3339)
- `ttl` (N, unix) — set to `auto_reset_at + 1h` for DynamoDB TTL cleanup of
  resolved entries.

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

## Key Decisions [coverage: high -- 7 sources]

- **DynamoDB conditional writes for distributed locks.** `ClaimJob` uses
  `attribute_not_exists(job_id)`; `ClaimInstanceForJob` uses a four-clause
  condition that bakes in TTL-based stealing. There is no separate lock
  table — claims and pool config share `runs-fleet-pools`, disambiguated by a
  `__instance_claim:` prefix on the partition key. This avoids a second table
  and lets a crashed orchestrator's claims expire on their own.
- **`MarkJobRequeuedByJobID` returns `(false, nil)` on conditional failure**,
  not an error. Callers can treat "already requeued / terminating" as a
  benign no-op rather than an alarm.
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
  auto_reset_at + 1h` so DynamoDB GC reclaims resolved entries automatically.
  Instance claims rely on application-level expiry rather than DynamoDB TTL
  because they need to be stealable mid-flight.
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

## Gotchas [coverage: high -- 7 sources]

- **`GetJobByInstance` is a `Scan`, not a `Query`.** The jobs table is keyed
  on `job_id` only; there's no GSI on `instance_id` yet. The TODO in
  `pkg/db/jobs.go` flags this as acceptable while the table is small (~1000
  items, ephemeral) and the call site (spot interruption handling) is
  low-frequency. Don't add hot-path callers without first adding the GSI.
- **Two key shapes in the jobs table.** `SaveJob` marshals a record whose
  primary attribute is `instance_id`, but every other lifecycle method
  (`ClaimJob`, `MarkJobComplete`, `MarkJobRequeuedByJobID`, etc.) keys on
  `job_id`. The deprecated `MarkJobRequeued(instanceID)` is wired to the wrong
  key shape — use `MarkJobRequeuedByJobID`.
- **`GetPoolBusyInstanceIDs` and `QueryPoolJobHistory` are Scans.** Same
  caveat as above — fine at ~100 jobs/day, will need a `(pool, status)` or
  `(pool, created_at)` GSI for high-volume deployments.
- **Reads are eventually consistent by default.** None of the DynamoDB calls
  in these files set `ConsistentRead: true`. Conditional writes on the same
  partition key are still atomic, but read-then-write patterns can race
  across orchestrators — which is exactly why mutations use conditional
  expressions instead of read-modify-write.
- **Instance-claim release is best-effort.** `ReleaseInstanceClaim` swallows
  `ConditionalCheckFailedException` (claim already gone or owned by someone
  else) as a debug log. Callers shouldn't rely on the claim being present
  after the call.
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
  `CheckCircuit` is what actually transitions state.

## Sources [coverage: high]

- [pkg/db/dynamo.go](../../pkg/db/dynamo.go)
- [pkg/db/jobs.go](../../pkg/db/jobs.go)
- [pkg/db/instance_claims.go](../../pkg/db/instance_claims.go)
- [pkg/circuit/breaker.go](../../pkg/circuit/breaker.go)
- [pkg/secrets/store.go](../../pkg/secrets/store.go)
- [pkg/secrets/ssm.go](../../pkg/secrets/ssm.go)
- [pkg/secrets/vault.go](../../pkg/secrets/vault.go)
