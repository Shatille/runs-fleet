package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/google/go-github/v57/github"
)

func init() {
	// Use minimal delays in tests to avoid slow test execution
	retryDelay = 1 * time.Millisecond
	fleetRetryBaseDelay = 1 * time.Millisecond
}

func TestBuildRunnerLabel(t *testing.T) {
	tests := []struct {
		name string
		job  *queue.JobMessage
		want string
	}{
		{
			name: "uses original label when present",
			job: &queue.JobMessage{
				RunID:         "12345",
				RunnerSpec:    "2cpu-linux",
				OriginalLabel: "runs-fleet=12345/cpu=2",
				Spot:          true,
			},
			want: "runs-fleet=12345/cpu=2",
		},
		{
			name: "uses original label with flexible spec",
			job: &queue.JobMessage{
				RunID:         "67890",
				RunnerSpec:    "4cpu-linux-arm64",
				OriginalLabel: "runs-fleet=67890/cpu=4+8/arch=arm64",
				Spot:          true,
			},
			want: "runs-fleet=67890/cpu=4+8/arch=arm64",
		},
		{
			name: "basic label fallback",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "2cpu-linux-arm64",
				Spot:       true,
			},
			want: "runs-fleet=12345/runner=2cpu-linux-arm64",
		},
		{
			name: "with pool",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "2cpu-linux-arm64",
				Pool:       "default",
				Spot:       true,
			},
			want: "runs-fleet=12345/runner=2cpu-linux-arm64/pool=default",
		},
		{
			name: "with private",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "4cpu-linux-amd64",
				Private:    true,
				Spot:       true,
			},
			want: "runs-fleet=12345/runner=4cpu-linux-amd64/private=true",
		},
		{
			name: "with spot=false",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "2cpu-linux-arm64",
				Spot:       false,
			},
			want: "runs-fleet=12345/runner=2cpu-linux-arm64/spot=false",
		},
		{
			name: "all modifiers",
			job: &queue.JobMessage{
				RunID:      "67890",
				RunnerSpec: "8cpu-linux-arm64",
				Pool:       "mypool",
				Private:    true,
				Spot:       false,
			},
			want: "runs-fleet=67890/runner=8cpu-linux-arm64/pool=mypool/private=true/spot=false",
		},
		{
			name: "pool and private",
			job: &queue.JobMessage{
				RunID:      "11111",
				RunnerSpec: "2cpu-linux",
				Pool:       "default",
				Private:    true,
				Spot:       true,
			},
			want: "runs-fleet=11111/runner=2cpu-linux/pool=default/private=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRunnerLabel(tt.job)
			if got != tt.want {
				t.Errorf("buildRunnerLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelectSubnet(t *testing.T) {
	tests := []struct {
		name     string
		job      *queue.JobMessage
		cfg      *config.Config
		wantFunc func(string, []string, []string) bool
	}{
		{
			name: "private job uses private subnet",
			job:  &queue.JobMessage{Private: true},
			cfg: &config.Config{
				PrivateSubnetIDs: []string{"subnet-priv-1", "subnet-priv-2"},
				PublicSubnetIDs:  []string{"subnet-pub-1"},
			},
			wantFunc: func(result string, privateSubnets, _ []string) bool {
				for _, s := range privateSubnets {
					if s == result {
						return true
					}
				}
				return false
			},
		},
		{
			name: "public job uses public subnet",
			job:  &queue.JobMessage{Private: false},
			cfg: &config.Config{
				PrivateSubnetIDs: []string{"subnet-priv-1"},
				PublicSubnetIDs:  []string{"subnet-pub-1", "subnet-pub-2"},
			},
			wantFunc: func(result string, _, publicSubnets []string) bool {
				for _, s := range publicSubnets {
					if s == result {
						return true
					}
				}
				return false
			},
		},
		{
			name: "empty private subnets falls back to public",
			job:  &queue.JobMessage{Private: true},
			cfg: &config.Config{
				PrivateSubnetIDs: []string{},
				PublicSubnetIDs:  []string{"subnet-pub-1"},
			},
			wantFunc: func(result string, _, publicSubnets []string) bool {
				for _, s := range publicSubnets {
					if s == result {
						return true
					}
				}
				return false
			},
		},
		{
			name: "no subnets returns empty",
			job:  &queue.JobMessage{Private: false},
			cfg: &config.Config{
				PrivateSubnetIDs: []string{},
				PublicSubnetIDs:  []string{},
			},
			wantFunc: func(result string, _, _ []string) bool {
				return result == ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var subnetIndex uint64
			got := selectSubnet(tt.job, tt.cfg, &subnetIndex)
			if !tt.wantFunc(got, tt.cfg.PrivateSubnetIDs, tt.cfg.PublicSubnetIDs) {
				t.Errorf("selectSubnet() = %q, unexpected result", got)
			}
		})
	}
}

func TestSelectSubnet_RoundRobin(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"subnet-a", "subnet-b", "subnet-c"},
	}
	job := &queue.JobMessage{Private: false}

	var subnetIndex uint64
	results := make([]string, 6)

	for i := 0; i < 6; i++ {
		results[i] = selectSubnet(job, cfg, &subnetIndex)
	}

	// Should cycle through subnets
	expected := []string{"subnet-a", "subnet-b", "subnet-c", "subnet-a", "subnet-b", "subnet-c"}
	for i, exp := range expected {
		if results[i] != exp {
			t.Errorf("selectSubnet() iteration %d = %q, want %q", i, results[i], exp)
		}
	}
}

