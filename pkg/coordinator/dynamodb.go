package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DynamoDBAPI defines DynamoDB operations for leader election.
type DynamoDBAPI interface {
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// Logger defines logging interface for the coordinator.
type Logger interface {
	Printf(format string, v ...interface{})
	Println(v ...interface{})
}

// lockRecord represents a leader lock in DynamoDB.
type lockRecord struct {
	LockID     string `dynamodbav:"lock_id"`
	Owner      string `dynamodbav:"owner"`
	ExpiresAt  int64  `dynamodbav:"expires_at"`
	AcquiredAt int64  `dynamodbav:"acquired_at"`
}

// DynamoDBCoordinator implements leader election using DynamoDB.
type DynamoDBCoordinator struct {
	config          Config
	dbClient        DynamoDBAPI
	logger          Logger
	isLeader        bool
	mu              sync.RWMutex
	stopCh          chan struct{}
	stoppedCh       chan struct{}
	heartbeatCancel context.CancelFunc
}

// NewDynamoDBCoordinator creates a new DynamoDB-based coordinator.
func NewDynamoDBCoordinator(cfg Config, dbClient DynamoDBAPI, logger Logger) *DynamoDBCoordinator {
	return &DynamoDBCoordinator{
		config:    cfg,
		dbClient:  dbClient,
		logger:    logger,
		isLeader:  false,
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
}

// Start begins the leader election process.
func (c *DynamoDBCoordinator) Start(ctx context.Context) error {
	c.logger.Printf("Starting coordinator: instance_id=%s, lock=%s", c.config.InstanceID, c.config.LockName)

	go c.runLeaderElection(ctx)

	return nil
}

// IsLeader returns true if the current instance is the leader.
func (c *DynamoDBCoordinator) IsLeader() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isLeader
}

// Stop gracefully shuts down the coordinator.
func (c *DynamoDBCoordinator) Stop(ctx context.Context) error {
	c.logger.Println("Stopping coordinator...")

	close(c.stopCh)

	select {
	case <-c.stoppedCh:
		c.logger.Println("Coordinator stopped")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runLeaderElection is the main loop that attempts to acquire and maintain leadership.
func (c *DynamoDBCoordinator) runLeaderElection(ctx context.Context) {
	defer close(c.stoppedCh)

	ticker := time.NewTicker(c.config.RetryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.releaseLock(ctx)
			return
		case <-c.stopCh:
			c.releaseLock(ctx)
			return
		case <-ticker.C:
			c.attemptLeadership(ctx)
		}
	}
}

// attemptLeadership attempts to acquire or renew the leader lock.
func (c *DynamoDBCoordinator) attemptLeadership(ctx context.Context) {
	currentLeader := c.IsLeader()

	if currentLeader {
		return
	}

	acquired, err := c.acquireLock(ctx)
	if err != nil {
		c.logger.Printf("Failed to acquire lock: %v", err)
		return
	}

	if acquired {
		c.logger.Printf("Acquired leadership: instance_id=%s", c.config.InstanceID)
		c.setLeader(true)
		c.startHeartbeat(ctx)
	}
}

// acquireLock attempts to acquire the leader lock.
func (c *DynamoDBCoordinator) acquireLock(ctx context.Context) (bool, error) {
	now := time.Now()
	expiresAt := now.Add(c.config.LeaseDuration)

	record := lockRecord{
		LockID:     c.config.LockName,
		Owner:      c.config.InstanceID,
		ExpiresAt:  expiresAt.Unix(),
		AcquiredAt: now.Unix(),
	}

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return false, fmt.Errorf("failed to marshal lock record: %w", err)
	}

	_, err = c.dbClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.config.LockTableName),
		Item:      item,
		ConditionExpression: aws.String(
			"attribute_not_exists(lock_id) OR expires_at < :now",
		),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Unix())},
		},
	})

	if err != nil {
		var ccfe *types.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return false, nil
		}
		return false, fmt.Errorf("failed to put lock item: %w", err)
	}

	return true, nil
}

// renewLock renews the leader lock with a new expiration time.
func (c *DynamoDBCoordinator) renewLock(ctx context.Context) error {
	now := time.Now()
	expiresAt := now.Add(c.config.LeaseDuration)

	_, err := c.dbClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.config.LockTableName),
		Key: map[string]types.AttributeValue{
			"lock_id": &types.AttributeValueMemberS{Value: c.config.LockName},
		},
		UpdateExpression: aws.String("SET expires_at = :expires_at"),
		ConditionExpression: aws.String(
			"owner = :owner AND expires_at > :now",
		),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":expires_at": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", expiresAt.Unix())},
			":owner":      &types.AttributeValueMemberS{Value: c.config.InstanceID},
			":now":        &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Unix())},
		},
	})

	if err != nil {
		var ccfe *types.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return fmt.Errorf("lock stolen or expired")
		}
		return fmt.Errorf("failed to renew lock: %w", err)
	}

	return nil
}

// releaseLock releases the leader lock.
func (c *DynamoDBCoordinator) releaseLock(_ context.Context) {
	if !c.IsLeader() {
		return
	}

	c.logger.Printf("Releasing leadership: instance_id=%s", c.config.InstanceID)

	releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.dbClient.UpdateItem(releaseCtx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.config.LockTableName),
		Key: map[string]types.AttributeValue{
			"lock_id": &types.AttributeValueMemberS{Value: c.config.LockName},
		},
		UpdateExpression: aws.String("SET expires_at = :expires_at"),
		ConditionExpression: aws.String("owner = :owner"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":expires_at": &types.AttributeValueMemberN{Value: "0"},
			":owner":      &types.AttributeValueMemberS{Value: c.config.InstanceID},
		},
	})

	if err != nil {
		c.logger.Printf("Failed to release lock: %v", err)
	}

	c.setLeader(false)
}

// startHeartbeat starts a background heartbeat to renew the lock.
func (c *DynamoDBCoordinator) startHeartbeat(ctx context.Context) {
	c.mu.Lock()
	if c.heartbeatCancel != nil {
		c.heartbeatCancel()
	}

	heartbeatCtx, cancel := context.WithCancel(ctx)
	c.heartbeatCancel = cancel
	c.mu.Unlock()

	go func() {
		ticker := time.NewTicker(c.config.HeartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-c.stopCh:
				return
			case <-ticker.C:
				if !c.IsLeader() {
					return
				}

				if err := c.renewLock(heartbeatCtx); err != nil {
					c.logger.Printf("Heartbeat failed: %v", err)
					c.setLeader(false)
					return
				}
			}
		}
	}()
}

// setLeader updates the leadership status.
func (c *DynamoDBCoordinator) setLeader(leader bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.isLeader != leader {
		if leader {
			c.logger.Printf("Became leader: instance_id=%s", c.config.InstanceID)
		} else {
			c.logger.Printf("Lost leadership: instance_id=%s", c.config.InstanceID)
			if c.heartbeatCancel != nil {
				c.heartbeatCancel()
				c.heartbeatCancel = nil
			}
		}
	}

	c.isLeader = leader
}
