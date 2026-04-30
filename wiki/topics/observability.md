---
topic: Observability (Metrics, Logging, Cost)
last_compiled: 2026-04-30
sources_count: 8
---

# Observability (Metrics, Logging, Cost)

## Purpose [coverage: high -- 8 sources]

runs-fleet emits multi-backend metrics, structured JSON logs, and daily cost
reports so operators can monitor fleet health, debug incidents across
distributed Fargate instances, and track AWS spend per workflow job.

The observability surface is split across three packages:

- `pkg/metrics` — A `Publisher` interface with three concrete backends
  (CloudWatch, Prometheus, Datadog DogStatsD) plus a fan-out `MultiPublisher`
  and a `NoopPublisher`. Backends are individually toggleable via env vars,
  so an operator can run "CloudWatch + Datadog", "Prometheus only", or any
  combination.
- `pkg/logging` — A thin wrapper around stdlib `log/slog` that pins the global
  handler to JSON-on-stdout, attaches a `host` attribute, and exposes a
  curated set of attribute keys (`KeyJobID`, `KeyPoolName`, etc.) and
  `log_type` categories so log aggregators can filter cleanly.
- `pkg/cost` — A daily report generator that pulls `RunsFleet` CloudWatch
  metrics, multiplies them by hard-coded ARM instance prices (with optional
  AWS Pricing API lookup), writes a markdown report to S3, and publishes the
  same body to an SNS topic.

The cost subsystem is explicit about its limitations: hard-coded prices for
three instance families, fixed 70% spot discount, and no data-transfer or
EBS line items.

## Architecture [coverage: high -- 8 sources]

### Metrics fan-out

`Publisher` (defined in `pkg/metrics/publisher.go`) is a 27-method interface
covering counters (`PublishJobSuccess`, `PublishCacheHit`), gauges
(`PublishQueueDepth`, `PublishFleetSize`, `PublishPoolUtilization`),
distributions (`PublishJobDuration`), and Datadog-specific surfaces
(`PublishServiceCheck`, `PublishEvent`).

`MultiPublisher` (`pkg/metrics/multi.go`) wraps an arbitrary list of
publishers. Each interface call invokes `publishAll`, which spawns a
goroutine per child, runs each publisher's method on a nested goroutine,
collects errors with a 5-second per-publisher timeout (`publishTimeout`),
logs warnings via the metrics logger, and returns the joined error set
(`errors.Join`). One slow backend cannot block the others.

```
caller → MultiPublisher.PublishX
            ├── goroutine → CloudWatchPublisher.PublishX  (5s timeout)
            ├── goroutine → PrometheusPublisher.PublishX  (5s timeout)
            └── goroutine → DatadogPublisher.PublishX     (5s timeout)
```

`NoopPublisher` (same file as the interface) is a zero-value-friendly stub
returned when metrics are disabled — every method returns `nil`. It exists
so callers can avoid `if publisher != nil` guards.

### Backend implementations

`CloudWatchPublisher` (`pkg/metrics/cloudwatch.go`) uses `cloudwatch.Client`
from AWS SDK v2. Default namespace is `RunsFleet` (overridable via
`NewCloudWatchPublisherWithNamespace`). Counters call `putMetric` (single
`MetricDatum` with `Value`); gauges call `putGaugeMetric` (uses
`StatisticValues` with `SampleCount=1` so CloudWatch treats the point as a
gauge sample). Dimensions are added inline for the four
dimension-bearing metrics: `PoolName` (`PoolUtilization`,
`PoolRunningJobs`), `TaskType` (`SchedulingFailure`), and `InstanceType`
(`CircuitBreakerTriggered`). `PublishServiceCheck` and `PublishEvent` are
no-ops on CloudWatch (Datadog-only surfaces).

`PrometheusPublisher` (`pkg/metrics/prometheus.go`) constructs a private
`prometheus.Registry` and pre-allocates one collector per metric:
`prometheus.Counter`/`Gauge`/`Histogram` for label-free metrics, and
`*CounterVec`/`*GaugeVec` for label-bearing ones. The histogram for
`job_duration_seconds` uses fixed buckets `[30, 60, 120, 300, 600, 900,
1800, 3600]`. Default namespace is `runs_fleet`. `Handler()` exposes the
registry for mounting at `/metrics`. `PublishServiceCheck` and
`PublishEvent` are no-ops.

