package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

// Test constants for table names.
const testPoolsTable = "pools-table"

// Test constant for repository names.
const testRepo = "org/repo"

// Test constant for error messages.
const errPoolsTableNotConfigured = "pools table not configured"

// Test constant for condition expressions.
const condStatusRunning = "#status = :current_status"

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
	t.Parallel()

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
			t.Parallel()

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
	t.Parallel()

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
			t.Parallel()

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
	t.Parallel()

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
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					item, err := attributevalue.MarshalMap(map[string]interface{}{
						"instance_id":   "i-1234567890abcdef0",
						"job_id":        int64(12345),
						"run_id":        int64(67890),
						"instance_type": "t4g.medium",
						"pool":          "default",
						"spot":          true,
						"retry_count":   0,
						"status":        string(JobStatusRunning),
					})
					if err != nil {
						t.Fatalf("Failed to marshal item: %v", err)
					}
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{item}}, nil
				},
			},
			wantJob: true,
			wantErr: false,
		},
		{
			name:       "Not Found",
			instanceID: "i-notfound",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{}}, nil
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
			t.Parallel()

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
					if job.JobID != 12345 {
						t.Errorf("JobID = %d, want 12345", job.JobID)
					}
					if job.RunID != 67890 {
						t.Errorf("RunID = %d, want 67890", job.RunID)
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
	t.Parallel()

	tests := []struct {
		name    string
		job     *JobRecord
		mockDB  *MockDynamoDBAPI
		wantErr bool
	}{
		{
			name: "Success",
			job: &JobRecord{
				JobID:        12345,
				RunID:        67890,
				InstanceID:   "i-1234567890abcdef0",
				InstanceType: "t4g.medium",
				Pool:         "default",
				Spot:         true,
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
			name: "Zero Job ID",
			job: &JobRecord{
				JobID:      0,
				InstanceID: "i-123",
			},
			mockDB:  &MockDynamoDBAPI{},
			wantErr: true,
		},
		{
			name: "Empty Instance ID",
			job: &JobRecord{
				JobID: 12345,
				InstanceID: "",
			},
			mockDB:  &MockDynamoDBAPI{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

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
	t.Parallel()

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
			t.Parallel()

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
	t.Parallel()

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
			t.Parallel()

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
	t.Parallel()

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
						"job_id":      int64(12345),
						"status":      string(JobStatusRunning),
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
			t.Parallel()

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
	t.Parallel()

	tests := []struct {
		name     string
		jobID    int64
		status   string
		exitCode int
		duration int
		mockDB   *MockDynamoDBAPI
		wantErr  bool
	}{
		{
			name:     "Success",
			jobID:    12345678901,
			status:   string(JobStatusCompleted),
			exitCode: 0,
			duration: 120,
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
			name:     "Zero job ID",
			jobID:    0,
			status:   string(JobStatusCompleted),
			exitCode: 0,
			duration: 120,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
		{
			name:     "Empty status",
			jobID:    123,
			status:   "",
			exitCode: 0,
			duration: 120,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
		{
			name:     "DynamoDB error",
			jobID:    999,
			status:   string(JobStatusFailed),
			exitCode: 1,
			duration: 60,
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
				jobsTable:    "jobs-table",
			}

			err := client.MarkJobComplete(context.Background(), tt.jobID, tt.status, tt.exitCode, tt.duration)
			if (err != nil) != tt.wantErr {
				t.Errorf("MarkJobComplete() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestUpdateJobMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		jobID       int64
		startedAt   time.Time
		completedAt time.Time
		mockDB      *MockDynamoDBAPI
		wantErr     bool
	}{
		{
			name:        "Success",
			jobID:       12345678901,
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
			name:        "Zero job ID",
			jobID:       0,
			startedAt:   time.Now().Add(-2 * time.Minute),
			completedAt: time.Now(),
			mockDB:      &MockDynamoDBAPI{},
			wantErr:     true,
		},
		{
			name:        "DynamoDB error",
			jobID:       999,
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
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    "jobs-table",
			}

			err := client.UpdateJobMetrics(context.Background(), tt.jobID, tt.startedAt, tt.completedAt)
			if (err != nil) != tt.wantErr {
				t.Errorf("UpdateJobMetrics() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetPoolConfig_DynamoDBError(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
			t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	err := client.SaveJob(context.Background(), &JobRecord{
		JobID: 12345,
		InstanceID: "i-123",
	})
	if err == nil {
		t.Error("SaveJob() should return error when jobs table not configured")
	}
}

func TestMarkInstanceTerminating_NoJobsTable(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	job := JobRecord{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceID:   "i-abc123",
		InstanceType: "t4g.medium",
		Pool:         "default",
		Spot:         false,
		RetryCount:   2,
	}

	if job.JobID != 12345 {
		t.Errorf("JobID = %d, want 12345", job.JobID)
	}
	if job.RunID != 67890 {
		t.Errorf("RunID = %d, want 67890", job.RunID)
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
	if job.Spot {
		t.Error("Spot should be false")
	}
	if job.RetryCount != 2 {
		t.Errorf("RetryCount = %d, want 2", job.RetryCount)
	}
}

func TestPoolConfig_Structure(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
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
	t.Parallel()

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
		JobID: 12345,
		InstanceID: "i-123",
	})
	if err == nil {
		t.Error("SaveJob() should return error when DynamoDB fails")
	}
}

func TestMockDynamoDBAPI_DefaultBehavior(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

func TestGetJobByJobID_Success(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		GetItemFunc: func(_ context.Context, params *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			// Verify key is job_id
			if _, ok := params.Key["job_id"]; !ok {
				t.Error("GetJobByJobID() should use job_id as key")
			}

			item, err := attributevalue.MarshalMap(jobRecord{
				JobID:        12345,
				RunID:        67890,
				Repo:         testRepo,
				InstanceID:   "i-abc",
				InstanceType: "c7g.xlarge",
				Pool:         "default",
				Spot:         true,
				RetryCount:   1,
				Status:       string(JobStatusRunning),
			})
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	info, err := client.GetJobByJobID(context.Background(), 12345)
	if err != nil {
		t.Fatalf("GetJobByJobID() unexpected error: %v", err)
	}
	if info == nil {
		t.Fatal("GetJobByJobID() returned nil")
	}
	if info.JobID != 12345 {
		t.Errorf("JobID = %d, want 12345", info.JobID)
	}
	if info.Repo != testRepo {
		t.Errorf("Repo = %s, want org/repo", info.Repo)
	}
	if info.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", info.RetryCount)
	}
}

func TestGetJobByJobID_NotFound(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: nil}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	info, err := client.GetJobByJobID(context.Background(), 99999)
	if err != nil {
		t.Fatalf("GetJobByJobID() unexpected error: %v", err)
	}
	if info != nil {
		t.Errorf("GetJobByJobID() = %v, want nil", info)
	}
}

func TestGetJobByJobID_ZeroID(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "jobs-table",
	}

	_, err := client.GetJobByJobID(context.Background(), 0)
	if err == nil {
		t.Error("GetJobByJobID() should return error for zero job ID")
	}
}

func TestGetJobByJobID_NoTable(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	_, err := client.GetJobByJobID(context.Background(), 12345)
	if err == nil {
		t.Error("GetJobByJobID() should return error when jobs table not configured")
	}
}

func TestMarkJobRequeuedByJobID_Success(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, input *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			// Verify key uses job_id (number type)
			if _, ok := input.Key["job_id"]; !ok {
				t.Error("MarkJobRequeuedByJobID() should use job_id as key")
			}
			if input.ConditionExpression == nil || *input.ConditionExpression != condStatusRunning {
				t.Errorf("ConditionExpression = %v, want condStatusRunning", input.ConditionExpression)
			}
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	marked, err := client.MarkJobRequeuedByJobID(context.Background(), 12345)
	if err != nil {
		t.Errorf("MarkJobRequeuedByJobID() unexpected error: %v", err)
	}
	if !marked {
		t.Error("MarkJobRequeuedByJobID() should return true on success")
	}
}

func TestMarkJobRequeuedByJobID_AlreadyHandled(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, &types.ConditionalCheckFailedException{Message: nil}
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	marked, err := client.MarkJobRequeuedByJobID(context.Background(), 12345)
	if err != nil {
		t.Errorf("MarkJobRequeuedByJobID() unexpected error: %v", err)
	}
	if marked {
		t.Error("MarkJobRequeuedByJobID() should return false when not in running state")
	}
}

func TestMarkJobRequeuedByJobID_ZeroID(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "jobs-table",
	}

	_, err := client.MarkJobRequeuedByJobID(context.Background(), 0)
	if err == nil {
		t.Error("MarkJobRequeuedByJobID() should return error for zero job ID")
	}
}

func TestMarkJobRequeuedByJobID_NoTable(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	_, err := client.MarkJobRequeuedByJobID(context.Background(), 12345)
	if err == nil {
		t.Error("MarkJobRequeuedByJobID() should return error when jobs table not configured")
	}
}

func TestMarkJobRequeuedByJobID_DynamoDBError(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, errors.New("dynamodb error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	_, err := client.MarkJobRequeuedByJobID(context.Background(), 12345)
	if err == nil {
		t.Error("MarkJobRequeuedByJobID() should return error when DynamoDB fails")
	}
}

func TestClaimJob_Success(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		PutItemFunc: func(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			if params.ConditionExpression == nil || *params.ConditionExpression != "attribute_not_exists(job_id)" {
				t.Error("ClaimJob() should use conditional expression on job_id")
			}
			return &dynamodb.PutItemOutput{}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	err := client.ClaimJob(context.Background(), 12345)
	if err != nil {
		t.Errorf("ClaimJob() unexpected error: %v", err)
	}
}

func TestClaimJob_AlreadyClaimed(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		PutItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return nil, &types.ConditionalCheckFailedException{Message: nil}
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	err := client.ClaimJob(context.Background(), 12345)
	if !errors.Is(err, ErrJobAlreadyClaimed) {
		t.Errorf("ClaimJob() expected ErrJobAlreadyClaimed, got: %v", err)
	}
}

func TestClaimJob_EmptyJobID(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "jobs-table",
	}

	err := client.ClaimJob(context.Background(), 0)
	if err == nil {
		t.Error("ClaimJob() should return error for empty job ID")
	}
}

func TestClaimJob_NoJobsTable(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	err := client.ClaimJob(context.Background(), 12345)
	if err == nil {
		t.Error("ClaimJob() should return error when jobs table not configured")
	}
}

func TestClaimJob_DynamoDBError(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		PutItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return nil, errors.New("dynamodb error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	err := client.ClaimJob(context.Background(), 12345)
	if err == nil {
		t.Error("ClaimJob() should return error when DynamoDB fails")
	}
	if errors.Is(err, ErrJobAlreadyClaimed) {
		t.Error("ClaimJob() should not return ErrJobAlreadyClaimed for generic DynamoDB errors")
	}
}

func TestDeleteJobClaim_Success(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		DeleteItemFunc: func(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
			return &dynamodb.DeleteItemOutput{}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	err := client.DeleteJobClaim(context.Background(), 12345)
	if err != nil {
		t.Errorf("DeleteJobClaim() unexpected error: %v", err)
	}
}

func TestDeleteJobClaim_EmptyJobID(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "jobs-table",
	}

	err := client.DeleteJobClaim(context.Background(), 0)
	if err == nil {
		t.Error("DeleteJobClaim() should return error for empty job ID")
	}
}

func TestDeleteJobClaim_NoJobsTable(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	err := client.DeleteJobClaim(context.Background(), 12345)
	if err == nil {
		t.Error("DeleteJobClaim() should return error when jobs table not configured")
	}
}

func TestDeleteJobClaim_DynamoDBError(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		DeleteItemFunc: func(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
			return nil, errors.New("dynamodb error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		jobsTable:    "jobs-table",
	}

	err := client.DeleteJobClaim(context.Background(), 12345)
	if err == nil {
		t.Error("DeleteJobClaim() should return error when DynamoDB fails")
	}
}

func TestTouchPoolActivity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		poolName string
		mockDB   *MockDynamoDBAPI
		noTable  bool
		wantErr  bool
	}{
		{
			name:     "Success",
			poolName: "test-pool",
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					// Verify the update expression sets last_job_time
					if params.UpdateExpression == nil || *params.UpdateExpression != "SET last_job_time = :ljt" {
						return nil, errors.New("unexpected update expression")
					}
					return &dynamodb.UpdateItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:     "Empty Pool Name",
			poolName: "",
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
		{
			name:     "No Table Configured",
			poolName: "test-pool",
			mockDB:   &MockDynamoDBAPI{},
			noTable:  true,
			wantErr:  true,
		},
		{
			name:     "Pool Not Found",
			poolName: "unknown-pool",
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return nil, &types.ConditionalCheckFailedException{}
				},
			},
			wantErr: true,
		},
		{
			name:     "DynamoDB Error",
			poolName: "test-pool",
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return nil, errors.New("dynamodb error")
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			poolsTable := testPoolsTable
			if tt.noTable {
				poolsTable = ""
			}
			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   poolsTable,
			}

			err := client.TouchPoolActivity(context.Background(), tt.poolName)
			if (err != nil) != tt.wantErr {
				t.Errorf("TouchPoolActivity() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDeletePoolConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		poolName string
		mockDB   *MockDynamoDBAPI
		noTable  bool
		wantErr  bool
	}{
		{
			name:     "Success",
			poolName: "test-pool",
			mockDB: &MockDynamoDBAPI{
				DeleteItemFunc: func(_ context.Context, params *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
					// Verify the key is correct
					if params.Key["pool_name"] == nil {
						return nil, errors.New("missing pool_name key")
					}
					return &dynamodb.DeleteItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:     "Empty Pool Name",
			poolName: "",
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
		{
			name:     "No Table Configured",
			poolName: "test-pool",
			mockDB:   &MockDynamoDBAPI{},
			noTable:  true,
			wantErr:  true,
		},
		{
			name:     "DynamoDB Error",
			poolName: "test-pool",
			mockDB: &MockDynamoDBAPI{
				DeleteItemFunc: func(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
					return nil, errors.New("dynamodb error")
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			poolsTable := testPoolsTable
			if tt.noTable {
				poolsTable = ""
			}
			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   poolsTable,
			}

			err := client.DeletePoolConfig(context.Background(), tt.poolName)
			if (err != nil) != tt.wantErr {
				t.Errorf("DeletePoolConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// createTwoJobMockScan creates a mock ScanFunc returning two job items for testing.
// job1Created/job1Completed are offsets from now for the first job.
// job2Created is offset for the second job (no completed_at - still running).
func createTwoJobMockScan(now time.Time, poolName string, job1Created, job1Completed, job2Created time.Duration) func(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	return func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
		item1, _ := attributevalue.MarshalMap(map[string]interface{}{
			"job_id":       int64(1),
			"pool":         poolName,
			"created_at":   now.Add(job1Created).Format(time.RFC3339),
			"completed_at": now.Add(job1Completed).Format(time.RFC3339),
		})
		item2, _ := attributevalue.MarshalMap(map[string]interface{}{
			"job_id":     int64(2),
			"pool":       poolName,
			"created_at": now.Add(job2Created).Format(time.RFC3339),
			// No completed_at - still running
		})
		return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{item1, item2}}, nil
	}
}

func TestQueryPoolJobHistory(t *testing.T) {
	t.Parallel()

	now := time.Now()
	since := now.Add(-1 * time.Hour)

	tests := []struct {
		name     string
		poolName string
		mockDB   *MockDynamoDBAPI
		noTable  bool
		wantLen  int
		wantErr  bool
	}{
		{
			name:     "Success with jobs",
			poolName: "test-pool",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: createTwoJobMockScan(now, "test-pool", -30*time.Minute, -20*time.Minute, -10*time.Minute),
			},
			wantLen: 2,
			wantErr: false,
		},
		{
			name:     "Empty Pool Name",
			poolName: "",
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
		{
			name:     "No Table Configured",
			poolName: "test-pool",
			mockDB:   &MockDynamoDBAPI{},
			noTable:  true,
			wantErr:  true,
		},
		{
			name:     "Empty Results",
			poolName: "empty-pool",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{}}, nil
				},
			},
			wantLen: 0,
			wantErr: false,
		},
		{
			name:     "DynamoDB Error",
			poolName: "test-pool",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return nil, errors.New("dynamodb error")
				},
			},
			wantErr: true,
		},
		{
			name:     "Excludes orphaned jobs",
			poolName: "test-pool",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, input *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					if input.FilterExpression == nil || !strings.Contains(*input.FilterExpression, "#status <> :orphaned") {
						return nil, errors.New("filter expression must exclude orphaned jobs")
					}
					if _, ok := input.ExpressionAttributeNames["#status"]; !ok {
						return nil, errors.New("ExpressionAttributeNames must include #status")
					}
					if _, ok := input.ExpressionAttributeValues[":orphaned"]; !ok {
						return nil, errors.New("ExpressionAttributeValues must include :orphaned")
					}
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{}}, nil
				},
			},
			wantLen: 0,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			jobsTable := "jobs-table"
			if tt.noTable {
				jobsTable = ""
			}
			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    jobsTable,
			}

			entries, err := client.QueryPoolJobHistory(context.Background(), tt.poolName, since)
			if (err != nil) != tt.wantErr {
				t.Errorf("QueryPoolJobHistory() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(entries) != tt.wantLen {
				t.Errorf("QueryPoolJobHistory() returned %d entries, want %d", len(entries), tt.wantLen)
			}
		})
	}
}

//nolint:dupl // P90 and peak tests intentionally use similar job scenarios with different expected results
func TestGetPoolP90Concurrency(t *testing.T) {
	t.Parallel()

	now := time.Now()

	tests := []struct {
		name     string
		poolName string
		mockDB   *MockDynamoDBAPI
		wantP90  int
		wantErr  bool
	}{
		{
			name:     "Brief spike returns lower than peak",
			poolName: "test-pool",
			mockDB: &MockDynamoDBAPI{
				// 3 overlapping jobs for only 2 minutes out of 60
				// Peak is 3, but P90 should be much lower (most samples are 0)
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					item1, _ := attributevalue.MarshalMap(map[string]interface{}{
						"job_id":       int64(1),
						"pool":         "test-pool",
						"created_at":   now.Add(-30 * time.Minute).Format(time.RFC3339),
						"completed_at": now.Add(-20 * time.Minute).Format(time.RFC3339),
					})
					item2, _ := attributevalue.MarshalMap(map[string]interface{}{
						"job_id":       int64(2),
						"pool":         "test-pool",
						"created_at":   now.Add(-25 * time.Minute).Format(time.RFC3339),
						"completed_at": now.Add(-15 * time.Minute).Format(time.RFC3339),
					})
					item3, _ := attributevalue.MarshalMap(map[string]interface{}{
						"job_id":       int64(3),
						"pool":         "test-pool",
						"created_at":   now.Add(-22 * time.Minute).Format(time.RFC3339),
						"completed_at": now.Add(-12 * time.Minute).Format(time.RFC3339),
					})
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{item1, item2, item3}}, nil
				},
			},
			wantP90: 2, // P90 is 2, even though peak is 3
			wantErr: false,
		},
		{
			name:     "Sustained concurrency returns that value",
			poolName: "test-pool",
			mockDB: &MockDynamoDBAPI{
				// 2 jobs running for most of the hour
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					item1, _ := attributevalue.MarshalMap(map[string]interface{}{
						"job_id":       int64(1),
						"pool":         "test-pool",
						"created_at":   now.Add(-55 * time.Minute).Format(time.RFC3339),
						"completed_at": now.Add(-5 * time.Minute).Format(time.RFC3339),
					})
					item2, _ := attributevalue.MarshalMap(map[string]interface{}{
						"job_id":       int64(2),
						"pool":         "test-pool",
						"created_at":   now.Add(-50 * time.Minute).Format(time.RFC3339),
						"completed_at": now.Add(-10 * time.Minute).Format(time.RFC3339),
					})
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{item1, item2}}, nil
				},
			},
			wantP90: 2, // Both jobs overlap for 40+ minutes, P90 should be 2
			wantErr: false,
		},
		{
			name:     "No jobs returns zero",
			poolName: "empty-pool",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{}}, nil
				},
			},
			wantP90: 0,
			wantErr: false,
		},
		{
			name:     "Non-overlapping jobs",
			poolName: "test-pool",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					item1, _ := attributevalue.MarshalMap(map[string]interface{}{
						"job_id":       int64(1),
						"pool":         "test-pool",
						"created_at":   now.Add(-30 * time.Minute).Format(time.RFC3339),
						"completed_at": now.Add(-25 * time.Minute).Format(time.RFC3339),
					})
					item2, _ := attributevalue.MarshalMap(map[string]interface{}{
						"job_id":       int64(2),
						"pool":         "test-pool",
						"created_at":   now.Add(-20 * time.Minute).Format(time.RFC3339),
						"completed_at": now.Add(-15 * time.Minute).Format(time.RFC3339),
					})
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{item1, item2}}, nil
				},
			},
			wantP90: 1, // ~10 minutes of activity at concurrency 1, P90 captures this
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    "jobs-table",
			}

			p90, err := client.GetPoolP90Concurrency(context.Background(), tt.poolName, 1)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetPoolP90Concurrency() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && p90 != tt.wantP90 {
				t.Errorf("GetPoolP90Concurrency() = %d, want %d", p90, tt.wantP90)
			}
		})
	}
}

