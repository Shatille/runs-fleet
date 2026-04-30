---
topic: Config + Bootstrap (env vars + AWS clients)
last_compiled: 2026-04-30
sources_count: 3
---

# Config + Bootstrap (env vars + AWS clients)

## Purpose [coverage: high -- 3 sources]
Centralized configuration loading for the orchestrator. The `config` package reads every runtime knob from environment variables, validates required fields, parses structured values (JSON tags, tolerations, node selectors, subnet lists), and exposes a single `Config` struct consumed by every other package. It also defines shared timeout constants used by SQS pollers, message handlers, and cleanup paths. `docs/CONFIGURATION.md` is the user-facing reference for the same env vars.

## Architecture [coverage: high -- 3 sources]
Three artifacts make up the bootstrap layer:

1. `pkg/config/config.go` — declares `Config` (the struct), `Toleration`, the `BackendEC2` / `BackendK8s` constants, the `Load()` entrypoint, and the `Validate()` method tree (`validateEC2Config`, `validateK8sConfig`, `validateSecretsConfig`, `validateVaultAuthConfig`, `validateMetricsConfig`). Helpers `getEnv`, `getEnvInt`, `getEnvBool`, `getEnvFloat`, `getEnvIntDefault`, `splitAndFilter` provide typed env reads. Parsers `parseNodeSelector`, `parseTolerations`, `parseResourceLabels`, `parseTags` consume JSON / `key=value` strings with K8s label validation (`validateK8sLabelKey`, `isValidK8sLabelValue`) and AWS tag rules (`validateTags`).
2. `pkg/config/timeouts.go` — exports `ShortTimeout`, `MessageReceiveTimeout`, `MessageProcessTimeout`, `CleanupTimeout`, and the `MaxBodySize` HTTP body cap.
3. `docs/CONFIGURATION.md` — tabular reference grouped by Core / Queues / DynamoDB / S3+SNS / EC2 / K8s / DinD / Valkey / Cache / Metrics / Secrets / Vault / Logging.

Note: the file header says the package "manages application configuration from environment variables." AWS SDK clients (SQS, EC2, DynamoDB, S3, SSM, CloudWatch) are not constructed inside `pkg/config` itself — `Config` carries the inputs (region, table names, queue URLs, bucket names) that callers feed to `aws-config.LoadDefaultConfig` and SDK service constructors elsewhere in the codebase.

## Talks To [coverage: high -- 3 sources]
- **Process environment** via `os.Getenv` (every field).
- **`internal/validation`** for `validation.ValidateK8sJWTPath` (Vault Kubernetes auth).
- **`encoding/json`** for tolerations, resource labels, and EC2 tag JSON inputs.
- **`net.SplitHostPort` / `strconv`** for `validateHostPort` on the Datadog DogStatsD address.
- **Downstream consumers** — every package listed in the project layout (`pkg/fleet`, `pkg/queue`, `pkg/db`, `pkg/cache`, `pkg/secrets`, `pkg/metrics`, `pkg/provider/...`) reads fields off the returned `*Config`.

## API Surface [coverage: high -- 3 sources]

### Constants
- `BackendEC2 = "ec2"`, `BackendK8s = "k8s"` — values for `RUNS_FLEET_MODE`.
- Timeouts (`pkg/config/timeouts.go`):
  - `ShortTimeout = 3 * time.Second`
  - `MessageReceiveTimeout = 25 * time.Second`
  - `MessageProcessTimeout = 90 * time.Second` (sized for synchronous `CreateFleet`)
  - `CleanupTimeout = 5 * time.Second`
  - `MaxBodySize = 1 << 20` (1 MiB HTTP body cap)

