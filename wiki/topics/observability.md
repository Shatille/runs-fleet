---
topic: Observability (Metrics, Tracing, Logging, Cost)
last_compiled: 2026-07-21
sources_count: 20
---

# Observability (Metrics, Tracing, Logging, Cost)

## Purpose [coverage: high -- 20 sources]

runs-fleet emits multi-backend metrics, OpenTelemetry traces, structured JSON
logs, and daily cost reports so operators can monitor fleet health, debug
incidents across distributed Fargate instances, and track AWS spend per
workflow job.

The observability surface is split across four packages:

- `pkg/metrics` — A `Publisher` interface (~45 methods, organized into labeled
  families: job lifecycle, fleet/provisioning, pools, internals,
  cache/housekeeping, cost) with three concrete backends (CloudWatch,
  Prometheus, Datadog DogStatsD) plus a fan-out `MultiPublisher` and a
  `NoopPublisher`. Backends are individually toggleable via env vars.
  `docs/METRICS.md` is the canonical per-metric reference (names, dimensions,
  meaning); this article covers the machinery and its sharp edges.
- `pkg/tracing` — OpenTelemetry SDK setup (OTLP gRPC exporter, batcher),
  a package-level `Tracer()`, and W3C TraceContext propagation helpers that
  carry `traceparent` strings through SQS message attributes. Returns a noop
  provider when disabled (zero overhead).
- `pkg/logging` — A thin wrapper around stdlib `log/slog` that pins the global
  handler to JSON-on-stdout, attaches a `host` attribute, exposes curated
  attribute keys and `log_type` categories, and adds context-stashed attrs
  (`ContextWith` / `ContextWithJob`) so task identity (job_id, run_id, repo)
  rides the `context.Context` into every log line.
- `pkg/cost` — A daily report generator that prices the window's completed
  DynamoDB job records with a shared per-job pricer (`JobPricer`, also used
  by the admin cost page), adds CloudWatch-sourced informational figures
  (spot interruptions, runner-minute comparison), writes a markdown report to
  S3, and publishes the same body to an SNS topic. Explicit about its
  limitations: live Pricing-API/spot prices with hard-coded fallbacks (three
  ARM families, fixed 70% spot discount), no data-transfer or EBS line items.

## Architecture [coverage: high -- 20 sources]

### Metrics fan-out

`Publisher` (defined in [pkg/metrics/publisher.go](../../pkg/metrics/publisher.go))
covers counters (`PublishJobEnqueued`, `PublishJobCompleted`), gauges
(`PublishQueueDepth`, `PublishInstances`, `PublishPoolInstances`),
latency distributions (`PublishJobWaitSeconds`, `PublishJobStartupSeconds`,
`PublishAgentBootstrapSeconds`, `PublishInstanceProvisionSeconds`, ...), and
Datadog-specific surfaces (`PublishServiceCheck`, `PublishEvent`). The
interface doc declares the cardinality policy: the high-cardinality `repo`
label is restricted to the three job-lifecycle counters (`JobsEnqueued`,
`JobsAssigned`, `JobsCompleted`); every other metric uses small closed enums.

`MultiPublisher` ([pkg/metrics/multi.go](../../pkg/metrics/multi.go)) wraps an
arbitrary list of publishers. Each interface call invokes `publishAll`, which
spawns a goroutine per child, runs each publisher's method on a nested
goroutine, collects errors with a 5-second per-publisher timeout
(`publishTimeout`), logs warnings via the metrics logger, and returns the
joined error set (`errors.Join`). One slow backend cannot block the others.

```
caller → MultiPublisher.PublishX
            ├── goroutine → CloudWatchPublisher.PublishX  (5s timeout)
            ├── goroutine → PrometheusPublisher.PublishX  (5s timeout)
            └── goroutine → DatadogPublisher.PublishX     (5s timeout)
```

`NoopPublisher` (same file as the interface) is a zero-value-friendly stub
returned when metrics are disabled — every method returns `nil`.

Adding a Publisher method means touching **six** points: the interface +
`NoopPublisher` (publisher.go), CloudWatch, Prometheus (field + registration +
method), Datadog, and the `MultiPublisher` fan-out.

### Backend implementations

`CloudWatchPublisher` ([pkg/metrics/cloudwatch.go](../../pkg/metrics/cloudwatch.go))
uses `cloudwatch.Client` from AWS SDK v2. The namespace is the fixed constant
`cloudWatchNamespace = "RunsFleet"` — intentionally **not** configurable
(prevents metric collisions across deployments in one account/region).
Counters go through `putCounter`/`putCounterValue` (single `MetricDatum` with
`Value`); gauges and latency observations go through `putGauge`/`putStatistic`
(a single-sample `StatisticSet` with `SampleCount=1, Sum=Min=Max=value`).
The `dims()` helper drops empty dimension values so an absent optional label
(e.g. no pool) does not mint a distinct series. Three high-frequency latency
metrics are **deliberate no-ops** on this backend
(`PublishMessageProcessingSeconds`, `PublishLockWaitSeconds`,
`PublishAWSCallDuration`, ~lines 186–224): un-batched `PutMetricData` would
issue a synchronous API request per message/SDK call — the AWS-call histogram
alone would roughly double the orchestrator's AWS API volume. Those
histograms live on Prometheus/Datadog only; CloudWatch keeps the
low-frequency `AWSCallFailures` counter.

`PrometheusPublisher` ([pkg/metrics/prometheus.go](../../pkg/metrics/prometheus.go))
constructs a private `prometheus.Registry` and pre-allocates one collector per
metric (`CounterVec`/`GaugeVec`/`HistogramVec`). Name prefix is the fixed
constant `runs_fleet`. Two shared bucket sets:
`latencyBucketsLong = [0.5, 1, 2, 5, 10, 20, 30, 60, 90, 120]` for
job/provision/fleet/bootstrap latencies and
`latencyBucketsShort = [0.005 … 10, 30]` for lock-wait and message-processing;
`aws_call_duration_seconds` has its own `[0.05 … 30]` set. `Handler()` exposes
the registry for mounting at `/metrics`.