func TestPoolConfig_EphemeralFields(t *testing.T) {
	t.Parallel()

	// Test that ephemeral pool fields are correctly marshaled/unmarshaled
	now := time.Now().Truncate(time.Millisecond)
	config := &PoolConfig{
		PoolName:       "ephemeral-test",
		InstanceType:   "c7g.xlarge",
		DesiredRunning: 2,
		DesiredStopped: 0,
		Ephemeral:      true,
		LastJobTime:    now,
		Arch:           "arm64",
		CPUMin:         4,
		CPUMax:         8,
		RAMMin:         8.0,
		RAMMax:         16.0,
		Families:       []string{"c7g", "m7g"},
	}

	// Marshal and unmarshal
	item, err := attributevalue.MarshalMap(config)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	var result PoolConfig
	if err := attributevalue.UnmarshalMap(item, &result); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	// Verify fields
	if !result.Ephemeral {
		t.Error("Ephemeral should be true")
	}
	if !result.LastJobTime.Equal(now) {
		t.Errorf("LastJobTime = %v, want %v", result.LastJobTime, now)
	}
	if result.Arch != "arm64" {
		t.Errorf("Arch = %s, want arm64", result.Arch)
	}
	if result.CPUMin != 4 {
		t.Errorf("CPUMin = %d, want 4", result.CPUMin)
	}
	if result.CPUMax != 8 {
		t.Errorf("CPUMax = %d, want 8", result.CPUMax)
	}
	if result.RAMMin != 8.0 {
		t.Errorf("RAMMin = %f, want 8.0", result.RAMMin)
	}
	if result.RAMMax != 16.0 {
		t.Errorf("RAMMax = %f, want 16.0", result.RAMMax)
	}
	if len(result.Families) != 2 || result.Families[0] != "c7g" || result.Families[1] != "m7g" {
		t.Errorf("Families = %v, want [c7g, m7g]", result.Families)
	}
}

