# runs-fleet Knowledge Base

Last compiled: 2026-07-21
Total topics: 19 | Total concepts: 4 | Total sources: ~125 files (with cross-topic overlap)

Start here for codebase navigation. Each topic article synthesizes a related package or surface; each concept connects patterns across topics. Coverage tags inside each article tell you when to trust the wiki vs read raw source.

## Topics

| Topic | Also Known As | Sources | Last Updated | Status |
|-------|--------------|---------|--------------|--------|
| [project-overview](topics/project-overview.md) | README, intro, top-level, labels, cost model | 9 | 2026-07-09 | active |
| [cmd-server](topics/cmd-server.md) | orchestrator, Fargate task, server main | 6 | 2026-07-21 | active |
| [cmd-agent](topics/cmd-agent.md) | bootstrap binary, runner agent, on-instance, bootstrap timings | 7 | 2026-07-09 | active |
| [agent-runtime](topics/agent-runtime.md) | pkg/agent library, Executor, SafetyMonitor, telemetry | 10 | 2026-07-09 | active |
| [fleet-orchestration](topics/fleet-orchestration.md) | EC2 Fleet, CreateFleet, spot strategy, launch templates, DefaultFlexibleFamilies, burstable exclusion | 3 | 2026-07-21 | active |
| [warm-pools](topics/warm-pools.md) | pool reconciliation, hot/stopped/ephemeral pools (EC2-only) | 4 | 2026-07-09 | active |
| [compute-providers](topics/compute-providers.md) | ⚠️ historical — pkg/provider removed; see fleet-orchestration | 3 | 2026-07-03 | merge candidate |
| [queue-processing](topics/queue-processing.md) | SQS FIFO, message group ID (Valkey/K8s path removed) | 6 | 2026-07-03 | active |
| [job-state-machine](topics/job-state-machine.md) | runner manager, registration tokens, lifecycle, JobInfo | 7 | 2026-07-21 | active |
| [state-storage](topics/state-storage.md) | DynamoDB, circuit breaker, SSM/Vault secrets, audit table | 12 | 2026-07-21 | active |
| [github-integration](topics/github-integration.md) | webhook, HMAC, GitHub App client, registration tokens, label parser, label aliases | 3 | 2026-07-09 | active |
| [cache-service](topics/cache-service.md) | Actions cache, S3, pre-signed URLs, ACTIONS_CACHE_URL | 3 | 2026-04-30 | active |
| [events-and-termination](topics/events-and-termination.md) | EventBridge, spot warning, termination queue, re-queue, runner confirmation | 5 | 2026-07-09 | active |
| [observability](topics/observability.md) | metrics, CloudWatch, Datadog, Prometheus, slog, cost, tracing, JobStartupSeconds | 20 | 2026-07-21 | active |
| [housekeeping](topics/housekeeping.md) | cleanup tasks, orphan sweep, stale jobs, DLQ redrive | 3 | 2026-04-30 | active |
| [admin-ui](topics/admin-ui.md) | admin API, dashboard, native OIDC auth, audit viewer, cost page, metrics summary | 31 | 2026-07-21 | active |
| [config-bootstrap](topics/config-bootstrap.md) | env vars, RUNS_FLEET_*, AWS clients, timeouts | 3 | 2026-07-21 | active |
| [internal-services](topics/internal-services.md) | webhook server, worker loops, naming, validation, AWS SDK observability | 9 | 2026-07-09 | active |
| [infrastructure](topics/infrastructure.md) | Docker, Packer AMI, pre-baked images, Helm, Nix flake, Makefile | 19 | 2026-07-21 | active |

## Concepts

| Concept | Connects | Last Updated |
|---------|----------|-------------|
| [per-resource-locking](concepts/per-resource-locking.md) | warm-pools, state-storage, housekeeping | 2026-07-03 |
| [two-track-reliability](concepts/two-track-reliability.md) | project-overview, fleet-orchestration, warm-pools, events-and-termination | 2026-07-21 |
| [idempotent-retry-over-rollback](concepts/idempotent-retry-over-rollback.md) | fleet-orchestration, events-and-termination, internal-services, housekeeping, github-integration | 2026-04-30 |
| [db-record-as-rendezvous](concepts/db-record-as-rendezvous.md) | job-state-machine, state-storage, events-and-termination, observability, internal-services | 2026-07-21 |

