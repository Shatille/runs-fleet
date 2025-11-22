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

func TestClient_SendMessage(t *testing.T) {
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
				SendMessageFunc: func(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
					if params.MessageGroupId == nil || *params.MessageGroupId != "test-run-123" {
						t.Error("MessageGroupId not set correctly")
					}
					if params.MessageDeduplicationId == nil || *params.MessageDeduplicationId != "job-123" {
						t.Error("MessageDeduplicationId not set correctly")
					}
					return &sqs.SendMessageOutput{
						MessageId: aws.String("msg-123"),
					}, nil
				},
			},
			wantErr: false,
		},
		{
			name: "sqs error",
			job: &JobMessage{
				JobID:        "job-456",
				RunID:        "test-run-456",
				InstanceType: "t4g.medium",
			},
			mock: &mockSQSClient{
				SendMessageFunc: func(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
					return nil, errors.New("sqs error")
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{
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

func TestClient_ReceiveMessages(t *testing.T) {
	tests := []struct {
		name         string
		mock         *mockSQSClient
		wantMsgCount int
		wantErr      bool
	}{
		{
			name: "success with messages",
			mock: &mockSQSClient{
				ReceiveMessageFunc: func(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
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
				ReceiveMessageFunc: func(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
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
				ReceiveMessageFunc: func(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
					return nil, errors.New("sqs error")
				},
			},
			wantMsgCount: 0,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{
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

func TestClient_DeleteMessage(t *testing.T) {
	tests := []struct {
		name    string
		mock    *mockSQSClient
		wantErr bool
	}{
		{
			name: "success",
			mock: &mockSQSClient{
				DeleteMessageFunc: func(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
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
				DeleteMessageFunc: func(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
					return nil, errors.New("sqs error")
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{
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
