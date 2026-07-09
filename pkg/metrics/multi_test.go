package metrics

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// trackingPublisher tracks method calls for testing.
type trackingPublisher struct {
	NoopPublisher
	calls       atomic.Int32
	shouldError bool
}

func (t *trackingPublisher) PublishQueueDepth(_ context.Context, _ string, _ float64) error {
	t.calls.Add(1)
	if t.shouldError {
		return errors.New("tracking error")
	}
	return nil
}

func (t *trackingPublisher) PublishJobCompleted(_ context.Context, _, _, _ string) error {
	t.calls.Add(1)
	if t.shouldError {
		return errors.New("tracking error")
	}
	return nil
}

func (t *trackingPublisher) PublishFleetCreate(_ context.Context, _, _ string) error {
	t.calls.Add(1)
	if t.shouldError {
		return errors.New("tracking error")
	}
	return nil
}

func (t *trackingPublisher) Close() error {
	t.calls.Add(1)
	if t.shouldError {
		return errors.New("close error")
	}
	return nil
}

func TestNewMultiPublisher(t *testing.T) {
	t.Parallel()

	pub1 := &trackingPublisher{}
	pub2 := &trackingPublisher{}

	multi := NewMultiPublisher(pub1, pub2)
	if multi == nil {
		t.Fatal("NewMultiPublisher() returned nil")
	}

	pubs := multi.Publishers()
	if len(pubs) != 2 {
		t.Errorf("Publishers() = %d, want 2", len(pubs))
	}
}

func TestMultiPublisher_Add(t *testing.T) {
	t.Parallel()

	multi := NewMultiPublisher()
	if len(multi.Publishers()) != 0 {
		t.Errorf("Publishers() = %d, want 0", len(multi.Publishers()))
	}

	pub := &trackingPublisher{}
	multi.Add(pub)

	if len(multi.Publishers()) != 1 {
		t.Errorf("Publishers() after Add = %d, want 1", len(multi.Publishers()))
	}
}

func TestMultiPublisher_Publishers(t *testing.T) {
	t.Parallel()

	pub1 := &trackingPublisher{}
	pub2 := &trackingPublisher{}
	multi := NewMultiPublisher(pub1, pub2)

	pubs := multi.Publishers()
	if len(pubs) != 2 {
		t.Errorf("Publishers() = %d, want 2", len(pubs))
	}
}

func TestMultiPublisher_Close(t *testing.T) {
	t.Parallel()

	pub1 := &trackingPublisher{}
	pub2 := &trackingPublisher{}
	multi := NewMultiPublisher(pub1, pub2)

	err := multi.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}

	if pub1.calls.Load() != 1 {
		t.Errorf("pub1.Close() calls = %d, want 1", pub1.calls.Load())
	}
	if pub2.calls.Load() != 1 {
		t.Errorf("pub2.Close() calls = %d, want 1", pub2.calls.Load())
	}
}

func TestMultiPublisher_CloseWithErrors(t *testing.T) {
	t.Parallel()

	pub1 := &trackingPublisher{shouldError: true}
	pub2 := &trackingPublisher{shouldError: true}
	multi := NewMultiPublisher(pub1, pub2)

	err := multi.Close()
	if err == nil {
		t.Error("Close() should return error when children fail")
	}
}

