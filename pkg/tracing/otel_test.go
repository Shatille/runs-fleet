package tracing

import (
	"context"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

func TestLoadConfig_Disabled(t *testing.T) {
	// Save and clear environment
	original := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	_ = os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	defer func() { _ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", original) }()

	cfg := LoadConfig()

	if cfg.Enabled {
		t.Error("LoadConfig() should return Enabled=false when endpoint is not set")
	}
}

func TestLoadConfig_Enabled(t *testing.T) {
	// Save original values
	originalEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	originalRatio := os.Getenv("OTEL_TRACE_SAMPLING_RATIO")
	defer func() {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEndpoint)
		_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", originalRatio)
	}()

	_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	_ = os.Unsetenv("OTEL_TRACE_SAMPLING_RATIO")

	cfg := LoadConfig()

	if !cfg.Enabled {
		t.Error("LoadConfig() should return Enabled=true when endpoint is set")
	}
	if cfg.Endpoint != "localhost:4317" {
		t.Errorf("expected Endpoint 'localhost:4317', got '%s'", cfg.Endpoint)
	}
	if cfg.SamplingRatio != 1.0 {
		t.Errorf("expected default SamplingRatio 1.0, got %f", cfg.SamplingRatio)
	}
}

func TestLoadConfig_WithSamplingRatio(t *testing.T) {
	originalEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	originalRatio := os.Getenv("OTEL_TRACE_SAMPLING_RATIO")
	defer func() {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEndpoint)
		_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", originalRatio)
	}()

	_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", "0.5")

	cfg := LoadConfig()

	if cfg.SamplingRatio != 0.5 {
		t.Errorf("expected SamplingRatio 0.5, got %f", cfg.SamplingRatio)
	}
}

func TestLoadConfig_InvalidSamplingRatio(t *testing.T) {
	originalEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	originalRatio := os.Getenv("OTEL_TRACE_SAMPLING_RATIO")
	defer func() {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEndpoint)
		_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", originalRatio)
	}()

	_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", "invalid")

	cfg := LoadConfig()

	// Should use default value
	if cfg.SamplingRatio != 1.0 {
		t.Errorf("expected default SamplingRatio 1.0 for invalid input, got %f", cfg.SamplingRatio)
	}
}

func TestLoadConfig_OutOfRangeSamplingRatio(t *testing.T) {
	originalEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	originalRatio := os.Getenv("OTEL_TRACE_SAMPLING_RATIO")
	defer func() {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEndpoint)
		_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", originalRatio)
	}()

	tests := []struct {
		name     string
		ratio    string
		expected float64
	}{
		{"negative", "-0.5", 1.0},
		{"greater than 1", "1.5", 1.0},
		{"zero", "0", 0.0},
		{"one", "1", 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
			_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", tt.ratio)

			cfg := LoadConfig()

			if cfg.SamplingRatio != tt.expected {
				t.Errorf("expected SamplingRatio %f for %s, got %f", tt.expected, tt.ratio, cfg.SamplingRatio)
			}
		})
	}
}

func TestInit_Disabled(t *testing.T) {
	cfg := &Config{Enabled: false}

	provider, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if provider.IsEnabled() {
		t.Error("Init() should return disabled provider when config.Enabled is false")
	}
}

func TestInit_NilConfig(t *testing.T) {
	provider, err := Init(context.Background(), nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if provider.IsEnabled() {
		t.Error("Init() should return disabled provider when config is nil")
	}
}

func TestProvider_Shutdown_NilProvider(t *testing.T) {
	provider := &Provider{provider: nil, enabled: false}

	err := provider.Shutdown(context.Background())
	if err != nil {
		t.Errorf("Shutdown() should not return error for nil provider, got: %v", err)
	}
}

func TestProvider_IsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		expected bool
	}{
		{"enabled", true, true},
		{"disabled", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &Provider{enabled: tt.enabled}

			if provider.IsEnabled() != tt.expected {
				t.Errorf("IsEnabled() = %v, want %v", provider.IsEnabled(), tt.expected)
			}
		})
	}
}

func TestTracer(t *testing.T) {
	tracer := Tracer("test-tracer")

	if tracer == nil {
		t.Error("Tracer() returned nil")
	}
}

func TestStartSpan(t *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")

	if ctx == nil {
		t.Error("StartSpan() returned nil context")
	}
	if span == nil {
		t.Error("StartSpan() returned nil span")
	}

	span.End()
}

func TestSpanFromContext(t *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")
	defer span.End()

	retrievedSpan := SpanFromContext(ctx)

	if retrievedSpan == nil {
		t.Error("SpanFromContext() returned nil")
	}
}

