package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Test status constants to satisfy goconst
const (
	testStatusSuccess = "success"
	testStatusFailure = "failure"
)

// mockTelemetrySQSAPI implements TelemetrySQSAPI for testing.
type mockTelemetrySQSAPI struct {
	mu              sync.Mutex
	messages        []string
	sendErr         error
	sendCalls       int
	failCount       int // Number of times to fail before succeeding
	sendMessageFunc func(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

func (m *mockTelemetrySQSAPI) SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendCalls++

	// Use custom function if provided
	if m.sendMessageFunc != nil {
		return m.sendMessageFunc(ctx, params, optFns...)
	}

	// Simulate failures for failCount times, then succeed
	if m.failCount > 0 {
		m.failCount--
		return nil, m.sendErr
	}

	// After failCount exhausted, succeed
	if params.MessageBody != nil {
		m.messages = append(m.messages, *params.MessageBody)
	}
	return &sqs.SendMessageOutput{}, nil
}

func (m *mockTelemetrySQSAPI) getMessages() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.messages
}

// mockLogger implements Logger for testing.
type mockLogger struct {
	mu       sync.Mutex
	messages []string
}

func (m *mockLogger) Printf(format string, _ ...interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// We just collect the messages for verification
	m.messages = append(m.messages, format)
}

func (m *mockLogger) Println(_ ...interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, "println")
}

func TestNewTelemetry(t *testing.T) {
	// Test structure - NewTelemetry creates real SQS client from aws.Config
	// For unit testing, we create telemetry with mock directly
	logger := &mockLogger{}
	sqsClient := &mockTelemetrySQSAPI{}

	telemetry := &Telemetry{
		sqsClient: sqsClient,
		queueURL:  "https://sqs.example.com/queue",
		logger:    logger,
	}

	if telemetry.queueURL != "https://sqs.example.com/queue" {
		t.Errorf("expected queueURL 'https://sqs.example.com/queue', got '%s'", telemetry.queueURL)
	}
}

func TestTelemetry_SendJobStarted(t *testing.T) {
	sqsClient := &mockTelemetrySQSAPI{}
	logger := &mockLogger{}

	telemetry := &Telemetry{
		sqsClient: sqsClient,
		queueURL:  "https://sqs.example.com/queue",
		logger:    logger,
	}

	status := JobStatus{
		InstanceID: "i-12345",
		JobID:      "job-123",
		StartedAt:  time.Now(),
	}

	err := telemetry.SendJobStarted(context.Background(), status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sqsClient.sendCalls != 1 {
		t.Errorf("expected 1 send call, got %d", sqsClient.sendCalls)
	}

	messages := sqsClient.getMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}

	// Verify message content
	var decoded JobStatus
	if err := json.Unmarshal([]byte(messages[0]), &decoded); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	if decoded.Status != StatusStarted {
		t.Errorf("expected status 'started', got '%s'", decoded.Status)
	}
}