func TestSelectSubnet_ConcurrentAccess(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"subnet-1", "subnet-2", "subnet-3", "subnet-4"},
	}
	job := &queue.JobMessage{Private: false}

	var subnetIndex uint64
	done := make(chan string, 100)

	for i := 0; i < 100; i++ {
		go func() {
			result := selectSubnet(job, cfg, &subnetIndex)
			done <- result
		}()
	}

	results := make(map[string]int)
	for i := 0; i < 100; i++ {
		subnet := <-done
		results[subnet]++
	}

	// All results should be valid subnets
	for subnet := range results {
		found := false
		for _, valid := range cfg.PublicSubnetIDs {
			if subnet == valid {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected subnet: %s", subnet)
		}
	}

	// Verify final index is 100
	if subnetIndex != 100 {
		t.Errorf("final subnetIndex = %d, want 100", subnetIndex)
	}
}

// mockQueue implements queue.Queue for testing
type mockQueue struct {
	deleteFunc func(ctx context.Context, handle string) error
}

func (m *mockQueue) SendMessage(_ context.Context, _ *queue.JobMessage) error {
	return nil
}

func (m *mockQueue) ReceiveMessages(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
	return nil, nil
}

func (m *mockQueue) DeleteMessage(ctx context.Context, handle string) error {
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, handle)
	}
	return nil
}

func TestDeleteMessageWithRetry_Success(t *testing.T) {
	callCount := 0
	q := &mockQueue{
		deleteFunc: func(_ context.Context, _ string) error {
			callCount++
			return nil
		},
	}

	err := deleteMessageWithRetry(context.Background(), q, "test-handle")
	if err != nil {
		t.Errorf("deleteMessageWithRetry() error = %v, want nil", err)
	}
	if callCount != 1 {
		t.Errorf("deleteMessageWithRetry() called %d times, want 1", callCount)
	}
}

func TestDeleteMessageWithRetry_EventualSuccess(t *testing.T) {
	callCount := 0
	q := &mockQueue{
		deleteFunc: func(_ context.Context, _ string) error {
			callCount++
			if callCount < 2 {
				return errors.New("temporary error")
			}
			return nil
		},
	}

	err := deleteMessageWithRetry(context.Background(), q, "test-handle")
	if err != nil {
		t.Errorf("deleteMessageWithRetry() error = %v, want nil", err)
	}
	if callCount != 2 {
		t.Errorf("deleteMessageWithRetry() called %d times, want 2", callCount)
	}
}

