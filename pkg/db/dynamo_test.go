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
	ScanFunc       func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
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

func (m *MockDynamoDBAPI) Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	if m.ScanFunc != nil {
		return m.ScanFunc(ctx, params, optFns...)
	}
	return &dynamodb.ScanOutput{}, nil
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
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
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
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{Item: nil}, nil
				},
			},
			wantConfig: nil,
			wantErr:    false,
		},
		{
			name:       "Empty Pool Name",
			poolName:   "",
			mockDB:     &MockDynamoDBAPI{},
			wantConfig: nil,
			wantErr:    true,
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
	tests := []struct {
		name     string
		poolName string
		running  int
		stopped  int
		mockDB   *MockDynamoDBAPI
		wantErr  bool
	}{
		{
			name:     "Success",
			poolName: "test-pool",
			running:  10,
			stopped:  5,
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return &dynamodb.UpdateItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:     "Empty Pool Name",
			poolName: "",
			running:  10,
			stopped:  5,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
		{
			name:     "Negative Running Count",
			poolName: "test-pool",
			running:  -1,
			stopped:  5,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
		{
			name:     "Negative Stopped Count",
			poolName: "test-pool",
			running:  10,
			stopped:  -1,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   "pools-table",
			}

			err := client.UpdatePoolState(context.Background(), tt.poolName, tt.running, tt.stopped)
			if (err != nil) != tt.wantErr {
				t.Errorf("UpdatePoolState() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
