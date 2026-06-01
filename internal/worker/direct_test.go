package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
type mockDynamoForClaim struct {
	db.DynamoDBAPI
	deleteCalled  bool
	deleteCtxDone bool
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
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return nil, errors.New("fleet creation failed")
		},
	}

	job := &queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
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
	var buf bytes.Buffer
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

	failedLogs := recordsWithMsg(t, &buf, "warm pool assignment failed")
	if len(failedLogs) != 1 {
		t.Fatalf("expected 1 'warm pool assignment failed' log, got %d", len(failedLogs))
	}
	var rec map[string]any
	_ = json.Unmarshal([]byte(failedLogs[0]), &rec)
	if got := rec[logging.KeyPoolName]; got != "default" {
		t.Errorf("warm pool assignment failed: %s = %v, want default", logging.KeyPoolName, got)
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

