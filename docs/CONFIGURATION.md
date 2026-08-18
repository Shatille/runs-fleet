# Configuration

All configuration is via environment variables, set on the orchestrator's runtime environment (the Fargate task definition in production; see the Terraform repo). The repository's `.envrc.example` only loads the Nix dev shell (`use flake`) — it is **not** a config template.

## Core (Required)

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_GITHUB_APP_ID` | GitHub App ID |
| `RUNS_FLEET_GITHUB_APP_PRIVATE_KEY` | GitHub App private key (PEM format) |
| `RUNS_FLEET_GITHUB_WEBHOOK_SECRET` | Webhook HMAC secret |
| `AWS_REGION` | AWS region (default: `ap-northeast-1`) |

## Queues (SQS)

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_QUEUE_URL` | Main job queue URL (required) |
| `RUNS_FLEET_QUEUE_DLQ_URL` | Dead letter queue URL |
| `RUNS_FLEET_POOL_QUEUE_URL` | Warm pool batch queue URL |
| `RUNS_FLEET_EVENTS_QUEUE_URL` | EventBridge events queue (spot interruptions) |
| `RUNS_FLEET_TERMINATION_QUEUE_URL` | Instance termination notifications |
| `RUNS_FLEET_HOUSEKEEPING_QUEUE_URL` | Cleanup task scheduling |

## DynamoDB

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_JOBS_TABLE` | | Job state tracking |
| `RUNS_FLEET_JOBS_POOL_STATUS_GSI` | | Optional GSI on the jobs table for pool+status queries (busy-instance lookups). Falls back to a table scan if unset |
| `RUNS_FLEET_JOBS_INSTANCE_ID_GSI` | | Optional GSI on the jobs table for `instance_id` lookups. Falls back to a table scan if unset |
| `RUNS_FLEET_POOLS_TABLE` | | Pool configurations (also holds per-pool reconciliation locks, per-instance warm-pool claim rows, and runner offline sightings). Expired `__instance_claim:` rows are swept by the `expired_instance_claims` housekeeping task (every 15 min) and, as a storage-level backstop, by DynamoDB TTL on `claim_expiry`. Stale `__runner_offline:` rows are reaped by the `orphaned_runners` task (hourly); the table's single TTL slot is already spent on `claim_expiry`, so that sweep is their only collector. Neither touches pool-config or lock rows. See `deploy/terraform/dynamodb.tf` |
| `RUNS_FLEET_CIRCUIT_BREAKER_TABLE` | `runs-fleet-circuit-state` | Circuit breaker state |
| `RUNS_FLEET_AUDIT_TABLE` | | Admin API audit log (pool CRUD, housekeeping actions). Unset disables persistence -- actions are still logged via slog, just not queryable through `GET /api/audit-logs`. Requires a `user-index` GSI (hash: `user`, range: `timestamp`); see `deploy/terraform/dynamodb.tf` |

## S3 & SNS

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_CACHE_BUCKET` | GitHub Actions cache artifacts |
| `RUNS_FLEET_COST_REPORT_BUCKET` | Cost report storage |
| `RUNS_FLEET_COST_REPORT_SNS_TOPIC` | Cost report notifications |

