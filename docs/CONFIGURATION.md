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
| `RUNS_FLEET_POOLS_TABLE` | | Pool configurations (includes per-pool reconciliation locks) |
| `RUNS_FLEET_CIRCUIT_BREAKER_TABLE` | `runs-fleet-circuit-state` | Circuit breaker state |

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
| `RUNS_FLEET_MAX_RUNTIME_MINUTES` | `360` | Max job runtime (1-1440) |
| `RUNS_FLEET_LAUNCH_TEMPLATE_NAME` | `runs-fleet-runner` | EC2 launch template |
| `RUNS_FLEET_TAGS` | | Custom EC2 tags (JSON object) |
| `RUNS_FLEET_TAG_KEY_APPLICATION` | `Application` | Tag key for the fixed `runs-fleet` cost-attribution value |
| `RUNS_FLEET_TAG_KEY_SERVICE` | `Service` | Tag key for the fixed `runner` cost-attribution value |

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

## Cache

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_CACHE_SECRET` | HMAC secret for cache auth |
| `RUNS_FLEET_BASE_URL` | **Required.** Orchestrator's externally-reachable HTTPS base URL; served to runners as `ACTIONS_CACHE_URL` for the S3-backed Actions cache |

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
| `RUNS_FLEET_ADMIN_SECRET` | | Enables admin API auth: any non-empty value requires authenticated Keycloak gatekeeper headers on `/api/` requests; leave empty to disable auth (local dev). The value is a toggle only — it is not validated as a secret |
