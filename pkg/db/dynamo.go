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
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
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

// PoolSchedule defines time-based pool sizing for cost optimization.
type PoolSchedule struct {
	Name           string `dynamodbav:"name"`
	StartHour      int    `dynamodbav:"start_hour"`      // 0-23
	EndHour        int    `dynamodbav:"end_hour"`        // 0-23
	DaysOfWeek     []int  `dynamodbav:"days_of_week"`    // 0=Sunday, 1=Monday, etc.
	DesiredRunning int    `dynamodbav:"desired_running"` // Desired running instances during this schedule
	DesiredStopped int    `dynamodbav:"desired_stopped"` // Desired stopped instances during this schedule
}

// PoolConfig represents pool configuration from DynamoDB.
type PoolConfig struct {
	PoolName           string         `dynamodbav:"pool_name"`
	InstanceType       string         `dynamodbav:"instance_type"`
	DesiredRunning     int            `dynamodbav:"desired_running"`
	DesiredStopped     int            `dynamodbav:"desired_stopped"`
	IdleTimeoutMinutes int            `dynamodbav:"idle_timeout_minutes,omitempty"`
	Schedules          []PoolSchedule `dynamodbav:"schedules,omitempty"`
	// Environment isolation
	Environment string `dynamodbav:"environment,omitempty"` // dev, staging, prod
	// Multi-region support
	Region string `dynamodbav:"region,omitempty"` // AWS region for this pool
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

// ListPools returns all pool names from DynamoDB.
func (c *Client) ListPools(ctx context.Context) ([]string, error) {
	if c.poolsTable == "" {
		return nil, fmt.Errorf("pools table not configured")
	}

	input := &dynamodb.ScanInput{
		TableName:            aws.String(c.poolsTable),
		ProjectionExpression: aws.String("pool_name"),
	}

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to scan pools table: %w", err)
	}

	var pools []string
	for _, item := range output.Items {
		if name, ok := item["pool_name"]; ok {
			var poolName string
			if err := attributevalue.Unmarshal(name, &poolName); err != nil {
				continue
			}
			pools = append(pools, poolName)
		}
	}

	return pools, nil
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

// SavePoolConfig saves or updates a pool configuration in DynamoDB.
func (c *Client) SavePoolConfig(ctx context.Context, config *PoolConfig) error {
	if config == nil {
		return fmt.Errorf("pool config cannot be nil")
	}
	if config.PoolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}

	item, err := attributevalue.MarshalMap(config)
	if err != nil {
		return fmt.Errorf("failed to marshal pool config: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: config.PoolName},
		},
		UpdateExpression: aws.String(
			"SET instance_type = :it, desired_running = :dr, desired_stopped = :ds, " +
				"idle_timeout_minutes = :itm, schedules = :sc, environment = :env, #region = :reg"),
		ExpressionAttributeNames: map[string]string{
			"#region": "region", // region is a reserved word
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":it":  item["instance_type"],
			":dr":  item["desired_running"],
			":ds":  item["desired_stopped"],
			":itm": item["idle_timeout_minutes"],
			":sc":  item["schedules"],
			":env": item["environment"],
			":reg": item["region"],
		},
	})
	if err != nil {
		return fmt.Errorf("failed to save pool config: %w", err)
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
	})
	if err != nil {
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
