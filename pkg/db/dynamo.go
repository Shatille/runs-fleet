// Package db provides DynamoDB client operations for pool configuration and state management.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var dbLog = logging.WithComponent(logging.LogTypeDB, "dynamo")

// ErrJobAlreadyClaimed is returned when attempting to claim a job that is already being processed.
var ErrJobAlreadyClaimed = errors.New("job already claimed")

// ErrInstanceAlreadyClaimed is returned when attempting to claim an instance that is already assigned to a job.
var ErrInstanceAlreadyClaimed = errors.New("instance already claimed by another job")

// DynamoDBAPI defines DynamoDB operations for pool configuration storage.
//
//nolint:dupl // Mock struct in test file mirrors this interface - intentional pattern
type DynamoDBAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
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

// HasJobsTable returns true if the jobs table is configured.
// Use this to check before calling SaveJob or other job-related methods.
func (c *Client) HasJobsTable() bool {
	return c.jobsTable != ""
}

// jobRecord represents a job stored in DynamoDB.
// Primary key is instance_id (one job per instance, ephemeral runners).
type jobRecord struct {
	InstanceID   string `dynamodbav:"instance_id"`
	JobID        int64  `dynamodbav:"job_id"`
	RunID        int64  `dynamodbav:"run_id"`
	Repo         string `dynamodbav:"repo"`
	InstanceType string `dynamodbav:"instance_type"`
	Pool         string `dynamodbav:"pool"`
	Spot         bool   `dynamodbav:"spot"`
	RetryCount   int    `dynamodbav:"retry_count"`
	WarmPoolHit  bool   `dynamodbav:"warm_pool_hit"`
	Status       string `dynamodbav:"status"`
	CreatedAt    string `dynamodbav:"created_at"`
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
			// Skip task lock entries stored in the same table
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

// ErrPoolAlreadyExists is returned when trying to create an ephemeral pool that already exists.
var ErrPoolAlreadyExists = errors.New("pool already exists")

// ErrPoolReconcileLockHeld is returned when attempting to acquire a reconciliation lock
// that is already held by another instance.
var ErrPoolReconcileLockHeld = errors.New("pool reconciliation lock held by another instance")

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

// ErrPoolNotFound is returned when attempting to acquire a lock on a non-existent pool.
var ErrPoolNotFound = errors.New("pool not found")

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

	// First check if pool exists to provide clear error messages
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
			// Lock not held by us or already released - this is fine
			return nil
		}
		return fmt.Errorf("failed to release pool reconcile lock: %w", err)
	}

	return nil
}

// ErrTaskLockHeld is returned when attempting to acquire a task lock
// that is already held by another instance.
var ErrTaskLockHeld = errors.New("task lock held by another instance")

// taskLockPrefix is the key prefix for housekeeping task locks stored in the pools table.
const taskLockPrefix = "__task_lock:"

// taskLockKey returns the DynamoDB key for a housekeeping task lock.
func taskLockKey(taskType string) string {
	return taskLockPrefix + taskType
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
			// Lock not held by us or already released - this is fine
			return nil
		}
		return fmt.Errorf("failed to release task lock: %w", err)
	}

	return nil
}

// JobHistoryEntry represents a job with timing information for auto-scaling calculations.
type JobHistoryEntry struct {
	JobID       int64
	Pool        string
	CreatedAt   time.Time
	CompletedAt time.Time // Zero value if still running
}