func TestAcquirePoolReconcileLock(t *testing.T) {
	t.Parallel()

	// Helper to create a mock that returns a pool for GetItem
	poolExistsMock := func(updateFunc func(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)) *MockDynamoDBAPI {
		return &MockDynamoDBAPI{
			GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
				item, _ := attributevalue.MarshalMap(map[string]interface{}{
					"pool_name": "test-pool",
				})
				return &dynamodb.GetItemOutput{Item: item}, nil
			},
			UpdateItemFunc: updateFunc,
		}
	}

	tests := []struct {
		name     string
		poolName string
		owner    string
		ttl      time.Duration
		mockDB   *MockDynamoDBAPI
		wantErr  error
	}{
		{
			name:     "Success",
			poolName: "test-pool",
			owner:    "instance-1",
			ttl:      65 * time.Second,
			mockDB: poolExistsMock(func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
				if *params.TableName != "pools-table" {
					t.Errorf("TableName = %s, want pools-table", *params.TableName)
				}
				return &dynamodb.UpdateItemOutput{}, nil
			}),
			wantErr: nil,
		},
		{
			name:     "Lock Already Held",
			poolName: "test-pool",
			owner:    "instance-1",
			ttl:      65 * time.Second,
			mockDB: poolExistsMock(func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
				return nil, &types.ConditionalCheckFailedException{}
			}),
			wantErr: ErrPoolReconcileLockHeld,
		},
		{
			name:     "Pool Not Found",
			poolName: "nonexistent-pool",
			owner:    "instance-1",
			ttl:      65 * time.Second,
			mockDB: &MockDynamoDBAPI{
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{Item: nil}, nil // Pool does not exist
				},
			},
			wantErr: ErrPoolNotFound,
		},
		{
			name:     "Empty Pool Name",
			poolName: "",
			owner:    "instance-1",
			ttl:      65 * time.Second,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  errors.New("pool name cannot be empty"),
		},
		{
			name:     "Empty Owner",
			poolName: "test-pool",
			owner:    "",
			ttl:      65 * time.Second,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  errors.New("owner cannot be empty"),
		},
		{
			name:     "Zero TTL",
			poolName: "test-pool",
			owner:    "instance-1",
			ttl:      0,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  errors.New("TTL must be positive"),
		},
		{
			name:     "Negative TTL",
			poolName: "test-pool",
			owner:    "instance-1",
			ttl:      -1 * time.Second,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  errors.New("TTL must be positive"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   "pools-table",
			}

			err := client.AcquirePoolReconcileLock(context.Background(), tt.poolName, tt.owner, tt.ttl)
			if tt.wantErr != nil {
				if err == nil {
					t.Errorf("AcquirePoolReconcileLock() error = nil, wantErr %v", tt.wantErr)
					return
				}
				if !errors.Is(err, tt.wantErr) && err.Error() != tt.wantErr.Error() {
					t.Errorf("AcquirePoolReconcileLock() error = %v, wantErr %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Errorf("AcquirePoolReconcileLock() error = %v, wantErr nil", err)
			}
		})
	}
}

