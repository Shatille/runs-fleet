---
topic: Observability (Metrics, Tracing, Logging, Cost)
last_compiled: 2026-08-21
sources_count: 36
---

# Observability (Metrics, Tracing, Logging, Cost)

## Purpose [coverage: high -- 36 sources]

runs-fleet emits multi-backend metrics, OpenTelemetry traces, structured JSON
logs, and cost figures (a daily markdown report plus the admin Cost tab) so
operators can monitor fleet health, debug incidents across distributed Fargate
instances, and track AWS spend.

The observability surface is split across four packages plus a sampler:

- `pkg/metrics` — A `Publisher` interface (45 `Publish*` methods plus `Close`,
  organized into labeled families: job lifecycle, fleet/provisioning, pools,
  internals, cache/housekeeping, cost) with three concrete backends
  (CloudWatch, Prometheus, Datadog DogStatsD) plus a fan-out `MultiPublisher`
  and a `NoopPublisher`. Backends are individually toggleable via env vars;
  **the CloudWatch backend ships disabled as of 2026-08-21** (see Key
  Decisions). `docs/METRICS.md` is the canonical per-metric reference; this
  article covers the machinery and its sharp edges.
- `pkg/tracing` — OpenTelemetry SDK setup (OTLP gRPC exporter, batcher),
  a package-level `Tracer()`, and W3C TraceContext propagation helpers that
  carry `traceparent` strings through SQS message attributes. Returns a noop
  provider when disabled (zero overhead).
- `pkg/logging` — A thin wrapper around stdlib `log/slog` that pins the global
  handler to JSON-on-stdout, attaches a `host` attribute, exposes curated
  attribute keys and `log_type` categories, and adds context-stashed attrs
  (`ContextWith` / `ContextWithJob`) so task identity (job_id, run_id, repo)
  rides the `context.Context` into every log line.
- `pkg/cost` — Two independent pricing paths over one shared rate ladder:
  **job-attributed** cost (`JobPricer`, used by the daily report and the admin
  cost page) prices completed DynamoDB job records; **fleet-attributed** cost
  (`FleetPricer` + `ComputeFleetMTD`) prices every managed instance for the
  wall-clock time it existed, including idle pool capacity and stopped
  instances still paying for EBS. Explicit about its limits: live
  Pricing-API/spot prices with hard-coded fallbacks (three ARM families, fixed
  70% spot discount), a flat EBS size estimate, no data-transfer line items.
- `pkg/housekeeping/fleet_cost.go` — the 60-second fleet-cost sampler that
  feeds the fleet-attributed path, accumulating into `__fleet_day:` rollup rows
  in the pools table.

## Architecture [coverage: high -- 36 sources]

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
            ├── goroutine → CloudWatchPublisher.PublishX  (5s timeout; OFF by default)
            ├── goroutine → PrometheusPublisher.PublishX  (5s timeout)
            └── goroutine → DatadogPublisher.PublishX     (5s timeout)
