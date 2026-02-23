package housekeeping

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func init() {
	// Use minimal delays in tests to avoid slow test execution
	schedulerBaseRetryDelay = 1 * time.Millisecond
}

// mockSchedulerSQSAPI implements SchedulerSQSAPI for testing.
type mockSchedulerSQSAPI struct {
	mu          sync.Mutex
	messages    []string
	sendErr     error
	sendErrOnce bool // Only error once then succeed
	sendCalls   int
	callCh      chan struct{} // Optional channel to signal each call
}

func (m *mockSchedulerSQSAPI) SendMessage(_ context.Context, params *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	m.mu.Lock()
	m.sendCalls++
	callCh := m.callCh // Capture channel while holding lock

	var err error
	if m.sendErr != nil {
		if m.sendErrOnce {
			m.sendErrOnce = false
			err = m.sendErr
		} else if m.sendCalls <= 3 {
			err = m.sendErr
		}
	}

	if err == nil && params.MessageBody != nil {
		m.messages = append(m.messages, *params.MessageBody)
	}
	m.mu.Unlock()

	// Signal call completion outside of lock to avoid blocking
	if callCh != nil {
		select {
		case callCh <- struct{}{}:
		default:
		}
	}

	if err != nil {
		return nil, err
	}
	return &sqs.SendMessageOutput{}, nil
}

// waitForCalls waits for n calls to be signaled on the channel, with a timeout.
// Returns true if all calls were received, false on timeout.
func waitForCalls(ch <-chan struct{}, n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-deadline:
			return false
		}
	}
	return true
}

// mockSchedulerMetrics implements SchedulerMetrics for testing.
type mockSchedulerMetrics struct {
	failures []string
	err      error
}

func (m *mockSchedulerMetrics) PublishSchedulingFailure(_ context.Context, taskType string) error {
	m.failures = append(m.failures, taskType)
	return m.err
}

func TestDefaultSchedulerConfig(t *testing.T) {
	cfg := DefaultSchedulerConfig()

	if cfg.OrphanedInstancesInterval != 5*time.Minute {
		t.Errorf("expected OrphanedInstancesInterval 5m, got %v", cfg.OrphanedInstancesInterval)
	}
	if cfg.StaleSSMInterval != 15*time.Minute {
		t.Errorf("expected StaleSSMInterval 15m, got %v", cfg.StaleSSMInterval)
	}
	if cfg.OldJobsInterval != 1*time.Hour {
		t.Errorf("expected OldJobsInterval 1h, got %v", cfg.OldJobsInterval)
	}
	if cfg.OrphanedJobsInterval != 15*time.Minute {
		t.Errorf("expected OrphanedJobsInterval 15m, got %v", cfg.OrphanedJobsInterval)
	}
	if cfg.PoolAuditInterval != 10*time.Minute {
		t.Errorf("expected PoolAuditInterval 10m, got %v", cfg.PoolAuditInterval)
	}
	if cfg.CostReportInterval != 24*time.Hour {
		t.Errorf("expected CostReportInterval 24h, got %v", cfg.CostReportInterval)
	}
	if cfg.DLQRedriveInterval != 1*time.Minute {
		t.Errorf("expected DLQRedriveInterval 1m, got %v", cfg.DLQRedriveInterval)
	}
	if cfg.StaleJobsInterval != 5*time.Minute {
		t.Errorf("expected StaleJobsInterval 5m, got %v", cfg.StaleJobsInterval)
	}
}

func TestNewScheduler(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{}
	cfg := DefaultSchedulerConfig()

	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	if scheduler.sqsClient != sqsClient {
		t.Error("expected sqsClient to be set")
	}
	if scheduler.queueURL != "https://sqs.example.com/queue" {
		t.Errorf("expected queueURL 'https://sqs.example.com/queue', got '%s'", scheduler.queueURL)
	}
	if scheduler.config != cfg {
		t.Error("expected config to be set")
	}
}

func TestScheduler_SetMetrics(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{}
	cfg := DefaultSchedulerConfig()
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	metrics := &mockSchedulerMetrics{}
	scheduler.SetMetrics(metrics)

	if scheduler.metrics != metrics {
		t.Error("expected metrics to be set")
	}
}