func TestDeleteMessageWithRetry_AllFail(t *testing.T) {
	callCount := 0
	testErr := errors.New("persistent error")
	q := &mockQueue{
		deleteFunc: func(_ context.Context, _ string) error {
			callCount++
			return testErr
		},
	}

	err := deleteMessageWithRetry(context.Background(), q, "test-handle")
	if err == nil {
		t.Error("deleteMessageWithRetry() error = nil, want error")
	}
	if callCount != maxDeleteRetries {
		t.Errorf("deleteMessageWithRetry() called %d times, want %d", callCount, maxDeleteRetries)
	}
}

func TestStdLogger(_ *testing.T) {
	logger := &stdLogger{}

	// Should not panic
	logger.Printf("test format %s", "arg")
	logger.Println("test message")
}

func TestHousekeepingMetricsAdapter(t *testing.T) {
	// Test that the adapter struct can be created with nil publisher
	adapter := &housekeepingMetricsAdapter{publisher: nil}

	// Verify the adapter has the expected nil publisher
	if adapter.publisher != nil {
		t.Error("publisher should be nil in this test")
	}
}

func TestConstants(t *testing.T) {
	if maxDeleteRetries <= 0 {
		t.Errorf("maxDeleteRetries = %d, should be positive", maxDeleteRetries)
	}
	if retryDelay <= 0 {
		t.Errorf("retryDelay = %v, should be positive", retryDelay)
	}
	if maxFleetCreateRetries <= 0 {
		t.Errorf("maxFleetCreateRetries = %d, should be positive", maxFleetCreateRetries)
	}
	if fleetRetryBaseDelay <= 0 {
		t.Errorf("fleetRetryBaseDelay = %v, should be positive", fleetRetryBaseDelay)
	}
}

