---
topic: Config + Bootstrap (env vars + AWS clients)
last_compiled: 2026-08-21
sources_count: 5
---

# Config + Bootstrap (env vars + AWS clients)

## Purpose [coverage: high -- 5 sources]
Centralized configuration loading for the orchestrator. The `config` package reads every runtime knob from environment variables, validates required fields, parses structured values (JSON tags, JSON hot-pool caps, subnet/scope lists, IANA timezone), and exposes a single `Config` struct consumed by every other package. It also defines shared timeout constants used by SQS pollers, message handlers, cleanup paths, and the AWS transport/observability middleware in `internal/awsobs`. [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md) is the user-facing reference for the same env vars. Since the 2026-06 K8s runner-backend removal there is no backend mode: EC2 is the only path, and `RUNS_FLEET_MODE` is deprecated (warned about and ignored).

## Architecture [coverage: high -- 5 sources]
Five artifacts make up the bootstrap layer:

1. [pkg/config/config.go](../../pkg/config/config.go) — declares `Config` (the struct), the `Load()` entrypoint, and the `Validate()` method tree (`validateEC2Config`, `validateSecretsConfig`, `validateVaultAuthConfig`, `validateMetricsConfig`, `validateOIDCConfig`). Helpers `getEnv`, `getEnvInt`, `getEnvBool`, `getEnvFloat`, `getEnvIntDefault`, `splitAndFilter` provide typed env reads. `parseTags`/`validateTags` consume the EC2 tag JSON; `validateECRImageURL`, `validateBaseURL`, and `validateHostPort` enforce value shape.
2. [pkg/config/timezone.go](../../pkg/config/timezone.go) — **new, PR #455.** `LoadReportLocation()` resolves `RUNS_FLEET_REPORT_TIMEZONE` (default `Asia/Seoul`) into a `*time.Location`; `(*Config).ReportLocation()` is the read accessor; `SetReportLocationForTest` is the test seam. The resolved location is stored in the **unexported** field `Config.reportLocation`, so it can only be read through the accessor.
3. [pkg/config/hot_pools.go](../../pkg/config/hot_pools.go) — **new.** `HotPoolCaps` (the fleet-wide safety ceilings), `DefaultHotPoolCaps()`, `(HotPoolCaps).WithDefaults()`, and `ParseHotPoolCaps(jsonStr)` for `RUNS_FLEET_HOT_POOL_CAPS`.
4. [pkg/config/timeouts.go](../../pkg/config/timeouts.go) — exports `ShortTimeout`, `MessageReceiveTimeout`, `MessageProcessTimeout`, `CleanupTimeout`, `MaxShutdownDrainDelay`, the `MaxBodySize` HTTP body cap, and a family of AWS transport constants consumed by `internal/awsobs`.
5. [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md) — tabular reference grouped by Core / Queues / DynamoDB / S3+SNS / Cost reporting / EC2 Fleet / Label Aliases / Hot Pools / Cache / Graceful Shutdown / Metrics / Tracing / Secrets / Vault / Logging+Admin / Admin OIDC.

Note: the package header says it "manages application configuration from environment variables." AWS SDK clients (SQS, EC2, DynamoDB, S3, SSM, CloudWatch) are **not** constructed inside `pkg/config` — `Config` carries the inputs (region, table names, queue URLs, bucket names) that `cmd/server` feeds to `awsconfig.LoadDefaultConfig` and the SDK service constructors ([cmd-server](cmd-server.md)). Two things are parsed outside this package and embedded: `Config.Tracing` comes from `tracing.ParseConfig()`, and the label-alias rules are only *carried* here as raw JSON (`LabelAliasesJSON`), parsed at startup by `pkg/github.ParseAliasRules`.

