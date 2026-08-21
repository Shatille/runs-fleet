---
topic: Warm Pools (Reconciliation)
last_compiled: 2026-08-21
sources_count: 8
---

# Warm Pools (Reconciliation)

## Purpose [coverage: high -- 8 sources]

Warm pools pre-provision EC2 instances so workflow jobs start in seconds rather
than waiting on a cold EC2 boot. A single reconciler continuously drives each
pool toward a desired ready/stopped state, supporting hot pools (instances kept
running), stopped pools (batch-started on demand), and ephemeral pools that
auto-create from the first matching job and self-clean after idle.

Since PRs #399/#401 the package also implements **hot pools**: an opt-in,
Helm-gated mode where a pool keeps one or more *running* spares alive for a
short, demand-following "linger" window after job activity, so back-to-back
pipeline stages land on a live agent instead of paying the stopped-instance
boot. Per-pool warmth is not configured — an hourly auto-tuner
([pkg/housekeeping/hot_tuner.go](../../pkg/housekeeping/hot_tuner.go)) derives
each pool's linger/maxHot from its real run history, and an operator may set a
per-pool override in the admin UI that wins over the recommendation.

Warm pools are EC2-only: the K8s pool manager that once mirrored this behavior
for Helm-deployed placeholder pods was removed upstream (see Key Decisions).

## Architecture [coverage: high -- 8 sources]

The reconciler is `Manager` in
[pkg/pools/manager.go](../../pkg/pools/manager.go).

### The two paths into `reconcilePool`

`ReconcileLoop(ctx)` (manager.go:323) creates a
`time.NewTicker(reconcileInterval)` where `reconcileInterval = 60 * time.Second`
(manager.go:52), runs one immediate `reconcile(ctx)`, then selects on three
cases:

| case | handler | scope |
|---|---|---|
| `<-ctx.Done()` | return | — |
| `<-ticker.C` | `reconcile(ctx)` | every pool from `ListPools` |
| `<-m.demandCh` | `reconcileDemand(ctx, name)` | only the named pool(s) |

`NotifyPoolDemand(poolName)` (manager.go:279) is a **non-blocking** send into
`demandCh`, a channel with buffer **64** created in `NewManager`; when full the
notification is silently dropped and the caller relies on the next tick.
`reconcileDemand` drains the channel into a `map[string]struct{}` to dedup, then
calls `reconcilePool` once per unique pool. It is fed once per **queued**
`workflow_job` webhook from the post-ack hook in
[cmd/server/main.go:759](../../cmd/server/main.go), gated on a non-empty pool
name ≤63 chars matching `validPoolName`.

**There is no other debouncing.** The per-pool DynamoDB reconcile lock
(`reconcileLockTTL = reconcileInterval + 5*time.Second`, i.e. 65s —
manager.go:59) is *released in a `defer` at the end of each pass*
(manager.go:443), not held for its full TTL. So a webhook arriving one second
after a pass completes re-acquires the lock immediately and reconciles the pool
again. Under webhook load a pool is reconciled far more often than once per
60s. This is exactly the property that made pool reconciliation the fleet's
largest CloudWatch emitter (see Key Decisions).

### `reconcilePool` flow (manager.go:412)

1. **Lock.** `AcquirePoolReconcileLock(ctx, poolName, m.instanceID, reconcileLockTTL)`.
   `db.ErrPoolReconcileLockHeld` / `db.ErrPoolNotFound` return `nil` (skip
   silently); the wait is timed into `PublishLockWaitSeconds("pool_reconcile")`
   on both the success and failure branch. Three deferred closures are then
   registered — LIFO, so they run in the order: persist outcome → release lock →
   publish `PoolReconcileSeconds`.
2. **Outcome recorder.** `UpdatePoolReconcileResult(poolName, result, now)`
   writes `last_reconcile_at` and `last_reconcile_result` (`"success"` or
   `"failed: …"` truncated to `maxReconcileResultLen` = 300 runes) *while the
   lock is still held*. `ErrPoolNotFound` and shutdown errors are swallowed.
3. **Resolve targets.** `getScheduledDesiredCounts(poolConfig)` (time-of-day /
   day-of-week schedules), then `getEphemeralAutoScaledCount` for ephemeral
   pools (P90 concurrency over a 1-hour window, always `desiredRunning=0`), then
   the hot-pool **linger floor**: `lingerDesiredRunning(poolConfig, now)` returns
   `maxHot` if `now - LastJobTime < lingerMinutes`, else 0, and is applied via
   `max()` so it only ever *raises* `desiredRunning`. `lingerActive` flags the
   raise so scale-ups get `reason="linger"` on the `pool_actions` metric.
4. **Observe.** `getPoolInstances` runs `DescribeInstances` filtered on
   `tag:runs-fleet:pool=<name>`, `tag:runs-fleet:managed=true`, and states
   `pending|running|stopping|stopped`. It reads the durable
   `runs-fleet:assigned` tag into `PoolInstance.Assigned`, populates
   CPU/RAM/Arch/Family/Gen via `fleet.GetInstanceSpec`, and seeds in-memory
   `IdleSince` for any running instance not yet tracked.
5. **Account.** `GetPoolBusyInstanceIDs(poolName)` gives the busy set.
   `running`/`stopped` come from `countInstanceStates`; then a second inline
   pass over running instances splits them into `busy`, `assignedIdle`
   (not busy but `Assigned`), and the remainder:
   **`ready = running - busy - assignedIdle`**. Nonzero `assignedIdle` is logged
   with the instance IDs.
6. **Refresh dwell streaks.** `updateReadySince(poolName, runningInstances, busyIDs, now)`
   runs on *both* scale directions so the dwell guard always has continuous
   history.
