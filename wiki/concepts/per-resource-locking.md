---
concept: Per-Resource DynamoDB Locking
last_compiled: 2026-04-30
topics_connected: [warm-pools, state-storage, housekeeping]
status: active
---

# Per-Resource DynamoDB Locking

## Pattern

runs-fleet runs as a multi-instance Fargate service with no ZooKeeper, etcd, or external coordinator. Instead, every operation that must not run concurrently across replicas uses a per-resource DynamoDB conditional-write lock: each lock has an `*_lock_owner` (UUID), an `*_lock_expires` (epoch seconds), and a TTL chosen to outrun the operation's worst case. Acquisition uses `attribute_not_exists(...) OR <expires-attr> < :now`; release is best-effort. Crashed owners are tolerated because the lock simply expires.

This is "fan-out coordination via DynamoDB" — not a single global mutex. Two replicas can reconcile two different pools simultaneously, but never the same pool.

## Instances

- **Pool reconciliation** in [warm-pools](../topics/warm-pools.md): each pool row carries `reconcile_lock_owner` / `reconcile_lock_expires` with a 65s TTL (greater than the 60s reconcile interval). `Manager.AcquirePoolReconcileLock` blocks duplicate reconciliation across Fargate replicas without forcing a global leader.
- **Instance claims** in [state-storage](../topics/state-storage.md): `pkg/db/instance_claims.go` reuses the pools table with a synthetic `__instance_claim:<instanceID>` partition key to claim a warm-pool instance for a job atomically. Returns `ErrInstanceAlreadyClaimed` on contention.
- **Job claims** in [state-storage](../topics/state-storage.md): `ClaimJob` on the jobs table prevents two workers from picking the same SQS message twice (defense in depth on top of FIFO).
- **Task locks** in [housekeeping](../topics/housekeeping.md): `taskLockTTL = 6m` per task type prevents two replicas from running the same orphan sweep concurrently.
- **Circuit breaker writes** in [state-storage](../topics/state-storage.md): per-instance-type rows use conditional writes with TTL-based auto-reset; not a "lock" but the same conditional-write pattern.

## What This Means

The architectural payoff is operational simplicity: there is no leader election, no consensus protocol, no failover dance. Replicas are stateless and identical. A Fargate task can die mid-reconcile and the lock expires within 65s, after which any other replica picks up the work. This pushes "what happens during a deploy" from a hard problem to a non-event — rolling deploys just work.

The cost is that **every TTL is a tunable**. Set the lock TTL too short and a slow reconcile runs concurrently with itself (thrash). Set it too long and a crashed owner blocks recovery. The current TTL inventory worth knowing:

| Lock | TTL | Operation |
|------|-----|-----------|
| Pool reconcile | 65s | 60s reconcile loop |
| Housekeeping task | 6m | 4m per-task timeout |
| Instance claim | (per-job) | Until job completes or crashes |
| Job claim | (per-job) | Until terminal status set |

The K8s backend deliberately skips this entire pattern because the K8s API itself provides idempotent create-by-name semantics — a different consensus substrate, not a different correctness model.

The pattern is also why this codebase's DynamoDB schema is an unusual mix of "real" rows and synthetic-PK rows (instance claims piggyback on the pools table). It works, but it's a maintenance trap: anyone adding a new claim type needs to know the table-reuse convention or they'll create a parallel table for what should be one row.

## Sources

- [warm-pools](../topics/warm-pools.md)
- [state-storage](../topics/state-storage.md)
- [housekeeping](../topics/housekeeping.md)
