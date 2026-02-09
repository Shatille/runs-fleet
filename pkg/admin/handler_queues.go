package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// SQSAPI defines the SQS operations needed for queue status.
type SQSAPI interface {
	GetQueueAttributes(ctx context.Context, params *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

// QueueConfig holds the queue URLs for the handler.
type QueueConfig struct {
	MainQueue         string
	MainQueueDLQ      string
	PoolQueue         string
	EventsQueue       string
	TerminationQueue  string
	HousekeepingQueue string
}

// QueueStatusResponse represents queue status in the admin API response.
type QueueStatusResponse struct {
	Name             string `json:"name"`
	URL              string `json:"url"`
	MessagesVisible  int    `json:"messages_visible"`
	MessagesInFlight int    `json:"messages_in_flight"`
	MessagesDelayed  int    `json:"messages_delayed"`
	DLQMessages      int    `json:"dlq_messages,omitempty"`
}

// QueuesHandler provides HTTP endpoints for queue status.
type QueuesHandler struct {
	sqs    SQSAPI
	config QueueConfig
	auth   *AuthMiddleware
	log    *logging.Logger
}

// NewQueuesHandler creates a new queues handler.
func NewQueuesHandler(sqsClient SQSAPI, config QueueConfig, auth *AuthMiddleware) *QueuesHandler {
	return &QueuesHandler{
		sqs:    sqsClient,
		config: config,
		auth:   auth,
		log:    logging.WithComponent(logging.LogTypeAdmin, "queues"),
	}
}

// RegisterRoutes registers queue API routes on the given mux.
func (h *QueuesHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/queues", h.auth.WrapFunc(h.ListQueues))
}

// ListQueues handles GET /api/queues.
func (h *QueuesHandler) ListQueues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	queues := []struct {
		name   string
		url    string
		dlqURL string
	}{
		{"main", h.config.MainQueue, h.config.MainQueueDLQ},
		{"pool", h.config.PoolQueue, ""},
		{"events", h.config.EventsQueue, ""},
		{"termination", h.config.TerminationQueue, ""},
		{"housekeeping", h.config.HousekeepingQueue, ""},
	}

	var results []QueueStatusResponse
	for _, q := range queues {
		if q.url == "" {
			continue
		}

		status, err := h.getQueueStatus(ctx, q.name, q.url)
		if err != nil {
			h.log.Warn("failed to get queue status",
				slog.String("queue", q.name),
				slog.String(logging.KeyError, err.Error()))
			continue
		}

		if q.dlqURL != "" {
			dlqStatus, err := h.getQueueStatus(ctx, q.name+"-dlq", q.dlqURL)
			if err == nil {
				status.DLQMessages = dlqStatus.MessagesVisible
			}
		}

		results = append(results, *status)
	}

	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"queues": results,
	})
}

func (h *QueuesHandler) getQueueStatus(ctx context.Context, name, url string) (*QueueStatusResponse, error) {
	attrs, err := h.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed,
		},
	})
	if err != nil {
		return nil, err
	}

	return &QueueStatusResponse{
		Name:             name,
		URL:              url,
		MessagesVisible:  atoi(attrs.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)]),
		MessagesInFlight: atoi(attrs.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible)]),
		MessagesDelayed:  atoi(attrs.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed)]),
	}, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (h *QueuesHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		h.log.Error("json encode failed", slog.String(logging.KeyError, err.Error()))
	}
}
