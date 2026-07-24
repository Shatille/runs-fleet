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
	"github.com/Shavakan/runs-fleet/pkg/fleet"
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
	MarkJobStarted(ctx context.Context, jobID int64, startedAt time.Time) (*events.JobInfo, error)
	UpdateJobMetrics(ctx context.Context, jobID int64, startedAt, completedAt time.Time) error
	GetJobByInstance(ctx context.Context, instanceID string) (*events.JobInfo, error)
	MarkJobRequeuedByJobID(ctx context.Context, jobID int64) (bool, error)
}

// JobQueueAPI enqueues job re-queue messages (used to recover a job whose
// instance failed to bootstrap).
type JobQueueAPI interface {
	SendMessage(ctx context.Context, job *queue.JobMessage) error
}

// MetricsAPI provides metrics publishing for job completion.
type MetricsAPI interface {
	PublishJobCompleted(ctx context.Context, pool, result, repo string) error
	PublishRunnerConfirmed(ctx context.Context, pool string) error
	PublishInstanceProvisionSeconds(ctx context.Context, source, family string, seconds float64) error
	PublishAgentBootstrapSeconds(ctx context.Context, pool, phase string, seconds float64) error
	PublishJobExecutionSeconds(ctx context.Context, pool, result string, seconds float64) error
	PublishMessageProcessingSeconds(ctx context.Context, queue, result string, seconds float64) error
	PublishJobRequeued(ctx context.Context, reason string) error
	PublishSchedulingFailure(ctx context.Context, taskType string) error
	PublishRunnerExecutionSeconds(ctx context.Context, arch string, vcpu int, spot bool, result string, seconds float64) error
	PublishRunnerToolCacheMiss(ctx context.Context, tool, version, arch string) error
	PublishRunnerCacheInterception(ctx context.Context, status string) error
	PublishRunnerBuildCacheInterception(ctx context.Context, status string) error
	PublishCacheBytesStored(ctx context.Context, bytes int64) error
}

// maxBootstrapRequeues bounds how many times a job whose instance fails to boot
// is re-queued before we give up and let it fail. Mirrors the webhook's
// maxJobRetries and housekeeping's MaxRequeueRetries (both 2): a systemic
// bootstrap failure (e.g. a bad AMI/agent rollout) must fail fast and loudly
// rather than loop forever burning instances.
const maxBootstrapRequeues = 2

// reasonBootstrapFailed labels the requeue / scheduling-failure metrics emitted
// when an instance fails to start the agent on boot, so operators can alert on
// a spike (a bad rollout) distinctly from spot interruptions.
const reasonBootstrapFailed = "bootstrap_failed"

// queueTermination is the queue label for the termination worker's
// message-processing latency metric.
const queueTermination = "termination"

// Source labels for the instance-provision-latency metric, matching the
// worker's enum (internal/worker). hot_pool is a warm_pool subset: the job
// landed on a RUNNING spare and paid no boot, so its latency should separate
// cleanly from a stopped-instance resume (warm_pool) in the metric.
const (
	sourceWarmPool  = "warm_pool"
	sourceColdStart = "cold_start"
	sourceHotPool   = "hot_pool"
)

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
	// ToolCacheMisses lists Actions tool-cache entries the job downloaded on-demand
	// (not pre-baked), as "<Tool>/<version>/<platform>" keys, for the tool-cache-miss metric.
	ToolCacheMisses []string `json:"tool_cache_misses,omitempty"`
	// CacheInterception is the on-host cache interceptor's outcome (engaged|failed|disabled).
	CacheInterception string `json:"cache_interception,omitempty"`
	// BuildCacheInterception is the buildx layer-cache shim's outcome
	// (engaged|skipped|failed|disabled). Absent from a pre-rollout agent.
	BuildCacheInterception string `json:"build_cache_interception,omitempty"`
	// CacheBytesWritten is v2 cache blob bytes stored to S3 through the interceptor
	// this job (folded into the CacheBytesStored counter — the blob PUT bypasses the
	// orchestrator, so it can't be counted server-side).
	CacheBytesWritten int64 `json:"cache_bytes_written,omitempty"`
	// Bootstrap*Seconds decompose the agent-side startup; the handler publishes
	// each positive segment as agent_bootstrap_seconds. Absent (zero) from an old
	// agent, in which case no bootstrap metric is emitted.
	BootstrapBootSeconds     float64 `json:"bootstrap_boot_seconds,omitempty"`
	BootstrapConfigSeconds   float64 `json:"bootstrap_config_seconds,omitempty"`
	BootstrapRunnerSeconds   float64 `json:"bootstrap_runner_seconds,omitempty"`
	BootstrapRegisterSeconds float64 `json:"bootstrap_register_seconds,omitempty"`
	BootstrapTotalSeconds    float64 `json:"bootstrap_total_seconds,omitempty"`
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
	jobQueue     JobQueueAPI
	config       *config.Config
}