### Types
- `Toleration{ Key, Operator, Value, Effect string }` — JSON-tagged, mirrors the K8s toleration shape.
- `Config` — single struct holding every setting. Fields by group:
  - **Core:** `AWSRegion`, `DefaultBackend`, `GitHubWebhookSecret`, `GitHubAppID`, `GitHubAppPrivateKey`.
  - **Queues:** `QueueURL`, `QueueDLQURL`, `PoolQueueURL`, `EventsQueueURL`, `TerminationQueueURL`, `HousekeepingQueueURL`.
  - **DynamoDB:** `JobsTableName`, `PoolsTableName`, `CircuitBreakerTable`.
  - **S3 / SNS:** `CacheBucketName`, `CostReportSNSTopic`, `CostReportBucket`.
  - **EC2:** `VPCID`, `PublicSubnetIDs`, `PrivateSubnetIDs`, `SecurityGroupID`, `InstanceProfileARN`, `KeyName`, `SpotEnabled`, `MaxRuntimeMinutes`, `LogLevel`, `LaunchTemplateName`, `RunnerImage`, `Tags`.
  - **Cache / Admin:** `CacheSecret`, `CacheURL`, `AdminSecret`.
  - **K8s:** `KubeConfig`, `KubeNamespace`, `KubeServiceAccount`, `KubeNodeSelector`, `KubeTolerations`, `KubeRunnerImage`, `KubeIdleTimeoutMinutes`, `KubeReleaseName`, `KubeStorageClass`, `KubeResourceLabels`.
  - **K8s DinD:** `KubeDindImage`, `KubeDaemonJSONConfigMap`, `KubeDockerWaitSeconds`, `KubeDockerGroupGID`, `KubeRegistryMirror`.
  - **Valkey:** `ValkeyAddr`, `ValkeyPassword`, `ValkeyDB`.
  - **Metrics:** `MetricsNamespace`, `MetricsCloudWatchEnabled`, `MetricsPrometheusEnabled`, `MetricsPrometheusPath`, `MetricsDatadogEnabled`, `MetricsDatadogAddr`, `MetricsDatadogTags`, `MetricsDatadogSampleRate`, `MetricsDatadogBufferPoolSize`, `MetricsDatadogWorkersCount`, `MetricsDatadogMaxMsgsPerPayload`.
  - **Secrets / Vault:** `SecretsBackend`, `SecretsPathPrefix`, `VaultAddr`, `VaultKVMount`, `VaultKVVersion`, `VaultBasePath`, `VaultAuthMethod`, `VaultAWSRole`, `VaultK8sAuthMount`, `VaultK8sRole`, `VaultK8sJWTPath`.

### Functions / methods
- `Load() (*Config, error)` — reads env, parses, calls `Validate`, returns the config. Errors wrap with `"config error: %w"` or `"config validation failed: %w"`.
- `(*Config).IsEC2Backend() bool` — true when `DefaultBackend == "ec2"` or empty.
- `(*Config).IsK8sBackend() bool` — true when `DefaultBackend == "k8s"`.
- `(*Config).Validate() error` — top-level validator, dispatching by backend.

## Data [coverage: high -- 3 sources]

Selected env-var → field mappings (full list lives in `docs/CONFIGURATION.md`):