func TestAcquirePoolReconcileLock_ReentrantByOwner(t *testing.T) {
	t.Parallel()

	// Verify that same owner can re-acquire lock (condition includes reconcile_lock_owner = :owner)
	updateCalls := 0
	mockDB := &MockDynamoDBAPI{
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			item, _ := attributevalue.MarshalMap(map[string]interface{}{
				"pool_name": "test-pool",
			})
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			updateCalls++
			// Verify condition allows re-entry by same owner
			condition := *params.ConditionExpression
			if condition == "" {
				t.Error("ConditionExpression should not be empty")
			}
			// Condition should include owner check for re-entry
			if params.ExpressionAttributeValues[":owner"] == nil {
				t.Error("Owner should be in expression values")
			}
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   "pools-table",
	}

	// First acquisition
	err := client.AcquirePoolReconcileLock(context.Background(), "test-pool", "owner-1", 65*time.Second)
	if err != nil {
		t.Errorf("First AcquirePoolReconcileLock() error = %v", err)
	}

	// Second acquisition by same owner (re-entrant)
	err = client.AcquirePoolReconcileLock(context.Background(), "test-pool", "owner-1", 65*time.Second)
	if err != nil {
		t.Errorf("Second AcquirePoolReconcileLock() error = %v", err)
	}

	if updateCalls != 2 {
		t.Errorf("UpdateItem called %d times, want 2", updateCalls)
	}
}

func TestAcquirePoolReconcileLock_ExpiredLockCanBeAcquired(t *testing.T) {
	t.Parallel()

	// Verify that an expired lock can be acquired by a new owner
	mockDB := &MockDynamoDBAPI{
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			item, _ := attributevalue.MarshalMap(map[string]interface{}{
				"pool_name": "test-pool",
			})
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			// Verify condition includes timestamp check for expiration
			condition := *params.ConditionExpression
			if condition == "" {
				t.Error("ConditionExpression should not be empty")
			}
			// Condition should check reconcile_lock_expires < :now
			if params.ExpressionAttributeValues[":now"] == nil {
				t.Error("Now timestamp should be in expression values")
			}
			// Simulate: lock was expired so condition passes
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   "pools-table",
	}

	err := client.AcquirePoolReconcileLock(context.Background(), "test-pool", "new-owner", 65*time.Second)
	if err != nil {
		t.Errorf("AcquirePoolReconcileLock() on expired lock error = %v", err)
	}
}

func TestAcquirePoolReconcileLock_NoPoolsTable(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		poolsTable:   "",
	}

	err := client.AcquirePoolReconcileLock(context.Background(), "test-pool", "instance-1", 65*time.Second)
	if err == nil {
		t.Error("AcquirePoolReconcileLock() should fail when pools table not configured")
	}
	if err.Error() != errPoolsTableNotConfigured {
		t.Errorf("AcquirePoolReconcileLock() error = %v, want '%s'", err, errPoolsTableNotConfigured)
	}
}

func TestAcquirePoolReconcileLock_GetPoolConfigError(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return nil, errors.New("network error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   "pools-table",
	}

	err := client.AcquirePoolReconcileLock(context.Background(), "test-pool", "instance-1", 65*time.Second)
	if err == nil {
		t.Error("AcquirePoolReconcileLock() should fail on GetPoolConfig error")
	}
}

func TestAcquirePoolReconcileLock_DynamoDBError(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			item, _ := attributevalue.MarshalMap(map[string]interface{}{
				"pool_name": "test-pool",
			})
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, errors.New("network error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   "pools-table",
	}

	err := client.AcquirePoolReconcileLock(context.Background(), "test-pool", "instance-1", 65*time.Second)
	if err == nil {
		t.Error("AcquirePoolReconcileLock() should fail on DynamoDB error")
	}
	if err.Error() != "failed to acquire pool reconcile lock: network error" {
		t.Errorf("AcquirePoolReconcileLock() error = %v, want 'failed to acquire pool reconcile lock: network error'", err)
	}
}

func TestReleasePoolReconcileLock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		poolName string
		owner    string
		mockDB   *MockDynamoDBAPI
		wantErr  bool
	}{
		{
			name:     "Success",
			poolName: "test-pool",
			owner:    "instance-1",
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					if *params.TableName != "pools-table" {
						t.Errorf("TableName = %s, want pools-table", *params.TableName)
					}
					return &dynamodb.UpdateItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:     "Lock Not Held By Us",
			poolName: "test-pool",
			owner:    "instance-1",
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return nil, &types.ConditionalCheckFailedException{}
				},
			},
			wantErr: false, // Should NOT error - silently succeed
		},
		{
			name:     "Empty Pool Name",
			poolName: "",
			owner:    "instance-1",
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
		{
			name:     "Empty Owner",
			poolName: "test-pool",
			owner:    "",
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   "pools-table",
			}

			err := client.ReleasePoolReconcileLock(context.Background(), tt.poolName, tt.owner)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReleasePoolReconcileLock() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestReleasePoolReconcileLock_NoPoolsTable(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		poolsTable:   "",
	}

	err := client.ReleasePoolReconcileLock(context.Background(), "test-pool", "instance-1")
	if err == nil {
		t.Error("ReleasePoolReconcileLock() should fail when pools table not configured")
	}
	if err.Error() != errPoolsTableNotConfigured {
		t.Errorf("ReleasePoolReconcileLock() error = %v, want '%s'", err, errPoolsTableNotConfigured)
	}
}

func TestReleasePoolReconcileLock_DynamoDBError(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, errors.New("network error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   "pools-table",
	}

	err := client.ReleasePoolReconcileLock(context.Background(), "test-pool", "instance-1")
	if err == nil {
		t.Error("ReleasePoolReconcileLock() should fail on DynamoDB error")
	}
}

