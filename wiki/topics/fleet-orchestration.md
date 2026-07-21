---
topic: Fleet Orchestration (EC2 Fleet API)
last_compiled: 2026-07-21
sources_count: 3
---

# Fleet Orchestration (EC2 Fleet API)

## Purpose [coverage: medium -- 3 sources]
`pkg/fleet` translates parsed `runs-on:` label specs into EC2 Fleet API calls
that launch ephemeral runner instances. It implements spot-first launch with
on-demand fallback for cold-start jobs and a `RunInstances`-based path for
warm-pool instances that need stop/start reliability. The package owns launch
template selection by arch, the instance-type catalog and resolution, spot
price caching, region availability filtering, multi-AZ subnet spanning, and
tag propagation.

## Architecture [coverage: medium -- 3 sources]

Main exported types and functions:

- `Manager` — orchestrator, holds `EC2API`, `*config.Config`, `CircuitBreaker`,
  `MetricsAPI`, and three caches (`spotCache`, `availabilityCache`,
  `subnetAZCache`).
- `NewManager(cfg aws.Config, appConfig *config.Config) *Manager` — constructor
  that wraps `ec2.NewFromConfig`.
- `Manager.SetCircuitBreaker(cb CircuitBreaker)` / `Manager.SetMetrics(m MetricsAPI)`
  — DI setters.
- `LaunchSpec` — input struct describing a single launch (RunID, InstanceType,
  InstanceTypes, SubnetID, SubnetIDs, Spot, Pool, Repo, ForceOnDemand,
  RetryCount, Arch, StorageGiB, Conditions, Reason).
- `Manager.CreateFleet(ctx, *LaunchSpec) ([]string, error)` — primary cold-start
  entry point; uses `FleetTypeInstant`.
- `Manager.CreateOnDemandInstance(ctx, *LaunchSpec) (string, error)` — used by
  warm pool reconciliation; calls `RunInstances` directly so the instance is
  stop/start-capable, rotating across AZs on capacity errors.
- `Manager.RankInstanceTypesByPrice(ctx, []string) []string` — interleaved,
  inverse-price-weighted ordering (weight = `round(maxPrice/price)`, capped at
  5) used by `pkg/pools` for warm-pool instance-type selection.
- `Manager.SpotPrice(ctx, instanceType) (float64, bool)` — single-type price
  lookup on the 5-minute cache with negative caching of confirmed no-price
  types.
- `EC2API` and `CircuitBreaker` interfaces for testability.
- `InstanceCatalog`, `InstanceSpec`, `FlexibleSpec`, `GetInstanceSpec`,
  `GetInstanceArch`, `GroupInstanceTypesByArch`, `ResolveInstanceTypes`,
  `DefaultFlexibleFamilies`.

Call sequence inside `CreateFleet`:

1. A `fleet.create` tracing span is opened with instance types, spot flag, and
   target capacity attributes.
2. `shouldUseSpot` consults `LaunchSpec.Spot`, `config.SpotEnabled`,
   `ForceOnDemand`, then `circuitBreaker.CheckCircuit(primaryType)`. Open
   circuit forces on-demand.
3. `buildTags` produces the full tag set (see Data section).
4. `buildFleetRequest` calls `buildLaunchTemplateConfigs` →
   `filterAvailableInstanceTypes` (region filter via
   `DescribeInstanceTypeOfferings`, 24h cache) → `resolveSubnets` (collapses
   `SubnetIDs` to one subnet per AZ via `subnetsOnePerAZ`) → either
   `buildSingleArchConfig` for a fixed arch, or `GroupInstanceTypesByArch` +
   `selectCheapestArch` (uses `DescribeSpotPriceHistory`, 5-min cache) when
   arch is unspecified. Overrides are the (instance type × subnet)
   cross-product, bounded by `maxFleetOverrides` with `typesPerSubnet`
   trimming types before dropping any subnet.
5. The request is wrapped in `CreateFleetInput` with
   `Type: types.FleetTypeInstant`, `TotalTargetCapacity=1`, and tag specs
   for `ResourceTypeInstance`.
6. On-demand path: `configureOnDemandRequest` rewrites configs to the single
   primary type spanned across every resolved subnet with
   `DefaultTargetCapacityTypeOnDemand`. Spot path: `configureSpotRequest` sets
   `SpotAllocationStrategy: SpotAllocationStrategyPriceCapacityOptimized`.
7. `ec2Client.CreateFleet` is invoked (only this call is timed for the
   `fleet_create` latency metric); `checkFleetErrors` converts
   `output.Errors` to a Go error, and a success/failure counter is published.