// QueryPoolJobHistory retrieves recent jobs for a pool within the specified time window.
//
// Performance note: Uses Scan with filter. Acceptable for current scale (~100 jobs/day).
// For high-volume deployments, add GSI on (pool, created_at) for efficient queries.
func (c *Client) QueryPoolJobHistory(ctx context.Context, poolName string, since time.Time) ([]JobHistoryEntry, error) {
	if poolName == "" {
		return nil, fmt.Errorf("pool name cannot be empty")
	}

	if c.jobsTable == "" {
		return nil, fmt.Errorf("jobs table not configured")
	}

	sinceStr := since.Format(time.RFC3339)

	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("#pool = :pool AND created_at >= :since"),
		ExpressionAttributeNames: map[string]string{
			"#pool": "pool",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pool":  &types.AttributeValueMemberS{Value: poolName},
			":since": &types.AttributeValueMemberS{Value: sinceStr},
		},
	}

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to scan jobs: %w", err)
	}

	var entries []JobHistoryEntry
	for _, item := range output.Items {
		var record struct {
			JobID       int64  `dynamodbav:"job_id"`
			Pool        string `dynamodbav:"pool"`
			CreatedAt   string `dynamodbav:"created_at"`
			CompletedAt string `dynamodbav:"completed_at"`
		}
		if err := attributevalue.UnmarshalMap(item, &record); err != nil {
			continue // Skip malformed records
		}

		entry := JobHistoryEntry{
			JobID: record.JobID,
			Pool:  record.Pool,
		}

		if record.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, record.CreatedAt); err == nil {
				entry.CreatedAt = t
			}
		}
		if record.CompletedAt != "" {
			if t, err := time.Parse(time.RFC3339, record.CompletedAt); err == nil {
				entry.CompletedAt = t
			}
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// GetPoolPeakConcurrency calculates the maximum number of concurrent jobs
// for a pool within the specified time window (in hours).
// Returns 0 if no jobs found or on error.
func (c *Client) GetPoolPeakConcurrency(ctx context.Context, poolName string, windowHours int) (int, error) {
	if windowHours <= 0 {
		windowHours = 1
	}

	since := time.Now().Add(-time.Duration(windowHours) * time.Hour)
	jobs, err := c.QueryPoolJobHistory(ctx, poolName, since)
	if err != nil {
		return 0, err
	}

	if len(jobs) == 0 {
		return 0, nil
	}

	// Calculate peak concurrency using event-based algorithm
	// Create events for job starts and ends, then sweep through
	type event struct {
		time  time.Time
		delta int // +1 for start, -1 for end
	}

	var events []event
	now := time.Now()

	for _, job := range jobs {
		if job.CreatedAt.IsZero() {
			continue
		}
		events = append(events, event{time: job.CreatedAt, delta: 1})

		endTime := job.CompletedAt
		if endTime.IsZero() {
			endTime = now // Job still running
		}
		events = append(events, event{time: endTime, delta: -1})
	}

	// Sort events by time
	sort.Slice(events, func(i, j int) bool {
		if events[i].time.Equal(events[j].time) {
			// Process starts before ends at the same time
			return events[i].delta > events[j].delta
		}
		return events[i].time.Before(events[j].time)
	})

	// Sweep through events to find peak
	var current, peak int
	for _, e := range events {
		current += e.delta
		if current > peak {
			peak = current
		}
	}

	return peak, nil
}

// GetPoolRunningJobCount returns the number of jobs currently running in a pool.
// Uses Scan with filter on pool and status fields.
//
// Performance note: Scan is acceptable for current scale (~100 jobs/day, ~10 concurrent).
// For high-volume deployments (>1000 concurrent jobs), add GSI on (pool, status) in
// Terraform and switch to Query. See: github.com/Shatille/runs-fleet/issues/TBD
func (c *Client) GetPoolRunningJobCount(ctx context.Context, poolName string) (int, error) {
	if poolName == "" {
		return 0, fmt.Errorf("pool name cannot be empty")
	}

	if c.jobsTable == "" {
		return 0, nil // No jobs table configured, return 0
	}

	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("#pool = :pool AND #status = :status"),
		ExpressionAttributeNames: map[string]string{
			"#pool":   "pool",
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pool":   &types.AttributeValueMemberS{Value: poolName},
			":status": &types.AttributeValueMemberS{Value: "running"},
		},
		Select: types.SelectCount,
	}

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return 0, fmt.Errorf("failed to scan jobs: %w", err)
	}

	return int(output.Count), nil
}

// GetPoolBusyInstanceIDs returns instance IDs that have running jobs in the pool.
// Used to identify which instances should not be stopped during reconciliation.
//
// Performance note: Uses Scan (same as GetPoolRunningJobCount). Acceptable for
// current scale; requires GSI for high-volume deployments.
func (c *Client) GetPoolBusyInstanceIDs(ctx context.Context, poolName string) ([]string, error) {
	if poolName == "" {
		return nil, fmt.Errorf("pool name cannot be empty")
	}

	if c.jobsTable == "" {
		return nil, nil // No jobs table configured
	}

	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("#pool = :pool AND #status = :status"),
		ExpressionAttributeNames: map[string]string{
			"#pool":   "pool",
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pool":   &types.AttributeValueMemberS{Value: poolName},
			":status": &types.AttributeValueMemberS{Value: "running"},
		},
		ProjectionExpression: aws.String("instance_id"),
	}

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to scan jobs: %w", err)
	}

	var instanceIDs []string
	for _, item := range output.Items {
		if v, ok := item["instance_id"]; ok {
			if s, ok := v.(*types.AttributeValueMemberS); ok && s.Value != "" {
				instanceIDs = append(instanceIDs, s.Value)
			}
		}
	}

	return instanceIDs, nil
}

