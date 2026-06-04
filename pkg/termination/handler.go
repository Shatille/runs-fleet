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
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
	"github.com/Shavakan/runs-fleet/pkg/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var termLog = logging.WithComponent(logging.LogTypeTermination, "handler")

// QueueAPI provides queue operations for termination event processing.
type QueueAPI interface {
	ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error)
	DeleteMessage(ctx context.Context, handle string) error
}

// DBAPI provides database operations for termination processing.
type DBAPI interface {
	MarkJobComplete(ctx context.Context, jobID int64, status string, exitCode, duration int) (*events.JobInfo, error)
	UpdateJobMetrics(ctx context.Context, jobID int64, startedAt, completedAt time.Time) error
}

// MetricsAPI provides metrics publishing for job completion.
type MetricsAPI interface {
	PublishJobCompleted(ctx context.Context, pool, result, repo string) error
	PublishJobExecutionSeconds(ctx context.Context, pool, result string, seconds float64) error
	PublishMessageProcessingSeconds(ctx context.Context, queue, result string, seconds float64) error
}

// queueTermination is the queue label for the termination worker's
// message-processing latency metric.
const queueTermination = "termination"

// Job-result values for the jobs_completed metric. These describe OUR runner's
// operational lifecycle, never the client workflow's pass/fail conclusion.
//
//   - served: the runner ran the job to completion and the runner process exited
//     cleanly. This is reported even when the client's workflow steps failed: the
//     ephemeral actions-runner exits 0 whenever it operated correctly, because we
//     do not set ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED, so a non-zero exit
//     never reflects a failed workflow step (see jobResult).
//   - interrupted: the runner was preempted by spot reclamation or other infra
//     interruption before the job finished.
//   - timeout: the runner hit the max-runtime ceiling and was terminated.
//   - error: the runner/agent failed operationally (crash, panic, could not start
//     or register, or the listener exited non-zero).
//
// Success/failure SLA is assignment-based and lives elsewhere: jobs_assigned
// (we delivered a runner) versus scheduling_failure (we failed to). jobs_completed
// is operational telemetry only and must never be derived from the workflow
// conclusion.
const (
	resultServed      = "served"
	resultInterrupted = "interrupted"
	resultTimeout     = "timeout"
	resultError       = "error"
)

