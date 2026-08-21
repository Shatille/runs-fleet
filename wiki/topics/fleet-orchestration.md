---
topic: Fleet Orchestration (EC2 Fleet API)
last_compiled: 2026-08-21
sources_count: 4
---

# Fleet Orchestration (EC2 Fleet API)

## Purpose [coverage: high -- 4 sources]

`pkg/fleet` translates parsed `runs-on:` label specs into EC2 API calls that
launch runner instances. It implements spot-first launch with on-demand
fallback for cold-start jobs and a `RunInstances`-based path for warm-pool
instances that need stop/start reliability. The package owns launch template
selection by arch, the instance-type catalog and resolution, spot price
caching, region availability filtering, multi-AZ subnet spanning, and tag
propagation.

Since PR #432 (commit 5a26a69) it also owns the answer to *"which AMI would a
new instance of this architecture boot today?"* —
[pkg/fleet/ami.go](../../pkg/fleet/ami.go), a new file. That question belongs
here because this package already owns launch templates, and both consumers
(the admin console and the housekeeping stale-AMI sweep) must not be able to
disagree about the answer.

## Architecture [coverage: high -- 4 sources]

Three non-test files:

- [pkg/fleet/fleet.go](../../pkg/fleet/fleet.go) (1331 lines) — `Manager`,
  fleet/instance creation, caches, tags, spot pricing.
- [pkg/fleet/instances.go](../../pkg/fleet/instances.go) (355 lines) — the
  instance-type catalog and flexible-spec resolution.
- [pkg/fleet/ami.go](../../pkg/fleet/ami.go) (218 lines) — `AMIResolver`,
  the reference-AMI oracle.

Main exported types and functions:

- `Manager` — orchestrator, holds `EC2API`, `*config.Config`, `CircuitBreaker`,
  `MetricsAPI`, and three caches (`spotCache`, `availabilityCache`,
  `subnetAZCache`).
- `NewManager(cfg aws.Config, appConfig *config.Config) *Manager`.
- `Manager.SetCircuitBreaker` / `Manager.SetMetrics` — DI setters.
- `LaunchSpec` — input struct describing a single launch.
- `Manager.CreateFleet` — primary cold-start entry point; uses
  `FleetTypeInstant`.
- `Manager.CreateOnDemandInstance` — used by warm pool reconciliation; calls
  `RunInstances` directly so the instance is stop/start-capable, rotating
  across AZs on capacity errors.
- `Manager.RankInstanceTypesByPrice` — interleaved, inverse-price-weighted
  ordering (weight = `round(maxPrice/price)`, capped at 5) used by `pkg/pools`.
- `Manager.SpotPrice` — single-type price lookup on the 5-minute cache with
  negative caching of confirmed no-price types.
- `EC2API`, `CircuitBreaker`, `MetricsAPI`, `LaunchTemplateAPI` interfaces.
- `InstanceCatalog`, `InstanceSpec`, `FlexibleSpec`, `GetInstanceSpec`,
  `GetInstanceArch`, `GroupInstanceTypesByArch`, `ResolveInstanceTypes`,
  `DefaultFlexibleFamilies`.
- `AMIResolver`, `NewAMIResolver`, `CurrentAMI`, `TemplateArchSuffix`.

Call sequence inside `CreateFleet`
([pkg/fleet/fleet.go:161](../../pkg/fleet/fleet.go)):

1. A `fleet.create` tracing span is opened with instance types, spot flag, and
   target capacity attributes.
2. `shouldUseSpot` consults `LaunchSpec.Spot`, `config.SpotEnabled`,
   `ForceOnDemand`, then `circuitBreaker.CheckCircuit(primaryType)`. An open
   circuit forces on-demand.
3. `buildTags` produces the full tag set (see Data).
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
   `Type: types.FleetTypeInstant`, `TotalTargetCapacity=1`, and tag specs for
   `ResourceTypeInstance`.
6. On-demand path: `configureOnDemandRequest` rewrites configs to the single
   primary type spanned across every resolved subnet with
   `DefaultTargetCapacityTypeOnDemand`. Spot path: `configureSpotRequest` sets
   `SpotAllocationStrategy: SpotAllocationStrategyPriceCapacityOptimized`.