| Env var | Field | Default | Required when |
|---|---|---|---|
| `AWS_REGION` | `AWSRegion` | `ap-northeast-1` | always |
| `RUNS_FLEET_MODE` | `DefaultBackend` | `ec2` | always |
| `RUNS_FLEET_GITHUB_WEBHOOK_SECRET` | `GitHubWebhookSecret` | — | always |
| `RUNS_FLEET_GITHUB_APP_ID` | `GitHubAppID` | — | always |
| `RUNS_FLEET_GITHUB_APP_PRIVATE_KEY` | `GitHubAppPrivateKey` | — | always |
| `RUNS_FLEET_QUEUE_URL` | `QueueURL` | — | EC2 backend |
| `RUNS_FLEET_VALKEY_ADDR` | `ValkeyAddr` | `valkey:6379` | K8s backend |
| `RUNS_FLEET_VPC_ID` | `VPCID` | — | EC2 |
| `RUNS_FLEET_SECURITY_GROUP_ID` | `SecurityGroupID` | — | EC2 |
| `RUNS_FLEET_INSTANCE_PROFILE_ARN` | `InstanceProfileARN` | — | EC2 |
| `RUNS_FLEET_PUBLIC_SUBNET_IDS` / `RUNS_FLEET_PRIVATE_SUBNET_IDS` | `PublicSubnetIDs` / `PrivateSubnetIDs` | — | at least one for EC2 |
| `RUNS_FLEET_RUNNER_IMAGE` | `RunnerImage` | — | EC2 (must match ECR URL regex) |
| `RUNS_FLEET_SPOT_ENABLED` | `SpotEnabled` | `true` | — |
| `RUNS_FLEET_MAX_RUNTIME_MINUTES` | `MaxRuntimeMinutes` | `360` | range 1–1440 |
| `RUNS_FLEET_CIRCUIT_BREAKER_TABLE` | `CircuitBreakerTable` | `runs-fleet-circuit-state` | — |
| `RUNS_FLEET_LAUNCH_TEMPLATE_NAME` | `LaunchTemplateName` | `runs-fleet-runner` | — |
| `RUNS_FLEET_TAGS` | `Tags` | — | EC2 only, JSON object |
| `RUNS_FLEET_KUBE_NAMESPACE` | `KubeNamespace` | — | K8s |
| `RUNS_FLEET_KUBE_RUNNER_IMAGE` | `KubeRunnerImage` | — | K8s |
| `RUNS_FLEET_KUBE_DOCKER_WAIT_SECONDS` | `KubeDockerWaitSeconds` | `120` | K8s, 10–300 |
| `RUNS_FLEET_KUBE_DOCKER_GROUP_GID` | `KubeDockerGroupGID` | `123` | K8s, 1–65535 |
| `RUNS_FLEET_KUBE_IDLE_TIMEOUT_MINUTES` | `KubeIdleTimeoutMinutes` | `10` | K8s |
| `RUNS_FLEET_KUBE_NODE_SELECTOR` | `KubeNodeSelector` | — | K8s, `key=value,...` |
| `RUNS_FLEET_KUBE_TOLERATIONS` | `KubeTolerations` | — | K8s, JSON array |
| `RUNS_FLEET_KUBE_RESOURCE_LABELS` | `KubeResourceLabels` | — | K8s, JSON object |
| `RUNS_FLEET_SECRETS_BACKEND` | `SecretsBackend` | `ssm` | EC2 |
| `VAULT_ADDR` | `VaultAddr` | — | required if backend = `vault` |
| `VAULT_AUTH_METHOD` | `VaultAuthMethod` | `aws` | — |
| `VAULT_K8S_ROLE` | `VaultK8sRole` | — | required for `kubernetes`/`k8s`/`jwt` auth |
| `VAULT_KV_VERSION` | `VaultKVVersion` | `0` (auto) | must be 0, 1, or 2 |
| `RUNS_FLEET_METRICS_DATADOG_ENABLED` | `MetricsDatadogEnabled` | `false` | — |
| `RUNS_FLEET_METRICS_DATADOG_ADDR` | `MetricsDatadogAddr` | `127.0.0.1:8125` | required if Datadog enabled |
| `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` | `MetricsCloudWatchEnabled` | `true` | — |

### Parsers and validators
- `splitAndFilter` — comma-split + trim for subnet IDs and Datadog tags.
- `parseNodeSelector` — `key=value,...`, validates K8s label key/value rules.
- `parseTolerations` — JSON array → `[]Toleration`.
- `parseResourceLabels` — JSON object; rejects `app`, `runs-fleet.io/`, `kubernetes.io/`, `k8s.io/` prefixes.
- `parseTags` — JSON object; max 35 custom tags (50 AWS limit minus 15 reserved system tags), key ≤ 128 chars, value ≤ 256 chars, rejects `aws:` and `runs-fleet:` prefixes.
- `validateECRImageURL` — enforces `<account>.dkr.ecr.<region>.amazonaws.com/<repo>[:tag][@sha256:...]`.
- `validateHostPort` — `host:port` parse, port range 1–65535.

