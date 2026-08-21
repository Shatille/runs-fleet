---
topic: Project Overview
last_compiled: 2026-08-21
sources_count: 9
---

# Project Overview

## Purpose [coverage: high -- 6 sources]

runs-fleet is a self-hosted, ephemeral GitHub Actions runner system that
replaces GitHub-hosted runners with spot-first EC2 instances. It targets
cost reduction (~$55-65/month vs ~$80/month for hosted runners on a
100 jobs/day workload) while preserving job-start latency via warm pools.

The system is written in Go (`go 1.25.12`, `toolchain go1.26.6` in
[go.mod](../../go.mod)) and runs as a long-lived orchestrator that consumes
GitHub webhooks, materialises workflow jobs onto ephemeral EC2 instances, and
tears them down after each job. EC2 is the only compute backend — the former
Kubernetes-pod backend (with its Valkey queue) was removed in 2026-06.

Two job-start flows: cold-start (~60s, fresh instance) and warm pool
(~10s, pre-provisioned instance). Roadmap phases 1-5 (cold-start MVP through
concurrent processing) are complete; candidate work beyond that is tracked
in [docs/ROADMAP.md](../../docs/ROADMAP.md) (alerting, cost-anomaly
detection, circuit-breaker refinement, admin RBAC, and more). Sources:
[README.md](../../README.md),
[.claude/CLAUDE.md](../../.claude/CLAUDE.md),
[AGENTS.md](../../AGENTS.md),
[docs/USAGE.md](../../docs/USAGE.md),
[docs/CONFIGURATION.md](../../docs/CONFIGURATION.md),
[docs/ROADMAP.md](../../docs/ROADMAP.md).

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

Code layout (top-level packages, per
[.claude/CLAUDE.md](../../.claude/CLAUDE.md) and
[AGENTS.md](../../AGENTS.md)):

- `cmd/server/` — Fargate orchestrator entry point (see [cmd-server](cmd-server.md))
- `cmd/agent/` — EC2 instance bootstrap binary
- `cmd/mirror-proxy/` — on-host Docker Hub → ECR pull-through mirror proxy
- `pkg/admin/` — Admin API and Next.js UI
- `pkg/fleet/` — EC2 fleet orchestration, spot strategy, launch templates
- `pkg/pools/` — Warm pool reconciliation (hot/stopped instances)
- `pkg/queue/` — Queue abstraction (SQS FIFO implementation)
- `pkg/github/` — GitHub App API client, webhook validation, label/alias parsing
- `pkg/db/` — DynamoDB state for jobs, pools, locks
- `pkg/cache/` — S3-backed GitHub Actions cache protocol
- `pkg/circuit/` — Circuit breaker for instance type failures
- `pkg/cost/` — Cost reporting and pricing calculations
- `pkg/events/` — EventBridge events (spot interruptions, job re-run recovery)
- `pkg/gitops/` — GitOps integration for pool configuration
- `pkg/housekeeping/` — Orphan/stale cleanup tasks
- `pkg/secrets/` — SSM or Vault backend abstraction
- `pkg/metrics/` — CloudWatch / Prometheus / Datadog backends
- `pkg/termination/` — Agent telemetry + job-completion consumer
- `pkg/tracing/` — OpenTelemetry distributed tracing

Build artifacts: a single `runs-fleet-server` binary plus per-arch agent
binaries (`agent-arm64`, `agent-amd64`) declared in
[flake.nix](../../flake.nix). The server image is multi-stage: Node builds
the admin UI, a golang alpine stage cross-compiles via
`BUILDPLATFORM`/`TARGETARCH`, and alpine is the runtime
([Dockerfile](../../Dockerfile)). [Makefile](../../Makefile) targets:
`build` (admin UI → server), `test`, `lint`, `coverage`, `docker-build`,
`docker-push`, `docker-build-runner`, `docker-push-runner` (multi-arch),
`scan-runner` / `sbom-runner` (the Trivy gate), `run-server`, `mocks`, and
`ci` (= `deps lint test build`). See
[infrastructure](infrastructure.md) for the two-layer Packer AMI pipeline
behind the EC2 instances themselves.

