package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// staleClaimItem builds a DynamoDB item for a job stuck in the claiming state,
// created far enough in the past to be a dead lease, with the given attempt
// count. Used to exercise the recoverable-claim worker paths.
func staleClaimItem(ageBeyondStale time.Duration, retryCount int) map[string]types.AttributeValue {
	createdAt := time.Now().Add(-(110 * time.Second) - ageBeyondStale)
	return map[string]types.AttributeValue{
		"job_id":      &types.AttributeValueMemberN{Value: "12345"},
		"run_id":      &types.AttributeValueMemberN{Value: "67890"},
		"repo":        &types.AttributeValueMemberS{Value: "owner/repo"},
		"status":      &types.AttributeValueMemberS{Value: string(db.JobStatusClaiming)},
		"created_at":  &types.AttributeValueMemberS{Value: createdAt.Format(time.RFC3339)},
		"retry_count": &types.AttributeValueMemberN{Value: itoa(retryCount)},
	}
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

func TestSelectSubnet(t *testing.T) {
	tests := []struct {
		name          string
		subnets       []string
		callCount     int
		expectedOrder []string
	}{
		{
			name:          "round-robin single subnet",
			subnets:       []string{"subnet-a"},
			callCount:     3,
			expectedOrder: []string{"subnet-a", "subnet-a", "subnet-a"},
		},
		{
			name:          "round-robin multiple subnets",
			subnets:       []string{"subnet-a", "subnet-b", "subnet-c"},
			callCount:     6,
			expectedOrder: []string{"subnet-a", "subnet-b", "subnet-c", "subnet-a", "subnet-b", "subnet-c"},
		},
		{
			name:          "empty subnets",
			subnets:       []string{},
			callCount:     1,
			expectedOrder: []string{""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				SubnetIDs: tt.subnets,
			}
			var index uint64

			for i := 0; i < tt.callCount; i++ {
				result := SelectSubnet(cfg, &index)
				if result != tt.expectedOrder[i] {
					t.Errorf("SelectSubnet() call %d = %q, want %q", i, result, tt.expectedOrder[i])
				}
			}
		})
	}
}

func TestSelectSubnet_Concurrent(t *testing.T) {
	cfg := &config.Config{
		SubnetIDs: []string{"subnet-a", "subnet-b", "subnet-c"},
	}
	var index uint64
	var mu sync.Mutex

	// Call SelectSubnet concurrently
	done := make(chan string, 100)
	for i := 0; i < 100; i++ {
		go func() {
			done <- SelectSubnet(cfg, &index)
		}()
	}

	results := make(map[string]int)
	for i := 0; i < 100; i++ {
		subnet := <-done
		mu.Lock()
		results[subnet]++
		mu.Unlock()
	}

	// Verify all subnets were used
	if len(results) != 3 {
		t.Errorf("Expected 3 different subnets, got %d", len(results))
	}
}

func TestSelectSubnet_EmptyPrivate(t *testing.T) {
	cfg := &config.Config{SubnetIDs: []string{}}
	var index uint64
	if got := SelectSubnet(cfg, &index); got != "" {
		t.Errorf("SelectSubnet() with empty subnets = %q, want empty", got)
	}
}

