package events

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// MockQueueAPI implements QueueAPI interface
type MockQueueAPI struct {
	ReceiveMessagesFunc func(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]types.Message, error)
	DeleteMessageFunc   func(ctx context.Context, receiptHandle string) error
	SendMessageFunc     func(ctx context.Context, job *queue.JobMessage) error
}

func (m *MockQueueAPI) ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]types.Message, error) {
	if m.ReceiveMessagesFunc != nil {
		return m.ReceiveMessagesFunc(ctx, maxMessages, waitTimeSeconds)
	}
	return nil, nil
}

func (m *MockQueueAPI) DeleteMessage(ctx context.Context, receiptHandle string) error {
	if m.DeleteMessageFunc != nil {
		return m.DeleteMessageFunc(ctx, receiptHandle)
	}
	return nil
}

func (m *MockQueueAPI) SendMessage(ctx context.Context, job *queue.JobMessage) error {
	if m.SendMessageFunc != nil {
		return m.SendMessageFunc(ctx, job)
	}
	return nil
}

// MockDBAPI implements DBAPI interface
type MockDBAPI struct {
	MarkInstanceTerminatingFunc func(ctx context.Context, instanceID string) error
	GetJobByInstanceFunc        func(ctx context.Context, instanceID string) (*JobInfo, error)
}

func (m *MockDBAPI) MarkInstanceTerminating(ctx context.Context, instanceID string) error {
	if m.MarkInstanceTerminatingFunc != nil {
		return m.MarkInstanceTerminatingFunc(ctx, instanceID)
	}
	return nil
}

func (m *MockDBAPI) GetJobByInstance(ctx context.Context, instanceID string) (*JobInfo, error) {
	if m.GetJobByInstanceFunc != nil {
		return m.GetJobByInstanceFunc(ctx, instanceID)
	}
	return nil, nil
}

// MockMetricsAPI implements MetricsAPI interface
type MockMetricsAPI struct {
	PublishSpotInterruptionFunc       func(ctx context.Context) error
	PublishFleetSizeIncrementFunc     func(ctx context.Context) error
	PublishFleetSizeDecrementFunc     func(ctx context.Context) error
	PublishMessageDeletionFailureFunc func(ctx context.Context) error
}

func (m *MockMetricsAPI) PublishSpotInterruption(ctx context.Context) error {
	if m.PublishSpotInterruptionFunc != nil {
		return m.PublishSpotInterruptionFunc(ctx)
	}
	return nil
}

func (m *MockMetricsAPI) PublishFleetSizeIncrement(ctx context.Context) error {
	if m.PublishFleetSizeIncrementFunc != nil {
		return m.PublishFleetSizeIncrementFunc(ctx)
	}
	return nil
}

func (m *MockMetricsAPI) PublishFleetSizeDecrement(ctx context.Context) error {
	if m.PublishFleetSizeDecrementFunc != nil {
		return m.PublishFleetSizeDecrementFunc(ctx)
	}
	return nil
}

func (m *MockMetricsAPI) PublishMessageDeletionFailure(ctx context.Context) error {
	if m.PublishMessageDeletionFailureFunc != nil {
		return m.PublishMessageDeletionFailureFunc(ctx)
	}
	return nil
}