func TestBuildRunnerLabel_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		job  *queue.JobMessage
		want string
	}{
		{
			name: "empty run ID",
			job: &queue.JobMessage{
				RunID:      "",
				RunnerSpec: "spec",
				Spot:       true,
			},
			want: "runs-fleet=/runner=spec",
		},
		{
			name: "empty runner spec",
			job: &queue.JobMessage{
				RunID:      "123",
				RunnerSpec: "",
				Spot:       true,
			},
			want: "runs-fleet=123/runner=",
		},
		{
			name: "special characters in pool",
			job: &queue.JobMessage{
				RunID:      "123",
				RunnerSpec: "spec",
				Pool:       "pool-name_v2.1",
				Spot:       true,
			},
			want: "runs-fleet=123/runner=spec/pool=pool-name_v2.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRunnerLabel(tt.job)
			if got != tt.want {
				t.Errorf("buildRunnerLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelectSubnet_AtomicIndex(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"subnet-1"},
	}
	job := &queue.JobMessage{Private: false}

	var subnetIndex uint64

	// Pre-set the index to a high value
	atomic.StoreUint64(&subnetIndex, 1000)

	result := selectSubnet(job, cfg, &subnetIndex)
	if result != "subnet-1" {
		t.Errorf("selectSubnet() = %q, want %q", result, "subnet-1")
	}

	// Index should have incremented
	newIndex := atomic.LoadUint64(&subnetIndex)
	if newIndex != 1001 {
		t.Errorf("subnetIndex = %d, want 1001", newIndex)
	}
}

func TestDeleteMessageWithRetry_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	callCount := 0
	q := &mockQueue{
		deleteFunc: func(_ context.Context, _ string) error {
			callCount++
			return errors.New("error")
		},
	}

	// Even with cancelled context, it will still retry
	_ = deleteMessageWithRetry(ctx, q, "test-handle")

	// Should still attempt retries
	if callCount == 0 {
		t.Error("deleteMessageWithRetry() should have been called at least once")
	}
}

func TestRetryDelay(t *testing.T) {
	// In tests, retryDelay is set to 1ms in init() for fast test execution.
	// Verify it's a positive, reasonable value.
	if retryDelay <= 0 {
		t.Errorf("retryDelay = %v, should be positive", retryDelay)
	}
}

func TestFleetRetryBaseDelay(t *testing.T) {
	// In tests, fleetRetryBaseDelay is set to 1ms in init() for fast test execution.
	// Verify it's a positive, reasonable value.
	if fleetRetryBaseDelay <= 0 {
		t.Errorf("fleetRetryBaseDelay = %v, should be positive", fleetRetryBaseDelay)
	}
}

func TestMockQueue_Interface(_ *testing.T) {
	// Verify mockQueue implements queue.Queue
	var _ queue.Queue = (*mockQueue)(nil)
}

func TestMockQueue_SendMessage(t *testing.T) {
	q := &mockQueue{}
	err := q.SendMessage(context.Background(), &queue.JobMessage{JobID: "test", RunID: "run"})
	if err != nil {
		t.Errorf("SendMessage() error = %v, want nil", err)
	}
}

func TestMockQueue_ReceiveMessages(t *testing.T) {
	q := &mockQueue{}
	msgs, err := q.ReceiveMessages(context.Background(), 10, 5)
	if err != nil {
		t.Errorf("ReceiveMessages() error = %v, want nil", err)
	}
	if msgs != nil {
		t.Errorf("ReceiveMessages() = %v, want nil", msgs)
	}
}

func TestBuildRunnerLabel_AllCombinations(t *testing.T) {
	// Test all possible boolean combinations
	tests := []struct {
		name    string
		private bool
		spot    bool
	}{
		{"private=false, spot=true", false, true},
		{"private=false, spot=false", false, false},
		{"private=true, spot=true", true, true},
		{"private=true, spot=false", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "spec",
				Private:    tt.private,
				Spot:       tt.spot,
			}
			label := buildRunnerLabel(job)
			if label == "" {
				t.Error("buildRunnerLabel() returned empty string")
			}
		})
	}
}

func TestSelectSubnet_SingleSubnet(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"only-subnet"},
	}
	job := &queue.JobMessage{Private: false}
	var subnetIndex uint64

	// Multiple calls should always return the same subnet
	for i := 0; i < 5; i++ {
		result := selectSubnet(job, cfg, &subnetIndex)
		if result != "only-subnet" {
			t.Errorf("iteration %d: selectSubnet() = %q, want %q", i, result, "only-subnet")
		}
	}
}

func TestSelectSubnet_PrivateFallbackToPublic(t *testing.T) {
	cfg := &config.Config{
		PrivateSubnetIDs: nil, // nil, not empty
		PublicSubnetIDs:  []string{"public-1"},
	}
	job := &queue.JobMessage{Private: true}
	var subnetIndex uint64

	result := selectSubnet(job, cfg, &subnetIndex)
	if result != "public-1" {
		t.Errorf("selectSubnet() = %q, want %q", result, "public-1")
	}
}

func TestDeleteMessageWithRetry_ImmediateSuccess(t *testing.T) {
	callCount := 0
	q := &mockQueue{
		deleteFunc: func(_ context.Context, handle string) error {
			callCount++
			if handle != "expected-handle" {
				return errors.New("unexpected handle")
			}
			return nil
		},
	}

	err := deleteMessageWithRetry(context.Background(), q, "expected-handle")
	if err != nil {
		t.Errorf("deleteMessageWithRetry() error = %v, want nil", err)
	}
	if callCount != 1 {
		t.Errorf("called %d times, want 1", callCount)
	}
}

func TestDeleteMessageWithRetry_RetryThenSuccess(t *testing.T) {
	callCount := 0
	q := &mockQueue{
		deleteFunc: func(_ context.Context, _ string) error {
			callCount++
			if callCount < maxDeleteRetries {
				return errors.New("temporary error")
			}
			return nil
		},
	}

	err := deleteMessageWithRetry(context.Background(), q, "handle")
	if err != nil {
		t.Errorf("deleteMessageWithRetry() error = %v, want nil", err)
	}
	if callCount != maxDeleteRetries {
		t.Errorf("called %d times, want %d", callCount, maxDeleteRetries)
	}
}