`DatadogPublisher` ([pkg/metrics/datadog.go](../../pkg/metrics/datadog.go))
uses the official `github.com/DataDog/datadog-go/v5/statsd` client with the
fixed `runs_fleet.` prefix (`statsd.WithNamespace`) and global tags from
`DatadogConfig.Tags`. All latency metrics use `Distribution` (not
`Histogram`) so percentiles are computed globally across the Fargate fleet
rather than per-host. `p.sampleRate` (default 1.0) applies only to the
high-frequency paths — `message_processing_seconds`, `lock_wait_seconds`,
`cache_requests`, `cache_operations`; everything else hard-codes rate `1`.
The `ddTag` helper drops empty tag values, mirroring CloudWatch's `dims()`.
`PublishServiceCheck`/`PublishEvent` map status/alert-type onto Datadog enums;
they are no-ops on the other two backends.

### Acquisition-latency pipeline (PR #387, 2026-07-09)

Three distributions now decompose "how long until a runner picks up the job":

- **`JobStartupSeconds` (Pool, Source)** — the headline end-to-end number on
  the **GitHub clock**: `workflow_job` created → started. Published from a new
  `in_progress` webhook case:
  `HandleWorkflowJobInProgress` ([internal/handler/webhook.go](../../internal/handler/webhook.go))
  derives a `StartupObservation` only when the job ran on one of our runners
  (runner-name `runs-fleet-` prefix) AND carries parseable runs-fleet labels
  (`in_progress` fires for every runner in the repo, including foreign ones).
  `PublishJobStartupMetrics` resolves Source (`warm_pool`/`cold_start`) via
  one `GetJobByJobID` read; any miss publishes with an **empty source** rather
  than dropping the observation. Runs post-ack in a panic-recovered background
  goroutine (`cmd/server/main.go` `"in_progress"` case).
- **`InstanceProvisionSeconds` (Source, Family)** — assignment →
  runner-registered, emitted at `confirmRunnerStarted` in
  [pkg/termination/handler.go](../../pkg/termination/handler.go): agent
  `StartedAt` minus the job record's `created_at`, surfaced by
  `MarkJobStarted`'s `ReturnValueAllNew` echo (`events.JobInfo` gained
  `WarmPoolHit` + `CreatedAt`). Semantics: `warm_pool` = assignment →
  registered (resume + bootstrap); `cold_start` = post-CreateFleet →
  registered (CreateFleet itself is `FleetCreateSeconds`). Previously
  defined-but-never-called.
- **`AgentBootstrapSeconds` (Pool, Phase)** — agent-measured bootstrap
  segments riding the "started" telemetry as five additive `omitempty`
  fields; the termination handler publishes one observation per positive
  segment. Phase is a closed enum fixed orchestrator-side: `boot`
  (`/proc/uptime` at agent entry), `config`, `runner_download`,
  `registration`, `total`.

### Tracing

`pkg/tracing` is four small files. `ParseConfig()`
([pkg/tracing/config.go](../../pkg/tracing/config.go)) reads
`RUNS_FLEET_TRACING_ENABLED` (default false), `RUNS_FLEET_OTEL_ENDPOINT`,
`RUNS_FLEET_OTEL_INSECURE` (default true), `RUNS_FLEET_OTEL_SERVICE_NAME`
(default `runs-fleet`), and `RUNS_FLEET_ENV` (deployment environment
resource attribute). `Setup()`
([pkg/tracing/provider.go](../../pkg/tracing/provider.go)) returns a **noop
`TracerProvider`** when disabled or endpoint-less; otherwise it builds an OTLP
gRPC exporter with a batching span processor and a resource carrying service
name/version (version from `debug.ReadBuildInfo`, `"dev"` fallback).
`Tracer()` ([pkg/tracing/tracer.go](../../pkg/tracing/tracer.go)) is the
package-level accessor (`otel.Tracer("runs-fleet")`) — instrumentation sites
never hold a provider. `InjectTraceContext`/`ExtractTraceContext`
([pkg/tracing/propagation.go](../../pkg/tracing/propagation.go)) round-trip a
W3C `traceparent` string via a map carrier, which `pkg/queue/sqs.go` stores in
SQS message attributes so traces span webhook → queue → worker → termination.
Instrumented sites: `cmd/server/main.go`, `internal/handler/webhook.go`,
`internal/worker/ec2.go`, `pkg/fleet/fleet.go`, `pkg/queue/sqs.go`,
`pkg/events/handler.go` (`events.spot_interruption`), and
`pkg/termination/handler.go` (`termination.process`,
`termination.bootstrap_failed`).

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

[pkg/logging/context.go](../../pkg/logging/context.go) adds context-stashed
attrs: `ContextWith(ctx, attrs...)` accumulates `slog.Attr`s on the context;
`ContextWithJob(ctx, jobID, runID, repo)` stashes the standard task-identity
triple (zero-valued fields omitted). A `contextHandler` wrapper injects the
stashed attrs into every record — but only via `*logging.Logger` methods,
which all require a context. Stash a key at most once per context branch:
slog does not de-duplicate, so re-stashing emits it twice.

### Cost reporting

`pkg/cost/reporter.go` defines `Reporter` with injected dependencies: a
`JobLister` (satisfied by `*db.Client`), three AWS clients (`CloudWatchAPI`,
`S3API`, `SNSAPI`), a `PriceFetcherAPI` for live on-demand prices, and a
`SpotPricer` (satisfied by `*fleet.Manager`) for live spot prices.
`GenerateDailyReport` is the entry point (dispatched from
[pkg/housekeeping/tasks.go](../../pkg/housekeeping/tasks.go)). Flow (reworked
in PR #390, 2026-07 — the EC2 section previously aggregated CloudWatch
metrics; see Gotchas for the history):

