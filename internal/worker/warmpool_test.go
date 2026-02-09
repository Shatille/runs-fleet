package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Test constants
const (
	testInstanceStopped = "i-stopped1"
	testInstanceRunning = "i-running1"
)

func TestTryAssignToWarmPool_NoPool(t *testing.T) {
	assigner := &WarmPoolAssigner{}

	job := &queue.JobMessage{
		JobID: 123,
		RunID: 456,
		Pool:  "", // No pool specified
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Assigned {
		t.Error("expected not assigned when no pool specified")
	}
}

func TestTryAssignToWarmPool_NilManagers(t *testing.T) {
	assigner := &WarmPoolAssigner{
		Pool:   nil,
		Runner: nil,
	}

	job := &queue.JobMessage{
		JobID: 123,
		RunID: 456,
		Pool:  "test-pool",
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Assigned {
		t.Error("expected not assigned when managers are nil")
	}
}

// mockEC2API implements pools.EC2API for testing
type mockEC2API struct {
	describeInstancesFunc          func(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	startInstancesFunc             func(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	stopInstancesFunc              func(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	terminateInstancesFunc         func(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	cancelSpotInstanceRequestsFunc func(ctx context.Context, params *ec2.CancelSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error)
	createTagsFunc                 func(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

func (m *mockEC2API) DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if m.describeInstancesFunc != nil {
		return m.describeInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.DescribeInstancesOutput{}, nil
}

func (m *mockEC2API) StartInstances(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	if m.startInstancesFunc != nil {
		return m.startInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.StartInstancesOutput{}, nil
}

func (m *mockEC2API) StopInstances(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	if m.stopInstancesFunc != nil {
		return m.stopInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.StopInstancesOutput{}, nil
}

func (m *mockEC2API) TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	if m.terminateInstancesFunc != nil {
		return m.terminateInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (m *mockEC2API) CancelSpotInstanceRequests(ctx context.Context, params *ec2.CancelSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	if m.cancelSpotInstanceRequestsFunc != nil {
		return m.cancelSpotInstanceRequestsFunc(ctx, params, optFns...)
	}
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

func (m *mockEC2API) CreateTags(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	if m.createTagsFunc != nil {
		return m.createTagsFunc(ctx, params, optFns...)
	}
	return &ec2.CreateTagsOutput{}, nil
}

// mockDBClient implements pools.DBClient interface for testing
type mockDBClient struct {
	claimInstanceForJobFunc  func(ctx context.Context, instanceID string, jobID int64, ttl time.Duration) error
	releaseInstanceClaimFunc func(ctx context.Context, instanceID string, jobID int64) error
}

func (m *mockDBClient) GetPoolConfig(_ context.Context, _ string) (*db.PoolConfig, error) {
	return nil, nil
}

func (m *mockDBClient) UpdatePoolState(_ context.Context, _ string, _, _ int) error {
	return nil
}

func (m *mockDBClient) ListPools(_ context.Context) ([]string, error) {
	return nil, nil
}

func (m *mockDBClient) GetPoolP90Concurrency(_ context.Context, _ string, _ int) (int, error) {
	return 0, nil
}

func (m *mockDBClient) GetPoolBusyInstanceIDs(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockDBClient) AcquirePoolReconcileLock(_ context.Context, _, _ string, _ time.Duration) error {
	return nil
}

func (m *mockDBClient) ReleasePoolReconcileLock(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockDBClient) ClaimInstanceForJob(ctx context.Context, instanceID string, jobID int64, ttl time.Duration) error {
	if m.claimInstanceForJobFunc != nil {
		return m.claimInstanceForJobFunc(ctx, instanceID, jobID, ttl)
	}
	return nil
}

func (m *mockDBClient) ReleaseInstanceClaim(ctx context.Context, instanceID string, jobID int64) error {
	if m.releaseInstanceClaimFunc != nil {
		return m.releaseInstanceClaimFunc(ctx, instanceID, jobID)
	}
	return nil
}

func (m *mockDBClient) SaveSpotRequestID(_ context.Context, _, _ string, _ bool) error {
	return nil
}

func (m *mockDBClient) GetSpotRequestIDs(_ context.Context, _ []string) (map[string]db.SpotRequestInfo, error) {
	return make(map[string]db.SpotRequestInfo), nil
}

// createTestAssigner creates a WarmPoolAssigner with mock EC2 for testing
func createTestAssigner(instances []ec2types.Instance) *WarmPoolAssigner {
	mockEC2 := &mockEC2API{
		describeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{Instances: instances}},
			}, nil
		},
	}

	mockDB := &mockDBClient{}
	poolManager := pools.NewManager(mockDB, nil, &config.Config{})
	poolManager.SetEC2Client(mockEC2)

	return &WarmPoolAssigner{
		Pool:   poolManager,
		Runner: nil,
		DB:     nil,
	}
}

func TestWarmPoolAssigner_NoAvailableInstance(t *testing.T) {
	instances := []ec2types.Instance{
		{
			InstanceId:   aws.String(testInstanceRunning),
			InstanceType: ec2types.InstanceTypeC7gXlarge,
			State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		},
	}
	assigner := createTestAssigner(instances)

	job := &queue.JobMessage{
		JobID: 123,
		RunID: 456,
		Repo:  "owner/repo",
		Pool:  "test-pool",
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Assigned {
		t.Error("expected job to NOT be assigned when no stopped instances")
	}
}

func TestWarmPoolAssigner_NoRunnerManager(t *testing.T) {
	instances := []ec2types.Instance{
		{
			InstanceId:   aws.String(testInstanceStopped),
			InstanceType: ec2types.InstanceTypeC7gXlarge,
			State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
		},
	}
	assigner := createTestAssigner(instances)

	job := &queue.JobMessage{
		JobID: 123,
		RunID: 456,
		Repo:  "owner/repo",
		Pool:  "test-pool",
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With nil Runner, should not assign (early return)
	if result.Assigned {
		t.Error("expected not assigned when Runner is nil")
	}
}

func TestWarmPoolAssigner_EmptyPool(t *testing.T) {
	assigner := createTestAssigner(nil)

	job := &queue.JobMessage{
		JobID: 123,
		RunID: 456,
		Repo:  "owner/repo",
		Pool:  "empty-pool",
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Assigned {
		t.Error("expected job to NOT be assigned from empty pool")
	}
}

// mockPoolManager implements PoolManager for testing
type mockPoolManager struct {
	claimAndStartFunc func(ctx context.Context, poolName string, jobID int64, repo string) (*pools.AvailableInstance, error)
	stopFunc          func(ctx context.Context, instanceID string) error
	stopCalled        bool
	stoppedInstanceID string
}

func (m *mockPoolManager) ClaimAndStartPoolInstance(ctx context.Context, poolName string, jobID int64, repo string) (*pools.AvailableInstance, error) {
	if m.claimAndStartFunc != nil {
		return m.claimAndStartFunc(ctx, poolName, jobID, repo)
	}
	return nil, pools.ErrNoAvailableInstance
}

func (m *mockPoolManager) StopPoolInstance(ctx context.Context, instanceID string) error {
	m.stopCalled = true
	m.stoppedInstanceID = instanceID
	if m.stopFunc != nil {
		return m.stopFunc(ctx, instanceID)
	}
	return nil
}

// mockRunnerPreparer implements RunnerPreparer for testing
type mockRunnerPreparer struct {
	prepareFunc   func(ctx context.Context, req runner.PrepareRunnerRequest) error
	prepareCalled bool
	lastRequest   runner.PrepareRunnerRequest
}

func (m *mockRunnerPreparer) PrepareRunner(ctx context.Context, req runner.PrepareRunnerRequest) error {
	m.prepareCalled = true
	m.lastRequest = req
	if m.prepareFunc != nil {
		return m.prepareFunc(ctx, req)
	}
	return nil
}

// mockJobDBClient implements JobDBClient for testing
type mockJobDBClient struct {
	hasJobsTable        bool
	saveJobFunc         func(ctx context.Context, job *db.JobRecord) error
	releaseClaimFunc    func(ctx context.Context, instanceID string, jobID int64) error
	saveJobCalled       bool
	releaseClaimCalled  bool
	lastSavedJob        *db.JobRecord
	releasedInstanceID  string
}

func (m *mockJobDBClient) HasJobsTable() bool {
	return m.hasJobsTable
}

func (m *mockJobDBClient) SaveJob(ctx context.Context, job *db.JobRecord) error {
	m.saveJobCalled = true
	m.lastSavedJob = job
	if m.saveJobFunc != nil {
		return m.saveJobFunc(ctx, job)
	}
	return nil
}

func (m *mockJobDBClient) ReleaseInstanceClaim(ctx context.Context, instanceID string, jobID int64) error {
	m.releaseClaimCalled = true
	m.releasedInstanceID = instanceID
	if m.releaseClaimFunc != nil {
		return m.releaseClaimFunc(ctx, instanceID, jobID)
	}
	return nil
}

// TestTryAssignToWarmPool_Success_FullFlow tests the complete success path:
// claim instance -> start -> prepare runner -> save job record
func TestTryAssignToWarmPool_Success_FullFlow(t *testing.T) {
	mockPool := &mockPoolManager{
		claimAndStartFunc: func(_ context.Context, _ string, _ int64, _ string) (*pools.AvailableInstance, error) {
			return &pools.AvailableInstance{
				InstanceID:   "i-success123",
				InstanceType: "c7g.xlarge",
			}, nil
		},
	}

	mockRunner := &mockRunnerPreparer{}

	mockDB := &mockJobDBClient{
		hasJobsTable: true,
	}

	assigner := &WarmPoolAssigner{
		Pool:   mockPool,
		Runner: mockRunner,
		DB:     mockDB,
	}

	job := &queue.JobMessage{
		JobID: 12345,
		RunID: 67890,
		Repo:  "owner/repo",
		Pool:  "test-pool",
		Spot:  true,
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify assignment succeeded
	if !result.Assigned {
		t.Error("expected job to be assigned")
	}
	if result.InstanceID != "i-success123" {
		t.Errorf("InstanceID = %s, want i-success123", result.InstanceID)
	}

	// Verify runner was prepared
	if !mockRunner.prepareCalled {
		t.Error("PrepareRunner should be called")
	}
	if mockRunner.lastRequest.InstanceID != "i-success123" {
		t.Errorf("PrepareRunner InstanceID = %s, want i-success123", mockRunner.lastRequest.InstanceID)
	}

	// Verify job was saved
	if !mockDB.saveJobCalled {
		t.Error("SaveJob should be called")
	}
	if mockDB.lastSavedJob.JobID != 12345 {
		t.Errorf("Saved JobID = %d, want 12345", mockDB.lastSavedJob.JobID)
	}
	if !mockDB.lastSavedJob.WarmPoolHit {
		t.Error("WarmPoolHit should be true")
	}

	// Verify instance was NOT stopped (success path)
	if mockPool.stopCalled {
		t.Error("StopPoolInstance should NOT be called on success")
	}
}

// TestTryAssignToWarmPool_SSMPrepFailure_StopsInstance tests that when
// SSM preparation fails, the instance is stopped and claim is released.
func TestTryAssignToWarmPool_SSMPrepFailure_StopsInstance(t *testing.T) {
	mockPool := &mockPoolManager{
		claimAndStartFunc: func(_ context.Context, _ string, _ int64, _ string) (*pools.AvailableInstance, error) {
			return &pools.AvailableInstance{
				InstanceID:   "i-ssmfail",
				InstanceType: "c7g.xlarge",
			}, nil
		},
	}

	mockRunner := &mockRunnerPreparer{
		prepareFunc: func(_ context.Context, _ runner.PrepareRunnerRequest) error {
			return errors.New("SSM parameter store error")
		},
	}

	mockDB := &mockJobDBClient{
		hasJobsTable: true,
	}

	assigner := &WarmPoolAssigner{
		Pool:   mockPool,
		Runner: mockRunner,
		DB:     mockDB,
	}

	job := &queue.JobMessage{
		JobID: 12345,
		RunID: 67890,
		Repo:  "owner/repo",
		Pool:  "test-pool",
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify assignment failed gracefully (not an error, just not assigned)
	if result.Assigned {
		t.Error("expected job to NOT be assigned after SSM prep failure")
	}

	// Verify instance was stopped
	if !mockPool.stopCalled {
		t.Error("StopPoolInstance should be called after SSM prep failure")
	}
	if mockPool.stoppedInstanceID != "i-ssmfail" {
		t.Errorf("Stopped instance = %s, want i-ssmfail", mockPool.stoppedInstanceID)
	}

	// Verify claim was released
	if !mockDB.releaseClaimCalled {
		t.Error("ReleaseInstanceClaim should be called after SSM prep failure")
	}

	// Verify job was NOT saved (failure before save)
	if mockDB.saveJobCalled {
		t.Error("SaveJob should NOT be called after SSM prep failure")
	}
}

// TestTryAssignToWarmPool_DBSaveFailure_StopsInstance tests that when
// job record save fails, the claim is released and instance is stopped.
func TestTryAssignToWarmPool_DBSaveFailure_StopsInstance(t *testing.T) {
	mockPool := &mockPoolManager{
		claimAndStartFunc: func(_ context.Context, _ string, _ int64, _ string) (*pools.AvailableInstance, error) {
			return &pools.AvailableInstance{
				InstanceID:   "i-dbfail",
				InstanceType: "c7g.xlarge",
			}, nil
		},
	}

	mockRunner := &mockRunnerPreparer{} // PrepareRunner succeeds

	mockDB := &mockJobDBClient{
		hasJobsTable: true,
		saveJobFunc: func(_ context.Context, _ *db.JobRecord) error {
			return errors.New("DynamoDB write error")
		},
	}

	assigner := &WarmPoolAssigner{
		Pool:   mockPool,
		Runner: mockRunner,
		DB:     mockDB,
	}

	job := &queue.JobMessage{
		JobID: 12345,
		RunID: 67890,
		Repo:  "owner/repo",
		Pool:  "test-pool",
	}

	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify assignment failed gracefully
	if result.Assigned {
		t.Error("expected job to NOT be assigned after DB save failure")
	}

	// Verify PrepareRunner was called (it succeeded)
	if !mockRunner.prepareCalled {
		t.Error("PrepareRunner should be called before DB save")
	}

	// Verify claim was released BEFORE stopping instance
	if !mockDB.releaseClaimCalled {
		t.Error("ReleaseInstanceClaim should be called after DB save failure")
	}

	// Verify instance was stopped
	if !mockPool.stopCalled {
		t.Error("StopPoolInstance should be called after DB save failure")
	}
	if mockPool.stoppedInstanceID != "i-dbfail" {
		t.Errorf("Stopped instance = %s, want i-dbfail", mockPool.stoppedInstanceID)
	}
}
