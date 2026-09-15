# runs-fleet Knowledge Base

Last compiled: 2026-09-15
Total topics: 21 | Total concepts: 5 | Total sources: ~186 files (with cross-topic overlap)

Start here for codebase navigation. Each topic article synthesizes a related package or surface; each concept connects patterns across topics. Coverage tags inside each article tell you when to trust the wiki vs read raw source.

## Topics

| Topic | Also Known As | Sources | Last Updated | Status |
|-------|--------------|---------|--------------|--------|
| [project-overview](topics/project-overview.md) | README, intro, top-level, labels, cost model, AGENTS.md, cost caveats | 11 | 2026-09-15 | active |
| [cmd-server](topics/cmd-server.md) | orchestrator, Fargate task, server main, boot sequence | 8 | 2026-09-15 | active |
| [cmd-agent](topics/cmd-agent.md) | bootstrap binary, runner agent, on-instance, standby mode, bootstrap timings, DOTNET_INSTALL_DIR | 12 | 2026-09-15 | active |
| [agent-runtime](topics/agent-runtime.md) | pkg/agent library, Executor, SafetyMonitor, telemetry, log shipping | 12 | 2026-08-21 | active |
| [fleet-orchestration](topics/fleet-orchestration.md) | EC2 Fleet, CreateFleet, spot strategy, launch templates, instance catalog, AMI resolution | 4 | 2026-08-21 | active |
| [warm-pools](topics/warm-pools.md) | pool reconciliation, hot/stopped/ephemeral pools, hot-pool auto-tuner, linger | 8 | 2026-08-21 | active |
| [compute-providers](topics/compute-providers.md) | ⚠️ historical — pkg/provider removed; see fleet-orchestration | 3 | 2026-07-03 | merge candidate |
| [queue-processing](topics/queue-processing.md) | SQS FIFO, message group ID (Valkey/K8s path removed) | 6 | 2026-07-03 | active |
| [job-state-machine](topics/job-state-machine.md) | runner manager, JIT config, registration, lifecycle, requeue, job theft | 20 | 2026-08-21 | active |
| [state-storage](topics/state-storage.md) | DynamoDB, sentinel prefixes, circuit breaker, SSM/Vault secrets, audit table | 24 | 2026-09-15 | active |
| [github-integration](topics/github-integration.md) | webhook, HMAC, GitHub App client, JIT config, runner deregistration, label aliases | 5 | 2026-08-21 | active |
| [cache-service](topics/cache-service.md) | Actions cache, S3, pre-signed URLs, ACTIONS_CACHE_URL | 3 | 2026-04-30 | active |
| [events-and-termination](topics/events-and-termination.md) | EventBridge, spot warning, termination queue, re-queue, workflow re-run | 6 | 2026-08-21 | active |
| [observability](topics/observability.md) | metrics, CloudWatch (**off by default**), Datadog, Prometheus, slog, cost, fleet cost attribution, report timezone, tracing | 40 | 2026-09-15 | active |
| [housekeeping](topics/housekeeping.md) | cleanup tasks, orphan sweep, stale jobs, stale AMI, orphaned runners, DLQ redrive, fleet-cost sampler | 14 | 2026-09-15 | active |
| [admin-ui](topics/admin-ui.md) | admin API, dashboard, native OIDC auth, audit viewer, cost page, hung jobs, AMI replacement | 34 | 2026-08-21 | active |
| [config-bootstrap](topics/config-bootstrap.md) | env vars, RUNS_FLEET_*, AWS clients, timeouts, report timezone, hot-pool toggle | 5 | 2026-08-21 | active |
| [internal-services](topics/internal-services.md) | webhook server, worker loops, naming, validation, AWS SDK observability | 9 | 2026-08-21 | active |
| [infrastructure](topics/infrastructure.md) | Docker, Packer AMI (dual-arch), pre-baked tool cache, Helm, Nix flake, Trivy gate, CI workflows, .NET install dir | 30 | 2026-09-15 | active |
| [registry-mirroring](topics/registry-mirroring.md) | mirror proxy, ECR pull-through cache, Docker Hub 429, registry-mirrors, buildkitd.toml | 14 | 2026-08-21 | active |
| [build-caching](topics/build-caching.md) | buildx shim, transparent layer cache, type=s3, CLI plugin, buildx pin | 19 | 2026-08-21 | active |