## Talks To [coverage: medium -- 3 sources]
- **Process environment** via `os.Getenv` (every field).
- **`pkg/tracing`** for `tracing.ParseConfig()` (the embedded `Tracing` field).
- **`internal/validation`** for `validation.ValidateK8sJWTPath` (Vault Kubernetes-auth JWT path — the "K8s" here is Vault's auth method, unrelated to the removed K8s runner backend).
- **`time.LoadLocation`** for the reporting zone (the only stdlib call in `Load` that can fail on data outside the process's own environment: it reads the platform tzdata).
- **`encoding/json`** for the EC2 tag JSON and, via a `Decoder` with `DisallowUnknownFields`, the hot-pool caps JSON.
- **`net/url`** for `validateBaseURL`; **`net.SplitHostPort` / `strconv`** for `validateHostPort` on the Datadog DogStatsD address.
- **Downstream consumers** — every package in the project layout reads fields off the returned `*Config`. `pkg/cost`, `pkg/housekeeping`'s fleet-cost sampler, and `pkg/admin`'s cost handler all read `ReportLocation()`; `pkg/pools` and `pkg/admin` read `HotPoolsEnabled`/`HotPoolCaps`.

## API Surface [coverage: high -- 5 sources]

### Constants
Timeouts ([pkg/config/timeouts.go](../../pkg/config/timeouts.go)):
- `ShortTimeout = 3s`
- `MessageReceiveTimeout = 25s`
- `MessageProcessTimeout = 90s` (sized for synchronous `CreateFleet`)
- `CleanupTimeout = 5s`
- `MaxShutdownDrainDelay = 15s` (ceiling `Validate` enforces on `ShutdownDrainDelay`)
- `MaxBodySize = 1 << 20` (1 MiB HTTP body cap)
- AWS transport (consumed by `internal/awsobs`): `AWSResponseHeaderTimeout = 10s`; `AWSSQSResponseHeaderTimeout = 25s` (must exceed the 20s SQS long-poll wait); `AWSTCPUserTimeout = 20s` (kernel-level TCP_USER_TIMEOUT for wedged sockets); `AWSKeepAliveIdle = 15s` / `AWSKeepAliveInterval = 5s` / `AWSKeepAliveCount = 4`; `AWSSlowCallThreshold = 2s`; `AWSPerOpTimeout = 15s` (per-operation cap; the middleware exempts SQS `ReceiveMessage`, whose long-poll legitimately exceeds it).

Hot-pool caps ([pkg/config/hot_pools.go](../../pkg/config/hot_pools.go)) — defaults / ceilings:

| Field | Default | Ceiling |
|---|---|---|
| `maxLingerMinutes` | 30 | 120 |
| `maxHot` | 3 | 10 |
| `minJobsToActivate` | 20 | — |
| `lookbackDays` | 7 | 90 |
| `burstGapMinutes` | 20 | 120 |

Reporting zone ([pkg/config/timezone.go](../../pkg/config/timezone.go)): `defaultReportTimezone = "Asia/Seoul"` (unexported).

### Types
- `Config` — single struct holding every setting. Fields by group:
  - **Core:** `AWSRegion`, `GitHubWebhookSecret`, `GitHubAppID`, `GitHubAppPrivateKey`, `LabelAliasesJSON`.
  - **Hot pools:** `HotPoolsEnabled bool`, `HotPoolCaps HotPoolCaps`.
  - **Queues:** `QueueURL`, `QueueDLQURL`, `PoolQueueURL`, `EventsQueueURL`, `TerminationQueueURL`, `HousekeepingQueueURL`.
  - **DynamoDB:** `JobsTableName`, `JobsPoolStatusGSI`, `JobsInstanceIDGSI`, `PoolsTableName`, `AuditTableName`, `CircuitBreakerTable`.
  - **S3 / SNS:** `CacheBucketName`, `CostReportSNSTopic`, `CostReportBucket`.
  - **EC2:** `VPCID`, `SubnetIDs`, `SecurityGroupID`, `InstanceProfileARN`, `KeyName`, `SpotEnabled`, `LaunchTemplateName`, `RunnerImage`, `Tags`, the cost-attribution tag remaps `TagKeyApplication`/`TagValueApplication`/`TagKeyService`/`TagValueService`, and four independently-tuned lifetime bounds: `MaxRuntimeMinutes` (bounds a job and gates the age-based orphan sweep), `UnclaimedInstanceGraceMinutes` (a cold-start instance that never claimed a job has no job record for any job-driven sweep to find), `CompletedInstanceGraceMinutes` (an instance whose job already reached a terminal state), `StoppedInstanceGraceHours` (untagged stopped instances that no other sweep can see; `0` disables because terminating a stopped instance destroys its volume).
  - **Cache / Admin:** `CacheSecret`, `BaseURL` (required, absolute https), `AdminRateLimit`, `TraceUIURL`, `ShutdownDrainDelay`, `OIDCIssuerURL`/`OIDCClientID`/`OIDCClientSecret`/`OIDCRedirectURL` (defaults to `{BaseURL}/api/auth/callback`)/`OIDCScopes`/`OIDCGroupsClaim`, `AdminSessionSecret`, `AdminSessionTTLMinutes`.
  - **Metrics:** `MetricsCloudWatchEnabled` (**default `false`** as of PR #456), `MetricsPrometheusEnabled`, `MetricsPrometheusPath`, `MetricsDatadogEnabled`, `MetricsDatadogAddr`, `MetricsDatadogTags`, `MetricsDatadogSampleRate`, `MetricsDatadogBufferPoolSize`, `MetricsDatadogWorkersCount`, `MetricsDatadogMaxMsgsPerPayload`. The metric name prefix is fixed in `pkg/metrics` (`RunsFleet` / `runs_fleet`) and is not configurable.
  - **Tracing:** `Tracing tracing.Config` (parsed by `pkg/tracing`, not here).
  - **Secrets / Vault:** `SecretsBackend`, `SecretsPathPrefix`, `VaultAddr`, `VaultKVMount`, `VaultKVVersion`, `VaultBasePath`, `VaultAuthMethod`, `VaultAWSRole`, `VaultK8sAuthMount`, `VaultK8sRole`, `VaultK8sJWTPath`.
  - **Unexported:** `reportLocation *time.Location`.
- `HotPoolCaps` — `MaxLingerMinutes`, `MaxHot`, `MinJobsToActivate`, `LookbackDays`, `BurstGapMinutes` (all `int`, JSON-tagged lowerCamelCase).

### Functions / methods
- `Load() (*Config, error)` — warns if the deprecated `RUNS_FLEET_MODE` is set, reads env, parses, calls `Validate`, returns the config. Errors wrap with `"config error: %w"` or `"config validation failed: %w"`.
- `(*Config).Validate() error` — required core fields (webhook secret, App ID/key, queue URL, BaseURL shape), the runtime/grace-period bounds, the drain-delay ceiling, then the EC2 / secrets / metrics / OIDC sub-validators.
- `LoadReportLocation() (*time.Location, error)`
- `(*Config).ReportLocation() *time.Location` — falls back to `time.UTC` for a nil receiver or an unset field.
- `(*Config).SetReportLocationForTest(loc *time.Location)`
- `DefaultHotPoolCaps() HotPoolCaps`
- `(HotPoolCaps).WithDefaults() HotPoolCaps` — per-field, idempotent
- `ParseHotPoolCaps(jsonStr string) (HotPoolCaps, error)`

## Data [coverage: high -- 5 sources]

Selected env-var → field mappings (full list lives in [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)):

| Env var | Field | Default | Required when |
|---|---|---|---|
| `AWS_REGION` | `AWSRegion` | `ap-northeast-1` | always |
| `RUNS_FLEET_GITHUB_WEBHOOK_SECRET` | `GitHubWebhookSecret` | — | always |
| `RUNS_FLEET_GITHUB_APP_ID` | `GitHubAppID` | — | always |
| `RUNS_FLEET_GITHUB_APP_PRIVATE_KEY` | `GitHubAppPrivateKey` | — | always |
| `RUNS_FLEET_QUEUE_URL` | `QueueURL` | — | always |
| `RUNS_FLEET_BASE_URL` | `BaseURL` | — | always (absolute `https://` URL) |
| `RUNS_FLEET_REPORT_TIMEZONE` | `reportLocation` | `Asia/Seoul` | must be a loadable IANA zone |
| `RUNS_FLEET_LABEL_ALIASES` | `LabelAliasesJSON` | — | optional JSON array |
| `RUNS_FLEET_HOT_POOLS_ENABLED` | `HotPoolsEnabled` | `false` | — |
| `RUNS_FLEET_HOT_POOL_CAPS` | `HotPoolCaps` | all-defaults | JSON object; unknown fields rejected |
| `RUNS_FLEET_VPC_ID` | `VPCID` | — | always |
| `RUNS_FLEET_SECURITY_GROUP_ID` | `SecurityGroupID` | — | always |
| `RUNS_FLEET_INSTANCE_PROFILE_ARN` | `InstanceProfileARN` | — | always |
| `RUNS_FLEET_SUBNET_IDS` | `SubnetIDs` | — | always |
| `RUNS_FLEET_RUNNER_IMAGE` | `RunnerImage` | — | always (must match ECR URL regex) |
| `RUNS_FLEET_SPOT_ENABLED` | `SpotEnabled` | `true` | — |
| `RUNS_FLEET_MAX_RUNTIME_MINUTES` | `MaxRuntimeMinutes` | `360` | range 1–1440 |
| `RUNS_FLEET_UNCLAIMED_INSTANCE_GRACE_MINUTES` | `UnclaimedInstanceGraceMinutes` | `30` | ≥ 5 |
| `RUNS_FLEET_COMPLETED_INSTANCE_GRACE_MINUTES` | `CompletedInstanceGraceMinutes` | `15` | ≥ 5 |
| `RUNS_FLEET_STOPPED_INSTANCE_GRACE_HOURS` | `StoppedInstanceGraceHours` | `0` (disabled) | `0` or ≥ 1 |
| `RUNS_FLEET_SHUTDOWN_DRAIN_DELAY_SECONDS` | `ShutdownDrainDelay` | `5` | 0 … `MaxShutdownDrainDelay` (15s) |
| `RUNS_FLEET_JOBS_POOL_STATUS_GSI` | `JobsPoolStatusGSI` | — | optional |
| `RUNS_FLEET_JOBS_INSTANCE_ID_GSI` | `JobsInstanceIDGSI` | — | optional |
| `RUNS_FLEET_AUDIT_TABLE` | `AuditTableName` | — | optional (unset = audit persistence off) |
| `RUNS_FLEET_CIRCUIT_BREAKER_TABLE` | `CircuitBreakerTable` | `runs-fleet-circuit-state` | — |
| `RUNS_FLEET_LAUNCH_TEMPLATE_NAME` | `LaunchTemplateName` | `runs-fleet-runner` | — |
| `RUNS_FLEET_TAGS` | `Tags` | — | JSON object |
| `RUNS_FLEET_TAG_KEY_APPLICATION` / `_VALUE_APPLICATION` | `TagKeyApplication` / `TagValueApplication` | `Application` / `runs-fleet` | — |
| `RUNS_FLEET_TAG_KEY_SERVICE` / `_VALUE_SERVICE` | `TagKeyService` / `TagValueService` | `Service` / `runner` | — |
| `RUNS_FLEET_ADMIN_RATE_LIMIT` | `AdminRateLimit` | `60` | — |
| `RUNS_FLEET_ADMIN_OIDC_ISSUER_URL` (+ `_CLIENT_ID`, `_CLIENT_SECRET`, `_REDIRECT_URL`, `_SCOPES`, `_GROUPS_CLAIM`) | `OIDC*` | scopes `openid,profile,email`; claim `groups` | all-or-nothing with the session secret |
| `RUNS_FLEET_ADMIN_SESSION_SECRET` / `_TTL_MINUTES` | `AdminSessionSecret` / `AdminSessionTTLMinutes` | TTL `480` | required once any OIDC var is set |
| `RUNS_FLEET_SECRETS_BACKEND` | `SecretsBackend` | `ssm` | — |
| `VAULT_ADDR` | `VaultAddr` | — | required if backend = `vault` |
| `VAULT_AUTH_METHOD` | `VaultAuthMethod` | `aws` | — |
| `VAULT_K8S_ROLE` | `VaultK8sRole` | — | required for `kubernetes`/`k8s`/`jwt` auth |
| `VAULT_KV_VERSION` | `VaultKVVersion` | `0` (auto) | must be 0, 1, or 2 |
| `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` | `MetricsCloudWatchEnabled` | **`false`** (was `true` before PR #456) | — |
| `RUNS_FLEET_METRICS_PROMETHEUS_ENABLED` / `_PATH` | `MetricsPrometheusEnabled` / `Path` | `false` / `/metrics` | — |
| `RUNS_FLEET_METRICS_DATADOG_ENABLED` | `MetricsDatadogEnabled` | `false` | — |
| `RUNS_FLEET_METRICS_DATADOG_ADDR` | `MetricsDatadogAddr` | `127.0.0.1:8125` | required if Datadog enabled |

`RUNS_FLEET_MODE` is deprecated: `Load()` logs a warning and ignores it (the K8s runner backend was removed 2026-06). All former `RUNS_FLEET_KUBE_*` / `RUNS_FLEET_VALKEY_*` vars are gone. Tracing env vars (`RUNS_FLEET_TRACING_ENABLED`, `RUNS_FLEET_OTEL_*`) are parsed by `tracing.ParseConfig()`.

### Parsers and validators
- `splitAndFilter` — comma-split + trim for subnet IDs, Datadog tags, and OIDC scopes.
- `parseTags` — JSON object; max 35 custom tags (50 AWS limit minus 15 reserved system tags), key ≤ 128 chars, value ≤ 256 chars, rejects `aws:` and `runs-fleet:` prefixes.
- `ParseHotPoolCaps` — blank input yields all defaults; unknown JSON fields are rejected (`DisallowUnknownFields`); any negative value errors; `WithDefaults()` fills zero fields per-field; each ceiling is then checked with a named error.
- `LoadReportLocation` — `time.LoadLocation` on the env value (or the `Asia/Seoul` default); a parse failure is returned as an error, never swallowed.
- `validateECRImageURL` — enforces `<12-digit-account>.dkr.ecr.<region>.amazonaws.com/<repo>[:tag][@sha256:...]`.
- `validateBaseURL` — absolute `https://` URL with a host; applied to both `BaseURL` and `OIDCIssuerURL`.
- `validateHostPort` — `host:port` parse, port range 1–65535.

## Key Decisions [coverage: high -- 5 sources]
- **Fail-fast over nil-degrade, when degrading costs money.** This is the package's governing principle and the reason two of the newest knobs are hard startup errors rather than fallbacks. A misconfigured orchestrator that keeps booting keeps accepting webhooks and burning EC2 spend on jobs it will mishandle; a crash-looping pod is loud, cheap, and self-announcing. `Load` therefore returns an error (and `cmd/server` exits) for an unparseable reporting zone, an out-of-range hot-pool cap, a bad `VAULT_KV_VERSION`, a partial OIDC configuration, a non-ECR runner image, and an out-of-range grace period.
- **2026-08 (PR #455): `RUNS_FLEET_REPORT_TIMEZONE`, default `Asia/Seoul`, unparseable = hard error.** The default is not UTC because the fleet is operated from Korea: a UTC day boundary splits the local working day at 09:00, so a "cost per day" chart would cut every day mid-morning and a month-to-date total would roll over mid-morning on the 1st. And the failure mode is the reason for the hard error — *bucketing cost into the wrong days is invisible once it starts*. There is no alarm for "this chart is silently offset by nine hours"; the deployment would carry the mistake indefinitely. So a typo in the zone name stops the process instead.
- **`ReportLocation()` falls back to UTC only for a nil or zero `Config`.** Production always goes through `Load`, which either sets a real location or fails. The fallback exists so a test or a partially-built fixture that formats a timestamp cannot nil-panic a caller — a test-ergonomics concession, deliberately *not* a production code path. Keeping `reportLocation` unexported is what enforces that: no caller can read a nil location directly.
- **2026-08 (PR #456): `MetricsCloudWatchEnabled` default flipped `true` → `false`.** Nothing queried the `RunsFleet` namespace (no alarms, no dashboards, and the admin console reads DynamoDB) while every metric cost one un-batched `PutMetricData` request plus a monthly charge per unique dimension combination. Only the default changed — the backend and every call site are untouched, and re-enabling is one env var. The Helm chart sets the variable unconditionally from `values.yaml`, so the Go default alone would not have reached a deployment; both changed together.
- **Hot pools get exactly one master toggle.** `RUNS_FLEET_HOT_POOLS_ENABLED` is the only chart knob that turns the feature on; there is deliberately no per-pool Helm config, because most repo users cannot touch the deployment chart. Per-pool linger/maxHot is auto-deduced from each pool's real run history by an hourly tuner and optionally overridden in the admin UI, always clamped to `HotPoolCaps`. The tuner and the override validation read the *same* `HotPoolCaps`, so there is one source of truth and a runaway recommendation cannot burn unbounded EC2 spend. With the toggle off the pool path is byte-identical to a build without the feature.
- **Hot-pool caps are validated at startup, not at reconcile time.** A misconfigured chart requesting an absurd hot footprint fails the process rather than surfacing 60 seconds later as a strange reconcile decision. `WithDefaults()` is per-field and idempotent so no consumer has to guess which fields are optional or use one field as a canary for the rest.
- **12-factor.** All settings come from env; no config files.
- **Default region `ap-northeast-1`.** Hardcoded in `Load`, biased to the operator's home region.
- **Single `Config` struct.** No per-subsystem config types; downstream packages pull only the fields they need. (Exception: `Tracing` embeds `tracing.Config`.)
- **No backend modes (2026-06).** `RUNS_FLEET_MODE` is deprecated-but-tolerated: setting it produces a warning instead of an error, so deployments carrying it across the K8s-backend removal keep booting. This is the one place the package prefers tolerance over fail-fast, because the variable is inert — it cannot cause a wrong action.
- **`BaseURL` is required and https-only.** It feeds the S3 Actions cache URLs handed to runners and the OIDC redirect default.
- **OIDC config is all-or-nothing.** `validateOIDCConfig` treats any one of issuer / client-id / client-secret / session-secret being set as intent to enable admin auth and errors on missing companions — a typo cannot silently disable auth. Checks are explicit (not a map + loop) so the reported missing field is deterministic.
- **Four independent instance-lifetime bounds, each justified by what *cannot* see the instance otherwise.** `UnclaimedInstanceGraceMinutes` exists because an instance that never claimed a job has no job record for any job-driven sweep to find; `CompletedInstanceGraceMinutes` because otherwise a finished ephemeral runner bills until `MaxRuntimeMinutes` even though every sweep can see the job is done; `StoppedInstanceGraceHours` because the pool reconciler only manages *tagged* members and every other sweep filters to *running* instances, so an untagged stopped instance bills for its EBS volume forever — and it defaults to `0` precisely because terminating a stopped instance destroys that volume.
- **Per-component timeouts as constants.** `MessageProcessTimeout = 90s` is explicitly sized for synchronous EC2 Fleet creation; `MessageReceiveTimeout = 25s` matches SQS long-poll headroom. The AWS transport constants encode their interplay in comments: `AWSSQSResponseHeaderTimeout` must exceed the 20s long-poll, `AWSPerOpTimeout` sits below it (so the middleware exempts `ReceiveMessage`), and `AWSTCPUserTimeout` stays below `MessageProcessTimeout`.
- **Reserved tag namespaces.** System owns `runs-fleet:` and rejects `aws:` on custom EC2 tags. Cost-attribution tag keys *and* values are independently remappable for forks with a different tag policy (PR #389); overriding the *value* is the only safe path for e.g. `Application=my-org-infra`, since a duplicate custom tag on the same key has undocumented EC2 collision behavior within one `CreateFleet` TagSpecification.
- **Validator is structural, not network.** `Validate` checks shape (regex, ranges, required-ness) but does not contact AWS, Vault, or the OIDC issuer — those failures surface when the client is later constructed and called (the OIDC issuer specifically at route setup, bounded by `oidcDiscoveryTimeout`).

## Gotchas [coverage: high -- 5 sources]
- **A bad `RUNS_FLEET_REPORT_TIMEZONE` kills the process, including on a platform with no tzdata.** `time.LoadLocation` reads the system zoneinfo database; a minimal container image without tzdata fails *every* non-UTC zone name, so a slimmed base image turns the default `Asia/Seoul` into a startup crash. `UTC` and `Local` are the only names resolvable without tzdata.
- **Changing the reporting zone does not re-bucket history.** Existing `__fleet_day:` rows in the pools table keep the keys they were written with; only future samples move ([docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)). A zone change therefore leaves a seam in the daily chart.
- **`ReportLocation()`'s UTC fallback can mask a construction bug.** Any code path that builds a `Config` literal instead of calling `Load` (tests, fixtures) silently gets UTC bucketing. That is the intent for tests, but it means "the cost chart is nine hours off" in a non-production harness is not necessarily a config error.
- **Metrics are now off by default in aggregate, not just for CloudWatch.** With `MetricsCloudWatchEnabled` false and the other two backends defaulting false, a deployment that sets no metrics env vars gets `metrics.NoopPublisher{}` from `initMetrics` — and no "metrics initialized" log line either ([cmd-server](cmd-server.md)).
- **`RUNS_FLEET_HOT_POOL_CAPS` rejects unknown fields.** A renamed or misspelled key (`maxLinger` for `maxLingerMinutes`) is a startup error, not a silently-defaulted field. This is deliberate but surprises anyone used to lenient JSON config.
- **A zero hot-pool cap field means "default", not "zero".** `WithDefaults()` cannot distinguish an explicit `0` from an absent field, so `{"maxHot": 0}` yields `maxHot: 3`. To force a pool cold, use the admin per-pool override (`0`), not the caps.
- **Missing required vars are validation errors, not panics.** `Load()` returns an error; `cmd/server/main.go` logs and exits.
- **`AWS_REGION` silently defaults to `ap-northeast-1`.** Operators in other regions who forget to set it hit cross-region latency or "resource not found" errors before realizing the region is wrong.
- **`RUNS_FLEET_MODE=k8s` no longer selects anything.** It only produces a startup warning; the process continues as an EC2 orchestrator.
- **`RUNS_FLEET_BASE_URL` must be `https://`.** `http://localhost:8080` fails `validateBaseURL` even for local dev.
- **`RUNS_FLEET_RUNNER_IMAGE` regex is strict.** Only ECR URLs of the form `<12-digit-account>.dkr.ecr.<region>.amazonaws.com/...` pass; Docker Hub or GHCR images are rejected at load.
- **Grace-period floors are enforced, and their lower bounds are load-bearing.** `UNCLAIMED_INSTANCE_GRACE_MINUTES < 5` is rejected because anything shorter would reap instances still on their way to claiming a job; `COMPLETED_INSTANCE_GRACE_MINUTES < 5` because the job record goes terminal *before* the agent finishes cleanup and self-terminates.
- **Datadog address validates only when Datadog is enabled.** Garbage in `RUNS_FLEET_METRICS_DATADOG_ADDR` with the backend disabled does not fail.
- **Numeric/boolean env parsing is inconsistent by design.** `getEnvInt` (used for the runtime/grace-period fields and `VAULT_KV_VERSION`) errors on garbage; `getEnvBool`, `getEnvFloat`, and `getEnvIntDefault` log a warning and fall back to the default. A typo in `RUNS_FLEET_SPOT_ENABLED` silently re-enables spot, and a typo in `RUNS_FLEET_HOT_POOLS_ENABLED` silently leaves hot pools off.
- **Datadog sample rate is clamped, not rejected.** Values outside [0,1] are clamped with a warning; a negative buffer-pool size clamps to 0.
- **Helper `getEnv` treats empty string as "unset."** An explicit `FOO=` is indistinguishable from `FOO` being absent — defaults always win for empty strings, including for the reporting timezone.
- **`Tags` is initialized as an empty map, not nil.** Callers can range over it safely without a nil check, even when `RUNS_FLEET_TAGS` is unset.

## Sources [coverage: high]
- [pkg/config/config.go](../../pkg/config/config.go)
- [pkg/config/timezone.go](../../pkg/config/timezone.go)
- [pkg/config/hot_pools.go](../../pkg/config/hot_pools.go)
- [pkg/config/timeouts.go](../../pkg/config/timeouts.go)
- [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)
