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

func TestDynamoDBCoordinator_ReleaseLock_NotLeader(t *testing.T) {
	logger := &testLogger{t: t}
	updateCalled := false
	mockDB := &mockDynamoDBClient{
		updateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			updateCalled = true
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}

	cfg := DefaultConfig("test-instance")
	coord := NewDynamoDBCoordinator(cfg, mockDB, logger)
	// isLeader is false by default

	coord.releaseLock(context.Background())

	if updateCalled {
		t.Error("releaseLock() should not call UpdateItem when not leader")
	}
}

func TestDynamoDBCoordinator_ReleaseLock_UpdateError(t *testing.T) {
	logger := &testLogger{t: t}
	mockDB := &mockDynamoDBClient{
		updateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, fmt.Errorf("network error")
		},
	}

	cfg := DefaultConfig("test-instance")
	coord := NewDynamoDBCoordinator(cfg, mockDB, logger)
	coord.setLeader(true)

	// Should not panic and should set leader to false
	coord.releaseLock(context.Background())

	if coord.IsLeader() {
		t.Error("releaseLock() should set isLeader to false even on error")
	}
}

func TestDynamoDBCoordinator_SetLeader_Transitions(t *testing.T) {
	logger := &testLogger{t: t}
	mockDB := &mockDynamoDBClient{}

	cfg := DefaultConfig("test-instance")
	coord := NewDynamoDBCoordinator(cfg, mockDB, logger)

	// Test false -> true transition
	coord.setLeader(true)
	if !coord.IsLeader() {
		t.Error("setLeader(true) should make IsLeader() return true")
	}

	// Test true -> true (no change)
	coord.setLeader(true)
	if !coord.IsLeader() {
		t.Error("setLeader(true) again should keep IsLeader() true")
	}

	// Test true -> false transition
	coord.setLeader(false)
	if coord.IsLeader() {
		t.Error("setLeader(false) should make IsLeader() return false")
	}

	// Test false -> false (no change)
	coord.setLeader(false)
	if coord.IsLeader() {
		t.Error("setLeader(false) again should keep IsLeader() false")
	}
}

func TestDynamoDBCoordinator_AcquireLock_PutError(t *testing.T) {
	logger := &testLogger{t: t}
	mockDB := &mockDynamoDBClient{
		putItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return nil, fmt.Errorf("DynamoDB service error")
		},
	}

	cfg := DefaultConfig("test-instance")
	coord := NewDynamoDBCoordinator(cfg, mockDB, logger)

	acquired, err := coord.acquireLock(context.Background())
	if err == nil {
		t.Error("acquireLock() should return error on DynamoDB error")
	}
	if acquired {
		t.Error("acquireLock() should return false on error")
	}
}

func TestDynamoDBCoordinator_RenewLock_UpdateError(t *testing.T) {
	logger := &testLogger{t: t}
	mockDB := &mockDynamoDBClient{
		updateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, fmt.Errorf("DynamoDB service error")
		},
	}

	cfg := DefaultConfig("test-instance")
	coord := NewDynamoDBCoordinator(cfg, mockDB, logger)
	coord.setLeader(true)

	err := coord.renewLock(context.Background())
	if err == nil {
		t.Error("renewLock() should return error on DynamoDB error")
	}
}

func TestNewDynamoDBCoordinator(t *testing.T) {
	logger := &testLogger{t: t}
	mockDB := &mockDynamoDBClient{}
	cfg := DefaultConfig("test-instance")

	coord := NewDynamoDBCoordinator(cfg, mockDB, logger)

	if coord == nil {
		t.Fatal("NewDynamoDBCoordinator() returned nil")
	}
	if coord.IsLeader() {
		t.Error("new coordinator should not be leader")
	}
	if coord.config.InstanceID != cfg.InstanceID {
		t.Errorf("InstanceID = %s, want %s", coord.config.InstanceID, cfg.InstanceID)
	}
}

func TestDynamoDBCoordinator_Interface(t *testing.T) {
	logger := &testLogger{t: t}
	mockDB := &mockDynamoDBClient{}
	cfg := DefaultConfig("test-instance")

	// Verify DynamoDBCoordinator implements Coordinator interface
	var _ Coordinator = NewDynamoDBCoordinator(cfg, mockDB, logger)
}

func TestLockRecord_Structure(t *testing.T) {
	record := lockRecord{
		LockID:     "test-lock",
		Owner:      "test-owner",
		ExpiresAt:  1234567890,
		AcquiredAt: 1234567800,
	}

	if record.LockID != "test-lock" {
		t.Errorf("LockID = %s, want test-lock", record.LockID)
	}
	if record.Owner != "test-owner" {
		t.Errorf("Owner = %s, want test-owner", record.Owner)
	}
	if record.ExpiresAt != 1234567890 {
		t.Errorf("ExpiresAt = %d, want 1234567890", record.ExpiresAt)
	}
	if record.AcquiredAt != 1234567800 {
		t.Errorf("AcquiredAt = %d, want 1234567800", record.AcquiredAt)
	}
}

func TestDynamoDBCoordinator_AttemptLeadership_AlreadyLeader(t *testing.T) {
	logger := &testLogger{t: t}
	putCalled := false
	mockDB := &mockDynamoDBClient{
		putItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			putCalled = true
			return &dynamodb.PutItemOutput{}, nil
		},
	}

	cfg := DefaultConfig("test-instance")
	coord := NewDynamoDBCoordinator(cfg, mockDB, logger)
	coord.setLeader(true) // Already leader

	coord.attemptLeadership(context.Background())

	if putCalled {
		t.Error("attemptLeadership() should not try to acquire lock when already leader")
	}
}

func TestDynamoDBCoordinator_AttemptLeadership_AcquireFail(t *testing.T) {
	logger := &testLogger{t: t}
	mockDB := &mockDynamoDBClient{
		putItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return nil, &types.ConditionalCheckFailedException{
				Message: aws.String("Lock held by another instance"),
			}
		},
	}

	cfg := DefaultConfig("test-instance")
	coord := NewDynamoDBCoordinator(cfg, mockDB, logger)

	coord.attemptLeadership(context.Background())

	if coord.IsLeader() {
		t.Error("attemptLeadership() should not set leader when acquisition fails")
	}
}
