package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ErrPoolReconcileLockHeld is returned when attempting to acquire a reconciliation lock
// that is already held by another instance.
var ErrPoolReconcileLockHeld = errors.New("pool reconciliation lock held by another instance")

// ErrPoolNotFound is returned when attempting to acquire a lock on a non-existent pool.
var ErrPoolNotFound = errors.New("pool not found")

// ErrTaskLockHeld is returned when attempting to acquire a task lock
// that is already held by another instance.
var ErrTaskLockHeld = errors.New("task lock held by another instance")

// taskLockPrefix is the key prefix for housekeeping task locks stored in the pools table.
const taskLockPrefix = "__task_lock:"

// taskLockKey returns the DynamoDB key for a housekeeping task lock.
func taskLockKey(taskType string) string {
	return taskLockPrefix + taskType
}

// AcquirePoolReconcileLock attempts to acquire a reconciliation lock for the specified pool.
// Returns nil if the lock was acquired successfully.
// Returns ErrPoolReconcileLockHeld if another instance holds the lock.
// Returns ErrPoolNotFound if the pool does not exist.
// The lock expires after the specified TTL to handle crashed instances.
//
// The owner parameter should be a unique process identifier (e.g., UUID generated at startup),
// not a persistent identifier like hostname. This ensures crashed instances cannot bypass
// the TTL safety mechanism by reusing the same owner ID.
//
// Note: This implementation uses client-side timestamps for TTL calculation. In multi-region
// deployments, ensure NTP synchronization across instances to prevent clock skew issues.
func (c *Client) AcquirePoolReconcileLock(ctx context.Context, poolName, owner string, ttl time.Duration) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}
	if owner == "" {
		return fmt.Errorf("owner cannot be empty")
	}
	if ttl <= 0 {
		return fmt.Errorf("TTL must be positive")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	pool, err := c.GetPoolConfig(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to check pool existence: %w", err)
	}
	if pool == nil {
		return ErrPoolNotFound
	}

	now := time.Now()
	expiresAt := now.Add(ttl).Unix()

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: poolName},
		},
		UpdateExpression: aws.String("SET reconcile_lock_owner = :owner, reconcile_lock_expires = :expires"),
		ConditionExpression: aws.String(
			"attribute_exists(pool_name) AND " +
				"(attribute_not_exists(reconcile_lock_expires) OR reconcile_lock_expires < :now OR reconcile_lock_owner = :owner)",
		),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":owner":   &types.AttributeValueMemberS{Value: owner},
			":expires": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", expiresAt)},
			":now":     &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Unix())},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrPoolReconcileLockHeld
		}
		return fmt.Errorf("failed to acquire pool reconcile lock: %w", err)
	}

	return nil
}

// ReleasePoolReconcileLock releases a reconciliation lock for the specified pool.
// Only releases if the current instance is the lock owner.
// Returns nil even if the lock was already released or owned by another instance.
func (c *Client) ReleasePoolReconcileLock(ctx context.Context, poolName, owner string) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}
	if owner == "" {
		return fmt.Errorf("owner cannot be empty")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	_, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: poolName},
		},
		UpdateExpression: aws.String("REMOVE reconcile_lock_owner, reconcile_lock_expires"),
		ConditionExpression: aws.String(
			"attribute_exists(pool_name) AND reconcile_lock_owner = :owner",
		),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":owner": &types.AttributeValueMemberS{Value: owner},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil
		}
		return fmt.Errorf("failed to release pool reconcile lock: %w", err)
	}

	return nil
}

// AcquireTaskLock attempts to acquire an execution lock for the specified housekeeping task.
// Returns nil if the lock was acquired successfully.
// Returns ErrTaskLockHeld if another instance holds the lock.
// The lock expires after the specified TTL to handle crashed instances.
//
// Task locks use the pools table with a special key prefix (__task_lock:) to avoid
// requiring a separate DynamoDB table.
func (c *Client) AcquireTaskLock(ctx context.Context, taskType, owner string, ttl time.Duration) error {
	if taskType == "" {
		return fmt.Errorf("task type cannot be empty")
	}
	if owner == "" {
		return fmt.Errorf("owner cannot be empty")
	}
	if ttl <= 0 {
		return fmt.Errorf("TTL must be positive")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	now := time.Now()
	expiresAt := now.Add(ttl).Unix()
	lockKey := taskLockKey(taskType)

	_, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: lockKey},
		},
		UpdateExpression: aws.String("SET lock_owner = :owner, lock_expires = :expires"),
		ConditionExpression: aws.String(
			"attribute_not_exists(pool_name) OR " +
				"attribute_not_exists(lock_expires) OR " +
				"lock_expires < :now OR " +
				"lock_owner = :owner",
		),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":owner":   &types.AttributeValueMemberS{Value: owner},
			":expires": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", expiresAt)},
			":now":     &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Unix())},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrTaskLockHeld
		}
		return fmt.Errorf("failed to acquire task lock: %w", err)
	}

	return nil
}

// ReleaseTaskLock releases an execution lock for the specified housekeeping task.
// Only releases if the current instance is the lock owner.
// Returns nil even if the lock was already released or owned by another instance.
func (c *Client) ReleaseTaskLock(ctx context.Context, taskType, owner string) error {
	if taskType == "" {
		return fmt.Errorf("task type cannot be empty")
	}
	if owner == "" {
		return fmt.Errorf("owner cannot be empty")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	lockKey := taskLockKey(taskType)

	_, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: lockKey},
		},
		UpdateExpression: aws.String("REMOVE lock_owner, lock_expires"),
		ConditionExpression: aws.String(
			"attribute_exists(pool_name) AND lock_owner = :owner",
		),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":owner": &types.AttributeValueMemberS{Value: owner},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil
		}
		return fmt.Errorf("failed to release task lock: %w", err)
	}

	return nil
}