8. Instance IDs are flattened from `output.Instances[*].InstanceIds`.
9. `ec2Client.CreateTags` is called as a defensive fallback to ensure tags
   propagate (spot instances were observed to skip the in-`CreateFleet`
   tag specs); failure here is logged as a warning, not returned.

`CreateOnDemandInstance` skips the fleet machinery entirely: it picks a
launch template via `getLaunchTemplateForArch(arch)`, collapses
`spec.SubnetIDs` to one subnet per AZ, and calls `RunInstances` with
`MinCount=MaxCount=1` per subnet in order — a capacity-class error
(`isCapacityError`: `InsufficientInstanceCapacity` and friends) rotates to
the next AZ, any other error fails fast.

## Talks To [coverage: medium -- 3 sources]

- **EC2 API** (via `EC2API` interface): `CreateFleet`, `CreateTags`,
  `DeleteFleets`, `DescribeFleetInstances`, `DescribeSpotPriceHistory`,
  `DescribeInstanceTypeOfferings`, `DescribeInstances`, `DescribeSubnets`,
  `RunInstances`.
- **`pkg/circuit`** — `CircuitBreaker.CheckCircuit(ctx, instanceType)` returns
  `circuit.State`; `circuit.StateOpen` forces on-demand.
- **`pkg/config`** — `*config.Config` provides `SpotEnabled`,
  `LaunchTemplateName`, `RunnerImage`, `TerminationQueueURL`,
  `SecretsBackend`, Vault settings, custom `Tags` map, and the
  `TagKeyApplication`/`TagValueApplication`/`TagKeyService`/`TagValueService`
  cost-attribution remaps. `aws.Config` feeds the EC2 client constructor.
- **`pkg/metrics`** — optional `MetricsAPI` publishes `fleet_create`
  success/failure counts and CreateFleet latency, dimensioned by capacity
  (`spot`/`on_demand`).
- **`pkg/tracing`** — `fleet.create` span around fleet creation.
- **`pkg/logging`** — component logger
  `logging.WithComponent(logging.LogTypeFleet, "manager")` plus
  `logging.KeyRunID` / `logging.KeyInstanceID` field keys.
- **`pkg/github`** (consumer) — `ResolveFlexibleSpec` in
  [pkg/github/webhook.go](../../pkg/github/webhook.go) fills empty family
  lists from `DefaultFlexibleFamilies` and resolves labels against
  `InstanceCatalog`.
- **`pkg/pools`** (consumer) — warm pools call `CreateOnDemandInstance` and
  `RankInstanceTypesByPrice` (see [warm-pools](warm-pools.md)).

Note: this package does not call `pkg/db` directly; job state is updated by
the orchestrator layer that consumes the returned instance IDs.

## API Surface [coverage: medium -- 3 sources]

```go
type EC2API interface { /* CreateFleet, CreateTags, DeleteFleets,
    DescribeFleetInstances, DescribeSpotPriceHistory,
    DescribeInstanceTypeOfferings, DescribeInstances, DescribeSubnets,
    RunInstances */ }

type CircuitBreaker interface {
    CheckCircuit(ctx context.Context, instanceType string) (circuit.State, error)
}

type MetricsAPI interface {
    PublishFleetCreate(ctx context.Context, capacity, result string) error
    PublishFleetCreateSeconds(ctx context.Context, capacity string, seconds float64) error
}

type Manager struct { /* ec2Client, config, circuitBreaker, metrics, caches */ }

func NewManager(cfg aws.Config, appConfig *config.Config) *Manager
func (m *Manager) SetCircuitBreaker(cb CircuitBreaker)
func (m *Manager) SetMetrics(metrics MetricsAPI)
func (m *Manager) CreateFleet(ctx context.Context, spec *LaunchSpec) ([]string, error)
func (m *Manager) CreateOnDemandInstance(ctx context.Context, spec *LaunchSpec) (string, error)
func (m *Manager) RankInstanceTypesByPrice(ctx context.Context, instanceTypes []string) []string
func (m *Manager) SpotPrice(ctx context.Context, instanceType string) (float64, bool)

type LaunchSpec struct {
    RunID         int64
    InstanceType  string   // primary type (used if InstanceTypes is empty)
    InstanceTypes []string // spot diversification set
    SubnetID      string   // single-subnet fallback
    SubnetIDs     []string // all configured subnets; spans every AZ when non-empty
    Spot          bool
    Pool          string
    Repo          string
    ForceOnDemand bool
    RetryCount    int
    Arch          string
    StorageGiB    int
    Conditions    string
    Reason        string   // e.g. "ready_deficit", "stopped_replenish"
}

type InstanceSpec struct{ Type string; CPU int; RAM float64; Arch, Family string; Gen int }
type FlexibleSpec struct{ CPUMin, CPUMax int; RAMMin, RAMMax float64; Arch string; Families []string; Gen int }

var InstanceCatalog []InstanceSpec

func GetInstanceSpec(instanceType string) (InstanceSpec, bool)
func GetInstanceArch(instanceType string) string
func GroupInstanceTypesByArch(instanceTypes []string) map[string][]string
func ResolveInstanceTypes(spec FlexibleSpec) []string
func DefaultFlexibleFamilies(arch string) []string
func (is InstanceSpec) MatchesFlexibleSpec(spec FlexibleSpec) bool
```

