---
concept: DB Record as Cross-Instance Rendezvous
last_compiled: 2026-07-21
topics_connected: [job-state-machine, state-storage, events-and-termination, observability, internal-services]
status: active
---

# DB Record as Cross-Instance Rendezvous

## Pattern

runs-fleet's control plane (Fargate orchestrator) and data plane (EC2 agents) never talk to each other directly and share no clock, no version handshake, and no session. When a computation needs facts from both sides — "when was this job assigned?" (orchestrator knows) joined with "when did the runner register?" (agent knows) — the DynamoDB job record is the rendezvous point: each side writes the attributes it owns, and whichever side arrives second reads the joined row and completes the computation.

The pattern's fingerprint is `ReturnValueAllNew` on conditional updates: the writer of the *second* fact gets the full record back in the same round-trip, so the join costs zero extra reads. Guards (`seconds > 0`, zero-timestamp checks, `omitempty` fields) absorb the residual problems of joining facts from two clocks and two software versions.

## Instances

- **2026-07** in [observability](../topics/observability.md) / [events-and-termination](../topics/events-and-termination.md): `InstanceProvisionSeconds` (PR #387) joins the orchestrator-stamped `created_at` with the agent-reported `StartedAt` at `confirmRunnerStarted`, via `MarkJobStarted`'s `ReturnValueAllNew` response. This resolved a months-old TODO in `internal/worker/ec2.go` that had declared the metric impossible ("no cross-instance launch+ready pair is readily available") — the pair was always available; the record was the missing rendezvous.
- **2026-07** in [internal-services](../topics/internal-services.md): the `in_progress` webhook path resolves a job's warm/cold `Source` dimension by joining GitHub's payload (timestamps, labels) with the job record's `WarmPoolHit` via one `GetJobByJobID` read — GitHub's clock provides the duration, the record provides the classification.
- **Ongoing** in [job-state-machine](../topics/job-state-machine.md) / [state-storage](../topics/state-storage.md): the launched→running transition itself is the same shape — the agent's "started" telemetry message triggers a conditional update that only succeeds if the orchestrator previously wrote `status=launched`, making the record the arbiter of which side's view of the world wins.
- **Ongoing** in [events-and-termination](../topics/events-and-termination.md): agent version skew is handled by the record schema, not negotiation — additive `omitempty` telemetry fields mean an old agent's message simply lacks facts, and every consumer treats zero-as-absent. There is no version handshake anywhere in the system.
- **2026-07** in [observability](../topics/observability.md): the daily cost report (PR #390) abandoned its CloudWatch-metrics source — which had silently zeroed after a taxonomy rework — for the job records themselves, priced per job via the same `JobPricer` the admin cost page uses. The records-over-derived-signals move made the report both correct and self-consistent with the dashboard, and the failure mode it fixed (queries against retired metric names returning empty, not erroring) is exactly the fragility the record source doesn't have.

## What This Means

The system consistently converts "distributed coordination problem" into "eventually-joined row." That is the same move [per-resource-locking](per-resource-locking.md) makes for mutual exclusion and [idempotent-retry-over-rollback](idempotent-retry-over-rollback.md) makes for failure recovery — DynamoDB conditional writes are the only coordination primitive in the architecture, used for locks, state transitions, and now metric joins.

The practical lesson from PR #387: when a cross-instance computation looks impossible ("the two timestamps live on different machines"), check whether both facts already flow through the jobs table — they usually do, because every lifecycle event already writes there. The anti-pattern this avoids is adding a second coordination channel (agent calling EC2 APIs, orchestrator polling instances, version negotiation) for data the record already carries.

The standing caveat is clocks: any join of orchestrator-written and agent-written timestamps crosses two NTP domains. Every instance of the pattern guards with positive-span checks rather than trusting the arithmetic — the guard is part of the pattern, not optional hygiene.

## Sources

- [job-state-machine](../topics/job-state-machine.md)
- [state-storage](../topics/state-storage.md)
- [events-and-termination](../topics/events-and-termination.md)
- [observability](../topics/observability.md)
- [internal-services](../topics/internal-services.md)