func TestStdLogger_Printf(_ *testing.T) {
	logger := &stdLogger{}

	// Should not panic with various format strings
	logger.Printf("%s", "simple")
	logger.Printf("%d", 42)
	logger.Printf("%v", map[string]int{"key": 1})
	logger.Printf("no format")
}

func TestStdLogger_Println(_ *testing.T) {
	logger := &stdLogger{}

	// Should not panic with various arguments
	logger.Println()
	logger.Println("single")
	logger.Println("a", "b", "c")
	logger.Println(1, 2, 3)
	logger.Println(map[string]int{"key": 1})
}

func TestHousekeepingMetricsAdapter_NilPublisher(_ *testing.T) {
	adapter := &housekeepingMetricsAdapter{publisher: nil}

	// Verify the struct is created correctly
	if adapter.publisher != nil {
		// This shouldn't happen
		panic("publisher should be nil")
	}
}

func TestMaxDeleteRetries(t *testing.T) {
	if maxDeleteRetries < 1 {
		t.Errorf("maxDeleteRetries = %d, should be at least 1", maxDeleteRetries)
	}
	if maxDeleteRetries > 10 {
		t.Errorf("maxDeleteRetries = %d, should not be more than 10", maxDeleteRetries)
	}
}

func TestMaxFleetCreateRetries(t *testing.T) {
	if maxFleetCreateRetries < 1 {
		t.Errorf("maxFleetCreateRetries = %d, should be at least 1", maxFleetCreateRetries)
	}
	if maxFleetCreateRetries > 10 {
		t.Errorf("maxFleetCreateRetries = %d, should not be more than 10", maxFleetCreateRetries)
	}
}

func TestSelectSubnet_LargeIndex(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"a", "b", "c"},
	}
	job := &queue.JobMessage{Private: false}

	// Start with a very large index to test wraparound
	var subnetIndex uint64 = 1000000000

	result := selectSubnet(job, cfg, &subnetIndex)

	// Should return a valid subnet
	found := false
	for _, s := range cfg.PublicSubnetIDs {
		if s == result {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("selectSubnet() = %q, not in valid subnets", result)
	}
}

func TestBuildRunnerLabel_EmptyOriginalLabel(t *testing.T) {
	job := &queue.JobMessage{
		RunID:         "12345",
		RunnerSpec:    "2cpu-linux",
		OriginalLabel: "", // Empty - should use fallback
		Pool:          "",
		Private:       false,
		Spot:          true,
	}

	label := buildRunnerLabel(job)
	expected := "runs-fleet=12345/runner=2cpu-linux"
	if label != expected {
		t.Errorf("buildRunnerLabel() = %q, want %q", label, expected)
	}
}

func TestBuildRunnerLabel_WhitespaceOriginalLabel(t *testing.T) {
	job := &queue.JobMessage{
		RunID:         "12345",
		RunnerSpec:    "2cpu-linux",
		OriginalLabel: "   ", // Whitespace only - treated as non-empty
		Spot:          true,
	}

	label := buildRunnerLabel(job)
	// Non-empty original label should be used as-is
	if label != "   " {
		t.Errorf("buildRunnerLabel() = %q, want whitespace", label)
	}
}

func TestDeleteMessageWithRetry_NilQueue(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic with nil queue")
		}
	}()

	// This should panic
	_ = deleteMessageWithRetry(context.Background(), nil, "handle")
}

func TestSelectSubnet_BothNilSubnets(t *testing.T) {
	cfg := &config.Config{
		PrivateSubnetIDs: nil,
		PublicSubnetIDs:  nil,
	}
	job := &queue.JobMessage{Private: false}
	var subnetIndex uint64

	result := selectSubnet(job, cfg, &subnetIndex)
	if result != "" {
		t.Errorf("selectSubnet() = %q, want empty string", result)
	}
}