func TestProcessEvent(t *testing.T) {
	tests := []struct {
		name          string
		eventBody     string
		expectMetrics func(t *testing.T, m *MockMetricsAPI)
	}{
		{
			name: "Spot Interruption",
			eventBody: `{
				"detail-type": "EC2 Spot Instance Interruption Warning",
				"detail": {
					"instance-id": "i-1234567890abcdef0",
					"instance-action": "terminate"
				}
			}`,
			expectMetrics: func(_ *testing.T, m *MockMetricsAPI) {
				m.PublishSpotInterruptionFunc = func(_ context.Context) error {
					return nil
				}
			},
		},
		{
			name: "State Change - Terminated",
			eventBody: `{
				"detail-type": "EC2 Instance State-change Notification",
				"detail": {
					"instance-id": "i-1234567890abcdef0",
					"state": "terminated"
				}
			}`,
			expectMetrics: func(_ *testing.T, m *MockMetricsAPI) {
				m.PublishFleetSizeDecrementFunc = func(_ context.Context) error {
					return nil
				}
			},
		},
		{
			name: "State Change - Running",
			eventBody: `{
				"detail-type": "EC2 Instance State-change Notification",
				"detail": {
					"instance-id": "i-1234567890abcdef0",
					"state": "running"
				}
			}`,
			expectMetrics: func(_ *testing.T, m *MockMetricsAPI) {
				m.PublishFleetSizeIncrementFunc = func(_ context.Context) error {
					return nil
				}
			},
		},
		{
			name: "State Change - Stopped",
			eventBody: `{
				"detail-type": "EC2 Instance State-change Notification",
				"detail": {
					"instance-id": "i-1234567890abcdef0",
					"state": "stopped"
				}
			}`,
			expectMetrics: func(_ *testing.T, m *MockMetricsAPI) {
				m.PublishFleetSizeDecrementFunc = func(_ context.Context) error {
					return nil
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockMetrics := &MockMetricsAPI{}
			if tt.expectMetrics != nil {
				tt.expectMetrics(t, mockMetrics)
			}

			// Ensure mocks are called
			called := false
			switch tt.name {
			case "Spot Interruption":
				originalFunc := mockMetrics.PublishSpotInterruptionFunc
				mockMetrics.PublishSpotInterruptionFunc = func(ctx context.Context) error {
					called = true
					if originalFunc != nil {
						return originalFunc(ctx)
					}
					return nil
				}
			case "State Change - Running":
				originalFunc := mockMetrics.PublishFleetSizeIncrementFunc
				mockMetrics.PublishFleetSizeIncrementFunc = func(ctx context.Context) error {
					called = true
					if originalFunc != nil {
						return originalFunc(ctx)
					}
					return nil
				}
			default:
				originalFunc := mockMetrics.PublishFleetSizeDecrementFunc
				mockMetrics.PublishFleetSizeDecrementFunc = func(ctx context.Context) error {
					called = true
					if originalFunc != nil {
						return originalFunc(ctx)
					}
					return nil
				}
			}

			handler := &Handler{
				queueClient: &MockQueueAPI{
					DeleteMessageFunc: func(_ context.Context, _ string) error {
						return nil
					},
				},
				dbClient: &MockDBAPI{},
				metrics:  mockMetrics,
				config:   &config.Config{},
			}

			msg := types.Message{
				Body:          aws.String(tt.eventBody),
				ReceiptHandle: aws.String("receipt-handle"),
			}

			handler.processEvent(context.Background(), msg)

			if !called {
				t.Errorf("Expected metric function was not called")
			}
		})
	}
}

func TestProcessEventErrors(t *testing.T) {
	tests := []struct {
		name             string
		eventBody        *string
		receiptHandle    *string
		metricsError     error
		expectDeleteCall bool
	}{
		{
			name:             "Nil Body",
			eventBody:        nil,
			receiptHandle:    aws.String("receipt-handle"),
			expectDeleteCall: true,
		},
		{
			name:             "Nil Receipt Handle",
			eventBody:        aws.String(`{}`),
			receiptHandle:    nil,
			expectDeleteCall: false,
		},
		{
			name:             "Malformed JSON",
			eventBody:        aws.String(`{invalid json}`),
			receiptHandle:    aws.String("receipt-handle"),
			expectDeleteCall: true,
		},
		{
			name: "Unknown Event Type",
			eventBody: aws.String(`{
				"detail-type": "Unknown Event",
				"detail": {}
			}`),
			receiptHandle:    aws.String("receipt-handle"),
			expectDeleteCall: true,
		},
		{
			name: "Metrics Failure",
			eventBody: aws.String(`{
				"detail-type": "EC2 Spot Instance Interruption Warning",
				"detail": {
					"instance-id": "i-1234567890abcdef0",
					"instance-action": "terminate"
				}
			}`),
			receiptHandle:    aws.String("receipt-handle"),
			metricsError:     fmt.Errorf("cloudwatch throttled"),
			expectDeleteCall: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deleteCalled := false
			mockMetrics := &MockMetricsAPI{
				PublishSpotInterruptionFunc: func(_ context.Context) error {
					return tt.metricsError
				},
				PublishFleetSizeIncrementFunc: func(_ context.Context) error {
					return tt.metricsError
				},
				PublishFleetSizeDecrementFunc: func(_ context.Context) error {
					return tt.metricsError
				},
			}

			handler := &Handler{
				queueClient: &MockQueueAPI{
					DeleteMessageFunc: func(_ context.Context, _ string) error {
						deleteCalled = true
						return nil
					},
				},
				dbClient: &MockDBAPI{},
				metrics:  mockMetrics,
				config:   &config.Config{},
			}

			msg := types.Message{
				Body:          tt.eventBody,
				ReceiptHandle: tt.receiptHandle,
			}

			handler.processEvent(context.Background(), msg)

			if deleteCalled != tt.expectDeleteCall {
				t.Errorf("DeleteMessage called = %v, want %v", deleteCalled, tt.expectDeleteCall)
			}
		})
	}
}