// NewHandler creates a new termination handler. jobQueue may be nil (re-queue
// on bootstrap failure is then skipped; the unconfirmed-runner watchdog still
// recovers the job).
func NewHandler(q QueueAPI, db DBAPI, m MetricsAPI, secretsStore secrets.Store, jobQueue JobQueueAPI, cfg *config.Config) *Handler {
	return &Handler{
		queueClient:  q,
		dbClient:     db,
		metrics:      m,
		secretsStore: secretsStore,
		jobQueue:     jobQueue,
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
		// Non-retryable: an empty body never becomes valid, so ack it instead of
		// returning an error (the termination queue has no DLQ; a returned error
		// would redeliver for the full retention window).
		termLog.Warn(ctx, "dropping termination message with empty body")
		return h.ackMessage(ctx, msg)
	}

	var termMsg Message
	if err := json.Unmarshal([]byte(msg.Body), &termMsg); err != nil {
		termLog.Warn(ctx, "dropping unparseable termination message", slog.String("error", err.Error()))
		return h.ackMessage(ctx, msg)
	}

	ctx = logging.ContextWith(ctx,
		slog.String(logging.KeyInstanceID, termMsg.InstanceID),
		slog.String(logging.KeyJobID, termMsg.JobID))

	// "bootstrap_failed" is emitted by the boot shim (scripts/cloud-init-boot.sh)
	// when the agent fails to start before the Go agent runs, so it carries only
	// instance_id (no job_id). Handle it as an instance-scoped signal: recover the
	// job by instance and ack. It must be handled before the job_id-requiring
	// validation below.
	if termMsg.Status == "bootstrap_failed" {
		if termMsg.InstanceID == "" {
			termLog.Warn(ctx, "dropping bootstrap_failed message with no instance_id")
			return h.ackMessage(ctx, msg)
		}
		if err := h.processBootstrapFailure(ctx, &termMsg); err != nil {
			return err // transient (DB/queue) — let SQS redeliver
		}
		return h.ackMessage(ctx, msg)
	}

	// A "started" event announces the runner registered and began executing; it
	// carries no completion, so it confirms the job (launched -> running) rather
	// than going through processTermination. A DB error during confirmation is
	// retried via SQS redelivery so the watchdog does not later mistake a healthy
	// job for a never-confirmed one; a malformed message is non-retryable and acked.
	if termMsg.Status == "started" {
		if err := h.validateMessage(&termMsg); err != nil {
			termLog.Warn(ctx, "dropping invalid started message",
				slog.String("error", err.Error()))
			return h.ackMessage(ctx, msg)
		}
		if err := h.confirmRunnerStarted(ctx, &termMsg); err != nil {
			return err
		}
		return h.ackMessage(ctx, msg)
	}

	if err := h.validateMessage(&termMsg); err != nil {
		// Non-retryable: a message missing required fields never becomes valid.
		termLog.Warn(ctx, "dropping invalid termination message",
			slog.String("error", err.Error()), slog.String("status", termMsg.Status))
		return h.ackMessage(ctx, msg)
	}

	if err := h.processTermination(ctx, &termMsg); err != nil {
		return fmt.Errorf("failed to process termination: %w", err)
	}

	return h.ackMessage(ctx, msg)
}

