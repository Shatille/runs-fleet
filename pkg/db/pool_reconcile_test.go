package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestUpdatePoolReconcileResult(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 7, 4, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name     string
		poolName string
		result   string
		table    string
		mockDB   *MockDynamoDBAPI
		wantErr  bool
	}{
		{
			name:     "Success",
			poolName: "test-pool",
			result:   "success",
			table:    testPoolsTable,
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					if params.UpdateExpression == nil {
						t.Fatal("UpdateExpression should not be nil")
					}
					expr := *params.UpdateExpression
					if !strings.Contains(expr, "last_reconcile_at") || !strings.Contains(expr, "last_reconcile_result") {
						t.Errorf("UpdateExpression = %q, want it to set last_reconcile_at and last_reconcile_result", expr)
					}
					if params.ConditionExpression == nil || !strings.Contains(*params.ConditionExpression, "attribute_exists(pool_name)") {
						t.Errorf("ConditionExpression = %v, want attribute_exists(pool_name)", params.ConditionExpression)
					}
					if _, ok := params.Key["pool_name"]; !ok {
						t.Error("Key should use pool_name")
					}
					if v, ok := params.ExpressionAttributeValues[":res"].(*types.AttributeValueMemberS); !ok || v.Value != "success" {
						t.Errorf("result value = %#v, want success", params.ExpressionAttributeValues[":res"])
					}
					return &dynamodb.UpdateItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:     "Empty pool name",
			poolName: "",
			result:   "success",
			table:    testPoolsTable,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
		{
			name:     "Pools table not configured",
			poolName: "test-pool",
			result:   "success",
			table:    "",
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
		{
			name:     "DynamoDB error",
			poolName: "test-pool",
			result:   "failed: boom",
			table:    testPoolsTable,
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return nil, errors.New("dynamodb update error")
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   tt.table,
			}

			err := client.UpdatePoolReconcileResult(context.Background(), tt.poolName, tt.result, at)
			if (err != nil) != tt.wantErr {
				t.Errorf("UpdatePoolReconcileResult() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestUpdatePoolReconcileResult_PoolDeleted(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, &types.ConditionalCheckFailedException{Message: nil}
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   testPoolsTable,
	}

	err := client.UpdatePoolReconcileResult(context.Background(), "gone-pool", "success", time.Now())
	if !errors.Is(err, ErrPoolNotFound) {
		t.Errorf("UpdatePoolReconcileResult() error = %v, want ErrPoolNotFound", err)
	}
}