func TestDeleteMessageFailureMetric(t *testing.T) {
	metricCalled := false
	mockMetrics := &MockMetricsAPI{
		PublishSpotInterruptionFunc: func(_ context.Context) error {
			return nil
		},
		PublishMessageDeletionFailureFunc: func(_ context.Context) error {
			metricCalled = true
			return nil
		},
	}

	handler := &Handler{
		queueClient: &MockQueueAPI{
			DeleteMessageFunc: func(_ context.Context, _ string) error {
				return fmt.Errorf("deletion failed")
			},
		},
		dbClient: &MockDBAPI{},
		metrics:  mockMetrics,
		config:   &config.Config{},
	}

	msg := types.Message{
		Body: aws.String(`{
			"detail-type": "EC2 Spot Instance Interruption Warning",
			"detail": {
				"instance-id": "i-test",
				"instance-action": "terminate"
			}
		}`),
		ReceiptHandle: aws.String("receipt-test"),
	}

	handler.processEvent(context.Background(), msg)

	if !metricCalled {
		t.Error("Expected PublishMessageDeletionFailure to be called on deletion failure")
	}
}

func TestHandlerRunContextCancellation(t *testing.T) {
	handler := &Handler{
		queueClient: &MockQueueAPI{
			ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]types.Message, error) {
				return nil, nil
			},
		},
		dbClient: &MockDBAPI{},
		metrics:  &MockMetricsAPI{},
		config:   &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan bool)
	go func() {
		handler.Run(ctx)
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Error("Handler did not stop on context cancellation")
	}
}