## Recent Changes
- 2026-07-21: Incremental compile riding PR #390 — the daily cost report now computes EC2 costs from job records via the shared pkg/cost JobPricer (fixing the silent-zero bug the 2026-07-09 compile surfaced); plus #389 configurable tag values + Helm admin/OIDC/audit config. 8 topics + 2 concepts updated; see [observability](topics/observability.md) Key Decisions.
- 2026-07-09: Incremental compile after the Blacksmith-benchmark PR series — #383 (audit persistence), #384 (admin read dashboards), #385 (burstable families dropped from default selection), #386 (Docker images pre-baked into the base AMI), #387 (startup-latency metrics: JobStartupSeconds, InstanceProvisionSeconds wired, AgentBootstrapSeconds). 15 topics recompiled in parallel; most of the standing 2026-04-30 staleness debt cleared (K8s/Valkey remnants purged from fleet-orchestration, state-storage, config-bootstrap, cmd-agent, agent-runtime, observability, events-and-termination, infrastructure, project-overview). New concept [db-record-as-rendezvous](concepts/db-record-as-rendezvous.md). Compilation surfaced a live bug: `pkg/cost/getCostMetrics` queries metric names nothing publishes anymore — the daily cost report's EC2 section computes from zeros (see [observability](topics/observability.md) Gotchas).
- 2026-07-03: Admin UI auth model replaced: Keycloak-gatekeeper header trust → native OIDC (authorization-code + PKCE, HMAC-signed session cookie, no external gatekeeper required). Patched `admin-ui`'s auth-related sections directly rather than a full recompile — see [admin-ui](topics/admin-ui.md).
- 2026-07-03: Incremental compile after the K8s runner backend removal (2026-06) and the pkg/github relocation (PR #380). Rewrote `cmd-server`, `compute-providers`, `queue-processing`, `job-state-machine`, `warm-pools`, `internal-services`, `github-integration`. Flagged `compute-providers` as a merge candidate against `fleet-orchestration` — its underlying package (`pkg/provider`) no longer exists. Corrected concept count from 4 to 3: `asymmetric-backend-abstraction` was tracked in `.compile-state.json` but its file never existed on disk.
- 2026-06-19: Added config-driven custom label aliases (`RUNS_FLEET_LABEL_ALIASES`) for transparent migration of existing runners (e.g. ARC) without workflow changes. See [github-integration](topics/github-integration.md#custom-label-aliases-transparent-runner-migration).
- 2026-04-30: Initial compilation. 19 topic articles + 4 concept articles synthesized from project source. Codebase-mode first compile.

## Quick navigation by task

- **Onboarding / "what is this":** [project-overview](topics/project-overview.md)
- **Orchestrator boot or queue worker work:** [cmd-server](topics/cmd-server.md), [internal-services](topics/internal-services.md)
- **Spot strategy, instance type selection:** [fleet-orchestration](topics/fleet-orchestration.md), [two-track-reliability](concepts/two-track-reliability.md)
- **Pool config or reconciler bug:** [warm-pools](topics/warm-pools.md), [per-resource-locking](concepts/per-resource-locking.md)
- **Webhook / GitHub auth:** [github-integration](topics/github-integration.md)
- **Migrating from ARC / serving custom runner labels:** [github-integration › custom label aliases](topics/github-integration.md#custom-label-aliases-transparent-runner-migration)
- **Cost or metrics question:** [observability](topics/observability.md)
- **Startup/acquisition latency:** [observability](topics/observability.md), [db-record-as-rendezvous](concepts/db-record-as-rendezvous.md), [cmd-agent](topics/cmd-agent.md)
- **Failure handling:** [events-and-termination](topics/events-and-termination.md), [housekeeping](topics/housekeeping.md), [idempotent-retry-over-rollback](concepts/idempotent-retry-over-rollback.md)
- **Building/packaging the system:** [infrastructure](topics/infrastructure.md), [admin-ui](topics/admin-ui.md) for the embedded UI
