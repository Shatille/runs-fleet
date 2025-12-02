package queue

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type mockSQSClient struct {
	SendMessageFunc    func(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	ReceiveMessageFunc func(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessageFunc  func(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

func (m *mockSQSClient) SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	if m.SendMessageFunc != nil {
		return m.SendMessageFunc(ctx, params, optFns...)
	}
	return &sqs.SendMessageOutput{}, nil
}

func (m *mockSQSClient) ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	if m.ReceiveMessageFunc != nil {
		return m.ReceiveMessageFunc(ctx, params, optFns...)
	}
	return &sqs.ReceiveMessageOutput{}, nil
}

func (m *mockSQSClient) DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	if m.DeleteMessageFunc != nil {
		return m.DeleteMessageFunc(ctx, params, optFns...)
	}
	return &sqs.DeleteMessageOutput{}, nil
}

func TestJobMessage_Marshal(t *testing.T) {
	job := &JobMessage{
		JobID:        "456",
		RunID:        "123",
		InstanceType: "t4g.medium",
		Pool:         "default",
		Private:      true,
		Spot:         false,
		RunnerSpec:   "2cpu-linux-arm64",
	}

	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	expected := `{"job_id":"456","run_id":"123","instance_type":"t4g.medium","pool":"default","private":true,"spot":false,"runner_spec":"2cpu-linux-arm64"}`
	if string(data) != expected {
		t.Errorf("Marshal result = %s, want %s", string(data), expected)
	}
}

func TestJobMessage_Unmarshal(t *testing.T) {
	jsonStr := `{"run_id":"456","instance_type":"c7g.xlarge","private":false,"spot":true,"runner_spec":"4cpu-linux-arm64"}`

	var job JobMessage
	if err := json.Unmarshal([]byte(jsonStr), &job); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if job.RunID != "456" {
		t.Errorf("RunID = %s, want 456", job.RunID)
	}
	if job.InstanceType != "c7g.xlarge" {
		t.Errorf("InstanceType = %s, want c7g.xlarge", job.InstanceType)
	}
	if job.Pool != "" {
		t.Errorf("Pool = %s, want empty", job.Pool)
	}
	if job.Private != false {
		t.Errorf("Private = %v, want false", job.Private)
	}
	if job.Spot != true {
		t.Errorf("Spot = %v, want true", job.Spot)
	}
}

func TestSQSClient_SendMessage(t *testing.T) {
	tests := []struct {
		name    string
		job     *JobMessage
		mock    *mockSQSClient
		wantErr bool
	}{
		{
			name: "success",
			job: &JobMessage{
				JobID:        "job-123",
				RunID:        "test-run-123",
				InstanceType: "t4g.medium",
				Spot:         true,
				RunnerSpec:   "2cpu-linux-arm64",
			},
			mock: &mockSQSClient{
				SendMessageFunc: func(_ context.Context, params *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
					if params.MessageGroupId == nil || *params.MessageGroupId != "test-run-123" {
						t.Error("MessageGroupId not set correctly")
					}
					if params.MessageDeduplicationId == nil || *params.MessageDeduplicationId == "" {
						t.Error("MessageDeduplicationId not set")
					}
					if *params.MessageDeduplicationId == "test-run-123" {
						t.Error("MessageDeduplicationId should not equal RunID")
					}
					if *params.MessageDeduplicationId == "job-123" {
						t.Error("MessageDeduplicationId should not equal JobID")
					}
					return &sqs.SendMessageOutput{
						MessageId: aws.String("msg-123"),
					}, nil
				},
			},
			wantErr: false,
		},
		{
			name: "empty job ID",
			job: &JobMessage{
				RunID:        "test-run-123",
				InstanceType: "t4g.medium",
			},
			mock:    &mockSQSClient{},
			wantErr: true,
		},
		{
			name: "empty run ID",
			job: &JobMessage{
				JobID:        "job-123",
				InstanceType: "t4g.medium",
			},
			mock:    &mockSQSClient{},
			wantErr: true,
		},
		{
			name: "sqs error",
			job: &JobMessage{
				JobID:        "job-456",
				RunID:        "test-run-456",
				InstanceType: "t4g.medium",
			},
			mock: &mockSQSClient{
				SendMessageFunc: func(_ context.Context, _ *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
					return nil, errors.New("sqs error")
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &SQSClient{
				sqsClient: tt.mock,
				queueURL:  "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo",
			}

			err := client.SendMessage(context.Background(), tt.job)
			if (err != nil) != tt.wantErr {
				t.Errorf("SendMessage() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSQSClient_SendMessage_RetryGeneratesUniqueDedupID(t *testing.T) {
	var firstDedupID, secondDedupID string

	job := &JobMessage{
		JobID:        "job-retry-test",
		RunID:        "run-123",
		InstanceType: "t4g.medium",
		Spot:         true,
		RunnerSpec:   "2cpu-linux-arm64",
	}

	mock := &mockSQSClient{
		SendMessageFunc: func(_ context.Context, params *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
			if firstDedupID == "" {
				firstDedupID = *params.MessageDeduplicationId
			} else {
				secondDedupID = *params.MessageDeduplicationId
			}
			return &sqs.SendMessageOutput{MessageId: aws.String("msg-123")}, nil
		},
	}

	client := &SQSClient{
		sqsClient: mock,
		queueURL:  "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo",
	}

	if err := client.SendMessage(context.Background(), job); err != nil {
		t.Fatalf("First SendMessage() failed: %v", err)
	}

	if err := client.SendMessage(context.Background(), job); err != nil {
		t.Fatalf("Second SendMessage() failed: %v", err)
	}

	if firstDedupID == "" || secondDedupID == "" {
		t.Fatal("Deduplication IDs were not captured")
	}

	if firstDedupID == secondDedupID {
		t.Errorf("Retry produced same deduplication ID, preventing retries. First=%s, Second=%s", firstDedupID, secondDedupID)
	}
}

func TestSQSClient_ReceiveMessages(t *testing.T) {
	tests := []struct {
		name         string
		mock         *mockSQSClient
		wantMsgCount int
		wantErr      bool
	}{
		{
			name: "success with messages",
			mock: &mockSQSClient{
				ReceiveMessageFunc: func(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
					return &sqs.ReceiveMessageOutput{
						Messages: []types.Message{
							{
								MessageId:     aws.String("msg-1"),
								Body:          aws.String(`{"run_id":"123"}`),
								ReceiptHandle: aws.String("receipt-1"),
							},
							{
								MessageId:     aws.String("msg-2"),
								Body:          aws.String(`{"run_id":"456"}`),
								ReceiptHandle: aws.String("receipt-2"),
							},
						},
					}, nil
				},
			},
			wantMsgCount: 2,
			wantErr:      false,
		},
		{
			name: "success with no messages",
			mock: &mockSQSClient{
				ReceiveMessageFunc: func(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
					return &sqs.ReceiveMessageOutput{
						Messages: []types.Message{},
					}, nil
				},
			},
			wantMsgCount: 0,
			wantErr:      false,
		},
		{
			name: "sqs error",
			mock: &mockSQSClient{
				ReceiveMessageFunc: func(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
					return nil, errors.New("sqs error")
				},
			},
			wantMsgCount: 0,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &SQSClient{
				sqsClient: tt.mock,
				queueURL:  "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo",
			}

			messages, err := client.ReceiveMessages(context.Background(), 10, 20)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReceiveMessages() error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(messages) != tt.wantMsgCount {
				t.Errorf("ReceiveMessages() got %d messages, want %d", len(messages), tt.wantMsgCount)
			}
		})
	}
}

func TestSQSClient_DeleteMessage(t *testing.T) {
	tests := []struct {
		name    string
		mock    *mockSQSClient
		wantErr bool
	}{
		{
			name: "success",
			mock: &mockSQSClient{
				DeleteMessageFunc: func(_ context.Context, params *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
					if params.ReceiptHandle == nil || *params.ReceiptHandle != "receipt-123" {
						t.Error("ReceiptHandle not set correctly")
					}
					return &sqs.DeleteMessageOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name: "sqs error",
			mock: &mockSQSClient{
				DeleteMessageFunc: func(_ context.Context, _ *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
					return nil, errors.New("sqs error")
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &SQSClient{
				sqsClient: tt.mock,
				queueURL:  "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo",
			}

			err := client.DeleteMessage(context.Background(), "receipt-123")
			if (err != nil) != tt.wantErr {
				t.Errorf("DeleteMessage() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSQSClient_SendMessage_WithTraceContext(t *testing.T) {
	var capturedInput *sqs.SendMessageInput

	mock := &mockSQSClient{
		SendMessageFunc: func(_ context.Context, params *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
			capturedInput = params
			return &sqs.SendMessageOutput{MessageId: aws.String("msg-123")}, nil
		},
	}

	client := &SQSClient{
		sqsClient: mock,
		queueURL:  "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo",
	}

	job := &JobMessage{
		JobID:        "job-trace",
		RunID:        "run-trace",
		InstanceType: "t4g.medium",
		TraceID:      "trace-abc123",
		SpanID:       "span-def456",
		ParentID:     "parent-ghi789",
	}

	err := client.SendMessage(context.Background(), job)
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	// Verify trace context attributes are set
	if capturedInput.MessageAttributes == nil {
		t.Fatal("MessageAttributes should not be nil when trace context is present")
	}
	if capturedInput.MessageAttributes["TraceID"].StringValue == nil ||
		*capturedInput.MessageAttributes["TraceID"].StringValue != "trace-abc123" {
		t.Error("TraceID attribute not set correctly")
	}
	if capturedInput.MessageAttributes["SpanID"].StringValue == nil ||
		*capturedInput.MessageAttributes["SpanID"].StringValue != "span-def456" {
		t.Error("SpanID attribute not set correctly")
	}
	if capturedInput.MessageAttributes["ParentID"].StringValue == nil ||
		*capturedInput.MessageAttributes["ParentID"].StringValue != "parent-ghi789" {
		t.Error("ParentID attribute not set correctly")
	}
}

func TestSQSClient_SendMessage_WithPartialTraceContext(t *testing.T) {
	var capturedInput *sqs.SendMessageInput

	mock := &mockSQSClient{
		SendMessageFunc: func(_ context.Context, params *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
			capturedInput = params
			return &sqs.SendMessageOutput{MessageId: aws.String("msg-123")}, nil
		},
	}

	client := &SQSClient{
		sqsClient: mock,
		queueURL:  "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo",
	}

	// Only TraceID, no SpanID or ParentID
	job := &JobMessage{
		JobID:        "job-partial",
		RunID:        "run-partial",
		InstanceType: "t4g.medium",
		TraceID:      "trace-only",
	}

	err := client.SendMessage(context.Background(), job)
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	if capturedInput.MessageAttributes == nil {
		t.Fatal("MessageAttributes should not be nil when trace context is present")
	}
	if capturedInput.MessageAttributes["TraceID"].StringValue == nil ||
		*capturedInput.MessageAttributes["TraceID"].StringValue != "trace-only" {
		t.Error("TraceID attribute not set correctly")
	}
	// SpanID and ParentID should not be present since they're empty
	if _, ok := capturedInput.MessageAttributes["SpanID"]; ok {
		t.Error("SpanID should not be set when empty")
	}
	if _, ok := capturedInput.MessageAttributes["ParentID"]; ok {
		t.Error("ParentID should not be set when empty")
	}
}

func TestSQSClient_SendMessage_NoTraceContext(t *testing.T) {
	var capturedInput *sqs.SendMessageInput

	mock := &mockSQSClient{
		SendMessageFunc: func(_ context.Context, params *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
			capturedInput = params
			return &sqs.SendMessageOutput{MessageId: aws.String("msg-123")}, nil
		},
	}

	client := &SQSClient{
		sqsClient: mock,
		queueURL:  "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo",
	}

	job := &JobMessage{
		JobID:        "job-no-trace",
		RunID:        "run-no-trace",
		InstanceType: "t4g.medium",
		// No trace context
	}

	err := client.SendMessage(context.Background(), job)
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	// MessageAttributes should be nil when no trace context
	if capturedInput.MessageAttributes != nil {
		t.Error("MessageAttributes should be nil when no trace context is present")
	}
}

func TestSQSClient_ReceiveMessages_WithMessageAttributes(t *testing.T) {
	mock := &mockSQSClient{
		ReceiveMessageFunc: func(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
			return &sqs.ReceiveMessageOutput{
				Messages: []types.Message{
					{
						MessageId:     aws.String("msg-1"),
						Body:          aws.String(`{"run_id":"123"}`),
						ReceiptHandle: aws.String("receipt-1"),
						MessageAttributes: map[string]types.MessageAttributeValue{
							"TraceID": {
								DataType:    aws.String("String"),
								StringValue: aws.String("trace-recv"),
							},
							"SpanID": {
								DataType:    aws.String("String"),
								StringValue: aws.String("span-recv"),
							},
							"CustomAttr": {
								DataType:    aws.String("String"),
								StringValue: aws.String("custom-value"),
							},
						},
					},
				},
			}, nil
		},
	}

	client := &SQSClient{
		sqsClient: mock,
		queueURL:  "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo",
	}

	messages, err := client.ReceiveMessages(context.Background(), 10, 20)
	if err != nil {
		t.Fatalf("ReceiveMessages() error = %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}

	msg := messages[0]
	if msg.Attributes["TraceID"] != "trace-recv" {
		t.Errorf("TraceID = %q, want %q", msg.Attributes["TraceID"], "trace-recv")
	}
	if msg.Attributes["SpanID"] != "span-recv" {
		t.Errorf("SpanID = %q, want %q", msg.Attributes["SpanID"], "span-recv")
	}
	if msg.Attributes["CustomAttr"] != "custom-value" {
		t.Errorf("CustomAttr = %q, want %q", msg.Attributes["CustomAttr"], "custom-value")
	}
}

func TestSQSClient_ReceiveMessages_NilFields(t *testing.T) {
	mock := &mockSQSClient{
		ReceiveMessageFunc: func(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
			return &sqs.ReceiveMessageOutput{
				Messages: []types.Message{
					{
						// MessageId is nil
						Body:          aws.String(`{"run_id":"123"}`),
						ReceiptHandle: aws.String("receipt-1"),
					},
					{
						MessageId:     aws.String("msg-2"),
						Body:          nil, // Body is nil
						ReceiptHandle: aws.String("receipt-2"),
					},
				},
			}, nil
		},
	}

	client := &SQSClient{
		sqsClient: mock,
		queueURL:  "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo",
	}

	messages, err := client.ReceiveMessages(context.Background(), 10, 20)
	if err != nil {
		t.Fatalf("ReceiveMessages() error = %v", err)
	}

	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	// First message has nil MessageId
	if messages[0].ID != "" {
		t.Errorf("first message ID = %q, want empty", messages[0].ID)
	}
	if messages[0].Body != `{"run_id":"123"}` {
		t.Errorf("first message Body = %q, want %q", messages[0].Body, `{"run_id":"123"}`)
	}

	// Second message has nil Body
	if messages[1].ID != "msg-2" {
		t.Errorf("second message ID = %q, want %q", messages[1].ID, "msg-2")
	}
	if messages[1].Body != "" {
		t.Errorf("second message Body = %q, want empty", messages[1].Body)
	}
}

func TestDeref(t *testing.T) {
	tests := []struct {
		name  string
		input *string
		want  string
	}{
		{
			name:  "non-nil value",
			input: aws.String("test-value"),
			want:  "test-value",
		},
		{
			name:  "nil value",
			input: nil,
			want:  "",
		},
		{
			name:  "empty string",
			input: aws.String(""),
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deref(tt.input)
			if got != tt.want {
				t.Errorf("deref() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewSQSClientWithAPI(t *testing.T) {
	mock := &mockSQSClient{}
	client := NewSQSClientWithAPI(mock, "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo")

	if client.queueURL != "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo" {
		t.Errorf("queueURL = %q, want %q", client.queueURL, "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue.fifo")
	}
	if client.sqsClient != mock {
		t.Error("sqsClient not set correctly")
	}
}
