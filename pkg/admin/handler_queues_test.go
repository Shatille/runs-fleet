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
	t.Parallel()

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
			t.Parallel()

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
	t.Parallel()

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

func TestQueuesHandler_UnparseableAttribute(t *testing.T) {
	t.Parallel()

	const mainURL = "https://sqs.us-east-1.amazonaws.com/123456789/main"
	config := QueueConfig{MainQueue: mainURL}
	attrs := map[string]map[string]string{
		mainURL: {
			string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "not-a-number",
			"ApproximateAgeOfOldestMessage":                                "13",
		},
	}

	handler := NewQueuesHandler(&mockSQSAPI{attrs: attrs}, config, NewAuthMiddleware(""))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/queues/main", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusOK)
	}

	var resp QueueStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.MessagesVisible != 0 {
		t.Errorf("messages_visible = %d, want 0 for unparseable value", resp.MessagesVisible)
	}
	if resp.OldestMessageAgeSeconds != 13 {
		t.Errorf("oldest_message_age_seconds = %d, want 13 (parseable siblings unaffected)", resp.OldestMessageAgeSeconds)
	}
}

func TestQueuesHandler_GetQueue(t *testing.T) {
	t.Parallel()

	const mainURL = "https://sqs.us-east-1.amazonaws.com/123456789/main"
	const dlqURL = "https://sqs.us-east-1.amazonaws.com/123456789/main-dlq"
	const poolURL = "https://sqs.us-east-1.amazonaws.com/123456789/pool"

	config := QueueConfig{MainQueue: mainURL, MainQueueDLQ: dlqURL, PoolQueue: poolURL}
	attrs := map[string]map[string]string{
		mainURL: {
			string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "7",
			"ApproximateAgeOfOldestMessage":                                "42",
		},
		dlqURL:  {string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "2"},
		poolURL: {string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "1"},
	}

	tests := []struct {
		name       string
		queueName  string
		wantStatus int
		wantDLQ    int
		wantAge    int
	}{
		{name: "main with dlq and age", queueName: "main", wantStatus: http.StatusOK, wantDLQ: 2, wantAge: 42},
		{name: "pool", queueName: "pool", wantStatus: http.StatusOK},
		{name: "unknown name", queueName: "bogus", wantStatus: http.StatusNotFound},
		{name: "unconfigured queue", queueName: "events", wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := NewQueuesHandler(&mockSQSAPI{attrs: attrs}, config, NewAuthMiddleware(""))
			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)

			req := httptest.NewRequest("GET", "/api/queues/"+tt.queueName, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("got status %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus != http.StatusOK {
				return
			}

			var resp QueueStatusResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			if resp.Name != tt.queueName {
				t.Errorf("name = %q, want %q", resp.Name, tt.queueName)
			}
			if resp.DLQMessages != tt.wantDLQ {
				t.Errorf("dlq_messages = %d, want %d", resp.DLQMessages, tt.wantDLQ)
			}
			if resp.OldestMessageAgeSeconds != tt.wantAge {
				t.Errorf("oldest_message_age_seconds = %d, want %d", resp.OldestMessageAgeSeconds, tt.wantAge)
			}
		})
	}
}