## Concepts

| Concept | Connects | Last Updated |
|---------|----------|-------------|
| [per-resource-locking](concepts/per-resource-locking.md) | warm-pools, state-storage, housekeeping, observability | 2026-08-21 |
| [absent-is-not-zero](concepts/absent-is-not-zero.md) | observability, admin-ui, state-storage, housekeeping | 2026-09-15 |
| [two-track-reliability](concepts/two-track-reliability.md) | project-overview, fleet-orchestration, warm-pools, events-and-termination | 2026-07-21 |
| [db-record-as-rendezvous](concepts/db-record-as-rendezvous.md) | job-state-machine, state-storage, events-and-termination, observability, internal-services | 2026-07-21 |
| [idempotent-retry-over-rollback](concepts/idempotent-retry-over-rollback.md) | fleet-orchestration, events-and-termination, internal-services, housekeeping, github-integration | 2026-04-30 |

## Recent Changes

- 2026-09-15: Incremental compile on the `fix/runner-dotnet-install-dir` branch, covering PRs #457/#458/#459 and the unmerged .NET fix. 7 topics + 1 concept updated. **Runners now support `actions/setup-dotnet`** (c4c3b01): `/opt/dotnet` pre-created ec2-user-writable in the base layer, `DOTNET_INSTALL_DIR` exported from the agent unit. The notable asymmetry — `setup-dotnet` performs **no tool-cache lookup at all**, so the `/opt/hostedtoolcache` prebake that serves setup-python/setup-ruby/setup-go/setup-node cannot apply; every .NET job downloads its SDK, and because `/opt/dotnet` sits outside `AGENT_TOOLSDIRECTORY` the `ToolCacheMisses` metric has a blind spot for it. Also recorded: #458's cost-pricing rebuild (per-vCPU rate per family replacing a 3-family table that mispriced 75% of jobs; the fleet sampler had been built with nil pricers since #455; the fallback latch never lifted), #457's orphan sweep (16,000 consecutive failures over 16h from 250 legacy `pool=""` rows unrepresentable as a GSI key — plus the `return nil` that hid it), and #459's CloudWatch-agent removal. Four source-side doc drifts surfaced and left unfixed — see below.
- 2026-09-15: **Doc drifts found during compile (unfixed in code, all pre-existing).** (1) `pkg/housekeeping/tasks.go` `SetFleetPricers` doc comment still says the fallback table "covers only t4g/c7g/m7g" — #458 replaced that in the same PR. (2) `AGENTS.md:354-355` carries the same stale three-family claim; #458 updated `.claude/claude.md` but not `AGENTS.md`. (3) `packer/provision-base.sh:876` still echoes `- CloudWatch Agent: enabled` in the build summary though #459 removed the package, both configs, and the `systemctl enable`. (4) `pkg/cost/fleetpricing.go:11` `EBSGiBMonthRate` comment says "us-east-1" while the family table it references is ap-northeast-1. All four are comments/output only — no behavioural impact.
- 2026-08-21: Large incremental compile — ~100 commits (PRs #418–#456) since the last pass, 54 of 96 tracked sources changed plus 89 new files. **18 of 21 topics rewritten**; `cache-service`, `queue-processing`, and `compute-providers` untouched (no source churn). Two new topics for subsystems that did not exist at the last compile: [registry-mirroring](topics/registry-mirroring.md) (the host-local Docker Hub mirror over an ECR pull-through cache — #439/#440/#441/#444/#453) and [build-caching](topics/build-caching.md) (the transparent buildx layer-cache shim — #394/#396). New concept [absent-is-not-zero](concepts/absent-is-not-zero.md). Three schema corrections: JIT configs *are* minted here (`pkg/github/jitconfig.go`, #424) though they do **not** bind a runner to a job; housekeeping schedules on in-process timers with a distributed task lock, not SQS dispatch; the Packer pipeline is dual-arch, not ARM-only. Compilation surfaced a live doc bug — see below.
- 2026-08-21: **Doc bug found during compile (unfixed in code).** `pkg/github/jitconfig.go:41-48` correctly states that a JIT config makes a runner ephemeral but does **not** bind it to a job — dispatch stays label-matched. PR #424 shipped the opposite claim in four files; PR #430 corrected only `jitconfig.go`. `pkg/runner/manager.go`, `pkg/secrets/store.go`, and `pkg/agent/executor.go` still assert the false job-bound property, which misrepresents why the requeue sweeps exist. See [job-state-machine](topics/job-state-machine.md) Gotchas.
- 2026-08-21: `packer/README.md` is stale **at HEAD** — it still describes `buildkitd.toml` being rendered at boot by a `runs-fleet-mirror-buildkitd-config` systemd unit. Neither exists; #453 replaced that with the proxy writing the file directly via `reconcileBuildkitdConfig`. See [registry-mirroring](topics/registry-mirroring.md) Gotchas.
- 2026-07-21: Incremental compile riding PR #390 — the daily cost report now computes EC2 costs from job records via the shared pkg/cost JobPricer (fixing the silent-zero bug the 2026-07-09 compile surfaced); plus #389 configurable tag values + Helm admin/OIDC/audit config. 8 topics + 2 concepts updated.
- 2026-07-09: Incremental compile after the Blacksmith-benchmark PR series (#383–#387). 15 topics recompiled; most 2026-04-30 staleness debt cleared. New concept [db-record-as-rendezvous](concepts/db-record-as-rendezvous.md). Surfaced a live bug: `pkg/cost/getCostMetrics` queried metric names nothing published anymore (fixed by #390).
- 2026-07-03: Admin UI auth model replaced: Keycloak-gatekeeper header trust → native OIDC (authorization-code + PKCE, HMAC-signed session cookie).
- 2026-07-03: Incremental compile after the K8s runner backend removal (2026-06) and the pkg/github relocation (#380). Flagged `compute-providers` as a merge candidate against `fleet-orchestration` — still awaiting a human decision.
- 2026-04-30: Initial compilation. 19 topic articles + 4 concept articles. Codebase-mode first compile.

## Quick navigation by task

- **Onboarding / "what is this":** [project-overview](topics/project-overview.md)
- **Orchestrator boot or queue worker work:** [cmd-server](topics/cmd-server.md), [internal-services](topics/internal-services.md)
- **Spot strategy, instance type selection:** [fleet-orchestration](topics/fleet-orchestration.md), [two-track-reliability](concepts/two-track-reliability.md)
- **Pool config or reconciler bug:** [warm-pools](topics/warm-pools.md), [per-resource-locking](concepts/per-resource-locking.md)
- **Webhook / GitHub auth / runner registration:** [github-integration](topics/github-integration.md)
- **Runners stealing each other's jobs:** [job-state-machine](topics/job-state-machine.md), [github-integration](topics/github-integration.md)
- **Migrating from ARC / serving custom runner labels:** [github-integration › custom label aliases](topics/github-integration.md#custom-label-aliases-transparent-runner-migration)
- **Cost or metrics question:** [observability](topics/observability.md), [absent-is-not-zero](concepts/absent-is-not-zero.md)
- **Startup/acquisition latency:** [observability](topics/observability.md), [db-record-as-rendezvous](concepts/db-record-as-rendezvous.md), [cmd-agent](topics/cmd-agent.md)
- **Failure handling:** [events-and-termination](topics/events-and-termination.md), [housekeeping](topics/housekeeping.md), [idempotent-retry-over-rollback](concepts/idempotent-retry-over-rollback.md)
- **Building/packaging the system:** [infrastructure](topics/infrastructure.md), [admin-ui](topics/admin-ui.md) for the embedded UI
- **Docker Hub 429 / image pull slowness on runners:** [registry-mirroring](topics/registry-mirroring.md)
- **Slow Docker builds / layer cache misses:** [build-caching](topics/build-caching.md)
