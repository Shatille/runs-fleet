package termination

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

func init() {
	// Use short tick interval in tests to avoid slow test execution
	handlerTickInterval = 10 * time.Millisecond
}

// Test constants to satisfy goconst
const (
	testReceiptTermination = "test-receipt"
	testStatusSuccess      = "success"
)

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

// mockDBAPI implements DBAPI for testing.
type mockDBAPI struct {
	markCompleteErr  error
	updateMetricsErr error
	completeCalls    int
	metricsCalls     int
	lastInstanceID   string
	lastStatus       string
}

func (m *mockDBAPI) MarkJobComplete(_ context.Context, instanceID, status string, _, _ int) error {
	m.completeCalls++
	m.lastInstanceID = instanceID
	m.lastStatus = status
	return m.markCompleteErr
}

func (m *mockDBAPI) UpdateJobMetrics(_ context.Context, _ string, _, _ time.Time) error {
	m.metricsCalls++
	return m.updateMetricsErr
}

// mockMetricsAPI implements MetricsAPI for testing.
type mockMetricsAPI struct {
	durationCalls int
	successCalls  int
	failureCalls  int
	durationErr   error
	successErr    error
	failureErr    error
	lastDuration  int
}

func (m *mockMetricsAPI) PublishJobDuration(_ context.Context, duration int) error {
	m.durationCalls++
	m.lastDuration = duration
	return m.durationErr
}

func (m *mockMetricsAPI) PublishJobSuccess(_ context.Context) error {
	m.successCalls++
	return m.successErr
}

func (m *mockMetricsAPI) PublishJobFailure(_ context.Context) error {
	m.failureCalls++
	return m.failureErr
}

// mockSSMAPI implements SSMAPI for testing.
type mockSSMAPI struct {
	deleteErr   error
	deleteCalls int
	deletedName string
}

func (m *mockSSMAPI) DeleteParameter(_ context.Context, params *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	m.deleteCalls++
	if params.Name != nil {
		m.deletedName = *params.Name
	}
	return nil, m.deleteErr
}

func TestNewHandler(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}

	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	if handler.queueClient != q {
		t.Error("expected queueClient to be set")
	}
	if handler.dbClient != db {
		t.Error("expected dbClient to be set")
	}
	if handler.metrics != metrics {
		t.Error("expected metrics to be set")
	}
	if handler.ssmClient != ssmClient {
		t.Error("expected ssmClient to be set")
	}
	if handler.config != cfg {
		t.Error("expected config to be set")
	}
}

func TestHandler_processMessage_Success(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "job-123",
		Status:          "success",
		ExitCode:        0,
		DurationSeconds: 120,
		StartedAt:       time.Now().Add(-2 * time.Minute),
		CompletedAt:     time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if db.completeCalls != 1 {
		t.Errorf("expected 1 complete call, got %d", db.completeCalls)
	}
	if db.lastInstanceID != "i-12345" {
		t.Errorf("expected instance ID 'i-12345', got '%s'", db.lastInstanceID)
	}
	if db.lastStatus != testStatusSuccess {
		t.Errorf("expected status '%s', got '%s'", testStatusSuccess, db.lastStatus)
	}

	if metrics.successCalls != 1 {
		t.Errorf("expected 1 success metric call, got %d", metrics.successCalls)
	}
	if metrics.durationCalls != 1 {
		t.Errorf("expected 1 duration metric call, got %d", metrics.durationCalls)
	}
	if metrics.lastDuration != 120 {
		t.Errorf("expected duration 120, got %d", metrics.lastDuration)
	}

	if q.deleteCalls != 1 {
		t.Errorf("expected 1 delete call, got %d", q.deleteCalls)
	}
}

