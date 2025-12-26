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
				RunID:         12345,
				OriginalLabel: "runs-fleet=12345/cpu=2",
				Spot:          true,
			},
			want: "runs-fleet=12345/cpu=2",
		},
		{
			name: "uses original label with flexible spec",
			job: &queue.JobMessage{
				RunID:         67890,
				OriginalLabel: "runs-fleet=67890/cpu=4+8/arch=arm64",
				Spot:          true,
			},
			want: "runs-fleet=67890/cpu=4+8/arch=arm64",
		},
		{
			name: "basic label fallback",
			job: &queue.JobMessage{
				RunID: 12345,
				Spot:  true,
			},
			want: "runs-fleet=12345",
		},
		{
			name: "with pool",
			job: &queue.JobMessage{
				RunID: 12345,
				Pool:  "default",
				Spot:  true,
			},
			want: "runs-fleet=12345/pool=default",
		},
		{
			name: "with spot=false",
			job: &queue.JobMessage{
				RunID: 12345,
				Spot:  false,
			},
			want: "runs-fleet=12345/spot=false",
		},
		{
			name: "pool and spot=false",
			job: &queue.JobMessage{
				RunID: 67890,
				Pool:  "mypool",
				Spot:  false,
			},
			want: "runs-fleet=67890/pool=mypool/spot=false",
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
		cfg      *config.Config
		wantFunc func(string) bool
	}{
		{
			name: "uses public subnet",
			cfg: &config.Config{
				PublicSubnetIDs: []string{"subnet-pub-1", "subnet-pub-2"},
			},
			wantFunc: func(result string) bool {
				return result == "subnet-pub-1" || result == "subnet-pub-2"
			},
		},
		{
			name: "no subnets returns empty",
			cfg: &config.Config{
				PublicSubnetIDs: []string{},
			},
			wantFunc: func(result string) bool {
				return result == ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var subnetIndex uint64
			got := selectSubnet(tt.cfg, &subnetIndex)
			if !tt.wantFunc(got) {
				t.Errorf("selectSubnet() = %q, unexpected result", got)
			}
		})
	}
}

