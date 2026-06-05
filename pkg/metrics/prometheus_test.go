package metrics

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewPrometheusPublisher(t *testing.T) {
	t.Parallel()

	pub := NewPrometheusPublisher(PrometheusConfig{})
	if pub == nil {
		t.Fatal("NewPrometheusPublisher() returned nil")
	}
	if pub.registry == nil {
		t.Error("NewPrometheusPublisher() registry is nil")
	}
}

// TestPrometheusPublisher_FixedNamespace asserts the metric name prefix is fixed
// at runs_fleet and is not influenced by PrometheusConfig (the override knob is
// gone), guarding against metric-name collisions across deployments.
func TestPrometheusPublisher_FixedNamespace(t *testing.T) {
	t.Parallel()

	pub := NewPrometheusPublisher(PrometheusConfig{})
	ctx := context.Background()

	_ = pub.PublishJobEnqueued(ctx, "default", "arm64", "4", "octo/repo")

	body := scrape(t, pub)
	if !strings.Contains(body, "runs_fleet_jobs_enqueued_total") {
		t.Errorf("metrics output missing runs_fleet_ prefixed name; got:\n%s", body)
	}
}

func TestPrometheusPublisher_Handler(t *testing.T) {
	t.Parallel()

	pub := NewPrometheusPublisher(PrometheusConfig{})
	handler := pub.Handler()
	if handler == nil {
		t.Fatal("Handler() returned nil")
	}

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("Handler status = %d, want 200", w.Code)
	}
}

func TestPrometheusPublisher_Registry(t *testing.T) {
	t.Parallel()

	pub := NewPrometheusPublisher(PrometheusConfig{})
	registry := pub.Registry()
	if registry == nil {
		t.Error("Registry() returned nil")
	}
	if registry != pub.registry {
		t.Error("Registry() returned different registry")
	}
}

