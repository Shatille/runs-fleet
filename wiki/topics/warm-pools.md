---
topic: Warm Pools (Reconciliation)
last_compiled: 2026-04-30
sources_count: 4
---

# Warm Pools (Reconciliation)

## Purpose [coverage: high -- 4 sources]

Warm pools pre-provision EC2 instances (or K8s placeholder pods) so workflow jobs
start in seconds rather than waiting on a cold EC2 boot. The reconciler
continuously drives each pool toward a desired ready/stopped state, supporting
hot pools (instances always running), stopped pools (batch-started on demand),
and ephemeral pools that auto-create from the first matching job and self-clean
after idle. A second `K8sManager` mirrors the same idea by scaling
Helm-deployed placeholder Deployments per architecture.

## Architecture [coverage: high -- 4 sources]

The EC2 reconciler is implemented in `pkg/pools/manager.go` as `Manager`. Its
entry point `ReconcileLoop(ctx)` ticks every 60 seconds and calls
`reconcile(ctx)`, which lists all pools via `DBClient.ListPools` and dispatches
to `reconcilePool(ctx, poolName)` per pool.

`reconcilePool` flow:
1. Acquire per-pool DynamoDB lock with 65s TTL
   (`AcquirePoolReconcileLock`); skip silently on `ErrPoolReconcileLockHeld`
   or `ErrPoolNotFound`. Release deferred via `ReleasePoolReconcileLock`.
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
   - `ready < desiredRunning`: `startInstances` from stopped pool first, then
     `createPoolFleetInstances` (on-demand) for any remaining deficit.
   - `ready > desiredRunning`: pick candidates via `filterReadyInstances`
     (warm pools, immediate stop) or `filterIdleInstances`
     (hot pools, must exceed `IdleTimeoutMinutes`), then `stopInstances` up
     to `desiredStopped` headroom, with overflow going to
     `terminateInstances`.
6. If `stopped > desiredStopped`: terminate excess stopped instances. If
   `stopped < desiredStopped` and `desiredRunning == 0` (warm/ephemeral),
   create on-demand instances to top up the stopped pool.
7. Persist counts via `UpdatePoolState`.

Idle tracking is in-memory: `Manager.instanceIdle` is a `map[string]time.Time`
mutated by `MarkInstanceBusy` / `MarkInstanceIdle` and seeded on first sight of
a running instance.

Job assignment uses `ClaimAndStartPoolInstance(ctx, poolName, jobID, repo,
spec)`: it filters stopped instances by `FlexibleSpec`, sorts best-fit
(CPU asc, then RAM asc), atomically claims via `ClaimInstanceForJob`
(5-minute claim TTL via `instanceClaimTTL`), then calls
`startInstanceForJobLocked` which starts the instance and tags it with
`Role=<repo>` for cost allocation. On start failure the claim is released via
`ReleaseInstanceClaim`.

The K8s reconciler in `pkg/pools/k8s_manager.go` (`K8sManager`) only handles
the singleton `DefaultPoolName = "default"`. `reconcile` wraps each pass in a
30s `reconcileTimeout` context, then `scaleDeployment` issues an
`Update` against the `runs-fleet-placeholder-arm64` /
`runs-fleet-placeholder-amd64` Deployments produced by Helm.
`scaleOrGetCurrent` falls back to `getCurrentReplicas` if scaling errors so
metrics remain truthful. Admin entry points: `ScalePool`, `SetSchedule`,
`CreatePool`, `DeletePool`, `GetPlaceholderStatus`.

## Talks To [coverage: high -- 4 sources]

- `pkg/db` — `DBClient` interface (in `manager.go`) covers `GetPoolConfig`,
  `UpdatePoolState`, `ListPools`, `GetPoolP90Concurrency`,
  `GetPoolBusyInstanceIDs`, `AcquirePoolReconcileLock`,
  `ReleasePoolReconcileLock`, `ClaimInstanceForJob`,
  `ReleaseInstanceClaim`. Implementations live in `pkg/db/pool_config.go`
  (table CRUD on `runs-fleet-pools`) and `pkg/db/locks.go` (lock and claim
  primitives).
- `pkg/fleet` — `FleetAPI` exposes `CreateFleet`, `CreateOnDemandInstance`,
  `RankInstanceTypesByPrice`. Pools always call `CreateOnDemandInstance` for
  reliability. `fleet.FlexibleSpec`, `fleet.InstanceSpec`,
  `fleet.GetInstanceSpec`, and `fleet.LaunchSpec` provide spec matching and
  launch parameters.
- `pkg/github` — `github.JobConfig` and `github.ResolveFlexibleSpec` are used
  by `resolvePoolInstanceTypes` to translate pool-level CPU/RAM/family
  requirements into a list of concrete EC2 instance types.
- `pkg/state` — K8s side uses `state.K8sPoolConfig` and `state.K8sPoolSchedule`
  via `K8sStateStore`.
- AWS EC2 API — `EC2API` interface needs `DescribeInstances`,
  `StartInstances`, `StopInstances`, `TerminateInstances`, `CreateTags`.
