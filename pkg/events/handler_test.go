package events

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// MockQueueAPI implements QueueAPI interface
type MockQueueAPI struct {
	ReceiveMessagesFunc func(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]types.Message, error)
	DeleteMessageFunc   func(ctx context.Context, receiptHandle string) error
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

// MockDBAPI implements DBAPI interface
type MockDBAPI struct{}

// MockMetricsAPI implements MetricsAPI interface
type MockMetricsAPI struct {
	PublishSpotInterruptionFunc func(ctx context.Context) error
	PublishFleetSizeFunc        func(ctx context.Context, size float64) error
}

func (m *MockMetricsAPI) PublishSpotInterruption(ctx context.Context) error {
	if m.PublishSpotInterruptionFunc != nil {
		return m.PublishSpotInterruptionFunc(ctx)
	}
	return nil
}

func (m *MockMetricsAPI) PublishFleetSize(ctx context.Context, size float64) error {
	if m.PublishFleetSizeFunc != nil {
		return m.PublishFleetSizeFunc(ctx, size)
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
			expectMetrics: func(t *testing.T, m *MockMetricsAPI) {
				m.PublishFleetSizeFunc = func(_ context.Context, size float64) error {
					if size != -1 {
						t.Errorf("Expected fleet size change -1, got %f", size)
					}
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
			expectMetrics: func(t *testing.T, m *MockMetricsAPI) {
				m.PublishFleetSizeFunc = func(_ context.Context, size float64) error {
					if size != 1 {
						t.Errorf("Expected fleet size change 1, got %f", size)
					}
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
			expectMetrics: func(t *testing.T, m *MockMetricsAPI) {
				m.PublishFleetSizeFunc = func(_ context.Context, size float64) error {
					if size != -1 {
						t.Errorf("Expected fleet size change -1 for stopped, got %f", size)
					}
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
			if tt.name == "Spot Interruption" {
				originalFunc := mockMetrics.PublishSpotInterruptionFunc
				mockMetrics.PublishSpotInterruptionFunc = func(ctx context.Context) error {
					called = true
					if originalFunc != nil {
						return originalFunc(ctx)
					}
					return nil
				}
			} else {
				originalFunc := mockMetrics.PublishFleetSizeFunc
				mockMetrics.PublishFleetSizeFunc = func(ctx context.Context, size float64) error {
					called = true
					if originalFunc != nil {
						return originalFunc(ctx, size)
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
		name              string
		eventBody         *string
		receiptHandle     *string
		metricsError      error
		expectDeleteCall  bool
	}{
		{
			name:             "Nil Body",
			eventBody:        nil,
			receiptHandle:    aws.String("receipt-handle"),
			expectDeleteCall: false,
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
			expectDeleteCall: false,
		},
		{
			name: "Unknown Event Type",
			eventBody: aws.String(`{
				"detail-type": "Unknown Event",
				"detail": {}
			}`),
			receiptHandle:    aws.String("receipt-handle"),
			expectDeleteCall: false,
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
			expectDeleteCall: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deleteCalled := false
			mockMetrics := &MockMetricsAPI{
				PublishSpotInterruptionFunc: func(_ context.Context) error {
					return tt.metricsError
				},
				PublishFleetSizeFunc: func(_ context.Context, _ float64) error {
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
		expectedDelay time.Duration
	}{
		{
			name:          "Success on first try",
			failCount:     0,
			expectedCalls: 1,
			expectedDelay: 0,
		},
		{
			name:          "Success on second try",
			failCount:     1,
			expectedCalls: 2,
			expectedDelay: 100 * time.Millisecond,
		},
		{
			name:          "Success on third try",
			failCount:     2,
			expectedCalls: 3,
			expectedDelay: 600 * time.Millisecond,
		},
		{
			name:          "Fail all retries",
			failCount:     3,
			expectedCalls: 3,
			expectedDelay: 600 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callCount := 0
			startTime := time.Now()
			var mu sync.Mutex

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

			if tt.expectedDelay > 0 {
				if elapsed < tt.expectedDelay {
					t.Errorf("Expected delay of at least %v, got %v", tt.expectedDelay, elapsed)
				}
			}
		})
	}
}
