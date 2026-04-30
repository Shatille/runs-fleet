# Wiki Schema

This file defines the structure and conventions for the runs-fleet codebase wiki. It is generated on first compile and co-evolved between human and LLM on subsequent runs.

**Human:** You can edit this file to rename topics, merge them, add conventions, or change the article structure. The compiler will respect your changes on the next run.

**Compiler:** Read this file before classifying sources. Follow its conventions. Add new topics here when discovered. Never remove topics without human approval.

## Topics

- `project-overview`: Top-level briefing — README, USAGE, CONFIGURATION, architecture, label syntax, cost model.
- `cmd-server`: Fargate orchestrator entry point (`cmd/server/main.go`) — boot sequence, workers, HTTP handlers.
- `cmd-agent`: On-instance bootstrap binary (`cmd/agent/main.go`) — fetch config, register runner, execute, self-terminate.
- `agent-runtime`: `pkg/agent` library — Downloader, Executor, SafetyMonitor, telemetry, terminator.
- `fleet-orchestration`: `pkg/fleet` — EC2 Fleet API, launch templates, spot strategy, tag propagation.
- `warm-pools`: `pkg/pools` — hot/stopped/ephemeral pool reconciliation; per-pool DynamoDB locks.
- `compute-providers`: `pkg/provider` — backend abstraction; K8s implementation; EC2 path lives in pkg/fleet (asymmetric).
- `queue-processing`: `pkg/queue` — SQS FIFO (EC2) and Valkey Streams (K8s) under a common interface.
- `job-state-machine`: `pkg/state` (Valkey) + `pkg/runner` (manager + GitHub registration); EC2 state lives in pkg/db.
- `state-storage`: `pkg/db` (DynamoDB), `pkg/circuit` (per-instance-type breaker), `pkg/secrets` (SSM/Vault Store).
- `github-integration`: `pkg/github` webhook receiver, HMAC-SHA256, JIT tokens, label parser.
- `cache-service`: `pkg/cache` — GitHub Actions cache protocol over S3; HMAC token; pre-signed URLs.
- `events-and-termination`: `pkg/events` (EventBridge spot warnings), `pkg/termination` (agent telemetry consumer).
- `observability`: `pkg/metrics` (CloudWatch/Prometheus/Datadog), `pkg/logging` (slog), `pkg/cost` (S3+SNS reporter).
- `housekeeping`: `pkg/housekeeping` — orphan sweeps, stale jobs, TTL'd entities; scheduler dispatches via SQS.
- `admin-ui`: `pkg/admin` REST API + embedded Next.js UI; Keycloak gatekeeper auth model.
- `config-bootstrap`: `pkg/config` — env var loading, AWS region defaulting, timeout constants.
- `internal-services`: `internal/handler` (webhook server), `internal/validation` (path traversal guards), `internal/worker` (EC2/K8s/direct/warmpool worker loops, naming).
- `infrastructure`: Dockerfiles, Packer (ARM-only base + runner AMIs), Helm chart (K8s only), Nix flake, Makefile.

## Concepts

- `per-resource-locking`: DynamoDB conditional-write locks per resource (pool, task, instance claim) substitute for global leader election — connects [warm-pools, state-storage, housekeeping].
- `two-track-reliability`: Spot-first for cold-start, on-demand-only for warm pools; `ForceOnDemand=true` on retry — connects [project-overview, fleet-orchestration, warm-pools, events-and-termination].
- `asymmetric-backend-abstraction`: Provider interface only wraps K8s; EC2 wires pkg/fleet directly. Multi-replica K8s correctness is weaker — connects [compute-providers, queue-processing, agent-runtime, state-storage, warm-pools].
- `idempotent-retry-over-rollback`: Forward progress with retry counters and conditional writes; housekeeping is the second half of the consistency model — connects [fleet-orchestration, events-and-termination, internal-services, housekeeping, github-integration].

## Article Structure

Codebase-mode topic articles follow this format (configured in `.wiki-compiler.json`):
- **Purpose** [coverage] — what this module/service does and who depends on it
- **Architecture** [coverage] — key files, structure, entry points
- **Talks To** [coverage] — dependencies, communication patterns
- **API Surface** [coverage] — endpoints, exported functions, interfaces
- **Data** [coverage] — tables, queues, caches, state owned
- **Key Decisions** [coverage] — why it was built this way
- **Gotchas** [coverage] — known issues, edge cases, failure modes
- **Sources** — backlinks to all contributing files

Concept articles follow:
- **Pattern** — 1-2 paragraphs describing the cross-cutting pattern
- **Instances** — concrete occurrences with topic links
- **What This Means** — synthesis (the "so what")
- **Sources** — topic backlinks

Coverage tags: `[coverage: high -- N sources]` (5+), `[coverage: medium -- N sources]` (2-4), `[coverage: low -- N sources]` (0-1).

## Naming Conventions

- Topic slugs: lowercase-kebab-case (`fleet-orchestration`, `cmd-server`).
- Files: `{slug}.md` in `topics/` or `concepts/`.
- Dates: YYYY-MM-DD format.
- Links: markdown style with relative paths from `topics/` or `concepts/` (e.g., `[pkg/fleet/fleet.go](../../pkg/fleet/fleet.go)`).

## Cross-Reference Rules

- Topics that share 3+ sources or a documented dependency reference each other in Talks To or Key Decisions.
- Concept articles backlink to every topic they touch; topics may optionally cite concepts in Key Decisions.
- Decisions affecting multiple topics are noted in each relevant topic's Key Decisions.

## Evolution Log

- 2026-04-30: Initial schema generated from 19 topics, 4 concepts. Codebase-mode first compile.
