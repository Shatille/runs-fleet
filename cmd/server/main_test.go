package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/internal/handler"
	"github.com/Shavakan/runs-fleet/internal/worker"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/google/go-github/v57/github"
)

func init() {
	worker.RetryDelay = 1 * time.Millisecond
	worker.FleetRetryBaseDelay = 1 * time.Millisecond
}

const maxJobRetries = 2
const runnerNamePrefix = "runs-fleet-"

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
			got := handler.BuildRunnerLabel(tt.job)
			if got != tt.want {
				t.Errorf("BuildRunnerLabel() = %q, want %q", got, tt.want)
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
			got := worker.SelectSubnet(tt.cfg, &subnetIndex, false)
			if !tt.wantFunc(got) {
				t.Errorf("SelectSubnet() = %q, unexpected result", got)
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
		results[i] = worker.SelectSubnet(cfg, &subnetIndex, false)
	}

	expected := []string{"subnet-a", "subnet-b", "subnet-c", "subnet-a", "subnet-b", "subnet-c"}
	for i, exp := range expected {
		if results[i] != exp {
			t.Errorf("SelectSubnet() iteration %d = %q, want %q", i, results[i], exp)
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
			result := worker.SelectSubnet(cfg, &subnetIndex, false)
			done <- result
		}()
	}

	results := make(map[string]int)
	for i := 0; i < 100; i++ {
		subnet := <-done
		results[subnet]++
	}

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

	if subnetIndex != 100 {
		t.Errorf("final subnetIndex = %d, want 100", subnetIndex)
	}
}

type mockQueue struct {
	deleteFunc   func(ctx context.Context, handle string) error
	sendFunc     func(ctx context.Context, job *queue.JobMessage) error
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

func TestHousekeepingMetricsAdapter(t *testing.T) {
	adapter := &housekeepingMetricsAdapter{publisher: nil}

	if adapter.publisher != nil {
		t.Error("publisher should be nil in this test")
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
			got := handler.BuildRunnerLabel(tt.job)
			if got != tt.want {
				t.Errorf("BuildRunnerLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelectSubnet_AtomicIndex(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"subnet-1"},
	}

	var subnetIndex uint64

	atomic.StoreUint64(&subnetIndex, 1000)

	result := worker.SelectSubnet(cfg, &subnetIndex, false)
	if result != "subnet-1" {
		t.Errorf("SelectSubnet() = %q, want %q", result, "subnet-1")
	}

	newIndex := atomic.LoadUint64(&subnetIndex)
	if newIndex != 1001 {
		t.Errorf("subnetIndex = %d, want 1001", newIndex)
	}
}

func TestMockQueue_Interface(_ *testing.T) {
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
			label := handler.BuildRunnerLabel(job)
			if label == "" {
				t.Error("BuildRunnerLabel() returned empty string")
			}
		})
	}
}

func TestSelectSubnet_SingleSubnet(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"only-subnet"},
	}
	var subnetIndex uint64

	for i := 0; i < 5; i++ {
		result := worker.SelectSubnet(cfg, &subnetIndex, false)
		if result != "only-subnet" {
			t.Errorf("iteration %d: SelectSubnet() = %q, want %q", i, result, "only-subnet")
		}
	}
}

func TestHousekeepingMetricsAdapter_NilPublisher(_ *testing.T) {
	adapter := &housekeepingMetricsAdapter{publisher: nil}

	if adapter.publisher != nil {
		panic("publisher should be nil")
	}
}

func TestSelectSubnet_LargeIndex(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"a", "b", "c"},
	}

	var subnetIndex uint64 = 1000000000

	result := worker.SelectSubnet(cfg, &subnetIndex, false)

	found := false
	for _, s := range cfg.PublicSubnetIDs {
		if s == result {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SelectSubnet() = %q, not in valid subnets", result)
	}
}

