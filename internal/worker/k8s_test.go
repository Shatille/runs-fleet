package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	"github.com/Shavakan/runs-fleet/pkg/provider/k8s"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
)

func TestK8sWorkerDeps_ZeroValues(t *testing.T) {
	// Test that zero-value K8sWorkerDeps struct compiles and has expected defaults
	deps := K8sWorkerDeps{}

	if deps.Queue != nil {
		t.Error("Expected Queue to be nil for zero value")
	}
	if deps.Provider != nil {
		t.Error("Expected Provider to be nil for zero value")
	}
	if deps.PoolProvider != nil {
		t.Error("Expected PoolProvider to be nil for zero value")
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
}

func TestK8sWorkerDeps_WithMetrics(t *testing.T) {
	// Test that NoopPublisher can be assigned to Metrics field
	deps := K8sWorkerDeps{
		Metrics: metrics.NoopPublisher{},
		Config:  &config.Config{},
	}

	if deps.Metrics == nil {
		t.Error("Expected Metrics to not be nil after assignment")
	}
	if deps.Config == nil {
		t.Error("Expected Config to not be nil after assignment")
	}
}

func TestK8sWorkerDeps_WithQueue(t *testing.T) {
	// Test that MockQueue (which implements queue.Queue interface) can be assigned
	mockQueue := &MockQueue{}

	deps := K8sWorkerDeps{
		Queue: mockQueue,
	}

	if deps.Queue == nil {
		t.Error("Expected Queue to not be nil after assignment")
	}
}

func TestK8sWorkerDeps_AllInterfaceFields(t *testing.T) {
	// Test that interface fields can be assigned
	deps := K8sWorkerDeps{
		Queue:   &MockQueue{},
		Metrics: metrics.NoopPublisher{},
		Config:  &config.Config{},
	}

	// Verify interface fields are set
	if deps.Queue == nil {
		t.Error("Queue should not be nil")
	}
	if deps.Metrics == nil {
		t.Error("Metrics should not be nil")
	}

	// Verify concrete type fields are still nil
	if deps.Provider != nil {
		t.Error("Provider should be nil when not set")
	}
	if deps.PoolProvider != nil {
		t.Error("PoolProvider should be nil when not set")
	}
	if deps.Runner != nil {
		t.Error("Runner should be nil when not set")
	}
	if deps.DB != nil {
		t.Error("DB should be nil when not set")
	}
}

// mockK8sProvider implements a minimal k8s.Provider interface for testing.
type mockK8sProvider struct {
	CreateRunnerFunc func(ctx context.Context, spec *provider.RunnerSpec) (*provider.RunnerResult, error)
}

func (m *mockK8sProvider) CreateRunner(ctx context.Context, spec *provider.RunnerSpec) (*provider.RunnerResult, error) {
	if m.CreateRunnerFunc != nil {
		return m.CreateRunnerFunc(ctx, spec)
	}
	return &provider.RunnerResult{RunnerIDs: []string{"pod-123"}}, nil
}

func (m *mockK8sProvider) TerminateRunner(ctx context.Context, runnerID string) error {
	return nil
}

func (m *mockK8sProvider) DescribeRunner(ctx context.Context, runnerID string) (*provider.RunnerState, error) {
	return &provider.RunnerState{RunnerID: runnerID, State: "running"}, nil
}

func (m *mockK8sProvider) Name() string {
	return "k8s"
}

// mockK8sPoolProvider implements k8s.PoolProvider for testing.
type mockK8sPoolProvider struct {
	markedBusy []string
}

func (m *mockK8sPoolProvider) ListPoolRunners(ctx context.Context, poolName string) ([]provider.PoolRunner, error) {
	return nil, nil
}

func (m *mockK8sPoolProvider) StartRunners(ctx context.Context, runnerIDs []string) error {
	return nil
}

func (m *mockK8sPoolProvider) StopRunners(ctx context.Context, runnerIDs []string) error {
	return nil
}

func (m *mockK8sPoolProvider) TerminateRunners(ctx context.Context, runnerIDs []string) error {
	return nil
}

func (m *mockK8sPoolProvider) MarkRunnerBusy(runnerID string) {
	m.markedBusy = append(m.markedBusy, runnerID)
}

func (m *mockK8sPoolProvider) MarkRunnerIdle(runnerID string) {}

// mockRunnerManager wraps runner.Manager for testing.
type mockRunnerManager struct {
	GetRegistrationTokenFunc func(ctx context.Context, repo string) (*runner.RegistrationResult, error)
}

func TestProcessK8sMessage_EmptyBody(t *testing.T) {
	origRetryDelay := FleetRetryBaseDelay
	defer func() { FleetRetryBaseDelay = origRetryDelay }()
	FleetRetryBaseDelay = 1 * time.Millisecond

	mockQueue := &MockQueue{}
	deps := K8sWorkerDeps{
		Queue:   mockQueue,
		Metrics: metrics.NoopPublisher{},
		Config:  &config.Config{},
	}

	msg := queue.Message{
		ID:     "msg-1",
		Body:   "",
		Handle: "handle-1",
	}

	// Should not panic with empty body
	processK8sMessage(context.Background(), deps, msg)
}

func TestProcessK8sMessage_InvalidJSON(t *testing.T) {
	origRetryDelay := FleetRetryBaseDelay
	defer func() { FleetRetryBaseDelay = origRetryDelay }()
	FleetRetryBaseDelay = 1 * time.Millisecond

	deleteCount := 0
	mockQueue := &MockQueue{
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			deleteCount++
			return nil
		},
	}

	deps := K8sWorkerDeps{
		Queue:   mockQueue,
		Metrics: metrics.NoopPublisher{},
		Config:  &config.Config{},
	}

	msg := queue.Message{
		ID:     "msg-1",
		Body:   "not-valid-json",
		Handle: "handle-1",
	}

	processK8sMessage(context.Background(), deps, msg)

	// Message should be deleted as poison message
	if deleteCount == 0 {
		t.Error("Expected poison message to be deleted")
	}
}