func TestHandlerReceiveMessagesTimeout(t *testing.T) {
	receiveCallCount := 0
	handler := &Handler{
		queueClient: &MockQueueAPI{
			ReceiveMessagesFunc: func(ctx context.Context, _ int32, _ int32) ([]types.Message, error) {
				receiveCallCount++
				_, ok := ctx.Deadline()
				if !ok {
					t.Error("Expected context with deadline, got none")
				}
				return nil, nil
			},
		},
		dbClient: &MockDBAPI{},
		metrics:  &MockMetricsAPI{},
		config:   &config.Config{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan bool)
	go func() {
		handler.Run(ctx)
		done <- true
	}()

	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done

	if receiveCallCount == 0 {
		t.Error("ReceiveMessages was never called")
	}
}

func TestHandlerReceiveMessagesHasIndependentTimeout(t *testing.T) {
	var capturedTime time.Time
	var capturedDeadline time.Time
	handler := &Handler{
		queueClient: &MockQueueAPI{
			ReceiveMessagesFunc: func(ctx context.Context, _ int32, _ int32) ([]types.Message, error) {
				capturedTime = time.Now()
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("Expected context with deadline, got none")
				}
				capturedDeadline = deadline
				return nil, nil
			},
		},
		dbClient: &MockDBAPI{},
		metrics:  &MockMetricsAPI{},
		config:   &config.Config{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	done := make(chan bool)
	go func() {
		handler.Run(ctx)
		done <- true
	}()

	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done

	timeoutDuration := capturedDeadline.Sub(capturedTime)
	if timeoutDuration < 23*time.Second || timeoutDuration > 26*time.Second {
		t.Errorf("Expected timeout ~25s from call time, got %v", timeoutDuration)
	}
}

func TestConcurrentEventProcessing(t *testing.T) {
	const numMessages = 10
	const processingDelay = 100 * time.Millisecond

	var processStartTimes []time.Time
	var mu sync.Mutex

	messages := make([]types.Message, numMessages)
	for i := 0; i < numMessages; i++ {
		messages[i] = types.Message{
			Body: aws.String(fmt.Sprintf(`{
				"detail-type": "EC2 Spot Instance Interruption Warning",
				"detail": {"instance-id": "i-%d", "instance-action": "terminate"}
			}`, i)),
			ReceiptHandle: aws.String(fmt.Sprintf("receipt-%d", i)),
		}
	}

	mockMetrics := &MockMetricsAPI{
		PublishSpotInterruptionFunc: func(_ context.Context) error {
			mu.Lock()
			processStartTimes = append(processStartTimes, time.Now())
			mu.Unlock()
			time.Sleep(processingDelay)
			return nil
		},
	}

	handler := &Handler{
		queueClient: &MockQueueAPI{
			ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]types.Message, error) {
				return messages, nil
			},
			DeleteMessageFunc: func(_ context.Context, _ string) error {
				return nil
			},
		},
		dbClient: &MockDBAPI{},
		metrics:  mockMetrics,
		config:   &config.Config{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan bool)
	go func() {
		handler.Run(ctx)
		done <- true
	}()

	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done

	if len(processStartTimes) != numMessages {
		t.Fatalf("Expected %d messages processed, got %d", numMessages, len(processStartTimes))
	}

	firstStart := processStartTimes[0]
	var concurrentStarts int
	for _, startTime := range processStartTimes[1:] {
		if startTime.Sub(firstStart) < processingDelay/2 {
			concurrentStarts++
		}
	}

	if concurrentStarts < 3 {
		t.Errorf("Expected concurrent processing (at least 3 concurrent), but only %d/%d messages started concurrently", concurrentStarts, numMessages-1)
	}
}

func TestBoundedConcurrency(t *testing.T) {
	const numMessages = 20
	const maxConcurrency = 5
	const processingDelay = 50 * time.Millisecond

	var activeTasks int32
	var maxActive int32
	var mu sync.Mutex

	messages := make([]types.Message, numMessages)
	for i := 0; i < numMessages; i++ {
		messages[i] = types.Message{
			Body: aws.String(fmt.Sprintf(`{
				"detail-type": "EC2 Spot Instance Interruption Warning",
				"detail": {"instance-id": "i-%d", "instance-action": "terminate"}
			}`, i)),
			ReceiptHandle: aws.String(fmt.Sprintf("receipt-%d", i)),
		}
	}

	mockMetrics := &MockMetricsAPI{
		PublishSpotInterruptionFunc: func(_ context.Context) error {
			active := atomic.AddInt32(&activeTasks, 1)

			mu.Lock()
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()

			time.Sleep(processingDelay)
			atomic.AddInt32(&activeTasks, -1)
			return nil
		},
	}

	handler := &Handler{
		queueClient: &MockQueueAPI{
			ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]types.Message, error) {
				return messages, nil
			},
			DeleteMessageFunc: func(_ context.Context, _ string) error {
				return nil
			},
		},
		dbClient: &MockDBAPI{},
		metrics:  mockMetrics,
		config:   &config.Config{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan bool)
	go func() {
		handler.Run(ctx)
		done <- true
	}()

	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done

	if maxActive > maxConcurrency+1 {
		t.Errorf("Expected max concurrency ~%d, got %d", maxConcurrency, maxActive)
	}
	if maxActive < 2 {
		t.Error("Expected some concurrency, but processing appears to be sequential")
	}
}

