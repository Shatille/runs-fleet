package db

import (
	"context"
	"errors"
	"testing"
	"time"

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
	DeleteItemFunc func(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
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

func (m *MockDynamoDBAPI) DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	if m.DeleteItemFunc != nil {
		return m.DeleteItemFunc(ctx, params, optFns...)
	}
	return &dynamodb.DeleteItemOutput{}, nil
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
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					item, err := attributevalue.MarshalMap(map[string]interface{}{
						"instance_id":   "i-1234567890abcdef0",
						"job_id":        "job-123",
						"run_id":        "run-456",
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
					return &dynamodb.GetItemOutput{Item: item}, nil
				},
			},
			wantJob: true,
			wantErr: false,
		},
		{
			name:       "Not Found",
			instanceID: "i-notfound",
			mockDB: &MockDynamoDBAPI{
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{Item: nil}, nil
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
		{
			name:       "Not Running Status",
			instanceID: "i-completed",
			mockDB: &MockDynamoDBAPI{
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					item, _ := attributevalue.MarshalMap(map[string]interface{}{
						"instance_id": "i-completed",
						"job_id":      "job-done",
						"status":      "completed",
					})
					return &dynamodb.GetItemOutput{Item: item}, nil
				},
			},
			wantJob: false,
			wantErr: false,
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

func TestListPools(t *testing.T) {
	tests := []struct {
		name       string
		poolsTable string
		mockDB     *MockDynamoDBAPI
		wantPools  []string
		wantErr    bool
	}{
		{
			name:       "Success with pools",
			poolsTable: "pools-table",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{
						Items: []map[string]types.AttributeValue{
							{"pool_name": &types.AttributeValueMemberS{Value: "pool-1"}},
							{"pool_name": &types.AttributeValueMemberS{Value: "pool-2"}},
							{"pool_name": &types.AttributeValueMemberS{Value: "pool-3"}},
						},
					}, nil
				},
			},
			wantPools: []string{"pool-1", "pool-2", "pool-3"},
			wantErr:   false,
		},
		{
			name:       "Empty pools table",
			poolsTable: "pools-table",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{}}, nil
				},
			},
			wantPools: nil,
			wantErr:   false,
		},
		{
			name:       "Pools table not configured",
			poolsTable: "",
			mockDB:     &MockDynamoDBAPI{},
			wantPools:  nil,
			wantErr:    true,
		},
		{
			name:       "DynamoDB error",
			poolsTable: "pools-table",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return nil, errors.New("dynamodb scan error")
				},
			},
			wantPools: nil,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   tt.poolsTable,
			}

			pools, err := client.ListPools(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("ListPools() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(pools) != len(tt.wantPools) {
				t.Errorf("ListPools() got %d pools, want %d", len(pools), len(tt.wantPools))
			}
		})
	}
}

func TestSavePoolConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  *PoolConfig
		mockDB  *MockDynamoDBAPI
		wantErr bool
	}{
		{
			name: "Success",
			config: &PoolConfig{
				PoolName:       "test-pool",
				InstanceType:   "t4g.medium",
				DesiredRunning: 5,
				DesiredStopped: 2,
				Environment:    "prod",
				Region:         "us-east-1",
			},
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return &dynamodb.UpdateItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:    "Nil config",
			config:  nil,
			mockDB:  &MockDynamoDBAPI{},
			wantErr: true,
		},
		{
			name: "Empty pool name",
			config: &PoolConfig{
				PoolName:     "",
				InstanceType: "t4g.medium",
			},
			mockDB:  &MockDynamoDBAPI{},
			wantErr: true,
		},
		{
			name: "DynamoDB error",
			config: &PoolConfig{
				PoolName:     "test-pool",
				InstanceType: "t4g.medium",
			},
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
			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   "pools-table",
			}

			err := client.SavePoolConfig(context.Background(), tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("SavePoolConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMarkInstanceTerminating(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		mockDB     *MockDynamoDBAPI
		wantErr    bool
	}{
		{
			name:       "Success with running job",
			instanceID: "i-1234567890abcdef0",
			mockDB: &MockDynamoDBAPI{
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					item, _ := attributevalue.MarshalMap(map[string]interface{}{
						"instance_id": "i-1234567890abcdef0",
						"job_id":      "job-123",
						"status":      "running",
					})
					return &dynamodb.GetItemOutput{Item: item}, nil
				},
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return &dynamodb.UpdateItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:       "No job found",
			instanceID: "i-notfound",
			mockDB: &MockDynamoDBAPI{
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{Item: nil}, nil
				},
			},
			wantErr: false,
		},
		{
			name:       "Empty instance ID",
			instanceID: "",
			mockDB:     &MockDynamoDBAPI{},
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    "jobs-table",
			}

			err := client.MarkInstanceTerminating(context.Background(), tt.instanceID)
			if (err != nil) != tt.wantErr {
				t.Errorf("MarkInstanceTerminating() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMarkJobComplete(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		status     string
		exitCode   int
		duration   int
		mockDB     *MockDynamoDBAPI
		wantErr    bool
	}{
		{
			name:       "Success",
			instanceID: "i-1234567890abcdef0",
			status:     "completed",
			exitCode:   0,
			duration:   120,
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					// Verify that the update expression sets the right fields
					if params.UpdateExpression == nil {
						t.Error("UpdateExpression should not be nil")
					}
					return &dynamodb.UpdateItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:       "Empty instance ID",
			instanceID: "",
			status:     "completed",
			exitCode:   0,
			duration:   120,
			mockDB:     &MockDynamoDBAPI{},
			wantErr:    true,
		},
		{
			name:       "Empty status",
			instanceID: "i-123",
			status:     "",
			exitCode:   0,
			duration:   120,
			mockDB:     &MockDynamoDBAPI{},
			wantErr:    true,
		},
		{
			name:       "DynamoDB error",
			instanceID: "i-error",
			status:     "failed",
			exitCode:   1,
			duration:   60,
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
			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    "jobs-table",
			}

			err := client.MarkJobComplete(context.Background(), tt.instanceID, tt.status, tt.exitCode, tt.duration)
			if (err != nil) != tt.wantErr {
				t.Errorf("MarkJobComplete() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestUpdateJobMetrics(t *testing.T) {
	tests := []struct {
		name        string
		instanceID  string
		startedAt   time.Time
		completedAt time.Time
		mockDB      *MockDynamoDBAPI
		wantErr     bool
	}{
		{
			name:        "Success",
			instanceID:  "i-1234567890abcdef0",
			startedAt:   time.Now().Add(-2 * time.Minute),
			completedAt: time.Now(),
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return &dynamodb.UpdateItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:        "Empty instance ID",
			instanceID:  "",
			startedAt:   time.Now().Add(-2 * time.Minute),
			completedAt: time.Now(),
			mockDB:      &MockDynamoDBAPI{},
			wantErr:     true,
		},
		{
			name:        "DynamoDB error",
			instanceID:  "i-error",
			startedAt:   time.Now().Add(-2 * time.Minute),
			completedAt: time.Now(),
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
			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    "jobs-table",
			}

			err := client.UpdateJobMetrics(context.Background(), tt.instanceID, tt.startedAt, tt.completedAt)
			if (err != nil) != tt.wantErr {
				t.Errorf("UpdateJobMetrics() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetPoolConfig_DynamoDBError(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return nil, errors.New("dynamodb error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   "pools-table",
	}

	_, err := client.GetPoolConfig(context.Background(), "test-pool")
	if err == nil {
		t.Error("GetPoolConfig() should return error when DynamoDB fails")
	}
}

func TestHasJobsTable(t *testing.T) {
	tests := []struct {
		name      string
		jobsTable string
		want      bool
	}{
		{
			name:      "returns true when jobs table is configured",
			jobsTable: "my-jobs-table",
			want:      true,
		},
		{
			name:      "returns false when jobs table is empty",
			jobsTable: "",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{
				dynamoClient: &MockDynamoDBAPI{},
				jobsTable:    tt.jobsTable,
			}
			if got := client.HasJobsTable(); got != tt.want {
				t.Errorf("HasJobsTable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetJobByInstance_NoJobsTable(t *testing.T) {
	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	_, err := client.GetJobByInstance(context.Background(), "i-123")
	if err == nil {
		t.Error("GetJobByInstance() should return error when jobs table not configured")
	}
}

func TestSaveJob_NoJobsTable(t *testing.T) {
	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	err := client.SaveJob(context.Background(), &JobRecord{
		JobID:      "job-123",
		InstanceID: "i-123",
	})
	if err == nil {
		t.Error("SaveJob() should return error when jobs table not configured")
	}
}

func TestMarkInstanceTerminating_NoJobsTable(t *testing.T) {
	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	err := client.MarkInstanceTerminating(context.Background(), "i-123")
	if err == nil {
		t.Error("MarkInstanceTerminating() should return error when jobs table not configured")
	}
}

func TestUpdatePoolState_ConditionalCheckFailed(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, &types.ConditionalCheckFailedException{Message: nil}
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   "pools-table",
	}

	err := client.UpdatePoolState(context.Background(), "nonexistent-pool", 5, 2)
	if err == nil {
		t.Error("UpdatePoolState() should return error when pool does not exist")
	}
}

func TestClient_Structure(t *testing.T) {
	mockDB := &MockDynamoDBAPI{}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   "my-pools-table",
		jobsTable:    "my-jobs-table",
	}

	if client.poolsTable != "my-pools-table" {
		t.Errorf("poolsTable = %s, want my-pools-table", client.poolsTable)
	}
	if client.jobsTable != "my-jobs-table" {
		t.Errorf("jobsTable = %s, want my-jobs-table", client.jobsTable)
	}
}

func TestJobRecord_Structure(t *testing.T) {
	job := JobRecord{
		JobID:        "job-123",
		RunID:        "run-456",
		Repo:         "owner/repo",
		InstanceID:   "i-abc123",
		InstanceType: "t4g.medium",
		Pool:         "default",
		Private:      true,
		Spot:         false,
		RunnerSpec:   "2cpu-linux-arm64",
		RetryCount:   2,
	}

	if job.JobID != "job-123" {
		t.Errorf("JobID = %s, want job-123", job.JobID)
	}
	if job.RunID != "run-456" {
		t.Errorf("RunID = %s, want run-456", job.RunID)
	}
	if job.Repo != "owner/repo" {
		t.Errorf("Repo = %s, want owner/repo", job.Repo)
	}
	if job.InstanceID != "i-abc123" {
		t.Errorf("InstanceID = %s, want i-abc123", job.InstanceID)
	}
	if job.InstanceType != "t4g.medium" {
		t.Errorf("InstanceType = %s, want t4g.medium", job.InstanceType)
	}
	if job.Pool != "default" {
		t.Errorf("Pool = %s, want default", job.Pool)
	}
	if !job.Private {
		t.Error("Private should be true")
	}
	if job.Spot {
		t.Error("Spot should be false")
	}
	if job.RunnerSpec != "2cpu-linux-arm64" {
		t.Errorf("RunnerSpec = %s, want 2cpu-linux-arm64", job.RunnerSpec)
	}
	if job.RetryCount != 2 {
		t.Errorf("RetryCount = %d, want 2", job.RetryCount)
	}
}

func TestPoolConfig_Structure(t *testing.T) {
	cfg := PoolConfig{
		PoolName:           "test-pool",
		InstanceType:       "c6g.xlarge",
		DesiredRunning:     10,
		DesiredStopped:     5,
		IdleTimeoutMinutes: 30,
		Environment:        "production",
		Region:             "us-west-2",
	}

	if cfg.PoolName != "test-pool" {
		t.Errorf("PoolName = %s, want test-pool", cfg.PoolName)
	}
	if cfg.InstanceType != "c6g.xlarge" {
		t.Errorf("InstanceType = %s, want c6g.xlarge", cfg.InstanceType)
	}
	if cfg.DesiredRunning != 10 {
		t.Errorf("DesiredRunning = %d, want 10", cfg.DesiredRunning)
	}
	if cfg.DesiredStopped != 5 {
		t.Errorf("DesiredStopped = %d, want 5", cfg.DesiredStopped)
	}
	if cfg.IdleTimeoutMinutes != 30 {
		t.Errorf("IdleTimeoutMinutes = %d, want 30", cfg.IdleTimeoutMinutes)
	}
	if cfg.Environment != "production" {
		t.Errorf("Environment = %s, want production", cfg.Environment)
	}
	if cfg.Region != "us-west-2" {
		t.Errorf("Region = %s, want us-west-2", cfg.Region)
	}
}


func TestGetJobByInstance_DynamoDBError(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return nil, errors.New("dynamodb error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	_, err := client.GetJobByInstance(context.Background(), "i-123")
	if err == nil {
		t.Error("GetJobByInstance() should return error when DynamoDB fails")
	}
}

func TestSaveJob_DynamoDBError(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		PutItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return nil, errors.New("dynamodb error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	err := client.SaveJob(context.Background(), &JobRecord{
		JobID:      "job-123",
		InstanceID: "i-123",
	})
	if err == nil {
		t.Error("SaveJob() should return error when DynamoDB fails")
	}
}

func TestMockDynamoDBAPI_DefaultBehavior(t *testing.T) {
	mock := &MockDynamoDBAPI{}

	// Test default GetItem returns empty output
	getOutput, err := mock.GetItem(context.Background(), &dynamodb.GetItemInput{})
	if err != nil {
		t.Errorf("GetItem() unexpected error: %v", err)
	}
	if getOutput == nil {
		t.Error("GetItem() should return non-nil output")
	}

	// Test default UpdateItem returns empty output
	updateOutput, err := mock.UpdateItem(context.Background(), &dynamodb.UpdateItemInput{})
	if err != nil {
		t.Errorf("UpdateItem() unexpected error: %v", err)
	}
	if updateOutput == nil {
		t.Error("UpdateItem() should return non-nil output")
	}

	// Test default Scan returns empty output
	scanOutput, err := mock.Scan(context.Background(), &dynamodb.ScanInput{})
	if err != nil {
		t.Errorf("Scan() unexpected error: %v", err)
	}
	if scanOutput == nil {
		t.Error("Scan() should return non-nil output")
	}

	// Test default Query returns empty output
	queryOutput, err := mock.Query(context.Background(), &dynamodb.QueryInput{})
	if err != nil {
		t.Errorf("Query() unexpected error: %v", err)
	}
	if queryOutput == nil {
		t.Error("Query() should return non-nil output")
	}

	// Test default PutItem returns empty output
	putOutput, err := mock.PutItem(context.Background(), &dynamodb.PutItemInput{})
	if err != nil {
		t.Errorf("PutItem() unexpected error: %v", err)
	}
	if putOutput == nil {
		t.Error("PutItem() should return non-nil output")
	}
}

func TestUpdatePoolState_DynamoDBError(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, errors.New("dynamodb error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   "pools-table",
	}

	err := client.UpdatePoolState(context.Background(), "test-pool", 5, 2)
	if err == nil {
		t.Error("UpdatePoolState() should return error when DynamoDB fails")
	}
}

func TestMarkJobRequeued_Success(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, input *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			// Verify the condition expression checks for "running" status
			if input.ConditionExpression == nil || *input.ConditionExpression != "#status = :current_status" {
				t.Errorf("ConditionExpression = %v, want #status = :current_status", input.ConditionExpression)
			}
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	marked, err := client.MarkJobRequeued(context.Background(), "i-123")
	if err != nil {
		t.Errorf("MarkJobRequeued() unexpected error: %v", err)
	}
	if !marked {
		t.Error("MarkJobRequeued() should return true on success")
	}
}

func TestMarkJobRequeued_AlreadyHandled(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, &types.ConditionalCheckFailedException{Message: nil}
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	marked, err := client.MarkJobRequeued(context.Background(), "i-123")
	if err != nil {
		t.Errorf("MarkJobRequeued() unexpected error: %v", err)
	}
	if marked {
		t.Error("MarkJobRequeued() should return false when job not in running state")
	}
}

func TestMarkJobRequeued_EmptyInstanceID(t *testing.T) {
	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "jobs-table",
	}

	_, err := client.MarkJobRequeued(context.Background(), "")
	if err == nil {
		t.Error("MarkJobRequeued() should return error for empty instance ID")
	}
}

func TestMarkJobRequeued_NoJobsTable(t *testing.T) {
	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	_, err := client.MarkJobRequeued(context.Background(), "i-123")
	if err == nil {
		t.Error("MarkJobRequeued() should return error when jobs table not configured")
	}
}

func TestMarkJobRequeued_DynamoDBError(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, errors.New("dynamodb error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	_, err := client.MarkJobRequeued(context.Background(), "i-123")
	if err == nil {
		t.Error("MarkJobRequeued() should return error when DynamoDB fails")
	}
}

func TestClaimJob_Success(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		PutItemFunc: func(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			if params.ConditionExpression == nil || *params.ConditionExpression != "attribute_not_exists(instance_id)" {
				t.Error("ClaimJob() should use conditional expression")
			}
			return &dynamodb.PutItemOutput{}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	err := client.ClaimJob(context.Background(), "job-123")
	if err != nil {
		t.Errorf("ClaimJob() unexpected error: %v", err)
	}
}

func TestClaimJob_AlreadyClaimed(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		PutItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return nil, &types.ConditionalCheckFailedException{Message: nil}
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	err := client.ClaimJob(context.Background(), "job-123")
	if !errors.Is(err, ErrJobAlreadyClaimed) {
		t.Errorf("ClaimJob() expected ErrJobAlreadyClaimed, got: %v", err)
	}
}

func TestClaimJob_EmptyJobID(t *testing.T) {
	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "jobs-table",
	}

	err := client.ClaimJob(context.Background(), "")
	if err == nil {
		t.Error("ClaimJob() should return error for empty job ID")
	}
}

func TestClaimJob_NoJobsTable(t *testing.T) {
	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	err := client.ClaimJob(context.Background(), "job-123")
	if err == nil {
		t.Error("ClaimJob() should return error when jobs table not configured")
	}
}

func TestClaimJob_DynamoDBError(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		PutItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return nil, errors.New("dynamodb error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	err := client.ClaimJob(context.Background(), "job-123")
	if err == nil {
		t.Error("ClaimJob() should return error when DynamoDB fails")
	}
	if errors.Is(err, ErrJobAlreadyClaimed) {
		t.Error("ClaimJob() should not return ErrJobAlreadyClaimed for generic DynamoDB errors")
	}
}

func TestDeleteJobClaim_Success(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		DeleteItemFunc: func(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
			return &dynamodb.DeleteItemOutput{}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	err := client.DeleteJobClaim(context.Background(), "job-123")
	if err != nil {
		t.Errorf("DeleteJobClaim() unexpected error: %v", err)
	}
}

func TestDeleteJobClaim_EmptyJobID(t *testing.T) {
	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "jobs-table",
	}

	err := client.DeleteJobClaim(context.Background(), "")
	if err == nil {
		t.Error("DeleteJobClaim() should return error for empty job ID")
	}
}

func TestDeleteJobClaim_NoJobsTable(t *testing.T) {
	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	err := client.DeleteJobClaim(context.Background(), "job-123")
	if err == nil {
		t.Error("DeleteJobClaim() should return error when jobs table not configured")
	}
}

func TestDeleteJobClaim_DynamoDBError(t *testing.T) {
	mockDB := &MockDynamoDBAPI{
		DeleteItemFunc: func(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
			return nil, errors.New("dynamodb error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	err := client.DeleteJobClaim(context.Background(), "job-123")
	if err == nil {
		t.Error("DeleteJobClaim() should return error when DynamoDB fails")
	}
}
