package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/queue"
)

func TestBuildRunnerLabel(t *testing.T) {
	tests := []struct {
		name string
		job  *queue.JobMessage
		want string
	}{
		{
			name: "uses original label when present",
			job: &queue.JobMessage{
				RunID:         "12345",
				RunnerSpec:    "2cpu-linux",
				OriginalLabel: "runs-fleet=12345/cpu=2",
				Spot:          true,
			},
			want: "runs-fleet=12345/cpu=2",
		},
		{
			name: "uses original label with flexible spec",
			job: &queue.JobMessage{
				RunID:         "67890",
				RunnerSpec:    "4cpu-linux-arm64",
				OriginalLabel: "runs-fleet=67890/cpu=4+8/arch=arm64",
				Spot:          true,
			},
			want: "runs-fleet=67890/cpu=4+8/arch=arm64",
		},
		{
			name: "basic label fallback",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "2cpu-linux-arm64",
				Spot:       true,
			},
			want: "runs-fleet=12345/runner=2cpu-linux-arm64",
		},
		{
			name: "with pool",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "2cpu-linux-arm64",
				Pool:       "default",
				Spot:       true,
			},
			want: "runs-fleet=12345/runner=2cpu-linux-arm64/pool=default",
		},
		{
			name: "with private",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "4cpu-linux-amd64",
				Private:    true,
				Spot:       true,
			},
			want: "runs-fleet=12345/runner=4cpu-linux-amd64/private=true",
		},
		{
			name: "with spot=false",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "2cpu-linux-arm64",
				Spot:       false,
			},
			want: "runs-fleet=12345/runner=2cpu-linux-arm64/spot=false",
		},
		{
			name: "all modifiers",
			job: &queue.JobMessage{
				RunID:      "67890",
				RunnerSpec: "8cpu-linux-arm64",
				Pool:       "mypool",
				Private:    true,
				Spot:       false,
			},
			want: "runs-fleet=67890/runner=8cpu-linux-arm64/pool=mypool/private=true/spot=false",
		},
		{
			name: "pool and private",
			job: &queue.JobMessage{
				RunID:      "11111",
				RunnerSpec: "2cpu-linux",
				Pool:       "default",
				Private:    true,
				Spot:       true,
			},
			want: "runs-fleet=11111/runner=2cpu-linux/pool=default/private=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRunnerLabel(tt.job)
			if got != tt.want {
				t.Errorf("buildRunnerLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelectSubnet(t *testing.T) {
	tests := []struct {
		name     string
		job      *queue.JobMessage
		cfg      *config.Config
		wantFunc func(string, []string, []string) bool
	}{
		{
			name: "private job uses private subnet",
			job:  &queue.JobMessage{Private: true},
			cfg: &config.Config{
				PrivateSubnetIDs: []string{"subnet-priv-1", "subnet-priv-2"},
				PublicSubnetIDs:  []string{"subnet-pub-1"},
			},
			wantFunc: func(result string, privateSubnets, _ []string) bool {
				for _, s := range privateSubnets {
					if s == result {
						return true
					}
				}
				return false
			},
		},
		{
			name: "public job uses public subnet",
			job:  &queue.JobMessage{Private: false},
			cfg: &config.Config{
				PrivateSubnetIDs: []string{"subnet-priv-1"},
				PublicSubnetIDs:  []string{"subnet-pub-1", "subnet-pub-2"},
			},
			wantFunc: func(result string, _, publicSubnets []string) bool {
				for _, s := range publicSubnets {
					if s == result {
						return true
					}
				}
				return false
			},
		},
		{
			name: "empty private subnets falls back to public",
			job:  &queue.JobMessage{Private: true},
			cfg: &config.Config{
				PrivateSubnetIDs: []string{},
				PublicSubnetIDs:  []string{"subnet-pub-1"},
			},
			wantFunc: func(result string, _, publicSubnets []string) bool {
				for _, s := range publicSubnets {
					if s == result {
						return true
					}
				}
				return false
			},
		},
		{
			name: "no subnets returns empty",
			job:  &queue.JobMessage{Private: false},
			cfg: &config.Config{
				PrivateSubnetIDs: []string{},
				PublicSubnetIDs:  []string{},
			},
			wantFunc: func(result string, _, _ []string) bool {
				return result == ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var subnetIndex uint64
			got := selectSubnet(tt.job, tt.cfg, &subnetIndex)
			if !tt.wantFunc(got, tt.cfg.PrivateSubnetIDs, tt.cfg.PublicSubnetIDs) {
				t.Errorf("selectSubnet() = %q, unexpected result", got)
			}
		})
	}
}

func TestSelectSubnet_RoundRobin(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"subnet-a", "subnet-b", "subnet-c"},
	}
	job := &queue.JobMessage{Private: false}

	var subnetIndex uint64
	results := make([]string, 6)

	for i := 0; i < 6; i++ {
		results[i] = selectSubnet(job, cfg, &subnetIndex)
	}

	// Should cycle through subnets
	expected := []string{"subnet-a", "subnet-b", "subnet-c", "subnet-a", "subnet-b", "subnet-c"}
	for i, exp := range expected {
		if results[i] != exp {
			t.Errorf("selectSubnet() iteration %d = %q, want %q", i, results[i], exp)
		}
	}
}

