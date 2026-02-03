package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// TelemetryClient defines the interface for sending job telemetry.
// Implementations handle backend-specific telemetry (SQS vs Valkey).
type TelemetryClient interface {
	SendJobStarted(ctx context.Context, status JobStatus) error
	SendJobCompleted(ctx context.Context, status JobStatus) error
}

// Job status constants.
const (
	StatusStarted     = "started"
	StatusSuccess     = "success"
	StatusFailure     = "failure"
	StatusTimeout     = "timeout"
	StatusInterrupted = "interrupted"
)

// telemetryRetryBaseDelay is the base delay for retry backoff.
// Exposed as a variable to allow testing with shorter durations.
var telemetryRetryBaseDelay = 1 * time.Second

// DetermineCompletionStatus returns the appropriate status based on job result.
func DetermineCompletionStatus(interruptedBy string, exitCode int) string {
	if interruptedBy != "" {
		return StatusInterrupted
	}
	if exitCode == 0 {
		return StatusSuccess
	}
	return StatusFailure
}

// TelemetrySQSAPI defines SQS operations for telemetry.
type TelemetrySQSAPI interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// JobStatus represents job status for telemetry.
type JobStatus struct {
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

// SQSTelemetry handles sending job status to SQS.
type SQSTelemetry struct {
	sqsClient TelemetrySQSAPI
	queueURL  string
	logger    Logger
}

// Verify SQSTelemetry implements TelemetryClient.
var _ TelemetryClient = (*SQSTelemetry)(nil)

// NewSQSTelemetry creates a new SQS telemetry client.
func NewSQSTelemetry(cfg aws.Config, queueURL string, logger Logger) *SQSTelemetry {
	return &SQSTelemetry{
		sqsClient: sqs.NewFromConfig(cfg),
		queueURL:  queueURL,
		logger:    logger,
	}
}

// Telemetry is an alias for SQSTelemetry for backward compatibility.
// Deprecated: Use SQSTelemetry instead.
type Telemetry = SQSTelemetry

// SendJobStarted sends a job started notification.
func (t *Telemetry) SendJobStarted(ctx context.Context, status JobStatus) error {
	status.Status = StatusStarted
	return t.sendMessage(ctx, status)
}

// SendJobCompleted sends a job completion notification.
func (t *Telemetry) SendJobCompleted(ctx context.Context, status JobStatus) error {
	status.Status = DetermineCompletionStatus(status.InterruptedBy, status.ExitCode)
	return t.sendMessage(ctx, status)
}

// SendJobTimeout sends a job timeout notification.
func (t *Telemetry) SendJobTimeout(ctx context.Context, status JobStatus) error {
	status.Status = StatusTimeout
	return t.sendMessage(ctx, status)
}

// SendJobFailure sends a job failure notification.
func (t *Telemetry) SendJobFailure(ctx context.Context, status JobStatus) error {
	status.Status = StatusFailure
	return t.sendMessage(ctx, status)
}

// sendMessage sends a job status message to SQS.
func (t *Telemetry) sendMessage(ctx context.Context, status JobStatus) error {
	body, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("failed to marshal status: %w", err)
	}

	// Retry up to 3 times
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * telemetryRetryBaseDelay
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		_, err = t.sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:    aws.String(t.queueURL),
			MessageBody: aws.String(string(body)),
		})
		if err != nil {
			lastErr = err
			t.logger.Printf("Failed to send telemetry (attempt %d/3): %v", attempt+1, err)
			continue
		}

		t.logger.Printf("Sent telemetry: status=%s, job_id=%s", status.Status, status.JobID)
		return nil
	}

	return fmt.Errorf("failed to send telemetry after 3 attempts: %w", lastErr)
}

// SendWithTimeout sends a message with a timeout.
func (t *Telemetry) SendWithTimeout(status JobStatus, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return t.sendMessage(ctx, status)
}