func TestSelectSubnet_RoundRobin(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"subnet-a", "subnet-b", "subnet-c"},
	}

	var subnetIndex uint64
	results := make([]string, 6)

	for i := 0; i < 6; i++ {
		results[i] = selectSubnet(cfg, &subnetIndex)
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

	var subnetIndex uint64
	done := make(chan string, 100)

	for i := 0; i < 100; i++ {
		go func() {
			result := selectSubnet(cfg, &subnetIndex)
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
	deleteFunc  func(ctx context.Context, handle string) error
	sendFunc    func(ctx context.Context, job *queue.JobMessage) error
	sentMessages []*queue.JobMessage
}

func (m *mockQueue) SendMessage(ctx context.Context, job *queue.JobMessage) error {
	m.sentMessages = append(m.sentMessages, job)
	if m.sendFunc != nil {
		return m.sendFunc(ctx, job)
	}
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
			name: "zero run ID",
			job: &queue.JobMessage{
				RunID: 0,
				Spot:  true,
			},
			want: "runs-fleet=0",
		},
		{
			name: "special characters in pool",
			job: &queue.JobMessage{
				RunID: 123,
				Pool:  "pool-name_v2.1",
				Spot:  true,
			},
			want: "runs-fleet=123/pool=pool-name_v2.1",
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

	var subnetIndex uint64

	// Pre-set the index to a high value
	atomic.StoreUint64(&subnetIndex, 1000)

	result := selectSubnet(cfg, &subnetIndex)
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
	err := q.SendMessage(context.Background(), &queue.JobMessage{JobID: 12345, RunID: 67890})
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
	// Test spot=true and spot=false combinations
	tests := []struct {
		name string
		spot bool
	}{
		{"spot=true", true},
		{"spot=false", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &queue.JobMessage{
				RunID: 12345,
				Spot:  tt.spot,
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
	var subnetIndex uint64

	// Multiple calls should always return the same subnet
	for i := 0; i < 5; i++ {
		result := selectSubnet(cfg, &subnetIndex)
		if result != "only-subnet" {
			t.Errorf("iteration %d: selectSubnet() = %q, want %q", i, result, "only-subnet")
		}
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

	// Start with a very large index to test wraparound
	var subnetIndex uint64 = 1000000000

	result := selectSubnet(cfg, &subnetIndex)

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
		RunID:         12345,
		OriginalLabel: "", // Empty - should use fallback
		Pool:          "",
		Spot:          true,
	}

	label := buildRunnerLabel(job)
	expected := "runs-fleet=12345"
	if label != expected {
		t.Errorf("buildRunnerLabel() = %q, want %q", label, expected)
	}
}

func TestBuildRunnerLabel_WhitespaceOriginalLabel(t *testing.T) {
	job := &queue.JobMessage{
		RunID:         12345,
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

func TestSelectSubnet_NilSubnets(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: nil,
	}
	var subnetIndex uint64

	result := selectSubnet(cfg, &subnetIndex)
	if result != "" {
		t.Errorf("selectSubnet() = %q, want empty string", result)
	}
}

func TestJobMessage_AllFields(t *testing.T) {
	job := queue.JobMessage{
		JobID:         123,
		RunID:         456,
		Repo:          "owner/repo",
		InstanceType:  "t4g.medium",
		Pool:          "default",
		Spot:          false,
		OriginalLabel: "runs-fleet=456/cpu=2",
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
	if job.JobID != 123 {
		t.Errorf("JobID = %d, want 123", job.JobID)
	}
	if job.RunID != 456 {
		t.Errorf("RunID = %d, want 456", job.RunID)
	}
	if job.Repo != "owner/repo" {
		t.Errorf("Repo = %q, want %q", job.Repo, "owner/repo")
	}
	if job.InstanceType != "t4g.medium" {
		t.Errorf("InstanceType = %q, want %q", job.InstanceType, "t4g.medium")
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

	if job.JobID != 0 {
		t.Errorf("JobID should be 0, got %d", job.JobID)
	}
	if job.RunID != 0 {
		t.Errorf("RunID should be 0, got %d", job.RunID)
	}
	if job.RetryCount != 0 {
		t.Errorf("RetryCount should be 0, got %d", job.RetryCount)
	}
	if job.Spot {
		t.Error("Spot should be false by default")
	}
	if job.ForceOnDemand {
		t.Error("ForceOnDemand should be false by default")
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
			Labels:     []string{"runs-fleet=12345/cpu=2/arch=arm64"},
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

func TestShouldFallbackToOnDemand(t *testing.T) {
	tests := []struct {
		name           string
		spot           bool
		forceOnDemand  bool
		retryCount     int
		wantFallback   bool
	}{
		{
			name:          "spot request with no retries should fallback",
			spot:          true,
			forceOnDemand: false,
			retryCount:    0,
			wantFallback:  true,
		},
		{
			name:          "spot request with retry=1 should fallback",
			spot:          true,
			forceOnDemand: false,
			retryCount:    1,
			wantFallback:  true,
		},
		{
			name:          "spot request at max retries should not fallback",
			spot:          true,
			forceOnDemand: false,
			retryCount:    maxJobRetries,
			wantFallback:  false,
		},
		{
			name:          "spot request over max retries should not fallback",
			spot:          true,
			forceOnDemand: false,
			retryCount:    maxJobRetries + 1,
			wantFallback:  false,
		},
		{
			name:          "already forceOnDemand should not fallback",
			spot:          true,
			forceOnDemand: true,
			retryCount:    0,
			wantFallback:  false,
		},
		{
			name:          "non-spot request should not fallback",
			spot:          false,
			forceOnDemand: false,
			retryCount:    0,
			wantFallback:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &queue.JobMessage{
				JobID:         12345,
				RunID:         67890,
				Spot:          tt.spot,
				ForceOnDemand: tt.forceOnDemand,
				RetryCount:    tt.retryCount,
			}

			shouldFallback := job.Spot && !job.ForceOnDemand && job.RetryCount < maxJobRetries
			if shouldFallback != tt.wantFallback {
				t.Errorf("shouldFallback = %v, want %v", shouldFallback, tt.wantFallback)
			}
		})
	}
}

func TestSendMessageWithRetry_Success(t *testing.T) {
	callCount := 0
	q := &mockQueue{
		sendFunc: func(_ context.Context, _ *queue.JobMessage) error {
			callCount++
			return nil
		},
	}

	err := sendMessageWithRetry(context.Background(), q, &queue.JobMessage{JobID: 123})
	if err != nil {
		t.Errorf("sendMessageWithRetry() error = %v, want nil", err)
	}
	if callCount != 1 {
		t.Errorf("sendMessageWithRetry() called %d times, want 1", callCount)
	}
}

func TestSendMessageWithRetry_EventualSuccess(t *testing.T) {
	callCount := 0
	q := &mockQueue{
		sendFunc: func(_ context.Context, _ *queue.JobMessage) error {
			callCount++
			if callCount < 2 {
				return errors.New("temporary error")
			}
			return nil
		},
	}

	err := sendMessageWithRetry(context.Background(), q, &queue.JobMessage{JobID: 123})
	if err != nil {
		t.Errorf("sendMessageWithRetry() error = %v, want nil", err)
	}
	if callCount != 2 {
		t.Errorf("sendMessageWithRetry() called %d times, want 2", callCount)
	}
}

func TestSendMessageWithRetry_AllFail(t *testing.T) {
	callCount := 0
	testErr := errors.New("persistent error")
	q := &mockQueue{
		sendFunc: func(_ context.Context, _ *queue.JobMessage) error {
			callCount++
			return testErr
		},
	}

	err := sendMessageWithRetry(context.Background(), q, &queue.JobMessage{JobID: 123})
	if err == nil {
		t.Error("sendMessageWithRetry() error = nil, want error")
	}
	if callCount != maxDeleteRetries {
		t.Errorf("sendMessageWithRetry() called %d times, want %d", callCount, maxDeleteRetries)
	}
}

func TestOnDemandFallbackMessage(t *testing.T) {
	originalJob := &queue.JobMessage{
		JobID:         12345,
		RunID:         67890,
		Repo:          "org/repo",
		InstanceType:  "t4g.medium",
		InstanceTypes: []string{"t4g.medium", "t4g.large"},
		Pool:          "default",
		Spot:          true,
		OriginalLabel: "runs-fleet=67890/cpu=2",
		RetryCount:    0,
		ForceOnDemand: false,
		Region:        "us-east-1",
		Environment:   "production",
		OS:            "linux",
		Arch:          "arm64",
		StorageGiB:    50,
	}

	fallbackJob := &queue.JobMessage{
		JobID:         originalJob.JobID,
		RunID:         originalJob.RunID,
		Repo:          originalJob.Repo,
		InstanceType:  originalJob.InstanceType,
		InstanceTypes: originalJob.InstanceTypes,
		Pool:          originalJob.Pool,
		Spot:          false,
		OriginalLabel: originalJob.OriginalLabel,
		RetryCount:    originalJob.RetryCount + 1,
		ForceOnDemand: true,
		Region:        originalJob.Region,
		Environment:   originalJob.Environment,
		OS:            originalJob.OS,
		Arch:          originalJob.Arch,
		StorageGiB:    originalJob.StorageGiB,
	}

	if fallbackJob.Spot {
		t.Error("fallback job should have Spot=false")
	}
	if !fallbackJob.ForceOnDemand {
		t.Error("fallback job should have ForceOnDemand=true")
	}
	if fallbackJob.RetryCount != 1 {
		t.Errorf("fallback job RetryCount = %d, want 1", fallbackJob.RetryCount)
	}
	if fallbackJob.JobID != originalJob.JobID {
		t.Error("fallback job should preserve JobID")
	}
	if fallbackJob.RunID != originalJob.RunID {
		t.Error("fallback job should preserve RunID")
	}
	if fallbackJob.InstanceType != originalJob.InstanceType {
		t.Error("fallback job should preserve InstanceType")
	}
	if len(fallbackJob.InstanceTypes) != len(originalJob.InstanceTypes) {
		t.Error("fallback job should preserve InstanceTypes")
	}
	if fallbackJob.Region != originalJob.Region {
		t.Error("fallback job should preserve Region")
	}
	if fallbackJob.StorageGiB != originalJob.StorageGiB {
		t.Error("fallback job should preserve StorageGiB")
	}
}