`DatadogPublisher` (`pkg/metrics/datadog.go`) uses the official
`github.com/DataDog/datadog-go/v5/statsd` client. The namespace
(`runs_fleet`) is appended with a `.` separator and applied as a global
prefix (`statsd.WithNamespace`). Tags supplied via `DatadogConfig.Tags`
become global tags on every metric. The high-frequency
`PublishCacheHit`/`PublishCacheMiss` paths pass `p.sampleRate` (default
1.0) to `client.Incr`; everything else hard-codes rate `1`.
`PublishJobDuration` uses `Distribution` (not `Histogram`) so percentiles
are computed globally across the Fargate fleet rather than per-host.
`PublishServiceCheck` and `PublishEvent` map status/alert-type integers
onto Datadog's enum values.

### Logging

`pkg/logging/logger.go` exports `Init()`, called from `cmd/server/main.go`
and `cmd/agent/main.go` at startup. `Init` constructs an
`slog.NewJSONHandler` over `os.Stdout`, attaches a `host` attribute
(prefers `$HOSTNAME`, falls back to `os.Hostname`, ultimately `"unknown"`),
sets it as `slog.Default`, and redirects stdlib `log` output through a
`slogWriter` that forwards everything as `WARN` with `log_type=stdlib`.

The `Logger` type wraps `*slog.Logger` and is constructed via `New(attrs
...any)` or the `WithComponent(logType, component)` helper. Both return a
logger backed by a `lazyHandler`, which delegates to
`slog.Default().Handler()` at log time — package-level loggers declared
before `Init()` runs still pick up the JSON handler once it's installed.

### Cost reporting

`pkg/cost/reporter.go` defines `Reporter` with three injected AWS clients
(`CloudWatchAPI`, `S3API`, `SNSAPI`) plus a `PriceFetcherAPI` for dynamic
price lookups. `GenerateDailyReport` is the entry point. Flow:

1. Compute a 24-hour window aligned to the current hour.
2. `getCostMetrics` issues a single `GetMetricData` call against the
   `RunsFleet` namespace for five metrics: `FleetSizeIncrement`,
   `SpotInterruptions`, `JobSuccess`, `JobFailure`, `JobDuration` (sum
   over 1-hour periods).
3. Split fleet activations 80/20 spot/on-demand and compute hours.
4. Look up the average instance price (`t4g.medium`) via
   `PriceFetcher.GetPrice`; fall back to `0.0336` USD if the Pricing API
   call fails.
5. Compute `EC2SpotCost`, `EC2OnDemandCost`, `SpotSavings` using the fixed
   `SpotDiscount = 0.7`.
6. Estimate Fargate ($1.20/day flat), SQS ($0.40/M reqs), DynamoDB ($1.25/M
   writes + $0.25/M reads), CloudWatch logs ($0.50/GB), and S3 ($0.05
   flat).
7. `generateMarkdownReport` formats a markdown report with a disclaimer
   block.
8. Upload to S3 at `cost/<YYYY>/<MM>/<DD>.md` (skipped if `reportsBucket`
   is empty); publish to SNS with subject `runs-fleet Daily Cost Report -
   <date>` (skipped if `snsTopicARN` is empty).

`pkg/cost/pricing.go` defines `PriceFetcher` — a 24-hour cache wrapping
`pricing.Client`. The Pricing API is region-locked to `us-east-1` and
`ap-south-1`, so the fetcher overrides the supplied `aws.Config` to
`us-east-1`. `GetPrice` checks the cache, queries the API, and on any
error logs a warning, sets `useFallback = true` (sticky for the lifetime
of the fetcher until `RefreshCache` resets it), and returns the
hard-coded price from the package-level `instancePricing` map.

## Talks To [coverage: high -- 8 sources]

| Component             | Direction | Backend / API                                |
| --------------------- | --------- | -------------------------------------------- |
| CloudWatchPublisher   | out       | `cloudwatch.PutMetricData` (namespace `RunsFleet`) |
| PrometheusPublisher   | in        | HTTP `/metrics` scrape (handler at `Handler()`)    |
| DatadogPublisher      | out       | DogStatsD UDP (default `127.0.0.1:8125`)           |
| logging.Init          | out       | `os.Stdout` (JSON lines)                           |
| Reporter (cost)       | out       | `cloudwatch.GetMetricData` (read RunsFleet metrics) |
| Reporter (cost)       | out       | `s3.PutObject` (cost reports bucket)               |
| Reporter (cost)       | out       | `sns.Publish` (daily-cost SNS topic)               |
| PriceFetcher          | out       | `pricing.GetProducts` (us-east-1 endpoint)         |