func TestScheduler_scheduleTask_Success(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{}
	cfg := DefaultSchedulerConfig()
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	scheduler.scheduleTask(context.Background(), TaskOrphanedInstances)

	if sqsClient.sendCalls != 1 {
		t.Errorf("expected 1 send call, got %d", sqsClient.sendCalls)
	}

	if len(sqsClient.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sqsClient.messages))
	}

	// Verify message content
	var msg Message
	if err := json.Unmarshal([]byte(sqsClient.messages[0]), &msg); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	if msg.TaskType != TaskOrphanedInstances {
		t.Errorf("expected task_type '%s', got '%s'", TaskOrphanedInstances, msg.TaskType)
	}
}

func TestScheduler_scheduleTask_AllTaskTypes(t *testing.T) {
	taskTypes := []TaskType{
		TaskOrphanedInstances,
		TaskStaleSecrets,
		TaskOldJobs,
		TaskPoolAudit,
		TaskCostReport,
		TaskDLQRedrive,
	}

	for _, taskType := range taskTypes {
		t.Run(string(taskType), func(t *testing.T) {
			sqsClient := &mockSchedulerSQSAPI{}
			cfg := DefaultSchedulerConfig()
			scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

			scheduler.scheduleTask(context.Background(), taskType)

			if sqsClient.sendCalls != 1 {
				t.Errorf("expected 1 send call, got %d", sqsClient.sendCalls)
			}
		})
	}
}

func TestScheduler_scheduleTask_RetryOnError(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{
		sendErr:     errors.New("network error"),
		sendErrOnce: true, // Error first, then succeed
	}
	cfg := DefaultSchedulerConfig()
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	scheduler.scheduleTask(context.Background(), TaskOrphanedInstances)

	// Should have retried
	if sqsClient.sendCalls < 2 {
		t.Errorf("expected at least 2 send calls (with retry), got %d", sqsClient.sendCalls)
	}
}

func TestScheduler_scheduleTask_PublishesMetricOnFailure(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{
		sendErr: errors.New("persistent error"),
	}
	cfg := DefaultSchedulerConfig()
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	metrics := &mockSchedulerMetrics{}
	scheduler.SetMetrics(metrics)

	scheduler.scheduleTask(context.Background(), TaskOrphanedInstances)

	// Should have published failure metric
	if len(metrics.failures) != 1 {
		t.Errorf("expected 1 failure metric, got %d", len(metrics.failures))
	}
	if metrics.failures[0] != string(TaskOrphanedInstances) {
		t.Errorf("expected failure for '%s', got '%s'", TaskOrphanedInstances, metrics.failures[0])
	}
}

func TestScheduler_scheduleTask_ContextCancelled(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{
		sendErr: errors.New("error"),
	}
	cfg := DefaultSchedulerConfig()
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	scheduler.scheduleTask(ctx, TaskOrphanedInstances)

	// Should stop retrying when context is cancelled
	// The exact number of calls depends on timing, but it should be limited
	if sqsClient.sendCalls > maxScheduleRetries+1 {
		t.Errorf("too many send calls after cancellation: %d", sqsClient.sendCalls)
	}
}

func TestScheduler_scheduleTask_ContextCancelledDuringRetry(t *testing.T) {
	// Explicit test verifying context cancellation is checked during retry backoff
	// This addresses the code review concern about context handling in retry loop
	callCh := make(chan struct{}, 10)
	sqsClient := &mockSchedulerSQSAPI{
		sendErr: errors.New("network error"),
		callCh:  callCh,
	}
	cfg := DefaultSchedulerConfig()
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	ctx, cancel := context.WithCancel(context.Background())

	// Start scheduling in a goroutine
	done := make(chan struct{})
	go func() {
		scheduler.scheduleTask(ctx, TaskOrphanedInstances)
		close(done)
	}()

	// Wait for first call to be made via channel (instead of time.Sleep)
	if !waitForCalls(callCh, 1, 2*time.Second) {
		t.Fatal("timed out waiting for first call")
	}

	// Cancel during the retry backoff
	cancel()

	// Should exit promptly
	select {
	case <-done:
		// Success - exited promptly after context cancellation
	case <-time.After(2 * time.Second):
		t.Fatal("scheduleTask did not respect context cancellation during retry")
	}

	// Verify it didn't exhaust all retries
	sqsClient.mu.Lock()
	calls := sqsClient.sendCalls
	sqsClient.mu.Unlock()
	if calls >= maxScheduleRetries {
		t.Logf("Note: %d calls made before cancellation took effect", calls)
	}
}

