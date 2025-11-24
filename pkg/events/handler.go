// Package events handles EventBridge events from SQS.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// QueueAPI provides SQS operations for event processing.
type QueueAPI interface {
	ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]types.Message, error)
	DeleteMessage(ctx context.Context, receiptHandle string) error
}

// DBAPI provides database operations for event processing.
type DBAPI interface {
	// Add methods as needed by Handler
}

// MetricsAPI provides CloudWatch metrics publishing.
type MetricsAPI interface {
	PublishSpotInterruption(ctx context.Context) error
	PublishFleetSize(ctx context.Context, size float64) error
}

// Handler processes EventBridge events from SQS queue.
type Handler struct {
	queueClient QueueAPI
	dbClient    DBAPI
	metrics     MetricsAPI
	config      *config.Config
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

			var wg sync.WaitGroup
			for _, msg := range messages {
				msg := msg
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					processCtx, processCancel := context.WithTimeout(ctx, 30*time.Second)
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
	if msg.Body == nil {
		log.Printf("received message with nil body")
		return
	}
	if msg.ReceiptHandle == nil {
		log.Printf("received message with nil receipt handle")
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
		return
	}

	if err := h.queueClient.DeleteMessage(ctx, *msg.ReceiptHandle); err != nil {
		log.Printf("failed to delete event message: %v", err)
	}
}

func (h *Handler) handleSpotInterruption(ctx context.Context, detailRaw json.RawMessage) error {
	var detail SpotInterruptionDetail
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		return fmt.Errorf("failed to unmarshal spot interruption detail: %w", err)
	}

	log.Printf("Spot interruption received for instance %s", detail.InstanceID)

	mctx, mcancel := context.WithTimeout(ctx, 3*time.Second)
	defer mcancel()
	if err := h.metrics.PublishSpotInterruption(mctx); err != nil {
		return fmt.Errorf("failed to publish spot interruption metric: %w", err)
	}

	// TODO(CRITICAL): Mark instance as terminating in DB - Required for Phase 4 spot recovery
	// TODO(CRITICAL): Re-queue job if running on this instance - Required for Phase 4 spot recovery
	// See: https://github.com/Shavakan/runs-fleet/issues/TBD
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
		mctx, mcancel := context.WithTimeout(ctx, 3*time.Second)
		defer mcancel()
		if err := h.metrics.PublishFleetSize(mctx, 1); err != nil {
			return fmt.Errorf("failed to publish fleet size metric for running: %w", err)
		}
	case "stopped", "terminated":
		mctx, mcancel := context.WithTimeout(ctx, 3*time.Second)
		defer mcancel()
		if err := h.metrics.PublishFleetSize(mctx, -1); err != nil {
			return fmt.Errorf("failed to publish fleet size metric for %s: %w", detail.State, err)
		}
	case "pending", "stopping", "shutting-down":
		log.Printf("Ignoring intermediate state %s for instance %s", detail.State, detail.InstanceID)
	default:
		log.Printf("Unknown state %s for instance %s", detail.State, detail.InstanceID)
	}
	return nil
}
