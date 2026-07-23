// Package metrics provides metrics publishing abstractions and implementations.
package metrics

import (
	"context"
	"strconv"
)

// spotLabel renders the spot/on-demand dimension consistently across backends.
func spotLabel(spot bool) string {
	if spot {
		return "spot"
	}
	return "ondemand"
}

// vcpuLabel renders the vCPU-count dimension.
func vcpuLabel(vcpu int) string { return strconv.Itoa(vcpu) }

// Publisher defines the interface for publishing metrics to various backends.
//
// The taxonomy is organized into labeled families. The repo label is high
// cardinality and is therefore restricted to the three job-lifecycle counters
// (PublishJobEnqueued, PublishJobAssigned, PublishJobCompleted). Every other
// metric uses only the small enum label sets documented per method. Do not add
// repo to any histogram or any other metric.
type Publisher interface {
	// Close releases any resources held by the publisher.
	// Implementations that don't need cleanup should return nil.
	Close() error

	// --- Job lifecycle ---

	// PublishJobEnqueued increments jobs_enqueued_total when a job is queued.
	PublishJobEnqueued(ctx context.Context, pool, arch, capacity, repo string) error

	// PublishJobAssigned increments jobs_assigned_total when a job is assigned to
	// compute. source is warm_pool or cold_start. This is the success side of the
	// fulfillment SLA: it counts every request for which we delivered a runner. Its
	// failure counterpart is PublishSchedulingFailure.
	PublishJobAssigned(ctx context.Context, pool, source, repo string) error

	// PublishRunnerConfirmed increments runner_confirmed_total when a launched
	// instance's runner registers and begins executing the job (the agent's
	// "started" signal). A flatline here while jobs_assigned keeps climbing is a
	// leading indicator of a fleet-wide runner-registration failure.
	PublishRunnerConfirmed(ctx context.Context, pool string) error

	// PublishJobCompleted increments jobs_completed_total when a job finishes.
	// result is OUR runner's operational lifecycle: served, interrupted, error, or
	// timeout. served means the runner ran the job to completion and exited cleanly,
	// regardless of whether the client's workflow steps passed or failed. result is
	// never derived from the GitHub workflow conclusion — whether the workflow
	// succeeds or fails is the client's concern, not ours. The fulfillment SLA is
	// assignment-based (PublishJobAssigned vs PublishSchedulingFailure), not this
	// counter.
	PublishJobCompleted(ctx context.Context, pool, result, repo string) error

	// PublishJobRequeued increments jobs_requeued_total when a job is re-queued.
	PublishJobRequeued(ctx context.Context, reason string) error

	// PublishJobDeduplicated increments jobs_deduplicated_total when a job message
	// is discarded because the job was already claimed by a concurrent processor
	// (the dual-path dispatch: the fast direct path and the durable queue copy race
	// to claim, and the loser drops its message). path identifies the discarding
	// path (e.g. "queue"). This is correct deduplication, NOT a fulfillment failure:
	// the job did get a runner via the winning path, so it must never be counted as
	// a PublishSchedulingFailure.
	PublishJobDeduplicated(ctx context.Context, path string) error

	// PublishJobWaitSeconds records time from enqueue to assignment, by pool and
	// source.
	PublishJobWaitSeconds(ctx context.Context, pool, source string, seconds float64) error

	// PublishJobStartupSeconds records the end-to-end job acquisition latency on
	// the GitHub clock (workflow_job created to started), by pool and source.
	// source is warm_pool, cold_start, or empty when it cannot be resolved. This
	// is the headline startup number; it spans strictly more than JobWaitSeconds
	// (which covers only enqueue to assignment).
	PublishJobStartupSeconds(ctx context.Context, pool, source string, seconds float64) error

	// PublishAgentBootstrapSeconds records a single agent-side bootstrap segment,
	// by pool and phase. phase is a closed enum: boot, config, runner_download,
	// registration, or total.
	PublishAgentBootstrapSeconds(ctx context.Context, pool, phase string, seconds float64) error

	// PublishJobExecutionSeconds records job execution duration, by pool and
	// result.
	PublishJobExecutionSeconds(ctx context.Context, pool, result string, seconds float64) error

	// --- Fleet / provisioning ---

	// PublishInstanceProvisionSeconds records time to provision an instance, by
	// source (warm_pool or cold_start) and family.
	PublishInstanceProvisionSeconds(ctx context.Context, source, family string, seconds float64) error

	// PublishFleetCreate increments fleet_create_total, by capacity and result.
	PublishFleetCreate(ctx context.Context, capacity, result string) error

	// PublishFleetCreateSeconds records EC2 CreateFleet latency, by capacity.
	PublishFleetCreateSeconds(ctx context.Context, capacity string, seconds float64) error

	// PublishInstances sets the instances gauge for a state/capacity/pool set.
	PublishInstances(ctx context.Context, state, capacity, pool string, n int) error

	// PublishSpotInterruption increments the spot interruption counter, by family.
	PublishSpotInterruption(ctx context.Context, family string) error

	// PublishCircuitBreakerTrip increments the circuit breaker trip counter for an
	// instance type.
	PublishCircuitBreakerTrip(ctx context.Context, instanceType string) error

	// PublishCircuitBreakerOpen sets the circuit breaker open gauge (1 open, 0
	// closed) for an instance type.
	PublishCircuitBreakerOpen(ctx context.Context, instanceType string, open bool) error

	// --- Pools ---

	// PublishPoolInstances sets the pool instance gauge for a pool and state.
	// state is running, stopped, ready, or busy.
	PublishPoolInstances(ctx context.Context, pool, state string, n int) error

	// PublishPoolDesired sets the desired pool instance gauge for a pool and kind.
	// kind is running or stopped.
	PublishPoolDesired(ctx context.Context, pool, kind string, n int) error

	// PublishPoolAction increments pool_actions_total for an action and reason.
	// action is create, stop, terminate, or start.
	PublishPoolAction(ctx context.Context, pool, action, reason string) error

	// PublishPoolReconcileSeconds records pool reconcile loop latency.
	PublishPoolReconcileSeconds(ctx context.Context, seconds float64) error

	// --- Internals ---

	// PublishMessageProcessingSeconds records queue message processing latency, by
	// queue and result.
	PublishMessageProcessingSeconds(ctx context.Context, queue, result string, seconds float64) error

	// PublishLockWaitSeconds records lock acquisition wait time, by lock.
	PublishLockWaitSeconds(ctx context.Context, lock string, seconds float64) error

	// PublishWorkerInflight sets the in-flight worker gauge for a queue.
	PublishWorkerInflight(ctx context.Context, queue string, n int) error

	// PublishQueueDepth sets the queue depth gauge for a queue.
	PublishQueueDepth(ctx context.Context, queue string, depth float64) error

	// PublishQueueReceive increments the queue receive counter, by queue and
	// result. result is messages, empty, or error.
	PublishQueueReceive(ctx context.Context, queue, result string) error

	// PublishAWSCallDuration publishes the latency of a single AWS SDK call,
	// dimensioned by service and operation. Emitted on every call so latency
	// distributions and percentiles are observable. service and operation form a
	// small fixed set; callers must not pass unbounded values.
	PublishAWSCallDuration(ctx context.Context, service, operation string, durationSeconds float64) error

	// PublishAWSCallFailure increments a counter for a failed AWS SDK call,
	// dimensioned by service, operation, and a low-cardinality result
	// ("timeout" or "error").
	PublishAWSCallFailure(ctx context.Context, service, operation, result string) error

	// --- Cache / housekeeping / misc ---

	// PublishCacheRequest increments the cache request counter, by result.
	// result is hit or miss.
	PublishCacheRequest(ctx context.Context, result string) error

	// PublishCacheOperation counts a cache operation (reserve, commit, download).
	PublishCacheOperation(ctx context.Context, operation string) error

	// PublishCacheBytesStored adds to the total bytes written to the cache.
	PublishCacheBytesStored(ctx context.Context, bytes int64) error

	// PublishCacheError counts a cache server error, by operation.
	PublishCacheError(ctx context.Context, operation string) error

	// PublishCacheAuthRejected counts a rejected cache auth attempt, by reason.
	PublishCacheAuthRejected(ctx context.Context, reason string) error

	// PublishHousekeepingAction increments the housekeeping action counter by
	// count, labeled by action. action is orphaned_instances, ssm_params,
	// job_records, orphaned_jobs, or stale_jobs.
	PublishHousekeepingAction(ctx context.Context, action string, count int) error

	// PublishSchedulingFailure increments a scheduling failure counter with a task
	// type dimension. This is the failure side of the fulfillment SLA: it counts
	// every request for which we gave up on assigning a runner for any reason
	// (capacity exhaustion with no fallback, claim exhaustion, a housekeeping task
	// that could not schedule). Its success counterpart is PublishJobAssigned. It
	// must never reflect a client workflow's pass/fail, nor a benign dual-path
	// dedup discard (that is PublishJobDeduplicated, which is not an SLA failure).
	PublishSchedulingFailure(ctx context.Context, taskType string) error

	// PublishMessageDeletionFailure increments a message deletion failure counter
	// for a queue.
	PublishMessageDeletionFailure(ctx context.Context, queue string) error

	// PublishServiceCheck publishes a service health check.
	// status: 0=OK, 1=Warning, 2=Critical, 3=Unknown
	PublishServiceCheck(ctx context.Context, name string, status int, message string) error

	// PublishEvent publishes a notable event (e.g., spot interruption, circuit breaker triggered).
	// alertType: "info", "warning", "error", "success"
	PublishEvent(ctx context.Context, title, text, alertType string, tags []string) error

	// --- Cost ---

	// PublishInstanceHours increments instance hours consumed, by capacity and
	// family.
	PublishInstanceHours(ctx context.Context, capacity, family string, hours float64) error

	// PublishEstimatedCost sets the estimated cost gauge in USD.
	PublishEstimatedCost(ctx context.Context, usd float64) error

	// PublishRunnerExecutionSeconds records billable runner execution time,
	// dimensioned by architecture, vCPU count, spot/on-demand, and job result —
	// the standard axis hosted runners bill runner-minutes on, so the sum
	// reconstructs a comparable per-(arch,vCPU) usage breakdown.
	PublishRunnerExecutionSeconds(ctx context.Context, arch string, vcpu int, spot bool, result string, seconds float64) error

	// PublishRunnerToolCacheMiss counts a GitHub Actions tool-cache entry that a job
	// downloaded on-demand because it was not pre-baked into the AMI, dimensioned by
	// tool, version (major.minor), and arch. Used to tune which tool versions to bake.
	PublishRunnerToolCacheMiss(ctx context.Context, tool, version, arch string) error

	// PublishRunnerCacheInterception counts a job by the on-host cache interceptor's
	// outcome (status: engaged | failed | disabled). Makes silent fail-open
	// interception (cache traffic escaping to GitHub) visible.
	PublishRunnerCacheInterception(ctx context.Context, status string) error

	// PublishRunnerBuildCacheInterception counts a job by the transparent buildx
	// layer-cache shim's outcome (status: engaged | skipped | failed | disabled).
	// It is the rollout observability gate: a healthy rollout shows engaged
	// climbing, and any spike in failed surfaces a broken shim before it can
	// silently disable caching fleet-wide.
	PublishRunnerBuildCacheInterception(ctx context.Context, status string) error
}

