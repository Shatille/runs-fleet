// Package events handles EventBridge events from SQS.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// QueueAPI provides SQS operations for event processing.
type QueueAPI interface {
	ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]types.Message, error)
	DeleteMessage(ctx context.Context, receiptHandle string) error
	SendMessage(ctx context.Context, job *queue.JobMessage) error
}

// DBAPI provides database operations for event processing.
type DBAPI interface {
	MarkInstanceTerminating(ctx context.Context, instanceID string) error
	GetJobByInstance(ctx context.Context, instanceID string) (*JobInfo, error)
}

// JobInfo represents job details stored in DynamoDB.
type JobInfo struct {
	JobID        string
	RunID        string
	InstanceType string
	Pool         string
	Private      bool
	Spot         bool
	RunnerSpec   string
}

// MetricsAPI provides CloudWatch metrics publishing.
type MetricsAPI interface {
	PublishSpotInterruption(ctx context.Context) error
	PublishFleetSizeIncrement(ctx context.Context) error
	PublishFleetSizeDecrement(ctx context.Context) error
	PublishMessageDeletionFailure(ctx context.Context) error
}

// CircuitBreakerAPI provides circuit breaker operations.
type CircuitBreakerAPI interface {
	RecordInterruption(ctx context.Context, instanceType string) error
}

// Handler processes EventBridge events from SQS queue.
type Handler struct {
	queueClient    QueueAPI
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
	log.Println("Starting event handler loop...")
	ticker := time.NewTicker(1 * time.Second)
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
				log.Printf("failed to receive event messages: %v", err)
				continue
			}

			if len(messages) == 0 {
				jitter := time.Duration(160+rand.Int63n(80)) * time.Millisecond
				select {
				case <-time.After(jitter):
				case <-ctx.Done():
					return
				}
				continue
			}

			var wg sync.WaitGroup
			for _, msg := range messages {
				msg := msg
				wg.Add(1)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("panic in processEvent: %v", r)
						}
					}()
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					processCtx, processCancel := context.WithTimeout(ctx, config.MessageProcessTimeout)
					defer processCancel()
					h.processEvent(processCtx, msg)
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

func (h *Handler) processEvent(ctx context.Context, msg types.Message) {
	if msg.ReceiptHandle == nil {
		log.Printf("received message with nil receipt handle")
		return
	}

	defer func() {
		if err := h.queueClient.DeleteMessage(ctx, *msg.ReceiptHandle); err != nil {
			log.Printf("failed to delete event message: %v", err)
			metricCtx, metricCancel := context.WithTimeout(ctx, config.ShortTimeout)
			defer metricCancel()
			if metricErr := h.metrics.PublishMessageDeletionFailure(metricCtx); metricErr != nil {
				log.Printf("failed to publish deletion failure metric: %v", metricErr)
			}
		}
	}()

	if msg.Body == nil {
		log.Printf("received message with nil body")
		return
	}

	var event EventBridgeEvent
	if err := json.Unmarshal([]byte(*msg.Body), &event); err != nil {
		log.Printf("failed to unmarshal event: %v", err)
		return
	}

	var processErr error
	switch event.DetailType {
	case "EC2 Spot Instance Interruption Warning":
		processErr = h.handleSpotInterruption(ctx, event.Detail)
	case "EC2 Instance State-change Notification":
		processErr = h.handleStateChange(ctx, event.Detail)
	default:
		log.Printf("unknown event type: %s", event.DetailType)
		return
	}

	if processErr != nil {
		log.Printf("failed to process event: %v", processErr)
	}
}

func (h *Handler) retryWithBackoff(ctx context.Context, operation func(context.Context) error) error {
	backoffs := []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoffIdx := attempt - 1
			if backoffIdx >= len(backoffs) {
				backoffIdx = len(backoffs) - 1
			}
			select {
			case <-time.After(backoffs[backoffIdx]):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		mctx, mcancel := context.WithTimeout(ctx, config.ShortTimeout)
		err := operation(mctx)
		mcancel()

		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

func (h *Handler) handleSpotInterruption(ctx context.Context, detailRaw json.RawMessage) error {
	var detail SpotInterruptionDetail
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		return fmt.Errorf("failed to unmarshal spot interruption detail: %w", err)
	}

	log.Printf("Spot interruption received for instance %s", detail.InstanceID)

	if err := h.retryWithBackoff(ctx, h.metrics.PublishSpotInterruption); err != nil {
		return fmt.Errorf("failed to publish spot interruption metric: %w", err)
	}

	if err := h.dbClient.MarkInstanceTerminating(ctx, detail.InstanceID); err != nil {
		return fmt.Errorf("failed to mark instance as terminating: %w", err)
	}

	job, err := h.dbClient.GetJobByInstance(ctx, detail.InstanceID)
	if err != nil {
		return fmt.Errorf("failed to get job for instance: %w", err)
	}
	if job == nil {
		log.Printf("No job found for instance %s, skipping re-queue", detail.InstanceID)
		return nil
	}

	if job.JobID == "" || job.RunID == "" {
		log.Printf("Invalid job data for instance %s (JobID=%s, RunID=%s), skipping re-queue", detail.InstanceID, job.JobID, job.RunID)
		return nil
	}

	// Record interruption in circuit breaker
	if h.circuitBreaker != nil && job.InstanceType != "" {
		if err := h.circuitBreaker.RecordInterruption(ctx, job.InstanceType); err != nil {
			log.Printf("Warning: failed to record interruption in circuit breaker: %v", err)
		}
	}

	// Re-queue with ForceOnDemand to ensure job completes on retry
	requeueMsg := &queue.JobMessage{
		JobID:         job.JobID,
		RunID:         job.RunID,
		InstanceType:  job.InstanceType,
		Pool:          job.Pool,
		Private:       job.Private,
		Spot:          job.Spot,
		RunnerSpec:    job.RunnerSpec,
		RetryCount:    1,    // Mark as retry
		ForceOnDemand: true, // Force on-demand for retries after spot interruption
	}
	if err := h.queueClient.SendMessage(ctx, requeueMsg); err != nil {
		return fmt.Errorf("failed to re-queue job %s: %w", job.JobID, err)
	}

	log.Printf("Successfully re-queued job %s from interrupted instance %s (ForceOnDemand=true)", job.JobID, detail.InstanceID)
	return nil
}

func (h *Handler) handleStateChange(ctx context.Context, detailRaw json.RawMessage) error {
	var detail StateChangeDetail
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		return fmt.Errorf("failed to unmarshal state change detail: %w", err)
	}

	log.Printf("Instance %s changed state to %s", detail.InstanceID, detail.State)

	switch detail.State {
	case "running":
		if err := h.retryWithBackoff(ctx, h.metrics.PublishFleetSizeIncrement); err != nil {
			return fmt.Errorf("failed to publish fleet size increment metric: %w", err)
		}
	case "stopped", "terminated":
		if err := h.retryWithBackoff(ctx, h.metrics.PublishFleetSizeDecrement); err != nil {
			return fmt.Errorf("failed to publish fleet size decrement metric: %w", err)
		}
	case "pending", "stopping", "shutting-down":
		log.Printf("Ignoring intermediate state %s for instance %s", detail.State, detail.InstanceID)
	default:
		log.Printf("Unknown state %s for instance %s", detail.State, detail.InstanceID)
	}
	return nil
}