## API Surface [coverage: high -- 8 sources]

### Publisher interface (`pkg/metrics/publisher.go`)

Counters (no dimensions):
- `PublishFleetSizeIncrement(ctx)` / `PublishFleetSizeDecrement(ctx)`
- `PublishJobSuccess(ctx)` / `PublishJobFailure(ctx)` / `PublishJobQueued(ctx)`
- `PublishSpotInterruption(ctx)`
- `PublishMessageDeletionFailure(ctx)`
- `PublishCacheHit(ctx)` / `PublishCacheMiss(ctx)` (sampled on Datadog)
- `PublishJobClaimFailure(ctx)`
- `PublishWarmPoolHit(ctx)`

Counters with `count int` argument (used for cleanup metrics):
- `PublishOrphanedInstancesTerminated(ctx, count)`
- `PublishSSMParametersDeleted(ctx, count)`
- `PublishJobRecordsArchived(ctx, count)`
- `PublishOrphanedJobsCleanedUp(ctx, count)`
- `PublishStaleJobsReconciled(ctx, count)`

Counters with dimensions:
- `PublishSchedulingFailure(ctx, taskType)`
- `PublishCircuitBreakerTriggered(ctx, instanceType)`

Gauges:
- `PublishQueueDepth(ctx, depth float64)`
- `PublishFleetSize(ctx, size int)`
- `PublishPoolUtilization(ctx, poolName, utilization float64)`
- `PublishPoolRunningJobs(ctx, poolName, count int)`

Distribution / histogram:
- `PublishJobDuration(ctx, durationSeconds int)`

Datadog-specific (no-op on CloudWatch and Prometheus):
- `PublishServiceCheck(ctx, name, status int, message)` — status: `0=OK,
  1=Warning, 2=Critical, 3=Unknown` (constants `ServiceCheckOK`,
  `ServiceCheckWarning`, `ServiceCheckCritical`, `ServiceCheckUnknown`).
- `PublishEvent(ctx, title, text, alertType, tags)` — alertType:
  `"info" | "warning" | "error" | "success"`.

Lifecycle:
- `Close() error` — closes underlying client connections.

### Constructors

- `NewCloudWatchPublisher(cfg aws.Config) *CloudWatchPublisher`
- `NewCloudWatchPublisherWithNamespace(cfg aws.Config, namespace string)`
- `NewPrometheusPublisher(cfg PrometheusConfig) *PrometheusPublisher`
  - `(*PrometheusPublisher).Handler() http.Handler`
  - `(*PrometheusPublisher).Registry() *prometheus.Registry`
- `NewDatadogPublisher(cfg DatadogConfig) (*DatadogPublisher, error)`
- `NewMultiPublisher(publishers ...Publisher) *MultiPublisher`
  - `(*MultiPublisher).Add(p Publisher)`
  - `(*MultiPublisher).Publishers() []Publisher`
- `NoopPublisher{}` (zero-value)

### Logging helpers (`pkg/logging/logger.go`)

- `Init()` — sets up JSON slog on stdout, redirects stdlib `log`.
- `New(attrs ...any) *Logger` — lazy-handler logger.
- `WithComponent(logType, component string) *Logger` — convention for
  package-level loggers.
- `(*Logger).With(attrs ...any) *Logger`

Attribute key constants: `KeyAction`, `KeyAudit`, `KeyBackend`,
`KeyComponent`, `KeyCount`, `KeyDuration` (`"duration_ms"`), `KeyError`,
`KeyHost`, `KeyInstanceID`, `KeyInstanceType`, `KeyJobID`, `KeyJobName`,
`KeyLogType`, `KeyNamespace`, `KeyOwner`, `KeyPoolName`, `KeyQueueURL`,
`KeyReason`, `KeyRemoteAddr`, `KeyResult`, `KeyRepo`, `KeyRunID`, `KeyTask`,
`KeyUser`, `KeyWorkflowName`.

