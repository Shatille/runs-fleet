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

// SQSClient implements Queue interface using AWS SQS.
type SQSClient struct {
	sqsClient SQSAPI
	queueURL  string
}

// Verify SQSClient implements Queue interface.
var _ Queue = (*SQSClient)(nil)

// NewSQSClient creates an SQS-backed queue client.
func NewSQSClient(cfg aws.Config, queueURL string) *SQSClient {
	return &SQSClient{
		sqsClient: sqs.NewFromConfig(cfg),
		queueURL:  queueURL,
	}
}

// NewClient is an alias for NewSQSClient for backward compatibility.
func NewClient(cfg aws.Config, queueURL string) *SQSClient {
	return NewSQSClient(cfg, queueURL)
}

// NewSQSClientWithAPI creates an SQS client with a custom API implementation (for testing).
func NewSQSClientWithAPI(api SQSAPI, queueURL string) *SQSClient {
	return &SQSClient{
		sqsClient: api,
		queueURL:  queueURL,
	}
}

// SendMessage sends job message to SQS FIFO queue with deduplication.
func (c *SQSClient) SendMessage(ctx context.Context, job *JobMessage) error {
	if job.JobID == 0 {
		return fmt.Errorf("job ID is required for SQS FIFO deduplication")
	}
	if job.RunID == 0 {
		return fmt.Errorf("run ID is required for SQS FIFO message grouping")
	}

	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("failed to marshal job: %w", err)
	}

	dedupKey := fmt.Sprintf("%d-%d-%s", job.JobID, time.Now().UnixNano(), uuid.New().String()[:8])
	hash := sha256.Sum256([]byte(dedupKey))
	dedupID := hex.EncodeToString(hash[:])

	input := &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.queueURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String(fmt.Sprintf("%d", job.RunID)),
		MessageDeduplicationId: aws.String(dedupID),
	}

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
// VisibilityTimeout set to 120s to exceed MessageProcessTimeout (90s).
func (c *SQSClient) ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]Message, error) {
	input := &sqs.ReceiveMessageInput{
		QueueUrl:              aws.String(c.queueURL),
		MaxNumberOfMessages:   maxMessages,
		WaitTimeSeconds:       waitTimeSeconds,
		VisibilityTimeout:     int32(120),
		AttributeNames:        []types.QueueAttributeName{types.QueueAttributeNameAll},
		MessageAttributeNames: []string{"All"},
	}

	output, err := c.sqsClient.ReceiveMessage(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to receive messages: %w", err)
	}

	messages := make([]Message, 0, len(output.Messages))
	for _, sqsMsg := range output.Messages {
		msg := Message{
			Body:       deref(sqsMsg.Body),
			Handle:     deref(sqsMsg.ReceiptHandle),
			Attributes: make(map[string]string),
		}
		if sqsMsg.MessageId != nil {
			msg.ID = *sqsMsg.MessageId
		}
		for k, v := range sqsMsg.MessageAttributes {
			if v.StringValue != nil {
				msg.Attributes[k] = *v.StringValue
			}
		}
		messages = append(messages, msg)
	}

	return messages, nil
}

// DeleteMessage removes processed message from queue.
func (c *SQSClient) DeleteMessage(ctx context.Context, handle string) error {
	_, err := c.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.queueURL),
		ReceiptHandle: aws.String(handle),
	})
	if err != nil {
		return fmt.Errorf("failed to delete message: %w", err)
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
