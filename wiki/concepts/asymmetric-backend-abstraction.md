---
concept: Asymmetric Backend Abstraction (EC2 vs K8s)
last_compiled: 2026-04-30
topics_connected: [compute-providers, queue-processing, agent-runtime, state-storage, warm-pools]
status: active
---

# Asymmetric Backend Abstraction (EC2 vs K8s)

## Pattern

runs-fleet ships with two compute backends — EC2 and Kubernetes — but the abstraction is **not symmetric**. There is no `pkg/provider/ec2/`. The EC2 path wires `pkg/fleet`, `pkg/db`, and SQS queues directly into the orchestrator without going through the `Provider` interface. Only the K8s path (`pkg/provider/k8s/`) implements the formal interface.

This means the `Provider` interface is a **partial abstraction**: it papers over the surface that K8s needs, while EC2 retains its native API surface. Adding a third backend would require either extending the interface or living with the same asymmetry.

## Instances

- **The interface itself** in [compute-providers](../topics/compute-providers.md): `pkg/provider/interface.go` defines `Provider` and `RunnerSpec`. `RunnerSpec` is a union: it carries both EC2 fields (`InstanceType`, `Spot`, `SubnetID`, `ForceOnDemand`) and K8s fields (`CPUCores`, `MemoryGiB`, `StorageGiB`) — a tell that the interface is more "K8s adapter" than "true abstraction."
- **State storage** in [state-storage](../topics/state-storage.md): EC2 mode uses DynamoDB (`pkg/db`) for jobs, claims, pools, and circuit state. K8s mode uses Valkey state store (`pkg/state/valkey.go`) with TxPipeline for atomic index+blob writes. Same logical entities, different substrates, different consistency models.
- **Queue substrate** in [queue-processing](../topics/queue-processing.md): EC2 uses SQS FIFO with `MessageGroupId = RunID` and visibility timeouts. K8s uses Valkey Streams with consumer groups (`XADD`, `XREADGROUP`, `XACK`). Both implement the `Queue` interface but with different ordering and ack semantics.
- **Termination** in [agent-runtime](../topics/agent-runtime.md): `EC2Terminator` calls `ec2:TerminateInstances` then SQS telemetry. `K8sTerminator` uses the in-cluster Kubernetes client to delete the pod. The agent picks one at startup based on `IsK8sEnvironment()`.
- **Warm pool reconciliation** in [warm-pools](../topics/warm-pools.md): `pools.Manager` (EC2) acquires per-pool DynamoDB locks; `pools.K8sManager` skips locks entirely because K8s API resource creation is idempotent by name. K8s also has no "stopped" state — `StartRunners` is documented as a no-op. Idle reaping happens in a process-local map (`podIdle`) that is **not** cross-replica coordinated.
- **Worker dispatch** in [internal-services](../topics/internal-services.md): `internal/worker/ec2.go` and `internal/worker/k8s.go` are sibling files; the orchestrator's `cmd/server/main.go` boots `worker.EC2WorkerDeps` or `worker.K8sWorkerDeps` based on `RUNS_FLEET_MODE`.

## What This Means

The asymmetry is **intentional but underdocumented**. Three things follow:

1. **Adding a third backend is harder than the interface implies.** The interface lets a new backend slot in for runner lifecycle, but the queue substrate, state store, lock strategy, and reconciliation semantics are all separate decisions made in separate packages. A "GCP backend" would touch at least `pkg/queue`, `pkg/state` or `pkg/db`, `pkg/pools`, and a new `pkg/provider/gcp/`.

2. **EC2 features can drift from K8s features.** Because EC2 doesn't go through the interface, an EC2-specific feature (e.g., spot interruption handling, circuit breaker, tag-based orphan detection) doesn't automatically appear in the K8s path. The K8s manager has no equivalent to the spot circuit breaker because there's no spot to break. Conversely, the K8s manager's `podIdle` reaping has no EC2 analog — pool idle timeout is enforced differently.

3. **Multi-replica K8s correctness is weaker than multi-replica EC2 correctness.** EC2 mode's per-pool DynamoDB locks (see [per-resource-locking](./per-resource-locking.md)) ensure exactly-one reconciliation per pool. K8s mode's "rely on API idempotency" works for *create*, but the in-process `podIdle` map is per-replica — two replicas may make different idle decisions about the same pod. Acceptable today (low replica count) but a footgun for scale-out.

A tighter design would either (a) push EC2 through the same `Provider` interface, accepting that the interface grows EC2-shaped methods, or (b) split the interface into two — `RunnerProvider` for compute and a separate `BackendStore` for state/queue/locks. The current code does neither, which is fine for "ship two backends" but rough for "ship four."

The honest read: this is a system that started as EC2 and grew K8s support later. The code structure is a fossil of that order, and the wiki should treat the interface as guidance (look here first for K8s-specific behavior) rather than a contract.

## Sources

- [compute-providers](../topics/compute-providers.md)
- [queue-processing](../topics/queue-processing.md)
- [agent-runtime](../topics/agent-runtime.md)
- [state-storage](../topics/state-storage.md)
- [warm-pools](../topics/warm-pools.md)