Log-type constants: `LogTypeServer`, `LogTypeWebhook`, `LogTypeQueue`,
`LogTypePool`, `LogTypeHousekeep`, `LogTypeTermination`, `LogTypeEvents`,
`LogTypeCache`, `LogTypeAdmin`, `LogTypeFleet`, `LogTypeCircuit`,
`LogTypeRunner`, `LogTypeCost`, `LogTypeK8s`, `LogTypeMetrics`, `LogTypeDB`,
`LogTypeAgent`.

### Cost reporting

- `NewReporter(cfg aws.Config, appConfig *config.Config, snsTopicARN,
  reportsBucket string) *Reporter`
- `NewReporterWithClients(cwClient, s3Client, snsClient, priceFetcher,
  appConfig, snsTopicARN, reportsBucket) *Reporter` — for tests.
- `(*Reporter).GenerateDailyReport(ctx) error`
- `NewPriceFetcher(cfg aws.Config, region string) *PriceFetcher`
- `(*PriceFetcher).GetPrice(ctx, instanceType) (float64, error)`
- `(*PriceFetcher).GetPricing(ctx, instanceTypes []string) map[string]float64`
- `(*PriceFetcher).RefreshCache(ctx) error`

`SpotDiscount = 0.7` (package-level constant).

## Data [coverage: high -- 8 sources]

### Metric names emitted (CloudWatch / Datadog / Prometheus)

| CloudWatch                  | Datadog / Prometheus               | Type         | Dimension      |
| --------------------------- | ---------------------------------- | ------------ | -------------- |
| QueueDepth                  | queue_depth                        | gauge        | —              |
| FleetSizeIncrement          | fleet_size_increment[_total]       | counter      | —              |
| FleetSizeDecrement          | fleet_size_decrement[_total]       | counter      | —              |
| FleetSize                   | fleet_size                         | gauge        | —              |
| JobDuration                 | job_duration_seconds               | dist/histo   | —              |
| JobSuccess                  | job_success[_total]                | counter      | —              |
| JobFailure                  | job_failure[_total]                | counter      | —              |
| JobQueued                   | job_queued[_total]                 | counter      | —              |
| SpotInterruptions           | spot_interruptions[_total]         | counter      | —              |
| MessageDeletionFailures     | message_deletion_failures[_total]  | counter      | —              |
| CacheHits                   | cache_hits[_total]                 | counter      | — (sampled DD) |
| CacheMisses                 | cache_misses[_total]               | counter      | — (sampled DD) |
| OrphanedInstancesTerminated | orphaned_instances_terminated[_total] | counter   | —              |
| SSMParametersDeleted        | ssm_parameters_deleted[_total]     | counter      | —              |
| JobRecordsArchived          | job_records_archived[_total]       | counter      | —              |
| OrphanedJobsCleanedUp       | orphaned_jobs_cleaned_up[_total]   | counter      | —              |
| StaleJobsReconciled         | stale_jobs_reconciled[_total]      | counter      | —              |
| PoolUtilization             | pool_utilization_percent           | gauge        | PoolName       |
| PoolRunningJobs             | pool_running_jobs                  | gauge        | PoolName       |
| SchedulingFailure           | scheduling_failure[_total]         | counter      | TaskType       |
| CircuitBreakerTriggered     | circuit_breaker_triggered[_total]  | counter      | InstanceType   |
| JobClaimFailures            | job_claim_failures[_total]         | counter      | —              |
| WarmPoolHits                | warm_pool_hits[_total]             | counter      | —              |

Datadog adds two surfaces missing from the others: `ServiceCheck`
(name-prefixed with namespace, status `Ok|Warn|Critical|Unknown`) and
`Event` (`AlertType` mapped from string).

### Log fields

JSON lines emitted to stdout look like:

```json
{
  "time":"2026-04-30T...",
  "level":"INFO",
  "msg":"...",
  "host":"<hostname>",
  "log_type":"<one-of-LogType*>",
  "component":"<package-defined>",
  ...
}
```

Common fields used across the codebase: `action`, `audit`, `backend`,
`component`, `count`, `duration_ms`, `error`, `host`, `instance_id`,
`instance_type`, `job_id`, `job_name`, `log_type`, `namespace`, `owner`,
`pool_name`, `queue_url`, `reason`, `remote_addr`, `result`, `repo`,
`run_id`, `task`, `user`, `workflow_name`. Stdlib `log.Print*` calls are
forwarded as `level=WARN` with `log_type=stdlib`.

### Cost report shape

`Breakdown` struct (`pkg/cost/reporter.go`):