## Data [coverage: medium -- 3 sources]

**Tags** applied to every launched instance (via `buildTags`):

- `Name` — from `buildInstanceName(pool, repo, conditions)`, prefixed
  `runs-fleet-runner-`, capped at 64 chars.
- `runs-fleet:run-id` — workflow run ID.
- `runs-fleet:managed=true` — required by housekeeping IAM policy to
  authorize termination of orphaned instances.
- `runs-fleet:pool` — when `Pool` is set.
- `runs-fleet:arch` — when `Arch` is set.
- `Role` — set to `Repo` for cost allocation.
- `Application=runs-fleet` and `Service=runner` (defaults) — cost-attribution
  tags; both key names and values configurable via `TagKeyApplication` /
  `TagValueApplication` and `TagKeyService` / `TagValueService` (PR #389;
  values were fixed before). The `applicationTagKey`/`applicationTagValue`/
  `serviceTagKey`/`serviceTagValue` helpers fall back to the defaults on
  empty config fields, so runner instances are always tagged.
- `runs-fleet:runner-image`, `runs-fleet:termination-queue-url` — bootstrap script reads these.
- `runs-fleet:secrets-backend` plus `runs-fleet:vault-*`
  (`vault-addr`, `vault-kv-mount`, `vault-kv-version`, `vault-base-path`,
  `vault-auth-method=aws`, `vault-aws-role=runs-fleet-runner`) when
  `SecretsBackend == "vault"`. EC2 runners always use AWS IAM auth.
- Custom tags from `m.config.Tags`.

**Launch templates** — resolved by `getLaunchTemplateForArch(arch)`:

- Base name from `config.LaunchTemplateName`, default `runs-fleet-runner`.
- Linux amd64: `<base>-amd64`.
- Linux arm64 (default): `<base>-arm64`.
- All references use `Version: "$Latest"`.

**Block device override** — when `LaunchSpec.StorageGiB > 0`, root volume
overridden to `/dev/xvda`, gp3, encrypted, `DeleteOnTermination=true`.

**Caches:**

- `spotCache` — 5-minute TTL, populated via `DescribeSpotPriceHistory`
  (`Linux/UNIX`, max 100 results, query capped at first 10 instance types).
  Also holds a `checked` set that negatively caches types confirmed to have
  no spot price within the window.
- `availabilityCache` — 24-hour TTL, populated via paginated
  `DescribeInstanceTypeOfferings` (`LocationTypeRegion`).
- `subnetAZCache` — permanent (subnet-to-AZ mapping is static config),
  populated via `DescribeSubnets` only for unresolved subnets.

**Constants:**

- `spotPriceCacheTTL = 5 * time.Minute`
- `availabilityCacheTTL = 24 * time.Hour`
- `archARM64 = "arm64"`
- `instanceNameMaxLen = 64`
- `maxFleetOverrides = 60` — total override cap across all launch template
  configs in one request (AWS allows 300 for instant fleets; kept
  conservative). `typesPerSubnet` divides it so every subnet keeps at least
  one type.

