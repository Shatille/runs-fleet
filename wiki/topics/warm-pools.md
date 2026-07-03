---
topic: Warm Pools (Reconciliation)
last_compiled: 2026-07-03
sources_count: 3
---

# Warm Pools (Reconciliation)

## Purpose [coverage: medium -- 3 sources]

Warm pools pre-provision EC2 instances so workflow jobs start in seconds
rather than waiting on a cold EC2 boot. A single reconciler continuously
drives each pool toward a desired ready/stopped state, supporting hot pools
(instances always running), stopped pools (batch-started on demand), and
ephemeral pools that auto-create from the first matching job and self-clean
after idle. Warm pools are EC2-only: the K8s pool manager that once mirrored
this behavior for Helm-deployed placeholder pods was removed upstream (see
Key Decisions).

## Architecture [coverage: medium -- 3 sources]

The reconciler is implemented in `pkg/pools/manager.go` as `Manager`. Its
entry point `ReconcileLoop(ctx)` ticks every 60 seconds and calls
`reconcile(ctx)`, which lists all pools via `DBClient.ListPools` and
dispatches to `reconcilePool(ctx, poolName)` per pool. Between ticks,
`NotifyPoolDemand(poolName)` lets webhook handlers push a pool name onto a
buffered `demandCh` so a newly arrived job can trigger an immediate
reconciliation of just that pool instead of waiting up to 60s; `ReconcileLoop`
drains and deduplicates the channel via `reconcileDemand`.

`reconcilePool` flow:
1. Acquire a per-pool DynamoDB lock with a 65s TTL
   (`AcquirePoolReconcileLock`); skip silently on `ErrPoolReconcileLockHeld`
   or `ErrPoolNotFound`. Release is deferred via `ReleasePoolReconcileLock`.
2. Load `PoolConfig` and resolve `desiredRunning`, `desiredStopped` via
   `getScheduledDesiredCounts` (time-of-day / day-of-week schedules) or
   `getEphemeralAutoScaledCount` (P90 concurrency over the past hour for
   ephemeral pools).
3. Describe EC2 instances tagged `runs-fleet:pool=<name>` and
   `runs-fleet:managed=true` via `getPoolInstances`.
4. Compute `running`, `stopped`, `busy`, `ready` via `countInstanceStates`,
   intersecting busy job records with actual running instances so stale jobs
   from terminated instances cannot inflate `busy` and block scale-down.
5. Scale on the **ready** count (idle running instances), not total running:
   - `ready < desiredRunning`: `startInstances` from the stopped pool first,
     then `createPoolFleetInstances` (on-demand) for any remaining deficit.
   - `ready > desiredRunning`: pick candidates via `filterReadyInstances`
     (warm pools, immediate stop, but deferring any spare still inside
     `bootstrapGracePeriod`) or `filterIdleInstances` (hot pools, must exceed
     `IdleTimeoutMinutes`), then `stopInstances` up to `desiredStopped`
     headroom, with overflow going to `terminateInstances`. One-time spot
     instances (cold-start overflow that picked up pool tags) cannot be
     stopped, so they always fall through to the terminate path.
6. If `stopped > desiredStopped`: terminate excess stopped instances
   (excluding any instance a local claim has in flight, via
   `poolLock.filterNotInFlight`). If `stopped < desiredStopped` and
   `desiredRunning == 0` (warm/ephemeral), create on-demand instances to top
   up the stopped pool, crediting instances stopped this same pass and
   still-booting spares so the deficit calculation doesn't over-provision.
7. Persist counts via `UpdatePoolState` and publish pool gauges/actions to
   `MetricsAPI`.

Idle tracking is in-memory: `Manager.instanceIdle` is a
`map[string]time.Time` guarded by `idleMu`, seeded the first time
`getPoolInstances` observes a running instance, and mutated by
`MarkInstanceBusy` / `MarkInstanceIdle`. `stopInstances` and
`terminateInstances` also delete entries so the map never grows unbounded.
There is no `forgetInstances` helper as such — cleanup is inlined at each
call site that removes an instance from the pool.