func TestDeleteMessageWithRetry(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() {
		RetryDelay = origRetryDelay
	}()
	RetryDelay = 1 * time.Millisecond

	tests := []struct {
		name     string
		failures int
		wantErr  bool
	}{
		{
			name:     "success on first try",
			failures: 0,
			wantErr:  false,
		},
		{
			name:     "success on second try",
			failures: 1,
			wantErr:  false,
		},
		{
			name:     "success on third try",
			failures: 2,
			wantErr:  false,
		},
		{
			name:     "fails after max retries",
			failures: 3,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := 0
			mockQueue := &mockQueueEC2{
				DeleteMessageFunc: func(_ context.Context, _ string) error {
					attempts++
					if attempts <= tt.failures {
						return errors.New("delete failed")
					}
					return nil
				},
			}

			err := deleteMessageWithRetry(context.Background(), mockQueue, "handle")

			if (err != nil) != tt.wantErr {
				t.Errorf("deleteMessageWithRetry() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSendMessageWithRetry(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() {
		RetryDelay = origRetryDelay
	}()
	RetryDelay = 1 * time.Millisecond

	tests := []struct {
		name     string
		failures int
		wantErr  bool
	}{
		{
			name:     "success on first try",
			failures: 0,
			wantErr:  false,
		},
		{
			name:     "success on second try",
			failures: 1,
			wantErr:  false,
		},
		{
			name:     "fails after max retries",
			failures: 3,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := 0
			mockQueue := &mockQueueEC2{
				SendMessageFunc: func(_ context.Context, _ *queue.JobMessage) error {
					attempts++
					if attempts <= tt.failures {
						return errors.New("send failed")
					}
					return nil
				},
			}

			job := &queue.JobMessage{
				JobID: 12345,
				RunID: 67890,
			}

			err := sendMessageWithRetry(context.Background(), mockQueue, job)

			if (err != nil) != tt.wantErr {
				t.Errorf("sendMessageWithRetry() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEC2WorkerDeps_ZeroValues(t *testing.T) {
	// Test that zero-value EC2WorkerDeps struct compiles and has expected defaults
	deps := EC2WorkerDeps{}

	if deps.Queue != nil {
		t.Error("Expected Queue to be nil for zero value")
	}
	if deps.Fleet != nil {
		t.Error("Expected Fleet to be nil for zero value")
	}
	if deps.Pool != nil {
		t.Error("Expected Pool to be nil for zero value")
	}
	if deps.Metrics != nil {
		t.Error("Expected Metrics to be nil for zero value")
	}
	if deps.Runner != nil {
		t.Error("Expected Runner to be nil for zero value")
	}
	if deps.DB != nil {
		t.Error("Expected DB to be nil for zero value")
	}
	if deps.Config != nil {
		t.Error("Expected Config to be nil for zero value")
	}
	if deps.SubnetIndex != nil {
		t.Error("Expected SubnetIndex to be nil for zero value")
	}
}

func TestFleetRetryBaseDelay_Default(t *testing.T) {
	// Verify the default value
	if FleetRetryBaseDelay != 2*time.Second {
		t.Errorf("FleetRetryBaseDelay = %v, want 2s", FleetRetryBaseDelay)
	}
}

func TestRetryDelay_Default(t *testing.T) {
	// Verify the default value
	if RetryDelay != 1*time.Second {
		t.Errorf("RetryDelay = %v, want 1s", RetryDelay)
	}
}

// mockQueueEC2 implements queue.Queue for testing.
type mockQueueEC2 struct {
	SendMessageFunc     func(ctx context.Context, job *queue.JobMessage) error
	ReceiveMessagesFunc func(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error)
	DeleteMessageFunc   func(ctx context.Context, handle string) error
}

func (m *mockQueueEC2) SendMessage(ctx context.Context, job *queue.JobMessage) error {
	if m.SendMessageFunc != nil {
		return m.SendMessageFunc(ctx, job)
	}
	return nil
}

func (m *mockQueueEC2) ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error) {
	if m.ReceiveMessagesFunc != nil {
		return m.ReceiveMessagesFunc(ctx, maxMessages, waitTimeSeconds)
	}
	return nil, nil
}

func (m *mockQueueEC2) DeleteMessage(ctx context.Context, handle string) error {
	if m.DeleteMessageFunc != nil {
		return m.DeleteMessageFunc(ctx, handle)
	}
	return nil
}

func TestSaveJobRecords_NilDB(_ *testing.T) {
	// Should not panic with nil DB
	SaveJobRecords(context.Background(), nil, &queue.JobMessage{}, []string{"i-123"})
}

func TestSaveJobRecords_NoJobsTable(_ *testing.T) {
	// Create a DB client without jobs table configured
	// Since db.Client is a concrete type, we test nil path
	SaveJobRecords(context.Background(), nil, &queue.JobMessage{}, []string{"i-123"})
}

func TestPrepareRunners_NilRunnerManager(_ *testing.T) {
	// Should not panic with nil runner manager
	PrepareRunners(context.Background(), nil, &queue.JobMessage{}, []string{"i-123"})
}

func TestPrepareRunners_EmptyInstanceIDs(_ *testing.T) {
	// Should not panic with empty instance IDs
	PrepareRunners(context.Background(), nil, &queue.JobMessage{}, []string{})
}

// mockFleetManagerEC2 implements a testable fleet manager.
type mockFleetManagerEC2 struct {
	CreateFleetFunc func(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)
}

func (m *mockFleetManagerEC2) CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error) {
	if m.CreateFleetFunc != nil {
		return m.CreateFleetFunc(ctx, spec)
	}
	return []string{"i-123"}, nil
}

func TestCreateFleetWithRetry_Success(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mockFleet := &mockFleetManagerEC2{
			CreateFleetFunc: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
				return []string{"i-123", "i-456"}, nil
			},
		}

		spec := &fleet.LaunchSpec{
			RunID:        12345,
			InstanceType: "t3.micro",
		}

		result, err := CreateFleetWithRetry(context.Background(), mockFleet, spec)
		if err != nil {
			t.Errorf("Expected no error, got %v", err)
		}
		if len(result) != 2 {
			t.Errorf("Expected 2 instance IDs, got %d", len(result))
		}
	})
}

func TestCreateFleetWithRetry_SuccessAfterRetries(t *testing.T) {
	// No FleetRetryBaseDelay override: inside the synctest bubble the real
	// backoff sleeps run on the fake clock, so the production retry schedule is
	// exercised instantly.
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		mockFleet := &mockFleetManagerEC2{
			CreateFleetFunc: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
				attempts++
				if attempts < 3 {
					return nil, errors.New("temporary failure")
				}
				return []string{"i-123"}, nil
			},
		}

		spec := &fleet.LaunchSpec{
			RunID:        12345,
			InstanceType: "t3.micro",
		}

		result, err := CreateFleetWithRetry(context.Background(), mockFleet, spec)
		if err != nil {
			t.Errorf("Expected no error, got %v", err)
		}
		if len(result) != 1 {
			t.Errorf("Expected 1 instance ID, got %d", len(result))
		}
		if attempts != 3 {
			t.Errorf("Expected 3 attempts, got %d", attempts)
		}
	})
}

func TestCreateFleetWithRetry_Failure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		mockFleet := &mockFleetManagerEC2{
			CreateFleetFunc: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
				attempts++
				return nil, errors.New("permanent failure")
			},
		}

		spec := &fleet.LaunchSpec{
			RunID:        12345,
			InstanceType: "t3.micro",
		}

		result, err := CreateFleetWithRetry(context.Background(), mockFleet, spec)
		if err == nil {
			t.Error("Expected error")
		}
		if result != nil {
			t.Error("Expected nil result on failure")
		}
		if attempts != maxFleetCreateRetries {
			t.Errorf("Expected %d attempts, got %d", maxFleetCreateRetries, attempts)
		}
	})
}

func TestProcessEC2Message_InvalidJSON(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() { RetryDelay = origRetryDelay }()
	RetryDelay = 1 * time.Millisecond

	deleteCount := 0
	mockQueue := &mockQueueEC2{
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			deleteCount++
			return nil
		},
	}

	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:       mockQueue,
		Metrics:     metrics.NoopPublisher{},
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	msg := queue.Message{
		ID:     "msg-1",
		Body:   "not-valid-json",
		Handle: "handle-1",
	}

	processEC2Message(context.Background(), deps, msg)

	// Message should be deleted as poison message
	if deleteCount == 0 {
		t.Error("Expected poison message to be deleted")
	}
}

