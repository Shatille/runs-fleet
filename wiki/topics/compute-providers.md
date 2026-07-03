---
topic: Compute Providers (Backend Abstraction)
last_compiled: 2026-07-03
sources_count: 3
---

# Compute Providers (Backend Abstraction)

## Purpose [coverage: medium -- 3 sources]

There is no compute-provider abstraction in the current codebase. `pkg/provider`
— which used to define a backend-agnostic `Provider` interface so orchestrator
code could target either EC2 or a Kubernetes backend without knowing which —
has been deleted in its entirety (`pkg/provider/interface.go`,
`pkg/provider/k8s/provider.go`, `pkg/provider/k8s/pool.go` are all gone; `ls
pkg/provider` returns "No such path"). The K8s runner backend was removed
upstream as of 2026-06, leaving EC2 as the only compute backend.

EC2 fleet orchestration now lives directly in
[pkg/fleet/fleet.go](../../pkg/fleet/fleet.go) and
[pkg/fleet/instances.go](../../pkg/fleet/instances.go), with no interface
layer above it. Callers such as
[pkg/pools/manager.go](../../pkg/pools/manager.go) invoke `fleet.Manager`
methods (`CreateFleet`, `CreateOnDemandInstance`) and catalog helpers
(`fleet.GetInstanceSpec`, `fleet.FlexibleSpec`) directly — there is no
`Provider`, `PoolProvider`, `StateStore`, or `Coordinator` interface to satisfy
and no `backend=ec2|k8s` label or `RUNS_FLEET_MODE` switch. This topic is kept
as a placeholder primarily to record that history; see fleet-orchestration for
the live implementation.

## Architecture [coverage: low -- 1 source]

Current package layout, for contrast with the removed abstraction:

- `pkg/fleet/fleet.go` — `Manager` struct implementing fleet creation
  (`CreateFleet`, `CreateOnDemandInstance`) directly against the EC2 SDK
  (`EC2API` interface wraps `ec2.Client` for testability). No exported
  interface sits above `Manager`; consumers hold a concrete `*fleet.Manager`
  or a narrow local interface (e.g. `pkg/pools`' `CreateFleet`/
  `CreateOnDemandInstance` interface at manager.go:60-61) scoped to the two
  methods they call.
- `pkg/fleet/instances.go` — `InstanceCatalog`, `InstanceSpec`,
  `FlexibleSpec`, and resolution helpers (`ResolveInstanceTypes`,
  `GetInstanceArch`, `GroupInstanceTypesByArch`) used to translate label-based
  resource requests into concrete EC2 instance types.
- No `pkg/provider/`, no `pkg/provider/ec2/`, no `pkg/provider/k8s/`. There is
  nothing to be "asymmetric" with anymore — the prior asymmetry (K8s had a
  dedicated subpackage, EC2 did not) was resolved by deleting the K8s side
  rather than adding an EC2 side.

## Talks To [coverage: low -- 1 source]

- **`pkg/fleet`** — no longer wrapped behind any interface; called directly.
  See [fleet-orchestration](fleet-orchestration.md) for the full dependency
  list (EC2 Fleet API, circuit breaker, metrics).
- **`pkg/pools`** — the main caller of fleet creation for warm-pool
  replenishment; holds a narrow local interface over `*fleet.Manager`, not a
  `Provider`.
- No Kubernetes API, no Valkey, no Karpenter. These were K8s-backend-only
  dependencies and were removed along with `pkg/provider/k8s`.

## API Surface [coverage: low -- 1 source]

None. The `Provider`, `PoolProvider`, `StateStore`, and `Coordinator`
interfaces, and the `RunnerSpec`/`RunnerResult`/`RunnerState` structs that
used to live in `pkg/provider/interface.go`, no longer exist anywhere in the
codebase. The equivalent surface today is `fleet.Manager`'s own methods
(`CreateFleet`, `CreateOnDemandInstance`, `RankInstanceTypesByPrice`,
`SpotPrice`) plus the catalog functions in `pkg/fleet/instances.go` — see
[fleet-orchestration](fleet-orchestration.md) for their signatures.

## Data [coverage: low -- 0 sources]

None owned at this layer. EC2 instance state (tags, DynamoDB job/pool
records) is owned by `pkg/fleet` and `pkg/db`; there is no provider-owned
state store. The K8s-era Pod/ConfigMap/Secret/PVC resources described in the
pre-2026-06 version of this article no longer exist in any form — there is no
surviving equivalent on the EC2-only path (EC2 instances are the unit of
compute; no separate resource bundle is created per job).

## Key Decisions [coverage: medium -- 3 sources]

- **2026-06: K8s runner backend and its provider abstraction removed.** The
  K8s compute backend (`pkg/provider/k8s/`) and the `Provider` interface
  layer that sat above both backends (`pkg/provider/interface.go`) were
  deleted upstream. `pkg/fleet` became the sole, direct implementation of
  compute provisioning — EC2-only. This is a deliberate simplification, not
  an oversight: maintaining two backends (SQS FIFO vs Valkey queueing,
  stoppable EC2 instances vs non-stoppable K8s pods, IMDSv2 vs downward-API
  metadata) cost more in dual-path testing and gotchas than it returned in
  flexibility. Recorded here rather than silently dropped, per the wiki's
  history-preservation rule.
- **No interface reintroduced in EC2's place.** Rather than collapsing the
  `Provider` interface down to a single EC2 implementation (interface with
  one implementer), the abstraction was removed entirely and callers now
  depend on `pkg/fleet` concretely (or via small caller-local interfaces
  scoped to the methods they need, e.g. `pkg/pools`). This trades future
  multi-backend extensibility for less indirection today; reintroducing a
  second backend would mean re-adding an interface layer, not resurrecting
  the deleted one verbatim.
- **Historical context (pre-2026-06, now removed):** the old `Provider`
  interface leaned toward EC2's shape (`InstanceType`, `Spot`, `SubnetID`)
  while also carrying K8s-native fields (`CPUCores`, `MemoryGiB`,
  `StorageGiB`) on a shared `RunnerSpec` struct — a compromise that satisfied
  neither backend cleanly. That tension is moot now that only one backend
  exists.

## Gotchas [coverage: medium -- 2 sources]

- **Don't look for `pkg/provider`.** It doesn't exist. Old branches, forks,
  or cached documentation (including prior versions of this very article)
  referencing `Provider`, `PoolProvider`, `RunnerSpec`, or `pkg/provider/k8s`
  describe a backend that has been removed. Treat any such reference as
  historical, not current.
- **`RUNS_FLEET_MODE` and `backend=` labels are gone.** Job labels no longer
  select a compute backend; every job runs on EC2. If old workflow YAML still
  sets `backend=k8s` or similar, that token is simply ignored (or rejected)
  by the current label parser — check
  [github-integration](github-integration.md) for current label handling
  rather than assuming backend routing still exists.
- **This topic largely overlaps `fleet-orchestration` now.** With the
  abstraction gone, most substantive content about compute provisioning
  belongs in [fleet-orchestration](fleet-orchestration.md). See the
  obsolescence note left for the human maintainer in the compiler log /
  schema discussion — this article is being kept thin and pointer-like
  rather than duplicating fleet-orchestration's detail.

## Sources [coverage: medium -- 3 sources]

- [pkg/fleet/fleet.go](../../pkg/fleet/fleet.go)
- [pkg/fleet/instances.go](../../pkg/fleet/instances.go)
- [pkg/pools/manager.go](../../pkg/pools/manager.go)