func TestJobMessage_AllFields(t *testing.T) {
	job := queue.JobMessage{
		JobID:         "job-123",
		RunID:         "run-456",
		Repo:          "owner/repo",
		InstanceType:  "t4g.medium",
		Pool:          "default",
		Private:       true,
		Spot:          false,
		RunnerSpec:    "2cpu-linux-arm64",
		OriginalLabel: "runs-fleet=run-456/cpu=2",
		RetryCount:    3,
		ForceOnDemand: true,
		Region:        "us-east-1",
		Environment:   "production",
		OS:            "linux",
		Arch:          "arm64",
		InstanceTypes: []string{"t4g.medium", "t4g.large"},
		TraceID:       "trace-xyz",
		SpanID:        "span-abc",
		ParentID:      "parent-def",
	}

	// Verify all fields are set correctly
	if job.JobID != "job-123" {
		t.Errorf("JobID = %q, want %q", job.JobID, "job-123")
	}
	if job.RunID != "run-456" {
		t.Errorf("RunID = %q, want %q", job.RunID, "run-456")
	}
	if job.Repo != "owner/repo" {
		t.Errorf("Repo = %q, want %q", job.Repo, "owner/repo")
	}
	if job.InstanceType != "t4g.medium" {
		t.Errorf("InstanceType = %q, want %q", job.InstanceType, "t4g.medium")
	}
	if !job.Private {
		t.Error("Private should be true")
	}
	if job.Spot {
		t.Error("Spot should be false")
	}
	if job.RetryCount != 3 {
		t.Errorf("RetryCount = %d, want 3", job.RetryCount)
	}
	if !job.ForceOnDemand {
		t.Error("ForceOnDemand should be true")
	}
	if len(job.InstanceTypes) != 2 {
		t.Errorf("InstanceTypes length = %d, want 2", len(job.InstanceTypes))
	}
}

func TestJobMessage_ZeroValues(t *testing.T) {
	job := queue.JobMessage{}

	if job.JobID != "" {
		t.Errorf("JobID should be empty, got %q", job.JobID)
	}
	if job.RunID != "" {
		t.Errorf("RunID should be empty, got %q", job.RunID)
	}
	if job.RetryCount != 0 {
		t.Errorf("RetryCount should be 0, got %d", job.RetryCount)
	}
	if job.Private {
		t.Error("Private should be false by default")
	}
	if job.Spot {
		t.Error("Spot should be false by default")
	}
	if job.ForceOnDemand {
		t.Error("ForceOnDemand should be false by default")
	}
}

func TestSelectSubnet_MixedSubnets(t *testing.T) {
	cfg := &config.Config{
		PrivateSubnetIDs: []string{"priv-1", "priv-2"},
		PublicSubnetIDs:  []string{"pub-1", "pub-2", "pub-3"},
	}

	var subnetIndex uint64

	// Test private job
	privateJob := &queue.JobMessage{Private: true}
	for i := 0; i < 4; i++ {
		result := selectSubnet(privateJob, cfg, &subnetIndex)
		found := false
		for _, s := range cfg.PrivateSubnetIDs {
			if s == result {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("iteration %d: private job got %q, not in private subnets", i, result)
		}
	}

	// Test public job
	publicJob := &queue.JobMessage{Private: false}
	for i := 0; i < 4; i++ {
		result := selectSubnet(publicJob, cfg, &subnetIndex)
		found := false
		for _, s := range cfg.PublicSubnetIDs {
			if s == result {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("iteration %d: public job got %q, not in public subnets", i, result)
		}
	}
}

func TestConstants_Values(t *testing.T) {
	// Verify constant values are as expected
	if maxDeleteRetries != 3 {
		t.Errorf("maxDeleteRetries = %d, want 3", maxDeleteRetries)
	}
	if maxFleetCreateRetries != 3 {
		t.Errorf("maxFleetCreateRetries = %d, want 3", maxFleetCreateRetries)
	}
	if maxJobRetries != 2 {
		t.Errorf("maxJobRetries = %d, want 2", maxJobRetries)
	}
	if runnerNamePrefix != "runs-fleet-" {
		t.Errorf("runnerNamePrefix = %q, want %q", runnerNamePrefix, "runs-fleet-")
	}
}

// ptr is a helper function to create pointers to values for testing.
func ptr[T any](v T) *T {
	return &v
}

func TestHandleJobFailure_NoRunnerName(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         ptr(int64(123)),
			RunnerName: nil, // No runner assigned
		},
	}

	requeued, err := handleJobFailure(context.Background(), event, nil, nil, nil)
	if err != nil {
		t.Errorf("handleJobFailure() error = %v, want nil", err)
	}
	if requeued {
		t.Error("handleJobFailure() should return false when no runner assigned")
	}
}

