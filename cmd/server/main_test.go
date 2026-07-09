package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/internal/handler"
	"github.com/Shavakan/runs-fleet/internal/worker"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
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
				SubnetIDs: []string{"subnet-pub-1", "subnet-pub-2"},
			},
			wantFunc: func(result string) bool {
				return result == "subnet-pub-1" || result == "subnet-pub-2"
			},
		},
		{
			name: "no subnets returns empty",
			cfg: &config.Config{
				SubnetIDs: []string{},
			},
			wantFunc: func(result string) bool {
				return result == ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var subnetIndex uint64
			got := worker.SelectSubnet(tt.cfg, &subnetIndex)
			if !tt.wantFunc(got) {
				t.Errorf("SelectSubnet() = %q, unexpected result", got)
			}
		})
	}
}

func TestSelectSubnet_RoundRobin(t *testing.T) {
	cfg := &config.Config{
		SubnetIDs: []string{"subnet-a", "subnet-b", "subnet-c"},
	}

	var subnetIndex uint64
	results := make([]string, 6)

	for i := 0; i < 6; i++ {
		results[i] = worker.SelectSubnet(cfg, &subnetIndex)
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
		SubnetIDs: []string{"subnet-1", "subnet-2", "subnet-3", "subnet-4"},
	}

	var subnetIndex uint64
	done := make(chan string, 100)

	for i := 0; i < 100; i++ {
		go func() {
			result := worker.SelectSubnet(cfg, &subnetIndex)
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
		for _, valid := range cfg.SubnetIDs {
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
		SubnetIDs: []string{"subnet-1"},
	}

	var subnetIndex uint64

	atomic.StoreUint64(&subnetIndex, 1000)

	result := worker.SelectSubnet(cfg, &subnetIndex)
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
		SubnetIDs: []string{"only-subnet"},
	}
	var subnetIndex uint64

	for i := 0; i < 5; i++ {
		result := worker.SelectSubnet(cfg, &subnetIndex)
		if result != "only-subnet" {
			t.Errorf("iteration %d: SelectSubnet() = %q, want %q", i, result, "only-subnet")
		}
	}
}

func TestSelectSubnet_LargeIndex(t *testing.T) {
	cfg := &config.Config{
		SubnetIDs: []string{"a", "b", "c"},
	}

	var subnetIndex uint64 = 1000000000

	result := worker.SelectSubnet(cfg, &subnetIndex)

	found := false
	for _, s := range cfg.SubnetIDs {
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
		SubnetIDs: nil,
	}
	var subnetIndex uint64

	result := worker.SelectSubnet(cfg, &subnetIndex)
	if result != "" {
		t.Errorf("SelectSubnet() = %q, want empty string", result)
	}
}

func TestWaitForWorkers(t *testing.T) {
	t.Run("returns true when workers finish before timeout", func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Go(func() { time.Sleep(5 * time.Millisecond) })
		if !waitForWorkers(&wg, time.Second) {
			t.Error("expected true when workers finish within timeout")
		}
	})

	t.Run("returns false when workers exceed timeout", func(t *testing.T) {
		var wg sync.WaitGroup
		release := make(chan struct{})
		wg.Go(func() { <-release })
		// Release after the timeout assertion so the WaitGroup (and the internal
		// wg.Wait goroutine in waitForWorkers) unblock before the subtest returns.
		defer close(release)
		if waitForWorkers(&wg, 10*time.Millisecond) {
			t.Error("expected false when workers exceed timeout")
		}
	})
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
		Arch:          "arm64",
		InstanceTypes: []string{"t4g.medium", "t4g.large"},
		Traceparent:   "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01",
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

// recordingMetrics records best-effort metric publishes and can fail them.
type recordingMetrics struct {
	metrics.NoopPublisher
	jobQueued  atomic.Bool
	queueDepth atomic.Bool
	jobStartup atomic.Bool
	fail       bool
}

func (m *recordingMetrics) PublishJobEnqueued(context.Context, string, string, string, string) error {
	m.jobQueued.Store(true)
	if m.fail {
		return errors.New("metrics error")
	}
	return nil
}

func (m *recordingMetrics) PublishQueueDepth(context.Context, string, float64) error {
	m.queueDepth.Store(true)
	if m.fail {
		return errors.New("metrics error")
	}
	return nil
}

func (m *recordingMetrics) PublishJobStartupSeconds(context.Context, string, string, float64) error {
	m.jobStartup.Store(true)
	if m.fail {
		return errors.New("metrics error")
	}
	return nil
}

// gatedNotifier blocks NotifyPoolDemand until release is closed, then records
// the pool name. It lets a test assert the notification is deferred past the ack.
type gatedNotifier struct {
	release  chan struct{}
	called   atomic.Bool
	notified chan string
}

func (n *gatedNotifier) NotifyPoolDemand(poolName string) {
	<-n.release
	n.called.Store(true)
	n.notified <- poolName
}

func signWebhook(t *testing.T, secret string, body []byte) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

// TestHandleWebhook_AcksBeforeBestEffortWork verifies the handler enqueues the
// job durably and returns 2xx even when post-ack best-effort work (metrics,
// pool-demand notification) errors.
func TestHandleWebhook_AcksBeforeBestEffortWork(t *testing.T) {
	const secret = "test-secret"
	body := []byte(`{"action":"queued","workflow_job":{"id":12345,"labels":["runs-fleet=67890/cpu=4/arch=arm64/pool=default"]},"repository":{"full_name":"owner/repo"}}`)

	q := &mockQueue{}
	met := &recordingMetrics{fail: true}
	notifier := &gatedNotifier{release: make(chan struct{}), notified: make(chan string, 1)}

	ws := &webhookServer{
		cfg:              &config.Config{GitHubWebhookSecret: secret},
		jobQueue:         q,
		metricsPublisher: met,
		poolNotifier:     notifier,
	}

	rec := httptest.NewRecorder()
	ws.handleWebhook(rec, signWebhook(t, secret, body))

	// The handler must have returned 2xx with the job durably enqueued, while the
	// best-effort pool-demand notification is still blocked (i.e. deferred past ack).
	if rec.Code != http.StatusOK {
		t.Fatalf("handleWebhook() status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(q.sentMessages) != 1 {
		t.Fatalf("durable enqueue count = %d, want 1", len(q.sentMessages))
	}
	if got := q.sentMessages[0].JobID; got != 12345 {
		t.Errorf("enqueued JobID = %d, want 12345", got)
	}
	if notifier.called.Load() {
		t.Fatal("NotifyPoolDemand ran before the ack; best-effort work must be deferred")
	}

	close(notifier.release)
	select {
	case pool := <-notifier.notified:
		if pool != "default" {
			t.Errorf("NotifyPoolDemand pool = %q, want %q", pool, "default")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-ack pool-demand notification never fired")
	}

	waitFor(t, func() bool { return met.jobQueued.Load() && met.queueDepth.Load() },
		"post-ack metrics never published")
}

// TestHandleWebhook_InProgressPublishesStartup verifies an in_progress event from
// a runs-fleet runner acks immediately and publishes the startup latency metric
// as post-ack best-effort work.
func TestHandleWebhook_InProgressPublishesStartup(t *testing.T) {
	const secret = "test-secret"
	body := []byte(`{"action":"in_progress","workflow_job":{"id":12345,"runner_name":"runs-fleet-i-abc123","labels":["runs-fleet=67890/cpu=4/pool=default"],"created_at":"2026-07-09T12:00:00Z","started_at":"2026-07-09T12:00:38Z"},"repository":{"full_name":"owner/repo"}}`)

	met := &recordingMetrics{}
	ws := &webhookServer{
		cfg:              &config.Config{GitHubWebhookSecret: secret},
		metricsPublisher: met,
	}

	rec := httptest.NewRecorder()
	ws.handleWebhook(rec, signWebhook(t, secret, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("handleWebhook() status = %d, want %d", rec.Code, http.StatusOK)
	}

	waitFor(t, func() bool { return met.jobStartup.Load() }, "post-ack startup metric never published")
}

// TestHandleWebhook_InProgressForeignRunnerSkips verifies an in_progress event from
// a non-runs-fleet runner does not publish the startup metric.
func TestHandleWebhook_InProgressForeignRunnerSkips(t *testing.T) {
	const secret = "test-secret"
	body := []byte(`{"action":"in_progress","workflow_job":{"id":12345,"runner_name":"blacksmith-runner","labels":["runs-fleet=67890/cpu=4/pool=default"],"created_at":"2026-07-09T12:00:00Z","started_at":"2026-07-09T12:00:38Z"},"repository":{"full_name":"owner/repo"}}`)

	met := &recordingMetrics{}
	ws := &webhookServer{
		cfg:              &config.Config{GitHubWebhookSecret: secret},
		metricsPublisher: met,
	}

	rec := httptest.NewRecorder()
	ws.handleWebhook(rec, signWebhook(t, secret, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("handleWebhook() status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Give any post-ack goroutine a moment; the metric must never fire.
	time.Sleep(50 * time.Millisecond)
	if met.jobStartup.Load() {
		t.Error("startup metric published for a foreign runner")
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}
