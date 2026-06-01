// Package events handles EventBridge events from SQS.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var eventsLog = logging.WithComponent(logging.LogTypeEvents, "handler")

// eventsTickInterval is the interval for the events handler loop.
// Exposed as a variable to allow testing with shorter durations.
var eventsTickInterval = 1 * time.Second

// QueueAPI provides queue operations for event processing.
type QueueAPI interface {
	ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error)
	DeleteMessage(ctx context.Context, handle string) error
	SendMessage(ctx context.Context, job *queue.JobMessage) error
}

// JobRequeuer re-enqueues jobs onto the main job queue after an interruption.
type JobRequeuer interface {
	SendMessage(ctx context.Context, job *queue.JobMessage) error
}

// DBAPI provides database operations for event processing.
type DBAPI interface {
	MarkInstanceTerminating(ctx context.Context, instanceID string) error
	GetJobByInstance(ctx context.Context, instanceID string) (*JobInfo, error)
}

// JobInfo represents job details stored in DynamoDB.
type JobInfo struct {
	JobID        int64
	RunID        int64
	Repo         string
	InstanceType string
	Pool         string
	Spot         bool
	RetryCount   int // Number of times this job has been retried
}

// MetricsAPI provides CloudWatch metrics publishing.
type MetricsAPI interface {
	PublishSpotInterruption(ctx context.Context, family string) error
	PublishMessageDeletionFailure(ctx context.Context, queue string) error
	PublishCircuitBreakerTrip(ctx context.Context, instanceType string) error
	PublishCircuitBreakerOpen(ctx context.Context, instanceType string, open bool) error
	PublishJobRequeued(ctx context.Context, reason string) error
	PublishQueueReceive(ctx context.Context, queue, result string) error
	PublishMessageProcessingSeconds(ctx context.Context, queue, result string, seconds float64) error
}

// queueEvents is the queue label for the events worker's message-processing
// latency metric.
const queueEvents = "events"

// instanceFamily returns the instance family (the segment before the dot, e.g.
// "c7g" for "c7g.xlarge"). It returns "" for an empty or malformed type so the
// family label is omitted rather than carrying a noisy value.
func instanceFamily(instanceType string) string {
	if i := strings.IndexByte(instanceType, '.'); i > 0 {
		return instanceType[:i]
	}
	return ""
}

// CircuitBreakerAPI provides circuit breaker operations.
type CircuitBreakerAPI interface {
	RecordInterruption(ctx context.Context, instanceType string) error
	IsOpen(ctx context.Context, instanceType string) (bool, error)
}

// Handler processes EventBridge events from SQS queue.
type Handler struct {
	queueClient    QueueAPI
	jobQueue       JobRequeuer
	dbClient       DBAPI
	metrics        MetricsAPI
	config         *config.Config
	circuitBreaker CircuitBreakerAPI
}

// NewHandler creates a new event handler.
func NewHandler(q QueueAPI, db DBAPI, m MetricsAPI, cfg *config.Config) *Handler {
	return &Handler{
		queueClient: q,
		dbClient:    db,
		metrics:     m,
		config:      cfg,
	}
}

// SetJobQueue sets the main job queue used to re-enqueue jobs after an
// interruption. It must be set before the handler processes events.
func (h *Handler) SetJobQueue(q JobRequeuer) {
	h.jobQueue = q
}

// SetCircuitBreaker sets the circuit breaker for recording interruptions.
func (h *Handler) SetCircuitBreaker(cb CircuitBreakerAPI) {
	h.circuitBreaker = cb
}

// EventBridgeEvent represents an EventBridge event from SQS.
type EventBridgeEvent struct {
	Version    string          `json:"version"`
	ID         string          `json:"id"`
	DetailType string          `json:"detail-type"`
	Source     string          `json:"source"`
	Account    string          `json:"account"`
	Time       time.Time       `json:"time"`
	Region     string          `json:"region"`
	Resources  []string        `json:"resources"`
	Detail     json.RawMessage `json:"detail"`
}

// SpotInterruptionDetail contains EC2 spot interruption details.
type SpotInterruptionDetail struct {
	InstanceID     string `json:"instance-id"`
	InstanceAction string `json:"instance-action"`
}

// StateChangeDetail contains EC2 instance state change details.
type StateChangeDetail struct {
	InstanceID string `json:"instance-id"`
	State      string `json:"state"`
}