// MarkInstanceTerminating marks jobs on an instance as terminating in DynamoDB.
// Updates any running jobs for this instance to status "terminating".
func (c *Client) MarkInstanceTerminating(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("instance ID cannot be empty")
	}

	if c.jobsTable == "" {
		return fmt.Errorf("jobs table not configured")
	}

	// Find running job for this instance
	job, err := c.GetJobByInstance(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get job for instance: %w", err)
	}
	if job == nil {
		return nil // No running job on this instance
	}

	// Update job status to terminating using job_id as primary key
	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": job.JobID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(c.jobsTable),
		Key:              key,
		UpdateExpression: aws.String("SET #status = :status"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":status": &types.AttributeValueMemberS{Value: "terminating"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to mark job as terminating: %w", err)
	}

	return nil
}

// GetJobByInstance retrieves job information for a given instance ID from DynamoDB.
// Uses Scan with filter since job_id is the primary key, not instance_id.
func (c *Client) GetJobByInstance(ctx context.Context, instanceID string) (*events.JobInfo, error) {
	if instanceID == "" {
		return nil, fmt.Errorf("instance ID cannot be empty")
	}

	if c.jobsTable == "" {
		return nil, fmt.Errorf("jobs table not configured")
	}

	// Scan for running job with this instance_id (job_id is the primary key)
	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("instance_id = :instance_id AND #status = :status"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":instance_id": &types.AttributeValueMemberS{Value: instanceID},
			":status":      &types.AttributeValueMemberS{Value: "running"},
		},
		Limit: aws.Int32(1),
	}

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to scan for job by instance: %w", err)
	}

	if len(output.Items) == 0 {
		return nil, nil // No running job found for this instance
	}

	var record jobRecord
	if err := attributevalue.UnmarshalMap(output.Items[0], &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal job record: %w", err)
	}

	return &events.JobInfo{
		JobID:        record.JobID,
		RunID:        record.RunID,
		Repo:         record.Repo,
		InstanceType: record.InstanceType,
		Pool:         record.Pool,
		Spot:         record.Spot,
		RetryCount:   record.RetryCount,
	}, nil
}

// JobRecord contains job information for storage.
type JobRecord struct {
	JobID        int64
	RunID        int64
	Repo         string
	InstanceID   string
	InstanceType string
	Pool         string
	Spot         bool
	RetryCount   int
	WarmPoolHit  bool
}

// SaveJob creates or updates a job record in DynamoDB.
// The job is stored with status "running" and can be queried by instance_id via the GSI.
func (c *Client) SaveJob(ctx context.Context, job *JobRecord) error {
	if job == nil {
		return fmt.Errorf("job record cannot be nil")
	}
	if job.JobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}
	if job.InstanceID == "" {
		return fmt.Errorf("instance ID cannot be empty")
	}

	if c.jobsTable == "" {
		return fmt.Errorf("jobs table not configured")
	}

	record := jobRecord{
		InstanceID:   job.InstanceID,
		JobID:        job.JobID,
		RunID:        job.RunID,
		Repo:         job.Repo,
		InstanceType: job.InstanceType,
		Pool:         job.Pool,
		Spot:         job.Spot,
		RetryCount:   job.RetryCount,
		WarmPoolHit:  job.WarmPoolHit,
		Status:       "running",
		CreatedAt:    time.Now().Format(time.RFC3339),
	}

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("failed to marshal job record: %w", err)
	}

	_, err = c.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.jobsTable),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("failed to save job record: %w", err)
	}

	return nil
}

// ClaimJob atomically claims a job for processing using conditional write.
// Returns ErrJobAlreadyClaimed if the job is already being processed.
// Uses job_id as primary key to track claims.
func (c *Client) ClaimJob(ctx context.Context, jobID int64) error {
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}

	if c.jobsTable == "" {
		return fmt.Errorf("jobs table not configured")
	}

	record := jobRecord{
		JobID:     jobID,
		Status:    "claiming",
		CreatedAt: time.Now().Format(time.RFC3339),
	}

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("failed to marshal claim record: %w", err)
	}

	_, err = c.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(c.jobsTable),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(job_id)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrJobAlreadyClaimed
		}
		return fmt.Errorf("failed to claim job: %w", err)
	}

	return nil
}

// DeleteJobClaim removes a job claim record. Used for cleanup on processing failure.
func (c *Client) DeleteJobClaim(ctx context.Context, jobID int64) error {
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}

	if c.jobsTable == "" {
		return fmt.Errorf("jobs table not configured")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": jobID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	_, err = c.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.jobsTable),
		Key:       key,
	})
	if err != nil {
		return fmt.Errorf("failed to delete job claim: %w", err)
	}

	return nil
}