func TestAddEvent(_ *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")
	defer span.End()

	// Should not panic
	AddEvent(ctx, "test-event", attribute.String("key", "value"))
}

func TestSetAttributes(_ *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")
	defer span.End()

	// Should not panic
	SetAttributes(ctx, attribute.String("key", "value"), attribute.Int("count", 42))
}

func TestRecordError(_ *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")
	defer span.End()

	// Should not panic with nil error
	RecordError(ctx, nil)

	// Should not panic with actual error
	RecordError(ctx, context.DeadlineExceeded)
}

func TestInjectTraceContext(t *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")
	defer span.End()

	carrier := InjectTraceContext(ctx)

	if carrier == nil {
		t.Error("InjectTraceContext() returned nil")
	}
}

func TestExtractTraceContext(t *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")
	defer span.End()

	carrier := InjectTraceContext(ctx)
	newCtx := ExtractTraceContext(context.Background(), carrier)

	if newCtx == nil {
		t.Error("ExtractTraceContext() returned nil")
	}
}

func TestExtractTraceContext_EmptyCarrier(t *testing.T) {
	carrier := make(map[string]string)
	ctx := ExtractTraceContext(context.Background(), carrier)

	if ctx == nil {
		t.Error("ExtractTraceContext() returned nil for empty carrier")
	}
}

func TestNewHTTPMiddleware(t *testing.T) {
	middleware := NewHTTPMiddleware()

	if middleware == nil {
		t.Fatal("NewHTTPMiddleware() returned nil")
	}
	if middleware.tracer == nil {
		t.Error("NewHTTPMiddleware() did not set tracer")
	}
}

func TestNewJobTracer(t *testing.T) {
	tracer := NewJobTracer()

	if tracer == nil {
		t.Fatal("NewJobTracer() returned nil")
	}
	if tracer.tracer == nil {
		t.Error("NewJobTracer() did not set tracer")
	}
}

func TestJobTracer_StartJobSpan(t *testing.T) {
	tracer := NewJobTracer()

	ctx, span := tracer.StartJobSpan(context.Background(), "job-123", "run-456")

	if ctx == nil {
		t.Error("StartJobSpan() returned nil context")
	}
	if span == nil {
		t.Error("StartJobSpan() returned nil span")
	}

	span.End()
}

func TestJobTracer_StartFleetSpan(t *testing.T) {
	tracer := NewJobTracer()

	ctx, span := tracer.StartFleetSpan(context.Background(), "m5.large", "default")

	if ctx == nil {
		t.Error("StartFleetSpan() returned nil context")
	}
	if span == nil {
		t.Error("StartFleetSpan() returned nil span")
	}

	span.End()
}

func TestWithTimeout(t *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")
	defer span.End()

	timeoutCtx, cancel := WithTimeout(ctx, 5*time.Second, "test-operation")
	defer cancel()

	if timeoutCtx == nil {
		t.Error("WithTimeout() returned nil context")
	}

	// Verify context has deadline
	deadline, ok := timeoutCtx.Deadline()
	if !ok {
		t.Error("WithTimeout() did not set deadline")
	}
	if deadline.Before(time.Now()) {
		t.Error("WithTimeout() deadline is in the past")
	}
}

func TestConfig_Structure(t *testing.T) {
	cfg := Config{
		Enabled:       true,
		Endpoint:      "localhost:4317",
		SamplingRatio: 0.5,
	}

	if !cfg.Enabled {
		t.Error("Enabled should be true")
	}
	if cfg.Endpoint != "localhost:4317" {
		t.Errorf("expected Endpoint 'localhost:4317', got '%s'", cfg.Endpoint)
	}
	if cfg.SamplingRatio != 0.5 {
		t.Errorf("expected SamplingRatio 0.5, got %f", cfg.SamplingRatio)
	}
}

func TestConstants(t *testing.T) {
	if serviceName != "runs-fleet" {
		t.Errorf("expected serviceName 'runs-fleet', got '%s'", serviceName)
	}
	if serviceVersion != "1.0.0" {
		t.Errorf("expected serviceVersion '1.0.0', got '%s'", serviceVersion)
	}
}

func TestProvider_ShutdownWithProvider(t *testing.T) {
	// Create a disabled provider first
	cfg := &Config{Enabled: false}
	provider, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Shutdown should succeed even for disabled provider
	err = provider.Shutdown(context.Background())
	if err != nil {
		t.Errorf("Shutdown() should not return error for disabled provider, got: %v", err)
	}
}

