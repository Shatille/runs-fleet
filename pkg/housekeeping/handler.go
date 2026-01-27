// Package housekeeping handles scheduled cleanup tasks for runs-fleet.
package housekeeping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/queue"
)

// QueueAPI provides queue operations for housekeeping processing.
type QueueAPI interface {
	ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error)
	DeleteMessage(ctx context.Context, handle string) error
}

// TaskLocker provides distributed locking for housekeeping tasks.
type TaskLocker interface {
	AcquireTaskLock(ctx context.Context, taskType, owner string, ttl time.Duration) error
	ReleaseTaskLock(ctx context.Context, taskType, owner string) error
}

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
)

// Message represents a housekeeping task message.
type Message struct {
	TaskType  TaskType        `json:"task_type"`
	Timestamp time.Time       `json:"timestamp"`
	Params    json.RawMessage `json:"params,omitempty"`
}

// TaskExecutor executes housekeeping tasks.
type TaskExecutor interface {
	ExecuteOrphanedInstances(ctx context.Context) error
	ExecuteStaleSecrets(ctx context.Context) error
	ExecuteOldJobs(ctx context.Context) error
	ExecutePoolAudit(ctx context.Context) error
	ExecuteCostReport(ctx context.Context) error
	ExecuteDLQRedrive(ctx context.Context) error
	ExecuteEphemeralPoolCleanup(ctx context.Context) error
}

// Handler processes housekeeping tasks from SQS queue.
type Handler struct {
	queueClient  QueueAPI
	taskExecutor TaskExecutor
	taskLocker   TaskLocker
	instanceID   string
	config       *config.Config
	log          *logging.Logger
}

// logger returns the logger, using a default if not initialized.
func (h *Handler) logger() *logging.Logger {
	if h.log == nil {
		return logging.WithComponent(logging.LogTypeHousekeep, "handler")
	}
	return h.log
}

// NewHandler creates a new housekeeping handler.
func NewHandler(q QueueAPI, executor TaskExecutor, cfg *config.Config) *Handler {
	return &Handler{
		queueClient:  q,
		taskExecutor: executor,
		config:       cfg,
		log:          logging.WithComponent(logging.LogTypeHousekeep, "handler"),
	}
}

// SetTaskLocker sets the distributed task locker for HA deployments.
// When set, the handler will acquire a lock before executing each task
// to prevent concurrent execution across multiple instances.
func (h *Handler) SetTaskLocker(locker TaskLocker, instanceID string) {
	h.taskLocker = locker
	h.instanceID = instanceID
}

// Run starts the housekeeping handler loop.
func (h *Handler) Run(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			messages, err := h.queueClient.ReceiveMessages(ctx, 1, 20)
			if err != nil {
				h.logger().Error("receive messages failed", slog.String("error", err.Error()))
				continue
			}

			for _, msg := range messages {
				if err := h.processMessage(ctx, msg); err != nil {
					h.logger().Error("process message failed", slog.String("error", err.Error()))
				}
			}
		}
	}
}

// taskLockTTL is the TTL for task locks (task timeout + buffer).
const taskLockTTL = 6 * time.Minute

// processMessage processes a single housekeeping message.
func (h *Handler) processMessage(ctx context.Context, msg queue.Message) error {
	if msg.Body == "" {
		return fmt.Errorf("message body is empty")
	}

	var hkMsg Message
	if err := json.Unmarshal([]byte(msg.Body), &hkMsg); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	if h.taskLocker != nil {
		if err := h.taskLocker.AcquireTaskLock(ctx, string(hkMsg.TaskType), h.instanceID, taskLockTTL); err != nil {
			if errors.Is(err, db.ErrTaskLockHeld) {
				return nil
			}
			return fmt.Errorf("failed to acquire task lock: %w", err)
		}
		defer func() {
			if err := h.taskLocker.ReleaseTaskLock(context.Background(), string(hkMsg.TaskType), h.instanceID); err != nil {
				h.logger().Error("task lock release failed",
					slog.String(logging.KeyTask, string(hkMsg.TaskType)),
					slog.String("error", err.Error()))
			}
		}()
	}

	taskCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	var err error
	switch hkMsg.TaskType {
	case TaskOrphanedInstances:
		err = h.taskExecutor.ExecuteOrphanedInstances(taskCtx)
	case TaskStaleSecrets:
		err = h.taskExecutor.ExecuteStaleSecrets(taskCtx)
	case TaskOldJobs:
		err = h.taskExecutor.ExecuteOldJobs(taskCtx)
	case TaskPoolAudit:
		err = h.taskExecutor.ExecutePoolAudit(taskCtx)
	case TaskCostReport:
		err = h.taskExecutor.ExecuteCostReport(taskCtx)
	case TaskDLQRedrive:
		err = h.taskExecutor.ExecuteDLQRedrive(taskCtx)
	case TaskEphemeralPoolCleanup:
		err = h.taskExecutor.ExecuteEphemeralPoolCleanup(taskCtx)
	default:
		h.logger().Warn("unknown task type", slog.String(logging.KeyTask, string(hkMsg.TaskType)))
	}

	if err != nil {
		h.logger().Error("task failed",
			slog.String(logging.KeyTask, string(hkMsg.TaskType)),
			slog.String("error", err.Error()))
		return err
	}

	if msg.Handle != "" {
		if err := h.queueClient.DeleteMessage(ctx, msg.Handle); err != nil {
			return fmt.Errorf("failed to delete message: %w", err)
		}
	}

	return nil
}
