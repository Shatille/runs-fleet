// Package termination handles instance termination notifications from agents.
package termination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

var termLog = logging.WithComponent(logging.LogTypeTermination, "handler")

// QueueAPI provides queue operations for termination event processing.
type QueueAPI interface {
	ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error)
	DeleteMessage(ctx context.Context, handle string) error
}

// DBAPI provides database operations for termination processing.
type DBAPI interface {
	MarkJobComplete(ctx context.Context, jobID int64, status string, exitCode, duration int) error
	UpdateJobMetrics(ctx context.Context, jobID int64, startedAt, completedAt time.Time) error
}

// MetricsAPI provides CloudWatch metrics publishing for job completion.
type MetricsAPI interface {
	PublishJobDuration(ctx context.Context, duration int) error
	PublishJobSuccess(ctx context.Context) error
	PublishJobFailure(ctx context.Context) error
}

// Message represents a termination notification from an agent.
type Message struct {
	InstanceID      string    `json:"instance_id"`
	JobID           string    `json:"job_id"`
	Status          string    `json:"status"` // started, success, failure, timeout, interrupted
	ExitCode        int       `json:"exit_code"`
	DurationSeconds int       `json:"duration_seconds"`
	StartedAt       time.Time `json:"started_at"`
	CompletedAt     time.Time `json:"completed_at"`
	Error           string    `json:"error,omitempty"`
	InterruptedBy   string    `json:"interrupted_by,omitempty"`
}

// handlerTickInterval is the interval for the termination handler loop.
// Exposed as a variable to allow testing with shorter durations.
var handlerTickInterval = 1 * time.Second

// Handler processes termination notifications from agents.
type Handler struct {
	queueClient  QueueAPI
	dbClient     DBAPI
	metrics      MetricsAPI
	secretsStore secrets.Store
	config       *config.Config
}

// NewHandler creates a new termination handler.
func NewHandler(q QueueAPI, db DBAPI, m MetricsAPI, secretsStore secrets.Store, cfg *config.Config) *Handler {
	return &Handler{
		queueClient:  q,
		dbClient:     db,
		metrics:      m,
		secretsStore: secretsStore,
		config:       cfg,
	}
}

// Run starts the termination handler loop.
func (h *Handler) Run(ctx context.Context) {
	termLog.Info("termination handler starting")

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	ticker := time.NewTicker(handlerTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			termLog.Info("termination handler shutdown complete")
			return

		case <-ticker.C:
			messages, err := h.queueClient.ReceiveMessages(ctx, 10, 20)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					termLog.Warn("receive messages failed", slog.String("error", err.Error()))
				} else {
					termLog.Error("receive messages failed", slog.String("error", err.Error()))
				}
				continue
			}

			for _, msg := range messages {
				wg.Add(1)
				sem <- struct{}{}

				go func(m queue.Message) {
					defer wg.Done()
					defer func() { <-sem }()

					// Add timeout to prevent message processing from hanging
					msgCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()

					if err := h.processMessage(msgCtx, m); err != nil {
						termLog.Error("message processing failed", slog.String("error", err.Error()))
					}
				}(msg)
			}
		}
	}
}

// processMessage processes a single termination message.
func (h *Handler) processMessage(ctx context.Context, msg queue.Message) error {
	if msg.Body == "" {
		return fmt.Errorf("message body is empty")
	}

	var termMsg Message
	if err := json.Unmarshal([]byte(msg.Body), &termMsg); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	termLog.Info("processing termination",
		slog.String(logging.KeyInstanceID, termMsg.InstanceID),
		slog.String(logging.KeyJobID, termMsg.JobID),
		slog.String("status", termMsg.Status),
		slog.Int("exit_code", termMsg.ExitCode))

	if err := h.validateMessage(&termMsg); err != nil {
		return fmt.Errorf("invalid message: %w", err)
	}

	if err := h.processTermination(ctx, &termMsg); err != nil {
		return fmt.Errorf("failed to process termination: %w", err)
	}

	if msg.Handle != "" {
		if err := h.queueClient.DeleteMessage(ctx, msg.Handle); err != nil {
			return fmt.Errorf("failed to delete message: %w", err)
		}
	}

	return nil
}

// validateMessage validates required fields in termination message.
func (h *Handler) validateMessage(msg *Message) error {
	if msg.InstanceID == "" {
		return fmt.Errorf("instance_id is required")
	}
	if msg.JobID == "" {
		return fmt.Errorf("job_id is required")
	}
	if msg.Status == "" {
		return fmt.Errorf("status is required")
	}
	return nil
}

// processTermination handles the termination notification.
func (h *Handler) processTermination(ctx context.Context, msg *Message) error {
	// Only process completion messages (not "started" messages)
	if msg.Status == "started" {
		return nil
	}

	// Parse job ID from string to int64 (DynamoDB uses Number type)
	jobID, err := strconv.ParseInt(msg.JobID, 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse job ID %q: %w", msg.JobID, err)
	}

	// Update DynamoDB job record (keyed by job_id)
	if err := h.dbClient.MarkJobComplete(ctx, jobID, msg.Status, msg.ExitCode, msg.DurationSeconds); err != nil {
		return fmt.Errorf("failed to mark job complete: %w", err)
	}

	// Update job metrics (timestamps)
	if !msg.StartedAt.IsZero() && !msg.CompletedAt.IsZero() {
		if err := h.dbClient.UpdateJobMetrics(ctx, jobID, msg.StartedAt, msg.CompletedAt); err != nil {
			termLog.Warn("job metrics update failed",
				slog.Int64(logging.KeyJobID, jobID),
				slog.String("error", err.Error()))
		}
	}

	// Publish CloudWatch metrics
	if msg.DurationSeconds > 0 {
		if err := h.metrics.PublishJobDuration(ctx, msg.DurationSeconds); err != nil {
			termLog.Warn("job duration metric publish failed", slog.String("error", err.Error()))
		}
	}

	if msg.Status == "success" {
		if err := h.metrics.PublishJobSuccess(ctx); err != nil {
			termLog.Warn("job success metric publish failed", slog.String("error", err.Error()))
		}
	} else {
		if err := h.metrics.PublishJobFailure(ctx); err != nil {
			termLog.Warn("job failure metric publish failed", slog.String("error", err.Error()))
		}
	}

	// Clean up runner config from secrets store
	if err := h.deleteRunnerConfig(ctx, msg.InstanceID); err != nil {
		termLog.Warn("runner config delete failed",
			slog.String(logging.KeyInstanceID, msg.InstanceID),
			slog.String("error", err.Error()))
	}

	return nil
}

// deleteRunnerConfig deletes the runner configuration from the secrets store.
func (h *Handler) deleteRunnerConfig(ctx context.Context, instanceID string) error {
	if h.secretsStore == nil {
		return nil
	}

	err := h.secretsStore.Delete(ctx, instanceID)
	if err != nil {
		// Check if config was already deleted
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "NotFound") {
			return nil
		}
		return fmt.Errorf("failed to delete runner config for %s: %w", instanceID, err)
	}

	termLog.Info("runner config deleted",
		slog.String(logging.KeyInstanceID, instanceID))
	return nil
}