7. `ec2Client.CreateFleet` is invoked (only this call is timed for the
   `fleet_create` latency metric); `checkFleetErrors` converts `output.Errors`
   to a Go error, and a success/failure counter is published.
8. Instance IDs are flattened from `output.Instances[*].InstanceIds`.
9. `ec2Client.CreateTags` is called as a defensive fallback to ensure tags
   propagate (spot instances were observed to skip the in-`CreateFleet` tag
   specs); failure here is logged as a warning, not returned.

`CreateOnDemandInstance` ([pkg/fleet/fleet.go:1215](../../pkg/fleet/fleet.go))
skips the fleet machinery entirely: it picks a launch template via
`getLaunchTemplateForArch(arch)`, collapses `spec.SubnetIDs` to one subnet per
AZ, and calls `RunInstances` with `MinCount=MaxCount=1` per subnet in order —
a capacity-class error (`isCapacityError`: `InsufficientInstanceCapacity` and
friends) rotates to the next AZ, any other error fails fast.

**`AMIResolver`** ([pkg/fleet/ami.go:60](../../pkg/fleet/ami.go)) is a
separate, independently-constructed object — it takes a narrow
`LaunchTemplateAPI` (`DescribeLaunchTemplateVersions` only), not the full
`EC2API`, and is not a field on `Manager`. What it does, per architecture in
`TemplateArchSuffix` (`arm64` → template suffix `arm64`, `x86_64` → `amd64`
— EC2 reports `x86_64`, the template is named `amd64`):

1. `resolveArch` describes version `$Latest` of `<templateBase>-<suffix>` and
   reads `LaunchTemplateData.ImageId`. Any miss — error, empty result,
   template with no image — reports not-ok rather than a zero value, so the
   caller can tell "unknown" from "resolved to nothing."
2. `Current(ctx)` refreshes both architectures under one mutex behind a
   5-minute TTL (`amiCacheTTL`), bounded by a 5-second timeout
   (`amiResolveTimeout`) because the lock is held across the calls. It
   **merges over** the last known good map rather than replacing it, so one
   flaky call cannot take a previously-resolved architecture back to unknown
   for a whole TTL. It errors only when nothing has *ever* been read, and in
   that case does not store, so the next call retries. `cachedAt` is only
   reset when the refresh actually learned something.
3. `resolvedAt[arch]` records when each architecture was last *confirmed*
   against its template — deliberately distinct from `cachedAt`, because a
   merge carries an older value forward.
4. `CurrentImageIDs(ctx)` reduces `Current` to `arch → imageID` but **drops**
   any architecture whose `resolvedAt` is older than `amiTerminateFreshness`
   (= `2 * amiCacheTTL` = 10 min).
5. `UnresolvedArchs()` reports architectures with no recent-enough reference,
   sorted, for the console to render "we don't know" instead of a guess.

## Talks To [coverage: high -- 4 sources]

- **EC2 API** (via `EC2API`): `CreateFleet`, `CreateTags`, `DeleteFleets`,
  `DescribeFleetInstances`, `DescribeSpotPriceHistory`,
  `DescribeInstanceTypeOfferings`, `DescribeInstances`, `DescribeSubnets`,
  `RunInstances`. Separately, via `LaunchTemplateAPI`:
  `DescribeLaunchTemplateVersions`.
- **`pkg/circuit`** — `CircuitBreaker.CheckCircuit(ctx, instanceType)`;
  `circuit.StateOpen` forces on-demand.
- **`pkg/config`** — `*config.Config` provides `SpotEnabled`,
  `LaunchTemplateName`, `RunnerImage`, `TerminationQueueURL`,
  `SecretsBackend`, Vault settings, a custom `Tags` map, and the
  `TagKeyApplication`/`TagValueApplication`/`TagKeyService`/`TagValueService`
  cost-attribution remaps.
- **`pkg/metrics`** — optional `MetricsAPI` publishes `fleet_create`
  success/failure counts and CreateFleet latency, dimensioned by capacity
  (`spot`/`on_demand`).