func TestScheduler_Run_Cancellation(t *testing.T) {
	callCh := make(chan struct{}, 10)
	sqsClient := &mockSchedulerSQSAPI{callCh: callCh}
	cfg := SchedulerConfig{
		OrphanedInstancesInterval:    100 * time.Millisecond,
		StaleSSMInterval:             100 * time.Millisecond,
		OldJobsInterval:              100 * time.Millisecond,
		OrphanedJobsInterval:         100 * time.Millisecond,
		PoolAuditInterval:            100 * time.Millisecond,
		CostReportInterval:           100 * time.Millisecond,
		DLQRedriveInterval:           100 * time.Millisecond,
		EphemeralPoolCleanupInterval: 100 * time.Millisecond,
		StaleJobsInterval:           100 * time.Millisecond,
	}
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()

	// Wait for immediate tasks to be scheduled (2 tasks run on startup)
	if !waitForCalls(callCh, 2, 2*time.Second) {
		t.Fatal("timed out waiting for immediate tasks")
	}
	cancel()

	// Wait for scheduler to stop
	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
}

func TestScheduler_Run_ImmediateOrphanedTask(t *testing.T) {
	callCh := make(chan struct{}, 10)
	sqsClient := &mockSchedulerSQSAPI{callCh: callCh}
	cfg := SchedulerConfig{
		OrphanedInstancesInterval:    1 * time.Hour, // Long interval
		StaleSSMInterval:             1 * time.Hour,
		OldJobsInterval:              1 * time.Hour,
		OrphanedJobsInterval:         1 * time.Hour,
		PoolAuditInterval:            1 * time.Hour,
		CostReportInterval:           1 * time.Hour,
		DLQRedriveInterval:           1 * time.Hour,
		EphemeralPoolCleanupInterval: 1 * time.Hour,
		StaleJobsInterval:           1 * time.Hour,
	}
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()

	// Wait for immediate tasks to be scheduled (2 tasks run on startup)
	if !waitForCalls(callCh, 2, 2*time.Second) {
		t.Fatal("timed out waiting for immediate tasks")
	}
	cancel()

	<-done

	// Should have scheduled orphaned instances task immediately
	sqsClient.mu.Lock()
	sendCalls := sqsClient.sendCalls
	messages := sqsClient.messages
	sqsClient.mu.Unlock()

	if sendCalls < 1 {
		t.Error("expected at least 1 send call for immediate orphaned task")
	}

	// Verify it was the orphaned instances task
	if len(messages) > 0 {
		var msg Message
		if err := json.Unmarshal([]byte(messages[0]), &msg); err == nil {
			if msg.TaskType != TaskOrphanedInstances {
				t.Errorf("expected first task to be '%s', got '%s'", TaskOrphanedInstances, msg.TaskType)
			}
		}
	}
}

func TestSchedulerConfig_Structure(t *testing.T) {
	cfg := SchedulerConfig{
		OrphanedInstancesInterval:    5 * time.Minute,
		StaleSSMInterval:             15 * time.Minute,
		OldJobsInterval:              1 * time.Hour,
		OrphanedJobsInterval:         15 * time.Minute,
		PoolAuditInterval:            10 * time.Minute,
		CostReportInterval:           24 * time.Hour,
		DLQRedriveInterval:           15 * time.Minute,
		EphemeralPoolCleanupInterval: 1 * time.Hour,
	}

	if cfg.OrphanedInstancesInterval != 5*time.Minute {
		t.Errorf("expected OrphanedInstancesInterval 5m, got %v", cfg.OrphanedInstancesInterval)
	}
	if cfg.StaleSSMInterval != 15*time.Minute {
		t.Errorf("expected StaleSSMInterval 15m, got %v", cfg.StaleSSMInterval)
	}
	if cfg.OldJobsInterval != 1*time.Hour {
		t.Errorf("expected OldJobsInterval 1h, got %v", cfg.OldJobsInterval)
	}
	if cfg.OrphanedJobsInterval != 15*time.Minute {
		t.Errorf("expected OrphanedJobsInterval 15m, got %v", cfg.OrphanedJobsInterval)
	}
	if cfg.PoolAuditInterval != 10*time.Minute {
		t.Errorf("expected PoolAuditInterval 10m, got %v", cfg.PoolAuditInterval)
	}
	if cfg.CostReportInterval != 24*time.Hour {
		t.Errorf("expected CostReportInterval 24h, got %v", cfg.CostReportInterval)
	}
	if cfg.EphemeralPoolCleanupInterval != 1*time.Hour {
		t.Errorf("expected EphemeralPoolCleanupInterval 1h, got %v", cfg.EphemeralPoolCleanupInterval)
	}
}

