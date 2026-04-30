---
topic: Fleet Orchestration (EC2 Fleet API)
last_compiled: 2026-04-30
sources_count: 2
---

# Fleet Orchestration (EC2 Fleet API)

## Purpose [coverage: medium -- 2 sources]
`pkg/fleet` translates parsed `runs-on:` label specs into EC2 Fleet API calls
that launch ephemeral runner instances. It implements spot-first launch with
on-demand fallback for cold-start jobs and an `RunInstances`-based path for
warm-pool instances that need stop/start reliability. The package owns launch
template selection by OS/arch, instance-type catalog and resolution, spot price
caching, region availability filtering, and tag propagation.

## Architecture [coverage: medium -- 2 sources]

Main exported types and functions:

- `Manager` — orchestrator, holds `EC2API`, `*config.Config`, `CircuitBreaker`,
  and two TTL caches (`spotPriceCache`, `availabilityCache`).
- `NewManager(cfg aws.Config, appConfig *config.Config) *Manager` — constructor
  that wraps `ec2.NewFromConfig`.
- `Manager.SetCircuitBreaker(cb CircuitBreaker)` — DI for the circuit breaker.
- `LaunchSpec` — input struct describing a single launch (RunID, InstanceType,
  InstanceTypes, SubnetID, Spot, Pool, Repo, ForceOnDemand, RetryCount, Region,
  Environment, OS, Arch, StorageGiB, Conditions).
- `Manager.CreateFleet(ctx, *LaunchSpec) ([]string, error)` — primary cold-start
  entry point; uses `FleetTypeInstant`.
- `Manager.CreateOnDemandInstance(ctx, *LaunchSpec) (string, error)` — used by
  warm pool reconciliation; calls `RunInstances` directly so the instance is
  stop/start-capable.
- `Manager.RankInstanceTypesByPrice(ctx, []string) []string` — interleaved,
  inverse-price-weighted ordering used upstream for spot pool diversification.
- `EC2API` and `CircuitBreaker` interfaces for testability.
- `InstanceCatalog`, `InstanceSpec`, `FlexibleSpec`, `GetInstanceSpec`,
  `GetInstanceArch`, `GroupInstanceTypesByArch`, `ResolveInstanceTypes`,
  `DefaultFlexibleFamilies`, `WindowsInstanceTypes`, `IsValidWindowsInstanceType`.

Call sequence inside `CreateFleet`:

1. `shouldUseSpot` consults `LaunchSpec.Spot`, `config.SpotEnabled`,
   `ForceOnDemand`, then `circuitBreaker.CheckCircuit(primaryType)`. Open
   circuit forces on-demand.
2. `buildTags` produces the full tag set (see Data section).
3. `buildFleetRequest` calls `buildLaunchTemplateConfigs` →
   `filterAvailableInstanceTypes` (region filter via
   `DescribeInstanceTypeOfferings`, 24h cache) → either
   `buildSingleArchConfig` for a fixed arch, or `GroupInstanceTypesByArch` +
   `selectCheapestArch` (uses `DescribeSpotPriceHistory`, 5-min cache) when
   arch is unspecified.
4. The request is wrapped in `CreateFleetInput` with
   `Type: types.FleetTypeInstant`, `TotalTargetCapacity=1`, and tag specs
   for `ResourceTypeInstance`.
5. On-demand path: `configureOnDemandRequest` rewrites configs to a single
   primary type with `DefaultTargetCapacityTypeOnDemand`. Spot path:
   `configureSpotRequest` sets
   `SpotAllocationStrategy: SpotAllocationStrategyPriceCapacityOptimized`.
6. `ec2Client.CreateFleet` is invoked; `checkFleetErrors` converts
   `output.Errors` to a Go error.
7. Instance IDs are flattened from `output.Instances[*].InstanceIds`.
8. `ec2Client.CreateTags` is called as a defensive fallback to ensure tags
   propagate (spot instances were observed to skip the in-`CreateFleet`
   tag specs); failure here is logged as a warning, not returned.

`CreateOnDemandInstance` skips the fleet machinery entirely: it picks a
launch template via `getLaunchTemplateForArch(spec.OS, arch)` and calls
`RunInstances` with `MinCount=MaxCount=1` and the same tag set.

## Talks To [coverage: medium -- 2 sources]

- **EC2 API** (via `EC2API` interface): `CreateFleet`, `CreateTags`,
  `DeleteFleets`, `DescribeFleetInstances`, `DescribeSpotPriceHistory`,
  `DescribeInstanceTypeOfferings`, `DescribeInstances`, `RunInstances`.
- **`pkg/circuit`** — `CircuitBreaker.CheckCircuit(ctx, instanceType)` returns
  `circuit.State`; `circuit.StateOpen` forces on-demand.
