package housekeeping

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
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

func (m *mockEC2API) DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	m.describeCalls++
	if m.describeErr != nil {
		return nil, m.describeErr
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: m.instances,
	}, nil
}

func (m *mockEC2API) TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
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

func (m *mockTaskSSMAPI) GetParametersByPath(ctx context.Context, params *ssm.GetParametersByPathInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	m.getCalls++
	if m.getErr != nil {
		return nil, m.getErr
	}
	return &ssm.GetParametersByPathOutput{
		Parameters: m.parameters,
	}, nil
}

func (m *mockTaskSSMAPI) DeleteParameter(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
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

func (m *mockTaskDynamoDBAPI) Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	m.queryCalls++
	if m.queryErr != nil {
		return nil, m.queryErr
	}
	return &dynamodb.QueryOutput{
		Items: m.items,
	}, nil
}

func (m *mockTaskDynamoDBAPI) BatchWriteItem(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
	m.batchCalls++
	if m.batchWriteErr != nil {
		return nil, m.batchWriteErr
	}
	return &dynamodb.BatchWriteItemOutput{}, nil
}

func (m *mockTaskDynamoDBAPI) Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
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

func (m *mockTaskMetricsAPI) PublishOrphanedInstancesTerminated(ctx context.Context, count int) error {
	m.orphanedCount = count
	return m.err
}

func (m *mockTaskMetricsAPI) PublishSSMParametersDeleted(ctx context.Context, count int) error {
	m.ssmCount = count
	return m.err
}

func (m *mockTaskMetricsAPI) PublishJobRecordsArchived(ctx context.Context, count int) error {
	m.jobCount = count
	return m.err
}

func (m *mockTaskMetricsAPI) PublishPoolUtilization(ctx context.Context, poolName string, utilization float64) error {
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

func (m *mockCostReporter) GenerateDailyReport(ctx context.Context) error {
	m.calls++
	return m.err
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

func strPtr(s string) *string {
	return &s
}