- Kubernetes API — `kubernetes.Interface` for `AppsV1().Deployments(ns).Get`
  and `Update`.
- Pool queue / SQS (`pkg/queue`) — webhooks with `pool=` labels feed the pool
  queue; pool processors call `ClaimAndStartPoolInstance` when a stopped
  instance can satisfy the job. (Producer/consumer wiring lives outside these
  files but consumes this package's API.)

## API Surface [coverage: high -- 4 sources]

Exported types and functions across the four sources:

```go
// pkg/pools/manager.go
type DBClient interface { /* see "Talks To" */ }
type FleetAPI interface { /* see "Talks To" */ }
type EC2API interface { /* see "Talks To" */ }

type PoolInstance struct {
    InstanceID, State, InstanceType string
    LaunchTime, IdleSince           time.Time
    CPU                             int
    RAM                             float64
    Arch, Family                    string
    Gen                             int
}

type AvailableInstance struct {
    InstanceID, InstanceType, State string
}

var ErrNoAvailableInstance = errors.New("no available instance in pool")

type Manager struct { /* unexported */ }
func NewManager(dbClient DBClient, fleetManager FleetAPI, cfg *config.Config) *Manager
func (m *Manager) SetEC2Client(ec2Client EC2API)
func (m *Manager) ReconcileLoop(ctx context.Context)
func (m *Manager) MarkInstanceBusy(instanceID string)
func (m *Manager) MarkInstanceIdle(instanceID string)
func (m *Manager) GetAvailableInstance(ctx, poolName) (*AvailableInstance, error)
func (m *Manager) StartInstanceForJob(ctx, instanceID, repo string) error
func (m *Manager) ClaimAndStartPoolInstance(ctx, poolName, jobID, repo, *fleet.FlexibleSpec) (*AvailableInstance, error)
func (m *Manager) StopPoolInstance(ctx, instanceID string) error
```

```go
// pkg/pools/k8s_manager.go
const DefaultPoolName    = "default"
const MaxReplicasPerArch = 100

type K8sStateStore interface { /* GetK8sPoolConfig, SaveK8sPoolConfig,
    DeleteK8sPoolConfig, ListK8sPools, UpdateK8sPoolState */ }

type K8sManager struct { /* unexported */ }
func NewK8sManager(clientset, stateStore, *config.Config) *K8sManager
func (m *K8sManager) ReconcileLoop(ctx context.Context)
func (m *K8sManager) GetPlaceholderStatus(ctx) (arm64, amd64 *appsv1.Deployment, error)
func (m *K8sManager) ScalePool(ctx, poolName, arm64Replicas, amd64Replicas int) error
func (m *K8sManager) SetSchedule(ctx, poolName, []state.K8sPoolSchedule) error
func (m *K8sManager) CreatePool(ctx, poolName, arm64Replicas, amd64Replicas int) error
func (m *K8sManager) DeletePool(ctx, poolName) error
```

```go
// pkg/db/pool_config.go
type PoolSchedule struct { /* see Data section */ }
type PoolConfig   struct { /* see Data section */ }
var ErrPoolAlreadyExists = errors.New("pool already exists")

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

## Data [coverage: high -- 4 sources]

DynamoDB table: `runs-fleet-pools`. Partition key: `pool_name` (string).

`PoolConfig` attributes (from `pkg/db/pool_config.go`):

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
| `environment` | S | `dev`, `staging`, `prod` |
| `region` | S | Multi-region routing (reserved word, aliased) |
| `ephemeral` | BOOL | Auto-create / auto-delete pool |
| `last_job_time` | S (RFC3339) | Updated by `TouchPoolActivity` |
| `arch` | S | `arm64` / `amd64` |
| `cpu_min`, `cpu_max` | N | vCPU range |
| `ram_min`, `ram_max` | N | RAM range (GB) |
| `families` | L | e.g. `["c7g","m7g"]` |
| `multi_spec` | BOOL | Demand-driven mixed-spec pool (TODO) |
| `reconcile_lock_owner` | S | Set by `AcquirePoolReconcileLock` |
| `reconcile_lock_expires` | N | Unix epoch seconds |

`PoolSchedule` attributes: `name`, `start_hour` (0-23), `end_hour` (0-23),
`days_of_week` ([]int with 0=Sunday), `desired_running`, `desired_stopped`.

Task locks reuse the same table with key prefix `__task_lock:` and attributes
`lock_owner` / `lock_expires`. `ListPools` filters out any `pool_name` starting
with that prefix.

## Key Decisions [coverage: high -- 4 sources]

- **Per-pool DynamoDB locks instead of global leader election.** Each Fargate
  task generates a UUID `instanceID` in `NewManager` and acquires a lock
  scoped to one `pool_name`. Different pools reconcile in parallel across
  tasks; the same pool serializes.
- **65s TTL > 60s reconcile interval.** `reconcilePool` passes
  `65*time.Second` so a healthy lock holder always renews before expiry,
  while a crashed holder's lock auto-expires before the next tick.
- **Owner check uses unique UUID per process, not hostname.** The doc comment
  on `AcquirePoolReconcileLock` emphasises this so a crashed instance cannot
  bypass TTL by reusing its identity on restart.
- **Conditional acquire allows reentry by the same owner.** The condition
  `attribute_not_exists(reconcile_lock_expires) OR reconcile_lock_expires < :now
  OR reconcile_lock_owner = :owner` permits TTL refresh by the current holder.
- **Warm pools are on-demand only.** `createPoolFleetInstances` always calls
  `CreateOnDemandInstance`; the comment on it states "stop/start reliability
  matters more than spot savings".
- **Scale on `ready` not `running`.** Busy instances are excluded so a flood
  of jobs cannot trick the reconciler into thinking the pool is full.
- **Busy intersection prevents stale-job inflation.** `countInstanceStates`
  intersects `busyIDs` with current running instance IDs; comments call out
  this prevents "runaway scaling" from orphaned job records.
- **K8s skips locks.** `K8sManager` uses `DefaultPoolName` only and the
  comment notes "K8s scaling operations are idempotent, so concurrent
  reconciliation is safe."
- **Ephemeral pools auto-scale stopped, not running.** `getEphemeralAutoScaledCount`
  always returns `desiredRunning=0` and sizes `desiredStopped` from
  `GetPoolP90Concurrency` over a 1-hour window.
- **4-hour `ephemeralScaleDownWindow`.** If `last_job_time` is within 4
  hours, ephemerals keep at least 1 stopped instance even when P90 is 0,
  preventing scale-to-zero between infrequent jobs.
- **`DeletePoolConfig` is ephemeral-only.** Conditional expression
  `ephemeral = :true` blocks accidental deletion of persistent pools.
- **`CreateEphemeralPool` is race-safe.** `attribute_not_exists(pool_name)`
  condition makes concurrent first-job creates collapse to one winner;
  losers see `ErrPoolAlreadyExists`.
- **Best-fit instance selection.** `ClaimAndStartPoolInstance` sorts
  candidates by CPU asc, then RAM asc, picking the smallest instance that
  satisfies the spec.
- **Subnet selection prefers private.** `selectSubnet` returns from
  `PrivateSubnetIDs` when available, round-robin via an atomic counter.

## Gotchas [coverage: high -- 4 sources]

- **Lock expiration is client-clock-driven.** `AcquirePoolReconcileLock`'s doc
  warns that TTL math uses `time.Now()`; clock skew across tasks can let
  two reconcilers briefly hold the same lock. NTP sync is required.
- **`ReleasePoolReconcileLock` swallows the "not owner" case.** A
  conditional-check failure returns `nil`, not an error, so a release after
  TTL expiry (and another owner re-acquired) is silent.
- **`ReconcileLoop` no-ops if `SetEC2Client` was not called.** `reconcile`
  bails early when `ec2Client == nil`. The K8s side has no analog — it
  scales whatever it can reach.
- **Instance must be in `fleet.InstanceCatalog` for spec matching.**
  `matchesFlexibleSpec` returns false when `CPU == 0` (catalog miss) and a
  spec is provided. Catalog misses log a warning but still appear in counts.
- **Idle tracking is in-memory only.** A Fargate task restart loses
  `instanceIdle`; a fresh instance's `IdleSince` reseeds to `time.Now()`,
  effectively resetting the idle timeout.
- **Hot vs warm pool scale-down differ.** When `desiredRunning > 0`, only
  instances older than `IdleTimeoutMinutes` (default 10) are candidates; when
  `desiredRunning == 0 && desiredStopped > 0`, every non-busy running instance
  is fair game for immediate stop.
- **Batch-start failures fall through to fleet creation.** `startInstances`
  errors are logged but `deficit` is not decremented, so
  `createPoolFleetInstances` is invoked for the remainder. Repeated stop/start
  failures can churn EC2 spend.
- **Ephemeral pool inheritance is fixed at first job.** Pool spec
  (`arch`, `cpu_min/max`, `ram_min/max`, `families`) comes from the first
  job that creates the pool; later jobs claiming the same pool name still
  match against that frozen spec.
- **`UpdatePoolState` requires the pool to exist.** It uses
  `attribute_exists(pool_name)`, so it fails on pools deleted mid-reconcile;
  the error is logged but reconciliation otherwise continues.
- **`reconcileTimeout` (K8s) is 30s.** A wedged K8s API will fail the entire
  pass; recovery is the next 60s tick.
- **`MaxReplicasPerArch = 100`.** K8s `validateReplicaCounts` and
  `validateSchedules` cap each arch at 100 replicas to protect cluster
  capacity.
- **Schedule `start_hour == end_hour` is illegal on K8s.** It would never
  match, so `validateSchedules` rejects it. EC2 schedule validation is more
  lenient and inferred from the comparison `start_hour <= end_hour`.

## Sources [coverage: high]

- [pkg/pools/manager.go](../../pkg/pools/manager.go)
- [pkg/pools/k8s_manager.go](../../pkg/pools/k8s_manager.go)
- [pkg/db/pool_config.go](../../pkg/db/pool_config.go)
- [pkg/db/locks.go](../../pkg/db/locks.go)
