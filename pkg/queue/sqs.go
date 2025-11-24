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
	JobID        string `json:"job_id,omitempty"`
	RunID        string `json:"run_id"`
	InstanceType string `json:"instance_type"`
	Pool         string `json:"pool,omitempty"`
	Private      bool   `json:"private"`
	Spot         bool   `json:"spot"`
	RunnerSpec   string `json:"runner_spec"`
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
		QueueUrl:            aws.String(c.queueURL),
		MaxNumberOfMessages: maxMessages,
		WaitTimeSeconds:     waitTimeSeconds,
		VisibilityTimeout:   int32(60),
		AttributeNames:      []types.QueueAttributeName{types.QueueAttributeNameAll},
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