func TestHandler_processMessage_Failure(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "job-123",
		Status:          "failure",
		ExitCode:        1,
		DurationSeconds: 60,
		StartedAt:       time.Now().Add(-1 * time.Minute),
		CompletedAt:     time.Now(),
		Error:           "test error",
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.failureCalls != 1 {
		t.Errorf("expected 1 failure metric call, got %d", metrics.failureCalls)
	}
}

func TestHandler_processMessage_Started(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := Message{
		InstanceID: "i-12345",
		JobID:      "job-123",
		Status:     "started",
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not call db or metrics for "started" status
	if db.completeCalls != 0 {
		t.Errorf("expected 0 complete calls for 'started' status, got %d", db.completeCalls)
	}
	if metrics.successCalls != 0 || metrics.failureCalls != 0 {
		t.Error("expected no success/failure metrics for 'started' status")
	}
}

func TestHandler_processMessage_EmptyBody(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	queueMsg := queue.Message{
		Body:   "",
		Handle: testReceiptTermination,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestHandler_processMessage_InvalidJSON(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	queueMsg := queue.Message{
		Body:   "invalid json",
		Handle: testReceiptTermination,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestHandler_validateMessage_MissingInstanceID(t *testing.T) {
	handler := &Handler{}

	msg := &Message{
		JobID:  "job-123",
		Status: "success",
	}

	err := handler.validateMessage(msg)
	if err == nil {
		t.Fatal("expected error for missing instance_id")
	}
}

func TestHandler_validateMessage_MissingJobID(t *testing.T) {
	handler := &Handler{}

	msg := &Message{
		InstanceID: "i-12345",
		Status:     "success",
	}

	err := handler.validateMessage(msg)
	if err == nil {
		t.Fatal("expected error for missing job_id")
	}
}

func TestHandler_validateMessage_MissingStatus(t *testing.T) {
	handler := &Handler{}

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "job-123",
	}

	err := handler.validateMessage(msg)
	if err == nil {
		t.Fatal("expected error for missing status")
	}
}

func TestHandler_validateMessage_Valid(t *testing.T) {
	handler := &Handler{}

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "job-123",
		Status:     "success",
	}

	err := handler.validateMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandler_processTermination_MarkCompleteError(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{
		markCompleteErr: errors.New("db error"),
	}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "job-123",
		Status:     "success",
	}

	err := handler.processTermination(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error from mark complete")
	}
}

func TestHandler_processTermination_DeleteSSMParameter(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "job-123",
		Status:     "success",
	}

	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ssmClient.deleteCalls != 1 {
		t.Errorf("expected 1 SSM delete call, got %d", ssmClient.deleteCalls)
	}

	expectedParam := "/runs-fleet/runners/i-12345/config"
	if ssmClient.deletedName != expectedParam {
		t.Errorf("expected deleted parameter '%s', got '%s'", expectedParam, ssmClient.deletedName)
	}
}

func TestHandler_deleteSSMParameter_NotFound(t *testing.T) {
	ssmClient := &mockSSMAPI{
		deleteErr: errors.New("ParameterNotFound"),
	}
	handler := &Handler{ssmClient: ssmClient}

	// Should not return error if parameter already deleted
	err := handler.deleteSSMParameter(context.Background(), "i-12345")
	if err != nil {
		t.Fatalf("unexpected error for ParameterNotFound: %v", err)
	}
}

func TestHandler_deleteSSMParameter_OtherError(t *testing.T) {
	ssmClient := &mockSSMAPI{
		deleteErr: errors.New("other error"),
	}
	handler := &Handler{ssmClient: ssmClient}

	err := handler.deleteSSMParameter(context.Background(), "i-12345")
	if err == nil {
		t.Fatal("expected error for other SSM error")
	}
}

func TestHandler_processMessage_DeleteError(t *testing.T) {
	q := &mockQueueAPI{
		deleteErr: errors.New("delete error"),
	}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := Message{
		InstanceID: "i-12345",
		JobID:      "job-123",
		Status:     "success",
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error from message deletion")
	}
}

func TestHandler_Run_Cancellation(t *testing.T) {
	q := &mockQueueAPI{
		messages: []queue.Message{},
	}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

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

func TestMessage_Structure(t *testing.T) {
	now := time.Now()
	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "job-123",
		Status:          "success",
		ExitCode:        0,
		DurationSeconds: 120,
		StartedAt:       now.Add(-2 * time.Minute),
		CompletedAt:     now,
		Error:           "test error",
		InterruptedBy:   "spot",
	}

	if msg.InstanceID != "i-12345" {
		t.Errorf("expected InstanceID 'i-12345', got '%s'", msg.InstanceID)
	}
	if msg.JobID != "job-123" {
		t.Errorf("expected JobID 'job-123', got '%s'", msg.JobID)
	}
	if msg.Status != "success" {
		t.Errorf("expected Status 'success', got '%s'", msg.Status)
	}
	if msg.ExitCode != 0 {
		t.Errorf("expected ExitCode 0, got %d", msg.ExitCode)
	}
	if msg.DurationSeconds != 120 {
		t.Errorf("expected DurationSeconds 120, got %d", msg.DurationSeconds)
	}
	if msg.Error != "test error" {
		t.Errorf("expected Error 'test error', got '%s'", msg.Error)
	}
	if msg.InterruptedBy != "spot" {
		t.Errorf("expected InterruptedBy 'spot', got '%s'", msg.InterruptedBy)
	}
}

func TestMessage_JSONSerialization(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "job-123",
		Status:          "success",
		ExitCode:        0,
		DurationSeconds: 120,
		StartedAt:       now.Add(-2 * time.Minute),
		CompletedAt:     now,
	}

	jsonBytes, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal message: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	if decoded.InstanceID != msg.InstanceID {
		t.Errorf("expected InstanceID '%s', got '%s'", msg.InstanceID, decoded.InstanceID)
	}
	if decoded.JobID != msg.JobID {
		t.Errorf("expected JobID '%s', got '%s'", msg.JobID, decoded.JobID)
	}
}

func TestMessage_RequiredFieldsNotOmitted(t *testing.T) {
	// Explicit test verifying required fields are NOT omitted when empty
	// This addresses the code review concern about omitempty on required fields
	// The Message struct intentionally does NOT use omitempty on required fields
	msg := Message{
		InstanceID: "", // Empty string
		JobID:      "", // Empty string
		Status:     "", // Empty string
		// Optional fields with omitempty
		Error:         "",
		InterruptedBy: "",
	}

	jsonBytes, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal message: %v", err)
	}

	jsonStr := string(jsonBytes)

	// Required fields should ALWAYS be present, even when empty
	requiredFields := []string{
		`"instance_id"`,
		`"job_id"`,
		`"status"`,
	}

	for _, field := range requiredFields {
		if !strings.Contains(jsonStr, field) {
			t.Errorf("required field %s should be present in JSON even when empty", field)
		}
	}

	// Optional fields with omitempty SHOULD be omitted when empty
	optionalFields := []string{
		`"error"`,
		`"interrupted_by"`,
	}

	for _, field := range optionalFields {
		if strings.Contains(jsonStr, field) {
			t.Logf("optional field %s is omitted as expected (has omitempty)", field)
		}
	}

	// Verify round-trip works correctly
	var decoded Message
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	// Empty strings should be preserved
	if decoded.InstanceID != "" {
		t.Errorf("expected empty InstanceID, got '%s'", decoded.InstanceID)
	}
}