// NoopPublisher is a no-op implementation of Publisher for testing or disabled metrics.
// All methods are documented on the Publisher interface.
type NoopPublisher struct{}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) Close() error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobEnqueued(context.Context, string, string, string, string) error {
	return nil
}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobAssigned(context.Context, string, string, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishRunnerConfirmed(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobCompleted(context.Context, string, string, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobRequeued(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobDeduplicated(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobWaitSeconds(context.Context, string, string, float64) error {
	return nil
}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobStartupSeconds(context.Context, string, string, float64) error {
	return nil
}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishAgentBootstrapSeconds(context.Context, string, string, float64) error {
	return nil
}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobExecutionSeconds(context.Context, string, string, float64) error {
	return nil
}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishInstanceProvisionSeconds(context.Context, string, string, float64) error {
	return nil
}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishFleetCreate(context.Context, string, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishFleetCreateSeconds(context.Context, string, float64) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishInstances(context.Context, string, string, string, int) error {
	return nil
}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishSpotInterruption(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishCircuitBreakerTrip(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishCircuitBreakerOpen(context.Context, string, bool) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishPoolInstances(context.Context, string, string, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishPoolDesired(context.Context, string, string, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishPoolAction(context.Context, string, string, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishPoolReconcileSeconds(context.Context, float64) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishMessageProcessingSeconds(context.Context, string, string, float64) error {
	return nil
}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishLockWaitSeconds(context.Context, string, float64) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishWorkerInflight(context.Context, string, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishQueueDepth(context.Context, string, float64) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishQueueReceive(context.Context, string, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishAWSCallDuration(context.Context, string, string, float64) error {
	return nil
}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishAWSCallFailure(context.Context, string, string, string) error {
	return nil
}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishCacheRequest(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishCacheOperation(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishCacheBytesStored(context.Context, int64) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishCacheError(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishCacheAuthRejected(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishHousekeepingAction(context.Context, string, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishSchedulingFailure(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishMessageDeletionFailure(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishServiceCheck(context.Context, string, int, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishEvent(context.Context, string, string, string, []string) error {
	return nil
}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishInstanceHours(context.Context, string, string, float64) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishEstimatedCost(context.Context, float64) error { return nil }

// PublishRunnerExecutionSeconds is a no-op.
func (NoopPublisher) PublishRunnerExecutionSeconds(context.Context, string, int, bool, string, float64) error {
	return nil
}

// PublishRunnerToolCacheMiss is a no-op.
func (NoopPublisher) PublishRunnerToolCacheMiss(context.Context, string, string, string) error {
	return nil
}

// PublishRunnerCacheInterception is a no-op.
func (NoopPublisher) PublishRunnerCacheInterception(context.Context, string) error {
	return nil
}

// PublishRunnerBuildCacheInterception is a no-op.
func (NoopPublisher) PublishRunnerBuildCacheInterception(context.Context, string) error {
	return nil
}

// Ensure NoopPublisher implements Publisher.
var _ Publisher = NoopPublisher{}