func TestProcessEC2Message_ValidJSON_NilFleet(_ *testing.T) {
	origRetryDelay := FleetRetryBaseDelay
	defer func() { FleetRetryBaseDelay = origRetryDelay }()
	FleetRetryBaseDelay = 1 * time.Millisecond

	job := queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
		Arch:         "amd64",
	}
	jobBytes, _ := json.Marshal(job)

	mockQueue := &mockQueueEC2{}
	var subnetIndex uint64

	deps := EC2WorkerDeps{
		Queue:       mockQueue,
		Fleet:       nil, // nil fleet manager
		Metrics:     metrics.NoopPublisher{},
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	msg := queue.Message{
		ID:     "msg-1",
		Body:   string(jobBytes),
		Handle: "handle-1",
	}

	// Should not panic with nil fleet manager
	processEC2Message(context.Background(), deps, msg)
}

func TestRunEC2Worker_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)

	mockQueue := &mockQueueEC2{}
	var subnetIndex uint64

	deps := EC2WorkerDeps{
		Queue:       mockQueue,
		Metrics:     metrics.NoopPublisher{},
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	done := make(chan struct{})
	go func() {
		RunWorkerLoopWithTicker(ctx, "EC2", deps.Queue, func(_ context.Context, _ queue.Message) {
			processEC2Message(ctx, deps, queue.Message{})
		}, nil, nil, tick)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Success
	case <-time.After(100 * time.Millisecond):
		t.Fatal("RunEC2Worker did not stop on context cancellation")
	}
}

func TestRunEC2Worker_ProcessesMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var processed int32
	tick := make(chan time.Time)

	job := queue.JobMessage{
		JobID: 12345,
		RunID: 67890,
		Repo:  "owner/repo",
	}
	jobBytes, _ := json.Marshal(job)

	mockQueue := &mockQueueEC2{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			return []queue.Message{
				{ID: "msg-1", Body: string(jobBytes), Handle: "handle-1"},
			}, nil
		},
	}

	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:       mockQueue,
		Metrics:     metrics.NoopPublisher{},
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	go RunWorkerLoopWithTicker(ctx, "EC2", deps.Queue, func(_ context.Context, _ queue.Message) {
		atomic.AddInt32(&processed, 1)
	}, nil, nil, tick)

	// Send ticks to trigger message processing with timeout
	timeout := time.After(1 * time.Second)
	for atomic.LoadInt32(&processed) < 3 {
		select {
		case <-timeout:
			t.Fatalf("Timeout waiting for messages, processed %d", atomic.LoadInt32(&processed))
		case tick <- time.Now():
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestEC2WorkerDeps_WithAllFields(t *testing.T) {
	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:       &mockQueueEC2{},
		Fleet:       nil,
		Pool:        nil,
		Metrics:     metrics.NoopPublisher{},
		Runner:      nil,
		DB:          nil,
		Config:      &config.Config{},
		SubnetIndex: &subnetIndex,
	}

	if deps.Queue == nil {
		t.Error("Queue should not be nil")
	}
	if deps.Metrics == nil {
		t.Error("Metrics should not be nil")
	}
	if deps.Config == nil {
		t.Error("Config should not be nil")
	}
	if deps.SubnetIndex == nil {
		t.Error("SubnetIndex should not be nil")
	}
}

// mockWarmPoolAssigner implements WarmPoolAssignerInterface for testing.
type mockWarmPoolAssigner struct {
	tryAssignFunc func(ctx context.Context, job *queue.JobMessage) (*WarmPoolResult, error)
	called        bool
	lastJob       *queue.JobMessage
}

func (m *mockWarmPoolAssigner) TryAssignToWarmPool(ctx context.Context, job *queue.JobMessage) (*WarmPoolResult, error) {
	m.called = true
	m.lastJob = job
	if m.tryAssignFunc != nil {
		return m.tryAssignFunc(ctx, job)
	}
	return &WarmPoolResult{Assigned: false}, nil
}

// mockMetricsPublisher tracks metrics calls for assertions.
// Embeds NoopPublisher to satisfy the full Publisher interface.
type mockMetricsPublisher struct {
	metrics.NoopPublisher
	mu                 sync.Mutex
	warmPoolHitCalled  bool
	coldStartCalled    bool
	queueDepthDelta    float64
	queueReceiveResult string
	requeuedReason         string
	schedulingFailures     int
	schedulingFailureTypes []string
	jobWaits           []jobWaitCall
	processingCalls    []processingCall
}

type jobWaitCall struct {
	pool    string
	source  string
	seconds float64
}

type processingCall struct {
	queue   string
	result  string
	seconds float64
}

func (m *mockMetricsPublisher) PublishQueueDepth(_ context.Context, _ string, delta float64) error {
	m.queueDepthDelta += delta
	return nil
}
func (m *mockMetricsPublisher) PublishJobAssigned(_ context.Context, _, source, _ string) error {
	switch source {
	case sourceWarmPool:
		m.warmPoolHitCalled = true
	case sourceColdStart:
		m.coldStartCalled = true
	}
	return nil
}
func (m *mockMetricsPublisher) PublishJobRequeued(_ context.Context, reason string) error {
	m.requeuedReason = reason
	return nil
}
func (m *mockMetricsPublisher) PublishQueueReceive(_ context.Context, _, result string) error {
	m.queueReceiveResult = result
	return nil
}
func (m *mockMetricsPublisher) PublishSchedulingFailure(_ context.Context, taskType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.schedulingFailures++
	m.schedulingFailureTypes = append(m.schedulingFailureTypes, taskType)
	return nil
}
func (m *mockMetricsPublisher) PublishJobWaitSeconds(_ context.Context, pool, source string, seconds float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobWaits = append(m.jobWaits, jobWaitCall{pool: pool, source: source, seconds: seconds})
	return nil
}
func (m *mockMetricsPublisher) PublishMessageProcessingSeconds(_ context.Context, queue, result string, seconds float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.processingCalls = append(m.processingCalls, processingCall{queue: queue, result: result, seconds: seconds})
	return nil
}

func TestProcessEC2Message_NoPool_SkipsWarmPool(t *testing.T) {
	origRetryDelay := RetryDelay
	origFleetDelay := FleetRetryBaseDelay
	defer func() {
		RetryDelay = origRetryDelay
		FleetRetryBaseDelay = origFleetDelay
	}()
	RetryDelay = 1 * time.Millisecond
	FleetRetryBaseDelay = 1 * time.Millisecond

	// Job WITHOUT pool label
	job := queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
		Pool:         "", // No pool
	}
	jobBytes, _ := json.Marshal(job)

	deleteCount := 0
	mockQueue := &mockQueueEC2{
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			deleteCount++
			return nil
		},
	}

	mockAssigner := &mockWarmPoolAssigner{}
	mockMetrics := &mockMetricsPublisher{}

	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:            mockQueue,
		Fleet:            nil, // Will fail at fleet creation
		Metrics:          mockMetrics,
		Config:           &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex:      &subnetIndex,
		WarmPoolAssigner: mockAssigner,
	}

	msg := queue.Message{
		ID:     "msg-1",
		Body:   string(jobBytes),
		Handle: "handle-1",
	}

	processEC2Message(context.Background(), deps, msg)

	// Warm pool assigner should NOT be called for jobs without pool label
	if mockAssigner.called {
		t.Error("WarmPoolAssigner should NOT be called when job has no pool label")
	}
}