## EC2 Fleet

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_VPC_ID` | | VPC ID (required) |
| `RUNS_FLEET_SUBNET_IDS` | | Comma-separated subnet IDs (required) |
| `RUNS_FLEET_SECURITY_GROUP_ID` | | Security group ID (required) |
| `RUNS_FLEET_INSTANCE_PROFILE_ARN` | | IAM instance profile ARN (required) |
| `RUNS_FLEET_RUNNER_IMAGE` | | ECR image URL for runners (required) |
| `RUNS_FLEET_KEY_NAME` | | EC2 key pair name (optional) |
| `RUNS_FLEET_SPOT_ENABLED` | `true` | Enable spot instances (cold-start only; warm pool always uses on-demand) |
| `RUNS_FLEET_MAX_RUNTIME_MINUTES` | `360` | Max job runtime (1-1440). Also gates the age-based orphan sweep, which reaps any managed instance older than this + 10 minutes |
| `RUNS_FLEET_UNCLAIMED_INSTANCE_GRACE_MINUTES` | `30` | How long a cold-start instance may run without any job record before the orphan sweep terminates it as surplus (minimum 5). Pool-tagged instances are exempt — they are meant to idle awaiting work. Without this, a launch that never claimed a job is invisible to every job-driven sweep and bills until it crosses the `MAX_RUNTIME_MINUTES` cutoff |
| `RUNS_FLEET_COMPLETED_INSTANCE_GRACE_MINUTES` | `15` | How long an instance may keep running after every one of its job records reached a terminal state before the orphan sweep terminates it (minimum 5). Instances are ephemeral, so a finished job means no further work; the window covers the gap between the record being written terminal and the agent self-terminating. Without this, a runner that fails to exit bills until the `MAX_RUNTIME_MINUTES` cutoff |
| `RUNS_FLEET_STOPPED_INSTANCE_GRACE_HOURS` | `0` (disabled) | How long a **stopped** managed instance with no `runs-fleet:pool` tag may sit before the orphan sweep terminates it (0 disables; otherwise at least 1). Nothing else reaps these: the pool reconciler only manages tagged members and every other sweep filters to running instances, so an untagged stopped instance bills for its EBS volume indefinitely. Off by default because terminating a stopped instance destroys its volume |
| `RUNS_FLEET_STANDBY_DEADLINE_MINUTES` | `120` | Agent-side: how long an instance polls for a job config before exiting 0 (failsafe for a leaked spare; the reconciler is the primary decay path). Read on the runner, not the orchestrator. |
| `RUNS_FLEET_LAUNCH_TEMPLATE_NAME` | `runs-fleet-runner` | EC2 launch template |
| `RUNS_FLEET_TAGS` | | Custom EC2 tags (JSON object) |
| `RUNS_FLEET_TAG_KEY_APPLICATION` | `Application` | Tag key for the application cost-attribution value |
| `RUNS_FLEET_TAG_VALUE_APPLICATION` | `runs-fleet` | Tag value for the application cost-attribution key |
| `RUNS_FLEET_TAG_KEY_SERVICE` | `Service` | Tag key for the service cost-attribution value |
| `RUNS_FLEET_TAG_VALUE_SERVICE` | `runner` | Tag value for the service cost-attribution key |

## Label Aliases (custom runner labels)

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_LABEL_ALIASES` | | JSON array of alias rules. Empty/unset = disabled. Validated at startup. |

Maps externally-defined `runs-on` labels onto runs-fleet specs so runs-fleet can
serve jobs that target your *existing* runners (e.g. labels inherited from another
self-hosted runner system such as ARC) **without editing any workflow manifest**.
When a job has no `runs-fleet` marker, each of its `runs-on` labels is matched
against the rules in order; the first match expands to a spec, and the runner
registers under the original label so GitHub dispatches the job to it.