Job assignment uses `ClaimAndStartPoolInstance(ctx, poolName, jobID, repo,
spec)`: it filters stopped instances by `FlexibleSpec`, sorts best-fit
(CPU ascending, then RAM ascending), reserves a candidate locally under a
per-pool `poolLock` (never held across a network call), atomically claims it
via `ClaimInstanceForJob` (5-minute claim TTL, `instanceClaimTTL`), then
calls `StartInstanceForJob`, which starts the instance and tags it
`Role=<repo>` for cost allocation. On start failure the claim is released via
`ReleaseInstanceClaim`. `StartInstanceForJob` and `StopPoolInstance` are also
exposed directly for callers that already hold an instance ID (e.g. rollback
paths); `ClaimAndStartPoolInstance` is preferred wherever an instance still
needs to be selected, since it is the only path with the DynamoDB conditional
write guarding against double-claims across orchestrator processes.

## Talks To [coverage: medium -- 3 sources]

- `pkg/db` — `DBClient` interface (in `manager.go`) covers `GetPoolConfig`,
  `UpdatePoolState`, `ListPools`, `GetPoolP90Concurrency`,
  `GetPoolBusyInstanceIDs`, `AcquirePoolReconcileLock`,
  `ReleasePoolReconcileLock`, `ClaimInstanceForJob`, `ReleaseInstanceClaim`.
  Implementations live in [pkg/db/pool_config.go](../../pkg/db/pool_config.go)
  (table CRUD on `runs-fleet-pools`) and [pkg/db/locks.go](../../pkg/db/locks.go)
  (reconcile-lock and instance-claim primitives).
- `pkg/fleet` — `FleetAPI` exposes `CreateFleet`, `CreateOnDemandInstance`,
  `RankInstanceTypesByPrice`. Pools always call `CreateOnDemandInstance` for
  reliability. `fleet.FlexibleSpec`, `fleet.InstanceSpec`,
  `fleet.GetInstanceSpec`, and `fleet.LaunchSpec` provide spec matching and
  launch parameters.
- `pkg/github` — `github.JobConfig` and `github.ResolveFlexibleSpec` are used
  by `resolvePoolInstanceTypes` to translate pool-level CPU/RAM/family
  requirements into a list of concrete EC2 instance types.
- `pkg/config` — `config.Config` supplies `SubnetIDs`, round-robined by
  `selectSubnet` for new pool instances.
- `pkg/metrics` — optional `MetricsAPI` publishes pool action counts, desired
  vs. actual gauges, and lock-wait histograms; a nil metrics client is a
  no-op throughout.
- AWS EC2 API — `EC2API` interface needs `DescribeInstances`,
  `StartInstances`, `StopInstances`, `TerminateInstances`, `CreateTags`.
- Pool queue / SQS (`pkg/queue`) — webhooks with `pool=` labels feed the pool
  queue; pool processors call `ClaimAndStartPoolInstance` when a stopped
  instance can satisfy the job, and can call `NotifyPoolDemand` to skip the
  60s ticker. (Producer/consumer wiring lives outside this package but
  consumes its API.)

## API Surface [coverage: medium -- 3 sources]