func TestAcquireTaskLock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		taskType string
		owner    string
		ttl      time.Duration
		mockDB   *MockDynamoDBAPI
		wantErr  error
	}{
		{
			name:     "Success",
			taskType: "orphaned_instances",
			owner:    "instance-1",
			ttl:      6 * time.Minute,
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					if *params.TableName != testPoolsTable {
						t.Errorf("TableName = %s, want %s", *params.TableName, testPoolsTable)
					}
					// Verify the key uses the task lock prefix
					if key, ok := params.Key["pool_name"].(*types.AttributeValueMemberS); ok {
						if key.Value != "__task_lock:orphaned_instances" {
							t.Errorf("Key = %s, want __task_lock:orphaned_instances", key.Value)
						}
					}
					return &dynamodb.UpdateItemOutput{}, nil
				},
			},
			wantErr: nil,
		},
		{
			name:     "Lock Already Held",
			taskType: "orphaned_instances",
			owner:    "instance-1",
			ttl:      6 * time.Minute,
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return nil, &types.ConditionalCheckFailedException{}
				},
			},
			wantErr: ErrTaskLockHeld,
		},
		{
			name:     "Empty Task Type",
			taskType: "",
			owner:    "instance-1",
			ttl:      6 * time.Minute,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  errors.New("task type cannot be empty"),
		},
		{
			name:     "Empty Owner",
			taskType: "orphaned_instances",
			owner:    "",
			ttl:      6 * time.Minute,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  errors.New("owner cannot be empty"),
		},
		{
			name:     "Invalid TTL",
			taskType: "orphaned_instances",
			owner:    "instance-1",
			ttl:      0,
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  errors.New("TTL must be positive"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   testPoolsTable,
			}
			err := client.AcquireTaskLock(context.Background(), tt.taskType, tt.owner, tt.ttl)
			if tt.wantErr != nil {
				if err == nil {
					t.Errorf("AcquireTaskLock() error = nil, wantErr %v", tt.wantErr)
					return
				}
				if !errors.Is(err, tt.wantErr) && err.Error() != tt.wantErr.Error() {
					t.Errorf("AcquireTaskLock() error = %v, wantErr %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Errorf("AcquireTaskLock() error = %v, wantErr nil", err)
			}
		})
	}
}

func TestAcquireTaskLock_ReentrantByOwner(t *testing.T) {
	t.Parallel()

	updateCalls := 0
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			updateCalls++
			// Verify condition allows re-entry by same owner
			condition := *params.ConditionExpression
			if condition == "" {
				t.Error("ConditionExpression should not be empty")
			}
			if params.ExpressionAttributeValues[":owner"] == nil {
				t.Error("Owner should be in expression values")
			}
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   testPoolsTable,
	}

	// First acquisition
	err := client.AcquireTaskLock(context.Background(), "orphaned_instances", "owner-1", 6*time.Minute)
	if err != nil {
		t.Errorf("First AcquireTaskLock() error = %v", err)
	}

	// Second acquisition by same owner (re-entrant)
	err = client.AcquireTaskLock(context.Background(), "orphaned_instances", "owner-1", 6*time.Minute)
	if err != nil {
		t.Errorf("Second AcquireTaskLock() error = %v", err)
	}

	if updateCalls != 2 {
		t.Errorf("UpdateItem called %d times, want 2", updateCalls)
	}
}

func TestAcquireTaskLock_NoPoolsTable(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		poolsTable:   "",
	}

	err := client.AcquireTaskLock(context.Background(), "orphaned_instances", "instance-1", 6*time.Minute)
	if err == nil {
		t.Error("AcquireTaskLock() should fail when pools table not configured")
	}
	if err.Error() != errPoolsTableNotConfigured {
		t.Errorf("AcquireTaskLock() error = %v, want '%s'", err, errPoolsTableNotConfigured)
	}
}

func TestAcquireTaskLock_DynamoDBError(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, errors.New("network error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   testPoolsTable,
	}

	err := client.AcquireTaskLock(context.Background(), "orphaned_instances", "instance-1", 6*time.Minute)
	if err == nil {
		t.Error("AcquireTaskLock() should fail on DynamoDB error")
	}
	if err.Error() != "failed to acquire task lock: network error" {
		t.Errorf("AcquireTaskLock() error = %v, want 'failed to acquire task lock: network error'", err)
	}
}

func TestReleaseTaskLock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		taskType string
		owner    string
		mockDB   *MockDynamoDBAPI
		wantErr  bool
	}{
		{
			name:     "Success",
			taskType: "orphaned_instances",
			owner:    "instance-1",
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					if *params.TableName != testPoolsTable {
						t.Errorf("TableName = %s, want %s", *params.TableName, testPoolsTable)
					}
					// Verify the key uses the task lock prefix
					if key, ok := params.Key["pool_name"].(*types.AttributeValueMemberS); ok {
						if key.Value != "__task_lock:orphaned_instances" {
							t.Errorf("Key = %s, want __task_lock:orphaned_instances", key.Value)
						}
					}
					return &dynamodb.UpdateItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:     "Lock Not Held By Us",
			taskType: "orphaned_instances",
			owner:    "instance-1",
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return nil, &types.ConditionalCheckFailedException{}
				},
			},
			wantErr: false, // Should NOT error - silently succeed
		},
		{
			name:     "Empty Task Type",
			taskType: "",
			owner:    "instance-1",
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
		{
			name:     "Empty Owner",
			taskType: "orphaned_instances",
			owner:    "",
			mockDB:   &MockDynamoDBAPI{},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   testPoolsTable,
			}
			err := client.ReleaseTaskLock(context.Background(), tt.taskType, tt.owner)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReleaseTaskLock() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestReleaseTaskLock_NoPoolsTable(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		poolsTable:   "",
	}

	err := client.ReleaseTaskLock(context.Background(), "orphaned_instances", "instance-1")
	if err == nil {
		t.Error("ReleaseTaskLock() should fail when pools table not configured")
	}
	if err.Error() != errPoolsTableNotConfigured {
		t.Errorf("ReleaseTaskLock() error = %v, want '%s'", err, errPoolsTableNotConfigured)
	}
}

func TestReleaseTaskLock_DynamoDBError(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, errors.New("network error")
		},
	}

	client := &Client{
		dynamoClient: mockDB,
		poolsTable:   testPoolsTable,
	}

	err := client.ReleaseTaskLock(context.Background(), "orphaned_instances", "instance-1")
	if err == nil {
		t.Error("ReleaseTaskLock() should fail on DynamoDB error")
	}
}

func TestTaskLockKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		taskType string
		want     string
	}{
		{"orphaned_instances", "__task_lock:orphaned_instances"},
		{"dlq_redrive", "__task_lock:dlq_redrive"},
		{"cost_report", "__task_lock:cost_report"},
	}

	for _, tt := range tests {
		t.Run(tt.taskType, func(t *testing.T) {
			t.Parallel()

			got := taskLockKey(tt.taskType)
			if got != tt.want {
				t.Errorf("taskLockKey(%q) = %q, want %q", tt.taskType, got, tt.want)
			}
		})
	}
}

func TestClaimInstanceForJob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		instanceID string
		jobID      int64
		ttl        time.Duration
		mockDB     *MockDynamoDBAPI
		wantErr    bool
		errType    error
	}{
		{
			name:       "Success - new claim",
			instanceID: "i-12345",
			jobID:      100,
			ttl:        5 * time.Minute,
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					if *params.TableName != testPoolsTable {
						t.Errorf("wrong table name: %s", *params.TableName)
					}
					expectedKey := "__instance_claim:i-12345"
					if params.Key["pool_name"].(*types.AttributeValueMemberS).Value != expectedKey {
						t.Errorf("wrong key: %v", params.Key)
					}
					return &dynamodb.UpdateItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:       "Already claimed",
			instanceID: "i-12345",
			jobID:      100,
			ttl:        5 * time.Minute,
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return nil, &types.ConditionalCheckFailedException{}
				},
			},
			wantErr: true,
			errType: ErrInstanceAlreadyClaimed,
		},
		{
			name:       "Empty instance ID",
			instanceID: "",
			jobID:      100,
			ttl:        5 * time.Minute,
			mockDB:     &MockDynamoDBAPI{},
			wantErr:    true,
		},
		{
			name:       "Zero job ID",
			instanceID: "i-12345",
			jobID:      0,
			ttl:        5 * time.Minute,
			mockDB:     &MockDynamoDBAPI{},
			wantErr:    true,
		},
		{
			name:       "Invalid TTL",
			instanceID: "i-12345",
			jobID:      100,
			ttl:        0,
			mockDB:     &MockDynamoDBAPI{},
			wantErr:    true,
		},
		{
			name:       "DynamoDB error",
			instanceID: "i-12345",
			jobID:      100,
			ttl:        5 * time.Minute,
			mockDB: &MockDynamoDBAPI{
				UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
					return nil, errors.New("network error")
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
				poolsTable:   testPoolsTable,
			}
			err := client.ClaimInstanceForJob(context.Background(), tt.instanceID, tt.jobID, tt.ttl)
			if (err != nil) != tt.wantErr {
				t.Errorf("ClaimInstanceForJob() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.errType != nil && !errors.Is(err, tt.errType) {
				t.Errorf("ClaimInstanceForJob() error = %v, want %v", err, tt.errType)
			}
		})
	}
}

