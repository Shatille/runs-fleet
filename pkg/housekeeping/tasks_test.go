package housekeeping

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// mockEC2API implements EC2API for testing.
type mockEC2API struct {
	instances      []ec2types.Reservation
	describeErr    error
	terminateErr   error
	describeCalls  int
	terminateCalls int
	terminatedIDs  []string
}

func (m *mockEC2API) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	m.describeCalls++
	if m.describeErr != nil {
		return nil, m.describeErr
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: m.instances,
	}, nil
}

func (m *mockEC2API) TerminateInstances(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	m.terminateCalls++
	m.terminatedIDs = params.InstanceIds
	if m.terminateErr != nil {
		return nil, m.terminateErr
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

// mockTaskSSMAPI implements SSMAPI for testing.
type mockTaskSSMAPI struct {
	parameters     []ssmtypes.Parameter
	getErr         error
	deleteErr      error
	getCalls       int
	deleteCalls    int
	deletedParams  []string
}

func (m *mockTaskSSMAPI) GetParametersByPath(_ context.Context, _ *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	m.getCalls++
	if m.getErr != nil {
		return nil, m.getErr
	}
	return &ssm.GetParametersByPathOutput{
		Parameters: m.parameters,
	}, nil
}

func (m *mockTaskSSMAPI) DeleteParameter(_ context.Context, params *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	m.deleteCalls++
	if params.Name != nil {
		m.deletedParams = append(m.deletedParams, *params.Name)
	}
	if m.deleteErr != nil {
		return nil, m.deleteErr
	}
	return &ssm.DeleteParameterOutput{}, nil
}

// mockTaskDynamoDBAPI implements DynamoDBAPI for testing.
type mockTaskDynamoDBAPI struct {
	items         []map[string]types.AttributeValue
	queryErr      error
	batchWriteErr error
	scanErr       error
	queryCalls    int
	scanCalls     int
	batchCalls    int
}

func (m *mockTaskDynamoDBAPI) Query(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	m.queryCalls++
	if m.queryErr != nil {
		return nil, m.queryErr
	}
	return &dynamodb.QueryOutput{
		Items: m.items,
	}, nil
}

func (m *mockTaskDynamoDBAPI) BatchWriteItem(_ context.Context, _ *dynamodb.BatchWriteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
	m.batchCalls++
	if m.batchWriteErr != nil {
		return nil, m.batchWriteErr
	}
	return &dynamodb.BatchWriteItemOutput{}, nil
}

func (m *mockTaskDynamoDBAPI) Scan(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	m.scanCalls++
	if m.scanErr != nil {
		return nil, m.scanErr
	}
	return &dynamodb.ScanOutput{
		Items: m.items,
	}, nil
}

// mockTaskMetricsAPI implements MetricsAPI for testing.
type mockTaskMetricsAPI struct {
	orphanedCount    int
	ssmCount         int
	jobCount         int
	poolUtilizations map[string]float64
	err              error
}

func (m *mockTaskMetricsAPI) PublishOrphanedInstancesTerminated(_ context.Context, count int) error {
	m.orphanedCount = count
	return m.err
}

func (m *mockTaskMetricsAPI) PublishSSMParametersDeleted(_ context.Context, count int) error {
	m.ssmCount = count
	return m.err
}

func (m *mockTaskMetricsAPI) PublishJobRecordsArchived(_ context.Context, count int) error {
	m.jobCount = count
	return m.err
}

func (m *mockTaskMetricsAPI) PublishPoolUtilization(_ context.Context, poolName string, utilization float64) error {
	if m.poolUtilizations == nil {
		m.poolUtilizations = make(map[string]float64)
	}
	m.poolUtilizations[poolName] = utilization
	return m.err
}

// mockCostReporter implements CostReporter for testing.
type mockCostReporter struct {
	err   error
	calls int
}

func (m *mockCostReporter) GenerateDailyReport(_ context.Context) error {
	m.calls++
	return m.err
}

// mockTaskSQSAPI implements SQSAPI for testing.
type mockTaskSQSAPI struct {
	getQueueAttrsOutputs   []*sqs.GetQueueAttributesOutput // Multiple outputs for successive calls
	getQueueAttrsErrors    []error                         // Multiple errors for successive calls
	startMoveTaskOutput    *sqs.StartMessageMoveTaskOutput
	startMoveTaskErr       error
	getQueueAttrsCalls     int
	startMoveTaskCalls     int
	lastGetQueueAttrsInput *sqs.GetQueueAttributesInput
}

func (m *mockTaskSQSAPI) GetQueueAttributes(_ context.Context, params *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	idx := m.getQueueAttrsCalls
	m.getQueueAttrsCalls++
	m.lastGetQueueAttrsInput = params

	// Return error for this call if specified
	if idx < len(m.getQueueAttrsErrors) && m.getQueueAttrsErrors[idx] != nil {
		return nil, m.getQueueAttrsErrors[idx]
	}

	// Return output for this call if specified
	if idx < len(m.getQueueAttrsOutputs) {
		return m.getQueueAttrsOutputs[idx], nil
	}

	return nil, nil
}

func (m *mockTaskSQSAPI) StartMessageMoveTask(_ context.Context, _ *sqs.StartMessageMoveTaskInput, _ ...func(*sqs.Options)) (*sqs.StartMessageMoveTaskOutput, error) {
	m.startMoveTaskCalls++
	if m.startMoveTaskErr != nil {
		return nil, m.startMoveTaskErr
	}
	return m.startMoveTaskOutput, nil
}

func TestExecuteOrphanedInstances_NoOrphans(t *testing.T) {
	now := time.Now()

	ec2Client := &mockEC2API{
		instances: []ec2types.Reservation{
			{
				Instances: []ec2types.Instance{
					{
						InstanceId: strPtr("i-12345"),
						LaunchTime: &now, // Recent, not orphaned
						State: &ec2types.InstanceState{
							Name: ec2types.InstanceStateNameRunning,
						},
					},
				},
			},
		},
	}

	tasks := &Tasks{
		ec2Client: ec2Client,
		config: &config.Config{
			MaxRuntimeMinutes: 60,
		},
	}

	err := tasks.ExecuteOrphanedInstances(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ec2Client.terminateCalls != 0 {
		t.Errorf("expected 0 terminate calls, got %d", ec2Client.terminateCalls)
	}
}

func TestExecuteOrphanedInstances_WithOrphans(t *testing.T) {
	// Create an old launch time (older than max_runtime + 10 min)
	oldLaunchTime := time.Now().Add(-2 * time.Hour)

	ec2Client := &mockEC2API{
		instances: []ec2types.Reservation{
			{
				Instances: []ec2types.Instance{
					{
						InstanceId: strPtr("i-orphan-1"),
						LaunchTime: &oldLaunchTime,
						State: &ec2types.InstanceState{
							Name: ec2types.InstanceStateNameRunning,
						},
					},
				},
			},
		},
	}
	metrics := &mockTaskMetricsAPI{}

	tasks := &Tasks{
		ec2Client: ec2Client,
		metrics:   metrics,
		config: &config.Config{
			MaxRuntimeMinutes: 60,
		},
	}

	err := tasks.ExecuteOrphanedInstances(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ec2Client.terminateCalls != 1 {
		t.Errorf("expected 1 terminate call, got %d", ec2Client.terminateCalls)
	}

	if len(ec2Client.terminatedIDs) != 1 || ec2Client.terminatedIDs[0] != "i-orphan-1" {
		t.Errorf("expected to terminate 'i-orphan-1', got %v", ec2Client.terminatedIDs)
	}

	if metrics.orphanedCount != 1 {
		t.Errorf("expected orphaned metric count 1, got %d", metrics.orphanedCount)
	}
}

func TestExecuteOrphanedInstances_DescribeError(t *testing.T) {
	ec2Client := &mockEC2API{
		describeErr: errors.New("describe error"),
	}

	tasks := &Tasks{
		ec2Client: ec2Client,
		config: &config.Config{
			MaxRuntimeMinutes: 60,
		},
	}

	err := tasks.ExecuteOrphanedInstances(context.Background())
	if err == nil {
		t.Fatal("expected error from describe")
	}
}

func TestExecuteOrphanedInstances_TerminateError(t *testing.T) {
	oldLaunchTime := time.Now().Add(-2 * time.Hour)

	ec2Client := &mockEC2API{
		instances: []ec2types.Reservation{
			{
				Instances: []ec2types.Instance{
					{
						InstanceId: strPtr("i-orphan-1"),
						LaunchTime: &oldLaunchTime,
					},
				},
			},
		},
		terminateErr: errors.New("terminate error"),
	}

	tasks := &Tasks{
		ec2Client: ec2Client,
		config: &config.Config{
			MaxRuntimeMinutes: 60,
		},
	}

	err := tasks.ExecuteOrphanedInstances(context.Background())
	if err == nil {
		t.Fatal("expected error from terminate")
	}
}

func TestExecuteStaleSSM_NoStaleParams(t *testing.T) {
	now := time.Now()

	ssmClient := &mockTaskSSMAPI{
		parameters: []ssmtypes.Parameter{
			{
				Name: strPtr("/runs-fleet/runners/i-exists/config"),
			},
		},
	}

	ec2Client := &mockEC2API{
		instances: []ec2types.Reservation{
			{
				Instances: []ec2types.Instance{
					{
						InstanceId: strPtr("i-exists"),
						LaunchTime: &now,
						State: &ec2types.InstanceState{
							Name: ec2types.InstanceStateNameRunning,
						},
					},
				},
			},
		},
	}

	tasks := &Tasks{
		ec2Client: ec2Client,
		ssmClient: ssmClient,
		config:    &config.Config{},
	}

	err := tasks.ExecuteStaleSSM(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ssmClient.deleteCalls != 0 {
		t.Errorf("expected 0 delete calls, got %d", ssmClient.deleteCalls)
	}
}

func TestExecuteStaleSSM_GetError(t *testing.T) {
	ssmClient := &mockTaskSSMAPI{
		getErr: errors.New("get error"),
	}

	tasks := &Tasks{
		ssmClient: ssmClient,
		config:    &config.Config{},
	}

	err := tasks.ExecuteStaleSSM(context.Background())
	if err == nil {
		t.Fatal("expected error from get parameters")
	}
}

func TestExecuteOldJobs_NoJobsTable(t *testing.T) {
	tasks := &Tasks{
		config: &config.Config{
			JobsTableName: "",
		},
	}

	err := tasks.ExecuteOldJobs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteOldJobs_NoOldJobs(t *testing.T) {
	dynamoClient := &mockTaskDynamoDBAPI{
		items: []map[string]types.AttributeValue{},
	}

	tasks := &Tasks{
		dynamoClient: dynamoClient,
		config: &config.Config{
			JobsTableName: "jobs-table",
		},
	}

	err := tasks.ExecuteOldJobs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if dynamoClient.batchCalls != 0 {
		t.Errorf("expected 0 batch calls, got %d", dynamoClient.batchCalls)
	}
}

func TestExecuteOldJobs_WithOldJobs(t *testing.T) {
	dynamoClient := &mockTaskDynamoDBAPI{
		items: []map[string]types.AttributeValue{
			{"job_id": &types.AttributeValueMemberS{Value: "job-1"}},
			{"job_id": &types.AttributeValueMemberS{Value: "job-2"}},
		},
	}
	metrics := &mockTaskMetricsAPI{}

	tasks := &Tasks{
		dynamoClient: dynamoClient,
		metrics:      metrics,
		config: &config.Config{
			JobsTableName: "jobs-table",
		},
	}

	err := tasks.ExecuteOldJobs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if dynamoClient.batchCalls != 1 {
		t.Errorf("expected 1 batch call, got %d", dynamoClient.batchCalls)
	}

	if metrics.jobCount != 2 {
		t.Errorf("expected job count 2, got %d", metrics.jobCount)
	}
}

func TestExecuteOldJobs_ScanError(t *testing.T) {
	dynamoClient := &mockTaskDynamoDBAPI{
		scanErr: errors.New("scan error"),
	}

	tasks := &Tasks{
		dynamoClient: dynamoClient,
		config: &config.Config{
			JobsTableName: "jobs-table",
		},
	}

	err := tasks.ExecuteOldJobs(context.Background())
	if err == nil {
		t.Fatal("expected error from scan")
	}
}

func TestExecutePoolAudit_NoPoolsTable(t *testing.T) {
	tasks := &Tasks{
		config: &config.Config{
			PoolsTableName: "",
		},
	}

	err := tasks.ExecutePoolAudit(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecutePoolAudit_Success(t *testing.T) {
	dynamoClient := &mockTaskDynamoDBAPI{
		items: []map[string]types.AttributeValue{
			{
				"pool_name":       &types.AttributeValueMemberS{Value: "default-pool"},
				"desired_running": &types.AttributeValueMemberN{Value: "10"},
				"current_running": &types.AttributeValueMemberN{Value: "8"},
			},
		},
	}
	metrics := &mockTaskMetricsAPI{}

	tasks := &Tasks{
		dynamoClient: dynamoClient,
		metrics:      metrics,
		config: &config.Config{
			PoolsTableName: "pools-table",
		},
	}

	err := tasks.ExecutePoolAudit(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected utilization: 8/10 * 100 = 80%
	if metrics.poolUtilizations["default-pool"] != 80.0 {
		t.Errorf("expected pool utilization 80.0, got %v", metrics.poolUtilizations["default-pool"])
	}
}

func TestExecutePoolAudit_ScanError(t *testing.T) {
	dynamoClient := &mockTaskDynamoDBAPI{
		scanErr: errors.New("scan error"),
	}

	tasks := &Tasks{
		dynamoClient: dynamoClient,
		config: &config.Config{
			PoolsTableName: "pools-table",
		},
	}

	err := tasks.ExecutePoolAudit(context.Background())
	if err == nil {
		t.Fatal("expected error from scan")
	}
}

func TestExecuteCostReport_NoCostReporter(t *testing.T) {
	tasks := &Tasks{
		costReporter: nil,
		config:       &config.Config{},
	}

	err := tasks.ExecuteCostReport(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteCostReport_TypedNilPointer(t *testing.T) {
	// Regression test: passing a typed nil pointer (*mockCostReporter)(nil)
	// creates a non-nil interface with nil concrete value, which bypasses
	// the nil check and causes a panic when methods are called.
	var reporter *mockCostReporter // typed nil pointer

	tasks := &Tasks{
		costReporter: reporter, // interface wraps typed nil - NOT equal to nil
		config:       &config.Config{},
	}

	// This should not panic - the nil check must handle typed nil pointers
	err := tasks.ExecuteCostReport(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteCostReport_Success(t *testing.T) {
	costReporter := &mockCostReporter{}

	tasks := &Tasks{
		costReporter: costReporter,
		config:       &config.Config{},
	}

	err := tasks.ExecuteCostReport(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if costReporter.calls != 1 {
		t.Errorf("expected 1 cost report call, got %d", costReporter.calls)
	}
}

func TestExecuteCostReport_Error(t *testing.T) {
	costReporter := &mockCostReporter{
		err: errors.New("report error"),
	}

	tasks := &Tasks{
		costReporter: costReporter,
		config:       &config.Config{},
	}

	err := tasks.ExecuteCostReport(context.Background())
	if err == nil {
		t.Fatal("expected error from cost reporter")
	}
}

func TestInstanceTerminationGracePeriod(t *testing.T) {
	if instanceTerminationGracePeriod != 10*time.Minute {
		t.Errorf("expected grace period 10m, got %v", instanceTerminationGracePeriod)
	}
}

func TestExecuteDLQRedrive_NoDLQURL(t *testing.T) {
	tasks := &Tasks{
		config: &config.Config{
			QueueDLQURL: "",
			QueueURL:    "https://sqs.example.com/main-queue",
		},
	}

	err := tasks.ExecuteDLQRedrive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteDLQRedrive_NoMainQueueURL(t *testing.T) {
	tasks := &Tasks{
		config: &config.Config{
			QueueDLQURL: "https://sqs.example.com/dlq",
			QueueURL:    "",
		},
	}

	err := tasks.ExecuteDLQRedrive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteDLQRedrive_EmptyDLQ(t *testing.T) {
	sqsClient := &mockTaskSQSAPI{
		getQueueAttrsOutputs: []*sqs.GetQueueAttributesOutput{
			{
				Attributes: map[string]string{
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "0",
					string(sqstypes.QueueAttributeNameQueueArn):                    "arn:aws:sqs:us-east-1:123456789012:dlq",
				},
			},
		},
	}

	tasks := &Tasks{
		sqsClient: sqsClient,
		config: &config.Config{
			QueueDLQURL: "https://sqs.example.com/dlq",
			QueueURL:    "https://sqs.example.com/main-queue",
		},
	}

	err := tasks.ExecuteDLQRedrive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sqsClient.getQueueAttrsCalls != 1 {
		t.Errorf("expected 1 get queue attributes call, got %d", sqsClient.getQueueAttrsCalls)
	}
	if sqsClient.startMoveTaskCalls != 0 {
		t.Errorf("expected 0 start move task calls for empty DLQ, got %d", sqsClient.startMoveTaskCalls)
	}
}

func TestExecuteDLQRedrive_Success(t *testing.T) {
	sqsClient := &mockTaskSQSAPI{
		getQueueAttrsOutputs: []*sqs.GetQueueAttributesOutput{
			// First call - DLQ attributes
			{
				Attributes: map[string]string{
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "5",
					string(sqstypes.QueueAttributeNameQueueArn):                    "arn:aws:sqs:us-east-1:123456789012:dlq",
				},
			},
			// Second call - Main queue attributes
			{
				Attributes: map[string]string{
					string(sqstypes.QueueAttributeNameQueueArn): "arn:aws:sqs:us-east-1:123456789012:main-queue",
				},
			},
		},
		startMoveTaskOutput: &sqs.StartMessageMoveTaskOutput{
			TaskHandle: aws.String("task-handle-123"),
		},
	}

	tasks := &Tasks{
		sqsClient: sqsClient,
		config: &config.Config{
			QueueDLQURL: "https://sqs.example.com/dlq",
			QueueURL:    "https://sqs.example.com/main-queue",
		},
	}

	err := tasks.ExecuteDLQRedrive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sqsClient.startMoveTaskCalls != 1 {
		t.Errorf("expected 1 start move task call, got %d", sqsClient.startMoveTaskCalls)
	}
}

func TestExecuteDLQRedrive_GetDLQAttributesError(t *testing.T) {
	sqsClient := &mockTaskSQSAPI{
		getQueueAttrsErrors: []error{errors.New("failed to get DLQ attributes")},
	}

	tasks := &Tasks{
		sqsClient: sqsClient,
		config: &config.Config{
			QueueDLQURL: "https://sqs.example.com/dlq",
			QueueURL:    "https://sqs.example.com/main-queue",
		},
	}

	err := tasks.ExecuteDLQRedrive(context.Background())
	if err == nil {
		t.Fatal("expected error from get DLQ attributes")
	}
}

func TestExecuteDLQRedrive_StartMoveTaskError(t *testing.T) {
	sqsClient := &mockTaskSQSAPI{
		getQueueAttrsOutputs: []*sqs.GetQueueAttributesOutput{
			// First call - DLQ attributes
			{
				Attributes: map[string]string{
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "5",
					string(sqstypes.QueueAttributeNameQueueArn):                    "arn:aws:sqs:us-east-1:123456789012:dlq",
				},
			},
			// Second call - Main queue attributes
			{
				Attributes: map[string]string{
					string(sqstypes.QueueAttributeNameQueueArn): "arn:aws:sqs:us-east-1:123456789012:main-queue",
				},
			},
		},
		startMoveTaskErr: errors.New("failed to start move task"),
	}

	tasks := &Tasks{
		sqsClient: sqsClient,
		config: &config.Config{
			QueueDLQURL: "https://sqs.example.com/dlq",
			QueueURL:    "https://sqs.example.com/main-queue",
		},
	}

	err := tasks.ExecuteDLQRedrive(context.Background())
	if err == nil {
		t.Fatal("expected error from start move task")
	}
}

func TestExecuteDLQRedrive_EmptyDLQArn(t *testing.T) {
	sqsClient := &mockTaskSQSAPI{
		getQueueAttrsOutputs: []*sqs.GetQueueAttributesOutput{
			{
				Attributes: map[string]string{
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "5",
					string(sqstypes.QueueAttributeNameQueueArn):                    "",
				},
			},
		},
	}

	tasks := &Tasks{
		sqsClient: sqsClient,
		config: &config.Config{
			QueueDLQURL: "https://sqs.example.com/dlq",
			QueueURL:    "https://sqs.example.com/main-queue",
		},
	}

	err := tasks.ExecuteDLQRedrive(context.Background())
	if err == nil {
		t.Fatal("expected error for empty DLQ ARN")
	}
}

func TestExecuteDLQRedrive_EmptyMainQueueArn(t *testing.T) {
	sqsClient := &mockTaskSQSAPI{
		getQueueAttrsOutputs: []*sqs.GetQueueAttributesOutput{
			// First call - DLQ attributes
			{
				Attributes: map[string]string{
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "5",
					string(sqstypes.QueueAttributeNameQueueArn):                    "arn:aws:sqs:us-east-1:123456789012:dlq",
				},
			},
			// Second call - Main queue attributes with empty ARN
			{
				Attributes: map[string]string{
					string(sqstypes.QueueAttributeNameQueueArn): "",
				},
			},
		},
	}

	tasks := &Tasks{
		sqsClient: sqsClient,
		config: &config.Config{
			QueueDLQURL: "https://sqs.example.com/dlq",
			QueueURL:    "https://sqs.example.com/main-queue",
		},
	}

	err := tasks.ExecuteDLQRedrive(context.Background())
	if err == nil {
		t.Fatal("expected error for empty main queue ARN")
	}
}

func TestExecuteDLQRedrive_GetMainQueueAttributesError(t *testing.T) {
	sqsClient := &mockTaskSQSAPI{
		getQueueAttrsOutputs: []*sqs.GetQueueAttributesOutput{
			// First call - DLQ attributes success
			{
				Attributes: map[string]string{
					string(sqstypes.QueueAttributeNameApproximateNumberOfMessages): "5",
					string(sqstypes.QueueAttributeNameQueueArn):                    "arn:aws:sqs:us-east-1:123456789012:dlq",
				},
			},
		},
		// Second call will fail
		getQueueAttrsErrors: []error{nil, errors.New("failed to get main queue attributes")},
	}

	tasks := &Tasks{
		sqsClient: sqsClient,
		config: &config.Config{
			QueueDLQURL: "https://sqs.example.com/dlq",
			QueueURL:    "https://sqs.example.com/main-queue",
		},
	}

	err := tasks.ExecuteDLQRedrive(context.Background())
	if err == nil {
		t.Fatal("expected error from get main queue attributes")
	}
}

// mockPoolDBAPI implements PoolDBAPI for testing.
type mockPoolDBAPI struct {
	pools         []string
	poolConfigs   map[string]*PoolConfig
	listErr       error
	getErr        error
	deleteErr     error
	deletedPools  []string
}

func (m *mockPoolDBAPI) ListPools(_ context.Context) ([]string, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.pools, nil
}

func (m *mockPoolDBAPI) GetPoolConfig(_ context.Context, poolName string) (*PoolConfig, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	if m.poolConfigs == nil {
		return nil, nil
	}
	return m.poolConfigs[poolName], nil
}

func (m *mockPoolDBAPI) DeletePoolConfig(_ context.Context, poolName string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deletedPools = append(m.deletedPools, poolName)
	return nil
}

func TestExecuteEphemeralPoolCleanup_NoPoolDB(t *testing.T) {
	tasks := &Tasks{
		config: &config.Config{},
	}

	err := tasks.ExecuteEphemeralPoolCleanup(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteEphemeralPoolCleanup_NoPools(t *testing.T) {
	poolDB := &mockPoolDBAPI{
		pools: []string{},
	}

	tasks := &Tasks{
		poolDB: poolDB,
		config: &config.Config{},
	}

	err := tasks.ExecuteEphemeralPoolCleanup(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(poolDB.deletedPools) != 0 {
		t.Errorf("expected 0 deleted pools, got %d", len(poolDB.deletedPools))
	}
}

func TestExecuteEphemeralPoolCleanup_NonEphemeralPoolsSkipped(t *testing.T) {
	poolDB := &mockPoolDBAPI{
		pools: []string{"persistent-pool"},
		poolConfigs: map[string]*PoolConfig{
			"persistent-pool": {
				PoolName:    "persistent-pool",
				Ephemeral:   false,
				LastJobTime: time.Now().Add(-10 * time.Hour), // Old but not ephemeral
			},
		},
	}

	tasks := &Tasks{
		poolDB: poolDB,
		config: &config.Config{},
	}

	err := tasks.ExecuteEphemeralPoolCleanup(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(poolDB.deletedPools) != 0 {
		t.Errorf("expected 0 deleted pools (non-ephemeral skipped), got %d", len(poolDB.deletedPools))
	}
}

func TestExecuteEphemeralPoolCleanup_ActiveEphemeralPoolsSkipped(t *testing.T) {
	poolDB := &mockPoolDBAPI{
		pools: []string{"active-ephemeral"},
		poolConfigs: map[string]*PoolConfig{
			"active-ephemeral": {
				PoolName:    "active-ephemeral",
				Ephemeral:   true,
				LastJobTime: time.Now().Add(-1 * time.Hour), // Active (within TTL)
			},
		},
	}

	tasks := &Tasks{
		poolDB: poolDB,
		config: &config.Config{},
	}

	err := tasks.ExecuteEphemeralPoolCleanup(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(poolDB.deletedPools) != 0 {
		t.Errorf("expected 0 deleted pools (active ephemeral skipped), got %d", len(poolDB.deletedPools))
	}
}

func TestExecuteEphemeralPoolCleanup_InactiveEphemeralPoolsDeleted(t *testing.T) {
	poolDB := &mockPoolDBAPI{
		pools: []string{"stale-ephemeral-1", "stale-ephemeral-2", "active-ephemeral"},
		poolConfigs: map[string]*PoolConfig{
			"stale-ephemeral-1": {
				PoolName:    "stale-ephemeral-1",
				Ephemeral:   true,
				LastJobTime: time.Now().Add(-5 * time.Hour), // Stale (beyond TTL)
			},
			"stale-ephemeral-2": {
				PoolName:    "stale-ephemeral-2",
				Ephemeral:   true,
				LastJobTime: time.Now().Add(-10 * time.Hour), // Stale (beyond TTL)
			},
			"active-ephemeral": {
				PoolName:    "active-ephemeral",
				Ephemeral:   true,
				LastJobTime: time.Now().Add(-1 * time.Hour), // Active (within TTL)
			},
		},
	}

	tasks := &Tasks{
		poolDB: poolDB,
		config: &config.Config{},
	}

	err := tasks.ExecuteEphemeralPoolCleanup(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(poolDB.deletedPools) != 2 {
		t.Errorf("expected 2 deleted pools, got %d: %v", len(poolDB.deletedPools), poolDB.deletedPools)
	}

	// Verify correct pools were deleted
	deletedMap := make(map[string]bool)
	for _, p := range poolDB.deletedPools {
		deletedMap[p] = true
	}
	if !deletedMap["stale-ephemeral-1"] || !deletedMap["stale-ephemeral-2"] {
		t.Errorf("expected stale-ephemeral-1 and stale-ephemeral-2 to be deleted, got %v", poolDB.deletedPools)
	}
	if deletedMap["active-ephemeral"] {
		t.Error("active-ephemeral should not have been deleted")
	}
}

func TestExecuteEphemeralPoolCleanup_ListPoolsError(t *testing.T) {
	poolDB := &mockPoolDBAPI{
		listErr: errors.New("list pools error"),
	}

	tasks := &Tasks{
		poolDB: poolDB,
		config: &config.Config{},
	}

	err := tasks.ExecuteEphemeralPoolCleanup(context.Background())
	if err == nil {
		t.Fatal("expected error from list pools")
	}
}

func TestExecuteEphemeralPoolCleanup_GetPoolConfigError(t *testing.T) {
	poolDB := &mockPoolDBAPI{
		pools:  []string{"pool1"},
		getErr: errors.New("get pool config error"),
	}

	tasks := &Tasks{
		poolDB: poolDB,
		config: &config.Config{},
	}

	// Should not return error, just log and continue
	err := tasks.ExecuteEphemeralPoolCleanup(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteEphemeralPoolCleanup_DeletePoolConfigError(t *testing.T) {
	poolDB := &mockPoolDBAPI{
		pools: []string{"stale-pool"},
		poolConfigs: map[string]*PoolConfig{
			"stale-pool": {
				PoolName:    "stale-pool",
				Ephemeral:   true,
				LastJobTime: time.Now().Add(-10 * time.Hour),
			},
		},
		deleteErr: errors.New("delete pool config error"),
	}

	tasks := &Tasks{
		poolDB: poolDB,
		config: &config.Config{},
	}

	// Should not return error, just log and continue
	err := tasks.ExecuteEphemeralPoolCleanup(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEphemeralPoolTTL(t *testing.T) {
	if EphemeralPoolTTL != 4*time.Hour {
		t.Errorf("expected EphemeralPoolTTL to be 4h, got %v", EphemeralPoolTTL)
	}
}

func strPtr(s string) *string {
	return &s
}