// Run starts the event handler loop.
func (h *Handler) Run(ctx context.Context) {
	eventsLog.Info(ctx, "event handler starting")
	ticker := time.NewTicker(eventsTickInterval)
	defer ticker.Stop()

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			timeout := 25 * time.Second
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline)
				if remaining < timeout {
					timeout = remaining
				}
			}
			recvCtx, cancel := context.WithTimeout(ctx, timeout)
			messages, err := h.queueClient.ReceiveMessages(recvCtx, 10, 20)
			cancel()
			if err != nil {
				if h.metrics != nil {
					_ = h.metrics.PublishQueueReceive(ctx, "events", "error")
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					eventsLog.Warn(ctx, "receive messages failed", slog.String("error", err.Error()))
				} else {
					eventsLog.Error(ctx, "receive messages failed", slog.String("error", err.Error()))
				}
				continue
			}

			if len(messages) == 0 {
				if h.metrics != nil {
					_ = h.metrics.PublishQueueReceive(ctx, "events", "empty")
				}
				jitter := time.Duration(160+rand.Int63n(80)) * time.Millisecond
				select {
				case <-time.After(jitter):
				case <-ctx.Done():
					return
				}
				continue
			}

			if h.metrics != nil {
				_ = h.metrics.PublishQueueReceive(ctx, "events", "messages")
			}

			var wg sync.WaitGroup
			for _, msg := range messages {
				msg := msg
				wg.Add(1)
				go func() {
					// Default to "error"; set to "ok" only on a clean return so a
					// recovered panic reports "error".
					result := "error"
					var start time.Time
					defer func() {
						// start is zero only if the slot was never acquired (ctx
						// done); skip the metric in that case.
						if h.metrics != nil && !start.IsZero() {
							_ = h.metrics.PublishMessageProcessingSeconds(ctx, queueEvents, result, time.Since(start).Seconds())
						}
						if r := recover(); r != nil {
							eventsLog.Error(ctx, "panic in event processor", slog.Any("panic", r))
						}
					}()
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					// Start timing after acquiring the concurrency slot so the
					// metric reflects processing time, not slot-wait time.
					start = time.Now()
					processCtx, processCancel := context.WithTimeout(ctx, config.MessageProcessTimeout)
					defer processCancel()
					h.processEvent(processCtx, msg)
					result = "ok"
				}()
			}

			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (h *Handler) processEvent(ctx context.Context, msg queue.Message) {
	if msg.Handle == "" {
		eventsLog.Warn(ctx, "message with empty handle")
		return
	}

	defer func() {
		if err := h.queueClient.DeleteMessage(ctx, msg.Handle); err != nil {
			eventsLog.Error(ctx, "message delete failed", slog.String("error", err.Error()))
			metricCtx, metricCancel := context.WithTimeout(ctx, config.ShortTimeout)
			defer metricCancel()
			if err := h.metrics.PublishMessageDeletionFailure(metricCtx, "events"); err != nil {
				eventsLog.Error(ctx, "message deletion metric failed", slog.String("error", err.Error()))
			}
		}
	}()

	if msg.Body == "" {
		eventsLog.Warn(ctx, "message with empty body")
		return
	}

	var event EventBridgeEvent
	if err := json.Unmarshal([]byte(msg.Body), &event); err != nil {
		eventsLog.Error(ctx, "event unmarshal failed", slog.String("error", err.Error()))
		return
	}

	var processErr error
	switch event.DetailType {
	case "EC2 Spot Instance Interruption Warning":
		processErr = h.handleSpotInterruption(ctx, event.Detail)
	case "EC2 Instance State-change Notification":
		processErr = h.handleStateChange(ctx, event.Detail)
	default:
		eventsLog.Warn(ctx, "unknown event type", slog.String("detail_type", event.DetailType))
		return
	}

	if processErr != nil {
		eventsLog.Error(ctx, "event processing failed", slog.String("error", processErr.Error()))
	}
}