7. **Scale on `ready`, not `running`.**
   - `ready < desiredRunning`: `startInstances` from the stopped reserve first,
     then `createPoolFleetInstances` (on-demand) for the remainder.
   - `ready > desiredRunning`: build candidates via `stopEligibleSpares`
     (warm pools, `desiredRunning == 0 && desiredStopped > 0`) or
     `stopEligibleSpares` + `filterIdleInstances` (hot pools — the idle timeout
     is applied *on top of* the same guard chain since PR #412). Then
     `withoutLiveConfig` drops any candidate whose runner config still exists.
     Spot candidates are split off (`StopInstances` rejects spot) and routed to
     terminate; on-demand candidates are stopped up to the `desiredStopped`
     headroom, with overflow terminated.
8. **Stopped reserve.** `stopped > desiredStopped` terminates the excess,
   excluding instances a local claim has reserved
   (`poolLock.filterNotInFlight`). `stopped < desiredStopped && desiredRunning == 0`
   creates on-demand instances for
   `desiredStopped - stopped - stoppedCount - withinGraceSpares` — crediting
   both instances stopped this same pass and still-booting/dwelling spares so
   the deficit doesn't over-provision.
9. **Publish + persist.** Nine gauges per pass (see Data), then
   `UpdatePoolState(poolName, running, stopped, desiredRunning, desiredStopped)`
   — note the last two are the *resolved effective* targets this pass acted on,
   not the configured seed.

### The scale-down guard chain

`stopEligibleSpares` (manager.go:1031) is the single funnel both branches use.
An instance must clear every gate:

- not busy and not already `Assigned` (`filterReadyInstances`)
- not reserved by a concurrent local claim (`filterNotInFlight`)
- past `bootstrapGracePeriod` (`defaultBootstrapGracePeriod` = 3 minutes;
  a zero `LaunchTime` counts as still booting)
- continuously not-busy for `readyDwellPeriod` (`defaultReadyDwellPeriod` = 90
  seconds, deliberately > one `reconcileInterval` so a single missed "busy"
  observation cannot get a live instance stopped)

Then `withoutLiveConfig` probes `RunnerConfigChecker.HasRunnerConfig` per
candidate, keeping (not stopping) any instance with a live config *or* an
unreadable answer, and logging the deferred instance IDs — a count alone can't
distinguish a rotating set from a stuck one.

### Instance creation and claiming

`createPoolFleetInstances`: `resolvePoolInstanceTypes` translates the pool's
flexible spec (or a legacy pinned `InstanceType`) into candidate types,
`RankInstanceTypesByPrice` expands them into an inverse-price-weighted
interleaved sequence, and each instance picks uniformly at random from that
sequence (`ranked[m.randIntn(len(ranked))]`) — cheaper types win
proportionally more often. Each launch gets a round-robin `SubnetID` plus the
full `SubnetIDs` list and a `Reason` (`ready_deficit` / `linger` /
`stopped_replenish`).

`ClaimAndStartPoolInstance(ctx, poolName, jobID, repo, spec)` is the
assignment path:

1. dead-context guard → `ErrClaimContextDone` (retryable)
2. `getPoolInstances` (unlocked)
3. `claimCandidates` orders eligible instances. **Master-off is a free gate**:
   when `HotPoolsEnabled` is false it returns spec-matched *stopped* instances
   only, with **no** `GetPoolConfig` read and **no** busy query. Master-on and a
   live effective linger prepends spec-matched running∧not-busy∧not-spot∧not-Assigned
   spares, so a hot spare is tried before any stopped instance. Both groups are
   sorted CPU-then-RAM ascending (best-fit).
4. per candidate: `poolLock.reserve` (in-memory, released before any network
   call) → `ClaimInstanceForJob` (`instanceClaimTTL` = 5 min) → on
   `ErrInstanceAlreadyClaimed`, next candidate.
5. **running candidate**: re-check `HasRunnerConfig` *after* the claim (config
   presence while holding the claim is by definition stale), then
   `markInstanceAssigned` — tag `runs-fleet:assigned=true` + `Role=<repo>`,
   drop from idle/dwell tracking. **No `StartInstances`** — the standby agent
   already running on the host picks up the config the caller writes next.
6. **stopped candidate**: `StartInstanceForJob` (StartInstances +
   `markInstanceAssigned`); on failure release the DynamoDB claim.

`AvailableInstance.IsFromRunningSpare()` reports `State == "running"`, which
[internal/worker/warmpool.go](../../internal/worker/warmpool.go) records as
`HotPoolHit` on the job record.

### Auto-tuner

[pkg/housekeeping/hot_tuner.go](../../pkg/housekeeping/hot_tuner.go)'s
`deriveAutoTune(entries, caps)` is pure (no I/O, no clock beyond `TunedAt`).
`Tasks.ExecutePoolHotTuner` (pkg/housekeeping/tasks.go:1706) drives it on the
hourly housekeeping timer, deduplicated across replicas by the task lock:

- returns immediately when `HotPoolsEnabled` is false — zero DynamoDB load for
  fleets that never enable the feature
- one fully-paginated `ListJobsForAdmin(Since: now - LookbackDays)`, grouped by
  pool **in memory** (a per-pool GSI query would truncate at one page)
- every existing pool gets a fresh recommendation via `UpdatePoolAutoTune`,
  including the cold ones

`deriveAutoTune` is cold-until-proven: `< caps.MinJobsToActivate` jobs →
`Reason = "insufficient-history"`, linger 0. Jobs that never cluster (every
inter-job gap > `caps.BurstGapMinutes`) → `"no-burst-pattern"`, linger 0.
Otherwise `RecommendedLingerMinutes = ceil(p90IntraBurstGap / 60)` clamped to
`[1, caps.MaxLingerMinutes]` and `RecommendedMaxHot = peakConcurrency` clamped
to `[1, caps.MaxHot]`, with `Reason = "tuned"`. `peakConcurrency` is a ±1 event
sweep over `[CreatedAt, CompletedAt]` intervals; `p90Float` uses the same index
rule as `GetPoolP90Concurrency` (`floor(0.9*(N-1))` over ascending samples).

`Manager.effectiveHotSpec(pc)` (manager.go:829) resolves the precedence
**override > auto-recommendation > off**, clamps to `m.config.HotPoolCaps`, and
returns `(0, 0)` whenever the master toggle is off, the config is nil, or the
resolved linger is ≤0 (including an operator's `&0` force-cold). When linger is
active but maxHot resolves below 1, maxHot floors to 1.

## Talks To [coverage: high -- 8 sources]

- **[pkg/db](../../pkg/db)** — the `DBClient` interface (manager.go:78) covers
  `GetPoolConfig`, `UpdatePoolState`, `ListPools`, `GetPoolP90Concurrency`,
  `GetPoolBusyInstanceIDs`, `AcquirePoolReconcileLock`,
  `ReleasePoolReconcileLock`, `UpdatePoolReconcileResult`,
  `ClaimInstanceForJob`, `ReleaseInstanceClaim`. Implementations:
  [pkg/db/pool_config.go](../../pkg/db/pool_config.go) (table CRUD +
  `UpdatePoolAutoTune`), [pkg/db/locks.go](../../pkg/db/locks.go)
  (reconcile-lock and task-lock primitives),
  [pkg/db/instance_claims.go](../../pkg/db/instance_claims.go) (claim
  conditional writes, `HasLiveInstanceClaim`, `DeleteExpiredInstanceClaims`).
- **[pkg/fleet](../../pkg/fleet)** — `FleetAPI` exposes `CreateFleet`,
  `CreateOnDemandInstance`, `RankInstanceTypesByPrice`. Pools always call
  `CreateOnDemandInstance`. `fleet.FlexibleSpec`, `fleet.InstanceSpec`,
  `fleet.GetInstanceSpec`, `fleet.LaunchSpec` provide spec matching and launch
  params. See [fleet-orchestration](fleet-orchestration.md).
- **[pkg/github](../../pkg/github)** — `github.JobConfig` and
  `github.ResolveFlexibleSpec` translate pool-level CPU/RAM/family requirements
  into concrete instance types. A families-less spec falls back to
  `fleet.DefaultFlexibleFamilies`, which excludes burstable `t3`/`t4g` since
  PR #385.
- **[pkg/config](../../pkg/config)** — `config.Config` supplies `SubnetIDs`
  (round-robined by `selectSubnet`), `HotPoolsEnabled`, and `HotPoolCaps`
  (parsed by
  [pkg/config/hot_pools.go](../../pkg/config/hot_pools.go)).
- **[pkg/housekeeping](../../pkg/housekeeping)** — hosts the hourly auto-tuner
  (`ExecutePoolHotTuner`), the expired-instance-claim reaper
  (`ExecuteExpiredInstanceClaims`), and the ephemeral-pool cleanup
  (`ExecuteEphemeralPoolCleanup`, which calls `DeletePoolConfig`). See
  [housekeeping](housekeeping.md).
- **[pkg/metrics](../../pkg/metrics)** — the optional `MetricsAPI` publishes
  pool gauges, action counters, reconcile duration, and lock-wait histograms; a
  nil client is a no-op throughout. See [observability](observability.md).
- **[pkg/secrets](../../pkg/secrets)** (via the `RunnerConfigChecker`
  interface) — `HasRunnerConfig` is the earliest durable assignment signal,
  written before the job record reaches the busy GSI and deleted only when the
  instance's termination is processed.
- **[pkg/admin](../../pkg/admin)** — pool CRUD, the `override_linger_minutes` /
  `override_max_hot` three-state overrides (validated against `hotPoolCaps` in
  `Handler.validateOverrides`), and the pools view that renders
  `effective_desired_*` / `last_reconcile_*` / `auto_tune`. See
  [admin-ui](admin-ui.md).
- **AWS EC2 API** — `EC2API` needs `DescribeInstances`, `StartInstances`,
  `StopInstances`, `TerminateInstances`, `CreateTags`.
- **Pool queue / SQS** — webhooks with `pool=` labels feed the pool queue; pool
  processors call `ClaimAndStartPoolInstance`, and `cmd/server` calls
  `NotifyPoolDemand` on the queued webhook to skip the 60s ticker.

## API Surface [coverage: high -- 5 sources]

```go
// pkg/pools/manager.go
type DBClient interface { /* see "Talks To" */ }
type FleetAPI interface { /* see "Talks To" */ }
type EC2API  interface { /* see "Talks To" */ }

type RunnerConfigChecker interface {
    HasRunnerConfig(ctx context.Context, instanceID string) (bool, error)
}

type MetricsAPI interface {
    PublishPoolAction(ctx context.Context, pool, action, reason string) error
    PublishPoolDesired(ctx context.Context, pool, kind string, n int) error
    PublishPoolInstances(ctx context.Context, pool, state string, n int) error
    PublishPoolReconcileSeconds(ctx context.Context, seconds float64) error
    PublishLockWaitSeconds(ctx context.Context, lock string, seconds float64) error
    PublishInstances(ctx context.Context, state, capacity, pool string, n int) error
}

type PoolInstance struct {
    InstanceID, State, InstanceType string
    LaunchTime, IdleSince           time.Time
    Spot                            bool // one-time spot; cannot be stopped
    Assigned                        bool // runs-fleet:assigned tag present
    CPU                             int
    RAM                             float64
    Arch, Family                    string
    Gen                             int
}

type AvailableInstance struct{ InstanceID, InstanceType, State string }
func (a *AvailableInstance) IsFromRunningSpare() bool

var ErrNoAvailableInstance = errors.New("no available instance in pool")
var ErrClaimContextDone    = errors.New("claim aborted: context already done")

type Manager struct { /* unexported */ }
func NewManager(dbClient DBClient, fleetManager FleetAPI, cfg *config.Config) *Manager
func (m *Manager) SetEC2Client(ec2Client EC2API)
func (m *Manager) SetMetrics(metrics MetricsAPI)
func (m *Manager) SetRunnerConfigChecker(c RunnerConfigChecker)
func (m *Manager) ReconcileLoop(ctx context.Context)
func (m *Manager) NotifyPoolDemand(poolName string)
func (m *Manager) MarkInstanceBusy(instanceID string)
func (m *Manager) MarkInstanceIdle(instanceID string)
func (m *Manager) GetAvailableInstance(ctx context.Context, poolName string) (*AvailableInstance, error)
func (m *Manager) StartInstanceForJob(ctx context.Context, instanceID, repo string) error
func (m *Manager) ClaimAndStartPoolInstance(ctx context.Context, poolName string, jobID int64, repo string, spec *fleet.FlexibleSpec) (*AvailableInstance, error)
func (m *Manager) StopPoolInstance(ctx context.Context, instanceID string) error
```

```go
// pkg/config/hot_pools.go
type HotPoolCaps struct {
    MaxLingerMinutes  int `json:"maxLingerMinutes"`
    MaxHot            int `json:"maxHot"`
    MinJobsToActivate int `json:"minJobsToActivate"`
    LookbackDays      int `json:"lookbackDays"`
    BurstGapMinutes   int `json:"burstGapMinutes"`
}
func DefaultHotPoolCaps() HotPoolCaps
func (c HotPoolCaps) WithDefaults() HotPoolCaps
func ParseHotPoolCaps(jsonStr string) (HotPoolCaps, error)
```

```go
// pkg/db/pool_config.go
type PoolSchedule struct { /* see Data */ }
type PoolConfig   struct { /* see Data */ }
type AutoTuneRec  struct { /* see Data */ }
var ErrPoolAlreadyExists = errors.New("pool already exists")

func IsReservedPoolKey(poolName string) bool
func (c *Client) GetPoolConfig(ctx context.Context, poolName string) (*PoolConfig, error)
func (c *Client) ListPools(ctx context.Context) ([]string, error)
func (c *Client) UpdatePoolState(ctx context.Context, poolName string, running, stopped, effectiveDesiredRunning, effectiveDesiredStopped int) error
func (c *Client) SavePoolConfig(ctx context.Context, cfg *PoolConfig) error
func (c *Client) CreateEphemeralPool(ctx context.Context, cfg *PoolConfig) error
func (c *Client) TouchPoolActivity(ctx context.Context, poolName string) error
func (c *Client) UpdatePoolReconcileResult(ctx context.Context, poolName, result string, at time.Time) error
func (c *Client) UpdatePoolAutoTune(ctx context.Context, poolName string, rec AutoTuneRec) error
func (c *Client) DeletePoolConfig(ctx context.Context, poolName string) error // ephemeral-only

// pkg/db/locks.go
var ErrPoolReconcileLockHeld = errors.New("pool reconciliation lock held by another instance")
var ErrPoolNotFound          = errors.New("pool not found")
var ErrTaskLockHeld          = errors.New("task lock held by another instance")
func (c *Client) AcquirePoolReconcileLock(ctx context.Context, poolName, owner string, ttl time.Duration) error
func (c *Client) ReleasePoolReconcileLock(ctx context.Context, poolName, owner string) error
func (c *Client) AcquireTaskLock(ctx context.Context, taskType, owner string, ttl time.Duration) error
func (c *Client) ReleaseTaskLock(ctx context.Context, taskType, owner string) error

// pkg/db/instance_claims.go
var ErrInstanceAlreadyClaimed = errors.New("instance already claimed by another job")
func (c *Client) ClaimInstanceForJob(ctx context.Context, instanceID string, jobID int64, ttl time.Duration) error
func (c *Client) ReleaseInstanceClaim(ctx context.Context, instanceID string, jobID int64) error
func (c *Client) HasLiveInstanceClaim(ctx context.Context, instanceID string) (bool, error)
func (c *Client) DeleteExpiredInstanceClaims(ctx context.Context, now time.Time) (int, error)
```

## Data [coverage: high -- 6 sources]

DynamoDB table `runs-fleet-pools`, partition key `pool_name` (string).

`PoolConfig` attributes ([pkg/db/pool_config.go](../../pkg/db/pool_config.go)):

| Attribute | Type | Notes |
|---|---|---|
| `pool_name` | S | Partition key |
| `instance_type` | S | Legacy single instance type |
| `desired_running` | N | Configured target ready (idle) instances |
| `desired_stopped` | N | Configured target stopped instances |
| `current_running` / `current_stopped` | N | Observed, written by `UpdatePoolState` |
| `effective_desired_running` / `effective_desired_stopped` | N | The targets the last pass *actually resolved* (after ephemeral auto-scaling and any linger floor). Nil ≠ zero: nil means never reconciled. Absent from `SavePoolConfig`'s SET list |
| `idle_timeout_minutes` | N | Hot-pool idle threshold (`filterIdleInstances` defaults to 10 when ≤0) |
| `schedules` | L | List of `PoolSchedule` entries |
| `ephemeral` | BOOL | Auto-create / auto-delete pool |
| `last_job_time` | S (RFC3339) | Written by `TouchPoolActivity`; drives both the ephemeral scale-down window and the hot-pool linger window |
| `last_reconcile_at` | S (RFC3339) | Written by `UpdatePoolReconcileResult` |
| `last_reconcile_result` | S | `"success"` or `"failed: …"`, capped at 300 runes |
| `override_linger_minutes` / `override_max_hot` | N (nullable) | Admin three-state override: nil = use auto, `0` = force cold, `N` = fixed. **Are** in `SavePoolConfig`'s SET list via `attrOrNull`, so a nil clears them |
| `auto_tune` | M | `AutoTuneRec`. Tuner-written only, deliberately **excluded** from `SavePoolConfig`'s SET list so an admin save can never clobber a recommendation |
| `arch` | S | `arm64` / `amd64` |
| `cpu_min` / `cpu_max` | N | vCPU range |
| `ram_min` / `ram_max` | N | RAM range (GB) |
| `families` | L | e.g. `["c7g","m7g"]` |
| `multi_spec` | BOOL | Demand-driven mixed-spec pool (declared, unused by the reconciler) |
| `reconcile_lock_owner` | S | Written by `AcquirePoolReconcileLock` |
| `reconcile_lock_expires` | N | Unix epoch seconds |

`PoolSchedule`: `name`, `start_hour` (0-23), `end_hour` (0-23), `days_of_week`
(`[]int`, 0=Sunday), `desired_running`, `desired_stopped`. An overnight range
(start > end) is handled as `hour >= start || hour < end`.

`AutoTuneRec` (the recommendation **and its evidence**, so the admin UI can
explain why a pool is hot or cold): `recommended_linger_minutes`,
`recommended_max_hot`, `window_days`, `job_count`, `burst_count`,
`p90_intra_burst_gap_seconds`, `peak_concurrency`, `reason`
(`tuned` | `insufficient-history` | `no-burst-pattern`), `tuned_at`.

**Sentinel-prefixed rows share this table.** `IsReservedPoolKey` excludes four
prefixes — `__task_lock:`, `__instance_claim:`, the runner-sighting prefix, and
the fleet-day prefix — from both `ListPools` and `GetPoolConfig`, so they never
surface as phantom pools. Instance claims carry `job_id`, `claimed_at`,
`claim_expiry`.

**Gauges published per pool per reconcile pass (9 total):**

| call | series | dimensions |
|---|---|---|
| `PublishPoolInstances` ×5 | `PoolInstances` / `pool_instances` | `PoolName`, `State ∈ {running, stopped, ready, busy, assigned_idle}` |
| `PublishPoolDesired` ×2 | `PoolDesired` / `pool_desired` | `PoolName`, `Kind ∈ {running, stopped}` |
| `PublishInstances` ×2 | `Instances` | `State ∈ {running, stopped}`, `Capacity = on_demand`, `PoolName` |

Plus per-pass `PoolReconcileSeconds`, `LockWaitSeconds{Lock=pool_reconcile}`,
and one `PoolActions{Action,Reason}` counter **per affected instance**.

## Key Decisions [coverage: high -- 8 sources]

- **PR #456 (this branch): pool reconciliation is now the single writer of the
  pool gauges.** `ExecutePoolAudit` in housekeeping was removed
  (commit `a833fa2`): its only effect was publishing `PoolInstances` and
  `PoolDesired` under the *same* names and dimension tuples that
  `reconcilePool` (manager.go ~737-752) already writes — a second, staler
  writer into one time series, reading back out of DynamoDB what reconcile had
  just written via `UpdatePoolState`. Two writers at different cadences means
  the 10-minute sample becomes the datapoint for any minute reconcile misses,
  and `Sum` double-counts on overlap. It also paid for a non-paginated `Scan`
  of the pools table every 10 minutes to produce the duplicates.
  `PublishPoolInstances` / `PublishPoolDesired` were dropped from
  housekeeping's `MetricsAPI` entirely.
- **…and the CloudWatch backend now ships disabled by default** (commit
  `61e280c`). Nothing queried the `RunsFleet` namespace — no alarms, no
  dashboards, and the admin console is DynamoDB-backed. Meanwhile every metric
  cost one un-batched `PutMetricData`. These 9 gauges per pool per pass, on a
  60s ticker *and again on every queued-job webhook* (because the reconcile
  lock is released on completion rather than held for its TTL), were the
  largest single emitter in the fleet, feeding nothing. Both the Go default and
  the Helm chart value flipped; every `Publish*` call site is untouched, so
  re-enabling is one env var.
- **Hot pools: one master toggle, deliberately no per-pool config.**
  `RUNS_FLEET_HOT_POOLS_ENABLED` (default false) is the only Helm switch. The
  reasoning recorded in `docs/CONFIGURATION.md`: most repo users can't touch
  the deployment chart, so per-pool linger/maxHot is auto-deduced from run
  history instead, with an admin override on top. With the toggle off the pool
  path makes **zero** extra DynamoDB reads — `claimCandidates` short-circuits
  before `GetPoolConfig`, `effectiveHotSpec` returns `(0,0)`, and
  `ExecutePoolHotTuner` returns immediately.
- **Caps are the only cost containment, and they fail fast.**
  `ParseHotPoolCaps` rejects negatives and enforces hard ceilings
  (`maxLingerMinutes ≤ 120`, `maxHot ≤ 10`, `lookbackDays ≤ 90`,
  `burstGapMinutes ≤ 120`) at *startup* rather than at reconcile time.
  `Handler.validateOverrides` reads the same `HotPoolCaps`, so an operator can
  never exceed the ceiling the tuner respects.
- **Linger is a floor, not an override.** `reconcilePool` applies
  `lingerDesiredRunning` via `max()`, so it only raises `desiredRunning` during
  a burst tail and never lowers a scheduled or admin target.
- **Linger only fires for pools that stamp `last_job_time`** — in practice the
  label-created ephemeral pools, which are exactly the ones paying the
  stopped-instance boot today. Persistent pools use `desired_running` /
  schedules instead and never activate linger. This is intentional, and
  documented as such on `lingerDesiredRunning`.
- **A hot spare is assigned, not started.** The running-instance claim path
  calls `markInstanceAssigned` and skips `StartInstances` entirely; the standby
  agent already polling on the host picks up the config the caller writes next.
  The one-shot posture is unchanged — a hot spare serves exactly one job then
  self-terminates.
- **`runs-fleet:assigned` is an EC2 tag, not in-memory state.** The agent reads
  its config once at boot and never revalidates, so a *running* instance that
  already consumed a config can never serve another claim. A durable tag
  survives a replica restart where the in-memory idle map does not (PR #416).
- **PR #433 (`c2fc00e`) / PR #451 (`09538c1`): never destroy an instance already
  promised to a job.** `HasLiveInstanceClaim` exists because
  `ClaimInstanceForJob` writes to the pools table *before* `StartInstances` is
  called — during that window EC2 still reports the instance stopped and the
  jobs table holds no record, so `GetJobByInstance == nil` means "not yet
  bound", not "safe". PR #451 additionally stopped the admin replace-stale
  sweep from terminating *running* instances at all (an idle-running pool
  member holds a registered runner GitHub can dispatch to at any moment) and
  partitions results by EC2 state so a stale count exceeding the number
  replaced is explainable.
- **PR #418 (`acaa47b`): reap instances whose assignment was abandoned.** An
  instance holding a runner config whose agent died without sending its
  termination message was invisible to every cleanup path — the reconciler's
  config-presence guard defers its scale-down *forever*, the claim path skips
  it forever, and the stale-secrets sweep only deletes configs whose instance
  no longer exists. Runner configs are now stamped with `created_at`, so past
  the agent's runtime ceiling (6h) plus standby deadline (2h) the assignment is
  provably abandoned and the instance is terminated. This is also why
  `withoutLiveConfig` logs instance *IDs*, not just a count.
- **`assigned_idle` is emitted every pass, including zero.** The comment at
  manager.go:743 says why: an instance stuck assigned-but-not-busy is capacity
  the pool keeps replacing, and the incident this guards against went unnoticed
  for days because the only evidence was a log line. (No alarm consumes it —
  see Gotchas.)
- **Scale on `ready`, and exclude `assignedIdle` from it.** Counted as ready,
  one previously-assigned instance would satisfy `desiredRunning` indefinitely
  and every job in the pool would silently fall back to a stopped-instance
  start.
- **PR #412 (`ffd721b`): the hot-pool branch reuses the warm-pool guard chain.**
  The idle-timeout branch previously fed *every* running instance — busy
  included — through `filterIdleInstances`, whose only signal is the
  per-replica in-memory `IdleSince` map. That map is seeded for any running
  instance a replica hasn't observed and cleared only on the *assigning*
  replica, so another replica saw assigned instances as idle and stopped them
  (23 confirmed-runner reaps in 33h of production logs). Now the idle timeout
  is applied *on top of* `stopEligibleSpares` as the stricter hot-pool decay
  criterion.
- **65s TTL > 60s reconcile interval, kept derived.** `reconcileLockTTL` is
  literally `reconcileInterval + 5*time.Second` so the two constants can't
  drift out of that ordering; the buffer also absorbs clock skew and a slightly
  overrunning pass.
- **90s dwell > 60s interval, also for an ordering reason.** An instance must
  read not-busy across *two consecutive* reconciles before it can be stopped,
  which covers the case the busy set alone cannot — an instance whose job
  status is momentarily unqueryable (GSI lag, the claiming window before
  `SaveJob` writes `launched`).
- **Per-pool DynamoDB locks instead of global leader election.** Each Fargate
  task generates a UUID `instanceID` in `NewManager`; the lock is scoped to one
  `pool_name`, so different pools reconcile concurrently across replicas while
  the same pool always serializes to one writer.
- **Owner is a per-process UUID, not the hostname**, so a crashed instance
  cannot bypass the TTL by reusing its identity on restart. The conditional
  acquire (`attribute_not_exists(reconcile_lock_expires) OR
  reconcile_lock_expires < :now OR reconcile_lock_owner = :owner`) permits
  refresh by the current holder without an intervening release.
- **`auto_tune` and `last_reconcile_*` are excluded from `SavePoolConfig`'s SET
  list; the `override_*` fields are included via `attrOrNull`.** Two writers
  (tuner, admin) share the item, and each targeted `UpdateItem` writes only its
  own attributes — that's the whole reason the tuner uses
  `UpdatePoolAutoTune` rather than a read-modify-write.
- **`UpdatePoolState` writes observation and target in one call.** The doc
  comment states the reason: comparing an observed count against a target from
  a *different* pass is what makes a converged pool look stale. This is what
  PR #451's "make the pools view mean something" turned on.
- **Warm pools are on-demand only.** `createPoolFleetInstances` always calls
  `CreateOnDemandInstance` — stop/start reliability matters more than spot
  savings for short-lived job execution. See
  [two-track-reliability](../concepts/two-track-reliability.md).
- **Price-weighted random type selection, with PR #385's correction to the
  candidate list.** Each new instance's type is picked uniformly at random from
  `RankInstanceTypesByPrice`'s inverse-price-weighted sequence, so cheaper
  types win proportionally more often. That mechanism is why burstable defaults
  were dangerous: with `t3` in `fleet.DefaultFlexibleFamilies`, `t3.medium`
  dominated warm-pool picks and an A/B benchmark measured CI 2.25x slower than
  a competitor. PR #385 dropped `t3`/`t4g` from the default family lists
  (they remain in the catalog for explicit `family=` opt-in); the pick
  mechanism is unchanged.
- **Ephemeral pools carry a flexible spec, not a pinned type (PR #376).**
  `resolvePoolInstanceTypes` resolves the spec fresh each cycle; the pinned
  `InstanceType` fallback only applies to legacy admin-created pools whose spec
  fails to resolve. This stopped ephemeral pools freezing the smallest instance
  type chosen at creation time. The companion half of #376 was a RAM floor in
  the catalog itself — `t3.micro` (1 GiB) and `t3.small` (2 GiB) are excluded
  with an explicit comment that they were "too little RAM for CI, and being the
  cheapest match they'd win price-ranking for any unconstrained cpu=2 request";
  the smallest selectable RAM is now 4 GiB.
- **In-memory locks never cross a network call.** `poolLock` (per-pool
  candidate selection + `inFlight`) and `idleMu` (`instanceIdle` + `readySince`)
  only ever protect in-memory state; the DynamoDB conditional write in
  `ClaimInstanceForJob` is the sole cross-process guard against double-claims.
  See [per-resource-locking](../concepts/per-resource-locking.md).
- **`readySince` is keyed by pool, then instance.** Reconcile walks every pool
  in a single pass, so a global map would let one pool's prune wipe another
  pool's streaks and the dwell could never be satisfied.
- **Ephemeral pools auto-scale *stopped*, not running.**
  `getEphemeralAutoScaledCount` always returns `desiredRunning=0` and sizes
  `desiredStopped` from `GetPoolP90Concurrency` over a 1-hour window; a P90
  query failure falls back to `desiredStopped=1` if recently active.
- **4-hour `ephemeralScaleDownWindow`.** If `last_job_time` is within 4 hours,
  ephemeral pools keep at least 1 stopped instance even at P90 0, preventing
  scale-to-zero between infrequent jobs. The hot-pool cap of 120 linger minutes
  is deliberately inside this window.
- **`DeletePoolConfig` is ephemeral-only** (`ConditionExpression: ephemeral = :true`),
  and **`CreateEphemeralPool` is race-safe**
  (`attribute_not_exists(pool_name)` collapses concurrent first-job creates to
  one winner; losers see `ErrPoolAlreadyExists`).
- **Demand-driven reconciliation supplements the ticker, and drops under
  pressure.** `NotifyPoolDemand` is a non-blocking send to a 64-slot channel; a
  full channel drops the notification, trading a missed fast path for
  guaranteed non-blocking behavior on the webhook ack path.
- **Best-fit selection and round-robin subnets.** Candidates sort CPU-then-RAM
  ascending (`sortByBestFit`); `selectSubnet` walks `config.SubnetIDs` via an
  atomic counter.

## Gotchas [coverage: high -- 8 sources]

- **The reconcile lock does not rate-limit reconciliation.** Because it is
  released in a `defer` rather than held for its 65s TTL, a burst of queued-job
  webhooks for one pool produces a reconcile pass per webhook (up to the
  channel's 64-slot buffer and the dedup within a single drain). Anything
  costed per pass — the 9 gauges, `DescribeInstances`, `GetPoolBusyInstanceIDs`,
  `UpdatePoolReconcileResult`, `UpdatePoolState` — scales with webhook volume,
  not with the 60s interval. This was the direct cause of the CloudWatch spend
  PR #456 addressed.
- **Reconcile once stopped BUSY instances mid-job.** PR #395 (`1de8865`,
  branch `fix/pool-reconcile-stops-busy-instances`): warm-pool scale-down
  decided "safe to stop" from a single eventually-consistent snapshot with no
  dwell, and its busy signal only counted the `launched`/`running` statuses.
  Instances actively running CI jobs were stopped with `reason=excess_ready`,
  killing jobs mid-flight and producing a requeue loop that surfaced as
  "pending" jobs on busy repos. The fix widened
  `GetPoolBusyInstanceIDs` to `launched|running|terminating` **and paginated
  the GSI query** (busy instances past the 1 MB page were being silently
  dropped), then added the 90s not-busy dwell. PR #412 extended the same guard
  chain to the hot-pool idle branch. When reading this code, treat every gate
  in `stopEligibleSpares` as load-bearing incident scar tissue.
- **`assigned_idle` has no alarm anywhere.** The gauge is published every pass
  precisely so a sustained nonzero value is *alertable*, but a repo-wide search
  finds no `aws_cloudwatch_metric_alarm`, dashboard, or alert rule consuming
  it — and as of PR #456 the CloudWatch backend is off by default, so the
  series is not even being emitted unless an operator re-enables it. A
  sustained nonzero `assigned_idle` means assignments are being abandoned
  upstream and the pool is silently replacing capacity; today nothing pages on
  it.
- **Stopping a spare mid-boot causes a systemd "destructive transaction" that
  looks like a bootstrap failure.** If the reconciler stops a spare while
  agent-bootstrap's `systemctl start` is still running, the OS shutdown
  collides with the unit start; systemd reports a destructive transaction, the
  boot shim misreads it as a bootstrap failure and self-terminates the spare —
  churning the pool. Hence the 3-minute `bootstrapGracePeriod` (comfortably
  longer than a baked-AMI boot of ~60-120s), and hence
  `scripts/agent-bootstrap.sh`'s own `system_is_stopping` check. Grace-deferred
  and dwell-deferred on-demand spares are *credited* against the
  stopped-replenish deficit so the grace period doesn't also cause duplicate
  spare creation.
- **A pool spec that resolves zero instance types silently creates nothing.**
  `createPoolFleetInstances` logs "no instance types resolved" and returns 0 for
  that cycle. Since PR #385 this can happen for a family-less spec pinned to a
  burstable-only generation (`gen=3` amd64, `gen=4` arm64) — those generations
  only contained `t3`/`t4g`, which are no longer in the default family lists.
  A pool that explicitly configures `families: [t3]` still resolves them.
- **Lock expiration is client-clock-driven.** `AcquirePoolReconcileLock`'s doc
  comment warns TTL math uses `time.Now()`; clock skew across tasks can let two
  reconcilers briefly hold the same lock. NTP sync is required.
- **`ReleasePoolReconcileLock` swallows the "not owner" case.** A
  conditional-check failure returns `nil`, so a release after TTL expiry (with
  another owner already re-acquired) is silent.
- **`ReconcileLoop` no-ops if `SetEC2Client` was never called.** Both
  `reconcile` and `reconcileDemand` bail early on `ec2Client == nil`.
- **`SetRunnerConfigChecker` is optional, and its absence silently weakens the
  guards.** Without it, `withoutLiveConfig` returns candidates unfiltered and
  the hot-spare stale-config re-check in `ClaimAndStartPoolInstance` is skipped
  entirely — the reconciler falls back to the busy set and in-memory guards
  alone.
- **A failed `StopInstances` marks its instances handled but not banked.** On
  error the reconciler sets `stoppedNow = canStop` so the terminate path skips
  them (they may be transitioning after a claim), but does **not** increment
  `stoppedCount` — so they are not credited to the stopped reserve.
  `IncorrectInstanceState` is logged at Info as a benign race, not Error.
- **Batch-start failures fall through to fleet creation.** `startInstances`
  errors are logged but the deficit is not decremented, so
  `createPoolFleetInstances` runs for the remainder. Repeated stop/start
  failures can churn EC2 spend.
- **One-time spot instances cannot be stopped.** Cold-start overflow that ends
  up pool-tagged is `Spot: true`; `StopInstances` rejects spot, so they always
  route to terminate instead of joining the stopped reserve. They are also
  never credited to `withinGrace` and never offered as hot spares.
- **Instance must be in `fleet.InstanceCatalog` for spec matching.**
  `matchesFlexibleSpec` returns false when `CPU == 0` (catalog miss) and a spec
  is provided. Catalog misses log a warning but still appear in counts.
- **Idle tracking is in-memory only.** A Fargate task restart loses
  `instanceIdle` and `readySince`; a fresh sighting reseeds `IdleSince` to
  `time.Now()`, effectively resetting the idle-timeout clock. This is the
  weakness the durable `runs-fleet:assigned` tag was introduced to cover.
- **Ephemeral pool spec is frozen at first job.** `arch`, `cpu_min/max`,
  `ram_min/max`, `families` come from the job that created the pool; later jobs
  claiming the same pool name still match against that frozen spec.
- **`UpdatePoolState` requires the pool to exist** (`attribute_exists(pool_name)`),
  so it fails on pools deleted mid-reconcile; the error is logged (unless it is
  `ErrPoolNotFound` or a shutdown error) and reconciliation continues.
- **`effective_desired_*` being nil is not the same as zero.** A pool that has
  never been reconciled has no attribute at all; the admin pools view must
  distinguish "not yet reconciled" from "target is 0".
- **A newly enabled hot-pool fleet stays cold for up to one tuner tick.**
  Recommendations are only populated by `ExecutePoolHotTuner`'s hourly pass, so
  after flipping the toggle every pool reads `linger = 0` until the first pass
  lands. The direction (cold → warm) is the safe one.
- **The tuner rewrites *every* pool's recommendation every hour**, including
  cold ones, so a pool that briefly meets `MinJobsToActivate` and then goes
  quiet decays back to `insufficient-history` on the next pass rather than
  keeping a stale warm recommendation.
- **A new sentinel prefix in the pools table is a phantom pool waiting to
  happen.** Any row kind added to this table that isn't registered in
  `IsReservedPoolKey` gets reconciled as a real pool and inflates per-pool
  metric cardinality with one zero-valued series per ephemeral instance ID.
  This has happened twice (task locks, then runner offline sightings — PRs #438
  and #436-adjacent); the four prefixes now guarded are the fix, not a
  precaution.
- **Instance claims accumulate without the reaper.** `ClaimInstanceForJob` only
  overwrites the row on the *next* claim of the same instance, so an ephemeral
  fleet leaks one dead claim row per instance forever. Left unreaped, the
  backlog bloated the pools table past the 1 MB `Scan` page `ListPools` reads
  and hid real pools (PR #402). `DeleteExpiredInstanceClaims` sweeps them with
  a *conditional* delete so a renewed claim is never dropped.
- **`filterNotInFlight` only guards claims from *this* process.** It is an
  in-memory set, so a claim in flight on another replica is invisible to it —
  the DynamoDB claim and `HasLiveInstanceClaim` are the cross-process guards.

## Sources [coverage: high]

- [pkg/pools/manager.go](../../pkg/pools/manager.go)
- [pkg/housekeeping/hot_tuner.go](../../pkg/housekeeping/hot_tuner.go)
- [pkg/housekeeping/tasks.go](../../pkg/housekeeping/tasks.go)
- [pkg/config/hot_pools.go](../../pkg/config/hot_pools.go)
- [pkg/db/pool_config.go](../../pkg/db/pool_config.go)
- [pkg/db/instance_claims.go](../../pkg/db/instance_claims.go)
- [pkg/db/locks.go](../../pkg/db/locks.go)
- [cmd/server/main.go](../../cmd/server/main.go)
