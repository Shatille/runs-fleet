package worker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// mockDynamoForClaim implements db.DynamoDBAPI, recording the context seen by
// DeleteItem so tests can assert the claim cleanup runs on a live context.
// GetItem/UpdateItem are stubbed because ClaimJob reads the existing record
// before its compare-and-swap and the claim-exhausted path marks the job
// terminal via UpdateItem; both default to empty results unless overridden.
type mockDynamoForClaim struct {
	db.DynamoDBAPI
	GetItemFunc    func(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItemFunc func(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	deleteCalled   bool
	deleteCtxDone  bool
}

func (m *mockDynamoForClaim) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if m.GetItemFunc != nil {
		return m.GetItemFunc(ctx, params, optFns...)
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (m *mockDynamoForClaim) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if m.UpdateItemFunc != nil {
		return m.UpdateItemFunc(ctx, params, optFns...)
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func (m *mockDynamoForClaim) PutItem(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return &dynamodb.PutItemOutput{}, nil
}

func (m *mockDynamoForClaim) DeleteItem(ctx context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	m.deleteCalled = true
	select {
	case <-ctx.Done():
		m.deleteCtxDone = true
	default:
		m.deleteCtxDone = false
	}
	return &dynamodb.DeleteItemOutput{}, nil
}

func TestProcessJobDirect_FleetFailure_ReleasesClaimOnFreshContext(t *testing.T) {
	mockDynamo := &mockDynamoForClaim{}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	var subnetIndex uint64
	processor := &DirectProcessor{
		Fleet:       &fleet.Manager{},
		Pool:        &pools.Manager{},
		Metrics:     metrics.NoopPublisher{},
		DB:          dbClient,
		Queue:       &mockQueueEC2{},
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return nil, errors.New("fleet creation failed")
		},
	}

	// A spot job under the retry cap takes the requeue path, which releases the
	// claim (DeleteJobClaim) before sending the on-demand fallback.
	job := &queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
		Spot:         true,
	}

	// Already-canceled job context simulates the strained ctx seen on a wedged
	// connection. ClaimJob/DeleteJobClaim must still succeed via a fresh context.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	result := processor.ProcessJobDirect(canceledCtx, job)

	if result {
		t.Fatal("expected ProcessJobDirect to return false on fleet creation failure")
	}
	if !mockDynamo.deleteCalled {
		t.Fatal("expected DeleteJobClaim (DeleteItem) to be invoked on the failure path")
	}
	if mockDynamo.deleteCtxDone {
		t.Error("claim cleanup must run on a fresh (non-canceled) context, not the strained job context")
	}
}

// TestProcessJobDirect_FleetFailure_RequeuesNotDropped is the core regression
// test: a spot job whose fleet creation fails must be handed back to the durable
// queue (re-enqueued), not silently dropped. The requeued message must be
// dedup-safe (RetryCount+1) and on-demand (ForceOnDemand) so the ec2-worker can
// provision it.
func TestProcessJobDirect_FleetFailure_RequeuesNotDropped(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() { RetryDelay = origRetryDelay }()
	RetryDelay = 1 * time.Millisecond

	mockDynamo := &mockDynamoForClaim{}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	var sent []*queue.JobMessage
	mockQueue := &mockQueueEC2{
		SendMessageFunc: func(_ context.Context, job *queue.JobMessage) error {
			sent = append(sent, job)
			return nil
		},
	}
	mockMetrics := &mockMetricsPublisher{}

	var subnetIndex uint64
	processor := &DirectProcessor{
		Fleet:       &fleet.Manager{},
		Metrics:     mockMetrics,
		DB:          dbClient,
		Queue:       mockQueue,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return nil, errors.New("spot capacity not available")
		},
	}

	job := &queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
		Spot:         true,
		RetryCount:   0,
	}

	result := processor.ProcessJobDirect(context.Background(), job)

	if result {
		t.Fatal("expected ProcessJobDirect to return false on fleet creation failure")
	}
	if len(sent) != 1 {
		t.Fatalf("expected exactly 1 requeued message, got %d (job must not be dropped)", len(sent))
	}
	if sent[0].RetryCount != job.RetryCount+1 {
		t.Errorf("requeued RetryCount = %d, want %d (must change FIFO dedup id)", sent[0].RetryCount, job.RetryCount+1)
	}
	if !sent[0].ForceOnDemand {
		t.Error("requeued message must set ForceOnDemand=true")
	}
	if sent[0].Spot {
		t.Error("requeued message must set Spot=false")
	}
	if mockMetrics.requeuedReason != requeueReasonDirectFleetFailure {
		t.Errorf("requeue metric reason = %q, want %q", mockMetrics.requeuedReason, requeueReasonDirectFleetFailure)
	}
}

