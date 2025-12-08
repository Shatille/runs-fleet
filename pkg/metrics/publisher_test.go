package metrics

import (
	"context"
	"testing"
)

func TestNoopPublisher_ImplementsInterface(_ *testing.T) {
	var _ Publisher = NoopPublisher{}
}

func TestNoopPublisher_AllMethodsReturnNil(t *testing.T) {
	pub := NoopPublisher{}
	ctx := context.Background()

	tests := []struct {
		name    string
		publish func() error
	}{
		{"Close", pub.Close},
		{"PublishQueueDepth", func() error { return pub.PublishQueueDepth(ctx, 10.0) }},
		{"PublishFleetSizeIncrement", func() error { return pub.PublishFleetSizeIncrement(ctx) }},
		{"PublishFleetSizeDecrement", func() error { return pub.PublishFleetSizeDecrement(ctx) }},
		{"PublishJobDuration", func() error { return pub.PublishJobDuration(ctx, 120) }},
		{"PublishJobSuccess", func() error { return pub.PublishJobSuccess(ctx) }},
		{"PublishJobFailure", func() error { return pub.PublishJobFailure(ctx) }},
		{"PublishJobQueued", func() error { return pub.PublishJobQueued(ctx) }},
		{"PublishSpotInterruption", func() error { return pub.PublishSpotInterruption(ctx) }},
		{"PublishMessageDeletionFailure", func() error { return pub.PublishMessageDeletionFailure(ctx) }},
		{"PublishCacheHit", func() error { return pub.PublishCacheHit(ctx) }},
		{"PublishCacheMiss", func() error { return pub.PublishCacheMiss(ctx) }},
		{"PublishOrphanedInstancesTerminated", func() error { return pub.PublishOrphanedInstancesTerminated(ctx, 5) }},
		{"PublishSSMParametersDeleted", func() error { return pub.PublishSSMParametersDeleted(ctx, 3) }},
		{"PublishJobRecordsArchived", func() error { return pub.PublishJobRecordsArchived(ctx, 10) }},
		{"PublishPoolUtilization", func() error { return pub.PublishPoolUtilization(ctx, "default", 75.5) }},
		{"PublishSchedulingFailure", func() error { return pub.PublishSchedulingFailure(ctx, "runner-provision") }},
		{"PublishCircuitBreakerTriggered", func() error { return pub.PublishCircuitBreakerTriggered(ctx, "t4g.medium") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.publish()
			if err != nil {
				t.Errorf("%s() error = %v, want nil", tt.name, err)
			}
		})
	}
}
