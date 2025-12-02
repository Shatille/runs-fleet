// Package termination handles instance termination notifications from agents.
package termination

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// QueueAPI provides queue operations for termination event processing.
type QueueAPI interface {
	ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error)
	DeleteMessage(ctx context.Context, handle string) error
}

// DBAPI provides database operations for termination processing.
type DBAPI interface {
	MarkJobComplete(ctx context.Context, instanceID, status string, exitCode, duration int) error
	UpdateJobMetrics(ctx context.Context, instanceID string, startedAt, completedAt time.Time) error
}

// MetricsAPI provides CloudWatch metrics publishing for job completion.
type MetricsAPI interface {
	PublishJobDuration(ctx context.Context, duration int) error
	PublishJobSuccess(ctx context.Context) error
	PublishJobFailure(ctx context.Context) error
}

// SSMAPI provides SSM parameter operations.
type SSMAPI interface {
	DeleteParameter(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
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

// Handler processes termination notifications from agents.
type Handler struct {
	queueClient QueueAPI
	dbClient    DBAPI
	metrics     MetricsAPI
	ssmClient   SSMAPI
	config      *config.Config
}

// NewHandler creates a new termination handler.
func NewHandler(q QueueAPI, db DBAPI, m MetricsAPI, ssmClient SSMAPI, cfg *config.Config) *Handler {
	return &Handler{
		queueClient: q,
		dbClient:    db,
		metrics:     m,
		ssmClient:   ssmClient,
		config:      cfg,
	}
}

// Run starts the termination handler loop.
func (h *Handler) Run(ctx context.Context) {
	log.Println("Starting termination handler loop...")

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Termination handler shutting down...")
			wg.Wait()
			log.Println("Termination handler stopped")
			return

		case <-ticker.C:
			messages, err := h.queueClient.ReceiveMessages(ctx, 10, 20)
			if err != nil {
				log.Printf("Failed to receive messages: %v", err)
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
						log.Printf("Failed to process message: %v", err)
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

	log.Printf("Processing termination: instance_id=%s, job_id=%s, status=%s, exit_code=%d",
		termMsg.InstanceID, termMsg.JobID, termMsg.Status, termMsg.ExitCode)

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

	log.Printf("Termination processed successfully: job_id=%s", termMsg.JobID)
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
		log.Printf("Ignoring job started message: job_id=%s", msg.JobID)
		return nil
	}

	// Update DynamoDB job record (keyed by instance_id)
	if err := h.dbClient.MarkJobComplete(ctx, msg.InstanceID, msg.Status, msg.ExitCode, msg.DurationSeconds); err != nil {
		return fmt.Errorf("failed to mark job complete: %w", err)
	}

	// Update job metrics (timestamps)
	if !msg.StartedAt.IsZero() && !msg.CompletedAt.IsZero() {
		if err := h.dbClient.UpdateJobMetrics(ctx, msg.InstanceID, msg.StartedAt, msg.CompletedAt); err != nil {
			log.Printf("Warning: failed to update job metrics: %v", err)
		}
	}

	// Publish CloudWatch metrics
	if msg.DurationSeconds > 0 {
		if err := h.metrics.PublishJobDuration(ctx, msg.DurationSeconds); err != nil {
			log.Printf("Warning: failed to publish job duration metric: %v", err)
		}
	}

	if msg.Status == "success" {
		if err := h.metrics.PublishJobSuccess(ctx); err != nil {
			log.Printf("Warning: failed to publish job success metric: %v", err)
		}
	} else {
		if err := h.metrics.PublishJobFailure(ctx); err != nil {
			log.Printf("Warning: failed to publish job failure metric: %v", err)
		}
	}

	// Clean up SSM parameter
	if err := h.deleteSSMParameter(ctx, msg.InstanceID); err != nil {
		log.Printf("Warning: failed to delete SSM parameter: %v", err)
	}

	return nil
}

// deleteSSMParameter deletes the runner configuration parameter from SSM.
func (h *Handler) deleteSSMParameter(ctx context.Context, instanceID string) error {
	parameterPath := fmt.Sprintf("/runs-fleet/runners/%s/config", instanceID)

	_, err := h.ssmClient.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(parameterPath),
	})

	if err != nil {
		// Check if parameter was already deleted
		if strings.Contains(err.Error(), "ParameterNotFound") {
			log.Printf("SSM parameter already deleted: %s", parameterPath)
			return nil
		}
		return fmt.Errorf("failed to delete SSM parameter %s: %w", parameterPath, err)
	}

	log.Printf("Deleted SSM parameter: %s", parameterPath)
	return nil
}