func TestMetricsCallsHaveTimeout(t *testing.T) {
	var capturedCtx context.Context
	mockMetrics := &MockMetricsAPI{
		PublishSpotInterruptionFunc: func(ctx context.Context) error {
			capturedCtx = ctx
			return nil
		},
	}

	handler := &Handler{
		queueClient: &MockQueueAPI{
			ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]types.Message, error) {
				return []types.Message{{
					Body: aws.String(`{
						"detail-type": "EC2 Spot Instance Interruption Warning",
						"detail": {"instance-id": "i-test", "instance-action": "terminate"}
					}`),
					ReceiptHandle: aws.String("receipt-test"),
				}}, nil
			},
			DeleteMessageFunc: func(_ context.Context, _ string) error {
				return nil
			},
		},
		dbClient: &MockDBAPI{},
		metrics:  mockMetrics,
		config:   &config.Config{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan bool)
	go func() {
		handler.Run(ctx)
		done <- true
	}()

	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done

	if capturedCtx == nil {
		t.Fatal("PublishSpotInterruption was never called")
	}

	_, ok := capturedCtx.Deadline()
	if !ok {
		t.Error("Expected metrics call to have deadline, got none")
	}
}

func TestMetricsTimeoutDoesNotBlockProcessing(t *testing.T) {
	messages := []types.Message{
		{
			Body: aws.String(`{
				"detail-type": "EC2 Spot Instance Interruption Warning",
				"detail": {"instance-id": "i-1", "instance-action": "terminate"}
			}`),
			ReceiptHandle: aws.String("receipt-1"),
		},
		{
			Body: aws.String(`{
				"detail-type": "EC2 Spot Instance Interruption Warning",
				"detail": {"instance-id": "i-2", "instance-action": "terminate"}
			}`),
			ReceiptHandle: aws.String("receipt-2"),
		},
	}

	callCount := 0
	var mu sync.Mutex
	mockMetrics := &MockMetricsAPI{
		PublishSpotInterruptionFunc: func(_ context.Context) error {
			mu.Lock()
			callCount++
			isFirst := callCount == 1
			mu.Unlock()

			if isFirst {
				time.Sleep(100 * time.Millisecond)
			}
			return nil
		},
	}

	handler := &Handler{
		queueClient: &MockQueueAPI{
			ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]types.Message, error) {
				return messages, nil
			},
			DeleteMessageFunc: func(_ context.Context, _ string) error {
				return nil
			},
		},
		dbClient: &MockDBAPI{},
		metrics:  mockMetrics,
		config:   &config.Config{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan bool)
	go func() {
		handler.Run(ctx)
		done <- true
	}()

	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done

	if callCount != 2 {
		t.Errorf("Expected 2 metrics calls, got %d", callCount)
	}
}