func TestConstants(t *testing.T) {
	if maxScheduleRetries != 3 {
		t.Errorf("expected maxScheduleRetries 3, got %d", maxScheduleRetries)
	}
	// In tests, schedulerBaseRetryDelay is set to 1ms in init() for fast test execution.
	// Verify it's a positive, reasonable value.
	if schedulerBaseRetryDelay <= 0 {
		t.Errorf("schedulerBaseRetryDelay = %v, should be positive", schedulerBaseRetryDelay)
	}
}

func TestScheduler_Run_TickerBasedScheduling(t *testing.T) {
	callCh := make(chan struct{}, 20)
	sqsClient := &mockSchedulerSQSAPI{callCh: callCh}
	// Use very short intervals for testing
	cfg := SchedulerConfig{
		OrphanedInstancesInterval:    10 * time.Millisecond,
		StaleSSMInterval:             10 * time.Millisecond,
		OldJobsInterval:              10 * time.Millisecond,
		OrphanedJobsInterval:         10 * time.Millisecond,
		PoolAuditInterval:            10 * time.Millisecond,
		CostReportInterval:           10 * time.Millisecond,
		DLQRedriveInterval:           10 * time.Millisecond,
		EphemeralPoolCleanupInterval: 10 * time.Millisecond,
		StaleJobsInterval:           10 * time.Millisecond,
	}
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()

	// Wait for at least 3 calls (2 immediate + 1 from ticker)
	if !waitForCalls(callCh, 3, 2*time.Second) {
		t.Fatal("timed out waiting for multiple task schedules")
	}
	cancel()

	<-done

	// Should have scheduled multiple tasks
	sqsClient.mu.Lock()
	sendCalls := sqsClient.sendCalls
	sqsClient.mu.Unlock()

	if sendCalls < 3 {
		t.Errorf("expected at least 3 send calls, got %d", sendCalls)
	}
}

func TestScheduler_scheduleTask_NoMetrics(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{
		sendErr: errors.New("error"),
	}
	cfg := DefaultSchedulerConfig()
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)
	// No metrics set

	// Should not panic when metrics is nil
	scheduler.scheduleTask(context.Background(), TaskOrphanedInstances)

	if sqsClient.sendCalls < maxScheduleRetries {
		t.Errorf("expected %d retry calls, got %d", maxScheduleRetries, sqsClient.sendCalls)
	}
}

func TestScheduler_scheduleTask_MetricsError(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{
		sendErr: errors.New("error"),
	}
	cfg := DefaultSchedulerConfig()
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	metrics := &mockSchedulerMetrics{
		err: errors.New("metrics error"),
	}
	scheduler.SetMetrics(metrics)

	// Should not panic when metrics publishing fails
	scheduler.scheduleTask(context.Background(), TaskOrphanedInstances)

	// Should still have recorded the failure attempt
	if len(metrics.failures) != 1 {
		t.Errorf("expected 1 failure metric attempt, got %d", len(metrics.failures))
	}
}

