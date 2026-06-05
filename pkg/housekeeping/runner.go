// Package housekeeping handles scheduled cleanup tasks for runs-fleet.
package housekeeping

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// TaskType represents the type of housekeeping task.
type TaskType string

const (
	// TaskOrphanedInstances detects and terminates orphaned instances.
	TaskOrphanedInstances TaskType = "orphaned_instances"
	// TaskStaleSecrets cleans up stale secrets (SSM parameters or Vault entries).
	TaskStaleSecrets TaskType = "stale_secrets"
	// TaskOldJobs archives or deletes old job records.
	TaskOldJobs TaskType = "old_jobs"
	// TaskPoolAudit generates pool utilization reports.
	TaskPoolAudit TaskType = "pool_audit"
	// TaskCostReport generates daily cost reports.
	TaskCostReport TaskType = "cost_report"
	// TaskDLQRedrive moves messages from DLQ back to main queue.
	TaskDLQRedrive TaskType = "dlq_redrive"
	// TaskEphemeralPoolCleanup removes stale ephemeral pools.
	TaskEphemeralPoolCleanup TaskType = "ephemeral_pool_cleanup"
	// TaskOrphanedJobs cleans up jobs whose instances no longer exist.
	TaskOrphanedJobs TaskType = "orphaned_jobs"
	// TaskStaleJobs detects jobs stuck in running/claiming by checking GitHub API.
	TaskStaleJobs TaskType = "stale_jobs"
	// TaskUnconfirmedRunners recovers jobs whose instance launched but whose
	// runner never registered (stuck in the launched state past the boot budget).
	TaskUnconfirmedRunners TaskType = "unconfirmed_runners"
	// TaskOrphanedPackerInstances terminates packer builder instances left
	// running by killed or cancelled AMI-build workflows.
	TaskOrphanedPackerInstances TaskType = "orphaned_packer_instances"
)

// TaskLocker provides distributed locking for housekeeping tasks.
type TaskLocker interface {
	AcquireTaskLock(ctx context.Context, taskType, owner string, ttl time.Duration) error
	ReleaseTaskLock(ctx context.Context, taskType, owner string) error
}

// TaskExecutor executes housekeeping tasks.
type TaskExecutor interface {
	ExecuteOrphanedInstances(ctx context.Context) error
	ExecuteStaleSecrets(ctx context.Context) error
	ExecuteOldJobs(ctx context.Context) error
	ExecuteOrphanedJobs(ctx context.Context) error
	ExecuteStaleJobs(ctx context.Context) error
	ExecuteUnconfirmedRunners(ctx context.Context) error
	ExecutePoolAudit(ctx context.Context) error
	ExecuteCostReport(ctx context.Context) error
	ExecuteDLQRedrive(ctx context.Context) error
	ExecuteEphemeralPoolCleanup(ctx context.Context) error
	ExecuteOrphanedPackerInstances(ctx context.Context) error
}

// RunnerMetricsAPI publishes the housekeeping runner's per-task execution
// latency. It is satisfied by the concrete metrics.Publisher.
type RunnerMetricsAPI interface {
	PublishMessageProcessingSeconds(ctx context.Context, queue, result string, seconds float64) error
}

// queueHousekeeping is the metric "queue" label for housekeeping task latency.
const queueHousekeeping = "housekeeping"

// taskLockTTL is the TTL for task locks (task timeout + buffer).
const taskLockTTL = 6 * time.Minute

// taskTimeout bounds a single task execution.
const taskTimeout = 4 * time.Minute

// SchedulerConfig holds the per-task run intervals for the housekeeping runner.
type SchedulerConfig struct {
	// OrphanedInstancesInterval is how often to run orphaned instance cleanup.
	// Default: 5 minutes
	OrphanedInstancesInterval time.Duration

	// StaleSSMInterval is how often to run stale SSM parameter cleanup.
	// Default: 15 minutes
	StaleSSMInterval time.Duration

	// OldJobsInterval is how often to run old job records cleanup.
	// Default: 1 hour
	OldJobsInterval time.Duration

	// OrphanedJobsInterval is how often to run orphaned jobs cleanup.
	// Default: 15 minutes
	OrphanedJobsInterval time.Duration

	// PoolAuditInterval is how often to run pool utilization audit.
	// Default: 10 minutes
	PoolAuditInterval time.Duration

	// CostReportInterval is how often to generate cost reports.
	// Default: 24 hours
	CostReportInterval time.Duration

	// DLQRedriveInterval is how often to redrive messages from the main DLQ.
	// Default: 1 minute
	DLQRedriveInterval time.Duration

	// EphemeralPoolCleanupInterval is how often to cleanup stale ephemeral pools.
	// Default: 1 hour
	EphemeralPoolCleanupInterval time.Duration

	// StaleJobsInterval is how often to check for stale jobs via GitHub API.
	// Default: 5 minutes
	StaleJobsInterval time.Duration

	// UnconfirmedRunnersInterval is how often to recover jobs whose runner never
	// registered. Default: 2 minutes
	UnconfirmedRunnersInterval time.Duration

	// OrphanedPackerInstancesInterval is how often to reap stale packer
	// builder instances left behind by killed AMI-build workflows.
	// Default: 15 minutes
	OrphanedPackerInstancesInterval time.Duration
}

// DefaultSchedulerConfig returns the default per-task run intervals.
func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		OrphanedInstancesInterval:       5 * time.Minute,
		StaleSSMInterval:                15 * time.Minute,
		OldJobsInterval:                 1 * time.Hour,
		OrphanedJobsInterval:            15 * time.Minute,
		PoolAuditInterval:               10 * time.Minute,
		CostReportInterval:              24 * time.Hour,
		DLQRedriveInterval:              1 * time.Minute,
		EphemeralPoolCleanupInterval:    1 * time.Hour,
		StaleJobsInterval:               5 * time.Minute,
		UnconfirmedRunnersInterval:      2 * time.Minute,
		OrphanedPackerInstancesInterval: 15 * time.Minute,
	}
}

