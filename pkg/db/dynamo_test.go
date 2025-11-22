package db

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// MockDynamoDBAPI implements DynamoDBAPI interface
type MockDynamoDBAPI struct {
	GetItemFunc    func(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItemFunc func(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

func (m *MockDynamoDBAPI) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if m.GetItemFunc != nil {
		return m.GetItemFunc(ctx, params, optFns...)
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (m *MockDynamoDBAPI) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if m.UpdateItemFunc != nil {
		return m.UpdateItemFunc(ctx, params, optFns...)
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func TestGetPoolConfig(t *testing.T) {
	tests := []struct {
		name       string
		poolName   string
		mockDB     *MockDynamoDBAPI
		wantConfig *PoolConfig
		wantErr    bool
	}{
		{
			name:     "Found",
			poolName: "test-pool",
			mockDB: &MockDynamoDBAPI{
				GetItemFunc: func(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					item, err := attributevalue.MarshalMap(map[string]interface{}{
						"pool_name":       "test-pool",
						"instance_type":   "t3.micro",
						"desired_running": 5,
						"desired_stopped": 2,
					})
					if err != nil {
						t.Fatalf("Failed to marshal item: %v", err)
					}
					return &dynamodb.GetItemOutput{Item: item}, nil
				},
			},
			wantConfig: &PoolConfig{
				PoolName:       "test-pool",
				InstanceType:   "t3.micro",
				DesiredRunning: 5,
				DesiredStopped: 2,
			},
			wantErr: false,
		},
		{
			name:     "Not Found",
			poolName: "unknown-pool",
			mockDB: &MockDynamoDBAPI{
				GetItemFunc: func(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{Item: nil}, nil
				},
			},
			wantConfig: nil,
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   "pools-table",
			}

			got, err := client.GetPoolConfig(context.Background(), tt.poolName)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetPoolConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantConfig == nil {
				if got != nil {
					t.Errorf("GetPoolConfig() = %v, want nil", got)
				}
			} else {
				if got == nil {
					t.Errorf("GetPoolConfig() = nil, want %v", tt.wantConfig)
				} else {
					if got.PoolName != tt.wantConfig.PoolName {
						t.Errorf("PoolName = %v, want %v", got.PoolName, tt.wantConfig.PoolName)
					}
					if got.InstanceType != tt.wantConfig.InstanceType {
						t.Errorf("InstanceType = %v, want %v", got.InstanceType, tt.wantConfig.InstanceType)
					}
					if got.DesiredRunning != tt.wantConfig.DesiredRunning {
						t.Errorf("DesiredRunning = %v, want %v", got.DesiredRunning, tt.wantConfig.DesiredRunning)
					}
					if got.DesiredStopped != tt.wantConfig.DesiredStopped {
						t.Errorf("DesiredStopped = %v, want %v", got.DesiredStopped, tt.wantConfig.DesiredStopped)
					}
				}
			}
		})
	}
}

func TestUpdatePoolState(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			if *params.TableName != "pools-table" {
				t.Errorf("TableName = %s, want pools-table", *params.TableName)
			}
			var key map[string]string
			if err := attributevalue.UnmarshalMap(params.Key, &key); err != nil {
				t.Errorf("Failed to unmarshal key: %v", err)
			}
			if key["pool_name"] != "test-pool" {
				t.Errorf("Key pool_name = %s, want test-pool", key["pool_name"])
			}
			var values map[string]int
			if err := attributevalue.UnmarshalMap(params.ExpressionAttributeValues, &values); err != nil {
				t.Errorf("Failed to unmarshal values: %v", err)
			}
			if values[":r"] != 10 {
				t.Errorf(":r = %d, want 10", values[":r"])
			}
			if values[":s"] != 5 {
				t.Errorf(":s = %d, want 5", values[":s"])
			}
			if params.ConditionExpression == nil || *params.ConditionExpression != "attribute_exists(pool_name)" {
				t.Errorf("ConditionExpression = %v, want attribute_exists(pool_name)", params.ConditionExpression)
			}
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   "pools-table",
	}

	err := client.UpdatePoolState(context.Background(), "test-pool", 10, 5)
	if err != nil {
		t.Errorf("UpdatePoolState() error = %v, want nil", err)
	}
}
