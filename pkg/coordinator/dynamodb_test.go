package coordinator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type mockDynamoDBClient struct {
	putItemFunc    func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	getItemFunc    func(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	updateItemFunc func(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

func (m *mockDynamoDBClient) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if m.putItemFunc != nil {
		return m.putItemFunc(ctx, params, optFns...)
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (m *mockDynamoDBClient) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if m.getItemFunc != nil {
		return m.getItemFunc(ctx, params, optFns...)
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (m *mockDynamoDBClient) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if m.updateItemFunc != nil {
		return m.updateItemFunc(ctx, params, optFns...)
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func TestDynamoDBCoordinator_AcquireLock(t *testing.T) {
	logger := &testLogger{t: t}

	t.Run("successful acquisition", func(t *testing.T) {
		mockDB := &mockDynamoDBClient{
			putItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
				return &dynamodb.PutItemOutput{}, nil
			},
		}

		cfg := Config{
			InstanceID:        "test-instance",
			LockTableName:     "test-locks",
			LockName:          "test-lock",
			LeaseDuration:     60 * time.Second,
			HeartbeatInterval: 20 * time.Second,
			RetryInterval:     30 * time.Second,
		}

		coord := NewDynamoDBCoordinator(cfg, mockDB, logger)

		acquired, err := coord.acquireLock(context.Background())
		if err != nil {
			t.Fatalf("acquireLock() error = %v", err)
		}

		if !acquired {
			t.Error("acquireLock() should return true on success")
		}
	})

	t.Run("lock already held", func(t *testing.T) {
		mockDB := &mockDynamoDBClient{
			putItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
				return nil, &types.ConditionalCheckFailedException{
					Message: aws.String("Lock already held"),
				}
			},
		}

		cfg := DefaultConfig("test-instance")
		coord := NewDynamoDBCoordinator(cfg, mockDB, logger)

		acquired, err := coord.acquireLock(context.Background())
		if err != nil {
			t.Fatalf("acquireLock() error = %v", err)
		}

		if acquired {
			t.Error("acquireLock() should return false when lock is held")
		}
	})
}

func TestDynamoDBCoordinator_RenewLock(t *testing.T) {
	logger := &testLogger{t: t}

	t.Run("successful renewal", func(t *testing.T) {
		mockDB := &mockDynamoDBClient{
			updateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
				return &dynamodb.UpdateItemOutput{}, nil
			},
		}

		cfg := DefaultConfig("test-instance")
		coord := NewDynamoDBCoordinator(cfg, mockDB, logger)
		coord.setLeader(true)

		err := coord.renewLock(context.Background())
		if err != nil {
			t.Fatalf("renewLock() error = %v", err)
		}
	})

	t.Run("lock stolen", func(t *testing.T) {
		mockDB := &mockDynamoDBClient{
			updateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
				return nil, &types.ConditionalCheckFailedException{
					Message: aws.String("Lock condition failed"),
				}
			},
		}

		cfg := DefaultConfig("test-instance")
		coord := NewDynamoDBCoordinator(cfg, mockDB, logger)
		coord.setLeader(true)

		err := coord.renewLock(context.Background())
		if err == nil {
			t.Error("renewLock() should return error when lock is stolen")
		}
	})
}

func TestDynamoDBCoordinator_IsLeader(t *testing.T) {
	logger := &testLogger{t: t}
	mockDB := &mockDynamoDBClient{}

	cfg := DefaultConfig("test-instance")
	coord := NewDynamoDBCoordinator(cfg, mockDB, logger)

	if coord.IsLeader() {
		t.Error("IsLeader() should initially return false")
	}

	coord.setLeader(true)

	if !coord.IsLeader() {
		t.Error("IsLeader() should return true after setLeader(true)")
	}

	coord.setLeader(false)

	if coord.IsLeader() {
		t.Error("IsLeader() should return false after setLeader(false)")
	}
}

func TestDynamoDBCoordinator_ReleaseLock(t *testing.T) {
	logger := &testLogger{t: t}

	updateCalled := false
	mockDB := &mockDynamoDBClient{
		updateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			updateCalled = true

			if params.ExpressionAttributeValues[":expires_at"].(*types.AttributeValueMemberN).Value != "0" {
				t.Error("releaseLock() should set expires_at to 0")
			}

			return &dynamodb.UpdateItemOutput{}, nil
		},
	}

	cfg := DefaultConfig("test-instance")
	coord := NewDynamoDBCoordinator(cfg, mockDB, logger)
	coord.setLeader(true)

	coord.releaseLock(context.Background())

	if !updateCalled {
		t.Error("releaseLock() should call UpdateItem")
	}

	if coord.IsLeader() {
		t.Error("releaseLock() should set isLeader to false")
	}
}

func TestLockRecord_Marshall(t *testing.T) {
	now := time.Now()
	record := lockRecord{
		LockID:     "test-lock",
		Owner:      "test-owner",
		ExpiresAt:  now.Unix(),
		AcquiredAt: now.Unix(),
	}

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		t.Fatalf("MarshalMap() error = %v", err)
	}

	if item["lock_id"].(*types.AttributeValueMemberS).Value != "test-lock" {
		t.Error("lock_id not marshalled correctly")
	}

	if item["owner"].(*types.AttributeValueMemberS).Value != "test-owner" {
		t.Error("owner not marshalled correctly")
	}

	if item["expires_at"].(*types.AttributeValueMemberN).Value != fmt.Sprintf("%d", now.Unix()) {
		t.Error("expires_at not marshalled correctly as number")
	}
}

func TestDynamoDBCoordinator_StartStop(t *testing.T) {
	logger := &testLogger{t: t}
	mockDB := &mockDynamoDBClient{
		putItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return &dynamodb.PutItemOutput{}, nil
		},
	}

	cfg := Config{
		InstanceID:        "test-instance",
		LockTableName:     "test-locks",
		LockName:          "test-lock",
		LeaseDuration:     1 * time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
		RetryInterval:     100 * time.Millisecond,
	}

	coord := NewDynamoDBCoordinator(cfg, mockDB, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := coord.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()

	if err := coord.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}