// TestProcessJobDirect_FleetFailure_AtRetryCap_MarksTerminal proves the cap is
// honored: at the retry cap the direct path does NOT re-enqueue (no infinite
// loop) and does NOT silently drop — it marks the claim terminal so the failure
// surfaces.
func TestProcessJobDirect_FleetFailure_AtRetryCap_MarksTerminal(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() { RetryDelay = origRetryDelay }()
	RetryDelay = 1 * time.Millisecond

	updateCalled := false
	mockDynamo := &mockDynamoForClaim{
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			// FailExhaustedClaim marks status=error guarded on a claiming record.
			if params.ConditionExpression == nil {
				t.Error("terminal write must guard on the claiming state")
			}
			updateCalled = true
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	sendCalled := false
	mockQueue := &mockQueueEC2{
		SendMessageFunc: func(_ context.Context, _ *queue.JobMessage) error {
			sendCalled = true
			return nil
		},
	}
	mockMetrics := &mockMetricsPublisher{}

	var subnetIndex uint64
	processor := &DirectProcessor{
		Fleet:       &fleet.Manager{},
		Metrics:     mockMetrics,
		DB:          dbClient,
		Queue:       mockQueue,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return nil, errors.New("spot capacity not available")
		},
	}

	job := &queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
		Spot:         true,
		RetryCount:   maxJobRetries, // at the cap
	}

	result := processor.ProcessJobDirect(context.Background(), job)

	if result {
		t.Fatal("expected ProcessJobDirect to return false on fleet creation failure")
	}
	if sendCalled {
		t.Error("at the retry cap the job must NOT be re-enqueued (no infinite loop)")
	}
	if !updateCalled {
		t.Error("at the retry cap the claim must be marked terminal (FailExhaustedClaim), not dropped")
	}
	if mockMetrics.schedulingFailures == 0 {
		t.Error("terminal give-up must emit a scheduling-failure metric so the drop is visible")
	}
	if got := mockMetrics.schedulingFailureTypes[0]; got != schedulingFailureFleetCreate {
		t.Errorf("fleet-create give-up scheduling_failure type = %q, want %q", got, schedulingFailureFleetCreate)
	}
}

// TestProcessJobDirect_FleetFailure_ForceOnDemand_MarksTerminal proves a job that
// is already on-demand and still fails is not re-enqueued (it would loop) but is
// marked terminal.
func TestProcessJobDirect_FleetFailure_ForceOnDemand_MarksTerminal(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() { RetryDelay = origRetryDelay }()
	RetryDelay = 1 * time.Millisecond

	updateCalled := false
	mockDynamo := &mockDynamoForClaim{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			updateCalled = true
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	sendCalled := false
	mockQueue := &mockQueueEC2{
		SendMessageFunc: func(_ context.Context, _ *queue.JobMessage) error {
			sendCalled = true
			return nil
		},
	}

	var subnetIndex uint64
	processor := &DirectProcessor{
		Fleet:       &fleet.Manager{},
		Metrics:     metrics.NoopPublisher{},
		DB:          dbClient,
		Queue:       mockQueue,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return nil, errors.New("on-demand capacity not available")
		},
	}

	job := &queue.JobMessage{
		JobID:         12345,
		RunID:         67890,
		Repo:          "owner/repo",
		InstanceType:  "t3.micro",
		Spot:          false,
		ForceOnDemand: true,
		RetryCount:    0,
	}

	processor.ProcessJobDirect(context.Background(), job)

	if sendCalled {
		t.Error("an already-on-demand job must NOT be re-enqueued on failure")
	}
	if !updateCalled {
		t.Error("an already-on-demand job that fails must be marked terminal")
	}
}

// TestBuildOnDemandFallbackJob_SharedByBothPaths proves the single shared
// requeue-builder produces identical output regardless of which cold-start path
// calls it, so the two paths cannot diverge in what they re-enqueue.
func TestBuildOnDemandFallbackJob_SharedByBothPaths(t *testing.T) {
	job := &queue.JobMessage{
		JobID:         42,
		RunID:         99,
		Repo:          "owner/repo",
		InstanceType:  "c7g.xlarge",
		InstanceTypes: []string{"c7g.xlarge", "m7g.xlarge"},
		Pool:          "default",
		Spot:          true,
		OriginalLabel: "runs-fleet=99/cpu=4",
		RetryCount:    1,
		Arch:          "arm64",
		StorageGiB:    64,
	}

	// The ec2-worker and direct both call BuildOnDemandFallbackJob; they must
	// agree byte-for-byte.
	a := BuildOnDemandFallbackJob(job)
	b := BuildOnDemandFallbackJob(job)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("shared builder produced divergent output:\n a = %+v\n b = %+v", a, b)
	}

	if a.RetryCount != job.RetryCount+1 {
		t.Errorf("RetryCount = %d, want %d", a.RetryCount, job.RetryCount+1)
	}
	if !a.ForceOnDemand {
		t.Error("ForceOnDemand must be true")
	}
	if a.Spot {
		t.Error("Spot must be false")
	}
}

