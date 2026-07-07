package admin

import (
	"bytes"
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

// attrOldestMessageAge is the SQS queue attribute for the age (seconds) of the
// oldest message. The SDK's QueueAttributeName enum omits it, so it's passed as
// a raw string.
const attrOldestMessageAge = "ApproximateAgeOfOldestMessage"

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
	Name                    string `json:"name"`
	URL                     string `json:"url"`
	MessagesVisible         int    `json:"messages_visible"`
	MessagesInFlight        int    `json:"messages_in_flight"`
	MessagesDelayed         int    `json:"messages_delayed"`
	DLQMessages             int    `json:"dlq_messages,omitempty"`
	OldestMessageAgeSeconds int    `json:"oldest_message_age_seconds,omitempty"`
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

// queueNames is the fixed display order of runs-fleet queues.
var queueNames = []string{"main", "pool", "events", "termination", "housekeeping"}

// RegisterRoutes registers queue API routes on the given mux.
func (h *QueuesHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/queues", h.auth.WrapFunc(h.ListQueues))
	mux.Handle("GET /api/queues/{queue_name}", h.auth.WrapFunc(h.GetQueue))
}

// ListQueues handles GET /api/queues.
func (h *QueuesHandler) ListQueues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var results []QueueStatusResponse
	for _, name := range queueNames {
		url, dlqURL, ok := h.queueByName(name)
		if !ok {
			continue
		}

		status, err := h.getQueueStatus(ctx, name, url)
		if err != nil {
			h.log.Warn(ctx, "failed to get queue status",
				slog.String("queue", name),
				slog.String(logging.KeyError, err.Error()))
			continue
		}

		if dlqURL != "" {
			if dlqStatus, err := h.getQueueStatus(ctx, name+"-dlq", dlqURL); err == nil {
				status.DLQMessages = dlqStatus.MessagesVisible
			}
		}

		results = append(results, *status)
	}

	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"queues": results,
	})
}

// GetQueue handles GET /api/queues/{queue_name}. Unknown or unconfigured names
// resolve to 404.
func (h *QueuesHandler) GetQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("queue_name")

	url, dlqURL, ok := h.queueByName(name)
	if !ok {
		h.writeError(w, http.StatusNotFound, "Queue not found", "")
		return
	}

	status, err := h.getQueueStatus(ctx, name, url)
	if err != nil {
		h.log.Error(ctx, "failed to get queue status",
			slog.String("queue", name),
			slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to get queue", err.Error())
		return
	}

	if dlqURL != "" {
		if dlqStatus, err := h.getQueueStatus(ctx, name+"-dlq", dlqURL); err == nil {
			status.DLQMessages = dlqStatus.MessagesVisible
		}
	}

	h.writeJSON(w, http.StatusOK, status)
}

func (h *QueuesHandler) getQueueStatus(ctx context.Context, name, url string) (*QueueStatusResponse, error) {
	attrs, err := h.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed,
			sqstypes.QueueAttributeName(attrOldestMessageAge),
		},
	})
	if err != nil {
		return nil, err
	}

	return &QueueStatusResponse{
		Name:                    name,
		URL:                     url,
		MessagesVisible:         h.atoi(ctx, name, string(sqstypes.QueueAttributeNameApproximateNumberOfMessages), attrs.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)]),
		MessagesInFlight:        h.atoi(ctx, name, string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible), attrs.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible)]),
		MessagesDelayed:         h.atoi(ctx, name, string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed), attrs.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed)]),
		OldestMessageAgeSeconds: h.atoi(ctx, name, attrOldestMessageAge, attrs.Attributes[attrOldestMessageAge]),
	}, nil
}

// queueByName resolves a queue name to its URL and (for the main queue) its DLQ
// URL. ok is false for an unknown or unconfigured name.
func (h *QueuesHandler) queueByName(name string) (url, dlqURL string, ok bool) {
	switch name {
	case "main":
		url, dlqURL = h.config.MainQueue, h.config.MainQueueDLQ
	case "pool":
		url = h.config.PoolQueue
	case "events":
		url = h.config.EventsQueue
	case "termination":
		url = h.config.TerminationQueue
	case "housekeeping":
		url = h.config.HousekeepingQueue
	default:
		return "", "", false
	}
	if url == "" {
		return "", "", false
	}
	return url, dlqURL, true
}

// atoi parses an SQS numeric attribute, returning 0 for an absent value. A
// present-but-unparseable value is a data-integrity signal (API change or
// corruption), so it is logged rather than silently coerced to 0.
func (h *QueuesHandler) atoi(ctx context.Context, queue, attr, s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		h.log.Warn(ctx, "unparseable SQS queue attribute",
			slog.String("queue", queue),
			slog.String("attribute", attr),
			slog.String("value", s),
			slog.String(logging.KeyError, err.Error()))
		return 0
	}
	return n
}

func (h *QueuesHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}

func (h *QueuesHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	// Response-writer helper with no request/context in scope.
	ctx := context.Background()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(data); err != nil {
		h.log.Error(ctx, "json encode failed", slog.String(logging.KeyError, err.Error()))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		h.log.Error(ctx, "write response failed", slog.String(logging.KeyError, err.Error()))
	}
}
