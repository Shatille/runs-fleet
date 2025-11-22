package events

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type QueueAPI interface {
	ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]types.Message, error)
	DeleteMessage(ctx context.Context, receiptHandle string) error
}

type DBAPI interface {
	// Add methods as needed by Handler
}

type MetricsAPI interface {
	PublishSpotInterruption(ctx context.Context)
	PublishFleetSize(ctx context.Context, size float64)
}

type Handler struct {
	queueClient QueueAPI
	dbClient    DBAPI
	metrics     MetricsAPI
	config      *config.Config
}

func NewHandler(q QueueAPI, db DBAPI, m MetricsAPI, cfg *config.Config) *Handler {
	return &Handler{
		queueClient: q,
		dbClient:    db,
		metrics:     m,
		config:      cfg,
	}
}

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

type SpotInterruptionDetail struct {
	InstanceID     string `json:"instance-id"`
	InstanceAction string `json:"instance-action"`
}

type StateChangeDetail struct {
	InstanceID string `json:"instance-id"`
	State      string `json:"state"`
}

func (h *Handler) Run(ctx context.Context) {
	log.Println("Starting event handler loop...")
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			messages, err := h.queueClient.ReceiveMessages(ctx, 10, 20)
			if err != nil {
				log.Printf("Failed to receive event messages: %v", err)
				continue
			}

			for _, msg := range messages {
				h.processEvent(ctx, msg)
			}
		}
	}
}

func (h *Handler) processEvent(ctx context.Context, msg types.Message) {
	defer func() {
		if err := h.queueClient.DeleteMessage(ctx, *msg.ReceiptHandle); err != nil {
			log.Printf("Failed to delete event message: %v", err)
		}
	}()

	var event EventBridgeEvent
	if err := json.Unmarshal([]byte(*msg.Body), &event); err != nil {
		log.Printf("Failed to unmarshal event: %v", err)
		return
	}

	switch event.DetailType {
	case "EC2 Spot Instance Interruption Warning":
		h.handleSpotInterruption(ctx, event.Detail)
	case "EC2 Instance State-change Notification":
		h.handleStateChange(ctx, event.Detail)
	default:
		log.Printf("Unknown event type: %s", event.DetailType)
	}
}

func (h *Handler) handleSpotInterruption(ctx context.Context, detailRaw json.RawMessage) {
	var detail SpotInterruptionDetail
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		log.Printf("Failed to unmarshal spot interruption detail: %v", err)
		return
	}

	log.Printf("Spot interruption received for instance %s", detail.InstanceID)
	h.metrics.PublishSpotInterruption(ctx)

	// TODO: Mark instance as terminating in DB
	// TODO: Re-queue job if running on this instance
}

func (h *Handler) handleStateChange(ctx context.Context, detailRaw json.RawMessage) {
	var detail StateChangeDetail
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		log.Printf("Failed to unmarshal state change detail: %v", err)
		return
	}

	log.Printf("Instance %s changed state to %s", detail.InstanceID, detail.State)

	if detail.State == "terminated" {
		h.metrics.PublishFleetSize(ctx, -1) // Decrement fleet size (approximate)
	} else if detail.State == "running" {
		h.metrics.PublishFleetSize(ctx, 1) // Increment fleet size (approximate)
	}
}