```

`cmd/server/main.go:451` appends the CloudWatch publisher only when
`cfg.MetricsCloudWatchEnabled` is set, then always wraps the (possibly empty)
list in `NewMultiPublisher` at `cmd/server/main.go:490`. `NoopPublisher` (same
file as the interface) is a zero-value-friendly stub returned when metrics are
disabled entirely — every method returns `nil`.

Adding a Publisher method means touching **six** points: the interface +
`NoopPublisher` (publisher.go), CloudWatch, Prometheus (field + registration +
method), Datadog, and the `MultiPublisher` fan-out.

### Backend implementations

`CloudWatchPublisher` ([pkg/metrics/cloudwatch.go](../../pkg/metrics/cloudwatch.go))
uses `cloudwatch.Client` from AWS SDK v2. The namespace is the fixed constant
`cloudWatchNamespace = "RunsFleet"` — intentionally **not** configurable
(prevents metric collisions across deployments in one account/region).
Counters go through `putCounter`/`putCounterValue`/`putCounterValueUnit`
(single `MetricDatum` with `Value`, cloudwatch.go:357); gauges and latency
observations go through `putGauge`/`putStatistic` (a single-sample
`StatisticSet` with `SampleCount=1, Sum=Min=Max=value`, cloudwatch.go:375).
**Every one of those is a separate `PutMetricData` request carrying a
one-element `MetricData` slice against an API that accepts 1,000** — there is
no batching layer anywhere. The `dims()` helper drops empty dimension values
so an absent optional label (e.g. no pool) does not mint a distinct series.
Three high-frequency latency metrics are **deliberate no-ops** on this backend
(`PublishMessageProcessingSeconds` cloudwatch.go:190, `PublishLockWaitSeconds`
:196, `PublishAWSCallDuration` :222): un-batched `PutMetricData` would issue a
synchronous API request per message/SDK call — the AWS-call histogram alone
would roughly double the orchestrator's AWS API volume. Those histograms live
on Prometheus/Datadog only; CloudWatch keeps the low-frequency
`AWSCallFailures` counter.

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

### AWS SDK observability middleware

[internal/awsobs/middleware.go](../../internal/awsobs/middleware.go) installs a
smithy-go Initialize middleware on the shared `aws.Config` that times every SDK
operation and emits `AWSCallDuration` / `AWSCallFailures` dimensioned by
service and operation. Two constraints are baked into it: the middlewares
anchor *after* `RegisterServiceMetadata` (context changes propagate downward
only, so a middleware ahead of it sees empty service/operation names), and
calls to the `CloudWatch` service ID are **excluded outright** — the CloudWatch
metrics backend publishes via `PutMetricData` on the same instrumented config,
so timing them would emit a metric whose own publish issues another
`PutMetricData`, amplifying without bound. `timeout.go` in the same package
carries the per-operation deadlines.

### Acquisition-latency pipeline (PR #387, 2026-07-09)

Three distributions decompose "how long until a runner picks up the job":

- **`JobStartupSeconds` (Pool, Source)** — the headline end-to-end number on
  the **GitHub clock**: `workflow_job` created → started. Published from the
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
  `MarkJobStarted`'s `ReturnValueAllNew` echo (`events.JobInfo` carries
  `WarmPoolHit` + `CreatedAt`). Semantics: `warm_pool` = assignment →
  registered (resume + bootstrap); `cold_start` = post-CreateFleet →
  registered (CreateFleet itself is `FleetCreateSeconds`).
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

**Instance-side logs do not go to CloudWatch Logs.** PR #446 (38e7314,
2026-08-18) deleted both the AMI-side CloudWatch log collection and the
agent's `RUNS_FLEET_LOG_GROUP`-gated log-streaming path: the runner instance
role grants no `logs:*` action, so `PutLogEvents` had been retrying into
`AccessDenied` since it was added and neither `/runs-fleet/runner` nor
`/runner/system` was ever created. Runner `_diag` logs instead ship to S3 via
`pkg/agent/logship` (PR #445, 01c048b) and are served back by
[pkg/admin/handler_job_logs.go](../../pkg/admin/handler_job_logs.go); the
detail belongs to [agent-runtime](agent-runtime.md) and
[admin-ui](admin-ui.md). Only the CloudWatch *metrics* collection survived on
the AMI.

### Cost: two attributions over one rate ladder

`rateMemo` ([pkg/cost/fleetpricing.go](../../pkg/cost/fleetpricing.go):47) is
the single hourly-rate ladder shared by both pricers — live AWS price when
available, then the hard-coded table, then a flat spot discount — so the
job-attributed and fleet-sampled figures are derived from one source and their
coverage ratio compares like with like. It memoizes per instance type and is
**not** concurrency-safe (one per pricer).

#### JobPricer — per-job attribution

`JobPricer` ([pkg/cost/jobpricing.go](../../pkg/cost/jobpricing.go)) computes
one job's EC2 cost as a `JobPricing{Total, Spot, OnDemand, Savings, Hours}`
split. It was extracted from the admin cost handler
([pkg/admin/handler_cost.go](../../pkg/admin/handler_cost.go)) in PR #390
(behavior-preserving), so the daily report and the admin cost page price jobs
identically:

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

#### FleetPricer + sampler — fleet-wide attribution (PR #455, 2026-08, 4a815fe)

The job-derived total cannot see boot and teardown, idle pool capacity,
stopped instances still paying for storage, or instances that never got a job
at all. PR #455 added a sampler that measures the fleet directly:

1. `ExecuteFleetCostSample` ([pkg/housekeeping/fleet_cost.go](../../pkg/housekeeping/fleet_cost.go):75)
   runs as housekeeping task `TaskFleetCostSample` ("fleet_cost_sample") on a
   `fleetCostSampleInterval = 60s` schedule
   ([pkg/housekeeping/runner.go](../../pkg/housekeeping/runner.go):55, :255).
2. `describeManagedFleet` pages `DescribeInstances` filtered on
   `tag:runs-fleet:managed=true` plus the billable states
   (`pending`/`running`/`stopping`) **and** `stopped`. This is the only
   enumeration that sees pool members and cold-start instances together —
   pool reconciliation filters on `runs-fleet:pool` and the jobs table never
   records an instance that ran no job.
3. `fleetCostElapsed` prices the window *since the previous checkpoint*
   (`last_sample_at`), not a fixed interval, so a missed tick is absorbed by
   the next one. Yesterday's rollup is queried alongside today's because the
   first tick after local midnight would otherwise find no checkpoint. An
   elapsed gap beyond `fleetCostMaxElapsed = 15m` is clamped and marks the day
   `partial`; a zero or future checkpoint falls back to the nominal interval.
4. `FleetPricer.PriceInterval` ([pkg/cost/fleetpricing.go](../../pkg/cost/fleetpricing.go):137)
   charges a running instance compute + storage and a stopped one storage
   alone. `Hours` counts billable compute only, so a stopped instance cannot
   dilute a per-minute rate. EBS is priced from a **flat
   `fleetCostEBSGiB = 100` estimate** at `EBSGiBMonthRate = 0.08` / 730h —
   `ec2types.EbsInstanceBlockDevice` carries no `VolumeSize`, so real sizes
   would need a `DescribeVolumes` fan-out every tick.
5. `busyInstanceSet` calls `ListBusyInstanceIDs`
   ([pkg/db/fleet_cost.go](../../pkg/db/fleet_cost.go):189) to attribute the
   busy share. A lookup failure returns **nil**, and nil attributes nothing
   for that tick (attributing everything would erase the gap the feature
   measures).
6. `AddFleetCostSample` folds the tick into a `__fleet_day:<YYYY-MM-DD>` row
   in the **pools** table using DynamoDB's atomic `ADD` for every monetary and
   duration field (several replicas may sample concurrently; a lost update
   would undercount silently). `last_sample_at` is `SET` because it is a
   checkpoint. `partial` latches true and is never cleared. An empty fleet
   still writes, so the next tick does not measure from a stale timestamp.
7. `ComputeFleetMTDIn` ([pkg/cost/fleetmtd.go](../../pkg/cost/fleetmtd.go):66)
   sums the rollups for a period into a `FleetMTD`. It returns **`(nil, nil)`
   when no day is sampled** — deliberately not zero, because a zero rendered
   beside a non-zero attributed cost reads as "the fleet has no overhead".
   `AttributedPercent` comes from the sampler's own busy-versus-total
   instance-seconds, *not* from dividing the job-priced total by the fleet
   total: job records are hard-deleted after 7 days, so that ratio would decay
   across a month for calendar reasons.
8. The admin Cost tab renders it as `FleetCostBlock` (JSON `fleet`, `omitempty`)
   on `GET /api/cost/summary`
   ([pkg/admin/handler_cost.go](../../pkg/admin/handler_cost.go):169, :278).
   A read failure degrades to nil + a warn log rather than failing the
   response; `EBSEstimated` is always true; `Warning` carries a
   ready-to-display sentence when the period is under-sampled.

#### Report timezone

[pkg/config/timezone.go](../../pkg/config/timezone.go) resolves
`RUNS_FLEET_REPORT_TIMEZONE`, **default `Asia/Seoul`**: the fleet is operated
from Korea, and a UTC day boundary would cut every "cost per day" bucket at
09:00 local and roll a month-to-date total over mid-morning on the 1st. An
unparseable zone is a **hard config error**, not a silent fallback
(`pkg/config/config.go:191`). `Config.ReportLocation()` falls back to UTC only
for a zero/nil Config so tests cannot panic. `cmd/server/main.go:639` pushes it
into the admin cost handler; the sampler reads it at
`pkg/housekeeping/fleet_cost.go:83`. Sampler and reader must agree — a mismatch
puts the range off by a day and drops the current day's accumulating rollup out
of the queried window (`ComputeFleetMTDIn` doc, fleetmtd.go:61).

#### Daily report

`pkg/cost/reporter.go` defines `Reporter` with injected dependencies: a
`JobLister` (satisfied by `*db.Client`), three AWS clients (`CloudWatchAPI`,
`S3API`, `SNSAPI`), a `PriceFetcherAPI` for live on-demand prices, and a
`SpotPricer` (satisfied by `*fleet.Manager`) for live spot prices.
`GenerateDailyReport` is the entry point (dispatched from
[pkg/housekeeping/tasks.go](../../pkg/housekeeping/tasks.go)):

1. Compute a rolling 24-hour **UTC** window aligned to the current hour
   (`time.Now().UTC().Truncate(time.Hour)` minus 24h); the report is dated
   by the window's start day. Note this path is still UTC — the report
   timezone governs the *fleet-cost day buckets* and the admin Cost tab, not
   the daily report's window.
2. `accumulateEC2Costs` lists finished job records from DynamoDB with
   `db.AdminJobFilter{CompletedOnly: true, Since: start, Until: end}`.
   "Finished" is keyed on the presence of `completed_at`, **not** a status
   value: the stored status is GitHub's raw conclusion
   (success/failure/interrupted/…), all of which burned billable EC2 time,
   while unfinished rows have no duration and would be priced with the 0.5h
   fallback. The query is unlimited — no job cap any more (see Gotchas). A nil
   lister degrades to zeros with a warning (test-path safety); a lister
   **error fails the whole run**.
3. Each job is priced by a fresh `JobPricer`; spot/on-demand cost, hours, and
   savings aggregate into the breakdown.
4. `spotInterruptionCount` (reporter.go:332) queries CloudWatch with
   metric-math
   `SUM(SEARCH('{RunsFleet,Family} MetricName="SpotInterruptions"', 'Sum', 3600))`
   — the Family-dimensioned form, since a plain undimensioned lookup matches
   nothing. It returns `(int, bool)`: the bool reports whether any datapoint
   was actually observed, so an absent series (now the common case, with the
   CloudWatch backend off) renders as "unavailable" rather than a fabricated 0.
5. Supporting-service estimates scale off the completed-job count: Fargate
   ($1.20/day flat), SQS ($0.40/M reqs), DynamoDB ($1.25/M writes + $0.25/M
   reads), CloudWatch logs ($0.50/GB), and S3 ($0.05 flat).
6. `runnerMinuteCost` (in [pkg/cost/runnerminutes.go](../../pkg/cost/runnerminutes.go))
   issues a second `GetMetricData` call — one metric-math `SUM(SEARCH(...))`
   query per distinct `(arch, vCPU)` shape in `fleet.InstanceCatalog` — to
   total `RunnerExecutionSeconds` (dimensioned by `Arch`/`Vcpu`/`Spot`/
   `Result`). It converts seconds to billable vCPU-minutes and multiplies by
   `cost.DefaultRunnerMinuteRates()` (amd64 $0.002, arm64 $0.00125) to
   produce the standard hosted-runner-equivalent cost. A query failure is
   logged and skipped (the figure stays zero).
7. `generateMarkdownReport` formats a markdown report with a disclaimer
   block; the runner-minute section is included only when the figure is
   non-zero, and the spot-interruption line switches to
   `"unavailable (CloudWatch metrics disabled)"` when unobserved.
8. Upload to S3 at `cost/<YYYY>/<MM>/<DD>.md` (skipped if `reportsBucket`
   is empty); publish to SNS with subject `runs-fleet Daily Cost Report -
   <date>` (skipped if `snsTopicARN` is empty). Both failures are warnings,
   not errors.

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

## Talks To [coverage: high -- 36 sources]

| Component             | Direction | Backend / API                                |
| --------------------- | --------- | -------------------------------------------- |
| CloudWatchPublisher   | out       | `cloudwatch.PutMetricData` (namespace `RunsFleet`) — disabled by default |
| PrometheusPublisher   | in        | HTTP `/metrics` scrape (handler at `Handler()`)    |
| DatadogPublisher      | out       | DogStatsD UDP (default `127.0.0.1:8125`)           |
| awsobs middleware     | out       | `Publisher.PublishAWSCallDuration` / `…Failure` (skips the CloudWatch service ID) |
| tracing.Setup         | out       | OTLP gRPC collector (`RUNS_FLEET_OTEL_ENDPOINT`)   |
| tracing propagation   | both      | SQS message attributes (`traceparent`)             |
| logging.Init          | out       | `os.Stdout` (JSON lines)                           |
| Reporter (cost)       | out       | DynamoDB scan via `db.Client.ListJobsForAdmin` (`CompletedOnly`) |
| Reporter (cost)       | out       | `cloudwatch.GetMetricData` (SpotInterruptions + RunnerExecutionSeconds metric-math) |
| Reporter (cost)       | out       | `s3.PutObject` (cost reports bucket)               |
| Reporter (cost)       | out       | `sns.Publish` (daily-cost SNS topic)               |
| Fleet-cost sampler    | out       | `ec2.DescribeInstances` (tag `runs-fleet:managed`), `dynamodb.UpdateItem` (`__fleet_day:` ADD), `dynamodb.Scan` (busy jobs) |
| ComputeFleetMTD       | in        | `dynamodb.Scan` (ConsistentRead) over `__fleet_day:` rows |
| Admin CostHandler     | out       | `ComputeFleetMTDIn` → `GET /api/cost/summary` `fleet` block |
| JobPricer/FleetPricer | out       | Pricing API via `PriceFetcher`; `ec2.DescribeSpotPriceHistory` via `fleet.Manager` spot cache |
| PriceFetcher          | out       | `pricing.GetProducts` (us-east-1 endpoint)         |

Metric producers span the codebase; the notable cross-package flows are the
webhook `in_progress` path (→ `JobStartupSeconds`) and the termination
handler, which converts agent telemetry into `RunnerConfirmed`,
`InstanceProvisionSeconds`, `AgentBootstrapSeconds`, `JobsCompleted`,
`JobExecutionSeconds`, `RunnerExecutionSeconds`, tool-cache, cache-interception,
buildx-layer-cache, and runner-log-upload counters — see
[events-and-termination](events-and-termination.md).

## API Surface [coverage: high -- 36 sources]

### Publisher interface (`pkg/metrics/publisher.go`)

45 `Publish*` methods plus `Close`, organized into labeled families (full
per-metric reference: [docs/METRICS.md](../../docs/METRICS.md)):

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
- **Cost / runner-side**: `PublishInstanceHours(capacity, family, hours)`,
  `PublishEstimatedCost(usd)`, `PublishRunnerExecutionSeconds(arch, vcpu,
  spot, result, seconds)`, `PublishRunnerToolCacheMiss(tool, version, arch)`,
  `PublishRunnerCacheInterception(status)`,
  `PublishRunnerBuildCacheInterception(status)` — buildx layer-cache shim
  outcome (`engaged`/`skipped`/`failed`/`disabled`),
  `PublishRunnerLogUpload(status)` — S3 runner-log upload outcome
  (`uploaded`/`partial`/`failed`/`skipped`/`disabled`; a fleet whose instance
  role never gained `s3:PutObject` reports `failed` on every job, which is the
  only signal the logs are silently not kept)
- **Lifecycle**: `Close() error`

The fulfillment SLA is documented on the interface itself:
`PublishJobAssigned` (success) vs `PublishSchedulingFailure` (failure);
`PublishJobCompleted`'s `result` is *our* runner's operational lifecycle
(`served`/`interrupted`/`error`/`timeout`), never the client workflow's
pass/fail; `PublishJobDeduplicated` is benign dual-path dedup, never an SLA
failure.

### Constructors

- `NewCloudWatchPublisher(cfg aws.Config) *CloudWatchPublisher` — namespace
  fixed, no override constructor
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
  int, error)`; satisfied by `*db.Client`
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