func TestClaimInstanceForJob_NoPoolsTable(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		poolsTable:   "",
	}

	err := client.ClaimInstanceForJob(context.Background(), "i-12345", 100, 5*time.Minute)
	if err == nil {
		t.Error("ClaimInstanceForJob() should fail when pools table not configured")
	}
	if err.Error() != errPoolsTableNotConfigured {
		t.Errorf("ClaimInstanceForJob() error = %v, want '%s'", err, errPoolsTableNotConfigured)
	}
}

func TestReleaseInstanceClaim(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		instanceID string
		jobID      int64
		mockDB     *MockDynamoDBAPI
		wantErr    bool
	}{
		{
			name:       "Success",
			instanceID: "i-12345",
			jobID:      100,
			mockDB: &MockDynamoDBAPI{
				DeleteItemFunc: func(_ context.Context, params *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
					if *params.TableName != testPoolsTable {
						t.Errorf("wrong table name: %s", *params.TableName)
					}
					expectedKey := "__instance_claim:i-12345"
					if params.Key["pool_name"].(*types.AttributeValueMemberS).Value != expectedKey {
						t.Errorf("wrong key: %v", params.Key)
					}
					return &dynamodb.DeleteItemOutput{}, nil
				},
			},
			wantErr: false,
		},
		{
			name:       "Claim not held (condition fail)",
			instanceID: "i-12345",
			jobID:      100,
			mockDB: &MockDynamoDBAPI{
				DeleteItemFunc: func(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
					return nil, &types.ConditionalCheckFailedException{}
				},
			},
			wantErr: false, // Should NOT error - silently succeed
		},
		{
			name:       "Empty instance ID",
			instanceID: "",
			jobID:      100,
			mockDB:     &MockDynamoDBAPI{},
			wantErr:    true,
		},
		{
			name:       "Zero job ID",
			instanceID: "i-12345",
			jobID:      0,
			mockDB:     &MockDynamoDBAPI{},
			wantErr:    true,
		},
		{
			name:       "DynamoDB error",
			instanceID: "i-12345",
			jobID:      100,
			mockDB: &MockDynamoDBAPI{
				DeleteItemFunc: func(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
					return nil, errors.New("network error")
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
				poolsTable:   testPoolsTable,
			}
			err := client.ReleaseInstanceClaim(context.Background(), tt.instanceID, tt.jobID)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReleaseInstanceClaim() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestReleaseInstanceClaim_NoPoolsTable(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		poolsTable:   "",
	}

	err := client.ReleaseInstanceClaim(context.Background(), "i-12345", 100)
	if err == nil {
		t.Error("ReleaseInstanceClaim() should fail when pools table not configured")
	}
	if err.Error() != errPoolsTableNotConfigured {
		t.Errorf("ReleaseInstanceClaim() error = %v, want '%s'", err, errPoolsTableNotConfigured)
	}
}

func TestInstanceClaimKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		instanceID string
		want       string
	}{
		{"i-12345", "__instance_claim:i-12345"},
		{"i-abcdef", "__instance_claim:i-abcdef"},
	}

	for _, tt := range tests {
		t.Run(tt.instanceID, func(t *testing.T) {
			t.Parallel()

			got := instanceClaimKey(tt.instanceID)
			if got != tt.want {
				t.Errorf("instanceClaimKey(%q) = %q, want %q", tt.instanceID, got, tt.want)
			}
		})
	}
}

func TestListJobsForAdmin(t *testing.T) {
	t.Parallel()

	now := time.Now()
	older := now.Add(-1 * time.Hour)

	tests := []struct {
		name      string
		filter    AdminJobFilter
		mockDB    *MockDynamoDBAPI
		wantCount int
		wantTotal int
		wantErr   bool
	}{
		{
			name:   "Returns jobs with all fields populated",
			filter: AdminJobFilter{},
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{
						Items: []map[string]types.AttributeValue{
							{
								"job_id":           &types.AttributeValueMemberN{Value: "12345"},
								"run_id":           &types.AttributeValueMemberN{Value: "67890"},
								"repo":             &types.AttributeValueMemberS{Value: testRepo},
								"instance_id":      &types.AttributeValueMemberS{Value: "i-abc123"},
								"instance_type":    &types.AttributeValueMemberS{Value: "c7g.medium"},
								"pool":             &types.AttributeValueMemberS{Value: "default"},
								"spot":             &types.AttributeValueMemberBOOL{Value: true},
								"warm_pool_hit":    &types.AttributeValueMemberBOOL{Value: false},
								"retry_count":      &types.AttributeValueMemberN{Value: "0"},
								"status":           &types.AttributeValueMemberS{Value: "completed"},
								"exit_code":        &types.AttributeValueMemberN{Value: "0"},
								"duration_seconds": &types.AttributeValueMemberN{Value: "120"},
								"created_at":       &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
								"started_at":       &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
								"completed_at":     &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
							},
						},
					}, nil
				},
			},
			wantCount: 1,
			wantTotal: 1,
			wantErr:   false,
		},
		{
			name:   "Empty result",
			filter: AdminJobFilter{},
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{}}, nil
				},
			},
			wantCount: 0,
			wantTotal: 0,
			wantErr:   false,
		},
		{
			name:   "Pagination with offset and limit",
			filter: AdminJobFilter{Offset: 1, Limit: 1},
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{
						Items: []map[string]types.AttributeValue{
							{
								"job_id":     &types.AttributeValueMemberN{Value: "1"},
								"status":     &types.AttributeValueMemberS{Value: "completed"},
								"created_at": &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
							},
							{
								"job_id":     &types.AttributeValueMemberN{Value: "2"},
								"status":     &types.AttributeValueMemberS{Value: "running"},
								"created_at": &types.AttributeValueMemberS{Value: older.Format(time.RFC3339)},
							},
							{
								"job_id":     &types.AttributeValueMemberN{Value: "3"},
								"status":     &types.AttributeValueMemberS{Value: "failed"},
								"created_at": &types.AttributeValueMemberS{Value: older.Add(-1 * time.Hour).Format(time.RFC3339)},
							},
						},
					}, nil
				},
			},
			wantCount: 1,
			wantTotal: 3,
			wantErr:   false,
		},
		{
			name:   "DynamoDB error propagation",
			filter: AdminJobFilter{},
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return nil, errors.New("dynamodb scan error")
				},
			},
			wantCount: 0,
			wantTotal: 0,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    "jobs-table",
			}

			entries, total, err := client.ListJobsForAdmin(context.Background(), tt.filter)
			if (err != nil) != tt.wantErr {
				t.Errorf("ListJobsForAdmin() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(entries) != tt.wantCount {
				t.Errorf("ListJobsForAdmin() returned %d entries, want %d", len(entries), tt.wantCount)
			}
			if total != tt.wantTotal {
				t.Errorf("ListJobsForAdmin() total = %d, want %d", total, tt.wantTotal)
			}

			if tt.name == "Returns jobs with all fields populated" && len(entries) > 0 {
				e := entries[0]
				if e.JobID != 12345 {
					t.Errorf("JobID = %d, want 12345", e.JobID)
				}
				if e.Repo != testRepo {
					t.Errorf("Repo = %q, want %q", e.Repo, testRepo)
				}
				if e.InstanceID != "i-abc123" {
					t.Errorf("InstanceID = %q, want %q", e.InstanceID, "i-abc123")
				}
				if e.Status != "completed" {
					t.Errorf("Status = %q, want %q", e.Status, "completed")
				}
				if !e.Spot {
					t.Error("Spot = false, want true")
				}
				if e.DurationSeconds != 120 {
					t.Errorf("DurationSeconds = %d, want 120", e.DurationSeconds)
				}
			}
		})
	}
}

func TestListJobsForAdmin_JobsTableNotConfigured(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	_, _, err := client.ListJobsForAdmin(context.Background(), AdminJobFilter{})
	if err == nil {
		t.Error("ListJobsForAdmin() should fail when jobs table not configured")
	}
	if !strings.Contains(err.Error(), "jobs table not configured") {
		t.Errorf("ListJobsForAdmin() error = %v, want 'jobs table not configured'", err)
	}
}