- **`pkg/tracing`** — `fleet.create` span around fleet creation.
- **`pkg/logging`** — component logger
  `logging.WithComponent(logging.LogTypeFleet, "manager")`. Note `ami.go`
  logs nothing; its callers do.
- **`pkg/github`** (consumer) — `ResolveFlexibleSpec` in
  [pkg/github/webhook.go](../../pkg/github/webhook.go) fills empty family
  lists from `DefaultFlexibleFamilies` and resolves labels against
  `InstanceCatalog`.
- **`pkg/pools`** (consumer) — warm pools call `CreateOnDemandInstance` and
  `RankInstanceTypesByPrice` (see [warm-pools](warm-pools.md)).
- **`pkg/housekeeping`** (consumer of `AMIResolver`) — its `AMIReference`
  interface is exactly `CurrentImageIDs`;
  [pkg/housekeeping/stale_ami.go](../../pkg/housekeeping/stale_ami.go)
  `ExecuteStaleAMIInstances` uses it as the terminate oracle.
- **`pkg/admin`** (consumer of `AMIResolver`) —
  [pkg/admin/handler_ami.go](../../pkg/admin/handler_ami.go) serves
  `GET /api/instances/amis` from `Current` + `UnresolvedArchs`, and
  `handler_instances.go` uses `UnresolvedArchs` to decide whether staleness is
  even answerable for the instances list.
- **`cmd/server/main.go`** — builds **one** `AMIResolver` at
  [cmd/server/main.go:165](../../cmd/server/main.go) and hands the same
  pointer to housekeeping (`SetAMIReference`) and the admin handler
  (`SetAMIResolver`); two resolvers would cache independently and could
  disagree for a whole TTL, so the page could call an instance stale that the
  sweep considers fine, or the reverse.

This package does not call `pkg/db`; job state is updated by the orchestrator
layer that consumes the returned instance IDs.

## API Surface [coverage: high -- 4 sources]

```go
type EC2API interface { /* CreateFleet, CreateTags, DeleteFleets,
    DescribeFleetInstances, DescribeSpotPriceHistory,
    DescribeInstanceTypeOfferings, DescribeInstances, DescribeSubnets,
    RunInstances */ }

type LaunchTemplateAPI interface {
    DescribeLaunchTemplateVersions(ctx context.Context,
        params *ec2.DescribeLaunchTemplateVersionsInput,
        optFns ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplateVersionsOutput, error)
}

type CircuitBreaker interface {
    CheckCircuit(ctx context.Context, instanceType string) (circuit.State, error)
}

type MetricsAPI interface {
    PublishFleetCreate(ctx context.Context, capacity, result string) error
    PublishFleetCreateSeconds(ctx context.Context, capacity string, seconds float64) error
}

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

// ami.go
var TemplateArchSuffix = map[string]string{"arm64": "arm64", "x86_64": "amd64"}

type CurrentAMI struct {
    Architecture   string
    ImageID        string
    LaunchTemplate string
    Version        int64
    VersionCreated time.Time
}

func NewAMIResolver(api LaunchTemplateAPI, templateBase string) *AMIResolver
func (r *AMIResolver) Current(ctx context.Context) (map[string]CurrentAMI, error)
func (r *AMIResolver) CurrentImageIDs(ctx context.Context) (map[string]string, error)
func (r *AMIResolver) UnresolvedArchs() []string
```

## Data [coverage: high -- 4 sources]

**Tags** applied to every launched instance (via `buildTags`):

- `Name` — from `buildInstanceName(pool, repo, conditions)`, prefixed
  `runs-fleet-runner-`, capped at 64 chars.
- `runs-fleet:run-id` — workflow run ID.
- `runs-fleet:managed=true` — required by the housekeeping IAM policy to
  authorize termination of orphaned instances.