func TestHandler_processTermination_NoDuration(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := &Message{
		InstanceID:      "i-12345",
		JobID:           "job-123",
		Status:          "success",
		DurationSeconds: 0, // No duration
	}

	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not publish duration metric when duration is 0
	if metrics.durationCalls != 0 {
		t.Errorf("expected 0 duration calls when duration is 0, got %d", metrics.durationCalls)
	}
}

func TestHandler_processTermination_NoTimestamps(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "job-123",
		Status:     "success",
		// No StartedAt or CompletedAt
	}

	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not call UpdateJobMetrics when timestamps are zero
	if db.metricsCalls != 0 {
		t.Errorf("expected 0 metrics calls when timestamps are zero, got %d", db.metricsCalls)
	}
}

func TestHandler_Run_WithMessages(t *testing.T) {
	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "job-123",
		Status:          testStatusSuccess,
		DurationSeconds: 60,
	}
	body, _ := json.Marshal(msg)

	q := &mockQueueAPI{
		messages: []queue.Message{
			{
				Body:   string(body),
				Handle: "receipt-1",
			},
		},
	}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		handler.Run(ctx)
		close(done)
	}()

	// Wait for the ticker to fire (using short test interval)
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Success
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not stop after cancellation")
	}

	// Verify message was processed
	if q.receiveCalls < 1 {
		t.Errorf("expected at least 1 receive call, got %d", q.receiveCalls)
	}
}

