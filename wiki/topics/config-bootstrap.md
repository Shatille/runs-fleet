---
topic: Config + Bootstrap (env vars + AWS clients)
last_compiled: 2026-07-21
sources_count: 3
---

# Config + Bootstrap (env vars + AWS clients)

## Purpose [coverage: medium -- 3 sources]
Centralized configuration loading for the orchestrator. The `config` package reads every runtime knob from environment variables, validates required fields, parses structured values (JSON tags, subnet/scope lists), and exposes a single `Config` struct consumed by every other package. It also defines shared timeout constants used by SQS pollers, message handlers, cleanup paths, and the AWS transport/observability middleware in `internal/awsobs`. `docs/CONFIGURATION.md` is the user-facing reference for the same env vars. Since the 2026-06 K8s runner-backend removal there is no backend mode: EC2 is the only path, and `RUNS_FLEET_MODE` is deprecated (warned about and ignored).

## Architecture [coverage: medium -- 3 sources]
Three artifacts make up the bootstrap layer:

1. `pkg/config/config.go` — declares `Config` (the struct), the `Load()` entrypoint, and the `Validate()` method tree (`validateEC2Config`, `validateSecretsConfig`, `validateVaultAuthConfig`, `validateMetricsConfig`, `validateOIDCConfig`). Helpers `getEnv`, `getEnvInt`, `getEnvBool`, `getEnvFloat`, `getEnvIntDefault`, `splitAndFilter` provide typed env reads. `parseTags`/`validateTags` consume the EC2 tag JSON; `validateECRImageURL`, `validateBaseURL`, and `validateHostPort` enforce value shape. The former `BackendEC2`/`BackendK8s` constants, `Toleration` type, and K8s/Valkey parsing (`parseNodeSelector`, `parseTolerations`, `parseResourceLabels`) were deleted with the K8s backend (2026-06).
2. `pkg/config/timeouts.go` — exports `ShortTimeout`, `MessageReceiveTimeout`, `MessageProcessTimeout`, `CleanupTimeout`, the `MaxBodySize` HTTP body cap, and a family of AWS transport constants (`AWSResponseHeaderTimeout`, `AWSSQSResponseHeaderTimeout`, `AWSTCPUserTimeout`, `AWSKeepAliveIdle`/`Interval`/`Count`, `AWSSlowCallThreshold`, `AWSPerOpTimeout`) consumed by `internal/awsobs`.
3. `docs/CONFIGURATION.md` — tabular reference grouped by Core / Queues / DynamoDB / S3+SNS / EC2 Fleet / Label Aliases / Cache / Metrics / Tracing / Secrets / Vault / Logging+Admin / Admin OIDC.

Note: the file header says the package "manages application configuration from environment variables." AWS SDK clients (SQS, EC2, DynamoDB, S3, SSM, CloudWatch) are not constructed inside `pkg/config` itself — `Config` carries the inputs (region, table names, queue URLs, bucket names) that callers feed to `aws-config.LoadDefaultConfig` and SDK service constructors elsewhere in the codebase. One exception to "everything is parsed here": tracing env vars are parsed by `tracing.ParseConfig()` in `pkg/tracing` and embedded as `Config.Tracing`.

