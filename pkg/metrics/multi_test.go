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

func (t *trackingPublisher) PublishQueueDepth(_ context.Context, _ float64) error {
	t.calls.Add(1)
	if t.shouldError {
		return errors.New("tracking error")
	}
	return nil
}

func (t *trackingPublisher) PublishFleetSizeIncrement(_ context.Context) error {
	t.calls.Add(1)
	if t.shouldError {
		return errors.New("tracking error")
	}
	return nil
}

func (t *trackingPublisher) PublishJobSuccess(_ context.Context) error {
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
	pub1 := &trackingPublisher{}
	pub2 := &trackingPublisher{}
	multi := NewMultiPublisher(pub1, pub2)

	pubs := multi.Publishers()
	if len(pubs) != 2 {
		t.Errorf("Publishers() = %d, want 2", len(pubs))
	}
}

func TestMultiPublisher_Close(t *testing.T) {
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
	pub1 := &trackingPublisher{}
	pub2 := &trackingPublisher{}
	multi := NewMultiPublisher(pub1, pub2)
	ctx := context.Background()

	tests := []struct {
		name    string
		publish func() error
	}{
		{"PublishQueueDepth", func() error { return multi.PublishQueueDepth(ctx, 10.0) }},
		{"PublishFleetSizeIncrement", func() error { return multi.PublishFleetSizeIncrement(ctx) }},
		{"PublishFleetSizeDecrement", func() error { return multi.PublishFleetSizeDecrement(ctx) }},
		{"PublishJobDuration", func() error { return multi.PublishJobDuration(ctx, 120) }},
		{"PublishJobSuccess", func() error { return multi.PublishJobSuccess(ctx) }},
		{"PublishJobFailure", func() error { return multi.PublishJobFailure(ctx) }},
		{"PublishJobQueued", func() error { return multi.PublishJobQueued(ctx) }},
		{"PublishSpotInterruption", func() error { return multi.PublishSpotInterruption(ctx) }},
		{"PublishMessageDeletionFailure", func() error { return multi.PublishMessageDeletionFailure(ctx) }},
		{"PublishCacheHit", func() error { return multi.PublishCacheHit(ctx) }},
		{"PublishCacheMiss", func() error { return multi.PublishCacheMiss(ctx) }},
		{"PublishOrphanedInstancesTerminated", func() error { return multi.PublishOrphanedInstancesTerminated(ctx, 5) }},
		{"PublishSSMParametersDeleted", func() error { return multi.PublishSSMParametersDeleted(ctx, 3) }},
		{"PublishJobRecordsArchived", func() error { return multi.PublishJobRecordsArchived(ctx, 10) }},
		{"PublishPoolUtilization", func() error { return multi.PublishPoolUtilization(ctx, "default", 75.5) }},
		{"PublishPoolRunningJobs", func() error { return multi.PublishPoolRunningJobs(ctx, "default", 5) }},
		{"PublishSchedulingFailure", func() error { return multi.PublishSchedulingFailure(ctx, "runner-provision") }},
		{"PublishCircuitBreakerTriggered", func() error { return multi.PublishCircuitBreakerTriggered(ctx, "t4g.medium") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.publish()
			if err != nil {
				t.Errorf("%s() error = %v", tt.name, err)
			}
		})
	}
}

func TestMultiPublisher_PublishWithErrors(t *testing.T) {
	pub1 := &trackingPublisher{shouldError: true}
	pub2 := &trackingPublisher{}
	multi := NewMultiPublisher(pub1, pub2)

	// Should return error when any publisher fails
	err := multi.PublishQueueDepth(context.Background(), 10.0)
	if err == nil {
		t.Error("PublishQueueDepth() should return error when a publisher fails")
	}

	// Fan-out must continue to all publishers even when one fails
	if pub2.calls.Load() != 1 {
		t.Errorf("pub2 should still be called when pub1 fails, got %d calls", pub2.calls.Load())
	}
}

func TestMultiPublisher_FanOutsToAll(t *testing.T) {
	pub1 := &trackingPublisher{}
	pub2 := &trackingPublisher{}
	multi := NewMultiPublisher(pub1, pub2)

	_ = multi.PublishFleetSizeIncrement(context.Background())

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
