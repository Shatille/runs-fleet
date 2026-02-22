// Package metrics provides metrics publishing abstractions and implementations.
package metrics

import "context"

// Publisher defines the interface for publishing metrics to various backends.
type Publisher interface {
	// Close releases any resources held by the publisher.
	// Implementations that don't need cleanup should return nil.
	Close() error

	// PublishQueueDepth publishes the current queue depth as a gauge metric.
	PublishQueueDepth(ctx context.Context, depth float64) error

	// PublishFleetSizeIncrement publishes a fleet size increment event.
	PublishFleetSizeIncrement(ctx context.Context) error

	// PublishFleetSizeDecrement publishes a fleet size decrement event.
	PublishFleetSizeDecrement(ctx context.Context) error

	// PublishJobDuration publishes job execution duration in seconds.
	PublishJobDuration(ctx context.Context, durationSeconds int) error

	// PublishJobSuccess publishes a successful job completion event.
	PublishJobSuccess(ctx context.Context) error

	// PublishJobFailure publishes a failed job event.
	PublishJobFailure(ctx context.Context) error

	// PublishJobQueued publishes a job queued event.
	PublishJobQueued(ctx context.Context) error

	// PublishSpotInterruption publishes a spot instance interruption event.
	PublishSpotInterruption(ctx context.Context) error

	// PublishMessageDeletionFailure publishes a message deletion failure event.
	PublishMessageDeletionFailure(ctx context.Context) error

	// PublishCacheHit publishes a cache hit event.
	PublishCacheHit(ctx context.Context) error

	// PublishCacheMiss publishes a cache miss event.
	PublishCacheMiss(ctx context.Context) error

	// PublishOrphanedInstancesTerminated publishes count of orphaned instances terminated.
	PublishOrphanedInstancesTerminated(ctx context.Context, count int) error

	// PublishSSMParametersDeleted publishes count of SSM parameters deleted.
	PublishSSMParametersDeleted(ctx context.Context, count int) error

	// PublishJobRecordsArchived publishes count of job records archived.
	PublishJobRecordsArchived(ctx context.Context, count int) error

	// PublishOrphanedJobsCleanedUp publishes count of orphaned job records cleaned up.
	PublishOrphanedJobsCleanedUp(ctx context.Context, count int) error

	// PublishStaleJobsReconciled publishes count of stale jobs reconciled via GitHub API.
	PublishStaleJobsReconciled(ctx context.Context, count int) error

	// PublishPoolUtilization publishes pool utilization percentage with pool name dimension.
	PublishPoolUtilization(ctx context.Context, poolName string, utilization float64) error

	// PublishPoolRunningJobs publishes count of jobs with status=running for a pool.
	// This gauge helps detect stale job records that should have been completed.
	PublishPoolRunningJobs(ctx context.Context, poolName string, count int) error

	// PublishSchedulingFailure publishes a scheduling failure event with task type dimension.
	PublishSchedulingFailure(ctx context.Context, taskType string) error

	// PublishCircuitBreakerTriggered publishes a circuit breaker triggered event with instance type dimension.
	PublishCircuitBreakerTriggered(ctx context.Context, instanceType string) error

	// PublishJobClaimFailure publishes a job claim failure event that proceeded anyway.
	// This tracks cases where DB claim failed but job processing continued for availability.
	PublishJobClaimFailure(ctx context.Context) error

	// PublishWarmPoolHit publishes a warm pool hit event (job assigned to existing instance).
	PublishWarmPoolHit(ctx context.Context) error

	// PublishFleetSize publishes the current absolute fleet size as a gauge metric.
	PublishFleetSize(ctx context.Context, size int) error

	// PublishServiceCheck publishes a service health check.
	// status: 0=OK, 1=Warning, 2=Critical, 3=Unknown
	PublishServiceCheck(ctx context.Context, name string, status int, message string) error

	// PublishEvent publishes a notable event (e.g., spot interruption, circuit breaker triggered).
	// alertType: "info", "warning", "error", "success"
	PublishEvent(ctx context.Context, title, text, alertType string, tags []string) error
}

// NoopPublisher is a no-op implementation of Publisher for testing or disabled metrics.
// All methods are documented on the Publisher interface.
type NoopPublisher struct{}

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) Close() error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishQueueDepth(context.Context, float64) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishFleetSizeIncrement(context.Context) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishFleetSizeDecrement(context.Context) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobDuration(context.Context, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobSuccess(context.Context) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobFailure(context.Context) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobQueued(context.Context) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishSpotInterruption(context.Context) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishMessageDeletionFailure(context.Context) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishCacheHit(context.Context) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishCacheMiss(context.Context) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishOrphanedInstancesTerminated(context.Context, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishSSMParametersDeleted(context.Context, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobRecordsArchived(context.Context, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishOrphanedJobsCleanedUp(context.Context, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishStaleJobsReconciled(context.Context, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishPoolUtilization(context.Context, string, float64) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishPoolRunningJobs(context.Context, string, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishSchedulingFailure(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishCircuitBreakerTriggered(context.Context, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishJobClaimFailure(context.Context) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishWarmPoolHit(context.Context) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishFleetSize(context.Context, int) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishServiceCheck(context.Context, string, int, string) error { return nil }

//nolint:revive // Interface implementation - documented on Publisher interface
func (NoopPublisher) PublishEvent(context.Context, string, string, string, []string) error {
	return nil
}

// Ensure NoopPublisher implements Publisher.
var _ Publisher = NoopPublisher{}