```go
type Breakdown struct {
    Date              string  // YYYY-MM-DD
    TotalCost         float64
    EC2SpotCost       float64
    EC2OnDemandCost   float64
    EC2SpotHours      float64
    EC2OnDemandHours  float64
    SpotSavings       float64
    S3Cost            float64
    FargateCost       float64
    SQSCost           float64
    DynamoDBCost      float64
    CloudWatchCost    float64
    JobsCompleted     int
    SpotInterruptions int
    CacheHitRate      float64
}
```

The published artifact is markdown, not JSON — sections: header (date,
24-hour total), EC2 Compute (spot/on-demand cost & hours, savings),
Supporting Services (Fargate, SQS, DynamoDB, CloudWatch, S3), Job
Statistics (completed count, interruptions, cost-per-job), and a
disclaimer footer. S3 path: `cost/<YYYY>/<MM>/<DD>.md`.

### Hard-coded pricing table (`pkg/cost/reporter.go`)

`instancePricing` covers three ARM families, on-demand `us-east-1` prices,
2024 vintage:

- `t4g.{micro,small,medium,large,xlarge,2xlarge}` — `0.0084` to `0.2688`
- `c7g.{medium,large,xlarge,2xlarge}` — `0.0361` to `0.2900`
- `m7g.{medium,large,xlarge,2xlarge}` — `0.0408` to `0.3264`

Unknown instance types fall back to `t4g.medium` (`0.0336`).

## Key Decisions [coverage: high -- 8 sources]

- **Multi-backend by interface, fan-out by composition.** `Publisher` is a
  single interface and `MultiPublisher` is just another implementation —
  callers don't know how many backends are live. Operators compose any
  subset (CloudWatch + Datadog, Prometheus only, all three, none) without
  code changes.
- **5-second per-publisher timeout in fan-out.** A slow CloudWatch
  endpoint can't stall the Prometheus or Datadog write — `MultiPublisher`
  spawns a goroutine per child, races it against `time.After(5s)`, and
  collects errors via `errors.Join`.
- **Datadog `Distribution` over `Histogram` for `JobDuration`.** Datadog's
  `Distribution` aggregates percentiles globally across all hosts;
  `Histogram` is per-host. The codebase explicitly comments this choice.
- **Sample rate only on cache hit/miss.** Every other Datadog counter uses
  rate `1`. Cache events are the only metric that fires per-request and
  warranted explicit sampling.
- **Lazy-handler slog wrapper.** Package-level loggers
  (`var costLog = logging.WithComponent(...)`) are created at package init
  before `main` calls `Init()`. The `lazyHandler` resolves
  `slog.Default()` at log time, so those loggers transparently start
  emitting JSON once `Init` runs.
- **Stdlib `log` redirected to slog as WARN.** Any `log.Print` from
  vendored deps becomes JSON with `log_type=stdlib`, level WARN — no
  silent stderr drips.
- **Cost reports are estimates, not billing.** The pricing table sits
  behind a long comment explaining: prices go stale, regional variations
  not modeled, EBS/data-transfer/spot-actual not included, AWS Cost
  Explorer is the source of truth. The published markdown ends with a
  disclaimer block.
- **Pricing API region override.** `pricing.Client` only works in
  `us-east-1` and `ap-south-1`; `NewPriceFetcher` copies the supplied
  `aws.Config` and force-sets `Region = "us-east-1"` so callers don't
  need to think about it.
- **Sticky fallback flag.** Once a Pricing API call fails, `PriceFetcher`
  flips `useFallback = true` and stops calling the API for the lifetime
  of the fetcher (resets only on `RefreshCache`). This avoids hammering
  a broken API on every report.
- **80/20 spot/on-demand assumption.** `getCostMetrics` doesn't query
  actual spot vs on-demand counts; it splits `FleetSizeIncrement` 80%
  spot, 20% on-demand based on observed product behavior.

## Gotchas [coverage: high -- 8 sources]

- **CloudWatch put-rate.** Most metric methods issue one
  `PutMetricData` per call. High-frequency calls (e.g., per-cache-event)
  will hit CloudWatch's request limits — there's no batching layer.
  Prefer Prometheus or Datadog (sampled) for hot paths.
- **`PublishServiceCheck`/`PublishEvent` no-op outside Datadog.** Don't
  rely on these reaching CloudWatch or Prometheus — they silently return
  `nil`. Use them in addition to, not instead of, regular metrics.
