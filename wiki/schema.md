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
- `warm-pools`: `pkg/pools` — hot/stopped/ephemeral pool reconciliation; per-pool DynamoDB locks. EC2-only since the 2026-06 K8s removal (`pkg/pools/k8s_manager.go` deleted).
- `compute-providers`: **Obsolescence candidate — flagged 2026-07-03, not merged (needs human decision).** `pkg/provider` was removed entirely along with the K8s runner backend; EC2 fleet logic now lives directly in `pkg/fleet` with no interface layer above it. This topic is now a thin historical pointer and substantially overlaps `fleet-orchestration`.
- `queue-processing`: `pkg/queue` — SQS FIFO only. Valkey Streams (K8s path) removed 2026-06.
- `job-state-machine`: `pkg/db` (DynamoDB, sole job-state store since the Valkey/K8s removal) + `pkg/runner` (manager, registration via `pkg/github.Client`).
- `state-storage`: `pkg/db` (DynamoDB), `pkg/circuit` (per-instance-type breaker), `pkg/secrets` (SSM/Vault Store).
- `github-integration`: `pkg/github` — GitHub App API client (auth, registration tokens, workflow-job status; relocated from `pkg/runner` in PR #380) + webhook HMAC-SHA256 validation + label/alias parsing. No JIT tokens are issued anywhere in this codebase (prior schema text was inaccurate).
- `cache-service`: `pkg/cache` — GitHub Actions cache protocol over S3; HMAC token; pre-signed URLs.
- `events-and-termination`: `pkg/events` (EventBridge spot warnings), `pkg/termination` (agent telemetry consumer).
- `observability`: `pkg/metrics` (CloudWatch/Prometheus/Datadog), `pkg/logging` (slog), `pkg/cost` (S3+SNS reporter).
- `housekeeping`: `pkg/housekeeping` — orphan sweeps, stale jobs, TTL'd entities; scheduler dispatches via SQS.
- `admin-ui`: `pkg/admin` REST API + embedded Next.js UI; native OIDC auth (authorization-code + PKCE, HMAC-signed session cookie) as of 2026-07 — no external gatekeeper required.
- `config-bootstrap`: `pkg/config` — env var loading, AWS region defaulting, timeout constants.
- `internal-services`: `internal/handler` (webhook server), `internal/validation` (path traversal + Vault K8s-auth JWT path guards — unrelated to the K8s runner backend), `internal/worker` (EC2/direct/warmpool worker loops, naming — K8s worker path removed 2026-06), `internal/awsobs` (AWS SDK observability middleware: per-operation timeouts, call latency/failure metrics).
- `infrastructure`: Dockerfiles, Packer (ARM-only base + runner AMIs), Helm chart (K8s only), Nix flake, Makefile.

## Concepts

- `per-resource-locking`: DynamoDB conditional-write locks per resource (pool, task, instance claim) substitute for global leader election — connects [warm-pools, state-storage, housekeeping].
- `two-track-reliability`: Spot-first for cold-start, on-demand-only for warm pools; `ForceOnDemand=true` on retry — connects [project-overview, fleet-orchestration, warm-pools, events-and-termination].
- `idempotent-retry-over-rollback`: Forward progress with retry counters and conditional writes; housekeeping is the second half of the consistency model — connects [fleet-orchestration, events-and-termination, internal-services, housekeeping, github-integration].
- `db-record-as-rendezvous`: The DynamoDB job record joins facts from the orchestrator and agent (two machines, two clocks, no handshake) via `ReturnValueAllNew` conditional writes; positive-span guards are part of the pattern — connects [job-state-machine, state-storage, events-and-termination, observability, internal-services].

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
- 2026-07-03: Incremental compile after 152 commits / 284 changed files since last compile. Rewrote the 7 topics with structurally deleted or relocated sources: `compute-providers`, `queue-processing`, `job-state-machine`, `warm-pools`, `internal-services`, `github-integration`, `cmd-server`. Root cause of the drift: the K8s runner backend (removed 2026-06) deleted `pkg/provider/`, `pkg/state/valkey.go`, `pkg/pools/k8s_manager.go`, `pkg/queue/valkey.go`, and `internal/worker/k8s.go`; separately, `pkg/runner/github.go` was relocated to `pkg/github/client.go` (PR #380). Flagged `compute-providers` as an obsolescence/merge candidate against `fleet-orchestration` (not merged — human decision). Corrected the `asymmetric-backend-abstraction` concept, which `.compile-state.json` listed but whose file never existed on disk (schema.md's own Concepts section already correctly omitted it) — reconciled in `.compile-state.json`. The remaining 12 topics also accreted changes in this window (new features: admin cost page, Python/Ruby AL2023 runners, cache-interception v2, `pkg/tracing`) but were not touched this pass — still due for a `--full` recompile.
- 2026-07-03: Admin UI auth model replaced (Keycloak-gatekeeper header trust → native OIDC, authorization-code + PKCE, HMAC-signed session cookie; see `pkg/admin/oidc.go`, `session.go`, `handler_auth.go`, `auth.go`). Patched the `admin-ui` topic's auth-related sections directly rather than a full recompile — that topic is one of the 12 still due for `--full`.
- 2026-07-09: Incremental compile after the Blacksmith-benchmark PR series (#383 audit persistence, #384 admin read dashboards, #385 burstable exclusion from default families, #386 AMI Docker image pre-bake, #387 startup-latency metrics). Recompiled 15 topics in parallel; this pass also cleared most of the standing 2026-04-30 `--full` debt — `fleet-orchestration`, `state-storage`, `config-bootstrap`, `cmd-agent`, `agent-runtime`, `observability`, `events-and-termination`, `infrastructure`, `project-overview` all had pre-K8s-removal content purged and were rebuilt against current source. Added concept `db-record-as-rendezvous` (the PR #387 join pattern, observed independently in 5 topic articles). Updated `two-track-reliability` (old default-family lists corrected; new instance on price-optimization-vs-hardware-quality). Untouched this pass: `warm-pools` (updated), `cache-service`, `housekeeping`, `queue-processing`, `github-integration` (light update), `compute-providers` (still an obsolescence candidate awaiting human decision). Compilation surfaced a live code bug: `pkg/cost/getCostMetrics` queries metric names the rework deleted (`FleetSizeIncrement`, `JobSuccess`, `JobFailure`, `JobDuration`, dimensionless `SpotInterruptions`) — the daily cost report's EC2 section computes from zeros; recorded in `observability` Gotchas, needs a code fix.
- 2026-07-21: Incremental compile on the fix/cost-report-job-records branch (rides PR #390) covering #389 (configurable tag values + Helm admin/OIDC/audit config) and #390 (daily cost report computes EC2 costs from job records via the shared pkg/cost/jobpricing.go pricer; AdminJobFilter.Until; Family-dimensioned SpotInterruptions SEARCH; truncation surfacing). 8 topics updated (observability major — the 2026-07-09 live-bug gotcha is now a fixed historical note; config-bootstrap, infrastructure, fleet-orchestration for #389; state-storage, job-state-machine, admin-ui, cmd-server light). Concepts: db-record-as-rendezvous gained the cost-report instance; two-track-reliability's cost-report blind-spot bullet updated to fixed.