func (h *Handler) handleSpotInterruption(ctx context.Context, detailRaw json.RawMessage) error {
	var detail SpotInterruptionDetail
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		return fmt.Errorf("failed to unmarshal spot interruption detail: %w", err)
	}

	ctx, span := tracing.Tracer().Start(ctx, "events.spot_interruption",
		trace.WithAttributes(
			attribute.String("instance.id", detail.InstanceID),
		))
	defer span.End()

	ctx = logging.ContextWith(ctx, slog.String(logging.KeyInstanceID, detail.InstanceID))

	// family is resolved once the interrupted job's instance type is known
	// (below) and read by the deferred metric so the counter carries the family
	// label. It stays empty when no job is found, which the backend omits.
	var family string

	// Publish the interruption count best-effort and after the critical work
	// below: a throttled CloudWatch metric must never abort marking the instance
	// terminating or re-queuing the in-flight job (the SQS message is deleted
	// unconditionally, so an aborted handler loses the job). Deferred so it runs
	// on every exit path without gating.
	defer func() {
		// Detach from ctx cancellation (keeping trace values) so the metric is
		// still attempted when the request ctx is already cancelled — e.g. the
		// spot reclaim itself tore down the handler's context.
		mctx, mcancel := context.WithTimeout(context.WithoutCancel(ctx), config.ShortTimeout)
		defer mcancel()
		if err := h.metrics.PublishSpotInterruption(mctx, family); err != nil {
			eventsLog.Warn(mctx, "spot interruption metric publish failed",
				slog.String("error", err.Error()))
		}
	}()

	eventsLog.Info(ctx, "spot interruption received")

	if err := h.dbClient.MarkInstanceTerminating(ctx, detail.InstanceID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to mark instance as terminating: %w", err)
	}

	job, err := h.dbClient.GetJobByInstance(ctx, detail.InstanceID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to get job for instance: %w", err)
	}
	if job != nil {
		span.SetAttributes(
			attribute.Int64("job.id", job.JobID),
			attribute.String("instance.type", job.InstanceType),
		)
	}
	if job == nil {
		eventsLog.Warn(ctx, "no job for spot interruption requeue")
		return nil
	}

	if job.JobID == 0 || job.RunID == 0 {
		return nil
	}

	ctx = logging.ContextWithJob(ctx, job.JobID, job.RunID, job.Repo)
	family = instanceFamily(job.InstanceType)

	// Record interruption in circuit breaker
	if h.circuitBreaker != nil && job.InstanceType != "" {
		if err := h.circuitBreaker.RecordInterruption(ctx, job.InstanceType); err != nil {
			eventsLog.Warn(ctx, "circuit breaker record failed",
				slog.String("instance_type", job.InstanceType),
				slog.String("error", err.Error()))
		} else {
			open, err := h.circuitBreaker.IsOpen(ctx, job.InstanceType)
			if err != nil {
				eventsLog.Warn(ctx, "circuit breaker state check failed",
					slog.String("instance_type", job.InstanceType),
					slog.String("error", err.Error()))
			} else {
				metricCtx, metricCancel := context.WithTimeout(ctx, config.ShortTimeout)
				defer metricCancel()
				if err := h.metrics.PublishCircuitBreakerOpen(metricCtx, job.InstanceType, open); err != nil {
					eventsLog.Warn(metricCtx, "circuit breaker open metric failed",
						slog.String("instance_type", job.InstanceType),
						slog.String("error", err.Error()))
				}
				if open {
					if err := h.metrics.PublishCircuitBreakerTrip(metricCtx, job.InstanceType); err != nil {
						eventsLog.Warn(metricCtx, "circuit breaker trip metric failed",
							slog.String("instance_type", job.InstanceType),
							slog.String("error", err.Error()))
					}
				}
			}
		}
	}

	// Re-queue with ForceOnDemand to ensure job completes on retry
	// Increment RetryCount to track how many times this job has been retried
	// Note: Some fields (OriginalLabel, Region, etc.) are not stored in JobInfo.
	// Re-queued jobs use basic config. Full field storage is a future enhancement.
	requeueMsg := &queue.JobMessage{
		JobID:         job.JobID,
		RunID:         job.RunID,
		Repo:          job.Repo,
		InstanceType:  job.InstanceType,
		Pool:          job.Pool,
		Spot:          false,              // ForceOnDemand uses on-demand instances
		RetryCount:    job.RetryCount + 1, // Increment retry count
		ForceOnDemand: true,               // Force on-demand for retries after spot interruption
	}
	if h.jobQueue == nil {
		return fmt.Errorf("job queue not configured; cannot re-queue job %d", job.JobID)
	}
	if err := h.jobQueue.SendMessage(ctx, requeueMsg); err != nil {
		return fmt.Errorf("failed to re-queue job %d: %w", job.JobID, err)
	}

	if err := h.metrics.PublishJobRequeued(ctx, "spot_interruption"); err != nil {
		eventsLog.Warn(ctx, "job requeued metric failed", slog.String("error", err.Error()))
	}

	eventsLog.Info(ctx, "job requeued after spot interruption",
		slog.Int("retry_count", requeueMsg.RetryCount))
	return nil
}

func (h *Handler) handleStateChange(ctx context.Context, detailRaw json.RawMessage) error {
	var detail StateChangeDetail
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		return fmt.Errorf("failed to unmarshal state change detail: %w", err)
	}

	ctx = logging.ContextWith(ctx, slog.String(logging.KeyInstanceID, detail.InstanceID))

	eventsLog.Info(ctx, "instance state change",
		slog.String("state", detail.State))

	// The instances gauge is published from the pool reconcile loop using the
	// authoritative per-pool running/stopped counts (see pkg/pools), not from
	// per-event deltas here, which would drift and lose state on restart.
	switch detail.State {
	case "running", "stopped", "terminated", "pending", "stopping", "shutting-down":
		// Known states, no action needed.
	default:
		eventsLog.Warn(ctx, "unknown instance state",
			slog.String("state", detail.State))
	}
	return nil
}
