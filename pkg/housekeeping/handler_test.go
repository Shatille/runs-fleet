package housekeeping

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/queue"
)

// Test constants to satisfy goconst
const testReceipt = "test-receipt"

// mockQueueAPI implements QueueAPI for testing.
type mockQueueAPI struct {
	messages      []queue.Message
	receiveErr    error
	deleteErr     error
	receiveCalls  int
	deleteCalls   int
	deleteReceipt string
}

func (m *mockQueueAPI) ReceiveMessages(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
	m.receiveCalls++
	if m.receiveErr != nil {
		return nil, m.receiveErr
	}
	return m.messages, nil
}

func (m *mockQueueAPI) DeleteMessage(_ context.Context, receiptHandle string) error {
	m.deleteCalls++
	m.deleteReceipt = receiptHandle
	return m.deleteErr
}

// mockTaskExecutor implements TaskExecutor for testing.
type mockTaskExecutor struct {
	orphanedErr      error
	ssmErr           error
	jobsErr          error
	poolErr          error
	costErr          error
	dlqErr           error
	ephemeralPoolErr error
	orphanedCall     int
	ssmCall          int
	jobsCall         int
	poolCall         int
	costCall         int
	dlqCall          int
	ephemeralPoolCall int
}

func (m *mockTaskExecutor) ExecuteOrphanedInstances(_ context.Context) error {
	m.orphanedCall++
	return m.orphanedErr
}

func (m *mockTaskExecutor) ExecuteStaleSecrets(_ context.Context) error {
	m.ssmCall++
	return m.ssmErr
}

func (m *mockTaskExecutor) ExecuteOldJobs(_ context.Context) error {
	m.jobsCall++
	return m.jobsErr
}

func (m *mockTaskExecutor) ExecutePoolAudit(_ context.Context) error {
	m.poolCall++
	return m.poolErr
}

func (m *mockTaskExecutor) ExecuteCostReport(_ context.Context) error {
	m.costCall++
	return m.costErr
}

func (m *mockTaskExecutor) ExecuteDLQRedrive(_ context.Context) error {
	m.dlqCall++
	return m.dlqErr
}

func (m *mockTaskExecutor) ExecuteEphemeralPoolCleanup(_ context.Context) error {
	m.ephemeralPoolCall++
	return m.ephemeralPoolErr
}

func TestNewHandler(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}

	handler := NewHandler(q, executor, cfg)

	if handler.queueClient != q {
		t.Error("expected queueClient to be set")
	}
	if handler.taskExecutor != executor {
		t.Error("expected taskExecutor to be set")
	}
	if handler.config != cfg {
		t.Error("expected config to be set")
	}
}

func TestHandler_ProcessMessage_OrphanedInstances(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  TaskOrphanedInstances,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if executor.orphanedCall != 1 {
		t.Errorf("expected 1 orphaned call, got %d", executor.orphanedCall)
	}
}

func TestHandler_ProcessMessage_StaleSSM(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  TaskStaleSecrets,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if executor.ssmCall != 1 {
		t.Errorf("expected 1 ssm call, got %d", executor.ssmCall)
	}
}

func TestHandler_ProcessMessage_OldJobs(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  TaskOldJobs,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if executor.jobsCall != 1 {
		t.Errorf("expected 1 jobs call, got %d", executor.jobsCall)
	}
}

func TestHandler_ProcessMessage_PoolAudit(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  TaskPoolAudit,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if executor.poolCall != 1 {
		t.Errorf("expected 1 pool call, got %d", executor.poolCall)
	}
}

func TestHandler_ProcessMessage_CostReport(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  TaskCostReport,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if executor.costCall != 1 {
		t.Errorf("expected 1 cost call, got %d", executor.costCall)
	}
}

func TestHandler_ProcessMessage_DLQRedrive(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  TaskDLQRedrive,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if executor.dlqCall != 1 {
		t.Errorf("expected 1 dlq call, got %d", executor.dlqCall)
	}
}