func TestMetricsRetryWithExponentialBackoff(t *testing.T) {
	tests := []struct {
		name          string
		failCount     int
		expectedCalls int
		minDelay      time.Duration
	}{
		{
			name:          "Success on first try",
			failCount:     0,
			expectedCalls: 1,
			minDelay:      0,
		},
		{
			name:          "Success on second try",
			failCount:     1,
			expectedCalls: 2,
			minDelay:      100 * time.Millisecond,
		},
		{
			name:          "Success on third try",
			failCount:     2,
			expectedCalls: 3,
			minDelay:      600 * time.Millisecond,
		},
		{
			name:          "Fail all retries",
			failCount:     3,
			expectedCalls: 3,
			minDelay:      600 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callCount := 0
			startTime := time.Now()
			var mu sync.Mutex
			messageReturned := false

			mockMetrics := &MockMetricsAPI{
				PublishSpotInterruptionFunc: func(_ context.Context) error {
					mu.Lock()
					callCount++
					currentCall := callCount
					mu.Unlock()

					if currentCall <= tt.failCount {
						return fmt.Errorf("cloudwatch throttled")
					}
					return nil
				},
			}

			handler := &Handler{
				queueClient: &MockQueueAPI{
					ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]types.Message, error) {
						mu.Lock()
						defer mu.Unlock()
						if messageReturned {
							return nil, nil
						}
						messageReturned = true
						return []types.Message{{
							Body: aws.String(`{
								"detail-type": "EC2 Spot Instance Interruption Warning",
								"detail": {"instance-id": "i-test", "instance-action": "terminate"}
							}`),
							ReceiptHandle: aws.String("receipt-test"),
						}}, nil
					},
					DeleteMessageFunc: func(_ context.Context, _ string) error {
						return nil
					},
				},
				dbClient: &MockDBAPI{},
				metrics:  mockMetrics,
				config:   &config.Config{},
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			done := make(chan bool)
			go func() {
				handler.Run(ctx)
				done <- true
			}()

			time.Sleep(2 * time.Second)
			cancel()
			<-done

			elapsed := time.Since(startTime)

			mu.Lock()
			actualCallCount := callCount
			mu.Unlock()

			if actualCallCount != tt.expectedCalls {
				t.Errorf("Expected %d calls, got %d", tt.expectedCalls, actualCallCount)
			}

			if tt.minDelay > 0 {
				if elapsed < tt.minDelay {
					t.Errorf("Expected delay of at least %v, got %v", tt.minDelay, elapsed)
				}
			}
		})
	}
}

func TestPanicRecovery(t *testing.T) {
	panicCount := 0
	processCount := 0
	deletedReceipts := make(map[string]bool)
	var mu sync.Mutex

	messages := []types.Message{
		{
			Body: aws.String(`{
				"detail-type": "EC2 Spot Instance Interruption Warning",
				"detail": {"instance-id": "i-panic", "instance-action": "terminate"}
			}`),
			ReceiptHandle: aws.String("receipt-panic"),
		},
		{
			Body: aws.String(`{
				"detail-type": "EC2 Spot Instance Interruption Warning",
				"detail": {"instance-id": "i-normal", "instance-action": "terminate"}
			}`),
			ReceiptHandle: aws.String("receipt-normal"),
		},
	}

	mockMetrics := &MockMetricsAPI{
		PublishSpotInterruptionFunc: func(_ context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			processCount++
			if processCount == 1 {
				panicCount++
				panic("test panic in processEvent")
			}
			return nil
		},
	}

	handler := &Handler{
		queueClient: &MockQueueAPI{
			ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]types.Message, error) {
				mu.Lock()
				defer mu.Unlock()
				if processCount >= 2 {
					return nil, nil
				}
				return messages, nil
			},
			DeleteMessageFunc: func(_ context.Context, receiptHandle string) error {
				mu.Lock()
				defer mu.Unlock()
				deletedReceipts[receiptHandle] = true
				return nil
			},
		},
		dbClient: &MockDBAPI{},
		metrics:  mockMetrics,
		config:   &config.Config{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan bool)
	go func() {
		handler.Run(ctx)
		done <- true
	}()

	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	finalPanicCount := panicCount
	finalProcessCount := processCount
	panicReceiptDeleted := deletedReceipts["receipt-panic"]
	normalReceiptDeleted := deletedReceipts["receipt-normal"]
	mu.Unlock()

	if finalPanicCount != 1 {
		t.Errorf("Expected 1 panic, got %d", finalPanicCount)
	}
	if finalProcessCount < 2 {
		t.Errorf("Expected at least 2 messages processed despite panic, got %d", finalProcessCount)
	}
	if !panicReceiptDeleted {
		t.Error("Expected message with panic to be deleted, but it was not")
	}
	if !normalReceiptDeleted {
		t.Error("Expected normal message to be deleted, but it was not")
	}
}

