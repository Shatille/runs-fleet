package events

import (
	"context"
	"testing"

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
	PublishSpotInterruptionFunc func(ctx context.Context)
	PublishFleetSizeFunc        func(ctx context.Context, size float64)
}

func (m *MockMetricsAPI) PublishSpotInterruption(ctx context.Context) {
	if m.PublishSpotInterruptionFunc != nil {
		m.PublishSpotInterruptionFunc(ctx)
	}
}

func (m *MockMetricsAPI) PublishFleetSize(ctx context.Context, size float64) {
	if m.PublishFleetSizeFunc != nil {
		m.PublishFleetSizeFunc(ctx, size)
	}
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
			expectMetrics: func(t *testing.T, m *MockMetricsAPI) {
				m.PublishSpotInterruptionFunc = func(ctx context.Context) {
					// Success if called
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
				m.PublishFleetSizeFunc = func(ctx context.Context, size float64) {
					if size != -1 {
						t.Errorf("Expected fleet size change -1, got %f", size)
					}
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
				m.PublishFleetSizeFunc = func(ctx context.Context, size float64) {
					if size != 1 {
						t.Errorf("Expected fleet size change 1, got %f", size)
					}
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
				mockMetrics.PublishSpotInterruptionFunc = func(ctx context.Context) {
					called = true
					if originalFunc != nil {
						originalFunc(ctx)
					}
				}
			} else {
				originalFunc := mockMetrics.PublishFleetSizeFunc
				mockMetrics.PublishFleetSizeFunc = func(ctx context.Context, size float64) {
					called = true
					if originalFunc != nil {
						originalFunc(ctx, size)
					}
				}
			}

			handler := &Handler{
				queueClient: &MockQueueAPI{
					DeleteMessageFunc: func(ctx context.Context, receiptHandle string) error {
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

			// Access private method via reflection or just export it for testing?
			// For simplicity in this context, I'll assume I can call processEvent if I export it or move test to same package.
			// Since test is in same package `events`, I can call private methods if they are not exported but test file is `package events`.
			// Wait, `processEvent` is lowercase, so it's private.
			// But `handler_test.go` is `package events`, so it can access it.
			handler.processEvent(context.Background(), msg)

			if !called {
				t.Errorf("Expected metric function was not called")
			}
		})
	}
}