## Key Decisions [coverage: high -- 3 sources]
- **12-factor.** All settings come from env; no config files. The package starts with `// Package config manages application configuration from environment variables.`
- **Default region `ap-northeast-1`.** Hardcoded in `Load`, biased to the operator's home region.
- **Single `Config` struct.** No per-subsystem config types; downstream packages pull only the fields they need.
- **Backend dispatch.** `RUNS_FLEET_MODE` toggles entire validation paths (`validateEC2Config` vs `validateK8sConfig`); EC2 is the implicit default when `DefaultBackend == ""`.
- **Per-component timeouts as constants.** `MessageProcessTimeout = 90s` is explicitly sized for synchronous EC2 Fleet creation; `MessageReceiveTimeout = 25s` matches SQS long-poll headroom.
- **Reserved label/tag namespaces.** System owns `runs-fleet:` (EC2 tags), `runs-fleet.io/` / `kubernetes.io/` / `k8s.io/` (K8s labels), and the literal `app` label.
- **Validator is structural, not network.** `Validate` checks shape (regex, ranges, required-ness) but does not contact AWS, Vault, or K8s — failures surface only when the AWS SDK client is later constructed and called.

## Gotchas [coverage: high -- 3 sources]
- **Missing required vars are validation errors, not panics.** `Load()` returns an error; the caller (typically `cmd/server/main.go`) decides whether to log-and-exit. The CLAUDE.md note about "panic" is approximate — the actual behavior is `return nil, fmt.Errorf(...)`.
- **`AWS_REGION` silently defaults to `ap-northeast-1`.** Operators in other regions who forget to set it will hit cross-region latency or "resource not found" errors before realizing the region is wrong.
- **Empty `RUNS_FLEET_MODE` is treated as `ec2`.** `IsEC2Backend()` returns true for `""` as well as `"ec2"`. Mistyping the value (e.g., `EC2`) falls through `Validate` because the case check is exact-string and rejects only non-empty unknowns.
- **EC2 vs K8s envs do not conflict — they're ignored by the wrong backend.** Setting K8s vars under `mode=ec2` is a no-op (only EC2 validation runs), and vice versa. The risk is silently running the wrong backend rather than getting a clear error.
- **Subnet config is "at least one of public OR private."** Setting only `RUNS_FLEET_PRIVATE_SUBNET_IDS` is valid; jobs with `public=true` labels in such a deployment will fail later in fleet creation, not at config load.
- **`RUNS_FLEET_RUNNER_IMAGE` regex is strict.** Only ECR URLs of the form `<12-digit-account>.dkr.ecr.<region>.amazonaws.com/...` pass. Docker Hub or GHCR images are rejected at load.
- **`VAULT_KV_VERSION` accepts only 0, 1, 2.** Any other integer (including negative) errors out with an explicit message.
- **Datadog address validates only when Datadog is enabled.** Garbage in `RUNS_FLEET_METRICS_DATADOG_ADDR` with `RUNS_FLEET_METRICS_DATADOG_ENABLED=false` does not fail.
- **Tolerations and resource labels are JSON, node selectors are `key=value,...`.** Easy to mix up the two formats; the JSON parsers report `invalid ... JSON: ...` while the selector parser reports `missing '='`.
- **Helper `getEnv` treats empty string as "unset."** An explicit `FOO=` in the environment is indistinguishable from `FOO` being absent — defaults always win for empty strings.
- **`Tags` is initialized as an empty map, not nil.** Callers can range over it safely without a nil check, even when `RUNS_FLEET_TAGS` is unset.

## Sources [coverage: high]
- [pkg/config/config.go](../../pkg/config/config.go)
- [pkg/config/timeouts.go](../../pkg/config/timeouts.go)
- [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)