- **`pkg/config`** — `*config.Config` provides `SpotEnabled`,
  `LaunchTemplateName`, `RunnerImage`, `TerminationQueueURL`, `CacheURL`,
  `SecretsBackend`, Vault settings, custom `Tags` map. `aws.Config` feeds the
  EC2 client constructor.
- **`pkg/logging`** — component logger
  `logging.WithComponent(logging.LogTypeFleet, "manager")` plus
  `logging.KeyRunID` / `logging.KeyInstanceID` field keys.

Note: this package does not call `pkg/db` directly; job state is updated by
the orchestrator layer that consumes the returned instance IDs.

## API Surface [coverage: medium -- 2 sources]

```go
type EC2API interface { /* CreateFleet, CreateTags, DeleteFleets,
    DescribeFleetInstances, DescribeSpotPriceHistory,
    DescribeInstanceTypeOfferings, DescribeInstances, RunInstances */ }

type CircuitBreaker interface {
    CheckCircuit(ctx context.Context, instanceType string) (circuit.State, error)
}

type Manager struct { /* ec2Client, config, circuitBreaker, caches */ }

func NewManager(cfg aws.Config, appConfig *config.Config) *Manager
func (m *Manager) SetCircuitBreaker(cb CircuitBreaker)
func (m *Manager) CreateFleet(ctx context.Context, spec *LaunchSpec) ([]string, error)
func (m *Manager) CreateOnDemandInstance(ctx context.Context, spec *LaunchSpec) (string, error)
func (m *Manager) RankInstanceTypesByPrice(ctx context.Context, instanceTypes []string) []string

type LaunchSpec struct {
    RunID         int64
    InstanceType  string
    InstanceTypes []string
    SubnetID      string
    Spot          bool
    Pool          string
    Repo          string
    ForceOnDemand bool
    RetryCount    int
    Region        string
    Environment   string
    OS            string
    Arch          string
    StorageGiB    int
    Conditions    string
}

type InstanceSpec struct{ Type string; CPU int; RAM float64; Arch, Family string; Gen int }
type FlexibleSpec struct{ CPUMin, CPUMax int; RAMMin, RAMMax float64; Arch string; Families []string; Gen int }

var InstanceCatalog []InstanceSpec
var WindowsInstanceTypes map[string]bool

func GetInstanceSpec(instanceType string) (InstanceSpec, bool)
func GetInstanceArch(instanceType string) string
func GroupInstanceTypesByArch(instanceTypes []string) map[string][]string
func ResolveInstanceTypes(spec FlexibleSpec) []string
func DefaultFlexibleFamilies(arch string) []string
func IsValidWindowsInstanceType(instanceType string) bool
func (is InstanceSpec) MatchesFlexibleSpec(spec FlexibleSpec) bool
```

## Data [coverage: medium -- 2 sources]

**Tags** applied to every launched instance (via `buildTags`):

- `Name` — from `buildInstanceName(pool, repo, conditions)`, prefixed
  `runs-fleet-runner-`, capped at 64 chars.
- `runs-fleet:run-id` — workflow run ID.
- `runs-fleet:managed=true` — required by housekeeping IAM policy to
  authorize termination of orphaned instances.
- `runs-fleet:pool` — when `Pool` is set.
- `runs-fleet:os` — when `OS` is set.
- `runs-fleet:arch` — when `Arch` is set.
- `runs-fleet:region` — Phase 3 multi-region support.
- `runs-fleet:environment` and standard `Environment` — Phase 6 per-stack envs.
- `Role` — set to `Repo` for cost allocation.
- `runs-fleet:runner-image`, `runs-fleet:termination-queue-url`,
  `runs-fleet:cache-url` — bootstrap script reads these.
- `runs-fleet:secrets-backend` plus `runs-fleet:vault-*`
  (`vault-addr`, `vault-kv-mount`, `vault-kv-version`, `vault-base-path`,
  `vault-auth-method=aws`, `vault-aws-role=runs-fleet-runner`) when
  `SecretsBackend == "vault"`. EC2 runners always use AWS IAM auth.
- Custom tags from `m.config.Tags`.

**Launch templates** — resolved by `getLaunchTemplateForArch(os, arch)`:

- Base name from `config.LaunchTemplateName`, default `runs-fleet-runner`.
- Windows: `<base>-windows`.
- Linux amd64: `<base>-amd64`.
- Linux arm64 (default): `<base>-arm64`.
- All references use `Version: "$Latest"`.

**Block device override** — when `LaunchSpec.StorageGiB > 0`, root volume
overridden to `/dev/xvda`, gp3, encrypted, `DeleteOnTermination=true`.

**Caches:**

- `spotPriceCache` — 5-minute TTL, populated via `DescribeSpotPriceHistory`
  (`Linux/UNIX`, max 100 results, query capped at first 10 instance types).
- `availabilityCache` — 24-hour TTL, populated via
  `DescribeInstanceTypeOfferings` (`LocationTypeRegion`).

**Constants:**

