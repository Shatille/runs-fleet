---
concept: Idempotent Retry Over Rollback
last_compiled: 2026-04-30
topics_connected: [fleet-orchestration, events-and-termination, internal-services, housekeeping, github-integration]
status: active
---

# Idempotent Retry Over Rollback

## Pattern

When something fails, runs-fleet does not roll back — it advances. Failed operations are re-queued (with a retry counter), partial successes are completed via fallback paths, and out-of-band cleanup is deferred to housekeeping. The architectural commitment: **forward progress with at-most-N retries**, gated by conditional writes that turn double-deliveries into no-ops.

This is "compensating action via the queue" rather than "transactional consistency." There is no two-phase commit, no saga coordinator, no transactional outbox. Each step is independently idempotent because the next step (or the next replica's retry) can always observe the world and decide what to do.

## Instances

- **CreateFleet → CreateTags fallback** in [fleet-orchestration](../topics/fleet-orchestration.md) (commit `c7e0a6b`): EC2 Fleet sometimes returns success but doesn't propagate tags onto spot instances. Instead of treating the request as failed, `pkg/fleet/fleet.go` issues a follow-up `CreateTags` call against the returned instance IDs. Forward fix, not rollback.
- **Spot interruption re-queue** in [events-and-termination](../topics/events-and-termination.md): on a 2-min spot warning, the events handler marks the instance terminating and re-publishes the job with `Spot=false`, `ForceOnDemand=true`, `RetryCount+1`. The original message is deleted, the job state advances. After `maxJobRetries=2` the job is marked failed (terminal forward state, not a rollback).
- **On-demand fallback** in [internal-services](../topics/internal-services.md): `internal/worker/ec2.go` `handleOnDemandFallback` deletes the original SQS message *before* re-queueing as on-demand. Documented gotcha: if the re-queue itself fails, the job is genuinely lost — the rollback would require keeping the original message, but that loses the retry counter, so the design accepts the leak in exchange for monotonic progress.
- **Conditional-write orphan marking** in [housekeeping](../topics/housekeeping.md): `markJobOrphaned` and `markJobCompleted` use `condition: status IN (running, claiming)` and treat `ConditionalCheckFailedException` as "someone else got there first — that's fine." No retry, no fail — the operation is satisfied because the world is in the desired state.
- **GitHub webhook retry on receipt** in [github-integration](../topics/github-integration.md): if `MarkJobRequeuedByJobID` returns the retry counter at the limit, the handler refuses to enqueue — the job's terminal failure state is reached forward, not by rolling back the retry counter. Conversely, no exactly-once dedup on `X-GitHub-Delivery`: the design tolerates GitHub retransmits because conditional writes downstream make them no-ops.
- **Telemetry-then-terminate ordering** in [agent-runtime](../topics/agent-runtime.md): the agent sends termination telemetry *before* calling `TerminateInstance`. If telemetry fails, the agent terminates anyway — the orchestrator will detect the orphan via housekeeping. Forward bias even on the agent.

## What This Means

The pattern produces a particular failure mode: **transient errors leave artifacts that housekeeping must clean up**. This is a deliberate trade. The alternative (transactional rollback) would require:

- A coordinator service per request (single point of failure)
- A way to undo `ec2:RunInstances` (terminate) and `github:RegisterRunner` (deregister) atomically (impossible)
- Locking across the full request span (latency hit)

Forward progress sidesteps all three by making the system **eventually correct**: any partial state has a future event that resolves it. Specifically:

| Partial state | Resolver |
|---|---|
| Tagged instance with no job | Housekeeping orphan-instance sweep |
| Job with no instance | SQS visibility timeout → re-deliver → new instance |
| Runner registered with GitHub but no instance | GitHub auto-deregisters offline runners after 14 days; or housekeeping deregisters proactively |
| SSM parameter for a terminated instance | Housekeeping `TaskStaleSecrets` |
| Job stuck in `claiming` | Housekeeping `TaskStaleJobs` (10-min threshold) → mark failed |

The design assumes **housekeeping is on**. If the scheduler stops dispatching tasks, the eventual-correctness story breaks within hours. This is why `pkg/housekeeping` is more critical than its line count suggests: it's the second half of the consistency model.

The other implication: every retry counter is a tunable. `maxJobRetries=2` means at most 3 attempts per job. Setting it to 0 turns spot interruptions into hard failures. Setting it to 10 amplifies bad-instance-type loops into 10x cost. The current value is a balance against the circuit breaker (3 interruptions per type in 15min opens the circuit), so retries align with "give the cluster a chance to find a different capacity pool."

The honest version: this is an "eventual consistency" system that hides the eventual part behind aggressive housekeeping. It works because the failure modes are well-understood and compensable, not because rollback would be wrong.

## Sources

- [fleet-orchestration](../topics/fleet-orchestration.md)
- [events-and-termination](../topics/events-and-termination.md)
- [internal-services](../topics/internal-services.md)
- [housekeeping](../topics/housekeeping.md)
- [github-integration](../topics/github-integration.md)