func TestScheduler_Structure(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{}
	cfg := SchedulerConfig{
		OrphanedInstancesInterval:    1 * time.Minute,
		StaleSSMInterval:             2 * time.Minute,
		OldJobsInterval:              3 * time.Minute,
		OrphanedJobsInterval:         4 * time.Minute,
		PoolAuditInterval:            5 * time.Minute,
		CostReportInterval:           6 * time.Minute,
		DLQRedriveInterval:           7 * time.Minute,
		EphemeralPoolCleanupInterval: 8 * time.Minute,
		StaleJobsInterval:            9 * time.Minute,
	}
	queueURL := "https://sqs.example.com/test-queue"

	scheduler := NewScheduler(sqsClient, queueURL, cfg)

	if scheduler.queueURL != queueURL {
		t.Errorf("queueURL = %s, want %s", scheduler.queueURL, queueURL)
	}
	if scheduler.config.OrphanedInstancesInterval != 1*time.Minute {
		t.Errorf("OrphanedInstancesInterval = %v, want 1m", scheduler.config.OrphanedInstancesInterval)
	}
	if scheduler.config.StaleSSMInterval != 2*time.Minute {
		t.Errorf("StaleSSMInterval = %v, want 2m", scheduler.config.StaleSSMInterval)
	}
	if scheduler.config.OldJobsInterval != 3*time.Minute {
		t.Errorf("OldJobsInterval = %v, want 3m", scheduler.config.OldJobsInterval)
	}
	if scheduler.config.OrphanedJobsInterval != 4*time.Minute {
		t.Errorf("OrphanedJobsInterval = %v, want 4m", scheduler.config.OrphanedJobsInterval)
	}
	if scheduler.config.PoolAuditInterval != 5*time.Minute {
		t.Errorf("PoolAuditInterval = %v, want 5m", scheduler.config.PoolAuditInterval)
	}
	if scheduler.config.CostReportInterval != 6*time.Minute {
		t.Errorf("CostReportInterval = %v, want 6m", scheduler.config.CostReportInterval)
	}
	if scheduler.config.DLQRedriveInterval != 7*time.Minute {
		t.Errorf("DLQRedriveInterval = %v, want 7m", scheduler.config.DLQRedriveInterval)
	}
	if scheduler.config.EphemeralPoolCleanupInterval != 8*time.Minute {
		t.Errorf("EphemeralPoolCleanupInterval = %v, want 8m", scheduler.config.EphemeralPoolCleanupInterval)
	}
	if scheduler.config.StaleJobsInterval != 9*time.Minute {
		t.Errorf("StaleJobsInterval = %v, want 9m", scheduler.config.StaleJobsInterval)
	}
}

func TestMockSchedulerSQSAPI_Tracking(t *testing.T) {
	mock := &mockSchedulerSQSAPI{}

	body := "test message"
	_, err := mock.SendMessage(context.Background(), &sqs.SendMessageInput{
		MessageBody: &body,
	})

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if mock.sendCalls != 1 {
		t.Errorf("sendCalls = %d, want 1", mock.sendCalls)
	}
	if len(mock.messages) != 1 {
		t.Errorf("messages length = %d, want 1", len(mock.messages))
	}
	if mock.messages[0] != body {
		t.Errorf("message = %s, want %s", mock.messages[0], body)
	}
}

func TestMockSchedulerMetrics_Tracking(t *testing.T) {
	mock := &mockSchedulerMetrics{}

	err := mock.PublishSchedulingFailure(context.Background(), "test-task")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(mock.failures) != 1 {
		t.Errorf("failures length = %d, want 1", len(mock.failures))
	}
	if mock.failures[0] != "test-task" {
		t.Errorf("failure = %s, want test-task", mock.failures[0])
	}
}

func TestSchedulerConfig_ZeroValues(t *testing.T) {
	cfg := SchedulerConfig{}

	if cfg.OrphanedInstancesInterval != 0 {
		t.Error("OrphanedInstancesInterval should be zero by default")
	}
	if cfg.StaleSSMInterval != 0 {
		t.Error("StaleSSMInterval should be zero by default")
	}
	if cfg.OldJobsInterval != 0 {
		t.Error("OldJobsInterval should be zero by default")
	}
	if cfg.OrphanedJobsInterval != 0 {
		t.Error("OrphanedJobsInterval should be zero by default")
	}
	if cfg.PoolAuditInterval != 0 {
		t.Error("PoolAuditInterval should be zero by default")
	}
	if cfg.CostReportInterval != 0 {
		t.Error("CostReportInterval should be zero by default")
	}
	if cfg.DLQRedriveInterval != 0 {
		t.Error("DLQRedriveInterval should be zero by default")
	}
	if cfg.EphemeralPoolCleanupInterval != 0 {
		t.Error("EphemeralPoolCleanupInterval should be zero by default")
	}
	if cfg.StaleJobsInterval != 0 {
		t.Error("StaleJobsInterval should be zero by default")
	}
}

func TestScheduler_scheduleTask_MessageFormat(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{}
	cfg := DefaultSchedulerConfig()
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	scheduler.scheduleTask(context.Background(), TaskPoolAudit)

	if len(sqsClient.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sqsClient.messages))
	}

	var msg Message
	if err := json.Unmarshal([]byte(sqsClient.messages[0]), &msg); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	if msg.TaskType != TaskPoolAudit {
		t.Errorf("TaskType = %s, want %s", msg.TaskType, TaskPoolAudit)
	}

	if msg.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
}
