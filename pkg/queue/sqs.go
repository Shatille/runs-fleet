package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type SQSAPI interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

type Client struct {
	sqsClient SQSAPI
	queueURL  string
}

func NewClient(cfg aws.Config, queueURL string) *Client {
	return &Client{
		sqsClient: sqs.NewFromConfig(cfg),
		queueURL:  queueURL,
	}
}

type JobMessage struct {
	JobID        string `json:"job_id,omitempty"`
	RunID        string `json:"run_id"`
	InstanceType string `json:"instance_type"`
	Pool         string `json:"pool,omitempty"`
	Private      bool   `json:"private"`
	Spot         bool   `json:"spot"`
	RunnerSpec   string `json:"runner_spec"`
}

func (c *Client) SendMessage(ctx context.Context, job *JobMessage) error {
	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("failed to marshal job: %w", err)
	}

	input := &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.queueURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String(job.RunID),
		MessageDeduplicationId: aws.String(job.JobID),
	}

	_, err = c.sqsClient.SendMessage(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to send message to SQS: %w", err)
	}

	return nil
}

func (c *Client) ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]types.Message, error) {
	input := &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(c.queueURL),
		MaxNumberOfMessages: maxMessages,
		WaitTimeSeconds:     waitTimeSeconds,
		AttributeNames:      []types.QueueAttributeName{types.QueueAttributeNameAll},
	}

	output, err := c.sqsClient.ReceiveMessage(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to receive messages: %w", err)
	}

	return output.Messages, nil
}

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
