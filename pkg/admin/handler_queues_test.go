package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type mockSQSAPI struct {
	attrs map[string]map[string]string
	err   error
}

func (m *mockSQSAPI) GetQueueAttributes(_ context.Context, params *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	url := *params.QueueUrl
	return &sqs.GetQueueAttributesOutput{
		Attributes: m.attrs[url],
	}, nil
}

func TestQueuesHandler_ListQueues(t *testing.T) {
	tests := []struct {
		name           string
		config         QueueConfig
		attrs          map[string]map[string]string
		wantCount      int
		wantStatusCode int
	}{
		{
			name: "all queues configured",
			config: QueueConfig{
				MainQueue:         "https://sqs.us-east-1.amazonaws.com/123456789/main",
				MainQueueDLQ:      "https://sqs.us-east-1.amazonaws.com/123456789/main-dlq",
				PoolQueue:         "https://sqs.us-east-1.amazonaws.com/123456789/pool",
				EventsQueue:       "https://sqs.us-east-1.amazonaws.com/123456789/events",
				TerminationQueue:  "https://sqs.us-east-1.amazonaws.com/123456789/termination",
				HousekeepingQueue: "https://sqs.us-east-1.amazonaws.com/123456789/housekeeping",
			},
			attrs: map[string]map[string]string{
				"https://sqs.us-east-1.amazonaws.com/123456789/main": {
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages):           "10",
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible): "5",
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed):    "2",
				},
				"https://sqs.us-east-1.amazonaws.com/123456789/main-dlq": {
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "3",
				},
				"https://sqs.us-east-1.amazonaws.com/123456789/pool": {
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "0",
				},
				"https://sqs.us-east-1.amazonaws.com/123456789/events": {
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "1",
				},
				"https://sqs.us-east-1.amazonaws.com/123456789/termination": {
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "0",
				},
				"https://sqs.us-east-1.amazonaws.com/123456789/housekeeping": {
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "0",
				},
			},
			wantCount:      5,
			wantStatusCode: http.StatusOK,
		},
		{
			name: "only main queue configured",
			config: QueueConfig{
				MainQueue: "https://sqs.us-east-1.amazonaws.com/123456789/main",
			},
			attrs: map[string]map[string]string{
				"https://sqs.us-east-1.amazonaws.com/123456789/main": {
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "5",
				},
			},
			wantCount:      1,
			wantStatusCode: http.StatusOK,
		},
		{
			name:           "no queues configured",
			config:         QueueConfig{},
			attrs:          map[string]map[string]string{},
			wantCount:      0,
			wantStatusCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sqsMock := &mockSQSAPI{attrs: tt.attrs}
			auth := NewAuthMiddleware("")
			handler := NewQueuesHandler(sqsMock, tt.config, auth)

			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)

			req := httptest.NewRequest("GET", "/api/queues", nil)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatusCode {
				t.Errorf("got status %d, want %d", rec.Code, tt.wantStatusCode)
			}

			if tt.wantStatusCode == http.StatusOK {
				var resp struct {
					Queues []QueueStatusResponse `json:"queues"`
				}
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}

				if len(resp.Queues) != tt.wantCount {
					t.Errorf("got queue count %d, want %d", len(resp.Queues), tt.wantCount)
				}
			}
		})
	}
}

func TestQueuesHandler_DLQMessages(t *testing.T) {
	config := QueueConfig{
		MainQueue:    "https://sqs.us-east-1.amazonaws.com/123456789/main",
		MainQueueDLQ: "https://sqs.us-east-1.amazonaws.com/123456789/main-dlq",
	}

	sqsMock := &mockSQSAPI{
		attrs: map[string]map[string]string{
			"https://sqs.us-east-1.amazonaws.com/123456789/main": {
				string(sqstypes.QueueAttributeNameApproximateNumberOfMessages):           "10",
				string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible): "0",
				string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed):    "0",
			},
			"https://sqs.us-east-1.amazonaws.com/123456789/main-dlq": {
				string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "5",
			},
		},
	}

	auth := NewAuthMiddleware("")
	handler := NewQueuesHandler(sqsMock, config, auth)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/queues", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Queues []QueueStatusResponse `json:"queues"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Queues) != 1 {
		t.Fatalf("got queue count %d, want 1", len(resp.Queues))
	}

	mainQueue := resp.Queues[0]
	if mainQueue.MessagesVisible != 10 {
		t.Errorf("got messages visible %d, want 10", mainQueue.MessagesVisible)
	}
	if mainQueue.DLQMessages != 5 {
		t.Errorf("got DLQ messages %d, want 5", mainQueue.DLQMessages)
	}
}