- `runs-fleet:pool` — when `Pool` is set.
- `runs-fleet:arch` — when `Arch` is set.
- `Role` — set to `Repo` for cost allocation.
- `Application=runs-fleet` and `Service=runner` (defaults) — cost-attribution
  tags; both key names and values configurable via `TagKeyApplication` /
  `TagValueApplication` and `TagKeyService` / `TagValueService` (PR #389).
  The `applicationTagKey`/`applicationTagValue`/`serviceTagKey`/
  `serviceTagValue` helpers fall back to the defaults on empty config fields.
- `runs-fleet:runner-image`, `runs-fleet:termination-queue-url` — read by the
  bootstrap script.
- `runs-fleet:secrets-backend` plus `runs-fleet:vault-*` (`vault-addr`,
  `vault-kv-mount`, `vault-kv-version`, `vault-base-path`,
  `vault-auth-method=aws`, `vault-aws-role=runs-fleet-runner`) when
  `SecretsBackend == "vault"`. EC2 runners always use AWS IAM auth.
- Custom tags from `m.config.Tags`.

**Launch templates** — resolved by `getLaunchTemplateForArch(arch)`, base name
from `config.LaunchTemplateName` defaulting to `defaultLaunchTemplateBase`
(`"runs-fleet-runner"`, the constant now lives in `ami.go` and is shared):
amd64 → `<base>-amd64`, arm64 (default) → `<base>-arm64`. All references use
`Version: "$Latest"` — which is also why `$Latest`, not `$Default`, is what
`AMIResolver` reads, and therefore what "stale" means.

**Block device override** — when `LaunchSpec.StorageGiB > 0`, root volume
overridden to `/dev/xvda`, gp3, encrypted, `DeleteOnTermination=true`.

**Caches:**

- `spotCache` — 5-minute TTL (`spotPriceCacheTTL`), via
  `DescribeSpotPriceHistory` (`Linux/UNIX`, max 100 results, query capped at
  the first 10 instance types). Also holds a `checked` set that negatively
  caches types confirmed to have no spot price.
- `availabilityCache` — 24-hour TTL (`availabilityCacheTTL`), via paginated
  `DescribeInstanceTypeOfferings` (`LocationTypeRegion`).
- `subnetAZCache` — permanent (subnet-to-AZ mapping is static config), via
  `DescribeSubnets` only for unresolved subnets.
- `AMIResolver.cached` — 5-minute TTL (`amiCacheTTL`), with a separate
  `resolvedAt` per architecture and a stricter 10-minute
  (`amiTerminateFreshness`) bar for termination decisions; refresh bounded by
  `amiResolveTimeout` (5s).

**Constants:** `spotPriceCacheTTL = 5m`, `availabilityCacheTTL = 24h`,
`archARM64 = "arm64"`, `instanceNameMaxLen = 64`, `maxFleetOverrides = 60`
(total override cap in one request — AWS allows 300 for instant fleets, kept
conservative; `typesPerSubnet` divides it so every subnet keeps at least one
type), `defaultLaunchTemplateBase = "runs-fleet-runner"`, `amiCacheTTL = 5m`,
`amiTerminateFreshness = 10m`, `amiResolveTimeout = 5s`.

**Instance catalog** — `InstanceCatalog` has **116 entries** spanning **20
distinct (arch, vCPU) shapes**, evenly split **58 arm64 / 58 amd64** across
**14 families**: arm64 `t4g`, `c7g`, `m7g`, `r7g`, `c8g`, `m8g`, `r8g`; amd64
`t3`, `c6i`, `c7i`, `m6i`, `m7i`, `r6i`, `r7i`. (Verified by enumerating
`fleet.InstanceCatalog`.) The sub-4-GiB burstable sizes
(`t4g.micro`/`t4g.small`, `t3.micro`/`t3.small`) are excluded from the catalog
entirely — smallest selectable *burstable* RAM is 4 GiB (PR #376 RAM floor).
The absolute RAM floor in the catalog is 2 GiB, from the 1-vCPU compute-
optimized entries `c7g.medium` and `c8g.medium`
([pkg/fleet/instances.go:32,62](../../pkg/fleet/instances.go)), which a
`cpu=1` request can still reach.

`DefaultFlexibleFamilies` excludes burstable families altogether (PR #385):

| arch | default families |
| --- | --- |
| `arm64` | `c8g, m8g, r8g, c7g, m7g` |
| `amd64` | `c6i, c7i, m6i, m7i` |
| unset | both lists concatenated, ARM first |

`t3`/`t4g` remain in the catalog only for explicit `family=` opt-in and pool
configs that name them.

## Key Decisions [coverage: high -- 4 sources]

- **Spot-first with on-demand fallback** for cold-start: gated by
  `Spot && SpotEnabled && !ForceOnDemand && circuit != Open`. `ForceOnDemand`
  is what a retry sets, so a second attempt does not repeat a spot failure.
  See [two-track-reliability](../concepts/two-track-reliability.md).
- **Warm pool uses `RunInstances`, not `CreateFleet`, and never spot.**
  Fleet-launched instances cannot be stopped, and warm pools rely on
  stop/start; the spot savings on a pool that is mostly stopped are negligible
  next to stop/start reliability. `CreateOnDemandInstance` bypasses spot
  entirely.
- **`FleetTypeInstant`** for cold-start (synchronous, returns instance IDs in
  the API response), not `maintain` — runs-fleet does not want EC2 Fleet to
  replace interrupted instances; the orchestrator re-queues jobs instead.
- **`SpotAllocationStrategyPriceCapacityOptimized`** balances price and
  interruption likelihood across diversified types.
- **PR #385 (commit 89e63c0): burstable families dropped from
  `DefaultFlexibleFamilies`.** An A/B benchmark against a competitor measured
  CI **2.25x slower** on pure compute because family-less amd64 requests
  defaulted to a list containing `t3`, `RankInstanceTypesByPrice` weights ~5x
  toward the cheapest spot price, and the warm-pool manager picks randomly
  from that weighted list — so ~2.5GHz burstable `t3.medium` won ~70% of
  warm-pool picks. The 2.25x figure is the one recorded in source
  ([pkg/fleet/instances.go:321-322](../../pkg/fleet/instances.go)); it is the
  pure-compute delta, not the end-to-end wall-clock delta for the same
  benchmark. The general lesson: price ranking may pick *among* adequate
  hardware, but the default candidate set must exclude starvation-grade tiers.
- **PR #376 (commit 248322f): RAM floor in the catalog.** 1-2 GiB burstable
  sizes were removed from `InstanceCatalog` because an unconstrained `cpu=2`
  request price-ranked to 1 GiB `t3.micro` and starved jobs. PR #385 is the
  same failure mode one tier up.
- **PRs #431/#432/#433/#434 (the AMI-staleness series): `$Latest` is the
  reference, and the resolver lives in `pkg/fleet`.** Root problem: **EC2 does
  not re-image on `StartInstances`, so a stopped warm-pool instance keeps its
  creation-time AMI for as long as it exists**, and nothing in the codebase
  was AMI-aware. #431 (c64b839) made staleness visible; #432 (5a26a69) added
  both a deliberate `POST /api/instances/replace-stale` and an unattended drip,
  and moved the resolver into this package so the console and the sweep share
  one cache; #433 (c2fc00e) stopped the sweep retiring an instance already
  promised to a job; #434 (05c6559) removed the sweep's config flag as
  redundant with its own guards.
  Because every launch path pins `$Latest`, `$Latest` — not `$Default` — is
  what a new instance actually gets, and therefore what "stale" means
  ([pkg/fleet/ami.go:53-58](../../pkg/fleet/ami.go)).
- **`CurrentImageIDs` is deliberately stricter than `Current`.** `Current`
  tolerates a merged-in value of any age so the console keeps rendering;
  `CurrentImageIDs` — the oracle for *destroying* instances — drops any
  architecture not confirmed within `amiTerminateFreshness`, because a
  reference left to drift would eventually condemn instances that are
  perfectly current. Same instinct as the sweep's own guards: it skips on an
  unknown reference rather than treating unknown as "everything is stale."
- **Merge-over-last-known-good in `Current`.** One architecture's template
  outage must not blind the other, and a refresh that fails must not discard
  what the last one learned. Only a refresh that learned something resets
  `cachedAt`, so a wholly-failed refresh leaves the entry due for immediate
  retry instead of caching blindness for a TTL.
- **Bounded refresh under the lock.** `amiResolveTimeout` (5s) exists because
  `Current` holds `r.mu` across both `DescribeLaunchTemplateVersions` calls; an
  unbounded call would queue every caller behind a hung EC2 API.
- **ARM-preferred defaults** — `DefaultFlexibleFamilies("")` lists Graviton
  families first; when arch is unspecified and prices are unavailable,
  `selectCheapestArch` defaults to `arm64`.
- **Cheapest-arch selection** — when arch is empty, query spot prices per arch
  group and pick the lower average.
- **CPU range bounded diversification** — `FlexibleSpec.CPUMax > 0` caps the
  range; `ResolveInstanceTypes` sorts ascending by CPU then RAM for better
  spot availability of smaller instances.
- **Region availability filter** — types unavailable in the current region are
  stripped before fleet creation; without this, EC2 returns
  `InsufficientInstanceCapacity` even when the request would otherwise be
  valid.
- **Multi-AZ spanning with one subnet per AZ** — fleet overrides are the
  (type × subnet) matrix; EC2 keys instance pools by (type, AZ), so
  `subnetsOnePerAZ` collapses same-AZ subnets to avoid duplicate pools, which
  EC2 rejects with `InvalidFleetConfig`. If AZ resolution fails the raw list is
  used so fleet creation still proceeds.
- **Override budget prioritizes AZ coverage** — `typesPerSubnet` trims
  types-per-subnet to stay under `maxFleetOverrides = 60` rather than dropping
  a subnet, so a single-AZ capacity shortfall never fails the whole request.
- **AZ rotation in `CreateOnDemandInstance`** — `RunInstances` targets one
  subnet per call, so capacity-class errors rotate to the next AZ while any
  other error (auth, bad config) fails fast without burning attempts.
- **PR #389 (commit fea609c): cost-attribution tag *values* configurable**,
  mirroring the existing key remaps. The values were hardcoded
  (`runs-fleet`/`runner`) with only key names remappable; a fork needing
  `Application=my-org-infra` could not safely add a duplicate custom tag on
  the same key, because EC2 does not document collision behavior for two
  same-key tags in one `CreateFleet` TagSpecification. New env vars
  `RUNS_FLEET_TAG_VALUE_APPLICATION`/`RUNS_FLEET_TAG_VALUE_SERVICE` default to
  the prior values, so unset is a no-op.
- **`CreateTags` fallback after `CreateFleet`** for spot tag propagation
  (commit c7e0a6b). The `TagSpecifications` in `CreateFleetInput` were observed
  to silently drop on spot instances; without `runs-fleet:managed=true`, IAM
  policies block housekeeping termination.

## Gotchas [coverage: high -- 4 sources]

- **A stopped pool instance never re-images by itself.** This is the whole
  premise of the AMI-staleness series and worth restating: `StartInstances`
  reuses the existing root volume, so a stopped warm-pool spare holds its
  creation-time AMI indefinitely — new CVE fixes, new pre-baked tool caches,
  new agent binaries all pass it by. Only termination + relaunch re-images it.
  Running instances are left alone by the sweep (they pick up the new image
  when they cycle after their next job).
- **The unattended stale-AMI sweep runs unconditionally, gated only by IAM.**
  PR #432 shipped it behind `RUNS_FLEET_STALE_AMI_SWEEP_ENABLED`; PR #434
  (commit 05c6559) deleted that flag — no other housekeeping task carries one,
  and what actually governs the sweep is whether the reference AMI resolves.
  Without `ec2:DescribeLaunchTemplateVersions` the resolver returns nothing and
  the sweep is a no-op. Its real bounds are its own guards:
  `staleAMIPerPoolPerCycle = 1` per pool per cycle (so a pool of ten stale
  spares converges over ten cycles and never dips more than one below target),
  stopped pool members only, a per-candidate state re-read immediately before
  each terminate, one terminate issued at a time rather than batched, and a
  skip on anything unverifiable — failed re-read, started since the scan,
  missing pool tag, unknown architecture, or an instance already promised to a
  job (PR #433, commit c2fc00e).
- **`AMIResolver` is keyed by EC2's architecture vocabulary, not runs-fleet's.**
  `TemplateArchSuffix` maps `x86_64` → template suffix `amd64`. A consumer that
  looks up `"amd64"` in a `CurrentImageIDs` map will miss; the keys are
  `arm64` and `x86_64`.
- **`amiTerminateFreshness` (10 min) is only 2x the cache TTL.** An
  architecture whose template read fails twice in a row silently drops out of
  termination decisions — correct behavior, but it means the sweep can quietly
  stop working for one architecture while the console still shows a (stale)
  image ID for it. `UnresolvedArchs()` is the signal, and it must be called
  *after* `Current`.
- **Family-less `gen=3` (amd64) and `gen=4` (arm64) resolve to zero types
  since PR #385.** Those generations contained only burstable families, so
  `ResolveFlexibleSpec` returns "no instance types match the specified
  cpu/ram/family requirements" and the job is rejected. Escape hatch: pass
  `family=t3` / `family=t4g` explicitly.
- **The catalog's RAM floor is 2 GiB, not 4.** The PR #376 floor removed
  *burstable* sub-4-GiB sizes, but `c7g.medium` and `c8g.medium` (1 vCPU,
  2 GiB) remain and are reachable by `cpu=1` — narrower than the original
  starvation case (which was an unconstrained `cpu=2`) but not closed.
- **`InsufficientInstanceCapacity`** — `output.Errors` is non-empty even when
  the API call returns success; `checkFleetErrors` surfaces these as Go errors.
  Filtering via `DescribeInstanceTypeOfferings` reduces but does not eliminate
  this for spot pools.
- **Spot interruption races** — `FleetTypeInstant` does not auto-replace, so
  in-progress jobs must be re-queued (or, since PR #454, re-run via
  `pkg/github.RerunJob`) by the orchestrator on interruption. See
  [github-integration](github-integration.md).
- **Tag propagation timing** — `CreateFleet` `TagSpecifications` may not
  propagate to spot instances; the post-call `CreateTags` is best-effort and
  logs warnings rather than failing fleet creation.
- **Instance-name length** — `buildInstanceName` truncates at 64 chars; long
  pool/repo combinations may collide.
- **Architecture inference** — `CreateOnDemandInstance` and
  `configureOnDemandRequest` require either an explicit `Arch` or an instance
  type present in `InstanceCatalog`; otherwise they return
  `cannot determine architecture`.
- **Spot price query throttling** — `fetchAndCacheSpotPrices` caps queries at
  the first 10 instance types and returns 0 on API errors; callers fall back to
  default ordering. `SpotPrice` negatively caches only a *confirmed* absence
  (query succeeded, no price), so a transient failure is retried within the TTL.
- **`RankInstanceTypesByPrice` weighting is bounded and fallback-prone** —
  weights cap at 5, types without price data get weight 1 and sort last, and
  when no prices are available at all the input order is returned unchanged.
- **Custom storage requires gp3 + encrypted root** — `StorageGiB > 0` forces
  `/dev/xvda`, encrypted gp3, `DeleteOnTermination=true`; non-gp3 or
  multi-volume layouts must come from the launch template.

## Sources [coverage: high]

- [pkg/fleet/fleet.go](../../pkg/fleet/fleet.go)
- [pkg/fleet/instances.go](../../pkg/fleet/instances.go)
- [pkg/fleet/ami.go](../../pkg/fleet/ami.go)
- [pkg/fleet/instances_test.go](../../pkg/fleet/instances_test.go)
- [pkg/fleet/ami_test.go](../../pkg/fleet/ami_test.go)
- [pkg/housekeeping/stale_ami.go](../../pkg/housekeeping/stale_ami.go)
- [pkg/admin/handler_ami.go](../../pkg/admin/handler_ami.go)
- [cmd/server/main.go](../../cmd/server/main.go)