1. Compute a rolling 24-hour **UTC** window aligned to the current hour
   (`time.Now().UTC().Truncate(time.Hour)` minus 24h); the report is dated
   by the window's start day.
2. `accumulateEC2Costs` lists completed job records from DynamoDB
   (`ListJobsForAdmin`: status `completed`, `created_at` in `[start, end)`
   via the new `AdminJobFilter.Until` upper bound, capped at
   `jobWindowLimit = 10000` — the same cap the admin cost endpoints accept).
   The lister also returns the pre-cap match count; when it exceeds the
   priced set the reporter logs a "job window truncated, EC2 costs
   undercount" warning and the report carries an in-body truncation note.
   A nil lister degrades to zeros with a warning (test-path safety); a
   lister **error fails the whole run**.
3. Each job is priced by a fresh `JobPricer`
   ([pkg/cost/jobpricing.go](../../pkg/cost/jobpricing.go)) from the
   record's exact instance type, spot flag, and duration; spot/on-demand
   cost, hours, and savings aggregate into the breakdown
   (`JobsCompleted` = priced jobs, `JobsMatched` = pre-cap count).
4. `spotInterruptionCount` queries CloudWatch with metric-math
   `SUM(SEARCH('{RunsFleet,Family} MetricName="SpotInterruptions"', 'Sum', 3600))` —
   the Family-dimensioned form mirroring `runnerMinuteCost`'s SEARCH
   pattern, since a plain undimensioned lookup matches nothing. A CloudWatch
   error degrades the count to 0 with a warning.
5. Supporting-service estimates scale off the completed-job count: Fargate
   ($1.20/day flat), SQS ($0.40/M reqs), DynamoDB ($1.25/M writes + $0.25/M
   reads), CloudWatch logs ($0.50/GB), and S3 ($0.05 flat).
6. `runnerMinuteCost` (in [pkg/cost/runnerminutes.go](../../pkg/cost/runnerminutes.go))
   issues a second `GetMetricData` call — one metric-math `SUM(SEARCH(...))`
   query per distinct `(arch, vCPU)` shape in `fleet.InstanceCatalog` — to
   total `RunnerExecutionSeconds` (emitted at job termination, dimensioned by
   `Arch`/`Vcpu`/`Spot`/`Result`). It converts seconds to billable
   vCPU-minutes and multiplies by the default per-vCPU-minute rates
   (`cost.DefaultRunnerMinuteRates()`: amd64 $0.002, arm64 $0.00125) to
   produce the standard hosted-runner-equivalent cost. A query failure is
   logged and skipped (the figure stays zero).
7. `generateMarkdownReport` formats a markdown report with a disclaimer
   block; the runner-minute cost section is included only when the figure is
   non-zero, the truncation note only when the window was capped.
8. Upload to S3 at `cost/<YYYY>/<MM>/<DD>.md` (skipped if `reportsBucket`
   is empty); publish to SNS with subject `runs-fleet Daily Cost Report -
   <date>` (skipped if `snsTopicARN` is empty). Both failures are warnings,
   not errors.

#### JobPricer — shared per-job pricing

`JobPricer` ([pkg/cost/jobpricing.go](../../pkg/cost/jobpricing.go)) computes
one job's EC2 cost as a `JobPricing{Total, Spot, OnDemand, Savings, Hours}`
split. It was extracted from the admin cost handler
([pkg/admin/handler_cost.go](../../pkg/admin/handler_cost.go)) in the PR #390
series (behavior-preserving), so the daily report and the admin cost page
price jobs identically:

- Instance type defaults to `t4g.medium` when the record lacks one; billable
  duration is `DurationSeconds/3600` with a 0.5-hour minimum.
- On-demand price: the hard-coded table (`GetInstancePrice`), overridden by
  a live `PriceFetcherAPI.GetPrice` result when available and positive.
- Spot jobs: live market price via `SpotPricer.SpotPrice` (`fleet.Manager`'s
  5-minute spot-price cache with negative caching of confirmed no-price
  types and a `fetchMu`-serialized fetch path); fallback is the fixed
  `on-demand × (1 − SpotDiscount)` estimate. Savings = on-demand − actual
  on the live path (clamped positive), `on-demand × SpotDiscount` on the
  fallback path.
- Both lookups are memoized per instance type for the pricer's lifetime, so
  one run prices each distinct type once. **Documented contract: not safe
  for concurrent use — create one per run/request.** Cross-request caching
  lives in the underlying `PriceFetcher` (24h cache) and fleet spot cache
  (5min), which *are* concurrency-safe.

`cmd/server/main.go` wires `*db.Client` and `*fleet.Manager` into
`NewReporter`, guarding the typed-nil case (`var spot cost.SpotPricer; if
fleetManager != nil { spot = fleetManager }`) so a nil manager never becomes
a non-nil interface.

`pkg/cost/pricing.go` defines `PriceFetcher` — a 24-hour cache wrapping
`pricing.Client`. The Pricing API is region-locked to `us-east-1` and
`ap-south-1`, so the fetcher overrides the supplied `aws.Config` to
`us-east-1`. `GetPrice` checks the cache, queries the API, and on any
error logs a warning, sets `useFallback = true` (sticky for the lifetime
of the fetcher until `RefreshCache` resets it), and returns the
hard-coded price from the package-level `instancePricing` map.

## Talks To [coverage: high -- 20 sources]