func TestHandler_ProcessMessage_DLQRedriveError(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{
		dlqErr: errors.New("dlq redrive failed"),
	}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  TaskDLQRedrive,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error from DLQ redrive execution")
	}
	if err.Error() != "dlq redrive failed" {
		t.Errorf("expected 'dlq redrive failed', got '%s'", err.Error())
	}
	if executor.dlqCall != 1 {
		t.Errorf("expected 1 dlq call, got %d", executor.dlqCall)
	}
}

func TestHandler_ProcessMessage_EmptyBody(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	queueMsg := queue.Message{
		Body:   "",
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestHandler_ProcessMessage_InvalidJSON(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	queueMsg := queue.Message{
		Body:   "invalid json",
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestHandler_ProcessMessage_UnknownTaskType(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  "unknown_task",
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	// Should not error, just log unknown task
	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandler_ProcessMessage_TaskError(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{
		orphanedErr: errors.New("task error"),
	}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  TaskOrphanedInstances,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error from task execution")
	}
}

func TestHandler_ProcessMessage_CostReportError(t *testing.T) {
	// Explicit test for cost report error handling
	// Verifies that cost report generation errors are properly propagated
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{
		costErr: errors.New("cost report generation failed"),
	}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  TaskCostReport,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error from cost report execution")
	}
	if err.Error() != "cost report generation failed" {
		t.Errorf("expected 'cost report generation failed', got '%s'", err.Error())
	}
	if executor.costCall != 1 {
		t.Errorf("expected 1 cost call, got %d", executor.costCall)
	}
}

func TestHandler_ProcessMessage_DeleteError(t *testing.T) {
	q := &mockQueueAPI{
		deleteErr: errors.New("delete error"),
	}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  TaskOrphanedInstances,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error from message deletion")
	}
}

func TestHandler_ProcessMessage_DeletesOnSuccess(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	msg := Message{
		TaskType:  TaskOrphanedInstances,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)
	receipt := "test-receipt-123"

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: receipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if q.deleteCalls != 1 {
		t.Errorf("expected 1 delete call, got %d", q.deleteCalls)
	}
	if q.deleteReceipt != receipt {
		t.Errorf("expected receipt '%s', got '%s'", receipt, q.deleteReceipt)
	}
}

func TestHandler_Run_Cancellation(t *testing.T) {
	q := &mockQueueAPI{
		messages: []queue.Message{},
	}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		handler.Run(ctx)
		close(done)
	}()

	// Cancel immediately
	cancel()

	// Wait for handler to stop
	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not stop after cancellation")
	}
}

func TestTaskTypeConstants(t *testing.T) {
	tests := []struct {
		taskType TaskType
		expected string
	}{
		{TaskOrphanedInstances, "orphaned_instances"},
		{TaskStaleSecrets, "stale_secrets"},
		{TaskOldJobs, "old_jobs"},
		{TaskPoolAudit, "pool_audit"},
		{TaskCostReport, "cost_report"},
		{TaskDLQRedrive, "dlq_redrive"},
	}

	for _, tt := range tests {
		if string(tt.taskType) != tt.expected {
			t.Errorf("expected TaskType '%s', got '%s'", tt.expected, tt.taskType)
		}
	}
}

func TestMessage_JSONSerialization(t *testing.T) {
	now := time.Now().Truncate(time.Second) // Truncate for comparison
	msg := Message{
		TaskType:  TaskOrphanedInstances,
		Timestamp: now,
		Params:    json.RawMessage(`{"key": "value"}`),
	}

	jsonBytes, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal message: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	if decoded.TaskType != msg.TaskType {
		t.Errorf("expected task_type '%s', got '%s'", msg.TaskType, decoded.TaskType)
	}
}

// mockTaskLocker implements TaskLocker for testing.
type mockTaskLocker struct {
	acquireErr    error
	releaseErr    error
	acquireCalls  int
	releaseCalls  int
	lastTaskType  string
	lastOwner     string
}

func (m *mockTaskLocker) AcquireTaskLock(_ context.Context, taskType, owner string, _ time.Duration) error {
	m.acquireCalls++
	m.lastTaskType = taskType
	m.lastOwner = owner
	return m.acquireErr
}