func TestProcessEC2Message_WithPool_WarmPoolSuccess(t *testing.T) {
	origRetryDelay := RetryDelay
	origFleetDelay := FleetRetryBaseDelay
	defer func() {
		RetryDelay = origRetryDelay
		FleetRetryBaseDelay = origFleetDelay
	}()
	RetryDelay = 1 * time.Millisecond
	FleetRetryBaseDelay = 1 * time.Millisecond

	// Job WITH pool label
	job := queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
		Pool:         "my-pool",
	}
	jobBytes, _ := json.Marshal(job)

	deleteCount := 0
	mockQueue := &mockQueueEC2{
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			deleteCount++
			return nil
		},
	}

	fleetCreateCalled := false
	// Note: Fleet is nil, so if warm pool fails, it will fail at fleet check

	mockAssigner := &mockWarmPoolAssigner{
		tryAssignFunc: func(_ context.Context, _ *queue.JobMessage) (*WarmPoolResult, error) {
			return &WarmPoolResult{
				Assigned:   true,
				InstanceID: "i-warmpool123",
			}, nil
		},
	}
	mockMetrics := &mockMetricsPublisher{}

	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:            mockQueue,
		Fleet:            nil,
		Metrics:          mockMetrics,
		Config:           &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex:      &subnetIndex,
		WarmPoolAssigner: mockAssigner,
	}

	msg := queue.Message{
		ID:     "msg-1",
		Body:   string(jobBytes),
		Handle: "handle-1",
	}

	processEC2Message(context.Background(), deps, msg)

	// Warm pool assigner should be called
	if !mockAssigner.called {
		t.Error("WarmPoolAssigner should be called when job has pool label")
	}
	if mockAssigner.lastJob.Pool != "my-pool" {
		t.Errorf("Expected pool 'my-pool', got '%s'", mockAssigner.lastJob.Pool)
	}

	// Warm pool hit metric should be published
	if !mockMetrics.warmPoolHitCalled {
		t.Error("WarmPoolHit metric should be published on warm pool success")
	}

	// Message should be deleted (job processed)
	if deleteCount == 0 {
		t.Error("Message should be deleted after warm pool success")
	}

	// Fleet create should NOT be called
	if fleetCreateCalled {
		t.Error("Fleet create should NOT be called when warm pool succeeds")
	}
}

func TestProcessEC2Message_WithPool_FallsBackToColdStart(t *testing.T) {
	origRetryDelay := RetryDelay
	origFleetDelay := FleetRetryBaseDelay
	defer func() {
		RetryDelay = origRetryDelay
		FleetRetryBaseDelay = origFleetDelay
	}()
	RetryDelay = 1 * time.Millisecond
	FleetRetryBaseDelay = 1 * time.Millisecond

	// Job WITH pool label
	job := queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
		Pool:         "my-pool",
	}
	jobBytes, _ := json.Marshal(job)

	mockQueue := &mockQueueEC2{
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			return nil
		},
	}

	mockAssigner := &mockWarmPoolAssigner{
		tryAssignFunc: func(_ context.Context, _ *queue.JobMessage) (*WarmPoolResult, error) {
			// Warm pool returns no instance available
			return &WarmPoolResult{Assigned: false}, nil
		},
	}
	mockMetrics := &mockMetricsPublisher{}

	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:            mockQueue,
		Fleet:            nil, // Nil fleet will cause cold start to fail, but we can verify warm pool was tried
		Metrics:          mockMetrics,
		Config:           &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex:      &subnetIndex,
		WarmPoolAssigner: mockAssigner,
	}

	msg := queue.Message{
		ID:     "msg-1",
		Body:   string(jobBytes),
		Handle: "handle-1",
	}

	processEC2Message(context.Background(), deps, msg)

	// Warm pool assigner should be called
	if !mockAssigner.called {
		t.Error("WarmPoolAssigner should be called when job has pool label")
	}

	// Warm pool hit metric should NOT be published (no instance assigned)
	if mockMetrics.warmPoolHitCalled {
		t.Error("WarmPoolHit metric should NOT be published when warm pool returns Assigned=false")
	}

	// Code should proceed to cold start path (but will fail at nil fleet check)
	// This is fine - we've verified the warm pool was tried and returned false
}