// MarkJobComplete marks a job as complete in DynamoDB with exit status.
// Uses job_id as primary key (Number type in DynamoDB).
func (c *Client) MarkJobComplete(ctx context.Context, jobID int64, status string, exitCode, duration int) error {
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}
	if status == "" {
		return fmt.Errorf("status cannot be empty")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
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
// Uses job_id as primary key (Number type in DynamoDB).
func (c *Client) UpdateJobMetrics(ctx context.Context, jobID int64, startedAt, completedAt time.Time) error {
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
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

// instanceClaimPrefix is the key prefix for instance claims stored in the pools table.
const instanceClaimPrefix = "__instance_claim:"

// instanceClaimKey returns the DynamoDB key for an instance claim.
func instanceClaimKey(instanceID string) string {
	return instanceClaimPrefix + instanceID
}

// ClaimInstanceForJob atomically claims an instance for a specific job.
// Returns nil if the claim was acquired successfully.
// Returns ErrInstanceAlreadyClaimed if another job has already claimed this instance.
// The claim expires after the specified TTL to handle failed assignments.
//
// This provides distributed locking across multiple orchestrator instances to prevent
// race conditions where multiple orchestrators try to assign the same warm pool instance.
func (c *Client) ClaimInstanceForJob(ctx context.Context, instanceID string, jobID int64, ttl time.Duration) error {
	if instanceID == "" {
		return fmt.Errorf("instance ID cannot be empty")
	}
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}
	if ttl <= 0 {
		return fmt.Errorf("TTL must be positive")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	now := time.Now()
	expiresAt := now.Add(ttl).Unix()
	claimKey := instanceClaimKey(instanceID)

	_, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: claimKey},
		},
		UpdateExpression: aws.String("SET job_id = :job_id, claimed_at = :claimed_at, claim_expiry = :expiry"),
		ConditionExpression: aws.String(
			"attribute_not_exists(pool_name) OR " +
				"attribute_not_exists(claim_expiry) OR " +
				"claim_expiry < :now OR " +
				"job_id = :job_id",
		),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":job_id":     &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", jobID)},
			":claimed_at": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Unix())},
			":expiry":     &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", expiresAt)},
			":now":        &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Unix())},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrInstanceAlreadyClaimed
		}
		return fmt.Errorf("failed to claim instance: %w", err)
	}

	return nil
}

// ReleaseInstanceClaim releases an instance claim for a specific job.
// Only releases if the current job is the claim owner.
// Returns nil even if the claim was already released or owned by another job.
func (c *Client) ReleaseInstanceClaim(ctx context.Context, instanceID string, jobID int64) error {
	if instanceID == "" {
		return fmt.Errorf("instance ID cannot be empty")
	}
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	claimKey := instanceClaimKey(instanceID)

	_, err := c.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: claimKey},
		},
		ConditionExpression: aws.String("attribute_exists(pool_name) AND job_id = :job_id"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":job_id": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", jobID)},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			// Claim not held by us or already released
			dbLog.Debug("instance claim not held",
				slog.String(logging.KeyInstanceID, instanceID),
				slog.Int64(logging.KeyJobID, jobID))
			return nil
		}
		return fmt.Errorf("failed to release instance claim: %w", err)
	}

	return nil
}

// MarkJobRequeued atomically marks a job as "requeued" if its current status is "running".
// Returns true if the job was successfully marked (was in "running" state).
// Returns false if the job was already in another state (terminating, requeued, etc.).
// This prevents duplicate re-queuing from concurrent webhook handlers.
func (c *Client) MarkJobRequeued(ctx context.Context, instanceID string) (bool, error) {
	if instanceID == "" {
		return false, fmt.Errorf("instance ID cannot be empty")
	}

	if c.jobsTable == "" {
		return false, fmt.Errorf("jobs table not configured")
	}

	key, err := attributevalue.MarshalMap(map[string]string{
		"instance_id": instanceID,
	})
	if err != nil {
		return false, fmt.Errorf("failed to marshal key: %w", err)
	}

	update := "SET #status = :new_status, requeued_at = :requeued_at"
	condition := "#status = :current_status"
	exprNames := map[string]string{
		"#status": "status",
	}
	exprValues, err := attributevalue.MarshalMap(map[string]interface{}{
		":new_status":     "requeued",
		":current_status": "running",
		":requeued_at":    time.Now().Format(time.RFC3339),
	})
	if err != nil {
		return false, fmt.Errorf("failed to marshal values: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(c.jobsTable),
		Key:                       key,
		UpdateExpression:          aws.String(update),
		ConditionExpression:       aws.String(condition),
		ExpressionAttributeNames:  exprNames,
		ExpressionAttributeValues: exprValues,
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return false, nil
		}
		return false, fmt.Errorf("failed to mark job requeued: %w", err)
	}

	return true, nil
}
