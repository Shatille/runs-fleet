package housekeeping

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// mockQueueAPI implements QueueAPI for testing.
type mockQueueAPI struct {
	messages      []types.Message
	receiveErr    error
	deleteErr     error
	receiveCalls  int
	deleteCalls   int
	deleteReceipt string
}

func (m *mockQueueAPI) ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]types.Message, error) {
	m.receiveCalls++
	if m.receiveErr != nil {
		return nil, m.receiveErr
	}
	return m.messages, nil
}

func (m *mockQueueAPI) DeleteMessage(ctx context.Context, receiptHandle string) error {
	m.deleteCalls++
	m.deleteReceipt = receiptHandle
	return m.deleteErr
}

// mockTaskExecutor implements TaskExecutor for testing.
type mockTaskExecutor struct {
	orphanedErr  error
	ssmErr       error
	jobsErr      error
	poolErr      error
	costErr      error
	orphanedCall int
	ssmCall      int
	jobsCall     int
	poolCall     int
	costCall     int
}

func (m *mockTaskExecutor) ExecuteOrphanedInstances(ctx context.Context) error {
	m.orphanedCall++
	return m.orphanedErr
}

func (m *mockTaskExecutor) ExecuteStaleSSM(ctx context.Context) error {
	m.ssmCall++
	return m.ssmErr
}

func (m *mockTaskExecutor) ExecuteOldJobs(ctx context.Context) error {
	m.jobsCall++
	return m.jobsErr
}

func (m *mockTaskExecutor) ExecutePoolAudit(ctx context.Context) error {
	m.poolCall++
	return m.poolErr
}

func (m *mockTaskExecutor) ExecuteCostReport(ctx context.Context) error {
	m.costCall++
	return m.costErr
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
	bodyStr := string(body)
	receipt := "test-receipt"

	sqsMsg := types.Message{
		Body:          &bodyStr,
		ReceiptHandle: &receipt,
	}

	err := handler.processMessage(context.Background(), sqsMsg)
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
		TaskType:  TaskStaleSSM,
		Timestamp: time.Now(),
	}
	body, _ := json.Marshal(msg)
	bodyStr := string(body)
	receipt := "test-receipt"

	sqsMsg := types.Message{
		Body:          &bodyStr,
		ReceiptHandle: &receipt,
	}

	err := handler.processMessage(context.Background(), sqsMsg)
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
	bodyStr := string(body)
	receipt := "test-receipt"

	sqsMsg := types.Message{
		Body:          &bodyStr,
		ReceiptHandle: &receipt,
	}

	err := handler.processMessage(context.Background(), sqsMsg)
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
	bodyStr := string(body)
	receipt := "test-receipt"

	sqsMsg := types.Message{
		Body:          &bodyStr,
		ReceiptHandle: &receipt,
	}

	err := handler.processMessage(context.Background(), sqsMsg)
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
	bodyStr := string(body)
	receipt := "test-receipt"

	sqsMsg := types.Message{
		Body:          &bodyStr,
		ReceiptHandle: &receipt,
	}

	err := handler.processMessage(context.Background(), sqsMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if executor.costCall != 1 {
		t.Errorf("expected 1 cost call, got %d", executor.costCall)
	}
}

func TestHandler_ProcessMessage_NilBody(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	sqsMsg := types.Message{
		Body: nil,
	}

	err := handler.processMessage(context.Background(), sqsMsg)
	if err == nil {
		t.Fatal("expected error for nil body")
	}
}

func TestHandler_ProcessMessage_InvalidJSON(t *testing.T) {
	q := &mockQueueAPI{}
	executor := &mockTaskExecutor{}
	cfg := &config.Config{}
	handler := NewHandler(q, executor, cfg)

	bodyStr := "invalid json"
	sqsMsg := types.Message{
		Body: &bodyStr,
	}

	err := handler.processMessage(context.Background(), sqsMsg)
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
	bodyStr := string(body)
	receipt := "test-receipt"

	sqsMsg := types.Message{
		Body:          &bodyStr,
		ReceiptHandle: &receipt,
	}

	// Should not error, just log unknown task
	err := handler.processMessage(context.Background(), sqsMsg)
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
	bodyStr := string(body)

	sqsMsg := types.Message{
		Body: &bodyStr,
	}

	err := handler.processMessage(context.Background(), sqsMsg)
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
	bodyStr := string(body)

	sqsMsg := types.Message{
		Body: &bodyStr,
	}

	err := handler.processMessage(context.Background(), sqsMsg)
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
	bodyStr := string(body)
	receipt := "test-receipt"

	sqsMsg := types.Message{
		Body:          &bodyStr,
		ReceiptHandle: &receipt,
	}

	err := handler.processMessage(context.Background(), sqsMsg)
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
	bodyStr := string(body)
	receipt := "test-receipt-123"

	sqsMsg := types.Message{
		Body:          &bodyStr,
		ReceiptHandle: &receipt,
	}

	err := handler.processMessage(context.Background(), sqsMsg)
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
		messages: []types.Message{},
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
		{TaskStaleSSM, "stale_ssm"},
		{TaskOldJobs, "old_jobs"},
		{TaskPoolAudit, "pool_audit"},
		{TaskCostReport, "cost_report"},
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