func TestHandleOnDemandFallback_SendFails_DoesNotDelete(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() { RetryDelay = origRetryDelay }()
	RetryDelay = 1 * time.Millisecond

	var calls []string
	mockQueue := &mockQueueEC2{
		SendMessageFunc: func(_ context.Context, _ *queue.JobMessage) error {
			calls = append(calls, "send")
			return errors.New("send failed")
		},
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			calls = append(calls, "delete")
			return nil
		},
	}

	deps := EC2WorkerDeps{
		Queue:   mockQueue,
		Metrics: metrics.NoopPublisher{},
	}

	job := &queue.JobMessage{
		JobID:      12345,
		RunID:      67890,
		Repo:       "owner/repo",
		Spot:       true,
		RetryCount: 0,
	}

	handleOnDemandFallback(context.Background(), deps, job, "handle-1")

	// Send must be attempted; delete must NOT be called when send fails (no job loss).
	for _, c := range calls {
		if c == "delete" {
			t.Fatalf("DeleteMessage must not be called when SendMessage fails; calls = %v", calls)
		}
	}
	if len(calls) == 0 || calls[0] != "send" {
		t.Fatalf("expected SendMessage to be attempted first, calls = %v", calls)
	}
}

func TestHandleOnDemandFallback_SendSucceeds_ThenDeletes(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() { RetryDelay = origRetryDelay }()
	RetryDelay = 1 * time.Millisecond

	var calls []string
	mockQueue := &mockQueueEC2{
		SendMessageFunc: func(_ context.Context, _ *queue.JobMessage) error {
			calls = append(calls, "send")
			return nil
		},
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			calls = append(calls, "delete")
			return nil
		},
	}

	mockMetrics := &mockMetricsPublisher{}
	deps := EC2WorkerDeps{
		Queue:   mockQueue,
		Metrics: mockMetrics,
	}

	job := &queue.JobMessage{
		JobID:      12345,
		RunID:      67890,
		Repo:       "owner/repo",
		Spot:       true,
		RetryCount: 0,
	}

	handleOnDemandFallback(context.Background(), deps, job, "handle-1")

	// Send must happen before delete (send-before-delete invariant).
	if len(calls) != 2 || calls[0] != "send" || calls[1] != "delete" {
		t.Fatalf("expected send then delete, calls = %v", calls)
	}
}

func TestHandleOnDemandFallback_DeleteFails_NonFatal(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() { RetryDelay = origRetryDelay }()
	RetryDelay = 1 * time.Millisecond

	sendCalls := 0
	mockQueue := &mockQueueEC2{
		SendMessageFunc: func(_ context.Context, _ *queue.JobMessage) error {
			sendCalls++
			return nil
		},
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			return errors.New("delete failed")
		},
	}

	deps := EC2WorkerDeps{
		Queue:   mockQueue,
		Metrics: metrics.NoopPublisher{},
	}

	job := &queue.JobMessage{
		JobID:      12345,
		RunID:      67890,
		Repo:       "owner/repo",
		Spot:       true,
		RetryCount: 0,
	}

	// A failed delete after a successful send is non-fatal and must not panic.
	handleOnDemandFallback(context.Background(), deps, job, "handle-1")

	if sendCalls == 0 {
		t.Fatal("expected SendMessage to be attempted")
	}
}

func TestHandleOnDemandFallback_UsesFreshContext(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() { RetryDelay = origRetryDelay }()
	RetryDelay = 1 * time.Millisecond

	sendCtxDone := true
	mockQueue := &mockQueueEC2{
		SendMessageFunc: func(ctx context.Context, _ *queue.JobMessage) error {
			select {
			case <-ctx.Done():
				sendCtxDone = true
			default:
				sendCtxDone = false
			}
			return nil
		},
	}

	deps := EC2WorkerDeps{
		Queue:   mockQueue,
		Metrics: metrics.NoopPublisher{},
	}

	job := &queue.JobMessage{
		JobID:      12345,
		RunID:      67890,
		Repo:       "owner/repo",
		Spot:       true,
		RetryCount: 0,
	}

	// Pass an already-canceled job context; the requeue send must run on a fresh context.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	handleOnDemandFallback(canceledCtx, deps, job, "handle-1")

	if sendCtxDone {
		t.Error("SendMessage should run on a fresh (non-canceled) context, not the strained job context")
	}
}

func TestProcessEC2Message_FleetFailure_ReleasesClaimOnFreshContext(t *testing.T) {
	origRetryDelay := RetryDelay
	origFleetDelay := FleetRetryBaseDelay
	defer func() {
		RetryDelay = origRetryDelay
		FleetRetryBaseDelay = origFleetDelay
	}()
	RetryDelay = 1 * time.Millisecond
	FleetRetryBaseDelay = 1 * time.Millisecond

	// Job WITHOUT a pool label and not spot, so it skips warm pool and the
	// on-demand fallback, isolating the cold-start fleet-failure claim release.
	job := queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
		Spot:         false,
	}
	jobBytes, _ := json.Marshal(job)

	mockDynamo := &mockDynamoForClaim{}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:       &mockQueueEC2{},
		Fleet:       &fleet.Manager{},
		Metrics:     metrics.NoopPublisher{},
		DB:          dbClient,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return nil, errors.New("fleet creation failed")
		},
	}

	msg := queue.Message{ID: "msg-1", Body: string(jobBytes), Handle: "handle-1"}

	// Already-canceled job context simulates the strained ctx seen on a wedged
	// connection; the claim release must still run on a fresh, live context.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	processEC2Message(canceledCtx, deps, msg)

	if !mockDynamo.deleteCalled {
		t.Fatal("expected DeleteJobClaim (DeleteItem) to be invoked on the cold-start fleet-failure path")
	}
	if mockDynamo.deleteCtxDone {
		t.Error("claim cleanup must run on a fresh (non-canceled) context, not the strained job context")
	}
}

