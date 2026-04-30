# runs-fleet Knowledge Base

Last compiled: 2026-04-30
Total topics: 19 | Total concepts: 4 | Total sources: ~70 files

Start here for codebase navigation. Each topic article synthesizes a related package or surface; each concept connects patterns across topics. Coverage tags inside each article tell you when to trust the wiki vs read raw source.

## Topics

| Topic | Also Known As | Sources | Last Updated | Status |
|-------|--------------|---------|--------------|--------|
| [project-overview](topics/project-overview.md) | README, intro, top-level | 8 | 2026-04-30 | active |
| [cmd-server](topics/cmd-server.md) | orchestrator, Fargate task, server main | 3 | 2026-04-30 | active |
| [cmd-agent](topics/cmd-agent.md) | bootstrap binary, runner agent, on-instance | 4 | 2026-04-30 | active |
| [agent-runtime](topics/agent-runtime.md) | pkg/agent library, Executor, SafetyMonitor | 6 | 2026-04-30 | active |
| [fleet-orchestration](topics/fleet-orchestration.md) | EC2 Fleet, CreateFleet, spot strategy, launch templates | 2 | 2026-04-30 | active |
| [warm-pools](topics/warm-pools.md) | pool reconciliation, hot/stopped/ephemeral pools | 4 | 2026-04-30 | active |
| [compute-providers](topics/compute-providers.md) | backend abstraction, EC2 vs K8s, RunnerSpec | 3 | 2026-04-30 | active |
| [queue-processing](topics/queue-processing.md) | SQS FIFO, Valkey Streams, message group ID | 3 | 2026-04-30 | active |
| [job-state-machine](topics/job-state-machine.md) | runner manager, JIT token mint, lifecycle | 3 | 2026-04-30 | active |
| [state-storage](topics/state-storage.md) | DynamoDB, circuit breaker, SSM/Vault secrets | 7 | 2026-04-30 | active |
| [github-integration](topics/github-integration.md) | webhook, HMAC, GitHub App, label parser | 2 | 2026-04-30 | active |
| [cache-service](topics/cache-service.md) | Actions cache, S3, pre-signed URLs, ACTIONS_CACHE_URL | 3 | 2026-04-30 | active |
| [events-and-termination](topics/events-and-termination.md) | EventBridge, spot warning, termination queue, re-queue | 2 | 2026-04-30 | active |
| [observability](topics/observability.md) | metrics, CloudWatch, Datadog, Prometheus, slog, cost | 8 | 2026-04-30 | active |
| [housekeeping](topics/housekeeping.md) | cleanup tasks, orphan sweep, stale jobs, DLQ redrive | 3 | 2026-04-30 | active |
| [admin-ui](topics/admin-ui.md) | admin API, dashboard, Keycloak, audit log | 9 | 2026-04-30 | active |
| [config-bootstrap](topics/config-bootstrap.md) | env vars, RUNS_FLEET_*, AWS clients, timeouts | 3 | 2026-04-30 | active |
| [internal-services](topics/internal-services.md) | webhook server, worker loops, naming, validation | 8 | 2026-04-30 | active |
| [infrastructure](topics/infrastructure.md) | Docker, Packer AMI, Helm, Nix flake, Makefile | 10 | 2026-04-30 | active |

## Concepts

| Concept | Connects | Last Updated |
|---------|----------|-------------|
| [per-resource-locking](concepts/per-resource-locking.md) | warm-pools, state-storage, housekeeping | 2026-04-30 |
| [two-track-reliability](concepts/two-track-reliability.md) | project-overview, fleet-orchestration, warm-pools, events-and-termination | 2026-04-30 |
| [asymmetric-backend-abstraction](concepts/asymmetric-backend-abstraction.md) | compute-providers, queue-processing, agent-runtime, state-storage, warm-pools | 2026-04-30 |
| [idempotent-retry-over-rollback](concepts/idempotent-retry-over-rollback.md) | fleet-orchestration, events-and-termination, internal-services, housekeeping, github-integration | 2026-04-30 |

## Recent Changes
- 2026-04-30: Initial compilation. 19 topic articles + 4 concept articles synthesized from project source. Codebase-mode first compile.

## Quick navigation by task

- **Onboarding / "what is this":** [project-overview](topics/project-overview.md)
- **Orchestrator boot or queue worker work:** [cmd-server](topics/cmd-server.md), [internal-services](topics/internal-services.md)
- **Spot strategy, instance type selection:** [fleet-orchestration](topics/fleet-orchestration.md), [two-track-reliability](concepts/two-track-reliability.md)
- **Pool config or reconciler bug:** [warm-pools](topics/warm-pools.md), [per-resource-locking](concepts/per-resource-locking.md)
- **K8s mode work:** [compute-providers](topics/compute-providers.md), [asymmetric-backend-abstraction](concepts/asymmetric-backend-abstraction.md)
- **Webhook / GitHub auth:** [github-integration](topics/github-integration.md)
- **Cost or metrics question:** [observability](topics/observability.md)
- **Failure handling:** [events-and-termination](topics/events-and-termination.md), [housekeeping](topics/housekeeping.md), [idempotent-retry-over-rollback](concepts/idempotent-retry-over-rollback.md)
- **Building/packaging the system:** [infrastructure](topics/infrastructure.md)