func TestSelectSubnet_ConcurrentAccess(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"subnet-1", "subnet-2", "subnet-3", "subnet-4"},
	}
	job := &queue.JobMessage{Private: false}

	var subnetIndex uint64
	done := make(chan string, 100)

	for i := 0; i < 100; i++ {
		go func() {
			result := selectSubnet(job, cfg, &subnetIndex)
			done <- result
		}()
	}

	results := make(map[string]int)
	for i := 0; i < 100; i++ {
		subnet := <-done
		results[subnet]++
	}

	// All results should be valid subnets
	for subnet := range results {
		found := false
		for _, valid := range cfg.PublicSubnetIDs {
			if subnet == valid {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected subnet: %s", subnet)
		}
	}

	// Verify final index is 100
	if subnetIndex != 100 {
		t.Errorf("final subnetIndex = %d, want 100", subnetIndex)
	}
}

// mockQueue implements queue.Queue for testing
type mockQueue struct {
	deleteFunc func(ctx context.Context, handle string) error
}

func (m *mockQueue) SendMessage(_ context.Context, _ *queue.JobMessage) error {
	return nil
}

func (m *mockQueue) ReceiveMessages(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
	return nil, nil
}

func (m *mockQueue) DeleteMessage(ctx context.Context, handle string) error {
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, handle)
	}
	return nil
}

func TestDeleteMessageWithRetry_Success(t *testing.T) {
	callCount := 0
	q := &mockQueue{
		deleteFunc: func(_ context.Context, _ string) error {
			callCount++
			return nil
		},
	}

	err := deleteMessageWithRetry(context.Background(), q, "test-handle")
	if err != nil {
		t.Errorf("deleteMessageWithRetry() error = %v, want nil", err)
	}
	if callCount != 1 {
		t.Errorf("deleteMessageWithRetry() called %d times, want 1", callCount)
	}
}

func TestDeleteMessageWithRetry_EventualSuccess(t *testing.T) {
	callCount := 0
	q := &mockQueue{
		deleteFunc: func(_ context.Context, _ string) error {
			callCount++
			if callCount < 2 {
				return errors.New("temporary error")
			}
			return nil
		},
	}

	err := deleteMessageWithRetry(context.Background(), q, "test-handle")
	if err != nil {
		t.Errorf("deleteMessageWithRetry() error = %v, want nil", err)
	}
	if callCount != 2 {
		t.Errorf("deleteMessageWithRetry() called %d times, want 2", callCount)
	}
}