| Component             | Direction | Backend / API                                |
| --------------------- | --------- | -------------------------------------------- |
| CloudWatchPublisher   | out       | `cloudwatch.PutMetricData` (namespace `RunsFleet`) |
| PrometheusPublisher   | in        | HTTP `/metrics` scrape (handler at `Handler()`)    |
| DatadogPublisher      | out       | DogStatsD UDP (default `127.0.0.1:8125`)           |
| tracing.Setup         | out       | OTLP gRPC collector (`RUNS_FLEET_OTEL_ENDPOINT`)   |
| tracing propagation   | both      | SQS message attributes (`traceparent`)             |
| logging.Init          | out       | `os.Stdout` (JSON lines)                           |
| Reporter (cost)       | out       | DynamoDB scan via `db.Client.ListJobsForAdmin` (completed jobs) |
| Reporter (cost)       | out       | `cloudwatch.GetMetricData` (SpotInterruptions + RunnerExecutionSeconds metric-math) |
| Reporter (cost)       | out       | `s3.PutObject` (cost reports bucket)               |
| Reporter (cost)       | out       | `sns.Publish` (daily-cost SNS topic)               |
| JobPricer             | out       | Pricing API via `PriceFetcher`; `ec2.DescribeSpotPriceHistory` via `fleet.Manager` spot cache |
| PriceFetcher          | out       | `pricing.GetProducts` (us-east-1 endpoint)         |

Metric producers span the codebase; the notable cross-package flows are the
webhook `in_progress` path (→ `JobStartupSeconds`) and the termination
handler, which converts agent telemetry into `RunnerConfirmed`,
`InstanceProvisionSeconds`, `AgentBootstrapSeconds`, `JobsCompleted`,
`JobExecutionSeconds`, `RunnerExecutionSeconds`, tool-cache and
cache-interception counters — see
[events-and-termination](events-and-termination.md).

## API Surface [coverage: high -- 20 sources]

### Publisher interface (`pkg/metrics/publisher.go`)

Organized into labeled families (full per-metric reference:
[docs/METRICS.md](../../docs/METRICS.md)):

- **Job lifecycle**: `PublishJobEnqueued(pool, arch, capacity, repo)`,
  `PublishJobAssigned(pool, source, repo)`, `PublishRunnerConfirmed(pool)`,
  `PublishJobCompleted(pool, result, repo)`, `PublishJobRequeued(reason)`,
  `PublishJobDeduplicated(path)`, `PublishJobWaitSeconds(pool, source, s)`,
  `PublishJobStartupSeconds(pool, source, s)`,
  `PublishAgentBootstrapSeconds(pool, phase, s)`,
  `PublishJobExecutionSeconds(pool, result, s)`
- **Fleet / provisioning**: `PublishInstanceProvisionSeconds(source, family,
  s)`, `PublishFleetCreate(capacity, result)`,
  `PublishFleetCreateSeconds(capacity, s)`, `PublishInstances(state,
  capacity, pool, n)`, `PublishSpotInterruption(family)`,
  `PublishCircuitBreakerTrip(instanceType)`,
  `PublishCircuitBreakerOpen(instanceType, open)`
- **Pools**: `PublishPoolInstances(pool, state, n)`,
  `PublishPoolDesired(pool, kind, n)`, `PublishPoolAction(pool, action,
  reason)`, `PublishPoolReconcileSeconds(s)`
- **Internals**: `PublishMessageProcessingSeconds(queue, result, s)`,
  `PublishLockWaitSeconds(lock, s)`, `PublishWorkerInflight(queue, n)`,
  `PublishQueueDepth(queue, depth)`, `PublishQueueReceive(queue, result)`,
  `PublishAWSCallDuration(service, operation, s)`,
  `PublishAWSCallFailure(service, operation, result)`
- **Cache / housekeeping / misc**: `PublishCacheRequest(result)`,
  `PublishCacheOperation(op)`, `PublishCacheBytesStored(bytes)`,
  `PublishCacheError(op)`, `PublishCacheAuthRejected(reason)`,
  `PublishHousekeepingAction(action, count)`,
  `PublishSchedulingFailure(taskType)`,
  `PublishMessageDeletionFailure(queue)`, `PublishServiceCheck(name, status,
  message)`, `PublishEvent(title, text, alertType, tags)`
- **Cost**: `PublishInstanceHours(capacity, family, hours)`,
  `PublishEstimatedCost(usd)`, `PublishRunnerExecutionSeconds(arch, vcpu,
  spot, result, seconds)`, `PublishRunnerToolCacheMiss(tool, version, arch)`,
  `PublishRunnerCacheInterception(status)`
- **Lifecycle**: `Close() error`

The fulfillment SLA is documented on the interface itself:
`PublishJobAssigned` (success) vs `PublishSchedulingFailure` (failure);
`PublishJobCompleted`'s `result` is *our* runner's operational lifecycle
(`served`/`interrupted`/`error`/`timeout`), never the client workflow's
pass/fail; `PublishJobDeduplicated` is benign dual-path dedup, never an SLA
failure.

### Constructors

- `NewCloudWatchPublisher(cfg aws.Config) *CloudWatchPublisher` — namespace
  fixed, no override constructor anymore
- `NewPrometheusPublisher(cfg PrometheusConfig) *PrometheusPublisher`
  - `(*PrometheusPublisher).Handler() http.Handler`
  - `(*PrometheusPublisher).Registry() *prometheus.Registry`
- `NewDatadogPublisher(cfg DatadogConfig) (*DatadogPublisher, error)` —
  `DatadogConfig` adds client tuning knobs (`BufferPoolSize`,
  `BufferFlushInterval`, `WorkersCount`, `MaxMessagesPerPayload`)
- `NewMultiPublisher(publishers ...Publisher) *MultiPublisher`
  - `(*MultiPublisher).Add(p Publisher)` / `.Publishers() []Publisher`
- `NoopPublisher{}` (zero-value)

### Tracing (`pkg/tracing`)