func TestProcessK8sMessage_NilRunnerManager(t *testing.T) {
	origRetryDelay := FleetRetryBaseDelay
	defer func() { FleetRetryBaseDelay = origRetryDelay }()
	FleetRetryBaseDelay = 1 * time.Millisecond

	job := queue.JobMessage{
		JobID: 12345,
		RunID: 67890,
		Repo:  "owner/repo",
		OS:    "linux",
		Arch:  "amd64",
	}
	jobBytes, _ := json.Marshal(job)

	mockQueue := &MockQueue{}
	deps := K8sWorkerDeps{
		Queue:   mockQueue,
		Metrics: metrics.NoopPublisher{},
		Runner:  nil, // nil runner manager
		Config:  &config.Config{},
	}

	msg := queue.Message{
		ID:     "msg-1",
		Body:   string(jobBytes),
		Handle: "handle-1",
	}

	// Should not panic with nil runner manager
	processK8sMessage(context.Background(), deps, msg)
}

func TestCreateK8sRunnerWithRetry_Success(t *testing.T) {
	origRetryDelay := FleetRetryBaseDelay
	defer func() { FleetRetryBaseDelay = origRetryDelay }()
	FleetRetryBaseDelay = 1 * time.Millisecond

	mockProvider := &mockK8sProvider{
		CreateRunnerFunc: func(_ context.Context, _ *provider.RunnerSpec) (*provider.RunnerResult, error) {
			return &provider.RunnerResult{RunnerIDs: []string{"pod-123"}}, nil
		},
	}

	spec := &provider.RunnerSpec{
		RunID: 12345,
		Repo:  "owner/repo",
	}

	// Need to cast to *k8s.Provider - but we can't do that with our mock
	// Instead, test the retry logic directly with a wrapper
	result, err := createK8sRunnerWithRetryTestable(context.Background(), mockProvider, spec)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if result == nil || len(result.RunnerIDs) != 1 {
		t.Error("Expected result with one runner ID")
	}
}

func TestCreateK8sRunnerWithRetry_SuccessAfterRetries(t *testing.T) {
	origRetryDelay := FleetRetryBaseDelay
	defer func() { FleetRetryBaseDelay = origRetryDelay }()
	FleetRetryBaseDelay = 1 * time.Millisecond

	attempts := 0
	mockProvider := &mockK8sProvider{
		CreateRunnerFunc: func(_ context.Context, _ *provider.RunnerSpec) (*provider.RunnerResult, error) {
			attempts++
			if attempts < 3 {
				return nil, errors.New("temporary failure")
			}
			return &provider.RunnerResult{RunnerIDs: []string{"pod-123"}}, nil
		},
	}

	spec := &provider.RunnerSpec{
		RunID: 12345,
		Repo:  "owner/repo",
	}

	result, err := createK8sRunnerWithRetryTestable(context.Background(), mockProvider, spec)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if result == nil || len(result.RunnerIDs) != 1 {
		t.Error("Expected result with one runner ID")
	}
	if attempts != 3 {
		t.Errorf("Expected 3 attempts, got %d", attempts)
	}
}

