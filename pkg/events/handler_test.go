package events

import (
	"context"
	"fmt"
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