// jobResult maps an agent termination status to OUR operational job-result enum
// ({served, interrupted, error, timeout}). It deliberately does NOT encode the
// client workflow's pass/fail outcome.
//
// The agent derives its termination status from the ephemeral actions-runner's
// run.sh exit code (pkg/agent: exit 0 -> "success", non-zero -> "failure"). Because
// we do not set ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED, the runner's listener
// returns success (exit 0) whenever it operated correctly, regardless of whether
// the workflow's steps passed or failed. A non-zero exit therefore means the runner
// itself failed operationally — which is genuinely our "error" — while a clean exit
// means we served the job to completion ("served"), even on a red workflow.
func jobResult(status string) string {
	switch status {
	case string(db.JobStatusSuccess):
		return resultServed
	case "timeout":
		return resultTimeout
	case "interrupted":
		return resultInterrupted
	default:
		return resultError
	}
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
	termLog.Info(ctx, "termination handler starting")

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	ticker := time.NewTicker(handlerTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			termLog.Info(ctx, "termination handler shutdown complete")
			return

		case <-ticker.C:
			messages, err := h.queueClient.ReceiveMessages(ctx, 10, 20)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					termLog.Warn(ctx, "receive messages failed", slog.String("error", err.Error()))
				} else {
					termLog.Error(ctx, "receive messages failed", slog.String("error", err.Error()))
				}
				continue
			}

			for _, msg := range messages {
				wg.Add(1)
				sem <- struct{}{}

				go func(m queue.Message) {
					defer wg.Done()
					defer func() { <-sem }()

					// Detach from the handler's parent (SIGTERM) context while keeping
					// its log/trace values, so an in-flight termination message runs
					// to completion on shutdown instead of aborting with "context
					// canceled". The receive path above still stops on ctx.Done(), so
					// no new work is accepted, and the deferred wg.Wait() drains these
					// processors before Run returns. The timeout still bounds a hung
					// processor.
					msgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
					defer cancel()

					start := time.Now()
					err := h.processMessage(msgCtx, m)
					if h.metrics != nil {
						result := "ok"
						if err != nil {
							result = "error"
						}
						_ = h.metrics.PublishMessageProcessingSeconds(msgCtx, queueTermination, result, time.Since(start).Seconds())
					}
					if err != nil {
						termLog.Error(msgCtx, "message processing failed", slog.String("error", err.Error()))
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

	ctx = logging.ContextWith(ctx,
		slog.String(logging.KeyInstanceID, termMsg.InstanceID),
		slog.String(logging.KeyJobID, termMsg.JobID))

	// "started" events carry no completion to process; they only announce the
	// runner came up. Log them at Debug so they don't masquerade as the
	// "processing termination" line, which processTermination emits for
	// completion events once the DB record (run_id/repo) is in hand.
	if termMsg.Status == "started" {
		termLog.Debug(ctx, "runner started")
	}

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

	ctx, span := tracing.Tracer().Start(ctx, "termination.process",
		trace.WithAttributes(
			attribute.String("job.id", msg.JobID),
			attribute.String("job.status", msg.Status),
			attribute.Int("job.duration", msg.DurationSeconds),
		))
	defer span.End()

	// Parse job ID from string to int64 (DynamoDB uses Number type)
	jobID, err := strconv.ParseInt(msg.JobID, 10, 64)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to parse job ID %q: %w", msg.JobID, err)
	}

	// Update DynamoDB job record (keyed by job_id). The returned record is the
	// source of run_id/repo: the termination Message carries only instance_id and
	// job_id, so we enrich the context from the DB so every log below (and the
	// "processing termination" line) carries the full job identity.
	rec, err := h.dbClient.MarkJobComplete(ctx, jobID, msg.Status, msg.ExitCode, msg.DurationSeconds)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to mark job complete: %w", err)
	}
	if rec != nil {
		// job_id is already stashed by processMessage; pass 0 so ContextWithJob
		// stashes only run_id/repo and job_id is not emitted twice (slog does not
		// de-duplicate stashed attrs). Zero-valued run_id/repo are omitted.
		ctx = logging.ContextWithJob(ctx, 0, rec.RunID, rec.Repo)
	}

	termLog.Info(ctx, "processing termination",
		slog.String("status", msg.Status),
		slog.Int("exit_code", msg.ExitCode))

	// Update job metrics (timestamps)
	if !msg.StartedAt.IsZero() && !msg.CompletedAt.IsZero() {
		if err := h.dbClient.UpdateJobMetrics(ctx, jobID, msg.StartedAt, msg.CompletedAt); err != nil {
			termLog.Warn(ctx, "job metrics update failed",
				slog.String("error", err.Error()))
		}
	}

	// Publish job completion metrics. The job record (from MarkJobComplete above)
	// supplies pool and repo so the completion counter carries those labels.
	var pool, repo string
	if rec != nil {
		pool, repo = rec.Pool, rec.Repo
	}
	result := jobResult(msg.Status)
	if h.metrics != nil {
		if err := h.metrics.PublishJobCompleted(ctx, pool, result, repo); err != nil {
			termLog.Warn(ctx, "job completed metric publish failed", slog.String("error", err.Error()))
		}
	}
	// TODO(metrics): emit job execution latency via PublishJobExecutionSeconds(ctx,
	// pool, result, msg.DurationSeconds). Data is in hand (msg.DurationSeconds);
	// out of scope for this latency-histogram pass, wire in a follow-up.

	// Clean up runner config from secrets store
	if err := h.deleteRunnerConfig(ctx, msg.InstanceID); err != nil {
		termLog.Warn(ctx, "runner config delete failed",
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

	termLog.Info(ctx, "runner config deleted")
	return nil
}
