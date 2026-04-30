---
topic: Project Overview
last_compiled: 2026-04-30
sources_count: 8
---

## Purpose [coverage: high -- 5 sources]

runs-fleet is a self-hosted, ephemeral GitHub Actions runner system that
replaces GitHub-hosted runners with spot-first EC2 instances or Kubernetes
pods. It targets cost reduction (~$55-65/month vs ~$80/month for hosted
runners on a 100 jobs/day workload) while preserving job-start latency via
warm pools.

The system is written in Go 1.25+ and runs as a long-lived orchestrator on
ECS Fargate that consumes GitHub webhooks, materialises workflow jobs onto
ephemeral compute, and tears it down after each job. Two compute backends
are supported:

- **EC2 backend** (default) — EC2 Fleet API with spot diversification,
  on-demand fallback, and stop/start warm pools.
- **K8s backend** — Kubernetes pods with a Valkey/Redis queue.

Two job-start flows: cold-start (~60s, fresh instance) and warm pool
(~10s, pre-provisioned instance). Sources:
[README.md](../../README.md),
[.claude/CLAUDE.md](../../.claude/CLAUDE.md),
[docs/USAGE.md](../../docs/USAGE.md),
[docs/CONFIGURATION.md](../../docs/CONFIGURATION.md),
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
                EC2 Spot/On-demand Fleet  OR  K8s Pods
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
- `pkg/provider/` — Compute provider abstraction (`ec2/`, `k8s/`)
- `pkg/queue/` — Queue abstraction (SQS for EC2, Valkey for K8s)
- `pkg/db/` — DynamoDB state for jobs, pools, locks
- `pkg/cache/` — S3-backed GitHub Actions cache protocol
- `pkg/circuit/` — Circuit breaker for instance type failures
- `pkg/events/` — EventBridge events (spot interruptions)
- `pkg/secrets/` — SSM or Vault backend abstraction
- `pkg/metrics/` — CloudWatch / Prometheus / Datadog backends
- `pkg/tracing/` — OpenTelemetry distributed tracing

Build artifacts: a single `runs-fleet-server` binary plus per-arch agent
binaries (`agent-arm64`, `agent-amd64`) declared in
[flake.nix](../../flake.nix). The server image is multi-stage: Node 22
builds the admin UI, golang:1.25-alpine cross-compiles via
`BUILDPLATFORM`/`TARGETARCH`, and alpine:3.19 is the runtime
([Dockerfile](../../Dockerfile)). Multi-arch runner images are built in
parallel via `make docker-push-runner` ([Makefile](../../Makefile)).

