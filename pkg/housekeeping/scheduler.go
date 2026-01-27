package housekeeping

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// SchedulerSQSAPI defines SQS operations for the scheduler.
type SchedulerSQSAPI interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// SchedulerMetrics defines metrics operations for the scheduler.
type SchedulerMetrics interface {
	PublishSchedulingFailure(ctx context.Context, taskType string) error
}

// SchedulerConfig holds configuration for the housekeeping scheduler.
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

	// PoolAuditInterval is how often to run pool utilization audit.
	// Default: 10 minutes
	PoolAuditInterval time.Duration

	// CostReportInterval is how often to generate cost reports.
	// Default: 24 hours
	CostReportInterval time.Duration

	// DLQRedriveInterval is how often to redrive messages from DLQ.
	// Default: 1 minute
	DLQRedriveInterval time.Duration

	// EphemeralPoolCleanupInterval is how often to cleanup stale ephemeral pools.
	// Default: 1 hour
	EphemeralPoolCleanupInterval time.Duration
}

// DefaultSchedulerConfig returns the default scheduler configuration.
func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		OrphanedInstancesInterval:    5 * time.Minute,
		StaleSSMInterval:             15 * time.Minute,
		OldJobsInterval:              1 * time.Hour,
		PoolAuditInterval:            10 * time.Minute,
		CostReportInterval:           24 * time.Hour,
		DLQRedriveInterval:           1 * time.Minute,
		EphemeralPoolCleanupInterval: 1 * time.Hour,
	}
}

// Scheduler periodically sends housekeeping task messages to the queue.
type Scheduler struct {
	sqsClient SchedulerSQSAPI
	queueURL  string
	config    SchedulerConfig
	metrics   SchedulerMetrics
	log       *logging.Logger
}

// logger returns the logger, using a default if not initialized.
func (s *Scheduler) logger() *logging.Logger {
	if s.log == nil {
		return logging.WithComponent(logging.LogTypeHousekeep, "scheduler")
	}
	return s.log
}

// NewScheduler creates a new housekeeping scheduler with an SQS client interface.
// This constructor accepts the interface for better testability.
func NewScheduler(sqsClient SchedulerSQSAPI, queueURL string, schedulerCfg SchedulerConfig) *Scheduler {
	return &Scheduler{
		sqsClient: sqsClient,
		queueURL:  queueURL,
		config:    schedulerCfg,
		log:       logging.WithComponent(logging.LogTypeHousekeep, "scheduler"),
	}
}

// NewSchedulerFromConfig creates a new housekeeping scheduler using AWS config.
// This is a convenience constructor for production use.
func NewSchedulerFromConfig(cfg aws.Config, queueURL string, schedulerCfg SchedulerConfig) *Scheduler {
	return NewScheduler(sqs.NewFromConfig(cfg), queueURL, schedulerCfg)
}

// SetMetrics sets the metrics publisher for alerting on scheduling failures.
func (s *Scheduler) SetMetrics(m SchedulerMetrics) {
	s.metrics = m
}

// Run starts the scheduler loop.
func (s *Scheduler) Run(ctx context.Context) {
	orphanedTicker := time.NewTicker(s.config.OrphanedInstancesInterval)
	ssmTicker := time.NewTicker(s.config.StaleSSMInterval)
	jobsTicker := time.NewTicker(s.config.OldJobsInterval)
	poolTicker := time.NewTicker(s.config.PoolAuditInterval)
	costTicker := time.NewTicker(s.config.CostReportInterval)
	dlqTicker := time.NewTicker(s.config.DLQRedriveInterval)
	ephemeralPoolTicker := time.NewTicker(s.config.EphemeralPoolCleanupInterval)

	defer orphanedTicker.Stop()
	defer ssmTicker.Stop()
	defer jobsTicker.Stop()
	defer poolTicker.Stop()
	defer costTicker.Stop()
	defer dlqTicker.Stop()
	defer ephemeralPoolTicker.Stop()

	s.scheduleTask(ctx, TaskOrphanedInstances)
	s.scheduleTask(ctx, TaskDLQRedrive)

	for {
		select {
		case <-ctx.Done():
			return

		case <-orphanedTicker.C:
			s.scheduleTask(ctx, TaskOrphanedInstances)

		case <-ssmTicker.C:
			s.scheduleTask(ctx, TaskStaleSecrets)

		case <-jobsTicker.C:
			s.scheduleTask(ctx, TaskOldJobs)

		case <-poolTicker.C:
			s.scheduleTask(ctx, TaskPoolAudit)

		case <-costTicker.C:
			s.scheduleTask(ctx, TaskCostReport)

		case <-dlqTicker.C:
			s.scheduleTask(ctx, TaskDLQRedrive)

		case <-ephemeralPoolTicker.C:
			s.scheduleTask(ctx, TaskEphemeralPoolCleanup)
		}
	}
}

const (
	// maxScheduleRetries is the maximum number of retry attempts for scheduling a task.
	maxScheduleRetries = 3
)

// schedulerBaseRetryDelay is the initial delay before the first retry.
// Exposed as a variable to allow testing with shorter durations.
var schedulerBaseRetryDelay = 1 * time.Second

// scheduleTask sends a housekeeping task message to the queue with retry logic.
func (s *Scheduler) scheduleTask(ctx context.Context, taskType TaskType) {
	msg := Message{
		TaskType:  taskType,
		Timestamp: time.Now(),
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return
	}

	var lastErr error
	for attempt := 0; attempt < maxScheduleRetries; attempt++ {
		if attempt > 0 {
			backoff := schedulerBaseRetryDelay * time.Duration(1<<uint(attempt-1))
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}

		_, err = s.sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:    aws.String(s.queueURL),
			MessageBody: aws.String(string(body)),
		})
		if err == nil {
			return
		}
		lastErr = err
	}

	s.logger().Error("task scheduling failed",
		slog.String(logging.KeyTask, string(taskType)),
		slog.Int("attempts", maxScheduleRetries),
		slog.String("error", lastErr.Error()))

	if ctx.Err() == nil && s.metrics != nil {
		_ = s.metrics.PublishSchedulingFailure(ctx, string(taskType))
	}
}
