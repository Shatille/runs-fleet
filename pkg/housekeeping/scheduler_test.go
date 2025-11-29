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

// mockSchedulerSQSAPI implements SchedulerSQSAPI for testing.
type mockSchedulerSQSAPI struct {
	mu          sync.Mutex
	messages    []string
	sendErr     error
	sendErrOnce bool // Only error once then succeed
	sendCalls   int
}

func (m *mockSchedulerSQSAPI) SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
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

func (m *mockSchedulerMetrics) PublishSchedulingFailure(ctx context.Context, taskType string) error {
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

func TestScheduler_Run_Cancellation(t *testing.T) {
	sqsClient := &mockSchedulerSQSAPI{}
	cfg := SchedulerConfig{
		OrphanedInstancesInterval: 100 * time.Millisecond,
		StaleSSMInterval:          100 * time.Millisecond,
		OldJobsInterval:           100 * time.Millisecond,
		PoolAuditInterval:         100 * time.Millisecond,
		CostReportInterval:        100 * time.Millisecond,
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
	if baseRetryDelay != 1*time.Second {
		t.Errorf("expected baseRetryDelay 1s, got %v", baseRetryDelay)
	}
}