//nolint:dupl // Test tables are intentionally similar - testing different publishers
func TestMultiPublisher_PublishMethods(t *testing.T) {
	t.Parallel()

	pub1 := &trackingPublisher{}
	pub2 := &trackingPublisher{}
	multi := NewMultiPublisher(pub1, pub2)
	ctx := context.Background()

	tests := []struct {
		name    string
		publish func() error
	}{
		{"PublishJobEnqueued", func() error { return multi.PublishJobEnqueued(ctx, "default", "arm64", "4", "o/r") }},
		{"PublishJobAssigned", func() error { return multi.PublishJobAssigned(ctx, "default", "warm_pool", "o/r") }},
		{"PublishRunnerConfirmed", func() error { return multi.PublishRunnerConfirmed(ctx, "default") }},
		{"PublishJobCompleted", func() error { return multi.PublishJobCompleted(ctx, "default", "success", "o/r") }},
		{"PublishJobRequeued", func() error { return multi.PublishJobRequeued(ctx, "spot_interruption") }},
		{"PublishJobDeduplicated", func() error { return multi.PublishJobDeduplicated(ctx, "queue") }},
		{"PublishJobWaitSeconds", func() error { return multi.PublishJobWaitSeconds(ctx, "default", "cold_start", 12) }},
		{"PublishJobStartupSeconds", func() error { return multi.PublishJobStartupSeconds(ctx, "default", "cold_start", 38) }},
		{"PublishAgentBootstrapSeconds", func() error { return multi.PublishAgentBootstrapSeconds(ctx, "default", "total", 20) }},
		{"PublishJobExecutionSeconds", func() error { return multi.PublishJobExecutionSeconds(ctx, "default", "success", 90) }},
		{"PublishInstanceProvisionSeconds", func() error { return multi.PublishInstanceProvisionSeconds(ctx, "cold_start", "c7g", 30) }},
		{"PublishFleetCreate", func() error { return multi.PublishFleetCreate(ctx, "1", "success") }},
		{"PublishFleetCreateSeconds", func() error { return multi.PublishFleetCreateSeconds(ctx, "1", 5) }},
		{"PublishInstances", func() error { return multi.PublishInstances(ctx, "running", "4", "default", 3) }},
		{"PublishSpotInterruption", func() error { return multi.PublishSpotInterruption(ctx, "c7g") }},
		{"PublishCircuitBreakerTrip", func() error { return multi.PublishCircuitBreakerTrip(ctx, "c7g.xlarge") }},
		{"PublishCircuitBreakerOpen", func() error { return multi.PublishCircuitBreakerOpen(ctx, "c7g.xlarge", true) }},
		{"PublishPoolInstances", func() error { return multi.PublishPoolInstances(ctx, "default", "ready", 5) }},
		{"PublishPoolDesired", func() error { return multi.PublishPoolDesired(ctx, "default", "running", 2) }},
		{"PublishPoolAction", func() error { return multi.PublishPoolAction(ctx, "default", "create", "ready_deficit") }},
		{"PublishPoolReconcileSeconds", func() error { return multi.PublishPoolReconcileSeconds(ctx, 2) }},
		{"PublishMessageProcessingSeconds", func() error { return multi.PublishMessageProcessingSeconds(ctx, "main", "success", 0.2) }},
		{"PublishLockWaitSeconds", func() error { return multi.PublishLockWaitSeconds(ctx, "pool_reconcile", 0.05) }},
		{"PublishWorkerInflight", func() error { return multi.PublishWorkerInflight(ctx, "main", 4) }},
		{"PublishQueueDepth", func() error { return multi.PublishQueueDepth(ctx, "main", 10.0) }},
		{"PublishQueueReceive", func() error { return multi.PublishQueueReceive(ctx, "main", "messages") }},
		{"PublishAWSCallDuration", func() error { return multi.PublishAWSCallDuration(ctx, "SQS", "ReceiveMessage", 1.5) }},
		{"PublishAWSCallFailure", func() error { return multi.PublishAWSCallFailure(ctx, "SQS", "ReceiveMessage", "timeout") }},
		{"PublishCacheRequest", func() error { return multi.PublishCacheRequest(ctx, "hit") }},
		{"PublishCacheOperation", func() error { return multi.PublishCacheOperation(ctx, "commit") }},
		{"PublishCacheBytesStored", func() error { return multi.PublishCacheBytesStored(ctx, 1024) }},
		{"PublishCacheError", func() error { return multi.PublishCacheError(ctx, "commit") }},
		{"PublishCacheAuthRejected", func() error { return multi.PublishCacheAuthRejected(ctx, "invalid") }},
		{"PublishHousekeepingAction", func() error { return multi.PublishHousekeepingAction(ctx, "ssm_params", 3) }},
		{"PublishSchedulingFailure", func() error { return multi.PublishSchedulingFailure(ctx, "job_claim") }},
		{"PublishMessageDeletionFailure", func() error { return multi.PublishMessageDeletionFailure(ctx, "events") }},
		{"PublishInstanceHours", func() error { return multi.PublishInstanceHours(ctx, "4", "c7g", 2) }},
		{"PublishEstimatedCost", func() error { return multi.PublishEstimatedCost(ctx, 12.5) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.publish()
			if err != nil {
				t.Errorf("%s() error = %v", tt.name, err)
			}
		})
	}
}

func TestMultiPublisher_PublishWithErrors(t *testing.T) {
	t.Parallel()

	pub1 := &trackingPublisher{shouldError: true}
	pub2 := &trackingPublisher{}
	multi := NewMultiPublisher(pub1, pub2)

	// Should return error when any publisher fails
	err := multi.PublishQueueDepth(context.Background(), "main", 10.0)
	if err == nil {
		t.Error("PublishQueueDepth() should return error when a publisher fails")
	}

	// Fan-out must continue to all publishers even when one fails
	if pub2.calls.Load() != 1 {
		t.Errorf("pub2 should still be called when pub1 fails, got %d calls", pub2.calls.Load())
	}
}

func TestMultiPublisher_FanOutsToAll(t *testing.T) {
	t.Parallel()

	pub1 := &trackingPublisher{}
	pub2 := &trackingPublisher{}
	multi := NewMultiPublisher(pub1, pub2)

	_ = multi.PublishFleetCreate(context.Background(), "1", "success")

	if pub1.calls.Load() != 1 {
		t.Errorf("pub1 calls = %d, want 1", pub1.calls.Load())
	}
	if pub2.calls.Load() != 1 {
		t.Errorf("pub2 calls = %d, want 1", pub2.calls.Load())
	}
}

func TestMultiPublisher_ImplementsInterface(_ *testing.T) {
	var _ Publisher = (*MultiPublisher)(nil)
}