- `ParseConfig() Config` — env-var driven
- `Setup(ctx, cfg) (trace.TracerProvider, error)` — noop when disabled
- `Shutdown(ctx, tp) error` — flushes the SDK provider; safe on noop
- `Tracer() trace.Tracer` — package-level accessor
- `InjectTraceContext(ctx) string` / `ExtractTraceContext(traceparent) context.Context`

### Logging helpers (`pkg/logging`)

- `Init()` — sets up JSON slog on stdout, redirects stdlib `log`.
- `New(attrs ...any) *Logger` / `WithComponent(logType, component) *Logger`
- `(*Logger).With(attrs ...any) *Logger`
- `ContextWith(ctx, attrs ...slog.Attr) context.Context` /
  `ContextWithJob(ctx, jobID, runID int64, repo string) context.Context`
- `NewContextHandler(inner slog.Handler) slog.Handler` — for custom handlers

Attribute key constants: `KeyAction`, `KeyAliasLabel`, `KeyAudit`,
`KeyBackend`, `KeyComponent`, `KeyCount`, `KeyDuration` (`"duration_ms"`),
`KeyError`, `KeyHost`, `KeyInstanceID`, `KeyInstanceType`, `KeyJobID`,
`KeyJobName`, `KeyLogType`, `KeyNamespace`, `KeyOperation`, `KeyOwner`,
`KeyPoolName`, `KeyService`, `KeyQueueURL`, `KeyReason`, `KeyRemoteAddr`,
`KeyResult`, `KeyRepo`, `KeyRunID`, `KeyTask`, `KeyUser`, `KeyWorkflowName`.

Log-type constants: `LogTypeServer`, `LogTypeWebhook`, `LogTypeQueue`,
`LogTypePool`, `LogTypeHousekeep`, `LogTypeTermination`, `LogTypeEvents`,
`LogTypeCache`, `LogTypeAdmin`, `LogTypeFleet`, `LogTypeCircuit`,
`LogTypeRunner`, `LogTypeCost`, `LogTypeK8s`, `LogTypeMetrics`, `LogTypeDB`,
`LogTypeAgent`, `LogTypeAWS`.

### Cost reporting

- `NewReporter(cfg aws.Config, jobs JobLister, spotPricer SpotPricer,
  appConfig *config.Config, snsTopicARN, reportsBucket string) *Reporter`
- `NewReporterWithClients(cwClient, s3Client, snsClient, jobs, priceFetcher,
  spotPricer, appConfig, snsTopicARN, reportsBucket) *Reporter` — for tests.
- `(*Reporter).GenerateDailyReport(ctx) error`
- `JobLister` — `ListJobsForAdmin(ctx, db.AdminJobFilter) ([]db.AdminJobEntry,
  int, error)`; satisfied by `*db.Client`; the `int` is the pre-cap match
  count that drives truncation surfacing
- `SpotPricer` — `SpotPrice(ctx, instanceType) (float64, bool)`; satisfied by
  `*fleet.Manager`; `false` means no live price, fall back to the
  discount estimate
- `NewJobPricer(onDemand PriceFetcherAPI, spot SpotPricer) *JobPricer` —
  both args nil-safe (nil ⇒ hard-coded table / fixed discount)
- `(*JobPricer).Price(ctx, db.AdminJobEntry) JobPricing` — one job's cost,
  split as `{Total, Spot, OnDemand, Savings, Hours}`; not concurrent-safe
- `GetInstancePrice(instanceType) float64` — hard-coded table lookup,
  `t4g.medium` price for unknown types
- `NewPriceFetcher(cfg aws.Config, region string) *PriceFetcher`
- `(*PriceFetcher).GetPrice(ctx, instanceType) (float64, error)`
- `(*PriceFetcher).GetPricing(ctx, instanceTypes []string) map[string]float64`
- `(*PriceFetcher).RefreshCache(ctx) error`
- `DefaultRunnerMinuteRates() map[string]float64` — fresh copy, safe to
  retain/mutate

`SpotDiscount = 0.7` (package-level constant); `jobWindowLimit = 10000`
(unexported).

## Data [coverage: high -- 20 sources]

### Metric taxonomy (summary)

The full table lives in [docs/METRICS.md](../../docs/METRICS.md). Naming:
CloudWatch uses `PascalCase` under namespace `RunsFleet`; Prometheus/Datadog
use `snake_case` under `runs_fleet_`/`runs_fleet.` (counters end `_total` on
Prometheus). Highlights and dimensions:

| Metric (CloudWatch)        | Type      | Dimensions       | Notes |
| -------------------------- | --------- | ---------------- | ----- |
| JobsEnqueued / JobsAssigned / JobsCompleted | counter | Pool, …, **Repo** | only metrics allowed the repo label |
| RunnerConfirmed            | counter   | Pool             | agent "started" signal; flatline vs JobsAssigned = registration failure |
| JobWaitSeconds             | histogram | Pool, Source     | enqueue → assignment (SQS slice only) |
| JobStartupSeconds          | histogram | Pool, Source     | GitHub-clock created → started; headline startup number (PR #387) |
| InstanceProvisionSeconds   | histogram | Source, Family   | assignment → runner-registered (PR #387 wiring) |
| AgentBootstrapSeconds      | histogram | Pool, Phase      | boot \| config \| runner_download \| registration \| total (PR #387) |
| JobExecutionSeconds        | histogram | Pool, Result     | |
| FleetCreate[Seconds]       | ctr/histo | Capacity[, Result] | |
| SpotInterruptions          | counter   | Family           | |
| CircuitBreakerTrip / Open  | ctr/gauge | InstanceType     | |
| PoolInstances / PoolDesired / PoolActions | gauge/ctr | PoolName, … | |
| MessageProcessingSeconds, LockWaitSeconds, AWSCallDuration | histogram | … | **Prometheus/Datadog only** (CloudWatch no-op) |
| SchedulingFailure          | counter   | TaskType         | failure side of the fulfillment SLA |
| Cache* / RunnerCacheInterception / RunnerToolCacheMiss | counter | … | agent-sourced ones ride termination telemetry |
| RunnerExecutionSeconds     | counter   | Arch, Vcpu, Spot, Result | billable runner seconds; feeds runner-minute cost |
| InstanceHours / EstimatedCost | ctr/gauge | …            | defined but unwired — since PR #390 the reporter prices job records directly and no longer carries the wiring TODO |

Datadog adds two surfaces missing from the others: `ServiceCheck` and
`Event`.

### Log fields

JSON lines emitted to stdout look like:

```json
{
  "time":"2026-07-09T...",
  "level":"INFO",
  "msg":"...",
  "host":"<hostname>",
  "log_type":"<one-of-LogType*>",
  "component":"<package-defined>",
  "job_id":123, "run_id":456, "repo":"org/repo",
  ...
}
```

Task-identity fields (`job_id`, `run_id`, `repo`) are usually injected via
context stashing (`ContextWithJob`), not per-call args. Stdlib `log.Print*`
calls are forwarded as `level=WARN` with `log_type=stdlib`. `alias_label` is
set on the webhook "job enqueued" line — non-empty with the matched custom
label when the job was resolved via a config-driven alias.

### Cost report shape

`Breakdown` struct (`pkg/cost/reporter.go`):

```go
type Breakdown struct {
    Date              string  // YYYY-MM-DD (window start, UTC)
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
    JobsCompleted     int     // jobs actually priced (post-cap)
    SpotInterruptions int
    JobsMatched       int     // pre-cap window matches; > JobsCompleted ⇒ truncated

    RunnerVcpuMinutes float64 // billable vCPU-minutes (Σ runner-minutes × vCPU)
    RunnerMinuteCost  float64 // standard hosted-runner-equivalent cost
}
```

PR #390 added `JobsMatched` and deleted `CacheHitRate` (never populated).

The published artifact is markdown, not JSON — sections: header (date,
24-hour total), EC2 Compute (spot/on-demand cost & hours, savings),
Supporting Services (Fargate, SQS, DynamoDB, CloudWatch, S3), Job
Statistics (completed count — annotated "N of M (window truncated…)" when
capped — interruptions, cost-per-job), and a disclaimer footer. S3 path:
`cost/<YYYY>/<MM>/<DD>.md`.

### Hard-coded pricing table (`pkg/cost/reporter.go`)

`instancePricing` is the fallback (and `JobPricer` base) behind the live
Pricing API — three ARM families, on-demand `us-east-1` prices, 2024
vintage:

- `t4g.{micro,small,medium,large,xlarge,2xlarge}` — `0.0084` to `0.2688`
- `c7g.{medium,large,xlarge,2xlarge}` — `0.0361` to `0.2900`
- `m7g.{medium,large,xlarge,2xlarge}` — `0.0408` to `0.3264`

Unknown instance types fall back to `t4g.medium` (`0.0336`).

## Key Decisions [coverage: high -- 20 sources]

- **Multi-backend by interface, fan-out by composition.** `Publisher` is a
  single interface and `MultiPublisher` is just another implementation —
  callers don't know how many backends are live. Operators compose any
  subset without code changes.
- **5-second per-publisher timeout in fan-out.** A slow CloudWatch
  endpoint can't stall the Prometheus or Datadog write — `MultiPublisher`
  spawns a goroutine per child, races it against `time.After(5s)`, and
  collects errors via `errors.Join`.
- **Fixed namespaces.** `RunsFleet` / `runs_fleet` are constants, no longer
  constructor-overridable: a stable namespace prevents metric collisions
  across deployments sharing an AWS account or Prometheus/Datadog instance.
- **Cardinality policy on the interface.** `repo` is restricted to the three
  job-lifecycle counters; all other labels are closed enums documented per
  method; both CloudWatch (`dims`) and Datadog (`ddTag`) drop empty label
  values so optional labels don't fan out. The bar for a new label is
  written into the interface doc: "Do not add repo to any histogram or any
  other metric."
- **High-frequency histograms no-oped on CloudWatch.** There is no batching
  layer, so per-message/per-SDK-call latency metrics would issue one
  synchronous `PutMetricData` each — `PublishAWSCallDuration` alone would
  roughly double AWS API volume. Prometheus/Datadog carry those; CloudWatch
  keeps only low-frequency counters and single-sample statistics.
- **Acquisition latency made visible (PR #387, 2026-07-09).** A ~35–40s
  runner-acquisition latency was operationally invisible: the admin UI's
  "Avg Startup" measures assignment → started (`created_at` is stamped at
  assignment), and `JobWaitSeconds` covers only enqueue → assignment.
  `JobStartupSeconds` (GitHub-clock created → started, from the
  `in_progress` webhook) is the headline number spanning strictly more than
  `JobWaitSeconds`; `InstanceProvisionSeconds` and `AgentBootstrapSeconds`
  decompose where the time goes (orchestrator-side rendezvous vs on-instance
  boot/config/download/registration).
- **Agent has no metrics client.** Everything the agent observes
  (bootstrap timings, tool-cache misses, cache-interceptor outcome, v2
  cache bytes) rides the termination telemetry to SQS; the orchestrator
  parses and publishes. The `Phase` label enum for `AgentBootstrapSeconds`
  is fixed orchestrator-side as a cardinality guard — the agent never
  supplies label strings.
- **Datadog `Distribution` over `Histogram` for latencies.** Datadog's
  `Distribution` aggregates percentiles globally across all hosts;
  `Histogram` is per-host.
- **Tracing is noop-by-default.** `Setup` returns a noop provider when
  disabled; instrumentation calls `tracing.Tracer()` unconditionally with
  zero overhead. Trace context crosses process boundaries as a W3C
  `traceparent` string in SQS message attributes, not custom headers.
- **Lazy-handler slog wrapper.** Package-level loggers
  (`var costLog = logging.WithComponent(...)`) are created at package init
  before `main` calls `Init()`. The `lazyHandler` resolves
  `slog.Default()` at log time, so those loggers transparently start
  emitting JSON once `Init` runs.
- **Context-stashed log identity.** `ContextWithJob` threads job identity
  through call graphs without plumbing logger instances; the
  `contextHandler` injects stashed attrs at Handle time.
- **Stdlib `log` redirected to slog as WARN.** Any `log.Print` from
  vendored deps becomes JSON with `log_type=stdlib` — no silent stderr
  drips.
- **EC2 costs computed from job records, not CloudWatch (PR #390, 2026-07).**
  The 2026-07 metrics rework retired the counters the reporter aggregated
  (`FleetSizeIncrement`, `JobSuccess`, …), silently zeroing the EC2 section.
  Rather than chase metric names, the reporter now prices the same DynamoDB
  job records the admin cost page prices: the record already joins instance
  type, spot flag, and duration from the orchestrator and agent (see
  [db-record-as-rendezvous](../concepts/db-record-as-rendezvous.md)), so the
  daily report and the admin dashboard agree by construction, and a metric
  rename can never zero the report again. Only informational figures — spot
  interruptions and the runner-minute comparison — still come from
  CloudWatch, both via the same `SUM(SEARCH(...))` metric-math pattern.
- **One shared `JobPricer` for report and dashboard.** Per-job pricing was
  extracted from `pkg/admin/handler_cost.go` into `pkg/cost/jobpricing.go`
  (behavior-preserving); both callers construct a fresh pricer per
  run/request (the memoization scope), while cross-request caching lives in
  the concurrency-safe `PriceFetcher` and fleet spot caches underneath.
- **Asymmetric failure semantics in the daily report.** A job-lister error
  fails the run — housekeeping retries, and a zeroed-core report is worse
  than none — while CloudWatch, S3, and SNS failures log warnings and
  degrade only their own figures/outputs.
- **Cost reports are estimates, not billing.** Per-job durations and
  instance types are now exact, but pricing is still estimate-grade: live
  Pricing-API/spot prices when available, hard-coded table + fixed 70% spot
  discount as fallback, hand-tuned supporting-service coefficients; the
  published markdown ends with a disclaimer block pointing at AWS Cost
  Explorer as the source of truth.
- **Pricing API region override + sticky fallback.** `pricing.Client` only
  works in `us-east-1`/`ap-south-1`, so `NewPriceFetcher` force-sets
  `us-east-1`; a single API failure flips `useFallback = true` for the
  fetcher's lifetime (reset only by `RefreshCache`) to avoid hammering a
  broken API on every report.

## Gotchas [coverage: high -- 20 sources]

- **Per-pool gauges poisoned by instance-claim rows (high-cardinality
  footgun).** Instance claims and housekeeping task locks share the *pools*
  DynamoDB table, distinguished only by a sentinel `pool_name` prefix
  (`__instance_claim:<instance-id>`, `__task_lock:<task>`). Any path that
  scans the table as if every row were a pool and then publishes
  pool-dimensioned metrics mints **one zero-valued metric series per
  ephemeral instance ID** — unbounded cardinality that grows with instance
  churn. This drove CloudWatch custom metrics to 3,000+ series (~$900/mo)
  during busy periods while collapsing back to baseline when idle, so it
  hides between bills. The guard is `db.IsReservedPoolKey`: `ListPools`,
  `GetPoolConfig`, and `ExecutePoolAudit` all skip reserved keys. When
  adding a pool-dimensioned metric, confirm the pool value comes from a
  *filtered* pool list, never a raw table scan — and prefer a billing alarm
  on CloudWatch estimated charges to catch the next blowup on day one.
- **[Fixed 2026-07, PR #390] Cost reporter briefly queried retired metric
  names.** Between the metrics rework and PR #390, `getCostMetrics` asked
  CloudWatch for `FleetSizeIncrement`, `JobSuccess`, `JobFailure`,
  `JobDuration`, and a dimensionless `SpotInterruptions` — none emitted
  anymore — so the daily report's EC2 section computed from zeros. The EC2
  section now prices DynamoDB job records directly, and `SpotInterruptions`
  is queried with its `Family` dimension via metric-math SEARCH. Historical
  caveat: reports generated in that window have meaningless EC2 figures.
- **The 10k job cap truncates busy windows.** `accumulateEC2Costs` prices at
  most `jobWindowLimit = 10000` completed jobs per report; beyond that the
  EC2 figures undercount. Truncation is surfaced twice — a "job window
  truncated" warn log and the in-report "Jobs completed: N of M" note — but
  not worked around; `JobsMatched − JobsCompleted` tells you how many jobs
  went unpriced.
- **Zero-duration jobs bill 0.5 hours.** `JobPricer` applies a 0.5-hour
  minimum when `DurationSeconds` is missing or non-positive, so records that
  never recorded a duration inflate the hour and cost totals. (The admin
  runner-minute matrix deliberately *skips* those jobs instead — the two
  figures diverge on such records.)
- **`JobPricer` is not concurrency-safe.** The memo maps are plain maps by
  documented contract: construct one per run/request, never share across
  goroutines. Concurrency safety lives a layer down, in `PriceFetcher` and
  the fleet spot cache.
- **The savings line always says "(70% discount)".** `generateMarkdownReport`
  prints the fixed `SpotDiscount` label even when savings were computed from
  live spot prices; the dollar figure is right, the percentage label is
  cosmetic.
- **Cross-clock guard on `InstanceProvisionSeconds`.** The metric joins an
  orchestrator-stamped timestamp (`created_at` at assignment) with an
  agent-stamped one (`StartedAt` from the EC2 instance). Clock skew is
  bounded by chrony but not zero, so `publishProvisionSeconds` publishes
  only when both timestamps are non-zero **and** the span is positive — a
  skewed pair is silently dropped, never emitted as a negative or bogus
  value. Expect the histogram to slightly undercount very fast warm-pool
  hits.
- **Two-halves deploy for agent-sourced metrics.** The orchestrator half
  (handler + `pkg/metrics`) rides the orchestrator image; the agent half
  (the `bootstrap_*` telemetry fields) rides the **AMI cascade**
  (`build-runner.yml` → `build-amis.yml`). Deploy order is immaterial —
  zero-as-absent gives old-agent compatibility both ways — but
  `AgentBootstrapSeconds` flatlines until *both* halves are live. Don't
  debug an empty dashboard panel before the new AMI has rolled.
- **`JobStartupSeconds` source can be empty.** The Source label is resolved
  by a post-ack `GetJobByJobID` lookup; a nil DB client, missing jobs table,
  read error, or absent record publishes with `source=""` (dropped as a
  dimension) rather than losing the observation. Don't assume
  `warm_pool + cold_start` sums to the total.
- **Admin "Avg Startup" ≠ `JobStartupSeconds`.** The admin panel measures
  assignment → started; the metric measures GitHub created → started. They
  legitimately disagree — the metric includes the webhook → enqueue →
  assignment head the panel excludes.
- **CloudWatch put-rate.** Metric methods issue one `PutMetricData` per
  call with no batching. The known-hot paths are already no-oped on
  CloudWatch; any *new* high-frequency metric must follow the same pattern
  or prefer Prometheus/Datadog.
- **`PublishServiceCheck`/`PublishEvent` no-op outside Datadog.** They
  silently return `nil` on CloudWatch and Prometheus. Use them in addition
  to, not instead of, regular metrics.
- **Datadog sample-rate applies only to four metrics.** A 0.1 `SampleRate`
  scales `message_processing_seconds`, `lock_wait_seconds`,
  `cache_requests`, and `cache_operations`; every other metric hard-codes
  rate 1. Setting `SampleRate` expecting an across-the-board volume
  reduction will surprise you.
- **`MultiPublisher` 5s timeout abandons the result, not the work.** A
  backend that consistently exceeds 5 seconds logs a warning every publish
  and still consumes a goroutine for the full duration of the underlying
  call.
- **Pricing API hard-fails to fallback.** A single failed Pricing API call
  flips `useFallback` permanently for that fetcher instance. To recover,
  call `RefreshCache` or construct a new `PriceFetcher`.
- **Hard-coded prices are us-east-1, 2024, three families only.** `t4g`,
  `c7g`, `m7g`; anything else (including the `c8g`/`m8g` Graviton4
  generations the labels support) falls back to `t4g.medium = 0.0336`.
  Does not reflect ap-northeast-1 (this project's default region) pricing.
  Live Pricing-API and spot lookups mask this when they succeed; the table
  is what you get when they don't.
- **Data transfer, EBS, ENIs, NAT excluded.** The cost breakdown models
  EC2 compute + Fargate + SQS + DynamoDB + CloudWatch + S3 with hand-tuned
  coefficients; reality includes EBS, egress, NAT gateway hours, ECR, etc.
- **Logging `Init()` race.** `Init` overwrites `slog.Default` and stdlib
  `log.SetOutput`. Treat it as a once-at-startup operation.
- **Context attr stashing does not de-duplicate.** Re-stashing a key already
  present on the context (e.g. calling `ContextWithJob` twice with the same
  job_id) emits the attribute twice in the JSON line — slog does not
  de-duplicate. `pkg/termination` deliberately passes `jobID=0` on its
  second stash for exactly this reason.
- **`HOSTNAME` env var preferred over `os.Hostname`.** In Kubernetes the
  log `host` field is the pod name; on plain Fargate it falls back to the
  container hostname. Account for this when grouping by `host`.
- **CloudWatch gauges/latencies via `StatisticValues`.** Single-sample
  statistic sets mean `Average`, `Sum`, `Min`, `Max` all return the same
  number per datapoint. Intentional but uncommon — percentile analysis of
  the `*Seconds` metrics is only meaningful on Prometheus/Datadog.

## Sources [coverage: high]

- [pkg/metrics/publisher.go](../../pkg/metrics/publisher.go)
- [pkg/metrics/cloudwatch.go](../../pkg/metrics/cloudwatch.go)
- [pkg/metrics/prometheus.go](../../pkg/metrics/prometheus.go)
- [pkg/metrics/datadog.go](../../pkg/metrics/datadog.go)
- [pkg/metrics/multi.go](../../pkg/metrics/multi.go)
- [pkg/tracing/config.go](../../pkg/tracing/config.go)
- [pkg/tracing/provider.go](../../pkg/tracing/provider.go)
- [pkg/tracing/propagation.go](../../pkg/tracing/propagation.go)
- [pkg/tracing/tracer.go](../../pkg/tracing/tracer.go)
- [pkg/logging/logger.go](../../pkg/logging/logger.go)
- [pkg/logging/context.go](../../pkg/logging/context.go)
- [pkg/cost/reporter.go](../../pkg/cost/reporter.go)
- [pkg/cost/jobpricing.go](../../pkg/cost/jobpricing.go)
- [pkg/cost/pricing.go](../../pkg/cost/pricing.go)
- [pkg/cost/runnerminutes.go](../../pkg/cost/runnerminutes.go)
- [docs/METRICS.md](../../docs/METRICS.md)
- [internal/handler/webhook.go](../../internal/handler/webhook.go)
- [pkg/termination/handler.go](../../pkg/termination/handler.go)
- [pkg/db/pool_config.go](../../pkg/db/pool_config.go)
- [pkg/housekeeping/tasks.go](../../pkg/housekeeping/tasks.go)