- `spotPriceCacheTTL = 5 * time.Minute`
- `availabilityCacheTTL = 24 * time.Hour`
- `archARM64 = "arm64"`
- `instanceNameMaxLen = 64`
- `maxOverridesPerConfig = 20` (EC2 Fleet hard limit per launch template config).

**Instance catalog** — `InstanceCatalog` covers ARM64 t4g/c7g/m7g/r7g/c8g/m8g/r8g
and amd64 t3/c6i/c7i/m6i/m7i/r6i/r7i. `DefaultFlexibleFamilies` returns
arm64 priority list `[c8g, m8g, r8g, c7g, m7g, t4g]` by default, amd64
`[c6i, c7i, m6i, m7i, t3]`, or both interleaved when no arch is given.
`WindowsInstanceTypes` is a small allow-list of t3/m6i/c6i sizes only.

## Key Decisions [coverage: medium -- 2 sources]

- **Spot-first with on-demand fallback** for cold-start: gated by
  `Spot && SpotEnabled && !ForceOnDemand && circuit != Open`.
- **`FleetTypeInstant`** for cold-start (synchronous, returns instance IDs in
  the API response), not `maintain` — runs-fleet does not want EC2 Fleet to
  replace interrupted instances; the orchestrator re-queues jobs instead.
- **`SpotAllocationStrategyPriceCapacityOptimized`** balances price and
  interruption likelihood across diversified types.
- **ARM-preferred defaults** — `DefaultFlexibleFamilies("")` lists Graviton
  families before amd64; when arch is unspecified and prices are unavailable,
  `selectCheapestArch` defaults to `arm64`.
- **Cheapest-arch selection** — when arch is empty, query spot prices per arch
  group and pick the lower average.
- **CPU range bounded diversification** — `FlexibleSpec.CPUMax > 0` caps the
  range; `ResolveInstanceTypes` sorts ascending by CPU then RAM for better
  spot availability of smaller instances.
- **Region availability filter** — types unavailable in the current region
  are stripped before fleet creation; without this, EC2 returns
  `InsufficientInstanceCapacity` even when the request would otherwise be valid.
- **Cap of 20 overrides per launch template config** — EC2 Fleet limit;
  enforced by `buildSingleArchConfig`.
- **`CreateTags` fallback after `CreateFleet`** for spot tag propagation
  (commit c7e0a6b). The `TagSpecifications` in `CreateFleetInput` were
  observed to silently drop on spot instances; without
  `runs-fleet:managed=true`, IAM policies block housekeeping termination.
- **`httpClient` transport workaround for go-github** (commit cb37fba) —
  configured outside this package but consumed when callers prepare GitHub
  registration tokens before invoking the fleet manager.
- **Warm pool uses `RunInstances`, not `CreateFleet`** — fleet-launched
  instances cannot be stopped, so warm pools (which rely on stop/start)
  bypass spot entirely via `CreateOnDemandInstance`.

## Gotchas [coverage: medium -- 2 sources]

- **`InsufficientInstanceCapacity`** — `output.Errors` is non-empty even when
  the API call returns success; `checkFleetErrors` surfaces these as Go
  errors. Filtering via `DescribeInstanceTypeOfferings` reduces but does not
  eliminate this for spot pools.
- **Spot interruption races** — `FleetTypeInstant` does not auto-replace, so
  in-progress jobs must be re-queued by the orchestrator on interruption.
- **Tag propagation timing** — `CreateFleet` `TagSpecifications` may not
  propagate to spot instances; the post-call `CreateTags` is best-effort and
  logs warnings on failure rather than failing the fleet creation.
- **Request type semantics** — `Type: FleetTypeInstant` returns instance IDs
  synchronously and does not maintain capacity; `maintain` is intentionally
  not used.
- **Instance-name length** — `buildInstanceName` truncates at 64 chars to fit
  the EC2 `Name` tag conventions; long pool/repo combinations may collide.
- **Architecture inference** — `CreateOnDemandInstance` and
  `configureOnDemandRequest` require either an explicit `Arch` or an
  instance type present in `InstanceCatalog`; otherwise they return
  `cannot determine architecture`.
- **Spot price query throttling** — `fetchAndCacheSpotPrices` caps queries at
  the first 10 instance types and silently returns 0 on API errors; callers
  fall back to default ordering.
- **Windows allow-list is small** — `WindowsInstanceTypes` only covers a
  handful of t3/m6i/c6i sizes; arbitrary instance types passed for Windows
  will not be validated by `IsValidWindowsInstanceType` and may fail at
  launch.
- **Custom storage requires gp3 + encrypted root** — `StorageGiB > 0` forces
  `/dev/xvda`, encrypted gp3, `DeleteOnTermination=true`; non-gp3 or
  multi-volume layouts must come from the launch template.

## Sources [coverage: high]
- [pkg/fleet/fleet.go](../../pkg/fleet/fleet.go)
- [pkg/fleet/instances.go](../../pkg/fleet/instances.go)
