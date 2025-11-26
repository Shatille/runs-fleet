// Package queue provides SQS-based message queue operations for job orchestration.
package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
)

// SQSAPI defines SQS operations for queue message handling.
type SQSAPI interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// Client provides SQS queue operations for job messages.
type Client struct {
	sqsClient SQSAPI
	queueURL  string
}

// NewClient creates SQS client for specified queue URL.
func NewClient(cfg aws.Config, queueURL string) *Client {
	return &Client{
		sqsClient: sqs.NewFromConfig(cfg),
		queueURL:  queueURL,
	}
}

// JobMessage represents workflow job configuration for queue transport.
type JobMessage struct {
	JobID         string `json:"job_id,omitempty"`
	RunID         string `json:"run_id"`
	InstanceType  string `json:"instance_type"`
	Pool          string `json:"pool,omitempty"`
	Private       bool   `json:"private"`
	Spot          bool   `json:"spot"`
	RunnerSpec    string `json:"runner_spec"`
	RetryCount    int    `json:"retry_count,omitempty"`     // Number of times this job has been retried
	ForceOnDemand bool   `json:"force_on_demand,omitempty"` // Force on-demand for retries after spot interruption
	// Multi-region support (Phase 3)
	Region string `json:"region,omitempty"`
	// Per-stack environment support (Phase 6)
	Environment string `json:"environment,omitempty"`
	// Windows support (Phase 4)
	OS   string `json:"os,omitempty"`   // linux, windows
	Arch string `json:"arch,omitempty"` // x64, arm64
	// Flexible instance selection (Phase 10)
	InstanceTypes []string `json:"instance_types,omitempty"` // Multiple instance types for spot diversification
	// OpenTelemetry tracing (Phase 5)
	TraceID  string `json:"trace_id,omitempty"`
	SpanID   string `json:"span_id,omitempty"`
	ParentID string `json:"parent_id,omitempty"`
}

// SendMessage sends job message to SQS FIFO queue with deduplication.
// Deduplication ID uses JobID + timestamp + UUID to prevent race conditions from concurrent sends.
func (c *Client) SendMessage(ctx context.Context, job *JobMessage) error {
	if job.JobID == "" {
		return fmt.Errorf("job ID is required for SQS FIFO deduplication")
	}
	if job.RunID == "" {
		return fmt.Errorf("run ID is required for SQS FIFO message grouping")
	}

	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("failed to marshal job: %w", err)
	}

	dedupKey := fmt.Sprintf("%s-%d-%s", job.JobID, time.Now().UnixNano(), uuid.New().String()[:8])
	hash := sha256.Sum256([]byte(dedupKey))
	dedupID := hex.EncodeToString(hash[:])

	input := &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.queueURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String(job.RunID),
		MessageDeduplicationId: aws.String(dedupID),
	}

	// Add trace context as message attributes if present
	if job.TraceID != "" {
		input.MessageAttributes = map[string]types.MessageAttributeValue{
			"TraceID": {
				DataType:    aws.String("String"),
				StringValue: aws.String(job.TraceID),
			},
		}
		if job.SpanID != "" {
			input.MessageAttributes["SpanID"] = types.MessageAttributeValue{
				DataType:    aws.String("String"),
				StringValue: aws.String(job.SpanID),
			}
		}
		if job.ParentID != "" {
			input.MessageAttributes["ParentID"] = types.MessageAttributeValue{
				DataType:    aws.String("String"),
				StringValue: aws.String(job.ParentID),
			}
		}
	}

	_, err = c.sqsClient.SendMessage(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to send message to SQS: %w", err)
	}

	return nil
}

// ReceiveMessages retrieves messages from queue with long polling.
// VisibilityTimeout is set to 60 seconds to allow for message processing and retry logic.
func (c *Client) ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]types.Message, error) {
	input := &sqs.ReceiveMessageInput{
		QueueUrl:              aws.String(c.queueURL),
		MaxNumberOfMessages:   maxMessages,
		WaitTimeSeconds:       waitTimeSeconds,
		VisibilityTimeout:     int32(60),
		AttributeNames:        []types.QueueAttributeName{types.QueueAttributeNameAll},
		MessageAttributeNames: []string{"All"}, // Include message attributes for trace context
	}

	output, err := c.sqsClient.ReceiveMessage(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to receive messages: %w", err)
	}

	return output.Messages, nil
}

// DeleteMessage removes processed message from queue.
func (c *Client) DeleteMessage(ctx context.Context, receiptHandle string) error {
	_, err := c.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.queueURL),
		ReceiptHandle: aws.String(receiptHandle),
	})
	if err != nil {
		return fmt.Errorf("failed to delete message: %w", err)
	}
	return nil
}

// ExtractTraceContext extracts trace context from SQS message attributes.
func ExtractTraceContext(msg types.Message) (traceID, spanID, parentID string) {
	if msg.MessageAttributes == nil {
		return "", "", ""
	}

	if attr, ok := msg.MessageAttributes["TraceID"]; ok && attr.StringValue != nil {
		traceID = *attr.StringValue
	}
	if attr, ok := msg.MessageAttributes["SpanID"]; ok && attr.StringValue != nil {
		spanID = *attr.StringValue
	}
	if attr, ok := msg.MessageAttributes["ParentID"]; ok && attr.StringValue != nil {
		parentID = *attr.StringValue
	}

	return traceID, spanID, parentID
}