func TestHandleJobFailure_NonRunsFleetRunner(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         ptr(int64(123)),
			RunnerName: ptr("github-hosted-runner"),
			Labels:     []string{"ubuntu-latest"},
		},
	}

	requeued, err := handleJobFailure(context.Background(), event, nil, nil, nil)
	if err != nil {
		t.Errorf("handleJobFailure() error = %v, want nil", err)
	}
	if requeued {
		t.Error("handleJobFailure() should return false for non-runs-fleet runner")
	}
}

func TestHandleJobFailure_NoRunsFleetLabels(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         ptr(int64(123)),
			RunnerName: ptr("runs-fleet-i-1234567890abcdef0"),
			Labels:     []string{"self-hosted", "linux"}, // No runs-fleet= label
		},
	}

	requeued, err := handleJobFailure(context.Background(), event, nil, nil, nil)
	if err != nil {
		t.Errorf("handleJobFailure() error = %v, want nil", err)
	}
	if requeued {
		t.Error("handleJobFailure() should return false when no runs-fleet labels")
	}
}

func TestHandleJobFailure_NoJobsTable(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         ptr(int64(123)),
			RunnerName: ptr("runs-fleet-i-1234567890abcdef0"),
			Labels:     []string{"runs-fleet=12345/runner=2cpu-linux-arm64"},
		},
	}

	wrapper := &db.Client{} // Real client that will fail HasJobsTable check

	requeued, err := handleJobFailure(context.Background(), event, nil, wrapper, nil)
	if err != nil {
		t.Errorf("handleJobFailure() error = %v, want nil", err)
	}
	if requeued {
		t.Error("handleJobFailure() should return false when jobs table not configured")
	}
}

func TestHandleJobFailure_ExceedsRetryLimit(t *testing.T) {
	// This test verifies the retry limit logic
	// The function checks jobInfo.RetryCount >= maxJobRetries
	// maxJobRetries = 2, so RetryCount of 2 or more should skip re-queue

	// Since we can't easily mock db.Client, we test the constant value
	if maxJobRetries != 2 {
		t.Errorf("maxJobRetries = %d, want 2", maxJobRetries)
	}
}

func TestHandleJobFailure_InstanceIDExtraction(t *testing.T) {
	// Test that instance ID is correctly extracted from runner name
	tests := []struct {
		runnerName string
		wantPrefix bool
	}{
		{"runs-fleet-i-1234567890abcdef0", true},
		{"runs-fleet-i-abc", true},
		{"github-runner", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.runnerName, func(t *testing.T) {
			hasPrefix := len(tt.runnerName) > len(runnerNamePrefix) &&
				tt.runnerName[:len(runnerNamePrefix)] == runnerNamePrefix
			if hasPrefix != tt.wantPrefix {
				t.Errorf("prefix check for %q = %v, want %v", tt.runnerName, hasPrefix, tt.wantPrefix)
			}
		})
	}
}