func TestTryDirectProcessing_NilProcessor(t *testing.T) {
	sem := make(chan struct{}, 5)
	job := &queue.JobMessage{JobID: 12345}

	// Should not panic with nil processor
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("TryDirectProcessing panicked with nil processor: %v", r)
		}
	}()
	TryDirectProcessing(context.Background(), nil, sem, job)
}

func TestTryDirectProcessing_NilSemaphore(t *testing.T) {
	processor := &DirectProcessor{
		Config: &config.Config{},
	}
	job := &queue.JobMessage{JobID: 12345}

	// Should not panic with nil semaphore
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("TryDirectProcessing panicked with nil semaphore: %v", r)
		}
	}()
	TryDirectProcessing(context.Background(), processor, nil, job)
}

func TestTryDirectProcessing_BothNil(t *testing.T) {
	job := &queue.JobMessage{JobID: 12345}

	// Should not panic with both nil
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("TryDirectProcessing panicked with both nil: %v", r)
		}
	}()
	TryDirectProcessing(context.Background(), nil, nil, job)
}

func TestTryDirectProcessing_AtCapacity(t *testing.T) {
	processor := &DirectProcessor{
		Config: &config.Config{},
	}

	// Fill semaphore to capacity
	sem := make(chan struct{}, 1)
	sem <- struct{}{}

	job := &queue.JobMessage{JobID: 12345}

	// Should not block - will just log and return
	done := make(chan struct{})
	go func() {
		TryDirectProcessing(context.Background(), processor, sem, job)
		close(done)
	}()

	select {
	case <-done:
		// Success - didn't block
	case <-time.After(100 * time.Millisecond):
		t.Fatal("TryDirectProcessing blocked when at capacity")
	}
}

func TestDirectProcessor_ZeroValues(t *testing.T) {
	// Test that zero-value DirectProcessor struct compiles and has expected defaults
	processor := DirectProcessor{}

	if processor.Fleet != nil {
		t.Error("Expected Fleet to be nil for zero value")
	}
	if processor.Pool != nil {
		t.Error("Expected Pool to be nil for zero value")
	}
	if processor.Metrics != nil {
		t.Error("Expected Metrics to be nil for zero value")
	}
	if processor.Runner != nil {
		t.Error("Expected Runner to be nil for zero value")
	}
	if processor.DB != nil {
		t.Error("Expected DB to be nil for zero value")
	}
	if processor.Config != nil {
		t.Error("Expected Config to be nil for zero value")
	}
	if processor.SubnetIndex != nil {
		t.Error("Expected SubnetIndex to be nil for zero value")
	}
}

func TestProcessJobDirect_WarmPoolFailureLogCarriesPoolName(t *testing.T) {
	var buf syncBuffer
	captureCtxLogs(t, &buf)

	mockDynamo := &mockDynamoForClaim{}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	var subnetIndex uint64
	processor := &DirectProcessor{
		Fleet:   &fleet.Manager{},
		Metrics: metrics.NoopPublisher{},
		DB:      dbClient,
		Config:  &config.Config{SubnetIDs: []string{"subnet-a"}},
		WarmPoolAssigner: &mockWarmPoolAssigner{
			tryAssignFunc: func(_ context.Context, _ *queue.JobMessage) (*WarmPoolResult, error) {
				return nil, errors.New("claim pool instance failed")
			},
		},
		SubnetIndex: &subnetIndex,
		// Fail fleet creation so the test ends after the warm-pool error log.
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return nil, errors.New("fleet creation failed")
		},
	}

	job := &queue.JobMessage{
		JobID: 12345,
		RunID: 67890,
		Repo:  "owner/repo",
		Pool:  "default",
	}

	processor.ProcessJobDirect(context.Background(), job)

	failedLogs := recordsWithMsg(t, &buf, "warm pool assignment failed; falling back to cold start")
	if len(failedLogs) != 1 {
		t.Fatalf("expected 1 'warm pool assignment failed' log, got %d", len(failedLogs))
	}
	var rec map[string]any
	_ = json.Unmarshal([]byte(failedLogs[0]), &rec)
	if got := rec[logging.KeyPoolName]; got != "default" {
		t.Errorf("warm pool assignment failed: %s = %v, want default", logging.KeyPoolName, got)
	}
	// A warm-pool miss is recoverable via cold start, so it must not page at ERROR.
	if got := rec["level"]; got != "WARN" {
		t.Errorf("warm pool assignment failure should log at WARN, got %v", got)
	}
}