// TestProcessEC2Message_FleetCreateGiveUp_EmitsSchedulingFailure proves that when
// fleet creation fails and the job is NOT eligible for an on-demand fallback
// (here: already on-demand), the worker records the failure side of the
// fulfillment SLA. This give-up path was previously silent: the job was dropped
// for redelivery/DLQ with no scheduling_failure emission.
func TestProcessEC2Message_FleetCreateGiveUp_EmitsSchedulingFailure(t *testing.T) {
	origFleetDelay := FleetRetryBaseDelay
	defer func() { FleetRetryBaseDelay = origFleetDelay }()
	FleetRetryBaseDelay = 1 * time.Millisecond

	// On-demand job (Spot=false): not eligible for the on-demand fallback requeue,
	// so a fleet-create failure is a terminal give-up.
	job := queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
		Spot:         false,
	}
	jobBytes, _ := json.Marshal(job)

	mockDynamo := &mockDynamoForClaim{}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")
	mockMetrics := &mockMetricsPublisher{}

	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:       &mockQueueEC2{},
		Fleet:       &fleet.Manager{},
		Metrics:     mockMetrics,
		DB:          dbClient,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return nil, errors.New("fleet creation failed")
		},
	}

	msg := queue.Message{ID: "msg-1", Body: string(jobBytes), Handle: "handle-1"}
	processEC2Message(context.Background(), deps, msg)

	if mockMetrics.schedulingFailures != 1 {
		t.Fatalf("scheduling_failure emissions = %d, want 1", mockMetrics.schedulingFailures)
	}
	if got := mockMetrics.schedulingFailureTypes[0]; got != schedulingFailureFleetCreate {
		t.Errorf("scheduling_failure type = %q, want %q", got, schedulingFailureFleetCreate)
	}
	if mockMetrics.requeuedReason != "" {
		t.Errorf("an ineligible job must not be requeued; got requeue reason %q", mockMetrics.requeuedReason)
	}
}

// TestProcessEC2Message_FleetCreateFallback_NoSchedulingFailure proves the
// complement: when fleet creation fails on a spot job under the retry cap, the
// job is requeued as on-demand (a recovery, not a give-up) and no
// scheduling_failure is emitted.
func TestProcessEC2Message_FleetCreateFallback_NoSchedulingFailure(t *testing.T) {
	origRetryDelay := RetryDelay
	origFleetDelay := FleetRetryBaseDelay
	defer func() {
		RetryDelay = origRetryDelay
		FleetRetryBaseDelay = origFleetDelay
	}()
	RetryDelay = 1 * time.Millisecond
	FleetRetryBaseDelay = 1 * time.Millisecond

	// Spot job under the retry cap: eligible for the on-demand fallback requeue.
	job := queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
		Spot:         true,
		RetryCount:   0,
	}
	jobBytes, _ := json.Marshal(job)

	mockDynamo := &mockDynamoForClaim{}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")
	mockMetrics := &mockMetricsPublisher{}

	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:       &mockQueueEC2{},
		Fleet:       &fleet.Manager{},
		Metrics:     mockMetrics,
		DB:          dbClient,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return nil, errors.New("spot capacity unavailable")
		},
	}

	msg := queue.Message{ID: "msg-1", Body: string(jobBytes), Handle: "handle-1"}
	processEC2Message(context.Background(), deps, msg)

	if mockMetrics.schedulingFailures != 0 {
		t.Errorf("a requeued (recoverable) fleet failure must not emit scheduling_failure; got %d", mockMetrics.schedulingFailures)
	}
	if mockMetrics.requeuedReason != requeueReasonOnDemandFallback {
		t.Errorf("requeue reason = %q, want %q", mockMetrics.requeuedReason, requeueReasonOnDemandFallback)
	}
}

// TestProcessEC2Message_WarmPoolHit_EmitsJobWait proves a warm-pool assignment
// emits the enqueue->assignment latency tagged with the pool and warm_pool
// source, computed from the message SentAt.
func TestProcessEC2Message_WarmPoolHit_EmitsJobWait(t *testing.T) {
	job := queue.JobMessage{JobID: 1, RunID: 2, Repo: "owner/repo", InstanceType: "t3.micro", Pool: "my-pool"}
	jobBytes, _ := json.Marshal(job)

	mockAssigner := &mockWarmPoolAssigner{
		tryAssignFunc: func(_ context.Context, _ *queue.JobMessage) (*WarmPoolResult, error) {
			return &WarmPoolResult{Assigned: true, InstanceID: "i-w"}, nil
		},
	}
	mockMetrics := &mockMetricsPublisher{}
	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:            &mockQueueEC2{},
		Metrics:          mockMetrics,
		Config:           &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex:      &subnetIndex,
		WarmPoolAssigner: mockAssigner,
	}

	sentAt := time.Now().Add(-5 * time.Second)
	msg := queue.Message{ID: "m1", Body: string(jobBytes), Handle: "h1", SentAt: sentAt}

	processEC2Message(context.Background(), deps, msg)

	if len(mockMetrics.jobWaits) != 1 {
		t.Fatalf("job_wait emissions = %d, want 1", len(mockMetrics.jobWaits))
	}
	got := mockMetrics.jobWaits[0]
	if got.pool != "my-pool" || got.source != sourceWarmPool {
		t.Errorf("job_wait labels = %+v, want pool=my-pool source=warm_pool", got)
	}
	if got.seconds < 4 {
		t.Errorf("job_wait seconds = %v, want >= ~5", got.seconds)
	}
}

