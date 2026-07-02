# Metrics

runs-fleet publishes operational metrics through a single `metrics.Publisher`
interface (`pkg/metrics/publisher.go`) fanned out to any combination of three
backends. This document is the reference for every metric, its dimensions, and
how it is produced.

## Backends and naming

| Backend | Enable | Namespace / prefix | Name style | Notes |
|---------|--------|--------------------|------------|-------|
| CloudWatch | `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` (default **true**) | `RunsFleet` namespace | `PascalCase` metric, `PascalCase` dimensions | Fixed namespace (not configurable). Some high-frequency histograms are no-oped here (see below). |
| Prometheus | `RUNS_FLEET_METRICS_PROMETHEUS_ENABLED` | `runs_fleet_` name prefix | `snake_case`, counters end `_total` | Scraped from the `/metrics` endpoint. |
| Datadog | `RUNS_FLEET_METRICS_DATADOG_ENABLED` | `runs_fleet.` name prefix | `snake_case` (DogStatsD) | Also carries `ServiceCheck` / `Event`, which the other backends drop. For exact metric strings see `pkg/metrics/datadog.go`. |

When more than one backend is enabled they are wrapped in a `MultiPublisher`
that forwards every call to each; when none is enabled a `NoopPublisher` is used
(zero overhead). The tables below list the CloudWatch and Prometheus names as
the canonical reference; the Datadog name mirrors the Prometheus base under the
`runs_fleet.` prefix.

### Cardinality policy

The `repo` label is high-cardinality and is intentionally restricted to the
three job-lifecycle counters (`JobsEnqueued`, `JobsAssigned`, `JobsCompleted`).
Every other metric uses only small, enumerated label sets. **Do not add `repo`
(or any other unbounded value) to a histogram or to any additional metric.**
Empty dimension values are dropped on the CloudWatch backend so an absent
optional label (e.g. no pool) does not fan out into a distinct series.

## Metric reference

Type is `counter` unless noted. Latency metrics are Prometheus/Datadog
histograms; on CloudWatch they are emitted as single-sample statistic sets.

### Job lifecycle

| CloudWatch | Prometheus | Type | Dimensions | Meaning |
|------------|------------|------|------------|---------|
| `JobsEnqueued` | `jobs_enqueued_total` | counter | Pool, Arch, Capacity, Repo | A job was queued. |
| `JobsAssigned` | `jobs_assigned_total` | counter | Pool, Source, Repo | A runner was delivered for a request (success side of the fulfillment SLA). Source: `warm_pool` \| `cold_start`. |
| `RunnerConfirmed` | `runner_confirmed_total` | counter | Pool | A launched runner registered and began executing (agent "started" signal). A flatline while `JobsAssigned` climbs is a leading indicator of fleet-wide registration failure. |
| `JobsCompleted` | `jobs_completed_total` | counter | Pool, Result, Repo | A job finished. Result is **our** runner lifecycle — `served` \| `interrupted` \| `error` \| `timeout` — never the client workflow's pass/fail. |
| `JobsRequeued` | `jobs_requeued_total` | counter | Reason | A job was re-queued (e.g. spot interruption, bootstrap failure). |
| `JobsDeduplicated` | `jobs_deduplicated_total` | counter | Path | The dual-path dispatch loser dropped its copy. Correct dedup — **not** a scheduling failure. |
| `JobWaitSeconds` | `job_wait_seconds` | histogram | Pool, Source | Enqueue → assignment latency. |
| `JobExecutionSeconds` | `job_execution_seconds` | histogram | Pool, Result | Job execution duration. |

### Fleet / provisioning

| CloudWatch | Prometheus | Type | Dimensions | Meaning |
|------------|------------|------|------------|---------|
| `InstanceProvisionSeconds` | `instance_provision_seconds` | histogram | Source, Family | Time to provision an instance. |
| `FleetCreate` | `fleet_create_total` | counter | Capacity, Result | EC2 CreateFleet outcome. |
| `FleetCreateSeconds` | `fleet_create_seconds` | histogram | Capacity | CreateFleet latency. |
| `Instances` | `instances` | gauge | State, Capacity, Pool | Current instance count by state. |
| `SpotInterruptions` | `spot_interruptions_total` | counter | Family | Spot 2-minute interruption warnings. |
| `CircuitBreakerTrip` | `circuit_breaker_trip_total` | counter | InstanceType | Circuit breaker tripped for an instance type. |
| `CircuitBreakerOpen` | `circuit_breaker_open` | gauge (0/1) | InstanceType | Whether the breaker is open. |

### Pools

| CloudWatch | Prometheus | Type | Dimensions | Meaning |
|------------|------------|------|------------|---------|
| `PoolInstances` | `pool_instances` | gauge | PoolName, State | Pool instances by state (`running` \| `stopped` \| `ready` \| `busy`). |
| `PoolDesired` | `pool_desired` | gauge | PoolName, Kind | Desired pool size (`running` \| `stopped`). |
| `PoolActions` | `pool_actions_total` | counter | PoolName, Action, Reason | Reconcile action (`create` \| `stop` \| `terminate` \| `start`). |
| `PoolReconcileSeconds` | `pool_reconcile_seconds` | histogram | — | Reconcile-loop latency. |

### Internals