## Talks To [coverage: medium -- 3 sources]
- **Process environment** via `os.Getenv` (every field).
- **`pkg/tracing`** for `tracing.ParseConfig()` (the embedded `Tracing` field).
- **`internal/validation`** for `validation.ValidateK8sJWTPath` (Vault Kubernetes-auth JWT path — the "K8s" here is Vault's auth method, unrelated to the removed K8s runner backend).
- **`encoding/json`** for the EC2 tag JSON input.
- **`net/url`** for `validateBaseURL`; **`net.SplitHostPort` / `strconv`** for `validateHostPort` on the Datadog DogStatsD address.
- **Downstream consumers** — every package listed in the project layout (`pkg/fleet`, `pkg/queue`, `pkg/db`, `pkg/cache`, `pkg/secrets`, `pkg/metrics`, `pkg/admin`, `pkg/github`, ...) reads fields off the returned `*Config`.

## API Surface [coverage: medium -- 3 sources]

### Constants
Timeouts (`pkg/config/timeouts.go`):
- `ShortTimeout = 3 * time.Second`
- `MessageReceiveTimeout = 25 * time.Second`
- `MessageProcessTimeout = 90 * time.Second` (sized for synchronous `CreateFleet`)
- `CleanupTimeout = 5 * time.Second`
- `MaxBodySize = 1 << 20` (1 MiB HTTP body cap)
- AWS transport (consumed by `internal/awsobs`): `AWSResponseHeaderTimeout = 10s`; `AWSSQSResponseHeaderTimeout = 25s` (must exceed the 20s SQS long-poll wait); `AWSTCPUserTimeout = 20s` (kernel-level TCP_USER_TIMEOUT for wedged sockets); `AWSKeepAliveIdle = 15s` / `AWSKeepAliveInterval = 5s` / `AWSKeepAliveCount = 4`; `AWSSlowCallThreshold = 2s` (slow-call logging bound); `AWSPerOpTimeout = 15s` (per-operation cap; the middleware exempts SQS ReceiveMessage, whose long-poll legitimately exceeds it).

### Types
- `Config` — single struct holding every setting. Fields by group:
  - **Core:** `AWSRegion`, `GitHubWebhookSecret`, `GitHubAppID`, `GitHubAppPrivateKey`, `LabelAliasesJSON` (raw JSON, validated at startup by `pkg/github.ParseAliasRules`).
  - **Queues:** `QueueURL`, `QueueDLQURL`, `PoolQueueURL`, `EventsQueueURL`, `TerminationQueueURL`, `HousekeepingQueueURL`.
  - **DynamoDB:** `JobsTableName`, `JobsPoolStatusGSI`, `JobsInstanceIDGSI`, `PoolsTableName`, `AuditTableName` (2026-07, admin audit persistence — unset disables it), `CircuitBreakerTable`.
  - **S3 / SNS:** `CacheBucketName`, `CostReportSNSTopic`, `CostReportBucket`.
  - **EC2:** `VPCID`, `SubnetIDs`, `SecurityGroupID`, `InstanceProfileARN`, `KeyName`, `SpotEnabled`, `MaxRuntimeMinutes`, `LaunchTemplateName`, `RunnerImage`, `Tags`, plus cost-attribution tag remaps `TagKeyApplication`/`TagValueApplication`/`TagKeyService`/`TagValueService` (defaults `Application`=`runs-fleet`, `Service`=`runner`; keys and values independently configurable since PR #389 — values were previously fixed).
  - **Cache / Admin:** `CacheSecret`, `BaseURL` (required, absolute https), `AdminRateLimit`, `TraceUIURL`, `OIDCIssuerURL`/`OIDCClientID`/`OIDCClientSecret`/`OIDCRedirectURL` (defaults to `{BaseURL}/api/auth/callback`)/`OIDCScopes`/`OIDCGroupsClaim`, `AdminSessionSecret`, `AdminSessionTTLMinutes` (native OIDC as of 2026-07; superseded the earlier `AdminSecret` toggle).
  - **Metrics:** `MetricsCloudWatchEnabled`, `MetricsPrometheusEnabled`, `MetricsPrometheusPath`, `MetricsDatadogEnabled`, `MetricsDatadogAddr`, `MetricsDatadogTags`, `MetricsDatadogSampleRate`, `MetricsDatadogBufferPoolSize`, `MetricsDatadogWorkersCount`, `MetricsDatadogMaxMsgsPerPayload`. The metric name prefix is fixed in `pkg/metrics` (`RunsFleet` / `runs_fleet`) and is not configurable.
  - **Tracing:** `Tracing tracing.Config` (parsed by `pkg/tracing`, not here).
  - **Secrets / Vault:** `SecretsBackend`, `SecretsPathPrefix`, `VaultAddr`, `VaultKVMount`, `VaultKVVersion`, `VaultBasePath`, `VaultAuthMethod`, `VaultAWSRole`, `VaultK8sAuthMount`, `VaultK8sRole`, `VaultK8sJWTPath`.

### Functions / methods
- `Load() (*Config, error)` — warns if the deprecated `RUNS_FLEET_MODE` is set, reads env, parses, calls `Validate`, returns the config. Errors wrap with `"config error: %w"` or `"config validation failed: %w"`.
- `(*Config).Validate() error` — top-level validator: required core fields (webhook secret, App ID/key, queue URL, BaseURL shape, runtime bounds), then the EC2 / secrets / metrics / OIDC sub-validators. The former `IsEC2Backend()`/`IsK8sBackend()` methods are gone.

## Data [coverage: medium -- 3 sources]

Selected env-var → field mappings (full list lives in `docs/CONFIGURATION.md`):

| Env var | Field | Default | Required when |
|---|---|---|---|
| `AWS_REGION` | `AWSRegion` | `ap-northeast-1` | always |
| `RUNS_FLEET_GITHUB_WEBHOOK_SECRET` | `GitHubWebhookSecret` | — | always |
| `RUNS_FLEET_GITHUB_APP_ID` | `GitHubAppID` | — | always |
| `RUNS_FLEET_GITHUB_APP_PRIVATE_KEY` | `GitHubAppPrivateKey` | — | always |
| `RUNS_FLEET_QUEUE_URL` | `QueueURL` | — | always |
| `RUNS_FLEET_BASE_URL` | `BaseURL` | — | always (absolute `https://` URL) |
| `RUNS_FLEET_LABEL_ALIASES` | `LabelAliasesJSON` | — | optional JSON array |
| `RUNS_FLEET_VPC_ID` | `VPCID` | — | always |
| `RUNS_FLEET_SECURITY_GROUP_ID` | `SecurityGroupID` | — | always |
| `RUNS_FLEET_INSTANCE_PROFILE_ARN` | `InstanceProfileARN` | — | always |
| `RUNS_FLEET_SUBNET_IDS` | `SubnetIDs` | — | always |
| `RUNS_FLEET_RUNNER_IMAGE` | `RunnerImage` | — | always (must match ECR URL regex) |
| `RUNS_FLEET_SPOT_ENABLED` | `SpotEnabled` | `true` | — |
| `RUNS_FLEET_MAX_RUNTIME_MINUTES` | `MaxRuntimeMinutes` | `360` | range 1–1440 |
| `RUNS_FLEET_JOBS_POOL_STATUS_GSI` | `JobsPoolStatusGSI` | — | optional |
| `RUNS_FLEET_JOBS_INSTANCE_ID_GSI` | `JobsInstanceIDGSI` | — | optional |
| `RUNS_FLEET_AUDIT_TABLE` | `AuditTableName` | — | optional (unset = audit persistence off) |
| `RUNS_FLEET_CIRCUIT_BREAKER_TABLE` | `CircuitBreakerTable` | `runs-fleet-circuit-state` | — |
| `RUNS_FLEET_LAUNCH_TEMPLATE_NAME` | `LaunchTemplateName` | `runs-fleet-runner` | — |
| `RUNS_FLEET_TAGS` | `Tags` | — | JSON object |
| `RUNS_FLEET_TAG_KEY_APPLICATION` | `TagKeyApplication` | `Application` | — |
| `RUNS_FLEET_TAG_VALUE_APPLICATION` | `TagValueApplication` | `runs-fleet` | — |
| `RUNS_FLEET_TAG_KEY_SERVICE` | `TagKeyService` | `Service` | — |
| `RUNS_FLEET_TAG_VALUE_SERVICE` | `TagValueService` | `runner` | — |
| `RUNS_FLEET_ADMIN_RATE_LIMIT` | `AdminRateLimit` | `60` | — |
| `RUNS_FLEET_ADMIN_OIDC_ISSUER_URL` (+ `_CLIENT_ID`, `_CLIENT_SECRET`, `_REDIRECT_URL`, `_SCOPES`, `_GROUPS_CLAIM`) | `OIDC*` | scopes `openid,profile,email`; claim `groups` | all-or-nothing with the session secret |
| `RUNS_FLEET_ADMIN_SESSION_SECRET` / `_TTL_MINUTES` | `AdminSessionSecret` / `AdminSessionTTLMinutes` | TTL `480` | required once any OIDC var is set |
| `RUNS_FLEET_SECRETS_BACKEND` | `SecretsBackend` | `ssm` | — |
| `VAULT_ADDR` | `VaultAddr` | — | required if backend = `vault` |
| `VAULT_AUTH_METHOD` | `VaultAuthMethod` | `aws` | — |
| `VAULT_K8S_ROLE` | `VaultK8sRole` | — | required for `kubernetes`/`k8s`/`jwt` auth |
| `VAULT_KV_VERSION` | `VaultKVVersion` | `0` (auto) | must be 0, 1, or 2 |
| `RUNS_FLEET_METRICS_DATADOG_ENABLED` | `MetricsDatadogEnabled` | `false` | — |
| `RUNS_FLEET_METRICS_DATADOG_ADDR` | `MetricsDatadogAddr` | `127.0.0.1:8125` | required if Datadog enabled |
| `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` | `MetricsCloudWatchEnabled` | `true` | — |

`RUNS_FLEET_MODE` is deprecated: `Load()` logs a warning and ignores it (the K8s runner backend was removed 2026-06). All former `RUNS_FLEET_KUBE_*` / `RUNS_FLEET_VALKEY_*` vars are gone. Tracing env vars (`RUNS_FLEET_TRACING_ENABLED`, `RUNS_FLEET_OTEL_*`) are parsed by `tracing.ParseConfig()` in `pkg/tracing`.

### Parsers and validators
- `splitAndFilter` — comma-split + trim for subnet IDs, Datadog tags, and OIDC scopes.
- `parseTags` — JSON object; max 35 custom tags (50 AWS limit minus 15 reserved system tags), key ≤ 128 chars, value ≤ 256 chars, rejects `aws:` and `runs-fleet:` prefixes.
- `validateECRImageURL` — enforces `<12-digit-account>.dkr.ecr.<region>.amazonaws.com/<repo>[:tag][@sha256:...]`.
- `validateBaseURL` — absolute `https://` URL with a host; applied to both `BaseURL` and `OIDCIssuerURL`.
- `validateHostPort` — `host:port` parse, port range 1–65535.

## Key Decisions [coverage: medium -- 3 sources]
- **12-factor.** All settings come from env; no config files. The package starts with `// Package config manages application configuration from environment variables.`
- **Default region `ap-northeast-1`.** Hardcoded in `Load`, biased to the operator's home region.
- **Single `Config` struct.** No per-subsystem config types; downstream packages pull only the fields they need. (Exception: `Tracing` embeds `tracing.Config`, parsed by that package.)
- **No backend modes (2026-06).** `RUNS_FLEET_MODE` is deprecated-but-tolerated: setting it produces a warning instead of an error, so deployments carrying it across the K8s-backend removal keep booting. EC2 validation always runs; the `validateK8sConfig` path is gone.
- **`BaseURL` is required and https-only.** It feeds the S3 Actions cache URLs handed to runners (PR #325 made it mandatory to activate the cache) and the OIDC redirect default (`{BaseURL}/api/auth/callback`).
- **OIDC config is all-or-nothing.** `validateOIDCConfig` treats any one of issuer / client-id / client-secret / session-secret being set as intent to enable admin auth and errors on missing companions — a typo cannot silently disable auth. Checks are explicit (not a map + loop) so the reported missing field is deterministic.
- **Per-component timeouts as constants.** `MessageProcessTimeout = 90s` is explicitly sized for synchronous EC2 Fleet creation; `MessageReceiveTimeout = 25s` matches SQS long-poll headroom. The AWS transport constants encode their interplay in comments: `AWSSQSResponseHeaderTimeout` must exceed the 20s long-poll, `AWSPerOpTimeout` sits below it (so the middleware exempts ReceiveMessage), and `AWSTCPUserTimeout` stays below `MessageProcessTimeout`.
- **Reserved tag namespaces.** System owns `runs-fleet:` and rejects `aws:` on custom EC2 tags. Cost-attribution tag keys *and* values are independently remappable (`TagKeyApplication`/`TagValueApplication`, `TagKeyService`/`TagValueService`) for forks with a different tag policy. PR #389 made the values configurable — previously fixed at `runs-fleet`/`runner`, which remain the defaults, so unset is a no-op. Overriding the value is the only safe path for e.g. `Application=my-org-infra`: a duplicate custom tag on the same key has undocumented EC2 collision behavior within one `CreateFleet` TagSpecification.
- **Validator is structural, not network.** `Validate` checks shape (regex, ranges, required-ness) but does not contact AWS, Vault, or the OIDC issuer — failures surface only when the client is later constructed and called.

## Gotchas [coverage: medium -- 3 sources]
- **Missing required vars are validation errors, not panics.** `Load()` returns an error; the caller (typically `cmd/server/main.go`) decides whether to log-and-exit.
- **`AWS_REGION` silently defaults to `ap-northeast-1`.** Operators in other regions who forget to set it will hit cross-region latency or "resource not found" errors before realizing the region is wrong.
- **`RUNS_FLEET_MODE=k8s` no longer selects anything.** It only produces a startup warning; the process continues as an EC2 orchestrator. Manifests migrated from the pre-2026-06 K8s deployment get a log line, not an error.
- **`RUNS_FLEET_BASE_URL` must be `https://`.** `http://localhost:8080` fails `validateBaseURL` even for local dev.
- **`RUNS_FLEET_RUNNER_IMAGE` regex is strict.** Only ECR URLs of the form `<12-digit-account>.dkr.ecr.<region>.amazonaws.com/...` pass. Docker Hub or GHCR images are rejected at load.
- **`VAULT_KV_VERSION` accepts only 0, 1, 2.** Any other integer (including negative) errors out with an explicit message.
- **Datadog address validates only when Datadog is enabled.** Garbage in `RUNS_FLEET_METRICS_DATADOG_ADDR` with `RUNS_FLEET_METRICS_DATADOG_ENABLED=false` does not fail.
- **Numeric/boolean env parsing is inconsistent by design.** `getEnvInt` (via `Load`, for `RUNS_FLEET_MAX_RUNTIME_MINUTES` and `VAULT_KV_VERSION`) errors on garbage; `getEnvBool`, `getEnvFloat`, and `getEnvIntDefault` log a warning and fall back to the default. A typo in `RUNS_FLEET_SPOT_ENABLED` silently re-enables spot.
- **Datadog sample rate is clamped, not rejected.** Values outside [0,1] are clamped with a warning; a negative buffer-pool size clamps to 0.
- **Helper `getEnv` treats empty string as "unset."** An explicit `FOO=` in the environment is indistinguishable from `FOO` being absent — defaults always win for empty strings.
- **`Tags` is initialized as an empty map, not nil.** Callers can range over it safely without a nil check, even when `RUNS_FLEET_TAGS` is unset.

## Sources [coverage: medium]
- [pkg/config/config.go](../../pkg/config/config.go)
- [pkg/config/timeouts.go](../../pkg/config/timeouts.go)
- [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)