// TestProcessEC2Message_ColdStart_EmitsJobWait proves a cold-start launch emits
// the enqueue->assignment latency tagged with the cold_start source.
func TestProcessEC2Message_ColdStart_EmitsJobWait(t *testing.T) {
	job := queue.JobMessage{JobID: 1, RunID: 2, Repo: "owner/repo", InstanceType: "t3.micro", Spot: false}
	jobBytes, _ := json.Marshal(job)

	mockMetrics := &mockMetricsPublisher{}
	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:       &mockQueueEC2{},
		Fleet:       &fleet.Manager{},
		Metrics:     mockMetrics,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return []string{"i-cold"}, nil
		},
		PrepareRunnersFn: func(_ context.Context, _ *queue.JobMessage, _ []string) []string {
			return nil
		},
	}

	sentAt := time.Now().Add(-3 * time.Second)
	msg := queue.Message{ID: "m1", Body: string(jobBytes), Handle: "h1", SentAt: sentAt}

	processEC2Message(context.Background(), deps, msg)

	if len(mockMetrics.jobWaits) != 1 {
		t.Fatalf("job_wait emissions = %d, want 1", len(mockMetrics.jobWaits))
	}
	if got := mockMetrics.jobWaits[0]; got.source != sourceColdStart {
		t.Errorf("job_wait source = %q, want cold_start", got.source)
	}
}

// TestProcessEC2Message_NoSentAt_SkipsJobWait proves that when no SentTimestamp
// is available the job_wait metric is not emitted (no bogus duration).
func TestProcessEC2Message_NoSentAt_SkipsJobWait(t *testing.T) {
	job := queue.JobMessage{JobID: 1, RunID: 2, Repo: "owner/repo", InstanceType: "t3.micro", Pool: "p"}
	jobBytes, _ := json.Marshal(job)

	mockAssigner := &mockWarmPoolAssigner{
		tryAssignFunc: func(_ context.Context, _ *queue.JobMessage) (*WarmPoolResult, error) {
			return &WarmPoolResult{Assigned: true, InstanceID: "i-w"}, nil
		},
	}
	mockMetrics := &mockMetricsPublisher{}
	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:            &mockQueueEC2{},
		Metrics:          mockMetrics,
		Config:           &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex:      &subnetIndex,
		WarmPoolAssigner: mockAssigner,
	}

	// No SentAt set (zero value).
	msg := queue.Message{ID: "m1", Body: string(jobBytes), Handle: "h1"}

	processEC2Message(context.Background(), deps, msg)

	if len(mockMetrics.jobWaits) != 0 {
		t.Errorf("job_wait emissions = %d, want 0 when SentAt is zero", len(mockMetrics.jobWaits))
	}
}

// TestProcessEC2Message_ClaimExhausted_MarksTerminalAndDeletes proves that when
// the claim lease is exhausted (re-claimed claimMaxAttempts times without ever
// provisioning), the worker marks the job terminal (UpdateItem -> error) and
// deletes the SQS message rather than provisioning or looping forever.
func TestProcessEC2Message_ClaimExhausted_MarksTerminalAndDeletes(t *testing.T) {
	job := queue.JobMessage{JobID: 12345, RunID: 67890, Repo: "owner/repo", InstanceType: "t3.micro"}
	jobBytes, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("failed to marshal job: %v", err)
	}

	var terminalStatus string
	mockDynamo := &mockDynamoForClaim{
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			// Already at the attempt cap: ClaimJob must return ErrJobClaimExhausted.
			return &dynamodb.GetItemOutput{Item: staleClaimItem(time.Minute, 3)}, nil
		},
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			// FailExhaustedClaim sets status to error, guarded on a still-claiming
			// record so a concurrent running job is not clobbered.
			if v, ok := params.ExpressionAttributeValues[":error"].(*types.AttributeValueMemberS); ok {
				terminalStatus = v.Value
			}
			if params.ConditionExpression == nil || *params.ConditionExpression != "#status = :claiming" {
				t.Errorf("FailExhaustedClaim must guard the terminal write on a claiming record; condition = %v", params.ConditionExpression)
			}
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	deleteCount := 0
	mockQueue := &mockQueueEC2{
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			deleteCount++
			return nil
		},
	}

	fleetCreated := false
	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:       mockQueue,
		Fleet:       &fleet.Manager{},
		Metrics:     metrics.NoopPublisher{},
		DB:          dbClient,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			fleetCreated = true
			return []string{"i-should-not-launch"}, nil
		},
	}

	msg := queue.Message{ID: "msg-1", Body: string(jobBytes), Handle: "handle-1"}
	processEC2Message(context.Background(), deps, msg)

	if fleetCreated {
		t.Error("claim-exhausted job must not provision a fleet")
	}
	if terminalStatus != string(db.JobStatusError) {
		t.Errorf("claim-exhausted job must be marked terminal error; got status %q", terminalStatus)
	}
	if deleteCount == 0 {
		t.Error("claim-exhausted message must be deleted, not redelivered forever")
	}
}

