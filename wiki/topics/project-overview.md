---
topic: Project Overview
last_compiled: 2026-07-09
sources_count: 9
---

## Purpose [coverage: high -- 6 sources]

runs-fleet is a self-hosted, ephemeral GitHub Actions runner system that
replaces GitHub-hosted runners with spot-first EC2 instances. It targets
cost reduction (~$55-65/month vs ~$80/month for hosted runners on a
100 jobs/day workload) while preserving job-start latency via warm pools.

The system is written in Go 1.25+ and runs as a long-lived orchestrator
(deployed on ECS Fargate or a Kubernetes cluster) that consumes GitHub
webhooks, materialises workflow jobs onto ephemeral EC2 instances, and tears
them down after each job. EC2 is the only compute backend — the former
Kubernetes-pod backend (with its Valkey queue) was removed in 2026-06.

Two job-start flows: cold-start (~60s, fresh instance) and warm pool
(~10s, pre-provisioned instance). Roadmap phases 1-5 (cold-start MVP through
concurrent processing) are complete; candidate work beyond that is tracked
in [docs/ROADMAP.md](../../docs/ROADMAP.md) (alerting, cost-anomaly
detection, circuit-breaker refinement, admin RBAC, and more). Sources:
[README.md](../../README.md),
[.claude/CLAUDE.md](../../.claude/CLAUDE.md),
[docs/USAGE.md](../../docs/USAGE.md),
[docs/CONFIGURATION.md](../../docs/CONFIGURATION.md),
[docs/ROADMAP.md](../../docs/ROADMAP.md),
[flake.nix](../../flake.nix).

## Architecture [coverage: high -- 5 sources]

```
GitHub Webhook → API Gateway → SQS FIFO
                                   ↓
                Orchestrator (Go on Fargate)
                ├── Queue processors (main, pool, events)
                ├── Pool manager (warm instances)
                └── Fleet manager (EC2 API)
                                   ↓
                EC2 Spot/On-demand Fleet
                                   ↓
                Runner Instances (ephemeral)
                └── Agent binary (bootstrap, self-terminate)
```

Code layout (top-level packages from
[.claude/CLAUDE.md](../../.claude/CLAUDE.md)):

- `cmd/server/` — Fargate orchestrator entry point
- `cmd/agent/` — EC2 instance bootstrap binary
- `pkg/admin/` — Admin API and Next.js UI
- `pkg/fleet/` — EC2 fleet orchestration, spot strategy, launch templates
- `pkg/pools/` — Warm pool reconciliation (hot/stopped instances)
- `pkg/queue/` — Queue abstraction (SQS FIFO implementation)
- `pkg/github/` — GitHub App API client, webhook validation, label/alias parsing
- `pkg/db/` — DynamoDB state for jobs, pools, locks
- `pkg/cache/` — S3-backed GitHub Actions cache protocol
- `pkg/circuit/` — Circuit breaker for instance type failures
- `pkg/events/` — EventBridge events (spot interruptions)
- `pkg/housekeeping/` — Orphan/stale cleanup tasks
- `pkg/secrets/` — SSM or Vault backend abstraction
- `pkg/metrics/` — CloudWatch / Prometheus / Datadog backends
- `pkg/tracing/` — OpenTelemetry distributed tracing

Build artifacts: a single `runs-fleet-server` binary plus per-arch agent
binaries (`agent-arm64`, `agent-amd64`) declared in
[flake.nix](../../flake.nix). The server image is multi-stage: Node 22
builds the admin UI, golang:1.25-alpine cross-compiles via
`BUILDPLATFORM`/`TARGETARCH`, and alpine:3.19 is the runtime
([Dockerfile](../../Dockerfile)). Multi-arch runner images are built in
parallel via `make docker-push-runner` ([Makefile](../../Makefile)); see
[infrastructure](infrastructure.md) for the AMI pipeline behind the EC2
instances themselves.