```go
// pkg/pools/manager.go
type DBClient interface { /* see "Talks To" */ }
type FleetAPI interface { /* see "Talks To" */ }
type MetricsAPI interface {
    PublishPoolAction(ctx, pool, action, reason string) error
    PublishPoolDesired(ctx, pool, kind string, n int) error
    PublishPoolInstances(ctx, pool, state string, n int) error
    PublishPoolReconcileSeconds(ctx, seconds float64) error
    PublishLockWaitSeconds(ctx, lock string, seconds float64) error
    PublishInstances(ctx, state, capacity, pool string, n int) error
}
type EC2API interface { /* see "Talks To" */ }

type PoolInstance struct {
    InstanceID, State, InstanceType string
    LaunchTime, IdleSince           time.Time
    Spot                            bool
    CPU                             int
    RAM                             float64
    Arch, Family                    string
    Gen                             int
}

type AvailableInstance struct {
    InstanceID, InstanceType, State string
}

var ErrNoAvailableInstance = errors.New("no available instance in pool")
var ErrClaimContextDone    = errors.New("claim aborted: context already done")

type Manager struct { /* unexported */ }
func NewManager(dbClient DBClient, fleetManager FleetAPI, cfg *config.Config) *Manager
func (m *Manager) SetEC2Client(ec2Client EC2API)
func (m *Manager) SetMetrics(metrics MetricsAPI)
func (m *Manager) ReconcileLoop(ctx context.Context)
func (m *Manager) NotifyPoolDemand(poolName string)
func (m *Manager) MarkInstanceBusy(instanceID string)
func (m *Manager) MarkInstanceIdle(instanceID string)
func (m *Manager) GetAvailableInstance(ctx, poolName) (*AvailableInstance, error)
func (m *Manager) StartInstanceForJob(ctx, instanceID, repo string) error
func (m *Manager) ClaimAndStartPoolInstance(ctx, poolName, jobID int64, repo string, spec *fleet.FlexibleSpec) (*AvailableInstance, error)
func (m *Manager) StopPoolInstance(ctx, instanceID string) error
```

```go
// pkg/db/pool_config.go
type PoolSchedule struct { /* see Data section */ }
type PoolConfig   struct { /* see Data section */ }
var ErrPoolAlreadyExists = errors.New("pool already exists")

func IsReservedPoolKey(poolName string) bool
func (c *Client) GetPoolConfig(ctx, poolName) (*PoolConfig, error)
func (c *Client) ListPools(ctx) ([]string, error)
func (c *Client) UpdatePoolState(ctx, poolName string, running, stopped int) error
func (c *Client) SavePoolConfig(ctx, *PoolConfig) error
func (c *Client) CreateEphemeralPool(ctx, *PoolConfig) error
func (c *Client) TouchPoolActivity(ctx, poolName) error
func (c *Client) DeletePoolConfig(ctx, poolName) error  // ephemeral-only

// pkg/db/locks.go
var ErrPoolReconcileLockHeld = errors.New("pool reconciliation lock held by another instance")
var ErrPoolNotFound          = errors.New("pool not found")
var ErrTaskLockHeld          = errors.New("task lock held by another instance")

func (c *Client) AcquirePoolReconcileLock(ctx, poolName, owner string, ttl time.Duration) error
func (c *Client) ReleasePoolReconcileLock(ctx, poolName, owner string) error
func (c *Client) AcquireTaskLock(ctx, taskType, owner string, ttl time.Duration) error
func (c *Client) ReleaseTaskLock(ctx, taskType, owner string) error
```

## Data [coverage: medium -- 3 sources]

DynamoDB table: `runs-fleet-pools`. Partition key: `pool_name` (string).

`PoolConfig` attributes (from
[pkg/db/pool_config.go](../../pkg/db/pool_config.go)):

| Attribute | Type | Notes |
|---|---|---|
| `pool_name` | S | Partition key |
| `instance_type` | S | Legacy single instance type |
| `desired_running` | N | Target ready (idle) instances |
| `desired_stopped` | N | Target stopped instances |
| `current_running` | N | Updated by `UpdatePoolState` |
| `current_stopped` | N | Updated by `UpdatePoolState` |
| `idle_timeout_minutes` | N | Hot-pool idle threshold (default 10) |
| `schedules` | L | List of `PoolSchedule` entries |
| `ephemeral` | BOOL | Auto-create / auto-delete pool |
| `last_job_time` | S (RFC3339) | Updated by `TouchPoolActivity` |
| `arch` | S | `arm64` / `amd64` |
| `cpu_min`, `cpu_max` | N | vCPU range |
| `ram_min`, `ram_max` | N | RAM range (GB) |
| `families` | L | e.g. `["c7g","m7g"]` |
| `multi_spec` | BOOL | Demand-driven mixed-spec pool (TODO, unused by reconciler) |
| `reconcile_lock_owner` | S | Set by `AcquirePoolReconcileLock` |
| `reconcile_lock_expires` | N | Unix epoch seconds |