// ackMessage deletes a message from the queue (no-op when there is no handle,
// e.g. in tests).
func (h *Handler) ackMessage(ctx context.Context, msg queue.Message) error {
	if msg.Handle == "" {
		return nil
	}
	if err := h.queueClient.DeleteMessage(ctx, msg.Handle); err != nil {
		return fmt.Errorf("failed to delete message: %w", err)
	}
	return nil
}

// processBootstrapFailure recovers a job whose instance failed to start the
// agent on boot. The boot shim has only the instance id, so we resolve the job
// by instance, re-queue it onto a fresh on-demand instance (idempotent via
// MarkJobRequeuedByJobID, so a redelivery or the unconfirmed-runner watchdog
// cannot double-enqueue), and delete the dead instance's runner config. Returns
// an error only for transient DB/queue failures (so the message is retried);
// nil means the caller should ack.
func (h *Handler) processBootstrapFailure(ctx context.Context, msg *Message) error {
	ctx, span := tracing.Tracer().Start(ctx, "termination.bootstrap_failed",
		trace.WithAttributes(attribute.String("instance.id", msg.InstanceID)))
	defer span.End()

	termLog.Warn(ctx, "agent bootstrap failed on boot", slog.String("error", msg.Error))

	job, err := h.dbClient.GetJobByInstance(ctx, msg.InstanceID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to get job for instance %s: %w", msg.InstanceID, err)
	}

	if job != nil && job.JobID != 0 && job.RunID != 0 && h.jobQueue != nil {
		ctx = logging.ContextWithJob(ctx, job.JobID, job.RunID, job.Repo)

		if job.RetryCount >= maxBootstrapRequeues {
			// Give up rather than loop forever: a job whose instances keep failing
			// to boot is almost always a systemic problem (bad AMI/agent rollout).
			// Stop re-queuing and emit a scheduling-failure metric so operators can
			// alert; the unconfirmed-runner watchdog marks the launched record
			// terminal once its window elapses.
			termLog.Error(ctx, "exhausted bootstrap re-queue retries; giving up",
				slog.Int("retry_count", job.RetryCount), slog.Int("max", maxBootstrapRequeues))
			if h.metrics != nil {
				if err := h.metrics.PublishSchedulingFailure(ctx, reasonBootstrapFailed); err != nil {
					termLog.Warn(ctx, "scheduling-failure metric failed", slog.String("error", err.Error()))
				}
			}
		} else {
			marked, err := h.dbClient.MarkJobRequeuedByJobID(ctx, job.JobID)
			if err != nil {
				return fmt.Errorf("failed to mark job %d requeued: %w", job.JobID, err)
			}
			if marked {
				requeueMsg := &queue.JobMessage{
					JobID:         job.JobID,
					RunID:         job.RunID,
					Repo:          job.Repo,
					InstanceType:  job.InstanceType,
					Pool:          job.Pool,
					Spot:          false,
					RetryCount:    job.RetryCount + 1,
					ForceOnDemand: true,
				}
				if err := h.jobQueue.SendMessage(ctx, requeueMsg); err != nil {
					return fmt.Errorf("failed to re-queue job %d after bootstrap failure: %w", job.JobID, err)
				}
				termLog.Info(ctx, "job re-queued after bootstrap failure",
					slog.Int("retry_count", requeueMsg.RetryCount))
				if h.metrics != nil {
					if err := h.metrics.PublishJobRequeued(ctx, reasonBootstrapFailed); err != nil {
						termLog.Warn(ctx, "job requeued metric failed", slog.String("error", err.Error()))
					}
				}
			}
		}
	}

	// Best-effort cleanup of the dead instance's runner config; the instance
	// self-terminates via the boot shim, so a failure here is not fatal.
	if err := h.deleteRunnerConfig(ctx, msg.InstanceID); err != nil {
		termLog.Warn(ctx, "runner config cleanup failed after bootstrap failure",
			slog.String("error", err.Error()))
	}
	return nil
}