Concurrency: multi-instance Fargate is supported through per-pool
reconciliation locks in DynamoDB (65s TTL, conditional writes). Different
pools can reconcile concurrently; the same pool serialises to one
orchestrator instance ([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).

## Talks To [coverage: high -- 5 sources]

External systems the orchestrator integrates with:

- **GitHub** — App-based auth (`go-github/v57`), webhook delivery,
  repo-level runner registration tokens (the reusable token flow — no JIT
  API usage), workflow-job status, and (since PR #454) the
  `RerunJobByID` endpoint used to recover a job a spot reclaim killed.
- **AWS EC2** — Fleet API, launch templates, spot + on-demand. SDK:
  `aws-sdk-go-v2/service/ec2`.
- **AWS ECR** — `aws-sdk-go-v2/service/ecr`, used by the on-host mirror proxy
  (`cmd/mirror-proxy`) to authenticate the Docker Hub pull-through cache.
- **AWS SQS FIFO** — Webhook fan-out, pool batches, EventBridge events,
  termination notifications, housekeeping.
- **AWS DynamoDB** — Job state, pool config + locks, circuit breaker,
  admin audit log, fleet-cost day rows.
- **AWS S3** — Cache artifacts, buildkit layer cache, runner logs, runner
  configs, agent binaries.
- **AWS SSM** — Parameter Store for runner secrets (default backend).
- **AWS CloudWatch** — Metrics backend (now **off by default**, see Key
  Decisions) and logs.
- **AWS Pricing API** — Cost reporting (`pkg/cost/`).
- **AWS SNS** — Cost report notifications.
- **EventBridge** — Spot interruption 2-min warnings (and EC2 state-change
  notifications, which the handler currently discards — see
  [events-and-termination](events-and-termination.md)).
- **HashiCorp Vault** (optional) — Alternative secrets backend with
  `aws`, `kubernetes`, `approle`, or `token` auth methods.
- **OIDC provider** (optional) — Native relying-party auth for the admin
  UI (`coreos/go-oidc`).
- **Datadog DogStatsD** (optional) — Metrics backend.
- **Prometheus** (optional) — `/metrics` scrape endpoint.
- **OTLP collector** (optional) — OpenTelemetry trace export over gRPC.

Dependency footprint visible in [go.mod](../../go.mod): AWS SDK v2 modules
(ec2, sqs, dynamodb, s3, ssm, sns, cloudwatch, pricing, ecr, imds),
`smithy-go`, `google/go-github/v57`, `golang-jwt/jwt/v5`,
`coreos/go-oidc/v3`, `hashicorp/vault/api` (+ `auth/aws`),
`prometheus/client_golang`, `DataDog/datadog-go/v5`,
`go.opentelemetry.io/otel`, `oklog/ulid/v2`, `google/uuid`.
The former `redis/go-redis` and `k8s.io/*` dependencies left with the K8s
backend removal.

## API Surface [coverage: high -- 5 sources]

**HTTP endpoints (orchestrator, port `:8080`):**

- `POST /webhook` — webhook intake (HMAC-SHA256 validated against
  `RUNS_FLEET_GITHUB_WEBHOOK_SECRET`)
- `GET /health` — liveness (used by the Dockerfile HEALTHCHECK)
- `GET /ready` — readiness; pings the job queue and reports 503 during
  shutdown drain
- `GET /metrics` — Prometheus scrape (when
  `RUNS_FLEET_METRICS_PROMETHEUS_ENABLED=true`; path configurable via
  `RUNS_FLEET_METRICS_PROMETHEUS_PATH`)
- `/api/…` + `/admin/` — Admin API and Next.js static-export UI, gated by
  native OIDC auth (`RUNS_FLEET_ADMIN_OIDC_ISSUER_URL` and related vars),
  including `GET /api/audit-logs` when the audit table is configured
- Cache protocol endpoints (both the legacy `ACTIONS_CACHE_URL` API and
  the `actions/cache@v4`+ protocol; HMAC-authenticated, S3 pre-signed URL
  redirects). Requires `RUNS_FLEET_BASE_URL`, the externally-reachable
  HTTPS base URL served to runners.

**Workflow `runs-on` label syntax:**

```yaml
runs-on: "runs-fleet"
runs-on: "runs-fleet/cpu=4"
runs-on: "runs-fleet/cpu=4/arch=arm64/pool=default"
runs-on: "runs-fleet/cpu=4+16/ram=8+32/family=c7g+m7g"
runs-on: "runs-fleet/cpu=4/arch=arm64/gen=8"
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4"  # legacy form, still supported
```

The bare `runs-fleet` marker is the **only required token**; run_id is
sourced from the webhook payload
([internal/handler/webhook.go:68](../../internal/handler/webhook.go)). The
legacy `runs-fleet=<run-id>/...` form remains supported — its run_id
segment is optional and ignored, because the webhook is authoritative.

Resource labels:

| Label | Meaning |
|-------|---------|
| `cpu=<n>` | vCPU count (default 2); expands to a 2x range (e.g. `cpu=4` → 4-8) |
| `cpu=<min>+<max>` | Explicit vCPU range (`cpu=4+4` for exact) |
| `ram=<n>` | Minimum RAM in GB — *no* auto-range, unlike `cpu=` |
| `ram=<min>+<max>` | Explicit RAM range in GB |
| `arch=<arm64\|amd64>` | Architecture; omit to let runs-fleet pick |
| `family=<f1>+<f2>` | Instance families (e.g. `c7g+m7g`) |
| `gen=<n>` | Instance generation (1-10, e.g. `gen=8` for Graviton4) |
| `disk=<size>` | Disk in GiB (1-16384, gp3) |

Routing labels:

| Label | Meaning |
|-------|---------|
| `runs-fleet` | Runner marker (required) |
| `pool=<name>` | Warm pool routing (≤63 chars, alphanumeric/`-`/`_`; auto-creates ephemeral pools) |
| `spot=false` | Force on-demand cold-start |

Default families when no `family=` label is given
([docs/USAGE.md](../../docs/USAGE.md)): ARM64 → c8g, m8g, r8g, c7g, m7g;
AMD64 → c6i, c7i, m6i, m7i; no arch → all of the above. Burstable families
(`t3`, `t4g`) are excluded from the defaults since PR #385 — they remain in
the catalog for explicit `family=t3`/`family=t4g` opt-in. When `arch` is
omitted, runs-fleet queries average spot prices per candidate arch, submits
only the cheaper arch's launch template to EC2 Fleet
(`price-capacity-optimized` selects within it), and falls back to `arm64` if
the price fetch fails. Ephemeral pools are created 0-running / 1-stopped
with a 30-minute idle timeout and are deleted after sustained inactivity.

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
- `runs-fleet-pools` (`RUNS_FLEET_POOLS_TABLE`) — pool configuration,
  per-pool `reconcile_lock_owner` / `reconcile_lock_expires` (65s TTL,
  conditional writes), task locks, instance claims, auto-tune records, and
  the `__fleet_day:` fleet-cost sample rows
- `runs-fleet-circuit-state` (`RUNS_FLEET_CIRCUIT_BREAKER_TABLE`) —
  circuit breaker state per instance type
- `runs-fleet-audit` (`RUNS_FLEET_AUDIT_TABLE`, optional) — append-only
  admin action log with ~90-day TTL; unset disables persistence

**S3 buckets:**

- `runs-fleet-cache` (`RUNS_FLEET_CACHE_BUCKET`) — Actions cache (30-day
  lifecycle), plus the transparent buildkit layer cache under `buildkit/`
  and per-job runner logs
- `runs-fleet-config` — runner configs, agent binaries
- Cost report bucket (`RUNS_FLEET_COST_REPORT_BUCKET`)

**SQS queues:**

- Main queue (`RUNS_FLEET_QUEUE_URL`) — job requests, FIFO, batch 10,
  5-min visibility, DLQ after 3 receives
- Pool queue (`RUNS_FLEET_POOL_QUEUE_URL`) — batched warm-pool jobs
- Events queue (`RUNS_FLEET_EVENTS_QUEUE_URL`) — EventBridge / spot
  interruptions
- Termination queue (`RUNS_FLEET_TERMINATION_QUEUE_URL`) — agent telemetry
  (no DLQ; see [events-and-termination](events-and-termination.md))
- Housekeeping queue (`RUNS_FLEET_HOUSEKEEPING_QUEUE_URL`)
- DLQ (`RUNS_FLEET_QUEUE_DLQ_URL`)

Pool reconciliation runs on a 60s loop. Hot pools keep instances
running; stopped pools batch-start up to 50 on-demand instances on
demand; idle timeout defaults to 60 minutes
([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).

**Newest config surface** (see [config-bootstrap](config-bootstrap.md) for
the full loader):

- `RUNS_FLEET_REPORT_TIMEZONE` — IANA zone that cost days and months are
  bucketed in; default `Asia/Seoul` (PR #455). UTC would split a Korean
  working day at 09:00 and roll a month-to-date total over mid-morning on
  the 1st. An unparseable zone is a **hard startup error**.
- `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` — now defaults to **`false`**
  (PR #456, this branch). It was the one backend previously on by default.
- `RUNS_FLEET_HOT_POOLS_ENABLED` / `RUNS_FLEET_HOT_POOL_CAPS` — single
  master toggle plus fleet-wide safety ceilings for demand-following hot
  pools; per-pool warmth is auto-tuned, never Helm-configured.

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
- **2026-08: CloudWatch metrics ship disabled (PR #456).** Nothing queried
  the `RunsFleet` namespace — no alarms, no dashboards, and the admin
  console reads DynamoDB — while every metric cost one un-batched
  `PutMetricData` request plus a monthly charge per unique dimension
  combination. Pool reconciliation alone emitted 9 gauges per pool per 60s
  pass (and again on every queued-job webhook via `NotifyPoolDemand`).
  Batching would have optimised delivery of data with no consumer, so the
  default was flipped instead: every `Publish*` call site and the backend
  itself are untouched, and re-enabling is one env var. The Helm chart sets
  the variable unconditionally from `values.yaml`, so the Go default alone
  would not have reached a deployment — both changed.
- **2026-08: cost reporting is timezone-explicit (PR #455).** Day and month
  bucketing goes through `Config.ReportLocation()`
  (`RUNS_FLEET_REPORT_TIMEZONE`, default `Asia/Seoul`) rather than UTC, and
  an unparseable zone fails startup. Bucketing cost into the wrong days is
  invisible once it starts, so a silent UTC fallback was rejected.
- **2026-08: a spot reclaim can trigger a GitHub job re-run (PR #454).**
  A re-queue only rescues a job GitHub still has dispatchable; once GitHub
  concludes the job failed, registration-binds-to-label means no
  replacement runner can ever be handed it. `pkg/events/rerun.go` therefore
  waits for GitHub's conclusion and calls `RerunJobByID`. Deliberately
  single-job, not rerun-failed-jobs, so genuine failures are not
  re-executed.
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
- **Two committed agent-guidance files.**
  [.claude/CLAUDE.md](../../.claude/CLAUDE.md) (Claude Code) and
  [AGENTS.md](../../AGENTS.md) (the tool-agnostic convention) carry the same
  contributor contract: read `wiki/` before scanning source; read
  `docker/runner/CLAUDE.md` before touching the runner image (Trivy CVE
  policy) and `packer/README.md` before touching the AMI layers; TDD with
  `make lint` + `make test` green before commit, then a `code-reviewer`
  pass; rebase-only linear history with atomic conventional commits; typed
  `db.JobStatus*` constants over bare strings; and `testing/synctest` as the
  default for goroutine/timer tests. They differ in one place: `CLAUDE.md`
  delegates the env-var reference to `docs/CONFIGURATION.md`, while
  `AGENTS.md` inlines an abridged copy of it.

## Gotchas [coverage: medium -- 4 sources]

- **Cost reporting is approximate.** The EC2 section is computed per-job
  from DynamoDB job records via the shared `JobPricer` (exact instance type,
  spot flag, duration), preferring the live Pricing API and spot feed but
  falling back to a hard-coded table (t4g, c7g, m7g) and a fixed 70% spot
  discount. Supporting-service costs (Fargate, SQS, DynamoDB, CloudWatch,
  S3) remain flat per-job estimates; regional price variation, data
  transfer, and S3 request costs are excluded
  ([AGENTS.md](../../AGENTS.md)).
- **Disabling CloudWatch blanks the metric-derived parts of the cost
  report.** The daily report reads two series back out of CloudWatch. The
  runner-minute section already self-hid when absent; PR #456 additionally
  made spot interruptions print `unavailable` rather than `0`, so an
  unmeasured quantity is no longer rendered as a measured one.
- **`AGENTS.md`'s inlined env-var list drifts.** Unlike
  [.claude/CLAUDE.md](../../.claude/CLAUDE.md), which delegates to
  `docs/CONFIGURATION.md`, `AGENTS.md` keeps an abridged copy that has to be
  updated by hand on every config change — it lagged the PR #456 CloudWatch
  default flip until that PR caught it, and still omits
  `RUNS_FLEET_REPORT_TIMEZONE`, the hot-pool vars, and the four
  instance-grace-period vars.
  [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md) and
  [pkg/config/config.go](../../pkg/config/config.go) are authoritative.
- **Family-less `gen=3` (amd64) and `gen=4` (arm64) now error.** Those
  generations contained only burstable families, so after #385 dropped
  `t3`/`t4g` from the defaults, such requests resolve to zero matching
  instance types unless a `family=` label opts back in.
- **gp3 disk costs scale linearly.** ~$0.08/GB-month — a 1 TB
  `disk=1024` adds ~$80/month, dwarfing compute cost
  ([docs/USAGE.md](../../docs/USAGE.md)).
- **Spot interruption is best-effort recovery.** EventBridge gives 2 min
  warning; the in-progress job is re-queued to a new instance and, if
  GitHub has already concluded it failed, re-run — but the current step is
  lost either way.
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
- **Default region is ap-northeast-1.** [Makefile](../../Makefile),
  [flake.nix](../../flake.nix), and `config.Load()` all default to Tokyo;
  there is no per-job region label or cross-region fallback (tracked in
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
- [AGENTS.md](../../AGENTS.md)
- [docs/USAGE.md](../../docs/USAGE.md)
- [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)
- [docs/METRICS.md](../../docs/METRICS.md)
- [docs/ROADMAP.md](../../docs/ROADMAP.md)
- [Makefile](../../Makefile)
- [go.mod](../../go.mod)