| CloudWatch | Prometheus | Type | Dimensions | Meaning |
|------------|------------|------|------------|---------|
| _(no-op)_ | `message_processing_seconds` | histogram | Queue, Result | Message processing latency. No-oped on CloudWatch (per-message frequency). |
| _(no-op)_ | `lock_wait_seconds` | histogram | Lock | Lock acquisition wait. No-oped on CloudWatch. |
| `WorkerInflight` | `worker_inflight` | gauge | Queue | In-flight workers per queue. |
| `QueueDepth` | `queue_depth` | gauge | Queue | Approximate queue depth. |
| `QueueReceive` | `queue_receive_total` | counter | Queue, Result | Receive outcome (`messages` \| `empty` \| `error`). |
| _(no-op)_ | `aws_call_duration_seconds` | histogram | Service, Operation | AWS SDK call latency. No-oped on CloudWatch (would double AWS API volume). |
| `AWSCallFailures` | `aws_call_failures_total` | counter | Service, Operation, Result | Failed AWS SDK call (`timeout` \| `error`). |
| `SchedulingFailure` | `scheduling_failure_total` | counter | TaskType | Gave up assigning a runner (failure side of the fulfillment SLA). |
| `MessageDeletionFailures` | `message_deletion_failures_total` | counter | Queue | SQS delete failed after processing. |

### Cache

| CloudWatch | Prometheus | Type | Dimensions | Meaning |
|------------|------------|------|------------|---------|
| `CacheRequests` | `cache_requests_total` | counter | Result | Intercepted cache lookup (`hit` \| `miss`). |
| `CacheOperations` | `cache_operations_total` | counter | Operation | Cache op (`reserve` \| `commit` \| `download`). |
| `CacheBytesStored` | `cache_bytes_stored_total` | counter (bytes) | — | Bytes written to the cache. Includes v1 commit sizes **and** v2 blob-write bytes metered on the runner (see below). |
| `CacheErrors` | `cache_errors_total` | counter | Operation | Cache server error. |
| `CacheAuthRejected` | `cache_auth_rejected_total` | counter | Reason | Rejected cache auth attempt. |
| `RunnerCacheInterception` | `runner_cache_interception_total` | counter | Status | Per-job cache-interceptor outcome (`engaged` \| `failed` \| `disabled`). See below. |

### Housekeeping / cost

| CloudWatch | Prometheus | Type | Dimensions | Meaning |
|------------|------------|------|------------|---------|
| `HousekeepingActions` | `housekeeping_actions_total` | counter | Action | Cleanup count by action (`orphaned_instances`, `ssm_params`, `job_records`, `orphaned_jobs`, `stale_jobs`). |
| `InstanceHours` | `instance_hours_total` | counter | Capacity, Family | Instance-hours consumed. |
| `EstimatedCost` | `estimated_cost_usd` | gauge | — | Estimated spend in USD (see cost caveats in the root `CLAUDE.md`). |
| `RunnerExecutionSeconds` | `runner_execution_seconds_total` | counter | Arch, Vcpu, Spot, Result | Billable runner seconds on the axis hosted runners bill on; sum reconstructs a per-(arch,vCPU) minutes breakdown. |
| `RunnerToolCacheMiss` | `runner_tool_cache_miss_total` | counter | Tool, Version, Arch | A GitHub Actions tool-cache entry downloaded on-demand because it was not pre-baked into the AMI. Version is `major.minor`. Used to tune which tool versions to bake. |

`ServiceCheck` and `Event` are Datadog-only (health checks and notable events);
they are no-ops on CloudWatch and Prometheus.

## Agent-sourced metrics

The EC2 agent has no metrics client of its own. Signals it observes during a job
(tool-cache misses, cache-interceptor outcome, v2 cache-write bytes) ride the
completion telemetry (`JobStatus`, `pkg/agent/telemetry.go`) to the termination
queue, where `pkg/termination/handler.go` parses them and emits the metrics
orchestrator-side. These metrics therefore ship in **two parts**:

- The **orchestrator half** (handler + `pkg/metrics`) rides the orchestrator image.
- The **agent half** (the fields the agent populates) is baked into the runner AMI.

The two halves are order-independent — absent telemetry fields are ignored, so
a metric simply stays at zero until both halves are deployed.

## Cache-interception observability

The on-host cache interceptor (`pkg/agent/cacheproxy`) is **fail-open**: if the
proxy, CA, or host-pin fails to engage — or the local TLS listener dies mid-job
— the runner silently talks to GitHub's real Actions cache instead. Without a
signal this is indistinguishable from "the job used no cache." Two metrics make
it visible.

### `RunnerCacheInterception{Status}`

One count per job, tagging the interceptor outcome:

| Status | Meaning |
|--------|---------|
| `engaged` | Proxy, CA, and host-pin all succeeded; the runner's cache traffic is intercepted. |
| `failed` | A fail-open branch was hit (proxy start / CA install / pin) — cache traffic escaped to GitHub. |
| `disabled` | No cache URL was configured; interception was not attempted. |

`engaged` should dominate. Any sustained `failed` is the fail-open alarm — cache
traffic is leaking to GitHub's (flaky) cache and the orchestrator is doing no
work for it.

### v2 write bytes → `CacheBytesStored`

GitHub Actions cache **v1** commits pass through the orchestrator, so their bytes
are already counted server-side. Cache **v2** writes upload the blob directly
from the runner to a presigned S3 URL through the on-host blob shim
(`pkg/cache/blobshim`) — the orchestrator's v2 finalize is a no-op ack and never
sees the payload. The shim meters each successful **data** PUT (a single-shot Put
Blob or a committed block list — never the intermediate block staging, which
would double-count) and the agent reports the per-job total via
`Proxy.BytesWritten()`. The handler folds it into the existing `CacheBytesStored`
counter, so the counter now reflects v1 commits **and** v2 writes with no
double-counting (v2 writes never traverse the orchestrator commit path).