// confirmRunnerStarted transitions a launched job to running on the agent's
// "started" signal and emits the confirmation metric. A DB error is returned so
// the message is retried (SQS redelivery) before the watchdog's window elapses;
// a malformed job_id is logged and skipped (non-retryable).
func (h *Handler) confirmRunnerStarted(ctx context.Context, msg *Message) error {
	jobID, err := strconv.ParseInt(msg.JobID, 10, 64)
	if err != nil {
		termLog.Warn(ctx, "started message has invalid job_id", slog.String("error", err.Error()))
		return nil
	}

	info, err := h.dbClient.MarkJobStarted(ctx, jobID, msg.StartedAt)
	if err != nil {
		return fmt.Errorf("failed to mark job started: %w", err)
	}
	if info == nil {
		// Already past the launched state (completed, or recovered): no-op.
		return nil
	}

	termLog.Info(ctx, "runner confirmed")
	if h.metrics != nil {
		if err := h.metrics.PublishRunnerConfirmed(ctx, info.Pool); err != nil {
			termLog.Warn(ctx, "runner confirmed metric failed", slog.String("error", err.Error()))
		}
		h.publishProvisionSeconds(ctx, info, msg.StartedAt)
		h.publishBootstrapSeconds(ctx, info.Pool, msg)
	}
	return nil
}

// publishBootstrapSeconds emits the agent-reported bootstrap segments as
// agent_bootstrap_seconds, one per positive segment. The phase enum is fixed
// orchestrator-side (a cardinality guard) rather than taken from the agent; a
// zero segment (an old agent, or an unmeasured phase) is skipped.
func (h *Handler) publishBootstrapSeconds(ctx context.Context, pool string, msg *Message) {
	segments := [...]struct {
		phase   string
		seconds float64
	}{
		{"boot", msg.BootstrapBootSeconds},
		{"config", msg.BootstrapConfigSeconds},
		{"runner_download", msg.BootstrapRunnerSeconds},
		{"registration", msg.BootstrapRegisterSeconds},
		{"total", msg.BootstrapTotalSeconds},
	}
	for _, s := range segments {
		if s.seconds <= 0 {
			continue
		}
		if err := h.metrics.PublishAgentBootstrapSeconds(ctx, pool, s.phase, s.seconds); err != nil {
			termLog.Warn(ctx, "agent bootstrap metric failed",
				slog.String("phase", s.phase),
				slog.String("error", err.Error()))
		}
	}
}

// publishProvisionSeconds emits the assignment-to-runner-registered latency for a
// confirmed runner. warm_pool covers resume+bootstrap; cold_start covers
// post-CreateFleet through registration (CreateFleet itself is FleetCreateSeconds).
// The DB record is the cross-instance rendezvous joining the assignment timestamp
// (stamped orchestrator-side) with the started signal (stamped agent-side). It
// publishes only when both timestamps are non-zero and the span is positive, so a
// cross-clock skew (bounded by chrony) cannot emit a negative or bogus value.
func (h *Handler) publishProvisionSeconds(ctx context.Context, info *events.JobInfo, startedAt time.Time) {
	if info.CreatedAt.IsZero() || startedAt.IsZero() {
		return
	}
	seconds := startedAt.Sub(info.CreatedAt).Seconds()
	if seconds <= 0 {
		return
	}
	source := sourceColdStart
	if info.WarmPoolHit {
		source = sourceWarmPool
	}
	// A hot-pool hit is a warm-pool hit served by a running spare; it takes the
	// more specific label so the <10s no-boot cohort is observable on its own.
	if info.HotPoolHit {
		source = sourceHotPool
	}
	if err := h.metrics.PublishInstanceProvisionSeconds(ctx, source, instanceFamily(info.InstanceType), seconds); err != nil {
		termLog.Warn(ctx, "instance provision metric failed", slog.String("error", err.Error()))
	}
}

