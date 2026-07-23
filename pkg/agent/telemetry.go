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

// DetermineCompletionStatus maps a run.sh outcome to the agent's operational
// termination status. The status describes whether OUR runner did its job, not
// whether the client's workflow passed.
//
// The ephemeral actions-runner's run.sh (Runner.Listener) exits 0 whenever it
// operated correctly, because we do not set ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED;
// its exit code never carries the workflow step result. So:
//   - interruptedBy set: the runner was preempted (spot/infra) -> StatusInterrupted.
//   - exit 0: the runner ran the job to completion and exited cleanly -> StatusSuccess.
//     This holds even when the workflow's steps failed; that is the client's outcome,
//     not ours, and the termination handler maps StatusSuccess to the "served" result.
//   - non-zero exit: the runner/listener itself failed operationally -> StatusFailure,
//     which the termination handler maps to the operational "error" result.
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
	// ToolCacheMisses lists Actions tool-cache entries downloaded on-demand during
	// the job (not pre-baked), as "<Tool>/<version>/<platform>" keys. The orchestrator
	// turns these into a metric to tune the baked tool set. Best-effort, may be empty.
	ToolCacheMisses []string `json:"tool_cache_misses,omitempty"`
	// CacheInterception is the on-host cache interceptor's outcome for this job:
	// "engaged", "failed" (fell open to GitHub's cache), or "disabled" (no cache
	// configured). Lets the orchestrator surface silent fail-open interception.
	CacheInterception string `json:"cache_interception,omitempty"`
	// BuildCacheInterception is the transparent buildx layer-cache shim's outcome
	// for this job: "engaged", "skipped", "failed", or "disabled". Absent
	// (omitempty) from a pre-rollout agent, which the orchestrator treats as no
	// measurement.
	BuildCacheInterception string `json:"build_cache_interception,omitempty"`
	// CacheBytesWritten is the blob bytes stored to the S3 cache through the v2
	// interceptor this job (the blob PUT bypasses the orchestrator, so it can't be
	// counted server-side). Zero when interception didn't engage.
	CacheBytesWritten int64 `json:"cache_bytes_written,omitempty"`
	// Bootstrap*Seconds decompose the agent-side startup into segments the
	// orchestrator turns into the agent_bootstrap_seconds metric. All are
	// omitempty so a zero (unmeasured, or a pre-rollout agent) is absent on the
	// wire; the orchestrator treats absence as "no measurement".
	BootstrapBootSeconds     float64 `json:"bootstrap_boot_seconds,omitempty"`
	BootstrapConfigSeconds   float64 `json:"bootstrap_config_seconds,omitempty"`
	BootstrapRunnerSeconds   float64 `json:"bootstrap_runner_seconds,omitempty"`
	BootstrapRegisterSeconds float64 `json:"bootstrap_register_seconds,omitempty"`
	BootstrapTotalSeconds    float64 `json:"bootstrap_total_seconds,omitempty"`
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

// SendJobStarted sends a job started notification.
func (t *SQSTelemetry) SendJobStarted(ctx context.Context, status JobStatus) error {
	status.Status = StatusStarted
	return t.sendMessage(ctx, status)
}

// SendJobCompleted sends a job completion notification.
func (t *SQSTelemetry) SendJobCompleted(ctx context.Context, status JobStatus) error {
	status.Status = DetermineCompletionStatus(status.InterruptedBy, status.ExitCode)
	return t.sendMessage(ctx, status)
}

// SendJobTimeout sends a job timeout notification.
func (t *SQSTelemetry) SendJobTimeout(ctx context.Context, status JobStatus) error {
	status.Status = StatusTimeout
	return t.sendMessage(ctx, status)
}

// SendJobFailure sends a job failure notification.
func (t *SQSTelemetry) SendJobFailure(ctx context.Context, status JobStatus) error {
	status.Status = StatusFailure
	return t.sendMessage(ctx, status)
}

// sendMessage sends a job status message to SQS.
func (t *SQSTelemetry) sendMessage(ctx context.Context, status JobStatus) error {
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
func (t *SQSTelemetry) SendWithTimeout(status JobStatus, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return t.sendMessage(ctx, status)
}
