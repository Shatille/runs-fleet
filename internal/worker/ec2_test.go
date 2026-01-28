package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/queue"
)

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
				PublicSubnetIDs: tt.subnets,
			}
			var index uint64

			for i := 0; i < tt.callCount; i++ {
				result := SelectSubnet(cfg, &index, false)
				if result != tt.expectedOrder[i] {
					t.Errorf("SelectSubnet() call %d = %q, want %q", i, result, tt.expectedOrder[i])
				}
			}
		})
	}
}

func TestSelectSubnet_Concurrent(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"subnet-a", "subnet-b", "subnet-c"},
	}
	var index uint64
	var mu sync.Mutex

	// Call SelectSubnet concurrently
	done := make(chan string, 100)
	for i := 0; i < 100; i++ {
		go func() {
			done <- SelectSubnet(cfg, &index, false)
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

func TestSelectSubnet_PrivateSubnetPriority(t *testing.T) {
	tests := []struct {
		name           string
		publicSubnets  []string
		privateSubnets []string
		publicIP       bool
		wantSubnets    []string
	}{
		{
			name:           "private subnets preferred when both configured",
			publicSubnets:  []string{"pub-a", "pub-b"},
			privateSubnets: []string{"priv-a", "priv-b"},
			publicIP:       false,
			wantSubnets:    []string{"priv-a", "priv-b"},
		},
		{
			name:           "public subnets used when publicIP=true",
			publicSubnets:  []string{"pub-a", "pub-b"},
			privateSubnets: []string{"priv-a", "priv-b"},
			publicIP:       true,
			wantSubnets:    []string{"pub-a", "pub-b"},
		},
		{
			name:           "fallback to public when no private subnets",
			publicSubnets:  []string{"pub-a", "pub-b"},
			privateSubnets: []string{},
			publicIP:       false,
			wantSubnets:    []string{"pub-a", "pub-b"},
		},
		{
			name:           "private-only configuration",
			publicSubnets:  []string{},
			privateSubnets: []string{"priv-a", "priv-b"},
			publicIP:       false,
			wantSubnets:    []string{"priv-a", "priv-b"},
		},
		{
			name:           "publicIP=true returns empty when no public subnets",
			publicSubnets:  []string{},
			privateSubnets: []string{"priv-a"},
			publicIP:       true,
			wantSubnets:    []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				PublicSubnetIDs:  tt.publicSubnets,
				PrivateSubnetIDs: tt.privateSubnets,
			}
			var index uint64

			result := SelectSubnet(cfg, &index, tt.publicIP)

			found := false
			for _, want := range tt.wantSubnets {
				if result == want {
					found = true
					break
				}
			}
			if !found && len(tt.wantSubnets) > 0 {
				t.Errorf("SelectSubnet() = %q, want one of %v", result, tt.wantSubnets)
			}
			if len(tt.wantSubnets) == 0 && result != "" {
				t.Errorf("SelectSubnet() = %q, want empty", result)
			}
		})
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

// fleetManagerInterface allows testing CreateFleetWithRetry with mock.
type fleetManagerInterface interface {
	CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)
}

// createFleetWithRetryTestable mirrors CreateFleetWithRetry logic but accepts an interface for testing.
// SYNC NOTE: Keep retry logic (attempts, backoff) in sync with CreateFleetWithRetry in ec2.go.
func createFleetWithRetryTestable(ctx context.Context, f fleetManagerInterface, spec *fleet.LaunchSpec) ([]string, error) {
	var instanceIDs []string
	var err error
	for attempt := 0; attempt < maxFleetCreateRetries; attempt++ {
		if attempt > 0 {
			backoff := FleetRetryBaseDelay * time.Duration(1<<uint(attempt-1))
			time.Sleep(backoff)
		}

		instanceIDs, err = f.CreateFleet(ctx, spec)
		if err == nil {
			return instanceIDs, nil
		}
	}
	return nil, err
}

func TestCreateFleetWithRetry_Success(t *testing.T) {
	origRetryDelay := FleetRetryBaseDelay
	defer func() { FleetRetryBaseDelay = origRetryDelay }()
	FleetRetryBaseDelay = 1 * time.Millisecond

	mockFleet := &mockFleetManagerEC2{
		CreateFleetFunc: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return []string{"i-123", "i-456"}, nil
		},
	}

	spec := &fleet.LaunchSpec{
		RunID:        12345,
		InstanceType: "t3.micro",
	}

	result, err := createFleetWithRetryTestable(context.Background(), mockFleet, spec)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if len(result) != 2 {
		t.Errorf("Expected 2 instance IDs, got %d", len(result))
	}
}

func TestCreateFleetWithRetry_SuccessAfterRetries(t *testing.T) {
	origRetryDelay := FleetRetryBaseDelay
	defer func() { FleetRetryBaseDelay = origRetryDelay }()
	FleetRetryBaseDelay = 1 * time.Millisecond

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

	result, err := createFleetWithRetryTestable(context.Background(), mockFleet, spec)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if len(result) != 1 {
		t.Errorf("Expected 1 instance ID, got %d", len(result))
	}
	if attempts != 3 {
		t.Errorf("Expected 3 attempts, got %d", attempts)
	}
}

func TestCreateFleetWithRetry_Failure(t *testing.T) {
	origRetryDelay := FleetRetryBaseDelay
	defer func() { FleetRetryBaseDelay = origRetryDelay }()
	FleetRetryBaseDelay = 1 * time.Millisecond

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

	result, err := createFleetWithRetryTestable(context.Background(), mockFleet, spec)
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
		Config:      &config.Config{PublicSubnetIDs: []string{"subnet-a"}},
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
		OS:           "linux",
		Arch:         "amd64",
	}
	jobBytes, _ := json.Marshal(job)

	mockQueue := &mockQueueEC2{}
	var subnetIndex uint64

	deps := EC2WorkerDeps{
		Queue:       mockQueue,
		Fleet:       nil, // nil fleet manager
		Metrics:     metrics.NoopPublisher{},
		Config:      &config.Config{PublicSubnetIDs: []string{"subnet-a"}},
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
		Config:      &config.Config{PublicSubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	done := make(chan struct{})
	go func() {
		RunWorkerLoopWithTicker(ctx, "EC2", deps.Queue, func(_ context.Context, _ queue.Message) {
			processEC2Message(ctx, deps, queue.Message{})
		}, tick)
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
		Config:      &config.Config{PublicSubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	go RunWorkerLoopWithTicker(ctx, "EC2", deps.Queue, func(_ context.Context, _ queue.Message) {
		atomic.AddInt32(&processed, 1)
	}, tick)

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

// Silence unused variable warnings for test imports
var (
	_ = db.ErrJobAlreadyClaimed
	_ = fleet.LaunchSpec{}
)