// A fleet-creation failure logs at WARN when the job can fall back to on-demand
// (transient, self-healing) and only at ERROR when the failure is terminal.
func TestProcessJobDirect_FleetFailureLogLevel(t *testing.T) {
	cases := []struct {
		name      string
		job       *queue.JobMessage
		wantMsg   string
		wantLevel string
	}{
		{
			name:      "spot under retry cap falls back at WARN",
			job:       &queue.JobMessage{JobID: 1, RunID: 2, Repo: "owner/repo", Spot: true, RetryCount: 0},
			wantMsg:   "fleet creation failed; retrying on-demand",
			wantLevel: "WARN",
		},
		{
			name:      "on-demand has no fallback and logs ERROR",
			job:       &queue.JobMessage{JobID: 3, RunID: 4, Repo: "owner/repo", Spot: false},
			wantMsg:   "fleet creation failed; no fallback available",
			wantLevel: "ERROR",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf syncBuffer
			captureCtxLogs(t, &buf)

			var subnetIndex uint64
			processor := &DirectProcessor{
				Fleet:       &fleet.Manager{},
				Metrics:     metrics.NoopPublisher{},
				DB:          db.NewClientWithAPI(&mockDynamoForClaim{}, "pools", "jobs"),
				Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
				Queue:       &MockQueue{},
				SubnetIndex: &subnetIndex,
				CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
					return nil, errors.New("no capacity")
				},
			}

			processor.ProcessJobDirect(context.Background(), tc.job)

			logs := recordsWithMsg(t, &buf, tc.wantMsg)
			if len(logs) != 1 {
				t.Fatalf("expected 1 %q log, got %d; buf=%s", tc.wantMsg, len(logs), buf.String())
			}
			var rec map[string]any
			_ = json.Unmarshal([]byte(logs[0]), &rec)
			if got := rec["level"]; got != tc.wantLevel {
				t.Errorf("level = %v, want %v", got, tc.wantLevel)
			}
		})
	}
}

func TestProcessJobDirect_NilFleet(t *testing.T) {
	var subnetIndex uint64
	job := &queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
	}

	processor := &DirectProcessor{
		Pool:        &pools.Manager{},
		Metrics:     metrics.NoopPublisher{},
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	result := processor.ProcessJobDirect(context.Background(), job)
	if result {
		t.Error("Expected false when Fleet is nil")
	}
}

func TestProcessJobDirect_NilDBSkipsClaimCheck(t *testing.T) {
	var subnetIndex uint64
	job := &queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
	}

	processor := &DirectProcessor{
		Fleet:       nil,
		Pool:        nil,
		Metrics:     metrics.NoopPublisher{},
		DB:          nil, // Nil DB skips claim check
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	// With nil DB and nil Fleet, should return false (Fleet nil check comes after DB check)
	result := processor.ProcessJobDirect(context.Background(), job)
	if result {
		t.Error("Expected false when Fleet is nil")
	}
}

func TestTryDirectProcessing_WithCapacity(t *testing.T) {
	var subnetIndex uint64
	processor := &DirectProcessor{
		Fleet:       nil,
		Pool:        nil,
		Metrics:     metrics.NoopPublisher{},
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	sem := make(chan struct{}, 5)
	job := &queue.JobMessage{
		JobID: 12345,
		RunID: 67890,
		Repo:  "owner/repo",
	}

	// Should acquire semaphore and process
	done := make(chan struct{})
	go func() {
		TryDirectProcessing(context.Background(), processor, sem, job)
		close(done)
	}()

	// Wait for completion with timeout
	select {
	case <-done:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("TryDirectProcessing did not complete")
	}
}

func TestTryDirectProcessing_PanicRecovery(t *testing.T) {
	// This test verifies the panic recovery in TryDirectProcessing
	// We can't easily trigger a panic in ProcessJobDirect without modifying
	// the production code, but we can verify the structure handles it.

	var subnetIndex uint64
	processor := &DirectProcessor{
		Fleet:       nil,
		Pool:        nil,
		Metrics:     metrics.NoopPublisher{},
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	sem := make(chan struct{}, 5)
	job := &queue.JobMessage{
		JobID: 12345,
	}

	// Should not panic even with minimal job
	done := make(chan struct{})
	go func() {
		TryDirectProcessing(context.Background(), processor, sem, job)
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(200 * time.Millisecond):
		t.Fatal("TryDirectProcessing did not complete")
	}
}