### Fleet cost (PR #455)

- `NewFleetPricer(onDemand PriceFetcherAPI, spot SpotPricer, ebsGiB int) *FleetPricer`
- `(*FleetPricer).PriceInterval(ctx, instanceType string, spot, running bool,
  d time.Duration) FleetSample` — `FleetSample{Compute, EBS, Total, Spot,
  OnDemand, Hours}`; `d <= 0` returns the zero value
- `EBSHourlyCost(gib int) float64`; `EBSGiBMonthRate = 0.08`
- `FleetCostStore` (in `pkg/cost`) — `GetFleetCostDays(ctx, fromDay, toDay)
  ([]db.FleetCostDay, error)`
- `ComputeFleetMTD(ctx, store, start, now) (*FleetMTD, error)` /
  `ComputeFleetMTDIn(ctx, store, start, now, loc)` — **`(nil, nil)` means
  nothing sampled, not zero cost**
- `(*db.Client).AddFleetCostSample(ctx, day string, d db.FleetCostDelta) error`
- `(*db.Client).GetFleetCostDays(ctx, fromDay, toDay) ([]db.FleetCostDay, error)`
- `(*db.Client).ListBusyInstanceIDs(ctx) ([]string, error)`
- `db.FleetDayFormat = "2006-01-02"`
- `housekeeping.FleetCostStore` (a wider interface: `AddFleetCostSample` +
  `GetFleetCostDays` + `ListBusyInstanceIDs`) and
  `(*Tasks).SetFleetCostStore(s)`; `(*Tasks).ExecuteFleetCostSample(ctx) error`