// instanceFamily returns the family segment of an instance type ("c7g" for
// "c7g.xlarge"), or "" for an empty or malformed type so the family label is
// omitted rather than carrying a noisy value.
func instanceFamily(instanceType string) string {
	if i := strings.IndexByte(instanceType, '.'); i > 0 {
		return instanceType[:i]
	}
	return ""
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
		// Record execution duration only when the agent reported one (>0); a zero
		// would skew the latency histogram for jobs that never ran (e.g. an early
		// failure before the workflow started).
		if msg.DurationSeconds > 0 {
			if err := h.metrics.PublishJobExecutionSeconds(ctx, pool, result, float64(msg.DurationSeconds)); err != nil {
				termLog.Warn(ctx, "job execution seconds metric publish failed", slog.String("error", err.Error()))
			}
			// Billable runner seconds keyed by arch+vCPU+spot — the axis per-minute
			// competitors bill on; skipped when the instance type isn't in the catalog.
			if rec != nil && rec.InstanceType != "" {
				if spec, ok := fleet.GetInstanceSpec(rec.InstanceType); ok {
					if err := h.metrics.PublishRunnerExecutionSeconds(ctx, spec.Arch, spec.CPU, rec.Spot, result, float64(msg.DurationSeconds)); err != nil {
						termLog.Warn(ctx, "runner execution seconds metric publish failed", slog.String("error", err.Error()))
					}
				}
			}
		}
		// Tool-cache misses: each is a tool version the job downloaded on-demand
		// because it wasn't pre-baked. Emit one metric per (tool, major.minor, arch)
		// so we can see which versions to bake. Best-effort, like the others.
		for _, key := range msg.ToolCacheMisses {
			tool, version, arch, ok := parseToolCacheMiss(key)
			if !ok {
				continue
			}
			if err := h.metrics.PublishRunnerToolCacheMiss(ctx, tool, version, arch); err != nil {
				termLog.Warn(ctx, "tool cache miss metric publish failed", slog.String("error", err.Error()))
			}
		}
		// Cache-interceptor outcome: makes silent fail-open interception visible.
		if msg.CacheInterception != "" {
			if err := h.metrics.PublishRunnerCacheInterception(ctx, msg.CacheInterception); err != nil {
				termLog.Warn(ctx, "cache interception metric publish failed", slog.String("error", err.Error()))
			}
		}
		// Buildx layer-cache shim outcome: the rollout observability gate.
		if msg.BuildCacheInterception != "" {
			if err := h.metrics.PublishRunnerBuildCacheInterception(ctx, msg.BuildCacheInterception); err != nil {
				termLog.Warn(ctx, "build cache interception metric publish failed", slog.String("error", err.Error()))
			}
		}
		// v2 cache write bytes (blob PUT bypasses the orchestrator; reported by the agent).
		if msg.CacheBytesWritten > 0 {
			if err := h.metrics.PublishCacheBytesStored(ctx, msg.CacheBytesWritten); err != nil {
				termLog.Warn(ctx, "cache bytes stored metric publish failed", slog.String("error", err.Error()))
			}
		}
	}

	// Clean up runner config from secrets store
	if err := h.deleteRunnerConfig(ctx, msg.InstanceID); err != nil {
		termLog.Warn(ctx, "runner config delete failed",
			slog.String("error", err.Error()))
	}

	return nil
}

// parseToolCacheMiss splits an agent tool-cache-miss key "<Tool>/<version>/<platform>"
// into its metric dimensions: tool, version normalized to major.minor (bounding metric
// cardinality — "3.10.14"->"3.10", "21.0.4-7"->"21.0"), and arch (the platform segment,
// e.g. "x64"/"arm64"). Returns ok=false for a malformed key so the caller skips it.
//
// Keys come from agent.SnapshotToolCache, which only emits paths with exactly two
// separators, so the tool is a single segment (never contains "/"); Split + len==3 is
// therefore an exact accept/reject (a tampered key with extra slashes is rejected).
func parseToolCacheMiss(key string) (tool, version, arch string, ok bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	tool, arch = parts[0], parts[2]
	// Normalize the platform to a bare arch: most setup-* actions write "x64"/"arm64",
	// but some prefix the OS ("linux-x64"). Strip it so the Arch dimension is consistent.
	for _, p := range []string{"linux-", "darwin-", "windows-"} {
		arch = strings.TrimPrefix(arch, p)
	}
	// major.minor from the version (strip patch and any build suffix).
	v := parts[1]
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	seg := strings.Split(v, ".")
	if len(seg) >= 2 {
		version = seg[0] + "." + seg[1]
	} else {
		version = seg[0]
	}
	return tool, version, arch, true
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
