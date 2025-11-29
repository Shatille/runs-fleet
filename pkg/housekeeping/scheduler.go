package housekeeping

import (
	"context"
	"encoding/json"
	"log"
	"time"

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
}

// DefaultSchedulerConfig returns the default scheduler configuration.
func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		OrphanedInstancesInterval: 5 * time.Minute,
		StaleSSMInterval:          15 * time.Minute,
		OldJobsInterval:           1 * time.Hour,
		PoolAuditInterval:         10 * time.Minute,
		CostReportInterval:        24 * time.Hour,
	}
}

// Scheduler periodically sends housekeeping task messages to the queue.
type Scheduler struct {
	sqsClient SchedulerSQSAPI
	queueURL  string
	config    SchedulerConfig
	metrics   SchedulerMetrics
}

// NewScheduler creates a new housekeeping scheduler with an SQS client interface.
// This constructor accepts the interface for better testability.
func NewScheduler(sqsClient SchedulerSQSAPI, queueURL string, schedulerCfg SchedulerConfig) *Scheduler {
	return &Scheduler{
		sqsClient: sqsClient,
		queueURL:  queueURL,
		config:    schedulerCfg,
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
	log.Println("Starting housekeeping scheduler...")

	// Create tickers for each task type
	orphanedTicker := time.NewTicker(s.config.OrphanedInstancesInterval)
	ssmTicker := time.NewTicker(s.config.StaleSSMInterval)
	jobsTicker := time.NewTicker(s.config.OldJobsInterval)
	poolTicker := time.NewTicker(s.config.PoolAuditInterval)
	costTicker := time.NewTicker(s.config.CostReportInterval)

	defer orphanedTicker.Stop()
	defer ssmTicker.Stop()
	defer jobsTicker.Stop()
	defer poolTicker.Stop()
	defer costTicker.Stop()

	// Run orphaned instances cleanup immediately on startup
	s.scheduleTask(ctx, TaskOrphanedInstances)

	for {
		select {
		case <-ctx.Done():
			log.Println("Housekeeping scheduler shutting down...")
			return

		case <-orphanedTicker.C:
			s.scheduleTask(ctx, TaskOrphanedInstances)

		case <-ssmTicker.C:
			s.scheduleTask(ctx, TaskStaleSSM)

		case <-jobsTicker.C:
			s.scheduleTask(ctx, TaskOldJobs)

		case <-poolTicker.C:
			s.scheduleTask(ctx, TaskPoolAudit)

		case <-costTicker.C:
			s.scheduleTask(ctx, TaskCostReport)
		}
	}
}

const (
	// maxScheduleRetries is the maximum number of retry attempts for scheduling a task.
	maxScheduleRetries = 3
	// baseRetryDelay is the initial delay before the first retry.
	baseRetryDelay = 1 * time.Second
)

// scheduleTask sends a housekeeping task message to the queue with retry logic.
func (s *Scheduler) scheduleTask(ctx context.Context, taskType TaskType) {
	msg := Message{
		TaskType:  taskType,
		Timestamp: time.Now(),
	}

	body, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal housekeeping message for task %s: %v", taskType, err)
		return
	}

	// Retry with exponential backoff
	var lastErr error
	for attempt := 0; attempt < maxScheduleRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s...
			backoff := baseRetryDelay * time.Duration(1<<uint(attempt-1))
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			log.Printf("Retrying to schedule housekeeping task %s (attempt %d/%d)", taskType, attempt+1, maxScheduleRetries)
		}

		_, err = s.sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:    aws.String(s.queueURL),
			MessageBody: aws.String(string(body)),
		})
		if err == nil {
			log.Printf("Scheduled housekeeping task: %s", taskType)
			return
		}
		lastErr = err
		log.Printf("Failed to schedule housekeeping task %s (attempt %d/%d): %v", taskType, attempt+1, maxScheduleRetries, err)
	}

	log.Printf("ALERT: Failed to schedule housekeeping task %s after %d attempts: %v", taskType, maxScheduleRetries, lastErr)

	// Publish metric for alerting if context wasn't cancelled
	if ctx.Err() == nil && s.metrics != nil {
		if metricErr := s.metrics.PublishSchedulingFailure(ctx, string(taskType)); metricErr != nil {
			log.Printf("Failed to publish scheduling failure metric: %v", metricErr)
		}
	}
}