- **Datadog sample-rate only on cache hit/miss.** A 0.1 `SampleRate` in
  `DatadogConfig` divides cache counters by 10 in the agent
  (`statsd.Incr(... rate)` scales counts upstream), but every other metric
  ignores it. If you set `SampleRate = 0.1` expecting a 10× volume
  reduction across the board, you'll be surprised.
- **`MultiPublisher` 5s timeout.** A backend that consistently exceeds
  5 seconds will log a warning every publish and still consume a
  goroutine for the full duration of the underlying call (the timeout
  only abandons the result, not the work).
- **Pricing API hard-fails to fallback.** A single failed Pricing API call
  flips `useFallback` permanently for that fetcher instance — subsequent
  calls return hard-coded prices without retrying. To recover, call
  `RefreshCache` (which resets the flag) or construct a new `PriceFetcher`.
- **Hard-coded prices are us-east-1, 2024.** `instancePricing` does not
  reflect ap-northeast-1 (this project's default region) pricing, nor any
  AWS pricing changes since 2024. The fallback path therefore biases
  reports systematically — usually low for ap-northeast-1 (Tokyo
  surcharge) and outdated otherwise.
- **Only three instance families priced.** `t4g`, `c7g`, `m7g`. Any other
  family (e.g., the `c8g`/`m8g` Graviton4 generations the runner labels
  support) falls back to `t4g.medium = 0.0336`, which is wrong for any
  larger or non-burstable instance.
- **70% spot discount is a constant.** `SpotDiscount = 0.7` is applied
  uniformly, regardless of actual spot price at report time. Reports do
  not reflect spot-price spikes or interruption-driven on-demand
  fallback.
- **Data transfer, EBS, ENIs, NAT excluded.** The cost breakdown only
  models EC2 compute + Fargate + SQS + DynamoDB + CloudWatch + S3, with
  hand-tuned coefficients (e.g., `1.20` flat for Fargate). Reality
  includes EBS, data egress, NAT gateway hours, ECR storage, etc.
- **80/20 spot/on-demand split is an assumption, not measured.** Even if
  every fleet activation succeeded as spot, the report would still
  attribute 20% of activations to on-demand pricing.
- **`avgJobHours` defaults to 0.5 with no signal.** When neither
  `JobSuccess` nor `JobFailure` reported any data points, the breakdown
  silently uses 30 minutes per job. Empty-day reports are not zero — they
  still attribute Fargate + SQS + DynamoDB + S3 base costs.
- **Logging `Init()` race.** `Init` overwrites `slog.Default` and stdlib
  `log.SetOutput`. Calling it concurrently with logging calls is unsafe.
  Treat it as a once-at-startup operation.
- **`HOSTNAME` env var preferred over `os.Hostname`.** In containers
  where Kubernetes injects `HOSTNAME` (the pod name), the log `host`
  field will be the pod name; in plain ECS Fargate it falls back to the
  container's hostname. Account for this when grouping by `host`.
- **CloudWatch gauges via `StatisticValues`.** `putGaugeMetric` sets
  `SampleCount=1, Sum=Min=Max=value`. CloudWatch alarms on `Average`,
  `Sum`, `Min`, `Max` all return the same number. This is intentional but
  uncommon — operators expecting a typical CloudWatch counter shape may
  be confused.

## Sources [coverage: high]

- [/Users/shavakan/workspace/runs-fleet/pkg/metrics/publisher.go](../../pkg/metrics/publisher.go)
- [/Users/shavakan/workspace/runs-fleet/pkg/metrics/cloudwatch.go](../../pkg/metrics/cloudwatch.go)
- [/Users/shavakan/workspace/runs-fleet/pkg/metrics/datadog.go](../../pkg/metrics/datadog.go)
- [/Users/shavakan/workspace/runs-fleet/pkg/metrics/prometheus.go](../../pkg/metrics/prometheus.go)
- [/Users/shavakan/workspace/runs-fleet/pkg/metrics/multi.go](../../pkg/metrics/multi.go)
- [/Users/shavakan/workspace/runs-fleet/pkg/logging/logger.go](../../pkg/logging/logger.go)
- [/Users/shavakan/workspace/runs-fleet/pkg/cost/reporter.go](../../pkg/cost/reporter.go)
- [/Users/shavakan/workspace/runs-fleet/pkg/cost/pricing.go](../../pkg/cost/pricing.go)