**Warm pools.** An aliased label also becomes its own warm pool (named after the
label) so migrated workloads get fast restarts: unless the rule's spec sets an
explicit `pool=`, the matched label is used as the pool name and an ephemeral
pool is auto-created (`DesiredRunning=0, DesiredStopped=1`) — first job
cold-starts, then a stopped replacement stays ready. Bump warm counts per-pool
via the admin API. (A label that isn't a legal pool name simply cold-starts.)

Each rule:

| Field | Description |
|-------|-------------|
| `match` | Label to match. A literal string, or a regular expression when `regex` is true. |
| `regex` | When true, `match` is a regex; `spec` may reference captures with `${1}` / `${name}`. |
| `spec`  | A runs-fleet label spec (`cpu=8+8/arch=arm64/disk=100/...`), parsed exactly like the native marker. |

Arch synonyms are normalized: `x64`/`x86_64` → `amd64`, `aarch64` → `arm64`.
Write `cpu=N+N` to pin vCPUs exactly (parity with a fixed-size runner) or `cpu=N`
for runs-fleet's default 2× range.

Example — a regex rule whose captures derive the spec for a whole family of
labels (here `ci-<N>x-<arch>`, e.g. `ci-8x-arm64`), plus a literal rule:

```json
[
  { "match": "^ci-(\\d+)x-(amd64|arm64)$", "regex": true, "spec": "cpu=${1}+${1}/arch=${2}" },
  { "match": "gpu-large", "spec": "cpu=16/arch=amd64/disk=200" }
]
```

**Matching & validation notes.**

- Rules are evaluated in order; the **first** match wins. The native `runs-fleet`
  marker always takes precedence over any alias.
- A regex `match` uses partial-match semantics — the pattern is **not**
  auto-anchored. **Anchor patterns with `^…$`** unless you intend substring
  matching — e.g. an unanchored `ci-(\d+)x` would also match `legacy-ci-8x-runner`.
- Literal-rule specs are fully validated at startup (parsed and resolved to
  instance types); a typo or an unsupported `family=` fails the boot. **Regex-rule
  specs that use captures can only be validated when a job matches**, so a bad
  capture template surfaces at job time (the job is then treated as unmatched).
  Test regex rules against representative labels before deploying.

**Migration note.** This enables a transparent cutover from existing self-hosted
runners. While both fleets run, they **compete** for queued jobs (whichever
claims a runner first wins; the loser's ephemeral runner self-terminates). Once
runs-fleet is proven to serve the jobs, **scale down the old runners**, then
remove them.

## Hot Pools (auto-tuned low-latency warmth)

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_HOT_POOLS_ENABLED` | `false` | Master toggle. The **only** Helm knob that turns hot pools on. When on, the feature applies uniformly to every pool; per-pool warmth is auto-tuned (below), not configured here. |
| `RUNS_FLEET_HOT_POOL_CAPS` | | JSON object of fleet-wide safety ceilings. Empty/unset = all defaults. Validated at startup. |

Normally an ephemeral pool sits at `desired_running=0`: every job starts a
*stopped* instance and pays the full boot + registration (~13s boot). For a
bursty pipeline whose stages run back-to-back, stages 2..N each pay that boot
even though a runner was hot moments earlier. Hot pools keep a **running** spare
alive for a short, demand-following window after job activity, so those later
stages land on a live agent and skip the stopped-instance boot.

**One master toggle, per-pool warmth auto-tuned.** `RUNS_FLEET_HOT_POOLS_ENABLED`
is the single Helm switch. With it off (default), the orchestrator's pool path is
byte-identical to a build without the feature: no extra DynamoDB reads, no changed
reconcile decisions, no running-instance claims. There is deliberately **no
per-pool Helm config** — most repo users can't touch the deployment chart, so
per-pool linger/maxHot is instead **auto-deduced from each pool's real run
history** by an hourly tuner and surfaced in the admin UI, where an operator can
set a per-pool override that wins.

**Effective spec per pool** resolves as `override > auto-recommendation > off`,
clamped to `RUNS_FLEET_HOT_POOL_CAPS`, and only when the master toggle is on.
**Cold-until-proven:** a pool with too little history, or no genuine burst
pattern, deduces to `linger=0` (fully cold, $0). The hourly tuner is itself gated
on the master toggle — with hot pools off it does no work and adds no DynamoDB
load, so an upgrade is behavior- and cost-neutral for fleets that never enable the
feature. After enabling, pools stay cold for at most one tuner tick until the
first pass populates recommendations (a safe cold→warm direction).

### Caps (`RUNS_FLEET_HOT_POOL_CAPS`)

Fleet-wide ceilings the auto-tuner and the admin overrides can never exceed. Any
omitted or zero field takes its default; a negative value or an out-of-range
ceiling fails startup.

| Field | Default | Description |
|-------|---------|-------------|
| `maxLingerMinutes` | `30` | Ceiling on the linger window (≤120). |
| `maxHot` | `3` | Ceiling on running spares kept during a linger window (≤10). |
| `minJobsToActivate` | `20` | Minimum jobs in the lookback window before a pool is eligible for a hot recommendation (cold-until-proven). |
| `lookbackDays` | `7` | History window the tuner samples each tick (≤90). |
| `burstGapMinutes` | `20` | Inter-job gap above which two jobs are treated as separate bursts (≤120). |

Helm (`values.yaml`):

```yaml
hotPools:
  enabled: true
  caps:            # optional; omit for defaults
    maxLingerMinutes: 30
    maxHot: 3
```

### Per-pool tuning (admin UI)

Per-pool linger/maxHot is managed in the admin pool editor, never in Helm:

- **Recommendation panel** (read-only): shows the tuner's rationale — e.g.
  *"Based on 42 jobs over 7d: p90 stage gap 240s, peak concurrency 2, 3 bursts →
  recommends 5m linger / 2 maxHot"*, or the cold reason (*insufficient history* /
  *no burst pattern → stays cold*).
- **Override inputs** for linger and maxHot. Blank = use the recommendation
  (placeholder shows the recommended value); `0` = force the pool cold; `N` = fix
  the value (bounded by the caps). Overrides are audited.

**How it works.** For a pool with a live effective linger window (recent job
activity within the resolved linger), reconciliation raises the running target to
the effective `maxHot` (a floor applied via `max()`, so it never lowers a
scheduled/admin target). When a job arrives, it is assigned to a running∧not-busy
spare (no boot) in preference to starting a stopped instance. Once the linger
window elapses, the floor drops to 0 and the existing warm-pool stop path banks
the spare within a couple of reconcile passes. Each spare stays **one-shot**: it
serves exactly one job, then self-terminates — no multi-job reuse.

**Scope & notes.**

- Only **ephemeral** pools (the label-created pools that pay the stopped-boot
  today) activate linger; persistent pools use `desired_running`/schedules instead.
- A hot spare is a running instance whose agent polls for its job config
  (see `RUNS_FLEET_STANDBY_DEADLINE_MINUTES`). The agent binary must be built from
  an AMI that includes the standby poll before enabling the feature.
- Observability adds no new metrics: linger-driven scale-ups carry
  `reason="linger"` on `pool_actions`, and a hot-spare assignment carries
  `source="hot_pool"` on `instance_provision_seconds` (a `warm_pool` subset for
  the sub-10s no-boot cohort).
- Rollback is instant and needs no AMI change: set `hotPools.enabled=false` and
  `helm upgrade`. Every effective spec drops to off on the next pass, spares are
  banked within ~5 min, assignment reverts to stopped-only, and the standby poll
  is inert. The stored recommendations/overrides go dormant; no cleanup needed.

## Cache

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_CACHE_SECRET` | HMAC secret for cache auth |
| `RUNS_FLEET_BASE_URL` | **Required.** Orchestrator's externally-reachable HTTPS base URL; served to runners as `ACTIONS_CACHE_URL` for the S3-backed Actions cache |

The cache bucket (`RUNS_FLEET_CACHE_BUCKET`) also serves transparent Docker
layer caching for buildx builds, under a `buildkit/` prefix beside the Actions
cache's `caches/`. This needs no configuration; it becomes effective once the
instance profile has S3 read/write on `buildkit/*` (until then, builds succeed
with cache-miss warnings). Per-workflow opt-out: `RUNS_FLEET_BUILD_CACHE=off`.
See "Build Cache" in [USAGE.md](USAGE.md).

The same bucket also holds each job's runner logs under a `runner-logs/` prefix,
so a failure stays diagnosable after GitHub expires its own logs (a superseded
attempt's are gone within hours). This needs no configuration; it becomes
effective once the instance profile has `s3:PutObject` on `runner-logs/*` and
the orchestrator's role has `s3:GetObject`/`s3:ListBucket` there. Until then
every job reports `log_upload=failed` and the `RunnerLogUpload` metric shows it
fleet-wide. Objects are expected to expire on a 14-day lifecycle rule scoped to
that prefix — set it, or they inherit whatever bucket-wide rule exists. Read
them from the console (a job's detail page) or with the `fetch-runner-logs`
skill.

These logs are not secret-masked in every path, unlike the ones GitHub renders.
Keep the bucket private, keep the runner's grant write-only, and note that
console reads are recorded in the audit trail.

## Graceful Shutdown

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_SHUTDOWN_DRAIN_DELAY_SECONDS` | `5` | On SIGTERM, seconds to keep serving HTTP after readiness flips to 503, so the load balancer deregisters the task before its listener closes — otherwise webhooks routed during the deregistration window fail and strand jobs. `0` disables. Must fit within the deploy's `stopTimeout`/`terminationGracePeriodSeconds` alongside the worker drain (~`MessageProcessTimeout + 10s`). |

## Metrics

The metric name prefix is fixed and cannot be configured: `RunsFleet` on
CloudWatch, `runs_fleet` on Prometheus and Datadog. A fixed prefix prevents
metric-name collisions across deployments that share a metrics backend.

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` | `true` | Enable CloudWatch metrics |
| `RUNS_FLEET_METRICS_PROMETHEUS_ENABLED` | `false` | Enable Prometheus `/metrics` endpoint |
| `RUNS_FLEET_METRICS_PROMETHEUS_PATH` | `/metrics` | Prometheus endpoint path |
| `RUNS_FLEET_METRICS_DATADOG_ENABLED` | `false` | Enable Datadog DogStatsD |
| `RUNS_FLEET_METRICS_DATADOG_ADDR` | `127.0.0.1:8125` | DogStatsD address |
| `RUNS_FLEET_METRICS_DATADOG_TAGS` | | Global tags (comma-separated) |
| `RUNS_FLEET_METRICS_DATADOG_SAMPLE_RATE` | `1.0` | DogStatsD sample rate (0.0-1.0) |
| `RUNS_FLEET_METRICS_DATADOG_BUFFER_POOL_SIZE` | `0` | DogStatsD buffer pool size |
| `RUNS_FLEET_METRICS_DATADOG_WORKERS_COUNT` | `0` | DogStatsD worker count |
| `RUNS_FLEET_METRICS_DATADOG_MAX_MSGS_PER_PAYLOAD` | `0` | DogStatsD max messages per payload |

## Tracing

OpenTelemetry tracing is disabled by default (noop provider, zero overhead). When enabled, spans are exported over OTLP gRPC.

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_TRACING_ENABLED` | `false` | Enable OpenTelemetry tracing |
| `RUNS_FLEET_OTEL_ENDPOINT` | | OTLP gRPC collector endpoint (required when tracing is enabled) |
| `RUNS_FLEET_OTEL_INSECURE` | `true` | Use an insecure gRPC connection to the collector |
| `RUNS_FLEET_OTEL_SERVICE_NAME` | `runs-fleet` | Service name reported on spans |
| `RUNS_FLEET_ENV` | | Deployment environment tag attached to spans (e.g. `production`) |

## Secrets Backend

Runner configuration secrets can be stored in SSM Parameter Store (default) or HashiCorp Vault.

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_SECRETS_BACKEND` | `ssm` | Backend: `ssm` or `vault` |
| `RUNS_FLEET_SECRETS_PATH_PREFIX` | `/runs-fleet/runners` | SSM parameter path prefix |

### Vault Configuration

Required when `RUNS_FLEET_SECRETS_BACKEND=vault`.

| Variable | Default | Description |
|----------|---------|-------------|
| `VAULT_ADDR` | | Vault server address (required) |
| `VAULT_NAMESPACE` | | Vault namespace (enterprise) |
| `VAULT_KV_MOUNT` | `secret` | KV secrets engine mount path |
| `VAULT_KV_VERSION` | auto-detect | KV version (1 or 2) |
| `VAULT_BASE_PATH` | `runs-fleet/runners` | Base path for secrets |
| `VAULT_AUTH_METHOD` | `aws` | Auth: `aws`, `kubernetes`, `approle`, `token` |

### Vault Auth Methods

| Auth | Variables |
|------|-----------|
| `aws` | `VAULT_AWS_ROLE` (default: `runs-fleet`), `VAULT_AWS_REGION` |
| `kubernetes` | `VAULT_K8S_ROLE` (required), `VAULT_K8S_JWT_PATH`, `VAULT_K8S_AUTH_MOUNT` (default: `kubernetes`) |
| `approle` | `VAULT_APP_ROLE_ID`, `VAULT_APP_SECRET_ID` (both required) |
| `token` | `VAULT_TOKEN` (required) |

## Logging & Admin

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `RUNS_FLEET_LOG_FORMAT` | `json` | Log format: `json` or `text` |
| `RUNS_FLEET_ADMIN_RATE_LIMIT` | `60` | Admin API rate limit (per-IP requests/minute) |
| `RUNS_FLEET_TRACE_UI_URL` | | Base URL for trace links shown in the admin jobs view (e.g. Jaeger UI) |

## Admin OIDC Authentication

The admin API/UI authenticates directly against an OIDC provider (Keycloak,
Auth0, Okta, Google, or any standards-compliant issuer) — no external
gatekeeper or reverse proxy required. Leave every variable below unset to
run the admin API unauthenticated (local dev only). Setting any one of them
requires setting all of them; a partial configuration fails at startup.

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_ADMIN_OIDC_ISSUER_URL` | | OIDC issuer URL. Must expose `/.well-known/openid-configuration` |
| `RUNS_FLEET_ADMIN_OIDC_CLIENT_ID` | | OAuth client ID registered with the provider |
| `RUNS_FLEET_ADMIN_OIDC_CLIENT_SECRET` | | OAuth client secret |
| `RUNS_FLEET_ADMIN_OIDC_REDIRECT_URL` | `{RUNS_FLEET_BASE_URL}/api/auth/callback` | Callback URL; must also be registered with the provider as an allowed redirect URI |
| `RUNS_FLEET_ADMIN_OIDC_SCOPES` | `openid,profile,email` | Comma-separated OAuth scopes requested at login |
| `RUNS_FLEET_ADMIN_OIDC_GROUPS_CLAIM` | `groups` | ID token claim name holding the user's groups. Keycloak/Okta use `groups`; Auth0 commonly namespaces it (e.g. `https://example.com/groups`); Google has none |
| `RUNS_FLEET_ADMIN_SESSION_SECRET` | | HMAC signing key for the admin session cookie. Generate with e.g. `openssl rand -base64 32` |
| `RUNS_FLEET_ADMIN_SESSION_TTL_MINUTES` | `480` | Admin session lifetime. No refresh: once expired, the user logs in again |
