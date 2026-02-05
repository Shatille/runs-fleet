package metrics

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewPrometheusPublisher(t *testing.T) {
	tests := []struct {
		name      string
		cfg       PrometheusConfig
		wantNS    string
	}{
		{
			name:   "default namespace",
			cfg:    PrometheusConfig{},
			wantNS: defaultPrometheusNamespace,
		},
		{
			name:   "custom namespace",
			cfg:    PrometheusConfig{Namespace: "custom"},
			wantNS: "custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub := NewPrometheusPublisher(tt.cfg)
			if pub == nil {
				t.Fatal("NewPrometheusPublisher() returned nil")
			}
			if pub.registry == nil {
				t.Error("NewPrometheusPublisher() registry is nil")
			}
		})
	}
}

func TestPrometheusPublisher_Handler(t *testing.T) {
	pub := NewPrometheusPublisher(PrometheusConfig{})

	handler := pub.Handler()
	if handler == nil {
		t.Fatal("Handler() returned nil")
	}

	// Test that the handler responds with metrics
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("Handler status = %d, want 200", w.Code)
	}
}

func TestPrometheusPublisher_Registry(t *testing.T) {
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
	pub := NewPrometheusPublisher(PrometheusConfig{})

	err := pub.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

//nolint:dupl // Test tables are intentionally similar - testing different publishers
func TestPrometheusPublisher_PublishMethods(t *testing.T) {
	pub := NewPrometheusPublisher(PrometheusConfig{Namespace: "test"})
	ctx := context.Background()

	tests := []struct {
		name    string
		publish func() error
	}{
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
		{"PublishPoolRunningJobs", func() error { return pub.PublishPoolRunningJobs(ctx, "default", 5) }},
		{"PublishSchedulingFailure", func() error { return pub.PublishSchedulingFailure(ctx, "runner-provision") }},
		{"PublishCircuitBreakerTriggered", func() error { return pub.PublishCircuitBreakerTriggered(ctx, "t4g.medium") }},
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

func TestPrometheusPublisher_MetricsExposed(t *testing.T) {
	pub := NewPrometheusPublisher(PrometheusConfig{Namespace: "test"})
	ctx := context.Background()

	// Publish some metrics
	_ = pub.PublishQueueDepth(ctx, 5.0)
	_ = pub.PublishJobSuccess(ctx)
	_ = pub.PublishPoolUtilization(ctx, "mypool", 80.0)

	// Check that metrics are exposed via handler
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	pub.Handler().ServeHTTP(w, req)

	body := w.Body.String()

	expectedMetrics := []string{
		"test_queue_depth 5",
		"test_job_success_total 1",
		"test_pool_utilization_percent{pool_name=\"mypool\"} 80",
	}

	for _, metric := range expectedMetrics {
		if !strings.Contains(body, metric) {
			t.Errorf("metrics output missing %q", metric)
		}
	}
}

func TestPrometheusPublisher_ImplementsInterface(_ *testing.T) {
	var _ Publisher = (*PrometheusPublisher)(nil)
}
