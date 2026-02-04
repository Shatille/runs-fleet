package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ErrPoolAlreadyExists is returned when trying to create an ephemeral pool that already exists.
var ErrPoolAlreadyExists = errors.New("pool already exists")

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
	PoolName     string `dynamodbav:"pool_name"`
	InstanceType string `dynamodbav:"instance_type"`
	// DesiredRunning is the number of ready (idle) instances to maintain.
	// Busy instances (running jobs) are not counted toward this target.
	DesiredRunning int            `dynamodbav:"desired_running"`
	DesiredStopped int            `dynamodbav:"desired_stopped"`
	IdleTimeoutMinutes int            `dynamodbav:"idle_timeout_minutes,omitempty"`
	Schedules          []PoolSchedule `dynamodbav:"schedules,omitempty"`
	// Environment isolation
	Environment string `dynamodbav:"environment,omitempty"` // dev, staging, prod
	// Multi-region support
	Region string `dynamodbav:"region,omitempty"` // AWS region for this pool

	// Ephemeral pool support
	Ephemeral   bool      `dynamodbav:"ephemeral,omitempty"`
	LastJobTime time.Time `dynamodbav:"last_job_time,omitempty"`

	// Flexible instance spec (inherited from first job for ephemeral pools)
	Arch     string   `dynamodbav:"arch,omitempty"`     // arm64, amd64
	CPUMin   int      `dynamodbav:"cpu_min,omitempty"`  // Minimum vCPUs
	CPUMax   int      `dynamodbav:"cpu_max,omitempty"`  // Maximum vCPUs
	RAMMin   float64  `dynamodbav:"ram_min,omitempty"`  // Minimum RAM in GB
	RAMMax   float64  `dynamodbav:"ram_max,omitempty"`  // Maximum RAM in GB
	Families []string `dynamodbav:"families,omitempty"` // Instance families (e.g., c7g, m7g)
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
			if strings.HasPrefix(poolName, taskLockPrefix) {
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

// attrOrNull returns the attribute value from the map, or NULL if not present.
// This prevents nil attribute values when fields have zero values with omitempty.
func attrOrNull(m map[string]types.AttributeValue, key string) types.AttributeValue {
	if v, ok := m[key]; ok {
		return v
	}
	return &types.AttributeValueMemberNULL{Value: true}
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
				"idle_timeout_minutes = :itm, schedules = :sc, environment = :env, #region = :reg, " +
				"ephemeral = :eph, last_job_time = :ljt, arch = :arch, cpu_min = :cpumin, " +
				"cpu_max = :cpumax, ram_min = :rammin, ram_max = :rammax, families = :fam"),
		ExpressionAttributeNames: map[string]string{
			"#region": "region", // region is a reserved word
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":it":     attrOrNull(item, "instance_type"),
			":dr":     attrOrNull(item, "desired_running"),
			":ds":     attrOrNull(item, "desired_stopped"),
			":itm":    attrOrNull(item, "idle_timeout_minutes"),
			":sc":     attrOrNull(item, "schedules"),
			":env":    attrOrNull(item, "environment"),
			":reg":    attrOrNull(item, "region"),
			":eph":    attrOrNull(item, "ephemeral"),
			":ljt":    attrOrNull(item, "last_job_time"),
			":arch":   attrOrNull(item, "arch"),
			":cpumin": attrOrNull(item, "cpu_min"),
			":cpumax": attrOrNull(item, "cpu_max"),
			":rammin": attrOrNull(item, "ram_min"),
			":rammax": attrOrNull(item, "ram_max"),
			":fam":    attrOrNull(item, "families"),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to save pool config: %w", err)
	}

	return nil
}

// CreateEphemeralPool creates a new ephemeral pool only if it doesn't already exist.
// Returns ErrPoolAlreadyExists if the pool is already present.
// This prevents race conditions when multiple concurrent jobs try to create the same pool.
func (c *Client) CreateEphemeralPool(ctx context.Context, config *PoolConfig) error {
	if config == nil {
		return fmt.Errorf("pool config cannot be nil")
	}
	if config.PoolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}
	if !config.Ephemeral {
		return fmt.Errorf("CreateEphemeralPool can only be used for ephemeral pools")
	}

	item, err := attributevalue.MarshalMap(config)
	if err != nil {
		return fmt.Errorf("failed to marshal pool config: %w", err)
	}

	_, err = c.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(c.poolsTable),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(pool_name)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrPoolAlreadyExists
		}
		return fmt.Errorf("failed to create ephemeral pool: %w", err)
	}

	return nil
}

// TouchPoolActivity updates the last job time for an ephemeral pool.
// This is called when a new job is assigned to the pool to prevent premature cleanup.
func (c *Client) TouchPoolActivity(ctx context.Context, poolName string) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	now := time.Now()
	nowStr, err := attributevalue.Marshal(now)
	if err != nil {
		return fmt.Errorf("failed to marshal time: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: poolName},
		},
		UpdateExpression:          aws.String("SET last_job_time = :ljt"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":ljt": nowStr},
		ConditionExpression:       aws.String("attribute_exists(pool_name)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return fmt.Errorf("pool %s does not exist", poolName)
		}
		return fmt.Errorf("failed to update pool activity: %w", err)
	}

	return nil
}

// DeletePoolConfig removes an ephemeral pool configuration from DynamoDB.
// Only ephemeral pools can be deleted to prevent accidental deletion of persistent pools.
func (c *Client) DeletePoolConfig(ctx context.Context, poolName string) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	_, err := c.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: poolName},
		},
		// Safety: only allow deletion of ephemeral pools
		ConditionExpression: aws.String("ephemeral = :true"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":true": &types.AttributeValueMemberBOOL{Value: true},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return fmt.Errorf("pool %s is not ephemeral or does not exist", poolName)
		}
		return fmt.Errorf("failed to delete pool config: %w", err)
	}

	return nil
}