func TestDeleteMessageWithRetry_AllFail(t *testing.T) {
	callCount := 0
	testErr := errors.New("persistent error")
	q := &mockQueue{
		deleteFunc: func(_ context.Context, _ string) error {
			callCount++
			return testErr
		},
	}

	err := deleteMessageWithRetry(context.Background(), q, "test-handle")
	if err == nil {
		t.Error("deleteMessageWithRetry() error = nil, want error")
	}
	if callCount != maxDeleteRetries {
		t.Errorf("deleteMessageWithRetry() called %d times, want %d", callCount, maxDeleteRetries)
	}
}

func TestStdLogger(_ *testing.T) {
	logger := &stdLogger{}

	// Should not panic
	logger.Printf("test format %s", "arg")
	logger.Println("test message")
}

func TestHousekeepingMetricsAdapter(t *testing.T) {
	// Test that the adapter struct can be created with nil publisher
	adapter := &housekeepingMetricsAdapter{publisher: nil}

	// Verify the adapter has the expected nil publisher
	if adapter.publisher != nil {
		t.Error("publisher should be nil in this test")
	}
}

func TestConstants(t *testing.T) {
	if maxDeleteRetries <= 0 {
		t.Errorf("maxDeleteRetries = %d, should be positive", maxDeleteRetries)
	}
	if retryDelay <= 0 {
		t.Errorf("retryDelay = %v, should be positive", retryDelay)
	}
	if maxFleetCreateRetries <= 0 {
		t.Errorf("maxFleetCreateRetries = %d, should be positive", maxFleetCreateRetries)
	}
	if fleetRetryBaseDelay <= 0 {
		t.Errorf("fleetRetryBaseDelay = %v, should be positive", fleetRetryBaseDelay)
	}
}

func TestBuildRunnerLabel_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		job  *queue.JobMessage
		want string
	}{
		{
			name: "empty run ID",
			job: &queue.JobMessage{
				RunID:      "",
				RunnerSpec: "spec",
				Spot:       true,
			},
			want: "runs-fleet=/runner=spec",
		},
		{
			name: "empty runner spec",
			job: &queue.JobMessage{
				RunID:      "123",
				RunnerSpec: "",
				Spot:       true,
			},
			want: "runs-fleet=123/runner=",
		},
		{
			name: "special characters in pool",
			job: &queue.JobMessage{
				RunID:      "123",
				RunnerSpec: "spec",
				Pool:       "pool-name_v2.1",
				Spot:       true,
			},
			want: "runs-fleet=123/runner=spec/pool=pool-name_v2.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRunnerLabel(tt.job)
			if got != tt.want {
				t.Errorf("buildRunnerLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelectSubnet_AtomicIndex(t *testing.T) {
	cfg := &config.Config{
		PublicSubnetIDs: []string{"subnet-1"},
	}
	job := &queue.JobMessage{Private: false}

	var subnetIndex uint64

	// Pre-set the index to a high value
	atomic.StoreUint64(&subnetIndex, 1000)

	result := selectSubnet(job, cfg, &subnetIndex)
	if result != "subnet-1" {
		t.Errorf("selectSubnet() = %q, want %q", result, "subnet-1")
	}

	// Index should have incremented
	newIndex := atomic.LoadUint64(&subnetIndex)
	if newIndex != 1001 {
		t.Errorf("subnetIndex = %d, want 1001", newIndex)
	}
}

func TestDeleteMessageWithRetry_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	callCount := 0
	q := &mockQueue{
		deleteFunc: func(_ context.Context, _ string) error {
			callCount++
			return errors.New("error")
		},
	}

	// Even with cancelled context, it will still retry
	_ = deleteMessageWithRetry(ctx, q, "test-handle")

	// Should still attempt retries
	if callCount == 0 {
		t.Error("deleteMessageWithRetry() should have been called at least once")
	}
}

func TestRetryDelay(t *testing.T) {
	if retryDelay != 1*time.Second {
		t.Errorf("retryDelay = %v, want 1s", retryDelay)
	}
}

func TestFleetRetryBaseDelay(t *testing.T) {
	if fleetRetryBaseDelay != 2*time.Second {
		t.Errorf("fleetRetryBaseDelay = %v, want 2s", fleetRetryBaseDelay)
	}
}