func TestSpotInterruptionHandling(t *testing.T) {
	tests := []struct {
		name              string
		instanceID        string
		job               *JobInfo
		dbMarkErr         error
		dbGetErr          error
		queueSendErr      error
		expectMarkCalled  bool
		expectGetCalled   bool
		expectQueueCalled bool
		expectError       bool
	}{
		{
			name:       "Successful job re-queue",
			instanceID: "i-test123",
			job: &JobInfo{
				JobID:        "job-123",
				RunID:        "run-456",
				InstanceType: "t4g.medium",
				Pool:         "default",
				Private:      false,
				Spot:         true,
				RunnerSpec:   "2cpu-linux-arm64",
			},
			expectMarkCalled:  true,
			expectGetCalled:   true,
			expectQueueCalled: true,
			expectError:       false,
		},
		{
			name:              "No job found - skip re-queue",
			instanceID:        "i-nojob",
			job:               nil,
			expectMarkCalled:  true,
			expectGetCalled:   true,
			expectQueueCalled: false,
			expectError:       false,
		},
		{
			name:       "Invalid job data - empty JobID",
			instanceID: "i-emptyjobid",
			job: &JobInfo{
				JobID: "",
				RunID: "run-valid",
				Spot:  true,
			},
			expectMarkCalled:  true,
			expectGetCalled:   true,
			expectQueueCalled: false,
			expectError:       false,
		},
		{
			name:       "Invalid job data - empty RunID",
			instanceID: "i-emptyrunid",
			job: &JobInfo{
				JobID: "job-valid",
				RunID: "",
				Spot:  true,
			},
			expectMarkCalled:  true,
			expectGetCalled:   true,
			expectQueueCalled: false,
			expectError:       false,
		},
		{
			name:             "DB mark failure",
			instanceID:       "i-dbfail",
			dbMarkErr:        fmt.Errorf("dynamodb unavailable"),
			expectMarkCalled: true,
			expectGetCalled:  false,
			expectError:      true,
		},
		{
			name:              "DB get failure",
			instanceID:        "i-getfail",
			dbGetErr:          fmt.Errorf("query failed"),
			expectMarkCalled:  true,
			expectGetCalled:   true,
			expectQueueCalled: false,
			expectError:       true,
		},
		{
			name:       "Queue send failure",
			instanceID: "i-queuefail",
			job: &JobInfo{
				JobID: "job-fail",
				RunID: "run-fail",
				Spot:  true,
			},
			queueSendErr:      fmt.Errorf("sqs throttled"),
			expectMarkCalled:  true,
			expectGetCalled:   true,
			expectQueueCalled: true,
			expectError:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			markCalled := false
			getCalled := false
			queueCalled := false

			mockDB := &MockDBAPI{
				MarkInstanceTerminatingFunc: func(_ context.Context, instanceID string) error {
					markCalled = true
					if instanceID != tt.instanceID {
						t.Errorf("Expected instanceID %s, got %s", tt.instanceID, instanceID)
					}
					return tt.dbMarkErr
				},
				GetJobByInstanceFunc: func(_ context.Context, instanceID string) (*JobInfo, error) {
					getCalled = true
					if instanceID != tt.instanceID {
						t.Errorf("Expected instanceID %s, got %s", tt.instanceID, instanceID)
					}
					return tt.job, tt.dbGetErr
				},
			}

			mockQueue := &MockQueueAPI{
				SendMessageFunc: func(_ context.Context, job *queue.JobMessage) error {
					queueCalled = true
					if tt.job != nil {
						if job.JobID != tt.job.JobID {
							t.Errorf("Expected JobID %s, got %s", tt.job.JobID, job.JobID)
						}
						if job.RunID != tt.job.RunID {
							t.Errorf("Expected RunID %s, got %s", tt.job.RunID, job.RunID)
						}
					}
					return tt.queueSendErr
				},
				DeleteMessageFunc: func(_ context.Context, _ string) error {
					return nil
				},
			}

			mockMetrics := &MockMetricsAPI{
				PublishSpotInterruptionFunc: func(_ context.Context) error {
					return nil
				},
			}

			handler := &Handler{
				queueClient: mockQueue,
				dbClient:    mockDB,
				metrics:     mockMetrics,
				config:      &config.Config{},
			}

			eventBody := fmt.Sprintf(`{
				"detail-type": "EC2 Spot Instance Interruption Warning",
				"detail": {
					"instance-id": "%s",
					"instance-action": "terminate"
				}
			}`, tt.instanceID)

			msg := types.Message{
				Body:          aws.String(eventBody),
				ReceiptHandle: aws.String("receipt-test"),
			}

			handler.processEvent(context.Background(), msg)

			if markCalled != tt.expectMarkCalled {
				t.Errorf("MarkInstanceTerminating called = %v, want %v", markCalled, tt.expectMarkCalled)
			}
			if getCalled != tt.expectGetCalled {
				t.Errorf("GetJobByInstance called = %v, want %v", getCalled, tt.expectGetCalled)
			}
			if queueCalled != tt.expectQueueCalled {
				t.Errorf("SendMessage called = %v, want %v", queueCalled, tt.expectQueueCalled)
			}
		})
	}
}

