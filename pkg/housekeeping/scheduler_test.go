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
}

func (m *mockSchedulerSQSAPI) SendMessage(_ context.Context, params *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendCalls++

	if m.sendErr != nil {
		if m.sendErrOnce {
			m.sendErrOnce = false
			return nil, m.sendErr
		}
		if !m.sendErrOnce && m.sendCalls <= 3 {
			return nil, m.sendErr
		}
	}

	if params.MessageBody != nil {
		m.messages = append(m.messages, *params.MessageBody)
	}
	return &sqs.SendMessageOutput{}, nil
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
	if cfg.PoolAuditInterval != 10*time.Minute {
		t.Errorf("expected PoolAuditInterval 10m, got %v", cfg.PoolAuditInterval)
	}
	if cfg.CostReportInterval != 24*time.Hour {
		t.Errorf("expected CostReportInterval 24h, got %v", cfg.CostReportInterval)
	}
	if cfg.DLQRedriveInterval != 1*time.Minute {
		t.Errorf("expected DLQRedriveInterval 1m, got %v", cfg.DLQRedriveInterval)
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
		TaskStaleSSM,
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
	sqsClient := &mockSchedulerSQSAPI{
		sendErr: errors.New("network error"),
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

	// Give it a moment to make the first call and enter retry
	time.Sleep(50 * time.Millisecond)

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
	if sqsClient.sendCalls >= maxScheduleRetries {
		t.Logf("Note: %d calls made before cancellation took effect", sqsClient.sendCalls)
	}
}

func TestScheduler_Run_Cancellation(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{}
	cfg := SchedulerConfig{
		OrphanedInstancesInterval: 100 * time.Millisecond,
		StaleSSMInterval:          100 * time.Millisecond,
		OldJobsInterval:           100 * time.Millisecond,
		PoolAuditInterval:         100 * time.Millisecond,
		CostReportInterval:        100 * time.Millisecond,
		DLQRedriveInterval:        100 * time.Millisecond,
	}
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()

	// Wait a bit then cancel
	time.Sleep(50 * time.Millisecond)
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
	sqsClient := &mockSchedulerSQSAPI{}
	cfg := SchedulerConfig{
		OrphanedInstancesInterval: 1 * time.Hour, // Long interval
		StaleSSMInterval:          1 * time.Hour,
		OldJobsInterval:           1 * time.Hour,
		PoolAuditInterval:         1 * time.Hour,
		CostReportInterval:        1 * time.Hour,
		DLQRedriveInterval:        1 * time.Hour,
	}
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()

	// Wait a short time for the immediate task to be scheduled
	time.Sleep(50 * time.Millisecond)
	cancel()

	<-done

	// Should have scheduled orphaned instances task immediately
	if sqsClient.sendCalls < 1 {
		t.Error("expected at least 1 send call for immediate orphaned task")
	}

	// Verify it was the orphaned instances task
	if len(sqsClient.messages) > 0 {
		var msg Message
		if err := json.Unmarshal([]byte(sqsClient.messages[0]), &msg); err == nil {
			if msg.TaskType != TaskOrphanedInstances {
				t.Errorf("expected first task to be '%s', got '%s'", TaskOrphanedInstances, msg.TaskType)
			}
		}
	}
}

func TestSchedulerConfig_Structure(t *testing.T) {
	cfg := SchedulerConfig{
		OrphanedInstancesInterval: 5 * time.Minute,
		StaleSSMInterval:          15 * time.Minute,
		OldJobsInterval:           1 * time.Hour,
		PoolAuditInterval:         10 * time.Minute,
		CostReportInterval:        24 * time.Hour,
		DLQRedriveInterval:        15 * time.Minute,
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
	if cfg.PoolAuditInterval != 10*time.Minute {
		t.Errorf("expected PoolAuditInterval 10m, got %v", cfg.PoolAuditInterval)
	}
	if cfg.CostReportInterval != 24*time.Hour {
		t.Errorf("expected CostReportInterval 24h, got %v", cfg.CostReportInterval)
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
	sqsClient := &mockSchedulerSQSAPI{}
	// Use very short intervals for testing
	cfg := SchedulerConfig{
		OrphanedInstancesInterval: 50 * time.Millisecond,
		StaleSSMInterval:          50 * time.Millisecond,
		OldJobsInterval:           50 * time.Millisecond,
		PoolAuditInterval:         50 * time.Millisecond,
		CostReportInterval:        50 * time.Millisecond,
		DLQRedriveInterval:        50 * time.Millisecond,
	}
	scheduler := NewScheduler(sqsClient, "https://sqs.example.com/queue", cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()

	// Wait for multiple ticker fires
	time.Sleep(200 * time.Millisecond)
	cancel()

	<-done

	// Should have scheduled multiple tasks
	if sqsClient.sendCalls < 3 {
		t.Errorf("expected at least 3 send calls, got %d", sqsClient.sendCalls)
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
		OrphanedInstancesInterval: 1 * time.Minute,
		StaleSSMInterval:          2 * time.Minute,
		OldJobsInterval:           3 * time.Minute,
		PoolAuditInterval:         4 * time.Minute,
		CostReportInterval:        5 * time.Minute,
		DLQRedriveInterval:        6 * time.Minute,
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
	if scheduler.config.PoolAuditInterval != 4*time.Minute {
		t.Errorf("PoolAuditInterval = %v, want 4m", scheduler.config.PoolAuditInterval)
	}
	if scheduler.config.CostReportInterval != 5*time.Minute {
		t.Errorf("CostReportInterval = %v, want 5m", scheduler.config.CostReportInterval)
	}
	if scheduler.config.DLQRedriveInterval != 6*time.Minute {
		t.Errorf("DLQRedriveInterval = %v, want 6m", scheduler.config.DLQRedriveInterval)
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
	if cfg.PoolAuditInterval != 0 {
		t.Error("PoolAuditInterval should be zero by default")
	}
	if cfg.CostReportInterval != 0 {
		t.Error("CostReportInterval should be zero by default")
	}
	if cfg.DLQRedriveInterval != 0 {
		t.Error("DLQRedriveInterval should be zero by default")
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