func TestStartSpan_NoopWhenDisabled(t *testing.T) {
	// When tracing is disabled, spans should still work (no-op)
	ctx, span := StartSpan(context.Background(), "test-span-noop")
	if ctx == nil {
		t.Error("StartSpan() should not return nil context even when disabled")
	}
	if span == nil {
		t.Error("StartSpan() should not return nil span even when disabled")
	}
	span.End()
}

func TestAddEvent_NoRecordingSpan(_ *testing.T) {
	// Background context has no span, should not panic
	AddEvent(context.Background(), "test-event-no-span", attribute.String("key", "value"))
}

func TestSetAttributes_NoRecordingSpan(_ *testing.T) {
	// Background context has no span, should not panic
	SetAttributes(context.Background(), attribute.String("key", "value"))
}

func TestRecordError_NoRecordingSpan(_ *testing.T) {
	// Background context has no span, should not panic
	RecordError(context.Background(), context.DeadlineExceeded)
	RecordError(context.Background(), nil)
}

func TestInjectTraceContext_NoSpan(t *testing.T) {
	// Should work even with background context
	carrier := InjectTraceContext(context.Background())
	if carrier == nil {
		t.Error("InjectTraceContext() should not return nil for background context")
	}
}

func TestExtractTraceContext_NilCarrier(t *testing.T) {
	// nil carrier should not panic
	ctx := ExtractTraceContext(context.Background(), nil)
	if ctx == nil {
		t.Error("ExtractTraceContext() should not return nil for nil carrier")
	}
}

func TestWithTimeout_BackgroundContext(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 1*time.Second, "test-background")
	defer cancel()

	if ctx == nil {
		t.Error("WithTimeout() should not return nil context")
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Error("WithTimeout() should set deadline")
	}
	if deadline.Before(time.Now()) {
		t.Error("WithTimeout() deadline should be in the future")
	}
}

func TestJobTracer_MultipleSpans(t *testing.T) {
	tracer := NewJobTracer()

	// Start multiple spans
	ctx1, span1 := tracer.StartJobSpan(context.Background(), "job-1", "run-1")
	ctx2, span2 := tracer.StartFleetSpan(ctx1, "m5.large", "pool-1")

	if ctx1 == nil || ctx2 == nil {
		t.Error("contexts should not be nil")
	}
	if span1 == nil || span2 == nil {
		t.Error("spans should not be nil")
	}

	span2.End()
	span1.End()
}

func TestHTTPMiddleware_TracerInitialized(t *testing.T) {
	middleware := NewHTTPMiddleware()

	if middleware.tracer == nil {
		t.Error("NewHTTPMiddleware() should initialize tracer")
	}
}

func TestJobTracer_TracerInitialized(t *testing.T) {
	tracer := NewJobTracer()

	if tracer.tracer == nil {
		t.Error("NewJobTracer() should initialize tracer")
	}
}

func TestTracer_MultipleCalls(t *testing.T) {
	// Multiple calls should return valid tracers
	tracer1 := Tracer("package1")
	tracer2 := Tracer("package2")
	tracer3 := Tracer("package1") // Same name as tracer1

	if tracer1 == nil || tracer2 == nil || tracer3 == nil {
		t.Error("Tracer() should never return nil")
	}
}

func TestLoadConfig_BoundaryRatios(t *testing.T) {
	originalEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	originalRatio := os.Getenv("OTEL_TRACE_SAMPLING_RATIO")
	defer func() {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEndpoint)
		_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", originalRatio)
	}()

	tests := []struct {
		name          string
		ratio         string
		expectedRatio float64
	}{
		{"exact zero", "0.0", 0.0},
		{"exact one", "1.0", 1.0},
		{"small positive", "0.001", 0.001},
		{"near one", "0.999", 0.999},
		{"half", "0.5", 0.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
			_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", tt.ratio)

			cfg := LoadConfig()

			if cfg.SamplingRatio != tt.expectedRatio {
				t.Errorf("expected SamplingRatio %f, got %f", tt.expectedRatio, cfg.SamplingRatio)
			}
		})
	}
}

func TestSpanFromContext_BackgroundContext(t *testing.T) {
	// Background context should return a non-nil (no-op) span
	span := SpanFromContext(context.Background())
	if span == nil {
		t.Error("SpanFromContext() should not return nil for background context")
	}
}

func TestProvider_Structure(t *testing.T) {
	// Test that provider fields are properly initialized
	p := &Provider{
		provider: nil,
		enabled:  true,
	}

	if !p.IsEnabled() {
		t.Error("Provider.IsEnabled() should return true when enabled=true")
	}

	p.enabled = false
	if p.IsEnabled() {
		t.Error("Provider.IsEnabled() should return false when enabled=false")
	}
}