`PoolSchedule` attributes: `name`, `start_hour` (0-23), `end_hour` (0-23),
`days_of_week` (`[]int`, 0=Sunday), `desired_running`, `desired_stopped`.

Task locks and instance claims reuse the same table with sentinel key
prefixes (`__task_lock:` for task locks; a separate prefix for instance
claims) plus `lock_owner` / `lock_expires` attributes.
`IsReservedPoolKey` identifies both prefixes so `ListPools` and
`GetPoolConfig` never surface them as phantom pools.

## Key Decisions [coverage: medium -- 3 sources]

- **2026-06: K8s pool manager removed.** `pkg/pools/k8s_manager.go` was
  deleted along with the K8s runner backend; `pkg/pools/manager.go` became
  EC2-only. Warm pools no longer have a Kubernetes analog — there is one
  reconciler, one lock model, and one instance-lifecycle path (EC2
  start/stop/terminate).
- **Per-pool DynamoDB locks instead of global leader election.** Each
  Fargate task generates a UUID `instanceID` in `NewManager` and acquires a
  lock scoped to one `pool_name`. This lets multiple Fargate tasks
  reconcile different pools concurrently while the same pool always
  serializes to one writer, with no cluster-wide leader to elect or fail
  over.
- **65s TTL > 60s reconcile interval.** `reconcilePool` passes
  `65*time.Second` so a healthy lock holder always renews before expiry,
  while a crashed holder's lock auto-expires before the next tick.
- **Owner check uses a unique UUID per process, not hostname.**
  `AcquirePoolReconcileLock`'s doc comment emphasizes this so a crashed
  instance cannot bypass TTL by reusing its identity on restart.
- **Conditional acquire allows reentry by the same owner.** The condition
  `attribute_not_exists(reconcile_lock_expires) OR reconcile_lock_expires <
  :now OR reconcile_lock_owner = :owner` permits TTL refresh by the current
  holder without an intervening release.
- **Warm pools are on-demand only.** `createPoolFleetInstances` always calls
  `CreateOnDemandInstance`; the comment on it states stop/start reliability
  matters more than spot savings for short-lived job execution.
- **Bootstrap grace period before stopping a spare.** `reconcilePool` will
  not stop a warm-pool spare within `bootstrapGracePeriod`
  (`defaultBootstrapGracePeriod`, 3 minutes) of its `LaunchTime`. See
  Gotchas for why.
- **Scale on `ready`, not `running`.** Busy instances are excluded so a
  flood of jobs cannot trick the reconciler into thinking the pool is full.
- **Busy intersection prevents stale-job inflation.** `countInstanceStates`
  intersects `busyIDs` with current running instance IDs; comments call out
  that this prevents "runaway scaling" from orphaned job records.
- **Demand-driven reconciliation supplements, not replaces, the ticker.**
  `NotifyPoolDemand` is a non-blocking send to a bounded channel; a full
  channel drops the notification and relies on the next 60s tick, trading a
  missed fast-path for guaranteed non-blocking behavior on the webhook path.
- **In-memory locks never cross a network call.** `poolLock` (guarding
  per-pool candidate selection) and `idleMu` (guarding `instanceIdle`) both
  only ever protect in-memory state; the DynamoDB conditional write in
  `ClaimInstanceForJob` remains the sole cross-process guard against
  double-claims.
- **Ephemeral pools auto-scale stopped, not running.**
  `getEphemeralAutoScaledCount` always returns `desiredRunning=0` and sizes
  `desiredStopped` from `GetPoolP90Concurrency` over a 1-hour window.
- **4-hour `ephemeralScaleDownWindow`.** If `last_job_time` is within 4
  hours, ephemeral pools keep at least 1 stopped instance even when P90 is
  0, preventing scale-to-zero between infrequent jobs.
- **`DeletePoolConfig` is ephemeral-only.** The conditional expression
  `ephemeral = :true` blocks accidental deletion of persistent pools.