func TestSpotInterruptionJobRequeueContent(t *testing.T) {
	expectedJob := &JobInfo{
		JobID:        "job-abc",
		RunID:        "run-xyz",
		InstanceType: "c7g.xlarge",
		Pool:         "heavy-builds",
		Private:      true,
		Spot:         true,
		RunnerSpec:   "4cpu-linux-arm64",
	}

	var capturedMessage *queue.JobMessage
	mockDB := &MockDBAPI{
		MarkInstanceTerminatingFunc: func(_ context.Context, _ string) error {
			return nil
		},
		GetJobByInstanceFunc: func(_ context.Context, _ string) (*JobInfo, error) {
			return expectedJob, nil
		},
	}

	mockQueue := &MockQueueAPI{
		SendMessageFunc: func(_ context.Context, job *queue.JobMessage) error {
			capturedMessage = job
			return nil
		},
		DeleteMessageFunc: func(_ context.Context, _ string) error {
			return nil
		},
	}

	mockMetrics := &MockMetricsAPI{
		PublishSpotInterruptionFunc: func(_ context.Context) error {
			return nil
		},
	}

	handler := &Handler{
		queueClient: mockQueue,
		dbClient:    mockDB,
		metrics:     mockMetrics,
		config:      &config.Config{},
	}

	msg := types.Message{
		Body: aws.String(`{
			"detail-type": "EC2 Spot Instance Interruption Warning",
			"detail": {
				"instance-id": "i-interrupted",
				"instance-action": "terminate"
			}
		}`),
		ReceiptHandle: aws.String("receipt-test"),
	}

	handler.processEvent(context.Background(), msg)

	if capturedMessage == nil {
		t.Fatal("Expected SendMessage to be called, but it was not")
	}

	if capturedMessage.JobID != expectedJob.JobID {
		t.Errorf("JobID: expected %s, got %s", expectedJob.JobID, capturedMessage.JobID)
	}
	if capturedMessage.RunID != expectedJob.RunID {
		t.Errorf("RunID: expected %s, got %s", expectedJob.RunID, capturedMessage.RunID)
	}
	if capturedMessage.InstanceType != expectedJob.InstanceType {
		t.Errorf("InstanceType: expected %s, got %s", expectedJob.InstanceType, capturedMessage.InstanceType)
	}
	if capturedMessage.Pool != expectedJob.Pool {
		t.Errorf("Pool: expected %s, got %s", expectedJob.Pool, capturedMessage.Pool)
	}
	if capturedMessage.Private != expectedJob.Private {
		t.Errorf("Private: expected %v, got %v", expectedJob.Private, capturedMessage.Private)
	}
	if capturedMessage.Spot != expectedJob.Spot {
		t.Errorf("Spot: expected %v, got %v", expectedJob.Spot, capturedMessage.Spot)
	}
	if capturedMessage.RunnerSpec != expectedJob.RunnerSpec {
		t.Errorf("RunnerSpec: expected %s, got %s", expectedJob.RunnerSpec, capturedMessage.RunnerSpec)
	}
}