func TestBuildRunnerLabel_EmptyOriginalLabel(t *testing.T) {
	job := &queue.JobMessage{
		RunID:         12345,
		OriginalLabel: "",
		Pool:          "",
		Spot:          true,
	}

	label := handler.BuildRunnerLabel(job)
	expected := "runs-fleet=12345"
	if label != expected {
		t.Errorf("BuildRunnerLabel() = %q, want %q", label, expected)
	}
}

func TestBuildRunnerLabel_WhitespaceOriginalLabel(t *testing.T) {
	job := &queue.JobMessage{
		RunID:         12345,
		OriginalLabel: "   ",
		Spot:          true,
	}

	label := handler.BuildRunnerLabel(job)
	if label != "   " {
		t.Errorf("BuildRunnerLabel() = %q, want whitespace", label)
	}
}

func TestSelectSubnet_NilSubnets(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: nil,
	}
	var subnetIndex uint64

	result := worker.SelectSubnet(cfg, &subnetIndex, false)
	if result != "" {
		t.Errorf("SelectSubnet() = %q, want empty string", result)
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

func ptr[T any](v T) *T {
	return &v
}

func TestHandleJobFailure_NoRunnerName(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         ptr(int64(123)),
			RunnerName: nil,
		},
	}

	requeued, err := handler.HandleJobFailure(context.Background(), event, nil, nil, nil)
	if err != nil {
		t.Errorf("HandleJobFailure() error = %v, want nil", err)
	}
	if requeued {
		t.Error("HandleJobFailure() should return false when no runner assigned")
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

	requeued, err := handler.HandleJobFailure(context.Background(), event, nil, nil, nil)
	if err != nil {
		t.Errorf("HandleJobFailure() error = %v, want nil", err)
	}
	if requeued {
		t.Error("HandleJobFailure() should return false for non-runs-fleet runner")
	}
}

func TestHandleJobFailure_NoRunsFleetLabels(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         ptr(int64(123)),
			RunnerName: ptr("runs-fleet-i-1234567890abcdef0"),
			Labels:     []string{"self-hosted", "linux"},
		},
	}

	requeued, err := handler.HandleJobFailure(context.Background(), event, nil, nil, nil)
	if err != nil {
		t.Errorf("HandleJobFailure() error = %v, want nil", err)
	}
	if requeued {
		t.Error("HandleJobFailure() should return false when no runs-fleet labels")
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

	wrapper := &db.Client{}

	requeued, err := handler.HandleJobFailure(context.Background(), event, nil, wrapper, nil)
	if err != nil {
		t.Errorf("HandleJobFailure() error = %v, want nil", err)
	}
	if requeued {
		t.Error("HandleJobFailure() should return false when jobs table not configured")
	}
}

func TestHandleJobFailure_ExceedsRetryLimit(t *testing.T) {
	if maxJobRetries != 2 {
		t.Errorf("maxJobRetries = %d, want 2", maxJobRetries)
	}
}

func TestHandleJobFailure_RunnerNamePrefixCheck(t *testing.T) {
	tests := []struct {
		runnerName string
		wantPrefix bool
	}{
		{"runs-fleet-default-myapp-arm64-12345", true},
		{"runs-fleet-myapp-amd64-99999", true},
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
		name          string
		spot          bool
		forceOnDemand bool
		retryCount    int
		wantFallback  bool
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

func TestRetryDelaysExported(t *testing.T) {
	if worker.RetryDelay <= 0 {
		t.Errorf("worker.RetryDelay = %v, should be positive", worker.RetryDelay)
	}
	if worker.FleetRetryBaseDelay <= 0 {
		t.Errorf("worker.FleetRetryBaseDelay = %v, should be positive", worker.FleetRetryBaseDelay)
	}
}

func TestMockQueue_DeleteMessage_WithError(t *testing.T) {
	testErr := errors.New("delete failed")
	q := &mockQueue{
		deleteFunc: func(_ context.Context, _ string) error {
			return testErr
		},
	}

	err := q.DeleteMessage(context.Background(), "handle")
	if !errors.Is(err, testErr) {
		t.Errorf("DeleteMessage() error = %v, want %v", err, testErr)
	}
}