**Instance catalog** — `InstanceCatalog` covers ARM64
t4g/c7g/m7g/r7g/c8g/m8g/r8g and amd64 t3/c6i/c7i/m6i/m7i/r6i/r7i. The
sub-4-GiB burstable sizes (`t4g.micro`/`t4g.small`, `t3.micro`/`t3.small`)
are excluded from the catalog entirely — smallest selectable RAM is 4 GiB
(PR #376 RAM floor). `DefaultFlexibleFamilies` excludes burstable families
altogether (PR #385): arm64 → `[c8g, m8g, r8g, c7g, m7g]`, amd64 →
`[c6i, c7i, m6i, m7i]`, no arch → both lists combined. `t3`/`t4g` remain in
the catalog only for explicit `family=` opt-in and pool configs that name
them.

## Key Decisions [coverage: medium -- 3 sources]

- **Spot-first with on-demand fallback** for cold-start: gated by
  `Spot && SpotEnabled && !ForceOnDemand && circuit != Open`.
- **`FleetTypeInstant`** for cold-start (synchronous, returns instance IDs in
  the API response), not `maintain` — runs-fleet does not want EC2 Fleet to
  replace interrupted instances; the orchestrator re-queues jobs instead.
- **`SpotAllocationStrategyPriceCapacityOptimized`** balances price and
  interruption likelihood across diversified types.
- **PR #385 (commit 89e63c0): burstable families dropped from
  `DefaultFlexibleFamilies`.** An A/B benchmark showed CI 2.25x slower than a
  competitor on pure compute because family-less amd64 requests defaulted to
  a list containing `t3`, `RankInstanceTypesByPrice` weights ~5x toward the
  cheapest spot price, and the warm-pool manager picks randomly from that
  weighted list — so ~2.5GHz burstable `t3.medium` won ~70% of warm-pool
  picks. `t3`/`t4g` now stay in `InstanceCatalog` for explicit `family=`
  opt-in only. Sequel to the PR #376 RAM floor (commit 248322f): the same
  failure mode — price optimization selecting starvation-grade hardware —
  one tier up.
- **PR #376 (commit 248322f): RAM floor in the catalog.** 1-2 GiB burstable
  sizes were removed from `InstanceCatalog` because an unconstrained `cpu=2`
  request price-ranked to 1 GiB `t3.micro` and starved jobs.
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
- **Multi-AZ spanning with one subnet per AZ** — fleet overrides are the
  (type × subnet) matrix; EC2 keys instance pools by (type, AZ), so
  `subnetsOnePerAZ` collapses same-AZ subnets to avoid duplicate pools,
  which EC2 rejects with `InvalidFleetConfig`. If AZ resolution fails the raw
  list is used so fleet creation still proceeds.
- **Override budget prioritizes AZ coverage** — `typesPerSubnet` trims
  types-per-subnet to stay under `maxFleetOverrides = 60` rather than
  dropping a subnet, so a single-AZ capacity shortfall never fails the whole
  request.
- **AZ rotation in `CreateOnDemandInstance`** — `RunInstances` targets one
  subnet per call, so capacity-class errors rotate to the next AZ while any
  other error (auth, bad config) fails fast without burning attempts.
- **PR #389 (commit fea609c): cost-attribution tag *values* configurable,
  mirroring the existing key remaps.** The values were hardcoded
  (`runs-fleet`/`runner`) with only the key names remappable; a fork needing
  `Application=my-org-infra` could not safely add a duplicate custom tag on
  the same key, because EC2 does not document collision behavior for two
  same-key tags in one `CreateFleet` TagSpecification. New env vars
  `RUNS_FLEET_TAG_VALUE_APPLICATION`/`RUNS_FLEET_TAG_VALUE_SERVICE` default
  to the prior hardcoded values, so unset is a no-op.
- **`CreateTags` fallback after `CreateFleet`** for spot tag propagation
  (commit c7e0a6b). The `TagSpecifications` in `CreateFleetInput` were
  observed to silently drop on spot instances; without
  `runs-fleet:managed=true`, IAM policies block housekeeping termination.
- **Warm pool uses `RunInstances`, not `CreateFleet`** — fleet-launched
  instances cannot be stopped, so warm pools (which rely on stop/start)
  bypass spot entirely via `CreateOnDemandInstance`.

## Gotchas [coverage: medium -- 3 sources]

- **Family-less `gen=3` (amd64) and `gen=4` (arm64) resolve to zero types
  since PR #385.** Those generations contained only burstable families, which
  are no longer in the defaults, so `ResolveFlexibleSpec` returns "no
  instance types match the specified cpu/ram/family requirements" and the job
  is rejected. Escape hatch: pass `family=t3` / `family=t4g` explicitly.
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
  the first 10 instance types and returns 0 on API errors; callers fall back
  to default ordering. `SpotPrice` negatively caches only a *confirmed*
  absence (query succeeded, no price), so a transient failure is retried
  within the TTL window.
- **`RankInstanceTypesByPrice` weighting is bounded and fallback-prone** —
  weights cap at 5, types without price data get weight 1 and sort last, and
  when no prices are available at all the input order is returned unchanged.
- **Custom storage requires gp3 + encrypted root** — `StorageGiB > 0` forces
  `/dev/xvda`, encrypted gp3, `DeleteOnTermination=true`; non-gp3 or
  multi-volume layouts must come from the launch template.

## Sources [coverage: high]
- [pkg/fleet/fleet.go](../../pkg/fleet/fleet.go)
- [pkg/fleet/instances.go](../../pkg/fleet/instances.go)
- [pkg/fleet/instances_test.go](../../pkg/fleet/instances_test.go)