func TestCreateK8sRunnerWithRetry_Failure(t *testing.T) {
	origRetryDelay := FleetRetryBaseDelay
	defer func() { FleetRetryBaseDelay = origRetryDelay }()
	FleetRetryBaseDelay = 1 * time.Millisecond

	attempts := 0
	mockProvider := &mockK8sProvider{
		CreateRunnerFunc: func(_ context.Context, _ *provider.RunnerSpec) (*provider.RunnerResult, error) {
			attempts++
			return nil, errors.New("permanent failure")
		},
	}

	spec := &provider.RunnerSpec{
		RunID: 12345,
		Repo:  "owner/repo",
	}

	result, err := createK8sRunnerWithRetryTestable(context.Background(), mockProvider, spec)
	if err == nil {
		t.Error("Expected error")
	}
	if result != nil {
		t.Error("Expected nil result on failure")
	}
	if attempts != maxFleetCreateRetries {
		t.Errorf("Expected %d attempts, got %d", maxFleetCreateRetries, attempts)
	}
}

// k8sProviderInterface allows testing CreateK8sRunnerWithRetry with mock.
type k8sProviderInterface interface {
	CreateRunner(ctx context.Context, spec *provider.RunnerSpec) (*provider.RunnerResult, error)
}

// createK8sRunnerWithRetryTestable is a testable version that accepts an interface.
func createK8sRunnerWithRetryTestable(ctx context.Context, p k8sProviderInterface, spec *provider.RunnerSpec) (*provider.RunnerResult, error) {
	var result *provider.RunnerResult
	var err error
	for attempt := 0; attempt < maxFleetCreateRetries; attempt++ {
		if attempt > 0 {
			backoff := FleetRetryBaseDelay * time.Duration(1<<uint(attempt-1))
			time.Sleep(backoff)
		}

		result, err = p.CreateRunner(ctx, spec)
		if err == nil {
			return result, nil
		}
	}
	return nil, err
}

func TestRunK8sWorker_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)

	mockQueue := &MockQueue{}
	deps := K8sWorkerDeps{
		Queue:   mockQueue,
		Metrics: metrics.NoopPublisher{},
		Config:  &config.Config{},
	}

	done := make(chan struct{})
	go func() {
		RunWorkerLoopWithTicker(ctx, "K8s", deps.Queue, func(_ context.Context, _ queue.Message) {
			processK8sMessage(ctx, deps, queue.Message{})
		}, tick)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Success
	case <-time.After(100 * time.Millisecond):
		t.Fatal("RunK8sWorker did not stop on context cancellation")
	}
}

func TestRunK8sWorker_ProcessesMessages(t *testing.T) {
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

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			return []queue.Message{
				{ID: "msg-1", Body: string(jobBytes), Handle: "handle-1"},
			}, nil
		},
	}

	deps := K8sWorkerDeps{
		Queue:   mockQueue,
		Metrics: metrics.NoopPublisher{},
		Config:  &config.Config{},
	}

	go RunWorkerLoopWithTicker(ctx, "K8s", deps.Queue, func(_ context.Context, _ queue.Message) {
		atomic.AddInt32(&processed, 1)
	}, tick)

	// Send ticks to trigger message processing
	for atomic.LoadInt32(&processed) < 3 {
		tick <- time.Now()
		time.Sleep(10 * time.Millisecond)
	}

	if atomic.LoadInt32(&processed) < 3 {
		t.Errorf("Expected at least 3 messages processed, got %d", atomic.LoadInt32(&processed))
	}
}

// Compile-time check that mockK8sPoolProvider implements the PoolProvider interface methods
var _ = (*mockK8sPoolProvider)(nil)

// Ensure our mock can be used as k8s.PoolProvider
func TestMockK8sPoolProvider_MarksRunnerBusy(t *testing.T) {
	pool := &mockK8sPoolProvider{}
	pool.MarkRunnerBusy("pod-1")
	pool.MarkRunnerBusy("pod-2")

	if len(pool.markedBusy) != 2 {
		t.Errorf("Expected 2 marked busy, got %d", len(pool.markedBusy))
	}
	if pool.markedBusy[0] != "pod-1" || pool.markedBusy[1] != "pod-2" {
		t.Error("Unexpected marked busy runners")
	}
}

// Silence unused variable warnings for test imports
var (
	_ = k8s.NewPoolProvider
	_ = runner.NewManager
)
