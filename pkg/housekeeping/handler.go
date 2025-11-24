// Package housekeeping handles scheduled cleanup tasks for runs-fleet.
package housekeeping

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// QueueAPI provides SQS operations for housekeeping processing.
type QueueAPI interface {
	ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]types.Message, error)
	DeleteMessage(ctx context.Context, receiptHandle string) error
}

// TaskType represents the type of housekeeping task.
type TaskType string

const (
	// TaskOrphanedInstances detects and terminates orphaned instances.
	TaskOrphanedInstances TaskType = "orphaned_instances"
	// TaskStaleSSM cleans up stale SSM parameters.
	TaskStaleSSM TaskType = "stale_ssm"
	// TaskOldJobs archives or deletes old job records.
	TaskOldJobs TaskType = "old_jobs"
	// TaskPoolAudit generates pool utilization reports.
	TaskPoolAudit TaskType = "pool_audit"
	// TaskCostReport generates daily cost reports.
	TaskCostReport TaskType = "cost_report"
)

// HousekeepingMessage represents a housekeeping task message.
type HousekeepingMessage struct {
	TaskType  TaskType        `json:"task_type"`
	Timestamp time.Time       `json:"timestamp"`
	Params    json.RawMessage `json:"params,omitempty"`
}

// TaskExecutor executes housekeeping tasks.
type TaskExecutor interface {
	ExecuteOrphanedInstances(ctx context.Context) error
	ExecuteStaleSSM(ctx context.Context) error
	ExecuteOldJobs(ctx context.Context) error
	ExecutePoolAudit(ctx context.Context) error
	ExecuteCostReport(ctx context.Context) error
}

// Handler processes housekeeping tasks from SQS queue.
type Handler struct {
	queueClient  QueueAPI
	taskExecutor TaskExecutor
	config       *config.Config
}

// NewHandler creates a new housekeeping handler.
func NewHandler(q QueueAPI, executor TaskExecutor, cfg *config.Config) *Handler {
	return &Handler{
		queueClient:  q,
		taskExecutor: executor,
		config:       cfg,
	}
}

// Run starts the housekeeping handler loop.
func (h *Handler) Run(ctx context.Context) {
	log.Println("Starting housekeeping handler loop...")

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Housekeeping handler shutting down...")
			return

		case <-ticker.C:
			messages, err := h.queueClient.ReceiveMessages(ctx, 1, 20)
			if err != nil {
				log.Printf("Failed to receive housekeeping messages: %v", err)
				continue
			}

			for _, msg := range messages {
				// Process sequentially - order may matter for some tasks
				if err := h.processMessage(ctx, msg); err != nil {
					log.Printf("Failed to process housekeeping message: %v", err)
				}
			}
		}
	}
}

// processMessage processes a single housekeeping message.
func (h *Handler) processMessage(ctx context.Context, msg types.Message) error {
	if msg.Body == nil {
		return fmt.Errorf("message body is nil")
	}

	var hkMsg HousekeepingMessage
	if err := json.Unmarshal([]byte(*msg.Body), &hkMsg); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	log.Printf("Processing housekeeping task: %s", hkMsg.TaskType)

	// Add timeout for task execution
	taskCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Execute the appropriate task
	var err error
	switch hkMsg.TaskType {
	case TaskOrphanedInstances:
		err = h.taskExecutor.ExecuteOrphanedInstances(taskCtx)
	case TaskStaleSSM:
		err = h.taskExecutor.ExecuteStaleSSM(taskCtx)
	case TaskOldJobs:
		err = h.taskExecutor.ExecuteOldJobs(taskCtx)
	case TaskPoolAudit:
		err = h.taskExecutor.ExecutePoolAudit(taskCtx)
	case TaskCostReport:
		err = h.taskExecutor.ExecuteCostReport(taskCtx)
	default:
		log.Printf("Unknown task type: %s", hkMsg.TaskType)
	}

	if err != nil {
		log.Printf("Task %s failed: %v", hkMsg.TaskType, err)
		// Don't delete message on failure - let it retry via visibility timeout
		return err
	}

	// Delete message on success
	if msg.ReceiptHandle != nil {
		if err := h.queueClient.DeleteMessage(ctx, *msg.ReceiptHandle); err != nil {
			return fmt.Errorf("failed to delete message: %w", err)
		}
	}

	log.Printf("Housekeeping task %s completed successfully", hkMsg.TaskType)
	return nil
}