// Runner executes housekeeping tasks on in-process timers, deduplicated across
// replicas by a distributed task lock. It replaces the former SQS scheduler and
// handler: the work is clock-derived and idempotent, so no queue is needed.
type Runner struct {
	executor   TaskExecutor
	taskLocker TaskLocker
	metrics    RunnerMetricsAPI
	instanceID string
	config     SchedulerConfig
	log        *logging.Logger
}

// NewRunner creates a housekeeping runner with the given executor and intervals.
func NewRunner(executor TaskExecutor, schedulerCfg SchedulerConfig) *Runner {
	return &Runner{
		executor: executor,
		config:   schedulerCfg,
		log:      logging.WithComponent(logging.LogTypeHousekeep, "runner"),
	}
}

// SetTaskLocker sets the distributed task locker for HA deployments. When set,
// the runner acquires a per-task lock before executing so only one replica runs
// a given task each tick.
func (r *Runner) SetTaskLocker(locker TaskLocker, instanceID string) {
	r.taskLocker = locker
	r.instanceID = instanceID
}

// SetMetrics sets the metrics publisher for the runner.
func (r *Runner) SetMetrics(m RunnerMetricsAPI) {
	r.metrics = m
}

func (r *Runner) logger() *logging.Logger {
	if r.log == nil {
		return logging.WithComponent(logging.LogTypeHousekeep, "runner")
	}
	return r.log
}

// taskSpec binds a task type to its run interval and executor method.
type taskSpec struct {
	taskType TaskType
	interval time.Duration
	execute  func(ctx context.Context) error
	initial  bool
}

// taskSpecs returns the per-task schedule. Tasks marked initial run once on
// start, matching the former scheduler's startup behavior.
func (r *Runner) taskSpecs() []taskSpec {
	e := r.executor
	c := r.config
	return []taskSpec{
		{taskType: TaskOrphanedInstances, interval: c.OrphanedInstancesInterval, execute: e.ExecuteOrphanedInstances, initial: true},
		{taskType: TaskDLQRedrive, interval: c.DLQRedriveInterval, execute: e.ExecuteDLQRedrive, initial: true},
		{taskType: TaskStaleSecrets, interval: c.StaleSSMInterval, execute: e.ExecuteStaleSecrets},
		{taskType: TaskOldJobs, interval: c.OldJobsInterval, execute: e.ExecuteOldJobs},
		{taskType: TaskOrphanedJobs, interval: c.OrphanedJobsInterval, execute: e.ExecuteOrphanedJobs},
		{taskType: TaskStaleJobs, interval: c.StaleJobsInterval, execute: e.ExecuteStaleJobs},
		{taskType: TaskUnconfirmedRunners, interval: c.UnconfirmedRunnersInterval, execute: e.ExecuteUnconfirmedRunners},
		{taskType: TaskPoolAudit, interval: c.PoolAuditInterval, execute: e.ExecutePoolAudit},
		{taskType: TaskCostReport, interval: c.CostReportInterval, execute: e.ExecuteCostReport},
		{taskType: TaskEphemeralPoolCleanup, interval: c.EphemeralPoolCleanupInterval, execute: e.ExecuteEphemeralPoolCleanup},
		{taskType: TaskOrphanedPackerInstances, interval: c.OrphanedPackerInstancesInterval, execute: e.ExecuteOrphanedPackerInstances},
	}
}

// Run launches one timer loop per task and blocks until ctx is cancelled and
// all in-flight tasks have drained.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, spec := range r.taskSpecs() {
		wg.Add(1)
		go func(s taskSpec) {
			defer wg.Done()
			r.runTaskLoop(ctx, s)
		}(spec)
	}
	wg.Wait()
}

// runTaskLoop runs one task on its interval until ctx is cancelled.
func (r *Runner) runTaskLoop(ctx context.Context, s taskSpec) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	if s.initial {
		r.tryRunTask(ctx, s)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tryRunTask(ctx, s)
		}
	}
}

// tryRunTask acquires the task lock (skipping if another replica holds it), then
// runs the task on a context detached from SIGTERM so an in-flight task drains
// on shutdown, bounded by taskTimeout.
func (r *Runner) tryRunTask(ctx context.Context, s taskSpec) {
	ctx = logging.ContextWith(ctx, slog.String(logging.KeyTask, string(s.taskType)))

	if r.taskLocker != nil {
		if err := r.taskLocker.AcquireTaskLock(ctx, string(s.taskType), r.instanceID, taskLockTTL); err != nil {
			if errors.Is(err, db.ErrTaskLockHeld) {
				return
			}
			r.logger().Error(ctx, "task lock acquire failed", slog.String("error", err.Error()))
			return
		}
		defer func() {
			releaseCtx := logging.ContextWith(context.Background(), slog.String(logging.KeyTask, string(s.taskType)))
			if err := r.taskLocker.ReleaseTaskLock(releaseCtx, string(s.taskType), r.instanceID); err != nil {
				r.logger().Error(releaseCtx, "task lock release failed", slog.String("error", err.Error()))
			}
		}()
	}

	taskCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskTimeout)
	defer cancel()

	start := time.Now()
	err := s.execute(taskCtx)
	if r.metrics != nil {
		result := "ok"
		if err != nil {
			result = "error"
		}
		_ = r.metrics.PublishMessageProcessingSeconds(taskCtx, queueHousekeeping, result, time.Since(start).Seconds())
	}
	if err != nil {
		r.logger().Error(taskCtx, "task failed", slog.String("error", err.Error()))
	}
}