func (m *mockTaskLocker) ReleaseTaskLock(_ context.Context, taskType, owner string) error {
	m.releaseCalls++
	m.lastTaskType = taskType
	m.lastOwner = owner
	return m.releaseErr
}

func TestHandler_SetTaskLocker(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	locker := &mockTaskLocker{}
	handler.SetTaskLocker(locker, "instance-1")

	if handler.taskLocker != locker {
		t.Error("expected taskLocker to be set")
	}
	if handler.instanceID != "instance-1" {
		t.Error("expected instanceID to be set")
	}
}

func TestHandler_ProcessMessage_WithLocking_Success(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	locker := &mockTaskLocker{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)
	handler.SetTaskLocker(locker, "instance-1")

	msg := Message{
		TaskType:  TaskOrphanedInstances,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if locker.acquireCalls != 1 {
		t.Errorf("expected 1 acquire call, got %d", locker.acquireCalls)
	}
	if locker.releaseCalls != 1 {
		t.Errorf("expected 1 release call, got %d", locker.releaseCalls)
	}
	if executor.orphanedCall != 1 {
		t.Errorf("expected 1 orphaned call, got %d", executor.orphanedCall)
	}
	if locker.lastTaskType != string(TaskOrphanedInstances) {
		t.Errorf("expected task type '%s', got '%s'", TaskOrphanedInstances, locker.lastTaskType)
	}
	if locker.lastOwner != "instance-1" {
		t.Errorf("expected owner 'instance-1', got '%s'", locker.lastOwner)
	}
}

func TestHandler_ProcessMessage_WithLocking_LockHeld(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	locker := &mockTaskLocker{
		acquireErr: db.ErrTaskLockHeld,
	}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)
	handler.SetTaskLocker(locker, "instance-1")

	msg := Message{
		TaskType:  TaskOrphanedInstances,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("expected no error when lock is held, got: %v", err)
	}

	// Task should NOT be executed when lock is held
	if executor.orphanedCall != 0 {
		t.Errorf("expected 0 orphaned calls when lock held, got %d", executor.orphanedCall)
	}
	// Lock should NOT be released (we didn't acquire it)
	if locker.releaseCalls != 0 {
		t.Errorf("expected 0 release calls when lock held, got %d", locker.releaseCalls)
	}
	// Message should NOT be deleted - let lock holder delete it
	// Message will return to queue after visibility timeout
	if q.deleteCalls != 0 {
		t.Errorf("expected 0 delete calls when lock held, got %d", q.deleteCalls)
	}
}

func TestHandler_ProcessMessage_WithLocking_AcquireError(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	locker := &mockTaskLocker{
		acquireErr: errors.New("dynamodb error"),
	}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)
	handler.SetTaskLocker(locker, "instance-1")

	msg := Message{
		TaskType:  TaskOrphanedInstances,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error when lock acquisition fails")
	}

	// Task should NOT be executed on lock error
	if executor.orphanedCall != 0 {
		t.Errorf("expected 0 orphaned calls on lock error, got %d", executor.orphanedCall)
	}
}

func TestHandler_ProcessMessage_WithLocking_TaskFailure(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{
		orphanedErr: errors.New("task failed"),
	}
	locker := &mockTaskLocker{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)
	handler.SetTaskLocker(locker, "instance-1")

	msg := Message{
		TaskType:  TaskOrphanedInstances,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error from task execution")
	}

	// Lock should still be released on task failure
	if locker.releaseCalls != 1 {
		t.Errorf("expected 1 release call on task failure, got %d", locker.releaseCalls)
	}
	// Message should NOT be deleted on failure (allow retry)
	if q.deleteCalls != 0 {
		t.Errorf("expected 0 delete calls on failure, got %d", q.deleteCalls)
	}
}

func TestHandler_ProcessMessage_WithoutLocking(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)
	// Note: NOT setting task locker

	msg := Message{
		TaskType:  TaskOrphanedInstances,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceipt,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Task should execute without locking
	if executor.orphanedCall != 1 {
		t.Errorf("expected 1 orphaned call, got %d", executor.orphanedCall)
	}
}
