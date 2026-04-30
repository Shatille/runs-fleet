---
concept: Two-Track Reliability (Spot for Cold-Start, On-Demand for Warm Pools)
last_compiled: 2026-04-30
topics_connected: [project-overview, fleet-orchestration, warm-pools, events-and-termination]
status: active
---

# Two-Track Reliability (Spot for Cold-Start, On-Demand for Warm Pools)

## Pattern

runs-fleet maintains two distinct reliability strategies for the same workload:

- **Cold-start path** is spot-first with on-demand fallback. The CLAUDE.md cost target (~$15-20/mo for 100 jobs/day) hinges on the ~70% spot discount. The job is willing to be interrupted because a 2-min spot warning re-queues it onto a fresh instance.
- **Warm-pool path** is on-demand only. The CLAUDE.md design principle states this explicitly: "stop/start reliability over negligible spot savings." Warm pools are the latency-sensitive path (~10s start), and stopped instances cannot be on spot — so the system avoids the operational complexity of mixing capacity types in a pool.

This is unusual. The default industry pattern is "spot everywhere" with diversification. runs-fleet deliberately accepts a higher unit cost on the path that already costs more (running idle instances) in exchange for predictability.

## Instances

- **Cold-start spot strategy** in [fleet-orchestration](../topics/fleet-orchestration.md): `pkg/fleet/fleet.go` `shouldUseSpot()` decides spot eligibility, `buildLaunchTemplateConfigs` produces price-capacity-optimized fleets across `DefaultFlexibleFamilies` (c7g/m7g/t4g for ARM, c6i/c7i/m6i/m7i/t3 for AMD), and `cpu=N` defaults to a 2x range (N to 2N vCPUs) for spot diversification.
- **Warm-pool on-demand-only** in [warm-pools](../topics/warm-pools.md): `Manager.createPoolFleetInstances` always calls `CreateOnDemandInstance` (not the spot Fleet path); pool reconciler maintains `current_running` / `current_stopped` against `desired_*` configured per pool.
- **Spot interruption recovery** in [events-and-termination](../topics/events-and-termination.md): EventBridge spot warning → `MarkInstanceTerminating` → re-queue with `Spot=false`, `ForceOnDemand=true`, `RetryCount+1`. Once a job has been bitten, it is upgraded to on-demand for its retry. After `maxJobRetries=2`, the job is marked failed.
- **Circuit breaker per instance type** in [state-storage](../topics/state-storage.md): `circuit.Breaker` opens after 3 spot interruptions in 15min for a given instance type; cooldown 30min. This protects against capacity-pool exhaustion that spot-first would otherwise hide.
- **`spot=false` label** documented in [project-overview](../topics/project-overview.md): users can opt their cold-start jobs into on-demand explicitly. Warm pool jobs ignore the flag — they are always on-demand by design.

## What This Means

The split reflects a strict mental model: **interruption tolerance is a property of the job, not the cluster**. Cold-start jobs accept interruption because their re-queue path is cheap; warm-pool jobs cannot accept interruption because the *pool itself* is the latency budget — losing an instance mid-job means re-running the cold-start dance.

The decision shows up subtly in the code:
- The 2x default CPU range exists *only* for spot path diversification. If you change spot policy to single-AZ or single-instance-type, the range becomes wasteful.
- The `ForceOnDemand=true` re-queue in `pkg/events/handler.go` is what makes the system progress monotonically: a job downgrades from spot → on-demand on retry, never the reverse. This means a hot capacity pool degrades the cluster's spot ratio, not its success rate.
- The cost reporter's hardcoded "70% spot discount" assumption (`pkg/cost/pricing.go`) is calibrated to the cold-start path. When warm-pool usage grows, the report under-reports actual cost — they're paying full on-demand on warm-pool runtime.

The choice deliberately leaves money on the table on the warm-pool path. The trade is correct: a warm pool that flips to spot would face a continuous reliability problem (on-demand instances cannot transition to/from spot capacity), whereas a cold-start fleet that occasionally takes an interruption pays only the re-queue cost.

The honest version of the cost story: this is a cluster that gets aggressive spot savings when it's idle-busy and pays a premium when it's spiky-busy. Most CI workloads are spiky, so warm pools amortize across spikes, and the on-demand premium is the price of a 6x latency improvement.

## Sources

- [project-overview](../topics/project-overview.md)
- [fleet-orchestration](../topics/fleet-orchestration.md)
- [warm-pools](../topics/warm-pools.md)
- [events-and-termination](../topics/events-and-termination.md)
