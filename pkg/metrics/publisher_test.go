package metrics

import (
	"context"
	"testing"
)

func TestNoopPublisher_ImplementsInterface(_ *testing.T) {
	var _ Publisher = NoopPublisher{}
}

func TestNoopPublisher_AllMethodsReturnNil(t *testing.T) {
	t.Parallel()

	pub := NoopPublisher{}
	ctx := context.Background()

	tests := []struct {
		name    string
		publish func() error
	}{
		{"Close", pub.Close},
		{"PublishJobEnqueued", func() error { return pub.PublishJobEnqueued(ctx, "default", "arm64", "4", "o/r") }},
		{"PublishJobAssigned", func() error { return pub.PublishJobAssigned(ctx, "default", "warm_pool", "o/r") }},
		{"PublishRunnerConfirmed", func() error { return pub.PublishRunnerConfirmed(ctx, "default") }},
		{"PublishJobCompleted", func() error { return pub.PublishJobCompleted(ctx, "default", "success", "o/r") }},
		{"PublishJobRequeued", func() error { return pub.PublishJobRequeued(ctx, "spot_interruption") }},
		{"PublishJobDeduplicated", func() error { return pub.PublishJobDeduplicated(ctx, "queue") }},
		{"PublishJobWaitSeconds", func() error { return pub.PublishJobWaitSeconds(ctx, "default", "cold_start", 12) }},
		{"PublishJobExecutionSeconds", func() error { return pub.PublishJobExecutionSeconds(ctx, "default", "success", 90) }},
		{"PublishInstanceProvisionSeconds", func() error { return pub.PublishInstanceProvisionSeconds(ctx, "cold_start", "c7g", 30) }},
		{"PublishFleetCreate", func() error { return pub.PublishFleetCreate(ctx, "1", "success") }},
		{"PublishFleetCreateSeconds", func() error { return pub.PublishFleetCreateSeconds(ctx, "1", 5) }},
		{"PublishInstances", func() error { return pub.PublishInstances(ctx, "running", "4", "default", 3) }},
		{"PublishSpotInterruption", func() error { return pub.PublishSpotInterruption(ctx, "c7g") }},
		{"PublishCircuitBreakerTrip", func() error { return pub.PublishCircuitBreakerTrip(ctx, "c7g.xlarge") }},
		{"PublishCircuitBreakerOpen", func() error { return pub.PublishCircuitBreakerOpen(ctx, "c7g.xlarge", true) }},
		{"PublishPoolInstances", func() error { return pub.PublishPoolInstances(ctx, "default", "ready", 5) }},
		{"PublishPoolDesired", func() error { return pub.PublishPoolDesired(ctx, "default", "running", 2) }},
		{"PublishPoolAction", func() error { return pub.PublishPoolAction(ctx, "default", "create", "ready_deficit") }},
		{"PublishPoolReconcileSeconds", func() error { return pub.PublishPoolReconcileSeconds(ctx, 2) }},
		{"PublishMessageProcessingSeconds", func() error { return pub.PublishMessageProcessingSeconds(ctx, "main", "success", 0.2) }},
		{"PublishLockWaitSeconds", func() error { return pub.PublishLockWaitSeconds(ctx, "pool_reconcile", 0.05) }},
		{"PublishWorkerInflight", func() error { return pub.PublishWorkerInflight(ctx, "main", 4) }},
		{"PublishQueueDepth", func() error { return pub.PublishQueueDepth(ctx, "main", 10.0) }},
		{"PublishQueueReceive", func() error { return pub.PublishQueueReceive(ctx, "main", "messages") }},
		{"PublishAWSCallDuration", func() error { return pub.PublishAWSCallDuration(ctx, "SQS", "ReceiveMessage", 1.5) }},
		{"PublishAWSCallFailure", func() error { return pub.PublishAWSCallFailure(ctx, "SQS", "ReceiveMessage", "timeout") }},
		{"PublishCacheRequest", func() error { return pub.PublishCacheRequest(ctx, "hit") }},
		{"PublishCacheOperation", func() error { return pub.PublishCacheOperation(ctx, "commit") }},
		{"PublishCacheBytesStored", func() error { return pub.PublishCacheBytesStored(ctx, 1024) }},
		{"PublishCacheError", func() error { return pub.PublishCacheError(ctx, "commit") }},
		{"PublishCacheAuthRejected", func() error { return pub.PublishCacheAuthRejected(ctx, "invalid") }},
		{"PublishHousekeepingAction", func() error { return pub.PublishHousekeepingAction(ctx, "ssm_params", 3) }},
		{"PublishSchedulingFailure", func() error { return pub.PublishSchedulingFailure(ctx, "job_claim") }},
		{"PublishMessageDeletionFailure", func() error { return pub.PublishMessageDeletionFailure(ctx, "events") }},
		{"PublishServiceCheck", func() error { return pub.PublishServiceCheck(ctx, "health", ServiceCheckOK, "ok") }},
		{"PublishEvent", func() error { return pub.PublishEvent(ctx, "t", "x", "info", nil) }},
		{"PublishInstanceHours", func() error { return pub.PublishInstanceHours(ctx, "4", "c7g", 2) }},
		{"PublishEstimatedCost", func() error { return pub.PublishEstimatedCost(ctx, 12.5) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.publish()
			if err != nil {
				t.Errorf("%s() error = %v, want nil", tt.name, err)
			}
		})
	}
}
