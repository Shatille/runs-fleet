package db

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// MockDynamoDBAPI implements DynamoDBAPI interface.
//
//nolint:dupl // Mock mirrors interface - intentional pattern
type MockDynamoDBAPI struct {
	GetItemFunc    func(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItemFunc func(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	ScanFunc       func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	QueryFunc      func(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	PutItemFunc    func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
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

func (m *MockDynamoDBAPI) Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if m.QueryFunc != nil {
		return m.QueryFunc(ctx, params, optFns...)
	}
	return &dynamodb.QueryOutput{}, nil
}

func (m *MockDynamoDBAPI) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if m.PutItemFunc != nil {
		return m.PutItemFunc(ctx, params, optFns...)
	}
	return &dynamodb.PutItemOutput{}, nil
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

func TestGetJobByInstance(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		mockDB     *MockDynamoDBAPI
		wantJob    bool
		wantErr    bool
	}{
		{
			name:       "Found",
			instanceID: "i-1234567890abcdef0",
			mockDB: &MockDynamoDBAPI{
				QueryFunc: func(_ context.Context, params *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
					// Verify correct index is used
					if params.IndexName == nil || *params.IndexName != "jobs-by-instance" {
						t.Errorf("Expected index 'jobs-by-instance', got %v", params.IndexName)
					}
					item, err := attributevalue.MarshalMap(map[string]interface{}{
						"job_id":        "job-123",
						"run_id":        "run-456",
						"instance_id":   "i-1234567890abcdef0",
						"instance_type": "t4g.medium",
						"pool":          "default",
						"private":       false,
						"spot":          true,
						"runner_spec":   "2cpu-linux-arm64",
						"retry_count":   0,
						"status":        "running",
					})
					if err != nil {
						t.Fatalf("Failed to marshal item: %v", err)
					}
					return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{item}}, nil
				},
			},
			wantJob: true,
			wantErr: false,
		},
		{
			name:       "Not Found",
			instanceID: "i-notfound",
			mockDB: &MockDynamoDBAPI{
				QueryFunc: func(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
					return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{}}, nil
				},
			},
			wantJob: false,
			wantErr: false,
		},
		{
			name:       "Empty Instance ID",
			instanceID: "",
			mockDB:     &MockDynamoDBAPI{},
			wantJob:    false,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    "jobs-table",
			}

			job, err := client.GetJobByInstance(context.Background(), tt.instanceID)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetJobByInstance() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantJob {
				if job == nil {
					t.Errorf("GetJobByInstance() = nil, want job")
				} else {
					if job.JobID != "job-123" {
						t.Errorf("JobID = %v, want job-123", job.JobID)
					}
					if job.RunID != "run-456" {
						t.Errorf("RunID = %v, want run-456", job.RunID)
					}
					if job.InstanceType != "t4g.medium" {
						t.Errorf("InstanceType = %v, want t4g.medium", job.InstanceType)
					}
				}
			} else if job != nil && !tt.wantErr {
				t.Errorf("GetJobByInstance() = %v, want nil", job)
			}
		})
	}
}

func TestSaveJob(t *testing.T) {
	tests := []struct {
		name    string
		job     *JobRecord
		mockDB  *MockDynamoDBAPI
		wantErr bool
	}{
		{
			name: "Success",
			job: &JobRecord{
				JobID:        "job-123",
				RunID:        "run-456",
				InstanceID:   "i-1234567890abcdef0",
				InstanceType: "t4g.medium",
				Pool:         "default",
				Private:      false,
				Spot:         true,
				RunnerSpec:   "2cpu-linux-arm64",
				RetryCount:   0,
			},
			mockDB: &MockDynamoDBAPI{
				PutItemFunc: func(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
					// Verify the item has required fields
					if params.Item == nil {
						t.Error("Expected item to be set")
					}
					return &dynamodb.PutItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:    "Nil Job",
			job:     nil,
			mockDB:  &MockDynamoDBAPI{},
			wantErr: true,
		},
		{
			name: "Empty Job ID",
			job: &JobRecord{
				JobID:      "",
				InstanceID: "i-123",
			},
			mockDB:  &MockDynamoDBAPI{},
			wantErr: true,
		},
		{
			name: "Empty Instance ID",
			job: &JobRecord{
				JobID:      "job-123",
				InstanceID: "",
			},
			mockDB:  &MockDynamoDBAPI{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    "jobs-table",
			}

			err := client.SaveJob(context.Background(), tt.job)
			if (err != nil) != tt.wantErr {
				t.Errorf("SaveJob() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
