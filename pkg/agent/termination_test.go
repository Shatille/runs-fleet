package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func init() {
	// Use minimal delays in tests to avoid slow test execution
	telemetryRetryBaseDelay = 1 * time.Millisecond
	ec2TerminationDelay = 1 * time.Millisecond
}

const (
	testInstanceID      = "i-12345"
	testStatusFailureTm = "failure"
)

type mockEC2API struct {
	mu                     sync.Mutex
	terminateInstancesFunc func(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	terminatedInstances    []string
}

func (m *mockEC2API) TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.terminatedInstances = append(m.terminatedInstances, params.InstanceIds...)
	if m.terminateInstancesFunc != nil {
		return m.terminateInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (m *mockEC2API) getTerminatedInstances() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.terminatedInstances
}

func TestTerminator_TerminateInstance(t *testing.T) {
	mockEC2 := &mockEC2API{}
	mockSQS := &mockTelemetrySQSAPI{}

	telemetry := &Telemetry{
		sqsClient: mockSQS,
		queueURL:  "https://sqs.example.com/test-queue",
		logger:    &mockLogger{},
	}

	terminator := &Terminator{
		ec2Client: mockEC2,
		telemetry: telemetry,
		logger:    &mockLogger{},
	}

	status := JobStatus{
		InstanceID:      testInstanceID,
		JobID:           "job-123",
		ExitCode:        0,
		DurationSeconds: 60,
		CompletedAt:     time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	err := terminator.TerminateInstance(ctx, testInstanceID, status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify telemetry was sent
	messages := mockSQS.getMessages()
	if len(messages) != 1 {
		t.Errorf("expected 1 telemetry message, got %d", len(messages))
	}

	// Verify instance was terminated
	terminated := mockEC2.getTerminatedInstances()
	if len(terminated) != 1 || terminated[0] != testInstanceID {
		t.Errorf("expected instance %s to be terminated, got %v", testInstanceID, terminated)
	}
}

func TestTerminator_TerminateInstance_NoTelemetry(t *testing.T) {
	mockEC2 := &mockEC2API{}

	terminator := &Terminator{
		ec2Client: mockEC2,
		telemetry: nil,
		logger:    &mockLogger{},
	}

	status := JobStatus{
		InstanceID: testInstanceID,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	err := terminator.TerminateInstance(ctx, testInstanceID, status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	terminated := mockEC2.getTerminatedInstances()
	if len(terminated) != 1 {
		t.Errorf("expected 1 terminated instance, got %d", len(terminated))
	}
}

func TestTerminator_TerminateInstance_TelemetryError(t *testing.T) {
	mockEC2 := &mockEC2API{}
	mockSQS := &mockTelemetrySQSAPI{
		sendMessageFunc: func(_ context.Context, _ *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
			return nil, errors.New("telemetry error")
		},
	}

	telemetry := &Telemetry{
		sqsClient: mockSQS,
		queueURL:  "https://sqs.example.com/test-queue",
		logger:    &mockLogger{},
	}

	terminator := &Terminator{
		ec2Client: mockEC2,
		telemetry: telemetry,
		logger:    &mockLogger{},
	}

	status := JobStatus{
		InstanceID: testInstanceID,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	// Should continue with termination even if telemetry fails
	err := terminator.TerminateInstance(ctx, testInstanceID, status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	terminated := mockEC2.getTerminatedInstances()
	if len(terminated) != 1 {
		t.Errorf("expected 1 terminated instance despite telemetry error, got %d", len(terminated))
	}
}

func TestTerminator_TerminateInstance_EC2Error(t *testing.T) {
	mockEC2 := &mockEC2API{
		terminateInstancesFunc: func(_ context.Context, _ *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			return nil, errors.New("EC2 error")
		},
	}

	terminator := &Terminator{
		ec2Client: mockEC2,
		telemetry: nil,
		logger:    &mockLogger{},
	}

	status := JobStatus{
		InstanceID: testInstanceID,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	err := terminator.TerminateInstance(ctx, testInstanceID, status)
	if err == nil {
		t.Error("expected error for EC2 failure")
	}
}

func TestTerminator_TerminateWithStatus(t *testing.T) {
	mockEC2 := &mockEC2API{}

	terminator := &Terminator{
		ec2Client: mockEC2,
		telemetry: nil,
		logger:    &mockLogger{},
	}

	err := terminator.TerminateWithStatus(testInstanceID, "success", 0, 60*time.Second, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	terminated := mockEC2.getTerminatedInstances()
	if len(terminated) != 1 || terminated[0] != testInstanceID {
		t.Errorf("expected instance %s to be terminated, got %v", testInstanceID, terminated)
	}
}

func TestTerminator_TerminateWithStatus_WithError(t *testing.T) {
	mockEC2 := &mockEC2API{}
	mockSQS := &mockTelemetrySQSAPI{}

	telemetry := &Telemetry{
		sqsClient: mockSQS,
		queueURL:  "https://sqs.example.com/test-queue",
		logger:    &mockLogger{},
	}

	terminator := &Terminator{
		ec2Client: mockEC2,
		telemetry: telemetry,
		logger:    &mockLogger{},
	}

	err := terminator.TerminateWithStatus(testInstanceID, testStatusFailureTm, 1, 30*time.Second, "job failed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTerminator_TerminateOnPanic(t *testing.T) {
	mockEC2 := &mockEC2API{}
	mockSQS := &mockTelemetrySQSAPI{}

	telemetry := &Telemetry{
		sqsClient: mockSQS,
		queueURL:  "https://sqs.example.com/test-queue",
		logger:    &mockLogger{},
	}

	terminator := &Terminator{
		ec2Client: mockEC2,
		telemetry: telemetry,
		logger:    &mockLogger{},
	}

	terminator.TerminateOnPanic(testInstanceID, "job-123", "panic: test panic")

	terminated := mockEC2.getTerminatedInstances()
	if len(terminated) != 1 || terminated[0] != testInstanceID {
		t.Errorf("expected instance %s to be terminated on panic, got %v", testInstanceID, terminated)
	}

	// Verify telemetry was sent with failure status
	messages := mockSQS.getMessages()
	if len(messages) != 1 {
		t.Errorf("expected 1 telemetry message, got %d", len(messages))
	}
}
