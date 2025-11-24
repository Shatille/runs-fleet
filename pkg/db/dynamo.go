// Package db provides DynamoDB client operations for pool configuration and state management.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DynamoDBAPI defines DynamoDB operations for pool configuration storage.
type DynamoDBAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// Client provides DynamoDB operations for pool configuration and state.
type Client struct {
	dynamoClient DynamoDBAPI
	poolsTable   string
	jobsTable    string
}

// NewClient creates DynamoDB client for specified pools and jobs tables.
func NewClient(cfg aws.Config, poolsTable, jobsTable string) *Client {
	return &Client{
		dynamoClient: dynamodb.NewFromConfig(cfg),
		poolsTable:   poolsTable,
		jobsTable:    jobsTable,
	}
}

// PoolConfig represents pool configuration from DynamoDB.
type PoolConfig struct {
	PoolName       string `dynamodbav:"pool_name"`
	InstanceType   string `dynamodbav:"instance_type"`
	DesiredRunning int    `dynamodbav:"desired_running"`
	DesiredStopped int    `dynamodbav:"desired_stopped"`
}

// GetPoolConfig retrieves pool configuration from DynamoDB.
func (c *Client) GetPoolConfig(ctx context.Context, poolName string) (*PoolConfig, error) {
	if poolName == "" {
		return nil, fmt.Errorf("pool name cannot be empty")
	}

	key, err := attributevalue.MarshalMap(map[string]string{
		"pool_name": poolName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal key: %w", err)
	}

	output, err := c.dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.poolsTable),
		Key:       key,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get item: %w", err)
	}

	if output.Item == nil {
		return nil, nil // Not found
	}

	var config PoolConfig
	if err := attributevalue.UnmarshalMap(output.Item, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal item: %w", err)
	}

	return &config, nil
}

// UpdatePoolState updates the current state of the pool (e.g., running/stopped counts).
// Pool must exist in the table before calling this method.
func (c *Client) UpdatePoolState(ctx context.Context, poolName string, running, stopped int) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}
	if running < 0 || stopped < 0 {
		return fmt.Errorf("running and stopped counts must be non-negative")
	}

	key, err := attributevalue.MarshalMap(map[string]string{
		"pool_name": poolName,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	update := "SET current_running = :r, current_stopped = :s"
	exprValues, err := attributevalue.MarshalMap(map[string]int{
		":r": running,
		":s": stopped,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal values: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(c.poolsTable),
		Key:                       key,
		UpdateExpression:          aws.String(update),
		ExpressionAttributeValues: exprValues,
		ConditionExpression:       aws.String("attribute_exists(pool_name)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return fmt.Errorf("pool %s does not exist", poolName)
		}
		return fmt.Errorf("failed to update item: %w", err)
	}

	return nil
}

// MarkInstanceTerminating marks an instance as terminating in DynamoDB.
// TODO(Phase 4): Implement job-to-instance tracking for spot interruption handling.
func (c *Client) MarkInstanceTerminating(_ context.Context, instanceID string) error {
	return fmt.Errorf("MarkInstanceTerminating not yet implemented for instance %s", instanceID)
}

// GetJobByInstance retrieves job information for a given instance ID from DynamoDB.
// TODO(Phase 4): Implement job-to-instance tracking for spot interruption re-queueing.
func (c *Client) GetJobByInstance(_ context.Context, instanceID string) (*events.JobInfo, error) {
	return nil, fmt.Errorf("GetJobByInstance not yet implemented for instance %s", instanceID)
}

// MarkJobComplete marks a job as complete in DynamoDB with exit status.
func (c *Client) MarkJobComplete(ctx context.Context, jobID, status string, exitCode, duration int) error {
	if jobID == "" {
		return fmt.Errorf("job ID cannot be empty")
	}
	if status == "" {
		return fmt.Errorf("status cannot be empty")
	}

	key, err := attributevalue.MarshalMap(map[string]string{
		"job_id": jobID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	update := "SET #status = :status, exit_code = :exit_code, duration_seconds = :duration, completed_at = :completed_at"
	exprNames := map[string]string{
		"#status": "status", // status is a reserved word in DynamoDB
	}
	exprValues, err := attributevalue.MarshalMap(map[string]interface{}{
		":status":       status,
		":exit_code":    exitCode,
		":duration":     duration,
		":completed_at": time.Now().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("failed to marshal values: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(c.jobsTable),
		Key:                       key,
		UpdateExpression:          aws.String(update),
		ExpressionAttributeNames:  exprNames,
		ExpressionAttributeValues: exprValues,
		ConditionExpression:       aws.String("attribute_exists(job_id)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			// Job doesn't exist, but this is idempotent - treat as success
			return nil
		}
		return fmt.Errorf("failed to update job: %w", err)
	}

	return nil
}

// UpdateJobMetrics updates job timing metrics in DynamoDB.
func (c *Client) UpdateJobMetrics(ctx context.Context, jobID string, startedAt, completedAt time.Time) error {
	if jobID == "" {
		return fmt.Errorf("job ID cannot be empty")
	}

	key, err := attributevalue.MarshalMap(map[string]string{
		"job_id": jobID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	update := "SET started_at = :started_at, completed_at = :completed_at"
	exprValues, err := attributevalue.MarshalMap(map[string]string{
		":started_at":   startedAt.Format(time.RFC3339),
		":completed_at": completedAt.Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("failed to marshal values: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(c.jobsTable),
		Key:                       key,
		UpdateExpression:          aws.String(update),
		ExpressionAttributeValues: exprValues,
	})
	if err != nil {
		return fmt.Errorf("failed to update job metrics: %w", err)
	}

	return nil
}