func TestPrometheusPublisher_Close(t *testing.T) {
	t.Parallel()

	pub := NewPrometheusPublisher(PrometheusConfig{})
	if err := pub.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

//nolint:dupl // Test tables are intentionally similar - testing different publishers
func TestPrometheusPublisher_PublishMethods(t *testing.T) {
	t.Parallel()

	pub := NewPrometheusPublisher(PrometheusConfig{})
	ctx := context.Background()

	tests := []struct {
		name    string
		publish func() error
	}{
		{"PublishJobEnqueued", func() error { return pub.PublishJobEnqueued(ctx, "default", "arm64", "4", "o/r") }},
		{"PublishJobAssigned", func() error { return pub.PublishJobAssigned(ctx, "default", "warm_pool", "o/r") }},
		{"PublishJobCompleted", func() error { return pub.PublishJobCompleted(ctx, "default", "success", "o/r") }},
		{"PublishJobRequeued", func() error { return pub.PublishJobRequeued(ctx, "spot_interruption") }},
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
		{"PublishInstanceHours", func() error { return pub.PublishInstanceHours(ctx, "4", "c7g", 1.5) }},
		{"PublishEstimatedCost", func() error { return pub.PublishEstimatedCost(ctx, 12.5) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.publish(); err != nil {
				t.Errorf("%s() error = %v", tt.name, err)
			}
		})
	}
}

func TestPrometheusPublisher_MetricsExposed(t *testing.T) {
	t.Parallel()

	pub := NewPrometheusPublisher(PrometheusConfig{})
	ctx := context.Background()

	_ = pub.PublishJobEnqueued(ctx, "default", "arm64", "4", "octo/repo")
	_ = pub.PublishJobCompleted(ctx, "default", "success", "octo/repo")
	_ = pub.PublishQueueDepth(ctx, "main", 5.0)
	_ = pub.PublishPoolInstances(ctx, "mypool", "ready", 2)
	_ = pub.PublishFleetCreate(ctx, "1", "success")
	_ = pub.PublishAWSCallDuration(ctx, "SQS", "ReceiveMessage", 1.5)
	_ = pub.PublishAWSCallFailure(ctx, "DynamoDB", "GetItem", "timeout")
	_ = pub.PublishCacheOperation(ctx, "commit")
	_ = pub.PublishCacheBytesStored(ctx, 2048)
	_ = pub.PublishCacheError(ctx, "commit")
	_ = pub.PublishCacheAuthRejected(ctx, "invalid")

	body := scrape(t, pub)

	expectedMetrics := []string{
		`runs_fleet_jobs_enqueued_total{arch="arm64",capacity="4",pool="default",repo="octo/repo"} 1`,
		`runs_fleet_jobs_completed_total{pool="default",repo="octo/repo",result="success"} 1`,
		`runs_fleet_queue_depth{queue="main"} 5`,
		`runs_fleet_pool_instances{pool="mypool",state="ready"} 2`,
		`runs_fleet_fleet_create_total{capacity="1",result="success"} 1`,
		`runs_fleet_aws_call_duration_seconds_count{operation="ReceiveMessage",service="SQS"} 1`,
		`runs_fleet_aws_call_failures_total{operation="GetItem",result="timeout",service="DynamoDB"} 1`,
		`runs_fleet_cache_operations_total{operation="commit"} 1`,
		`runs_fleet_cache_bytes_stored_total 2048`,
		`runs_fleet_cache_errors_total{operation="commit"} 1`,
		`runs_fleet_cache_auth_rejected_total{reason="invalid"} 1`,
	}

	for _, metric := range expectedMetrics {
		if !strings.Contains(body, metric) {
			t.Errorf("metrics output missing %q", metric)
		}
	}
}

// TestPrometheusPublisher_RepoCardinalityRule asserts the repo label appears only
// on the three job-lifecycle counters and on no histogram or other metric.
func TestPrometheusPublisher_RepoCardinalityRule(t *testing.T) {
	t.Parallel()

	pub := NewPrometheusPublisher(PrometheusConfig{})
	ctx := context.Background()

	_ = pub.PublishJobEnqueued(ctx, "default", "arm64", "4", "octo/repo")
	_ = pub.PublishJobAssigned(ctx, "default", "warm_pool", "octo/repo")
	_ = pub.PublishJobCompleted(ctx, "default", "success", "octo/repo")
	_ = pub.PublishJobWaitSeconds(ctx, "default", "cold_start", 12)
	_ = pub.PublishJobExecutionSeconds(ctx, "default", "success", 90)
	_ = pub.PublishInstanceProvisionSeconds(ctx, "cold_start", "c7g", 30)
	_ = pub.PublishInstances(ctx, "running", "4", "default", 1)

	body := scrape(t, pub)

	// repo must appear on exactly the three lifecycle counters.
	for _, name := range []string{"jobs_enqueued_total", "jobs_assigned_total", "jobs_completed_total"} {
		if !labelOnMetric(body, "runs_fleet_"+name, "repo") {
			t.Errorf("expected repo label on %s", name)
		}
	}

	// repo must NOT appear on any histogram or any other metric.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if !strings.Contains(line, `repo=`) {
			continue
		}
		if strings.HasPrefix(line, "runs_fleet_jobs_enqueued_total") ||
			strings.HasPrefix(line, "runs_fleet_jobs_assigned_total") ||
			strings.HasPrefix(line, "runs_fleet_jobs_completed_total") {
			continue
		}
		t.Errorf("unexpected repo label outside lifecycle counters: %q", line)
	}
}

func scrape(t *testing.T, pub *PrometheusPublisher) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	pub.Handler().ServeHTTP(w, req)
	return w.Body.String()
}

func labelOnMetric(body, metricPrefix, label string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, metricPrefix) && strings.Contains(line, label+"=") {
			return true
		}
	}
	return false
}

func TestPrometheusPublisher_ImplementsInterface(_ *testing.T) {
	var _ Publisher = (*PrometheusPublisher)(nil)
}