func TestGetJobForAdmin(t *testing.T) {
	t.Parallel()

	now := time.Now()

	tests := []struct {
		name    string
		jobID   int64
		mockDB  *MockDynamoDBAPI
		wantNil bool
		wantErr bool
	}{
		{
			name:  "Job found",
			jobID: 12345,
			mockDB: &MockDynamoDBAPI{
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{
						Item: map[string]types.AttributeValue{
							"job_id":           &types.AttributeValueMemberN{Value: "12345"},
							"run_id":           &types.AttributeValueMemberN{Value: "67890"},
							"repo":             &types.AttributeValueMemberS{Value: testRepo},
							"instance_id":      &types.AttributeValueMemberS{Value: "i-abc123"},
							"instance_type":    &types.AttributeValueMemberS{Value: "c7g.medium"},
							"pool":             &types.AttributeValueMemberS{Value: "default"},
							"spot":             &types.AttributeValueMemberBOOL{Value: true},
							"warm_pool_hit":    &types.AttributeValueMemberBOOL{Value: true},
							"retry_count":      &types.AttributeValueMemberN{Value: "1"},
							"status":           &types.AttributeValueMemberS{Value: "running"},
							"exit_code":        &types.AttributeValueMemberN{Value: "0"},
							"duration_seconds": &types.AttributeValueMemberN{Value: "300"},
							"created_at":       &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
						},
					}, nil
				},
			},
			wantNil: false,
			wantErr: false,
		},
		{
			name:  "Job not found",
			jobID: 99999,
			mockDB: &MockDynamoDBAPI{
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{Item: nil}, nil
				},
			},
			wantNil: true,
			wantErr: false,
		},
		{
			name:  "DynamoDB error propagation",
			jobID: 12345,
			mockDB: &MockDynamoDBAPI{
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return nil, errors.New("dynamodb get error")
				},
			},
			wantNil: true,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    "jobs-table",
			}

			entry, err := client.GetJobForAdmin(context.Background(), tt.jobID)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetJobForAdmin() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantNil {
				if entry != nil {
					t.Errorf("GetJobForAdmin() = %v, want nil", entry)
				}
			} else {
				if entry == nil {
					t.Fatal("GetJobForAdmin() = nil, want entry")
				}
				if entry.JobID != 12345 {
					t.Errorf("JobID = %d, want 12345", entry.JobID)
				}
				if entry.RunID != 67890 {
					t.Errorf("RunID = %d, want 67890", entry.RunID)
				}
				if entry.Repo != testRepo {
					t.Errorf("Repo = %q, want %q", entry.Repo, testRepo)
				}
				if entry.Status != "running" {
					t.Errorf("Status = %q, want %q", entry.Status, "running")
				}
				if !entry.WarmPoolHit {
					t.Error("WarmPoolHit = false, want true")
				}
				if entry.RetryCount != 1 {
					t.Errorf("RetryCount = %d, want 1", entry.RetryCount)
				}
			}
		})
	}
}

func TestGetJobForAdmin_JobsTableNotConfigured(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	entry, err := client.GetJobForAdmin(context.Background(), 12345)
	if err == nil {
		t.Error("GetJobForAdmin() should fail when jobs table not configured")
	}
	if entry != nil {
		t.Errorf("GetJobForAdmin() = %v, want nil on error", entry)
	}
}

func TestGetJobStatsForAdmin(t *testing.T) {
	t.Parallel()

	since := time.Now().Add(-24 * time.Hour)

	tests := []struct {
		name      string
		mockDB    *MockDynamoDBAPI
		wantStats *AdminJobStats
		wantErr   bool
	}{
		{
			name: "Various status distributions",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{
						Items: []map[string]types.AttributeValue{
							{
								"job_id": &types.AttributeValueMemberN{Value: "1"},
								"status": &types.AttributeValueMemberS{Value: "completed"},
							},
							{
								"job_id": &types.AttributeValueMemberN{Value: "2"},
								"status": &types.AttributeValueMemberS{Value: "success"},
							},
							{
								"job_id":        &types.AttributeValueMemberN{Value: "3"},
								"status":        &types.AttributeValueMemberS{Value: "failed"},
								"warm_pool_hit": &types.AttributeValueMemberBOOL{Value: true},
							},
							{
								"job_id": &types.AttributeValueMemberN{Value: "4"},
								"status": &types.AttributeValueMemberS{Value: "error"},
							},
							{
								"job_id":        &types.AttributeValueMemberN{Value: "5"},
								"status":        &types.AttributeValueMemberS{Value: "running"},
								"warm_pool_hit": &types.AttributeValueMemberBOOL{Value: true},
							},
							{
								"job_id": &types.AttributeValueMemberN{Value: "6"},
								"status": &types.AttributeValueMemberS{Value: "claiming"},
							},
							{
								"job_id": &types.AttributeValueMemberN{Value: "7"},
								"status": &types.AttributeValueMemberS{Value: "requeued"},
							},
						},
					}, nil
				},
			},
			wantStats: &AdminJobStats{
				Total:       7,
				Completed:   2,
				Failed:      2,
				Running:     2,
				Requeued:    1,
				WarmPoolHit: 2,
			},
			wantErr: false,
		},
		{
			name: "Empty table",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{}}, nil
				},
			},
			wantStats: &AdminJobStats{
				Total:       0,
				Completed:   0,
				Failed:      0,
				Running:     0,
				Requeued:    0,
				WarmPoolHit: 0,
			},
			wantErr: false,
		},
		{
			name: "DynamoDB error propagation",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return nil, errors.New("dynamodb scan error")
				},
			},
			wantStats: nil,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    "jobs-table",
			}

			stats, err := client.GetJobStatsForAdmin(context.Background(), since)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetJobStatsForAdmin() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantStats == nil {
				if stats != nil && !tt.wantErr {
					t.Errorf("GetJobStatsForAdmin() = %v, want nil", stats)
				}
				return
			}

			if stats == nil {
				t.Fatal("GetJobStatsForAdmin() = nil, want stats")
			}
			if stats.Total != tt.wantStats.Total {
				t.Errorf("Total = %d, want %d", stats.Total, tt.wantStats.Total)
			}
			if stats.Completed != tt.wantStats.Completed {
				t.Errorf("Completed = %d, want %d", stats.Completed, tt.wantStats.Completed)
			}
			if stats.Failed != tt.wantStats.Failed {
				t.Errorf("Failed = %d, want %d", stats.Failed, tt.wantStats.Failed)
			}
			if stats.Running != tt.wantStats.Running {
				t.Errorf("Running = %d, want %d", stats.Running, tt.wantStats.Running)
			}
			if stats.Requeued != tt.wantStats.Requeued {
				t.Errorf("Requeued = %d, want %d", stats.Requeued, tt.wantStats.Requeued)
			}
			if stats.WarmPoolHit != tt.wantStats.WarmPoolHit {
				t.Errorf("WarmPoolHit = %d, want %d", stats.WarmPoolHit, tt.wantStats.WarmPoolHit)
			}
		})
	}
}

func TestGetJobStatsForAdmin_JobsTableNotConfigured(t *testing.T) {
	t.Parallel()

	client := &Client{
		dynamoClient: &MockDynamoDBAPI{},
		jobsTable:    "",
	}

	stats, err := client.GetJobStatsForAdmin(context.Background(), time.Now())
	if err == nil {
		t.Error("GetJobStatsForAdmin() should fail when jobs table not configured")
	}
	if stats != nil {
		t.Errorf("GetJobStatsForAdmin() = %v, want nil on error", stats)
	}
}

func TestGetPoolBusyInstanceIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		poolName    string
		jobsTable   string
		mockDB      *MockDynamoDBAPI
		wantIDs     []string
		wantErr     bool
	}{
		{
			name:      "Returns instance IDs for running jobs",
			poolName:  "default",
			jobsTable: "jobs-table",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					if params.FilterExpression == nil {
						t.Error("Expected filter expression")
					}
					return &dynamodb.ScanOutput{
						Items: []map[string]types.AttributeValue{
							{"instance_id": &types.AttributeValueMemberS{Value: "i-111"}},
							{"instance_id": &types.AttributeValueMemberS{Value: "i-222"}},
						},
					}, nil
				},
			},
			wantIDs: []string{"i-111", "i-222"},
			wantErr: false,
		},
		{
			name:      "Empty result",
			poolName:  "empty-pool",
			jobsTable: "jobs-table",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{}}, nil
				},
			},
			wantIDs: nil,
			wantErr: false,
		},
		{
			name:      "Empty pool name",
			poolName:  "",
			jobsTable: "jobs-table",
			mockDB:    &MockDynamoDBAPI{},
			wantIDs:   nil,
			wantErr:   true,
		},
		{
			name:      "Jobs table not configured returns nil",
			poolName:  "default",
			jobsTable: "",
			mockDB:    &MockDynamoDBAPI{},
			wantIDs:   nil,
			wantErr:   false,
		},
		{
			name:      "DynamoDB error propagation",
			poolName:  "default",
			jobsTable: "jobs-table",
			mockDB: &MockDynamoDBAPI{
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return nil, errors.New("dynamodb scan error")
				},
			},
			wantIDs: nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				jobsTable:    tt.jobsTable,
			}

			ids, err := client.GetPoolBusyInstanceIDs(context.Background(), tt.poolName)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetPoolBusyInstanceIDs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(ids) != len(tt.wantIDs) {
				t.Errorf("GetPoolBusyInstanceIDs() returned %d IDs, want %d", len(ids), len(tt.wantIDs))
				return
			}

			for i, id := range ids {
				if id != tt.wantIDs[i] {
					t.Errorf("GetPoolBusyInstanceIDs()[%d] = %q, want %q", i, id, tt.wantIDs[i])
				}
			}
		})
	}
}

func TestCreateEphemeralPool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  *PoolConfig
		mockDB  *MockDynamoDBAPI
		wantErr error
	}{
		{
			name: "Successful creation",
			config: &PoolConfig{
				PoolName:       "ephemeral-pool-1",
				InstanceType:   "c7g.medium",
				DesiredRunning: 1,
				Ephemeral:      true,
			},
			mockDB: &MockDynamoDBAPI{
				PutItemFunc: func(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
					if params.ConditionExpression == nil || *params.ConditionExpression != "attribute_not_exists(pool_name)" {
						t.Errorf("Expected condition expression 'attribute_not_exists(pool_name)', got %v", params.ConditionExpression)
					}
					return &dynamodb.PutItemOutput{}, nil
				},
			},
			wantErr: nil,
		},
		{
			name: "Pool already exists",
			config: &PoolConfig{
				PoolName:  "existing-pool",
				Ephemeral: true,
			},
			mockDB: &MockDynamoDBAPI{
				PutItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
					return nil, &types.ConditionalCheckFailedException{
						Message: nil,
					}
				},
			},
			wantErr: ErrPoolAlreadyExists,
		},
		{
			name: "DynamoDB error propagation",
			config: &PoolConfig{
				PoolName:  "new-pool",
				Ephemeral: true,
			},
			mockDB: &MockDynamoDBAPI{
				PutItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
					return nil, errors.New("dynamodb put error")
				},
			},
			wantErr: errors.New("dynamodb put error"),
		},
		{
			name:    "Nil config",
			config:  nil,
			mockDB:  &MockDynamoDBAPI{},
			wantErr: errors.New("pool config cannot be nil"),
		},
		{
			name: "Empty pool name",
			config: &PoolConfig{
				PoolName:  "",
				Ephemeral: true,
			},
			mockDB:  &MockDynamoDBAPI{},
			wantErr: errors.New("pool name cannot be empty"),
		},
		{
			name: "Non-ephemeral pool rejected",
			config: &PoolConfig{
				PoolName:  "persistent-pool",
				Ephemeral: false,
			},
			mockDB:  &MockDynamoDBAPI{},
			wantErr: errors.New("CreateEphemeralPool can only be used for ephemeral pools"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				dynamoClient: tt.mockDB,
				poolsTable:   testPoolsTable,
			}

			err := client.CreateEphemeralPool(context.Background(), tt.config)

			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("CreateEphemeralPool() error = %v, want nil", err)
				}
				return
			}

			if err == nil {
				t.Errorf("CreateEphemeralPool() = nil, want error %v", tt.wantErr)
				return
			}

			if errors.Is(tt.wantErr, ErrPoolAlreadyExists) {
				if !errors.Is(err, ErrPoolAlreadyExists) {
					t.Errorf("CreateEphemeralPool() error = %v, want ErrPoolAlreadyExists", err)
				}
			} else if !strings.Contains(err.Error(), tt.wantErr.Error()) {
				t.Errorf("CreateEphemeralPool() error = %v, want containing %q", err, tt.wantErr.Error())
			}
		})
	}
}

func TestGetPoolBusyInstanceIDs_ScanPath(t *testing.T) {
	t.Parallel()

	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, input *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			if input.ProjectionExpression == nil || *input.ProjectionExpression != "instance_id" {
				return nil, errors.New("expected projection on instance_id")
			}
			return &dynamodb.ScanOutput{
				Items: []map[string]types.AttributeValue{
					{"instance_id": &types.AttributeValueMemberS{Value: "i-aaa"}},
					{"instance_id": &types.AttributeValueMemberS{Value: "i-bbb"}},
				},
			}, nil
		},
	}

	client := &Client{
		dynamoClient: mock,
		jobsTable:    "jobs-table",
	}

	ids, err := client.GetPoolBusyInstanceIDs(context.Background(), "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d instance IDs, want 2", len(ids))
	}
	if ids[0] != "i-aaa" || ids[1] != "i-bbb" {
		t.Errorf("got %v, want [i-aaa i-bbb]", ids)
	}
}

func TestGetPoolBusyInstanceIDs_GSIPath(t *testing.T) {
	t.Parallel()

	queryCalled := false
	mock := &MockDynamoDBAPI{
		QueryFunc: func(_ context.Context, input *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
			queryCalled = true
			if input.IndexName == nil || *input.IndexName != "pool-status-index" {
				return nil, errors.New("expected GSI name pool-status-index")
			}
			return &dynamodb.QueryOutput{
				Items: []map[string]types.AttributeValue{
					{"instance_id": &types.AttributeValueMemberS{Value: "i-gsi-1"}},
					{"instance_id": &types.AttributeValueMemberS{Value: "i-gsi-2"}},
				},
			}, nil
		},
		ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			t.Error("Scan should not be called when GSI query succeeds")
			return &dynamodb.ScanOutput{}, nil
		},
	}

	client := &Client{
		dynamoClient:      mock,
		jobsTable:         "jobs-table",
		jobsPoolStatusGSI: "pool-status-index",
	}

	ids, err := client.GetPoolBusyInstanceIDs(context.Background(), "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !queryCalled {
		t.Fatal("Query was not called")
	}
	if len(ids) != 2 {
		t.Fatalf("got %d instance IDs, want 2", len(ids))
	}
	if ids[0] != "i-gsi-1" || ids[1] != "i-gsi-2" {
		t.Errorf("got %v, want [i-gsi-1 i-gsi-2]", ids)
	}
}

func TestGetPoolBusyInstanceIDs_GSIFallbackToScan(t *testing.T) {
	t.Parallel()

	scanCalled := false
	mock := &MockDynamoDBAPI{
		QueryFunc: func(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
			return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "index not found"}
		},
		ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			scanCalled = true
			return &dynamodb.ScanOutput{
				Items: []map[string]types.AttributeValue{
					{"instance_id": &types.AttributeValueMemberS{Value: "i-fallback"}},
				},
			}, nil
		},
	}

	client := &Client{
		dynamoClient:      mock,
		jobsTable:         "jobs-table",
		jobsPoolStatusGSI: "pool-status-index",
	}

	ids, err := client.GetPoolBusyInstanceIDs(context.Background(), "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !scanCalled {
		t.Fatal("Scan was not called as fallback")
	}
	if len(ids) != 1 || ids[0] != "i-fallback" {
		t.Errorf("got %v, want [i-fallback]", ids)
	}
}

func TestGetPoolBusyInstanceIDs_GSINonValidationError(t *testing.T) {
	t.Parallel()

	mock := &MockDynamoDBAPI{
		QueryFunc: func(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
			return nil, errors.New("throttling: rate exceeded")
		},
	}

	client := &Client{
		dynamoClient:      mock,
		jobsTable:         "jobs-table",
		jobsPoolStatusGSI: "pool-status-index",
	}

	_, err := client.GetPoolBusyInstanceIDs(context.Background(), "default")
	if err == nil {
		t.Fatal("expected error for non-validation GSI failure")
	}
	if !strings.Contains(err.Error(), "throttling") {
		t.Errorf("expected throttling error, got: %v", err)
	}
}