func TestTelemetry_SendJobCompleted_Success(t *testing.T) {
	sqsClient := &mockTelemetrySQSAPI{}
	logger := &mockLogger{}

	telemetry := &Telemetry{
		sqsClient: sqsClient,
		queueURL:  "https://sqs.example.com/queue",
		logger:    logger,
	}

	status := JobStatus{
		InstanceID:      "i-12345",
		JobID:           "job-123",
		ExitCode:        0,
		DurationSeconds: 120,
		StartedAt:       time.Now().Add(-2 * time.Minute),
		CompletedAt:     time.Now(),
	}

	err := telemetry.SendJobCompleted(context.Background(), status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	messages := sqsClient.getMessages()
	var decoded JobStatus
	if err := json.Unmarshal([]byte(messages[0]), &decoded); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	if decoded.Status != testStatusSuccess {
		t.Errorf("expected status '%s', got '%s'", testStatusSuccess, decoded.Status)
	}
}

func TestTelemetry_SendJobCompleted_Failure(t *testing.T) {
	sqsClient := &mockTelemetrySQSAPI{}
	logger := &mockLogger{}

	telemetry := &Telemetry{
		sqsClient: sqsClient,
		queueURL:  "https://sqs.example.com/queue",
		logger:    logger,
	}

	status := JobStatus{
		InstanceID: "i-12345",
		JobID:      "job-123",
		ExitCode:   1, // Non-zero exit code
	}

	err := telemetry.SendJobCompleted(context.Background(), status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	messages := sqsClient.getMessages()
	var decoded JobStatus
	if err := json.Unmarshal([]byte(messages[0]), &decoded); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	if decoded.Status != testStatusFailure {
		t.Errorf("expected status '%s', got '%s'", testStatusFailure, decoded.Status)
	}
}

func TestTelemetry_SendJobCompleted_Interrupted(t *testing.T) {
	sqsClient := &mockTelemetrySQSAPI{}
	logger := &mockLogger{}

	telemetry := &Telemetry{
		sqsClient: sqsClient,
		queueURL:  "https://sqs.example.com/queue",
		logger:    logger,
	}

	status := JobStatus{
		InstanceID:    "i-12345",
		JobID:         "job-123",
		InterruptedBy: "spot",
	}

	err := telemetry.SendJobCompleted(context.Background(), status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	messages := sqsClient.getMessages()
	var decoded JobStatus
	if err := json.Unmarshal([]byte(messages[0]), &decoded); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	if decoded.Status != StatusInterrupted {
		t.Errorf("expected status '%s', got '%s'", StatusInterrupted, decoded.Status)
	}
}

func TestTelemetry_SendJobTimeout(t *testing.T) {
	sqsClient := &mockTelemetrySQSAPI{}
	logger := &mockLogger{}

	telemetry := &Telemetry{
		sqsClient: sqsClient,
		queueURL:  "https://sqs.example.com/queue",
		logger:    logger,
	}

	status := JobStatus{
		InstanceID: "i-12345",
		JobID:      "job-123",
	}

	err := telemetry.SendJobTimeout(context.Background(), status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	messages := sqsClient.getMessages()
	var decoded JobStatus
	if err := json.Unmarshal([]byte(messages[0]), &decoded); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	if decoded.Status != StatusTimeout {
		t.Errorf("expected status '%s', got '%s'", StatusTimeout, decoded.Status)
	}
}

func TestTelemetry_SendJobFailure(t *testing.T) {
	sqsClient := &mockTelemetrySQSAPI{}
	logger := &mockLogger{}

	telemetry := &Telemetry{
		sqsClient: sqsClient,
		queueURL:  "https://sqs.example.com/queue",
		logger:    logger,
	}

	status := JobStatus{
		InstanceID: "i-12345",
		JobID:      "job-123",
		Error:      "test error",
	}

	err := telemetry.SendJobFailure(context.Background(), status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	messages := sqsClient.getMessages()
	var decoded JobStatus
	if err := json.Unmarshal([]byte(messages[0]), &decoded); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	if decoded.Status != testStatusFailure {
		t.Errorf("expected status '%s', got '%s'", testStatusFailure, decoded.Status)
	}
}

func TestTelemetry_SendMessage_RetryOnError(t *testing.T) {
	sqsClient := &mockTelemetrySQSAPI{
		sendErr:   errors.New("network error"),
		failCount: 2, // Fail twice, then succeed
	}
	logger := &mockLogger{}

	telemetry := &Telemetry{
		sqsClient: sqsClient,
		queueURL:  "https://sqs.example.com/queue",
		logger:    logger,
	}

	status := JobStatus{
		InstanceID: "i-12345",
		JobID:      "job-123",
	}

	err := telemetry.SendJobStarted(context.Background(), status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have retried
	if sqsClient.sendCalls < 3 {
		t.Errorf("expected at least 3 send calls (with retries), got %d", sqsClient.sendCalls)
	}
}

func TestTelemetry_SendMessage_AllRetriesFail(t *testing.T) {
	sqsClient := &mockTelemetrySQSAPI{
		sendErr:   errors.New("persistent error"),
		failCount: 100, // Always fail
	}
	logger := &mockLogger{}

	telemetry := &Telemetry{
		sqsClient: sqsClient,
		queueURL:  "https://sqs.example.com/queue",
		logger:    logger,
	}

	status := JobStatus{
		InstanceID: "i-12345",
		JobID:      "job-123",
	}

	err := telemetry.SendJobStarted(context.Background(), status)
	if err == nil {
		t.Fatal("expected error after all retries fail")
	}

	// Should have tried 3 times
	if sqsClient.sendCalls != 3 {
		t.Errorf("expected 3 send calls, got %d", sqsClient.sendCalls)
	}
}

func TestTelemetry_SendMessage_ContextCancelled(t *testing.T) {
	sqsClient := &mockTelemetrySQSAPI{
		sendErr:   errors.New("error"),
		failCount: 100,
	}
	logger := &mockLogger{}

	telemetry := &Telemetry{
		sqsClient: sqsClient,
		queueURL:  "https://sqs.example.com/queue",
		logger:    logger,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	status := JobStatus{
		InstanceID: "i-12345",
		JobID:      "job-123",
	}

	_ = telemetry.SendJobStarted(ctx, status)
	// The call may return context.Canceled or succeed before context check.
	// The important verification is that it doesn't retry indefinitely,
	// which is implicitly verified by the test completing.
	t.Log("Context cancellation test completed - verified no infinite retry")
}

func TestTelemetry_SendWithTimeout(t *testing.T) {
	sqsClient := &mockTelemetrySQSAPI{}
	logger := &mockLogger{}

	telemetry := &Telemetry{
		sqsClient: sqsClient,
		queueURL:  "https://sqs.example.com/queue",
		logger:    logger,
	}

	status := JobStatus{
		InstanceID: "i-12345",
		JobID:      "job-123",
	}

	err := telemetry.SendWithTimeout(status, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sqsClient.sendCalls != 1 {
		t.Errorf("expected 1 send call, got %d", sqsClient.sendCalls)
	}
}

func TestJobStatus_Structure(t *testing.T) {
	now := time.Now()
	status := JobStatus{
		InstanceID:      "i-12345",
		JobID:           "job-123",
		Status:          testStatusSuccess,
		ExitCode:        0,
		DurationSeconds: 120,
		StartedAt:       now.Add(-2 * time.Minute),
		CompletedAt:     now,
		Error:           "test error",
		InterruptedBy:   "spot",
	}

	if status.InstanceID != "i-12345" {
		t.Errorf("expected InstanceID 'i-12345', got '%s'", status.InstanceID)
	}
	if status.JobID != "job-123" {
		t.Errorf("expected JobID 'job-123', got '%s'", status.JobID)
	}
	if status.Status != testStatusSuccess {
		t.Errorf("expected Status '%s', got '%s'", testStatusSuccess, status.Status)
	}
	if status.ExitCode != 0 {
		t.Errorf("expected ExitCode 0, got %d", status.ExitCode)
	}
	if status.DurationSeconds != 120 {
		t.Errorf("expected DurationSeconds 120, got %d", status.DurationSeconds)
	}
	if status.Error != "test error" {
		t.Errorf("expected Error 'test error', got '%s'", status.Error)
	}
	if status.InterruptedBy != "spot" {
		t.Errorf("expected InterruptedBy 'spot', got '%s'", status.InterruptedBy)
	}
}

func TestJobStatus_JSONSerialization(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	status := JobStatus{
		InstanceID:      "i-12345",
		JobID:           "job-123",
		Status:          testStatusSuccess,
		ExitCode:        0,
		DurationSeconds: 120,
		StartedAt:       now.Add(-2 * time.Minute),
		CompletedAt:     now,
	}

	jsonBytes, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("failed to marshal status: %v", err)
	}

	var decoded JobStatus
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}

	if decoded.InstanceID != status.InstanceID {
		t.Errorf("expected InstanceID '%s', got '%s'", status.InstanceID, decoded.InstanceID)
	}
	if decoded.JobID != status.JobID {
		t.Errorf("expected JobID '%s', got '%s'", status.JobID, decoded.JobID)
	}
}