- `admin.(*CostHandler).SetFleetCostStore(s cost.FleetCostStore)` /
  `.SetReportLocation(loc *time.Location)`
- `config.LoadReportLocation() (*time.Location, error)`;
  `(*config.Config).ReportLocation() *time.Location`;
  `(*config.Config).SetReportLocationForTest(loc)`

`SpotDiscount = 0.7` (package-level constant).

## Data [coverage: high -- 36 sources]

### Metric taxonomy (summary)

The full table lives in [docs/METRICS.md](../../docs/METRICS.md). Naming:
CloudWatch uses `PascalCase` under namespace `RunsFleet`; Prometheus/Datadog
use `snake_case` under `runs_fleet_`/`runs_fleet.` (counters end `_total` on
Prometheus). Highlights and dimensions:

| Metric (CloudWatch)        | Type      | Dimensions       | Notes |
| -------------------------- | --------- | ---------------- | ----- |
| JobsEnqueued / JobsAssigned / JobsCompleted | counter | Pool, …, **Repo** | only metrics allowed the repo label; series count scales with repo count |
| RunnerConfirmed            | counter   | Pool             | agent "started" signal; flatline vs JobsAssigned = registration failure |
| JobWaitSeconds             | histogram | Pool, Source     | enqueue → assignment (SQS slice only) |
| JobStartupSeconds          | histogram | Pool, Source     | GitHub-clock created → started; headline startup number (PR #387) |
| InstanceProvisionSeconds   | histogram | Source, Family   | assignment → runner-registered |
| AgentBootstrapSeconds      | histogram | Pool, Phase      | boot \| config \| runner_download \| registration \| total |
| JobExecutionSeconds        | histogram | Pool, Result     | |
| FleetCreate[Seconds]       | ctr/histo | Capacity[, Result] | |
| SpotInterruptions          | counter   | Family           | read back by the daily report |
| CircuitBreakerTrip / Open  | ctr/gauge | InstanceType     | |
| PoolInstances / PoolDesired / PoolActions | gauge/ctr | PoolName, … | 9 gauge puts per pool per 60s reconcile pass |
| MessageProcessingSeconds, LockWaitSeconds, AWSCallDuration | histogram | … | **Prometheus/Datadog only** (CloudWatch no-op) |
| QueueReceive               | counter   | Queue, Result    | Result includes `empty` — fires on every idle poll |
| SchedulingFailure          | counter   | TaskType         | failure side of the fulfillment SLA |
| Cache* / RunnerCacheInterception / RunnerBuildCacheInterception / RunnerToolCacheMiss / RunnerLogUpload | counter | … | agent-sourced ones ride termination telemetry |
| RunnerExecutionSeconds     | counter   | Arch, Vcpu, Spot, Result | billable runner seconds; feeds runner-minute cost |
| InstanceHours / EstimatedCost | ctr/gauge | …            | defined but unwired — the reporter prices records directly |

Datadog adds two surfaces missing from the others: `ServiceCheck` and
`Event`.

### `__fleet_day:` rollup rows (pools table)

One row per reporting-zone day, keyed `pool_name = "__fleet_day:<YYYY-MM-DD>"`
([pkg/db/fleet_cost.go](../../pkg/db/fleet_cost.go):23). Attributes:

| Attribute | Op | Meaning |
| --------- | -- | ------- |
| `day` | SET | `YYYY-MM-DD`; lexicographic ordering makes a string range scan a date range scan |
| `cost_usd`, `compute_usd`, `ebs_usd` | ADD | accumulating dollars |
| `instance_seconds` | ADD | instance-time observed, summed across the fleet |
| `attributed_seconds` | ADD | the part spent on instances running a job |
| `partial` | SET (latching) | a tick had to clamp its elapsed window ⇒ the day understates |
| `last_sample_at` | SET | Unix-seconds checkpoint the next tick measures from |

These rows carry **no TTL and no reaper**: they are the only surviving record
of fleet spend, because job records are hard-deleted after 7 days by the
old-jobs housekeeping task. One row per day is ~365/year, which the pools
table absorbs. `fleetDayPrefix` is registered in `db.IsReservedPoolKey`
([pkg/db/pool_config.go](../../pkg/db/pool_config.go):115), so pool
enumeration and reconciliation skip them.

`GetFleetCostDays` uses **`ConsistentRead: true`** — the sampler reads
`last_sample_at` and then writes the window since it, so a stale read would
re-price a window the previous tick already counted. The housekeeping task
lock serializes executions but does not make an eventually-consistent read
fresh.

### Log fields

JSON lines emitted to stdout look like:

```json
{
  "time":"2026-08-21T...",
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

`Breakdown` struct (`pkg/cost/reporter.go`:92):

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
    JobsCompleted     int
    SpotInterruptions int

    // false ⇒ the count is unknown, not zero (CloudWatch backend off, or
    // the query failed). Renders as "unavailable" in the markdown.
    SpotInterruptionsAvailable bool

    RunnerVcpuMinutes float64 // billable vCPU-minutes (Σ runner-minutes × vCPU)
    RunnerMinuteCost  float64 // standard hosted-runner-equivalent cost
}
```

The published artifact is markdown, not JSON — sections: header (date,
24-hour total), EC2 Compute (spot/on-demand cost & hours, savings),
Supporting Services (Fargate, SQS, DynamoDB, CloudWatch, S3), Job
Statistics (completed count, interruptions or `unavailable`, cost-per-job),
optional Runner-Minute Cost, and a disclaimer footer. S3 path:
`cost/<YYYY>/<MM>/<DD>.md`.

### Fleet cost API shape

`FleetMTD` ([pkg/cost/fleetmtd.go](../../pkg/cost/fleetmtd.go):22) →
`admin.FleetCostBlock` (JSON `fleet` on `/api/cost/summary`): `total_cost`,
`compute_cost`, `ebs_cost`, `attributed_cost`, `unattributed_cost`,
`attributed_percent`, `days_covered`, `days_in_period`, `partial`, `warning`,
`ebs_estimated` (always true).

### Hard-coded pricing table (`pkg/cost/reporter.go`)

`instancePricing` is the fallback (and both pricers' base) behind the live
Pricing API — three ARM families, on-demand `us-east-1` prices, 2024
vintage:

- `t4g.{micro,small,medium,large,xlarge,2xlarge}` — `0.0084` to `0.2688`
- `c7g.{medium,large,xlarge,2xlarge}` — `0.0361` to `0.2900`
- `m7g.{medium,large,xlarge,2xlarge}` — `0.0408` to `0.3264`

Unknown instance types fall back to `defaultInstanceHourlyPrice = 0.0336`
(the `t4g.medium` rate).

## Key Decisions [coverage: high -- 36 sources]

- **CloudWatch metrics ship disabled (2026-08-21, PR #456).**
  `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` defaults to `false` in **both**
  `pkg/config/config.go:257` and `deploy/helm/runs-fleet/values.yaml`
  (`metrics.cloudwatch.enabled: false`) — the Helm template sets the env var
  unconditionally (`templates/deployment.yaml:268`), so flipping the Go
  default alone would never reach a deployment. Rationale: **nothing queries
  the `RunsFleet` namespace.** There are no CloudWatch alarms or dashboards,
  and `pkg/admin` performs no CloudWatch reads — the console is
  DynamoDB-backed. Meanwhile every metric is one un-batched `PutMetricData`
  request and every unique dimension combination is a separately billed custom
  metric. Batching was deliberately **not** implemented: it would optimise the
  delivery of data with no consumer. Enable it once something consumes it.
- **Multi-backend by interface, fan-out by composition.** `Publisher` is a
  single interface and `MultiPublisher` is just another implementation —
  callers don't know how many backends are live. Operators compose any
  subset without code changes, which is exactly what made disabling
  CloudWatch a one-line config change rather than a refactor.
- **5-second per-publisher timeout in fan-out.** A slow endpoint can't stall
  the other backends — `MultiPublisher` spawns a goroutine per child, races it
  against `time.After(5s)`, and collects errors via `errors.Join`.
- **Fixed namespaces.** `RunsFleet` / `runs_fleet` are constants, not
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
  synchronous `PutMetricData` each. Prometheus/Datadog carry those; CloudWatch
  keeps only low-frequency counters and single-sample statistics. The awsobs
  middleware separately excludes the CloudWatch service ID to break the
  metric-publishing-a-metric-about-its-own-publish loop.
- **Unavailable ≠ zero (PR #456).** `spotInterruptionCount` returns an
  availability bool so the report prints "unavailable (CloudWatch metrics
  disabled)" rather than a fabricated `0`; a genuinely measured zero still
  renders `0`. `ComputeFleetMTD` applies the same principle at the struct
  level, returning `(nil, nil)` when nothing has been sampled so the Cost tab
  omits the card instead of showing $0.00 fleet overhead.
- **Fleet cost is sampled, not derived (PR #455, 2026-08).** The job-derived
  total structurally cannot see boot/teardown, idle pool capacity, stopped
  instances paying for EBS, or instances that never ran a job. Rather than
  correct the job number, a 60s sampler enumerates `runs-fleet:managed`
  instances directly and accumulates into day rollups. A sampler that observes
  every T and credits T per instance is unbiased in aggregate for any T; T
  controls variance, not bias — 60s gives ~3% daily error for ~100
  instances/day at 1440 `DescribeInstances` calls, and the per-instance figure
  is statistical either way, which is why it feeds a fleet total and never a
  per-job number.
- **Elapsed-since-checkpoint, not fixed-interval, attribution.** Each tick
  prices the real gap since `last_sample_at`, so a missed tick is absorbed
  rather than lost — capped at 15 minutes so a multi-hour outage cannot come
  back and attribute one enormous phantom block. A clamped tick latches the
  day `partial`, which the UI surfaces as a sentence rather than hiding.
- **Atomic ADD, not read-modify-write, for the rollups.** Several
  orchestrator replicas may sample concurrently; a lost update would
  undercount the fleet with no external signal. `last_sample_at` is `SET`
  because it is a checkpoint, not an accumulator.
- **Coverage from one sampling pass, not from dividing two totals.**
  `AttributedPercent` comes from the sampler's own busy-vs-total
  instance-seconds. Dividing the job-priced total by the fleet total would
  decay across a month for a calendar reason — job records are hard-deleted at
  7 days — and read as worsening attribution.
- **Report timezone defaults to `Asia/Seoul`, and a bad zone is fatal.** The
  fleet is operated from Korea; a UTC boundary splits a working day at 09:00
  local. An unparseable `RUNS_FLEET_REPORT_TIMEZONE` fails config load rather
  than falling back, because bucketing cost into the wrong days is invisible
  once it starts and a deployment would carry the mistake indefinitely.
- **One shared rate ladder (`rateMemo`) for both pricers.** Job-attributed and
  fleet-sampled figures resolve rates through the same live→table→discount
  cascade, so the coverage ratio between them compares like with like. Both
  pricers are per-run/per-tick and not concurrency-safe by documented
  contract; cross-request caching lives in the concurrency-safe `PriceFetcher`
  (24h) and fleet spot cache (5min) underneath.
- **EBS is priced at all, from an estimate.** A stopped warm-pool instance
  costs nothing else, so omitting storage would leave the stopped-pool half of
  the fleet reporting zero — the exact blind spot the fleet number exists to
  expose. `DescribeInstances` reports volume IDs but not sizes, so a flat
  100 GiB is assumed rather than fanning out `DescribeVolumes` every tick;
  the API labels the component estimated.
- **Fleet cost is opt-in-by-wiring.** With no `FleetCostStore` set, the
  sampler is inert and both the cost API and the report omit the fleet fields
  entirely rather than reporting a zero.
- **`completed_at`, not a status string, defines "finished".** Both the report
  and the admin cost page filter on `AdminJobFilter.CompletedOnly`
  (`attribute_exists(completed_at)`). The stored status is GitHub's raw
  conclusion — a vocabulary that drifts — while `completed_at` is written only
  by `MarkJobComplete` alongside `duration_seconds`. See
  [job-state-machine](job-state-machine.md).
- **EC2 costs computed from job records, not CloudWatch (PR #390, 2026-07).**
  The 2026-07 metrics rework retired the counters the reporter aggregated
  (`FleetSizeIncrement`, `JobSuccess`, …), silently zeroing the EC2 section.
  Rather than chase metric names, the reporter prices the same DynamoDB job
  records the admin cost page prices: the record already joins instance type,
  spot flag, and duration from the orchestrator and agent (see
  [db-record-as-rendezvous](../concepts/db-record-as-rendezvous.md)), so the
  daily report and the admin dashboard agree by construction, and a metric
  rename can never zero the report again. This decision is what makes the
  CloudWatch-off default cheap: only two informational figures still read from
  CloudWatch.
- **Acquisition latency made visible (PR #387, 2026-07-09).** A ~35–40s
  runner-acquisition latency was operationally invisible: the admin UI's
  "Avg Startup" measures assignment → started, and `JobWaitSeconds` covers
  only enqueue → assignment. `JobStartupSeconds` (GitHub-clock created →
  started) is the headline number; `InstanceProvisionSeconds` and
  `AgentBootstrapSeconds` decompose where the time goes.
- **Agent has no metrics client.** Everything the agent observes (bootstrap
  timings, tool-cache misses, cache-interceptor outcome, buildx layer-cache
  outcome, log-upload outcome) rides the termination telemetry to SQS; the
  orchestrator parses and publishes. The `Phase` enum for
  `AgentBootstrapSeconds` is fixed orchestrator-side as a cardinality guard —
  the agent never supplies label strings.
- **Datadog `Distribution` over `Histogram` for latencies.** Datadog's
  `Distribution` aggregates percentiles globally across all hosts;
  `Histogram` is per-host.
- **Tracing is noop-by-default.** `Setup` returns a noop provider when
  disabled; instrumentation calls `tracing.Tracer()` unconditionally with
  zero overhead. Trace context crosses process boundaries as a W3C
  `traceparent` string in SQS message attributes, not custom headers.
- **Lazy-handler slog wrapper.** Package-level loggers
  (`var costLog = logging.WithComponent(...)`) are created at package init
  before `main` calls `Init()`. The `lazyHandler` resolves `slog.Default()` at
  log time, so those loggers transparently start emitting JSON once `Init`
  runs.
- **Context-stashed log identity.** `ContextWithJob` threads job identity
  through call graphs without plumbing logger instances; the
  `contextHandler` injects stashed attrs at Handle time.
- **Stdlib `log` redirected to slog as WARN.** Any `log.Print` from
  vendored deps becomes JSON with `log_type=stdlib` — no silent stderr drips.
- **One log route, not a dormant second one (PR #446, 2026-08-18).** The
  CloudWatch Logs path was removed rather than fixed with an IAM grant: it had
  never worked (no `logs:*` on the instance role), and S3 shipping via
  `pkg/agent/logship` already covered the need.
- **Asymmetric failure semantics in the daily report.** A job-lister error
  fails the run — housekeeping retries, and a zeroed-core report is worse
  than none — while CloudWatch, S3, and SNS failures log warnings and
  degrade only their own figures/outputs. The admin fleet block follows the
  same rule: a read failure degrades to `nil`, never a 500.
- **Cost reports are estimates, not billing.** Per-job durations and instance
  types are exact, but pricing is estimate-grade: live Pricing-API/spot prices
  when available, hard-coded table + fixed 70% spot discount as fallback, a
  flat EBS size, hand-tuned supporting-service coefficients; the published
  markdown ends with a disclaimer pointing at AWS Cost Explorer.
- **Pricing API region override + sticky fallback.** `pricing.Client` only
  works in `us-east-1`/`ap-south-1`, so `NewPriceFetcher` force-sets
  `us-east-1`; a single API failure flips `useFallback = true` for the
  fetcher's lifetime (reset only by `RefreshCache`) to avoid hammering a
  broken API on every report.

## Gotchas [coverage: high -- 36 sources]

### CloudWatch backend (relevant when you turn it on)

- **`putGauge` sends `SampleCount=1`, so `Sum` is wrong at any period
  >60s.** Gauges and every `*Seconds` observation go through a single-sample
  `StatisticSet` where `Sum = Min = Max = value`
  ([pkg/metrics/cloudwatch.go](../../pkg/metrics/cloudwatch.go):375). Aggregate
  a gauge with the `Sum` statistic over a 5-minute or 1-hour period and
  CloudWatch adds the per-minute values: 5 instances steady for an hour reads
  as `Sum = 300`. Use `Average` (or `Maximum`) for gauges; `Sum` is only
  meaningful on the counter metrics that go through `putCounterValueUnit`.
  Percentile analysis of the `*Seconds` metrics is only meaningful on
  Prometheus/Datadog.
- **CloudWatch does not roll up across dimension values.** Querying
  `PoolInstances` without a `PoolName` returns **NO DATA**, not a fleet total —
  a datum published with dimensions exists only at that exact dimension
  combination. A fleet total needs metric math:
  `SUM(SEARCH('{RunsFleet,PoolName,State} MetricName="PoolInstances" State="running"', 'Average', 60))`.
  This is why `pkg/cost` uses `SUM(SEARCH(...))` for both metrics it reads
  back, and why a plain undimensioned `SpotInterruptions` lookup matched
  nothing.
- **`publishPoolAction` loops one API call per instance.**
  [pkg/pools/manager.go](../../pkg/pools/manager.go):1174 calls
  `PublishPoolAction` once per affected instance, where a single datum with
  `Value: count` would carry the same information. Starting 20 pool instances
  costs 20 `PutMetricData` requests.
- **`QueueReceive` fires on empty receives.** The events worker publishes
  `QueueReceive{Result: "empty"}` on every idle poll
  ([pkg/events/handler.go](../../pkg/events/handler.go):193) and then sleeps
  only a 160–240ms jitter, so an idle orchestrator bills a steady stream of
  `PutMetricData` calls for doing nothing.
- **Pool reconciliation is the largest emitter.** Nine gauge puts per pool per
  pass — 5 `PoolInstances` states, 2 `PoolDesired` kinds, 2 `Instances`
  (`pkg/pools/manager.go`:737–753) — on a 60s ticker, **plus** an extra
  triggered pass per queued-job webhook via `NotifyPoolDemand`
  (`cmd/server/main.go`:759).
- **The `Repo` dimension scales with repository count.** `JobsEnqueued`,
  `JobsAssigned`, and `JobsCompleted` mint a separately billed custom metric
  per repo × other-dimension combination. `docs/METRICS.md` documents this as
  a deliberate accepted tradeoff, not an oversight — but it is the term that
  grows without a code change.
- **Per-pool gauges poisoned by sentinel rows (historical, guarded).**
  Instance claims, task locks, runner sightings, and now `__fleet_day:`
  rollups share the *pools* DynamoDB table, distinguished only by a sentinel
  `pool_name` prefix. Any path that scans the table as if every row were a
  pool and then publishes pool-dimensioned metrics mints **one zero-valued
  series per ephemeral instance ID**. This drove CloudWatch custom metrics to
  3,000+ series (~$900/mo) during busy periods while collapsing back to
  baseline when idle, so it hid between bills. The guard is
  `db.IsReservedPoolKey` ([pkg/db/pool_config.go](../../pkg/db/pool_config.go):111),
  which every enumerating path calls. **When adding a new sentinel prefix,
  register it there** — `fleetDayPrefix` is, and an unregistered prefix
  renders as a phantom pool.
- **Two metrics degrade when CloudWatch is off.** `pkg/cost/reporter.go` reads
  back `SpotInterruptions` and `RunnerExecutionSeconds`, both written only by
  the CloudWatch backend. With it disabled the runner-minute section
  self-hides (the figure is zero) and interruptions print "unavailable". This
  is the accepted cost of the default; re-enable CloudWatch if you need either
  figure.
- **`PublishServiceCheck`/`PublishEvent` no-op outside Datadog.** They
  silently return `nil` on CloudWatch and Prometheus. Use them in addition
  to, not instead of, regular metrics.

### Metrics machinery

- **Datadog sample-rate applies only to four metrics.** A 0.1 `SampleRate`
  scales `message_processing_seconds`, `lock_wait_seconds`, `cache_requests`,
  and `cache_operations`; every other metric hard-codes rate 1. Setting
  `SampleRate` expecting an across-the-board volume reduction will surprise
  you.
- **`MultiPublisher` 5s timeout abandons the result, not the work.** A
  backend that consistently exceeds 5 seconds logs a warning every publish
  and still consumes a goroutine for the full duration of the underlying
  call.
- **Two-halves deploy for agent-sourced metrics.** The orchestrator half
  (handler + `pkg/metrics`) rides the orchestrator image; the agent half
  (telemetry fields) rides the **AMI cascade** (`build-runner.yml` →
  `build-amis.yml`). Deploy order is immaterial — zero-as-absent gives
  old-agent compatibility both ways — but `AgentBootstrapSeconds` flatlines
  until *both* halves are live. Don't debug an empty dashboard panel before
  the new AMI has rolled.
- **`JobStartupSeconds` source can be empty.** The Source label is resolved by
  a post-ack `GetJobByJobID` lookup; a nil DB client, missing jobs table, read
  error, or absent record publishes with `source=""` (dropped as a dimension)
  rather than losing the observation. Don't assume `warm_pool + cold_start`
  sums to the total.
- **Admin "Avg Startup" ≠ `JobStartupSeconds`.** The admin panel measures
  assignment → started; the metric measures GitHub created → started. They
  legitimately disagree — the metric includes the webhook → enqueue →
  assignment head the panel excludes.
- **Cross-clock guard on `InstanceProvisionSeconds`.** The metric joins an
  orchestrator-stamped timestamp (`created_at` at assignment) with an
  agent-stamped one (`StartedAt`). `publishProvisionSeconds` publishes only
  when both timestamps are non-zero **and** the span is positive — a skewed
  pair is silently dropped. Expect the histogram to slightly undercount very
  fast warm-pool hits.

### Cost

- **The two cost totals answer different questions and will not match.** The
  Cost tab's headline `TotalCost` prices job execution time; the `fleet` block
  prices instance wall-clock time. `attributed_percent` is the intended way to
  read the gap. Comparing the numbers directly, or treating the job total as
  "the bill", understates spend by whatever boot/teardown/idle capacity costs.
- **`__fleet_day:` rows are the only long-lived cost record, and nothing
  reaps them.** Job records are hard-deleted at 7 days, so any month-to-date
  figure derived from jobs truncates. The rollups have no TTL by design —
  don't add one, and don't include the prefix in a pools-table sweep.
- **A `partial` day understates and stays that way.** One clamped tick (a
  >15-minute gap since the last checkpoint) latches `partial=true` for the
  whole day and it is never cleared. `DaysCovered < DaysInPeriod` also sets it
  at the MTD level. Treat any `partial` total as a floor.
- **Coverage silently drops to zero when the busy lookup fails.**
  `busyInstanceSet` returns nil on a `ListBusyInstanceIDs` error, and a nil set
  attributes **nothing** for that tick — `instance_seconds` still accumulates
  while `attributed_seconds` does not, so `attributed_percent` reads low for a
  reason that has nothing to do with utilization. The only signal is a warn
  log ("busy instance lookup failed, coverage omitted for this tick").
- **Sampler and reader must share the timezone.** The sampler writes day keys
  in `Config.ReportLocation()`; the admin handler queries in
  `CostHandler.location()`. A mismatch (e.g. `SetReportLocation` never called)
  puts the range off by a day at the boundary and drops the current,
  still-accumulating day out of the window. Note also that
  `pkg/db/fleet_cost.go`'s comments call these "per-UTC-day" rollups — the
  code buckets in the configured zone, and the comment is stale.
- **`ListBusyInstanceIDs` scans the jobs table, filter-after-read.** The
  `created_at >= now - maxConcurrencyRuntime` bound is a *correctness* bound
  (a leaked stale row would mark an instance busy forever and inflate the
  attributed share), not a cost one: DynamoDB applies a filter after the read,
  so it trims the response, not the RCU.
- **EBS cost is a flat 100 GiB guess per instance.** Every managed instance,
  whatever its real root volume, is charged `100 × 0.08 / 730` per hour. The
  `ebs_estimated` flag on the API response is the only marker.
- **Zero-duration jobs bill 0.5 hours.** `JobPricer` applies a 0.5-hour
  minimum when `DurationSeconds` is missing or non-positive, so records that
  never recorded a duration inflate the hour and cost totals. (The admin
  runner-minute matrix deliberately *skips* those jobs instead — the two
  figures diverge on such records.) `FleetPricer` has no such floor: a
  non-positive interval returns the zero sample.
- **Neither pricer is concurrency-safe.** `rateMemo`'s memo maps are plain
  maps by documented contract: construct one `JobPricer` per run/request and
  one `FleetPricer` per sampling tick, never shared across goroutines.
  Concurrency safety lives a layer down, in `PriceFetcher` and the fleet spot
  cache.
- **The savings line always says "(70% discount)".** `generateMarkdownReport`
  prints the fixed `SpotDiscount` label even when savings were computed from
  live spot prices; the dollar figure is right, the percentage label is
  cosmetic.
- **The daily report window is UTC, the fleet/admin buckets are not.**
  `GenerateDailyReport` still computes a rolling 24h UTC window and dates the
  report by its UTC start day; only the fleet-cost day keys and the admin Cost
  tab honour `RUNS_FLEET_REPORT_TIMEZONE`. Comparing a daily report against a
  Cost-tab day will show a boundary offset.
- **Pricing API hard-fails to fallback.** A single failed Pricing API call
  flips `useFallback` permanently for that fetcher instance. To recover, call
  `RefreshCache` or construct a new `PriceFetcher`.
- **Hard-coded prices are us-east-1, 2024, three families only.** `t4g`,
  `c7g`, `m7g`; anything else (including the `c8g`/`m8g` Graviton4
  generations the labels support) falls back to `t4g.medium = 0.0336`. Does
  not reflect ap-northeast-1 (this project's default region). Live
  Pricing-API and spot lookups mask this when they succeed; the table is what
  you get when they don't.
- **Data transfer, ENIs, NAT, ECR still excluded.** EBS is now modelled (as
  an estimate) on the fleet path only; the daily report's breakdown still
  covers EC2 compute + Fargate + SQS + DynamoDB + CloudWatch + S3 with
  hand-tuned coefficients.
- **[Fixed 2026-07, PR #390] Cost reporter briefly queried retired metric
  names.** Between the 2026-07 metrics rework and PR #390, `getCostMetrics`
  asked CloudWatch for `FleetSizeIncrement`, `JobSuccess`, `JobFailure`,
  `JobDuration`, and a dimensionless `SpotInterruptions` — none emitted
  anymore — so the daily report's EC2 section computed from zeros. Recorded as
  a live bug on 2026-07-09 and fixed by 2026-07-21. Historical caveat:
  reports generated in that window have meaningless EC2 figures.
- **[Obsolete 2026-08, PR #403] The 10,000-job report cap is gone.** The
  2026-07-21 revision of this article documented a `jobWindowLimit = 10000`
  cap plus `Breakdown.JobsMatched` and an in-report truncation note. PR #403
  (3439671) removed all three when it switched the query to
  `CompletedOnly`: `accumulateEC2Costs` now passes no `Limit` and
  `ListJobsForAdmin` pages the full window. There is no truncation surfacing
  because there is no truncation — but note the query is an unbounded
  DynamoDB `Scan` over the jobs table with a post-read filter, so a very busy
  window costs RCU proportional to table size, not result size.

### Logging

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
- **`LogTypeK8s` still exists.** The K8s runner backend was removed in
  2026-06; the log-type constant survives. Seeing it in a filter dropdown does
  not mean anything emits it.

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
- [pkg/cost/fleetpricing.go](../../pkg/cost/fleetpricing.go)
- [pkg/cost/fleetmtd.go](../../pkg/cost/fleetmtd.go)
- [pkg/cost/pricing.go](../../pkg/cost/pricing.go)
- [pkg/cost/runnerminutes.go](../../pkg/cost/runnerminutes.go)
- [pkg/db/fleet_cost.go](../../pkg/db/fleet_cost.go)
- [pkg/db/pool_config.go](../../pkg/db/pool_config.go)
- [pkg/db/jobs.go](../../pkg/db/jobs.go)
- [pkg/housekeeping/fleet_cost.go](../../pkg/housekeeping/fleet_cost.go)
- [pkg/housekeeping/runner.go](../../pkg/housekeeping/runner.go)
- [pkg/housekeeping/tasks.go](../../pkg/housekeeping/tasks.go)
- [pkg/admin/handler_cost.go](../../pkg/admin/handler_cost.go)
- [pkg/admin/handler_job_logs.go](../../pkg/admin/handler_job_logs.go)
- [pkg/config/timezone.go](../../pkg/config/timezone.go)
- [pkg/config/config.go](../../pkg/config/config.go)
- [pkg/pools/manager.go](../../pkg/pools/manager.go)
- [pkg/events/handler.go](../../pkg/events/handler.go)
- [pkg/termination/handler.go](../../pkg/termination/handler.go)
- [internal/awsobs/middleware.go](../../internal/awsobs/middleware.go)
- [internal/handler/webhook.go](../../internal/handler/webhook.go)
- [cmd/server/main.go](../../cmd/server/main.go)
- [deploy/helm/runs-fleet/values.yaml](../../deploy/helm/runs-fleet/values.yaml)
- [deploy/helm/runs-fleet/templates/deployment.yaml](../../deploy/helm/runs-fleet/templates/deployment.yaml)
- [docs/METRICS.md](../../docs/METRICS.md)