- **`CreateEphemeralPool` is race-safe.** `attribute_not_exists(pool_name)`
  makes concurrent first-job creates collapse to one winner; losers see
  `ErrPoolAlreadyExists`.
- **Best-fit instance selection.** `ClaimAndStartPoolInstance` sorts
  candidates by CPU ascending, then RAM ascending, picking the smallest
  instance that satisfies the spec.
- **Round-robin subnet selection.** `selectSubnet` returns from
  `config.SubnetIDs` round-robin via an atomic counter.

## Gotchas [coverage: medium -- 3 sources]

- **Stopping a spare mid-boot causes a systemd "destructive transaction"
  that looks like a bootstrap failure.** If the reconciler stops a
  warm-pool spare while agent-bootstrap's `systemctl start` is still
  running, the OS shutdown collides with that unit start; systemd reports a
  destructive transaction, the boot shim misreads it as a bootstrap
  failure, and self-terminates the spare — churning the pool. This is why
  `reconcilePool` defers any spare within `bootstrapGracePeriod` (3
  minutes, comfortably longer than a baked-AMI boot of ~60-120s) from the
  stop-candidate list, and separately credits those still-booting spares
  against the stopped-replenish deficit so the grace period doesn't also
  trigger duplicate spare creation.
- **Lock expiration is client-clock-driven.** `AcquirePoolReconcileLock`'s
  doc comment warns that TTL math uses `time.Now()`; clock skew across
  tasks can let two reconcilers briefly hold the same lock. NTP sync is
  required.
- **`ReleasePoolReconcileLock` swallows the "not owner" case.** A
  conditional-check failure returns `nil`, not an error, so a release after
  TTL expiry (with another owner already re-acquired) is silent.
- **`ReconcileLoop` no-ops if `SetEC2Client` was not called.** Both
  `reconcile` and `reconcileDemand` bail early when `ec2Client == nil`.
- **Instance must be in `fleet.InstanceCatalog` for spec matching.**
  `matchesFlexibleSpec` returns false when `CPU == 0` (catalog miss) and a
  spec is provided. Catalog misses log a warning but still appear in
  counts.
- **Idle tracking is in-memory only.** A Fargate task restart loses
  `instanceIdle`; a fresh instance's `IdleSince` reseeds to `time.Now()` on
  next sight, effectively resetting the idle timeout clock.
- **Hot vs. warm pool scale-down differ.** When `desiredRunning > 0`, only
  instances older than `IdleTimeoutMinutes` (default 10) are candidates for
  stop; when `desiredRunning == 0 && desiredStopped > 0`, every non-busy
  running instance past the bootstrap grace is fair game for immediate
  stop.
- **One-time spot instances cannot be stopped.** Cold-start overflow
  instances that end up pool-tagged are `Spot: true`; `StopInstances`
  rejects spot instances, so they always route to the terminate path
  instead of joining the stopped reserve.
- **Batch-start failures fall through to fleet creation.** `startInstances`
  errors are logged but the deficit is not decremented, so
  `createPoolFleetInstances` is invoked for the remainder. Repeated
  stop/start failures can churn EC2 spend.
- **Ephemeral pool spec is fixed at first job.** Pool spec (`arch`,
  `cpu_min/max`, `ram_min/max`, `families`) comes from the first job that
  creates the pool; later jobs claiming the same pool name still match
  against that frozen spec.
- **`UpdatePoolState` requires the pool to exist.** It uses
  `attribute_exists(pool_name)`, so it fails on pools deleted mid-reconcile;
  the error is logged but reconciliation otherwise continues.
- **`filterNotInFlight` guards against terminating a claim in progress.**
  When trimming excess stopped instances, the reconciler excludes any
  instance a local `ClaimAndStartPoolInstance` call has reserved but not
  yet resolved — EC2 may still report it as stopped even though
  `StartInstances` is about to be (or has just been) called for a job.

## Sources [coverage: medium]

- [pkg/pools/manager.go](../../pkg/pools/manager.go)
- [pkg/db/pool_config.go](../../pkg/db/pool_config.go)
- [pkg/db/locks.go](../../pkg/db/locks.go)