Concurrency: multi-instance Fargate is supported through per-pool
reconciliation locks in DynamoDB (65s TTL, conditional writes). Different
pools can reconcile concurrently; the same pool serialises to one
orchestrator instance ([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).

## Talks To [coverage: high -- 5 sources]

External systems the orchestrator integrates with:

- **GitHub** — App-based auth (`go-github/v57`), webhook delivery,
  repo-level runner registration tokens (the reusable token flow — no JIT
  API usage), workflow-job status.
- **AWS EC2** — Fleet API, launch templates, spot + on-demand. SDK:
  `aws-sdk-go-v2/service/ec2`.
- **AWS SQS FIFO** — Webhook fan-out, pool batches, EventBridge events,
  termination notifications, housekeeping.
- **AWS DynamoDB** — Job state, pool config + locks, circuit breaker,
  admin audit log.
- **AWS S3** — Cache artifacts, runner configs, agent binaries.
- **AWS SSM** — Parameter Store for runner secrets (default backend).
- **AWS CloudWatch** — Metrics + logs.
- **AWS Pricing API** — Cost reporting (`pkg/cost/`).
- **AWS SNS** — Cost report notifications.
- **EventBridge** — Spot interruption 2-min warnings.
- **HashiCorp Vault** (optional) — Alternative secrets backend with
  `aws`, `kubernetes`, `approle`, or `token` auth methods.
- **OIDC provider** (optional) — Native relying-party auth for the admin
  UI (`coreos/go-oidc`).
- **Datadog DogStatsD** (optional) — Metrics backend.
- **Prometheus** (optional) — `/metrics` scrape endpoint.
- **OTLP collector** (optional) — OpenTelemetry trace export over gRPC.

Source dependency footprint visible in
[go.mod](../../go.mod): AWS SDK v2 modules,
`google/go-github/v57`, `golang-jwt/jwt/v5`, `coreos/go-oidc/v3`,
`hashicorp/vault/api`, `prometheus/client_golang`,
`DataDog/datadog-go/v5`, `go.opentelemetry.io/otel`, `oklog/ulid/v2`.
The former `redis/go-redis` and `k8s.io/*` dependencies left with the K8s
backend removal.

## API Surface [coverage: high -- 5 sources]

**HTTP endpoints (orchestrator):**

- Webhook intake (HMAC-SHA256 validated against
  `RUNS_FLEET_GITHUB_WEBHOOK_SECRET`)
- `/health` — health check (used by the Dockerfile HEALTHCHECK against
  `:8080`)
- `/metrics` — Prometheus scrape (when
  `RUNS_FLEET_METRICS_PROMETHEUS_ENABLED=true`)
- Admin API + UI (Next.js static export, gated by native OIDC auth —
  `RUNS_FLEET_ADMIN_OIDC_ISSUER_URL` and related vars, as of 2026-07),
  including `GET /api/audit-logs` when the audit table is configured
- Cache protocol endpoints (both the legacy `ACTIONS_CACHE_URL` API and
  the `actions/cache@v4`+ protocol; HMAC-authenticated, S3 pre-signed URL
  redirects). Requires `RUNS_FLEET_BASE_URL`, the externally-reachable
  HTTPS base URL served to runners.

**Workflow `runs-on` label syntax:**

```yaml
runs-on: "runs-fleet/cpu=4/arch=arm64/pool=default"
```

| Label | Meaning |
|-------|---------|
| `runs-fleet` | Runner marker (required); run_id read from webhook. Legacy `runs-fleet=<run-id>/...` still works |
| `cpu=<n>` | vCPU count (default 2); expands to a 2x range (e.g. `cpu=4` → 4-8) |
| `cpu=<min>+<max>` | Explicit vCPU range (`cpu=4+4` for exact) |
| `ram=<n>` / `ram=<min>+<max>` | RAM in GB. `ram=N` is a *minimum only* (no auto-range, unlike `cpu=`) |
| `arch=<arm64\|amd64>` | Architecture; omit to let runs-fleet pick |
| `family=<f1>+<f2>` | Instance families (e.g. `c7g+m7g`) |
| `gen=<n>` | Instance generation (1-10, e.g. `gen=8` for Graviton4) |
| `disk=<size>` | Disk in GiB (1-16384, gp3) |
| `pool=<name>` | Warm pool routing (auto-creates ephemeral pools) |
| `spot=false` | Force on-demand cold-start |

Default families when no `family=` label is given
([docs/USAGE.md](../../docs/USAGE.md)): ARM64 → c8g, m8g, r8g, c7g, m7g;
AMD64 → c6i, c7i, m6i, m7i; no arch → all of the above. Burstable families
(`t3`, `t4g`) are excluded from the defaults since PR #385 — they remain in
the catalog for explicit `family=t3`/`family=t4g` opt-in. When `arch` is
omitted, runs-fleet queries average spot prices per candidate arch, submits
only the cheaper arch's launch template to EC2 Fleet
(`price-capacity-optimized` selects within it), and falls back to `arm64` if
the price fetch fails. Ephemeral pools auto-scale `DesiredStopped` on p90
concurrent jobs over a 1-hour rolling window and delete after 4 hours of
inactivity.

**Label aliases:** `RUNS_FLEET_LABEL_ALIASES` maps externally-defined
`runs-on` labels (e.g. inherited from ARC) onto runs-fleet specs — literal or
regex rules with capture substitution — so existing workflows migrate without
edits; a matched label also becomes its own auto-created warm pool
([docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)).

## Data [coverage: high -- 5 sources]

**DynamoDB tables:**

- `runs-fleet-jobs` (`RUNS_FLEET_JOBS_TABLE`) — job lifecycle keyed by
  job_id, with optional GSIs for instance-id and pool+status lookups
  (`RUNS_FLEET_JOBS_INSTANCE_ID_GSI`, `RUNS_FLEET_JOBS_POOL_STATUS_GSI`;
  scan fallback when unset)
- `runs-fleet-pools` (`RUNS_FLEET_POOLS_TABLE`) — pool configuration plus
  per-pool `reconcile_lock_owner` / `reconcile_lock_expires` (65s TTL,
  conditional writes)
- `runs-fleet-circuit-state` (`RUNS_FLEET_CIRCUIT_BREAKER_TABLE`) —
  circuit breaker state per instance type
- `runs-fleet-audit` (`RUNS_FLEET_AUDIT_TABLE`, optional) — append-only
  admin action log with ~90-day TTL; unset disables persistence

**S3 buckets:**

- `runs-fleet-cache` (`RUNS_FLEET_CACHE_BUCKET`) — Actions cache, 30-day
  lifecycle
- `runs-fleet-config` — runner configs, agent binaries
- Cost report bucket (`RUNS_FLEET_COST_REPORT_BUCKET`)

**SQS queues:**

- Main queue (`RUNS_FLEET_QUEUE_URL`) — job requests, FIFO, batch 10,
  5-min visibility, DLQ after 3 receives
- Pool queue (`RUNS_FLEET_POOL_QUEUE_URL`) — batched warm-pool jobs
- Events queue (`RUNS_FLEET_EVENTS_QUEUE_URL`) — EventBridge / spot
  interruptions
- Termination queue (`RUNS_FLEET_TERMINATION_QUEUE_URL`)
- Housekeeping queue (`RUNS_FLEET_HOUSEKEEPING_QUEUE_URL`)
- DLQ (`RUNS_FLEET_QUEUE_DLQ_URL`)

Pool reconciliation runs on a 60s loop. Hot pools keep instances
running; stopped pools batch-start up to 50 on-demand instances on
demand; idle timeout defaults to 60 minutes
([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).

## Key Decisions [coverage: high -- 6 sources]

- **EC2 only, one orchestrator.** The Kubernetes-pod backend (and with it
  `pkg/provider/`, the Valkey queue, and the `backend=` label) was removed
  in 2026-06; EC2 fleet logic lives directly in `pkg/fleet` with no
  abstraction layer above it.
- **Spot-first cold-start, on-demand warm pool.** Cold-start jobs
  diversify across spot instance types for cost. Warm pools intentionally
  use on-demand only — stop/start reliability beats the negligible spot
  savings on a stopped fleet
  ([docs/CONFIGURATION.md](../../docs/CONFIGURATION.md),
  [docs/USAGE.md](../../docs/USAGE.md)).
- **ARM-preferred, burstable-free default families.** ARM64 (Graviton) is
  cheaper per vCPU; the defaults are c8g/m8g/r8g/c7g/m7g (ARM64) and
  c6i/c7i/m6i/m7i (AMD64). `t3`/`t4g` were dropped from the defaults in
  PR #385 after price-weighted selection made t3.medium win ~70% of
  warm-pool picks — the same "price optimization selects
  starvation-grade hardware" failure mode as the earlier RAM-floor fix
  (#376), one tier up.
- **The Blacksmith benchmark series (#385-#387, 2026-07).** An A/B benchmark
  against Blacksmith on a real repo (284 pytest tests: 70.31s vs 31.24s —
  2.25x slower on pure compute) drove three consecutive fixes: excluding
  burstables from default family selection (#385), pre-baking common Docker
  images into the base AMI so jobs stop paying ~25s of `mysql:8.0` pull per
  run (#386, see [infrastructure](infrastructure.md)), and new
  `JobStartupSeconds` / `AgentBootstrapSeconds` / `InstanceProvisionSeconds`
  metrics so runner-acquisition latency regressions surface on dashboards
  rather than via external benchmarking (#387).
- **Ephemeral instances, no reuse.** Each job gets a fresh runner that
  self-terminates — no state accumulation, no cross-tenant leakage.
- **Bounded spot diversification.** `cpu=N` expands to `[N, 2N]` vCPUs by
  default to widen the spot pool without over-provisioning. Explicit
  `cpu=N+N` opts out.
- **`price-capacity-optimized` allocation strategy.** EC2 Fleet selects
  cheapest-with-capacity from the diversified pool (within the single arch
  chosen up front when `arch` is omitted).
- **Per-pool distributed locks (DynamoDB conditional writes), no global
  leader election.** Multi-instance Fargate scales horizontally; same
  pool serialises ([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).
- **Eventual consistency tolerated.** DynamoDB, SQS, EC2 API all
  eventually consistent; design accommodates with retries and idempotent
  reconciliation.
- **Static export admin UI inside the server binary.** Single Docker
  image; UI is built at image-build time and embedded
  ([Dockerfile](../../Dockerfile)).

## Gotchas [coverage: medium -- 4 sources]

- **Cost reporting is approximate.** `pkg/cost/` hard-codes pricing for
  only 3 instance families (t4g, c7g, m7g), assumes a fixed 70% spot
  discount, ignores regional price variation, and excludes data transfer
  and S3 request costs ([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).
- **Family-less `gen=3` (amd64) and `gen=4` (arm64) now error.** Those
  generations contained only burstable families, so after #385 dropped
  `t3`/`t4g` from the defaults, such requests resolve to zero matching
  instance types unless a `family=` label opts back in.
- **gp3 disk costs scale linearly.** ~$0.08/GB-month — a 1 TB
  `disk=1024` adds ~$80/month, dwarfing compute cost
  ([docs/USAGE.md](../../docs/USAGE.md)).
- **Spot interruption is best-effort recovery.** EventBridge gives 2 min
  warning; the in-progress job is re-queued to a new instance, but the
  current step is lost.
- **`spot=false` only affects cold-start, and only the literal lowercase
  `false` counts.** Warm pool jobs are always on-demand by design;
  `spot=False` / `spot=0` do not disable spot
  ([docs/USAGE.md](../../docs/USAGE.md)).
- **`ram=N` has no upper bound.** Unlike `cpu=N` (auto 2x range), a bare
  `ram=` value is a minimum only; use `ram=min+max` for a bounded range.
- **Webhook validation is strict.** HMAC-SHA256 on every request; the
  webhook secret must match GitHub App settings exactly. IMDSv2 is
  required on EC2 ([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).
- **Secrets-in-env are deprecated.** Runner secrets belong in SSM or
  Vault; only orchestrator config is in env vars.
- **Default region is ap-northeast-1.** Both
  [Makefile](../../Makefile) and [flake.nix](../../flake.nix) hard-code
  Tokyo as the default `AWS_REGION`; there is no per-job region label or
  cross-region fallback (tracked in
  [docs/ROADMAP.md](../../docs/ROADMAP.md)).
- **Unanchored alias regexes match substrings.** `RUNS_FLEET_LABEL_ALIASES`
  regex rules use partial-match semantics; anchor with `^…$` or `ci-(\d+)x`
  will also match `legacy-ci-8x-runner`. Regex-rule specs with captures only
  validate at job time, not startup.
- **First-time Nix build needs `npmDepsHash` update.** The placeholder
  hash in [flake.nix](../../flake.nix) for the admin UI must be replaced
  with the real hash after the first build.

## Sources [coverage: high]

- [README.md](../../README.md)
- [.claude/CLAUDE.md](../../.claude/CLAUDE.md)
- [docs/USAGE.md](../../docs/USAGE.md)
- [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)
- [docs/ROADMAP.md](../../docs/ROADMAP.md)
- [Dockerfile](../../Dockerfile)
- [Makefile](../../Makefile)
- [flake.nix](../../flake.nix)
- [go.mod](../../go.mod)