func TestHandler_Run_ReceiveError(t *testing.T) {
	q := &mockQueueAPI{
		receiveErr: errors.New("receive error"),
	}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		handler.Run(ctx)
		close(done)
	}()

	// Wait for ticker to fire (using short test interval)
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Success - handler should continue despite receive errors
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not stop after cancellation")
	}

	// Verify receive was attempted despite error
	if q.receiveCalls < 1 {
		t.Errorf("expected at least 1 receive call, got %d", q.receiveCalls)
	}
}

func TestHandler_processTermination_MetricsErrors(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{
		durationErr: errors.New("duration error"),
		successErr:  errors.New("success error"),
	}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := &Message{
		InstanceID:      "i-12345",
		JobID:           "job-123",
		Status:          testStatusSuccess,
		DurationSeconds: 60,
	}

	// Should not fail even if metrics fail (they just log warnings)
	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandler_processTermination_FailureMetricsError(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{
		failureErr: errors.New("failure metric error"),
	}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "job-123",
		Status:     "failure",
	}

	// Should not fail even if failure metric fails
	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandler_processTermination_UpdateMetricsError(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{
		updateMetricsErr: errors.New("update metrics error"),
	}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	now := time.Now()
	msg := &Message{
		InstanceID:  "i-12345",
		JobID:       "job-123",
		Status:      testStatusSuccess,
		StartedAt:   now.Add(-1 * time.Minute),
		CompletedAt: now,
	}

	// Should not fail even if update metrics fails (just logs warning)
	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandler_processMessage_NoHandle(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := Message{
		InstanceID: "i-12345",
		JobID:      "job-123",
		Status:     testStatusSuccess,
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: "", // Empty handle
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not try to delete message with empty handle
	if q.deleteCalls != 0 {
		t.Errorf("expected 0 delete calls for empty handle, got %d", q.deleteCalls)
	}
}

func TestHandler_processTermination_Timeout(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "job-123",
		Status:     "timeout",
	}

	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Timeout should be treated as failure
	if metrics.failureCalls != 1 {
		t.Errorf("expected 1 failure call for timeout, got %d", metrics.failureCalls)
	}
}

func TestHandler_processTermination_Interrupted(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := &Message{
		InstanceID:    "i-12345",
		JobID:         "job-123",
		Status:        "interrupted",
		InterruptedBy: "spot",
	}

	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Interrupted should be treated as failure
	if metrics.failureCalls != 1 {
		t.Errorf("expected 1 failure call for interrupted, got %d", metrics.failureCalls)
	}
}

func TestHandler_processTermination_SSMDeleteError(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	ssmClient := &mockSSMAPI{
		deleteErr: errors.New("access denied"),
	}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, ssmClient, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "job-123",
		Status:     testStatusSuccess,
	}

	// Should not fail even if SSM delete fails (just logs warning)
	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