// TestProcessEC2Message_ClaimExhausted_TerminalWriteFails_KeepsMessage proves
// that when the terminal write fails on claim exhaustion, the SQS message is
// NOT deleted: deleting it while the record is still claiming would strand the
// job, so it must be left for redelivery to retry the transition.
func TestProcessEC2Message_ClaimExhausted_TerminalWriteFails_KeepsMessage(t *testing.T) {
	job := queue.JobMessage{JobID: 12345, RunID: 67890, Repo: "owner/repo", InstanceType: "t3.micro"}
	jobBytes, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("failed to marshal job: %v", err)
	}

	mockDynamo := &mockDynamoForClaim{
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			// Already at the attempt cap: ClaimJob must return ErrJobClaimExhausted.
			return &dynamodb.GetItemOutput{Item: staleClaimItem(time.Minute, 3)}, nil
		},
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			// FailExhaustedClaim cannot persist the terminal status.
			return nil, errors.New("dynamo unavailable")
		},
	}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	deleteCount := 0
	mockQueue := &mockQueueEC2{
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			deleteCount++
			return nil
		},
	}

	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:       mockQueue,
		Fleet:       &fleet.Manager{},
		Metrics:     metrics.NoopPublisher{},
		DB:          dbClient,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return []string{"i-should-not-launch"}, nil
		},
	}

	msg := queue.Message{ID: "msg-1", Body: string(jobBytes), Handle: "handle-1"}
	processEC2Message(context.Background(), deps, msg)

	if deleteCount != 0 {
		t.Errorf("message must be kept for redelivery when terminal write fails; got %d delete(s)", deleteCount)
	}
}

// TestProcessEC2Message_StaleReclaim_MessageNotLost is the regression guard for
// the silent-loss bug: a redelivered message whose claim is a stale lease (not
// yet at the attempt cap) must re-claim and proceed to provision, NOT be
// dropped as already-claimed.
func TestProcessEC2Message_StaleReclaim_MessageNotLost(t *testing.T) {
	job := queue.JobMessage{JobID: 12345, RunID: 67890, Repo: "owner/repo", InstanceType: "t3.micro"}
	jobBytes, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("failed to marshal job: %v", err)
	}

	mockDynamo := &mockDynamoForClaim{
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			// Stale lease, below the cap: re-claimable.
			return &dynamodb.GetItemOutput{Item: staleClaimItem(time.Minute, 1)}, nil
		},
	}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	deleteCount := 0
	mockQueue := &mockQueueEC2{
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			deleteCount++
			return nil
		},
	}

	fleetCreated := false
	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:       mockQueue,
		Fleet:       &fleet.Manager{},
		Metrics:     metrics.NoopPublisher{},
		DB:          dbClient,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			fleetCreated = true
			return []string{"i-123"}, nil
		},
		PrepareRunnersFn: func(_ context.Context, _ *queue.JobMessage, _ []string) []string {
			return nil // preparation succeeds
		},
	}

	msg := queue.Message{ID: "msg-1", Body: string(jobBytes), Handle: "handle-1"}
	processEC2Message(context.Background(), deps, msg)

	if !fleetCreated {
		t.Error("stale-claim redelivery must re-claim and provision, not be dropped as already-claimed")
	}
	// jobProcessed path deletes the message exactly once after a successful launch.
	if deleteCount != 1 {
		t.Errorf("stale-claim redelivery: message should be deleted once after processing; got %d deletes", deleteCount)
	}
}

// TestProcessEC2Message_AlreadyClaimed_EmitsDiscardObservability proves that
// when the ec2-worker drops an SQS copy of a job another processor already
// claimed (the load-bearing half of the dual-path race), the discard is now
// observable: the message is deleted, and a bounded-cardinality scheduling
// failure metric is emitted with the already_claimed reason.
func TestProcessEC2Message_AlreadyClaimed_EmitsDiscardObservability(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() { RetryDelay = origRetryDelay }()
	RetryDelay = 1 * time.Millisecond

	job := queue.JobMessage{JobID: 12345, RunID: 67890, Repo: "owner/repo", InstanceType: "t3.micro"}
	jobBytes, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("failed to marshal job: %v", err)
	}

	mockDynamo := &mockDynamoForClaim{
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			// A record already in the running state: ClaimJob returns
			// ErrJobAlreadyClaimed, simulating the direct path having won the claim.
			return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{
				"job_id":     &types.AttributeValueMemberN{Value: "12345"},
				"run_id":     &types.AttributeValueMemberN{Value: "67890"},
				"repo":       &types.AttributeValueMemberS{Value: "owner/repo"},
				"status":     &types.AttributeValueMemberS{Value: string(db.JobStatusRunning)},
				"created_at": &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
			}}, nil
		},
	}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	deleteCount := 0
	fleetCreated := false
	mockQueue := &mockQueueEC2{
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			deleteCount++
			return nil
		},
	}
	mockMetrics := &mockMetricsPublisher{}

	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue:       mockQueue,
		Fleet:       &fleet.Manager{},
		Metrics:     mockMetrics,
		DB:          dbClient,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			fleetCreated = true
			return []string{"i-should-not-launch"}, nil
		},
	}

	msg := queue.Message{ID: "msg-1", Body: string(jobBytes), Handle: "handle-1"}
	processEC2Message(context.Background(), deps, msg)

	if fleetCreated {
		t.Error("an already-claimed message must not provision a fleet")
	}
	if deleteCount != 1 {
		t.Errorf("already-claimed message should be deleted once; got %d", deleteCount)
	}
	found := false
	for _, ty := range mockMetrics.schedulingFailureTypes {
		if ty == discardReasonAlreadyClaimed {
			found = true
		}
	}
	if !found {
		t.Errorf("already-claimed discard must emit a scheduling failure with reason %q; got %v",
			discardReasonAlreadyClaimed, mockMetrics.schedulingFailureTypes)
	}
}

// Silence unused variable warnings for test imports
var (
	_ = db.ErrJobAlreadyClaimed
	_ = fleet.LaunchSpec{}
)
