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
}

// NewScheduler creates a new housekeeping scheduler.
func NewScheduler(cfg aws.Config, queueURL string, schedulerCfg SchedulerConfig) *Scheduler {
	return &Scheduler{
		sqsClient: sqs.NewFromConfig(cfg),
		queueURL:  queueURL,
		config:    schedulerCfg,
	}
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

// scheduleTask sends a housekeeping task message to the queue.
func (s *Scheduler) scheduleTask(ctx context.Context, taskType TaskType) {
	msg := Message{
		TaskType:  taskType,
		Timestamp: time.Now(),
	}

	body, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal housekeeping message: %v", err)
		return
	}

	_, err = s.sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(s.queueURL),
		MessageBody: aws.String(string(body)),
	})
	if err != nil {
		log.Printf("Failed to schedule housekeeping task %s: %v", taskType, err)
		return
	}

	log.Printf("Scheduled housekeeping task: %s", taskType)
}
