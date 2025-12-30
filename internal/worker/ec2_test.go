package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/queue"
)

func TestSelectSubnet(t *testing.T) {
	tests := []struct {
		name          string
		subnets       []string
		callCount     int
		expectedOrder []string
	}{
		{
			name:          "round-robin single subnet",
			subnets:       []string{"subnet-a"},
			callCount:     3,
			expectedOrder: []string{"subnet-a", "subnet-a", "subnet-a"},
		},
		{
			name:          "round-robin multiple subnets",
			subnets:       []string{"subnet-a", "subnet-b", "subnet-c"},
			callCount:     6,
			expectedOrder: []string{"subnet-a", "subnet-b", "subnet-c", "subnet-a", "subnet-b", "subnet-c"},
		},
		{
			name:          "empty subnets",
			subnets:       []string{},
			callCount:     1,
			expectedOrder: []string{""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				PublicSubnetIDs: tt.subnets,
			}
			var index uint64

			for i := 0; i < tt.callCount; i++ {
				result := SelectSubnet(cfg, &index)
				if result != tt.expectedOrder[i] {
					t.Errorf("SelectSubnet() call %d = %q, want %q", i, result, tt.expectedOrder[i])
				}
			}
		})
	}
}

func TestSelectSubnet_Concurrent(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"subnet-a", "subnet-b", "subnet-c"},
	}
	var index uint64
	var mu sync.Mutex

	// Call SelectSubnet concurrently
	done := make(chan string, 100)
	for i := 0; i < 100; i++ {
		go func() {
			done <- SelectSubnet(cfg, &index)
		}()
	}

	results := make(map[string]int)
	for i := 0; i < 100; i++ {
		subnet := <-done
		mu.Lock()
		results[subnet]++
		mu.Unlock()
	}

	// Verify all subnets were used
	if len(results) != 3 {
		t.Errorf("Expected 3 different subnets, got %d", len(results))
	}
}

func TestDeleteMessageWithRetry(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() {
		RetryDelay = origRetryDelay
	}()
	RetryDelay = 1 * time.Millisecond

	tests := []struct {
		name     string
		failures int
		wantErr  bool
	}{
		{
			name:     "success on first try",
			failures: 0,
			wantErr:  false,
		},
		{
			name:     "success on second try",
			failures: 1,
			wantErr:  false,
		},
		{
			name:     "success on third try",
			failures: 2,
			wantErr:  false,
		},
		{
			name:     "fails after max retries",
			failures: 3,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := 0
			mockQueue := &mockQueueEC2{
				DeleteMessageFunc: func(_ context.Context, _ string) error {
					attempts++
					if attempts <= tt.failures {
						return errors.New("delete failed")
					}
					return nil
				},
			}

			err := deleteMessageWithRetry(context.Background(), mockQueue, "handle")

			if (err != nil) != tt.wantErr {
				t.Errorf("deleteMessageWithRetry() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSendMessageWithRetry(t *testing.T) {
	origRetryDelay := RetryDelay
	defer func() {
		RetryDelay = origRetryDelay
	}()
	RetryDelay = 1 * time.Millisecond

	tests := []struct {
		name     string
		failures int
		wantErr  bool
	}{
		{
			name:     "success on first try",
			failures: 0,
			wantErr:  false,
		},
		{
			name:     "success on second try",
			failures: 1,
			wantErr:  false,
		},
		{
			name:     "fails after max retries",
			failures: 3,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := 0
			mockQueue := &mockQueueEC2{
				SendMessageFunc: func(_ context.Context, _ *queue.JobMessage) error {
					attempts++
					if attempts <= tt.failures {
						return errors.New("send failed")
					}
					return nil
				},
			}

			job := &queue.JobMessage{
				JobID: 12345,
				RunID: 67890,
			}

			err := sendMessageWithRetry(context.Background(), mockQueue, job)

			if (err != nil) != tt.wantErr {
				t.Errorf("sendMessageWithRetry() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEC2WorkerDeps_ZeroValues(t *testing.T) {
	// Test that zero-value EC2WorkerDeps struct compiles and has expected defaults
	deps := EC2WorkerDeps{}

	if deps.Queue != nil {
		t.Error("Expected Queue to be nil for zero value")
	}
	if deps.Fleet != nil {
		t.Error("Expected Fleet to be nil for zero value")
	}
	if deps.Pool != nil {
		t.Error("Expected Pool to be nil for zero value")
	}
	if deps.Metrics != nil {
		t.Error("Expected Metrics to be nil for zero value")
	}
	if deps.Runner != nil {
		t.Error("Expected Runner to be nil for zero value")
	}
	if deps.DB != nil {
		t.Error("Expected DB to be nil for zero value")
	}
	if deps.Config != nil {
		t.Error("Expected Config to be nil for zero value")
	}
	if deps.SubnetIndex != nil {
		t.Error("Expected SubnetIndex to be nil for zero value")
	}
}

func TestFleetRetryBaseDelay_Default(t *testing.T) {
	// Verify the default value
	if FleetRetryBaseDelay != 2*time.Second {
		t.Errorf("FleetRetryBaseDelay = %v, want 2s", FleetRetryBaseDelay)
	}
}

func TestRetryDelay_Default(t *testing.T) {
	// Verify the default value
	if RetryDelay != 1*time.Second {
		t.Errorf("RetryDelay = %v, want 1s", RetryDelay)
	}
}

// mockQueueEC2 implements queue.Queue for testing.
type mockQueueEC2 struct {
	SendMessageFunc     func(ctx context.Context, job *queue.JobMessage) error
	ReceiveMessagesFunc func(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error)
	DeleteMessageFunc   func(ctx context.Context, handle string) error
}

func (m *mockQueueEC2) SendMessage(ctx context.Context, job *queue.JobMessage) error {
	if m.SendMessageFunc != nil {
		return m.SendMessageFunc(ctx, job)
	}
	return nil
}

func (m *mockQueueEC2) ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error) {
	if m.ReceiveMessagesFunc != nil {
		return m.ReceiveMessagesFunc(ctx, maxMessages, waitTimeSeconds)
	}
	return nil, nil
}

func (m *mockQueueEC2) DeleteMessage(ctx context.Context, handle string) error {
	if m.DeleteMessageFunc != nil {
		return m.DeleteMessageFunc(ctx, handle)
	}
	return nil
}