Concurrency: multi-instance Fargate is supported through per-pool
reconciliation locks in DynamoDB (65s TTL, conditional writes). Different
pools can reconcile concurrently; the same pool serialises to one
orchestrator instance ([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).

## Talks To [coverage: high -- 5 sources]

External systems the orchestrator integrates with:

- **GitHub** — App-based auth (`go-github/v57`), webhook delivery,
  Just-In-Time runner registration tokens, GraphQL for pool sizing.
- **AWS EC2** — Fleet API, launch templates, spot + on-demand. SDK:
  `aws-sdk-go-v2/service/ec2`.
- **AWS SQS FIFO** — Webhook fan-out, pool batches, EventBridge events,
  termination notifications, housekeeping.
- **AWS DynamoDB** — Job state, pool config + locks, circuit breaker.
- **AWS S3** — Cache artifacts, runner configs, agent binaries.
- **AWS SSM** — Parameter Store for runner secrets (default backend).
- **AWS CloudWatch** — Metrics + logs.
- **AWS Pricing API** — Cost reporting (`pkg/cost/`).
- **AWS SNS** — Cost report notifications.
- **EventBridge** — Spot interruption 2-min warnings.
- **HashiCorp Vault** (optional) — Alternative secrets backend with
  `aws`, `kubernetes`, `approle`, or `token` auth methods.
- **Valkey/Redis** (K8s mode) — Queue backend (`go-redis/v9`).
- **Kubernetes API** (K8s mode) — Pod lifecycle (`k8s.io/client-go`).
- **Datadog DogStatsD** (optional) — Metrics backend.
- **Prometheus** (optional) — `/metrics` scrape endpoint.

Source dependency footprint visible in
[go.mod](../../go.mod): AWS SDK v2 modules,
`google/go-github/v57`, `golang-jwt/jwt/v5`, `hashicorp/vault/api`,
`prometheus/client_golang`, `DataDog/datadog-go/v5`, `redis/go-redis/v9`,
`k8s.io/{api,apimachinery,client-go} v0.35.0`.

## API Surface [coverage: high -- 5 sources]

**HTTP endpoints (orchestrator):**

- Webhook intake (HMAC-SHA256 validated against
  `RUNS_FLEET_GITHUB_WEBHOOK_SECRET`)
- `/health` — health check (used by the Dockerfile HEALTHCHECK against
  `:8080`)
- `/metrics` — Prometheus scrape (when
  `RUNS_FLEET_METRICS_PROMETHEUS_ENABLED=true`)
- Admin API + UI (Next.js static export, gated by
  `RUNS_FLEET_ADMIN_SECRET`)
- Cache protocol endpoints (HMAC-authenticated, S3 pre-signed URL
  redirects)

**Workflow `runs-on` label syntax:**

```yaml
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64/pool=default"
```

| Label | Meaning |
|-------|---------|
| `runs-fleet=<run-id>` | Workflow run identifier (required) |
| `cpu=<n>` | vCPU count; defaults to 2x range (e.g. `cpu=4` → 4-8) |
| `cpu=<min>+<max>` | Explicit vCPU range (`cpu=4+4` for exact) |
| `ram=<n>` / `ram=<min>+<max>` | RAM in GB |
| `arch=<arm64\|amd64>` | Architecture; omit for both |
| `family=<f1>+<f2>` | Instance families (e.g. `c7g+m7g`) |
| `gen=<n>` | Instance generation (e.g. `gen=8` for Graviton4) |
| `disk=<size>` | Disk in GiB (1-16384, gp3) |
| `pool=<name>` | Warm pool routing (auto-creates ephemeral pools) |
| `spot=false` | Force on-demand cold-start |
| `public=true` | Request public IP / public subnet |
| `backend=<ec2\|k8s>` | Override default backend |
| `region=<region>` | Multi-region target |
| `env=<dev\|staging\|prod>` | Environment isolation |

Default families (`docs/USAGE.md`): ARM64 → c7g, m7g, t4g; AMD64 → c6i,
c7i, m6i, m7i, t3. Architecture-agnostic jobs build per-arch launch
template configurations and let EC2 Fleet's `price-capacity-optimized`
strategy select. Ephemeral pools auto-scale on a 1-hour peak window and
delete after 4 hours of inactivity.

## Data [coverage: high -- 5 sources]

**DynamoDB tables:**

- `runs-fleet-jobs` (`RUNS_FLEET_JOBS_TABLE`) — job lifecycle keyed by
  job_id or instance_id
- `runs-fleet-pools` (`RUNS_FLEET_POOLS_TABLE`) — pool configuration plus
  per-pool `reconcile_lock_owner` / `reconcile_lock_expires` (65s TTL,
  conditional writes)
- `runs-fleet-circuit-state` (`RUNS_FLEET_CIRCUIT_BREAKER_TABLE`) —
  circuit breaker state per instance type

**S3 buckets:**

- `runs-fleet-cache` (`RUNS_FLEET_CACHE_BUCKET`) — Actions cache, 30-day
  lifecycle
- `runs-fleet-config` — runner configs, agent binaries
- Cost report bucket (`RUNS_FLEET_COST_REPORT_BUCKET`)

**SQS queues (EC2 mode):**

- Main queue (`RUNS_FLEET_QUEUE_URL`) — job requests, FIFO, batch 10,
  5-min visibility, DLQ after 3 receives
- Pool queue (`RUNS_FLEET_POOL_QUEUE_URL`) — batched warm-pool jobs
- Events queue (`RUNS_FLEET_EVENTS_QUEUE_URL`) — EventBridge / spot
  interruptions
- Termination queue (`RUNS_FLEET_TERMINATION_QUEUE_URL`)
- Housekeeping queue (`RUNS_FLEET_HOUSEKEEPING_QUEUE_URL`)
- DLQ (`RUNS_FLEET_QUEUE_DLQ_URL`)

**K8s mode:** queue is Valkey/Redis at `RUNS_FLEET_VALKEY_ADDR` (default
`valkey:6379`) instead of SQS.

Pool reconciliation runs on a 60s loop. Hot pools keep instances
running; stopped pools batch-start up to 50 on-demand instances on
demand; idle timeout defaults to 60 minutes
([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).

## Key Decisions [coverage: high -- 5 sources]

- **Spot-first cold-start, on-demand warm pool.** Cold-start jobs
  diversify across spot instance types for cost. Warm pools intentionally
  use on-demand only — stop/start reliability beats the negligible spot
  savings on a stopped fleet
  ([docs/CONFIGURATION.md](../../docs/CONFIGURATION.md),
  [docs/USAGE.md](../../docs/USAGE.md)).
- **ARM-preferred default families.** ARM64 (Graviton) is cheaper per
  vCPU; the default family list (c7g, m7g, t4g) reflects that. AMD64 is
  available for x86-specific builds.
- **Ephemeral instances, no reuse.** Each job gets a fresh runner that
  self-terminates — no state accumulation, no cross-tenant leakage.
- **Bounded spot diversification.** `cpu=N` expands to `[N, 2N]` vCPUs by
  default to widen the spot pool without over-provisioning. Explicit
  `cpu=N+N` opts out.
- **`price-capacity-optimized` allocation strategy.** EC2 Fleet selects
  cheapest-with-capacity from the diversified pool.
- **Two backends, one orchestrator.** `pkg/provider/` and `pkg/queue/`
  abstract over EC2/SQS vs K8s/Valkey. Backend chosen at deploy time via
  `RUNS_FLEET_MODE`; per-job override via `backend=` label.
- **Per-pool distributed locks (DynamoDB conditional writes), no global
  leader election.** Multi-instance Fargate scales horizontally; same
  pool serialises ([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).
- **Eventual consistency tolerated.** DynamoDB, SQS, EC2 API all
  eventually consistent; design accommodates with retries and idempotent
  reconciliation.
- **Static export admin UI inside the server binary.** Single Docker
  image; UI is built at image-build time and embedded
  ([Dockerfile](../../Dockerfile)).

## Gotchas [coverage: medium -- 3 sources]

- **Cost reporting is approximate.** `pkg/cost/` hard-codes pricing for
  only 3 instance families (t4g, c7g, m7g), assumes a fixed 70% spot
  discount, ignores regional price variation, and excludes data transfer
  and S3 request costs ([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).
- **gp3 disk costs scale linearly.** ~$0.08/GB-month — a 1 TB
  `disk=1024` adds ~$80/month, dwarfing compute cost
  ([docs/USAGE.md](../../docs/USAGE.md)).
- **Spot interruption is best-effort recovery.** EventBridge gives 2 min
  warning; the in-progress job is re-queued to a new instance, but the
  current step is lost.
- **`spot=false` only affects cold-start.** Warm pool jobs are always
  on-demand by design — the label is a no-op there.
- **Webhook validation is strict.** HMAC-SHA256 on every request; the
  webhook secret must match GitHub App settings exactly. IMDSv2 is
  required on EC2 ([.claude/CLAUDE.md](../../.claude/CLAUDE.md)).
- **Secrets-in-env are deprecated.** Runner secrets belong in SSM or
  Vault; only orchestrator config is in env vars.
- **Default region is ap-northeast-1.** Both
  [Makefile](../../Makefile) and [flake.nix](../../flake.nix) hard-code
  Tokyo as the default `AWS_REGION`. Multi-region deployments must set
  `region=` per job and provision per-region infra.
- **K8s mode requires Valkey.** SQS queue env vars are ignored when
  `RUNS_FLEET_MODE=k8s`; `RUNS_FLEET_VALKEY_ADDR` is required.
- **First-time Nix build needs `npmDepsHash` update.** The placeholder
  hash in [flake.nix](../../flake.nix) for the admin UI must be replaced
  with the real hash after the first build.

## Sources [coverage: high]

- [README.md](../../README.md)
- [.claude/CLAUDE.md](../../.claude/CLAUDE.md)
- [docs/USAGE.md](../../docs/USAGE.md)
- [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)
- [Dockerfile](../../Dockerfile)
- [Makefile](../../Makefile)
- [flake.nix](../../flake.nix)
- [go.mod](../../go.mod)
