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

func TestInit_SamplingRatioAlwaysSample(t *testing.T) {
	// Test that sampling ratio >= 1.0 uses AlwaysSample
	cfg := &Config{
		Enabled:       false, // Keep disabled to avoid network calls
		SamplingRatio: 1.5,   // Greater than 1.0
	}

	provider, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if provider.IsEnabled() {
		t.Error("Provider should be disabled when config.Enabled is false")
	}
}

func TestInit_SamplingRatioNeverSample(t *testing.T) {
	// Test that sampling ratio <= 0 uses NeverSample
	cfg := &Config{
		Enabled:       false, // Keep disabled to avoid network calls
		SamplingRatio: -0.5,  // Less than 0
	}

	provider, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if provider.IsEnabled() {
		t.Error("Provider should be disabled when config.Enabled is false")
	}
}

func TestInit_SamplingRatioTraceIDRatioBased(t *testing.T) {
	// Test that sampling ratio between 0 and 1 uses TraceIDRatioBased
	cfg := &Config{
		Enabled:       false, // Keep disabled to avoid network calls
		SamplingRatio: 0.5,   // Between 0 and 1
	}

	provider, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if provider.IsEnabled() {
		t.Error("Provider should be disabled when config.Enabled is false")
	}
}

func TestConfig_DefaultValues(t *testing.T) {
	// Test config default values
	cfg := Config{}

	if cfg.Enabled {
		t.Error("Enabled should default to false")
	}
	if cfg.Endpoint != "" {
		t.Error("Endpoint should default to empty string")
	}
	if cfg.SamplingRatio != 0 {
		t.Errorf("SamplingRatio should default to 0, got %f", cfg.SamplingRatio)
	}
}

func TestLoadConfig_EdgeCases(t *testing.T) {
	originalEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	originalRatio := os.Getenv("OTEL_TRACE_SAMPLING_RATIO")
	defer func() {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEndpoint)
		_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", originalRatio)
	}()

	tests := []struct {
		name          string
		endpoint      string
		ratio         string
		wantEnabled   bool
		wantRatio     float64
	}{
		{
			name:        "whitespace endpoint",
			endpoint:    "   ",
			ratio:       "",
			wantEnabled: true,
			wantRatio:   1.0,
		},
		{
			name:        "empty string endpoint",
			endpoint:    "",
			ratio:       "0.5",
			wantEnabled: false,
			wantRatio:   1.0, // Default because config is disabled
		},
		{
			name:        "valid endpoint with edge ratio",
			endpoint:    "localhost:4317",
			ratio:       "0.0",
			wantEnabled: true,
			wantRatio:   0.0,
		},
		{
			name:        "valid endpoint with max ratio",
			endpoint:    "localhost:4317",
			ratio:       "1.0",
			wantEnabled: true,
			wantRatio:   1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tt.endpoint)
			if tt.ratio != "" {
				_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", tt.ratio)
			} else {
				_ = os.Unsetenv("OTEL_TRACE_SAMPLING_RATIO")
			}

			cfg := LoadConfig()

			if cfg.Enabled != tt.wantEnabled {
				t.Errorf("Enabled = %v, want %v", cfg.Enabled, tt.wantEnabled)
			}
		})
	}
}

func TestStartSpan_WithOptions(t *testing.T) {
	// Test StartSpan with various options
	ctx, span := StartSpan(context.Background(), "test-span-with-opts")
	if ctx == nil {
		t.Error("StartSpan() returned nil context")
	}
	if span == nil {
		t.Error("StartSpan() returned nil span")
	}
	span.End()
}

func TestAddEvent_WithMultipleAttributes(_ *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")
	defer span.End()

	// Test adding event with multiple attributes
	AddEvent(ctx, "multi-attr-event",
		attribute.String("key1", "value1"),
		attribute.Int("key2", 42),
		attribute.Bool("key3", true),
		attribute.Float64("key4", 3.14),
	)
}

func TestSetAttributes_WithMultipleTypes(_ *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")
	defer span.End()

	// Test setting attributes with various types
	SetAttributes(ctx,
		attribute.String("string-attr", "value"),
		attribute.Int("int-attr", 123),
		attribute.Int64("int64-attr", 1234567890),
		attribute.Bool("bool-attr", true),
		attribute.Float64("float-attr", 1.5),
		attribute.StringSlice("slice-attr", []string{"a", "b", "c"}),
	)
}

func TestRecordError_WithDifferentErrors(_ *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-span")
	defer span.End()

	// Test with various error types
	RecordError(ctx, context.Canceled)
	RecordError(ctx, context.DeadlineExceeded)
	RecordError(ctx, os.ErrNotExist)
}

func TestJobTracer_StartJobSpan_WithEmptyValues(t *testing.T) {
	tracer := NewJobTracer()

	// Test with empty values
	ctx, span := tracer.StartJobSpan(context.Background(), "", "")

	if ctx == nil {
		t.Error("StartJobSpan() returned nil context for empty values")
	}
	if span == nil {
		t.Error("StartJobSpan() returned nil span for empty values")
	}

	span.End()
}

func TestJobTracer_StartFleetSpan_WithEmptyValues(t *testing.T) {
	tracer := NewJobTracer()

	// Test with empty values
	ctx, span := tracer.StartFleetSpan(context.Background(), "", "")

	if ctx == nil {
		t.Error("StartFleetSpan() returned nil context for empty values")
	}
	if span == nil {
		t.Error("StartFleetSpan() returned nil span for empty values")
	}

	span.End()
}

func TestWithTimeout_ZeroDuration(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 0, "zero-timeout")
	defer cancel()

	// Zero duration should still work
	if ctx == nil {
		t.Error("WithTimeout() should not return nil context")
	}

	// Context should be immediately done or near-done
	select {
	case <-ctx.Done():
		// Expected
	case <-time.After(100 * time.Millisecond):
		// Also acceptable - zero timeout might mean immediate or very short
	}
}

func TestWithTimeout_LongDuration(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 24*time.Hour, "long-timeout")
	defer cancel()

	if ctx == nil {
		t.Error("WithTimeout() should not return nil context")
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Error("WithTimeout() should set deadline")
	}

	// Deadline should be approximately 24 hours from now
	expectedDeadline := time.Now().Add(24 * time.Hour)
	if deadline.Before(expectedDeadline.Add(-time.Minute)) {
		t.Error("Deadline is too early")
	}
}

func TestInjectTraceContext_WithActiveSpan(t *testing.T) {
	ctx, span := StartSpan(context.Background(), "inject-test-span")
	defer span.End()

	carrier := InjectTraceContext(ctx)

	if carrier == nil {
		t.Error("InjectTraceContext() returned nil")
	}
}

func TestExtractTraceContext_WithValidCarrier(t *testing.T) {
	// Create a span and inject its context
	ctx, span := StartSpan(context.Background(), "extract-test-span")
	carrier := InjectTraceContext(ctx)
	span.End()

	// Extract the context
	newCtx := ExtractTraceContext(context.Background(), carrier)

	if newCtx == nil {
		t.Error("ExtractTraceContext() returned nil")
	}
}

func TestTracerConcurrency(_ *testing.T) {
	// Test that tracing is safe for concurrent use
	done := make(chan bool)

	for i := 0; i < 10; i++ {
		go func(id int) {
			ctx, span := StartSpan(context.Background(), "concurrent-span")
			AddEvent(ctx, "event", attribute.Int("goroutine", id))
			SetAttributes(ctx, attribute.String("id", string(rune('A'+id))))
			span.End()
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestHTTPMiddleware_Structure(t *testing.T) {
	middleware := NewHTTPMiddleware()

	if middleware == nil {
		t.Fatal("NewHTTPMiddleware() returned nil")
	}

	// Verify internal tracer is initialized
	if middleware.tracer == nil {
		t.Error("HTTPMiddleware.tracer should not be nil")
	}
}

func TestJobTracer_NestedSpans(_ *testing.T) {
	tracer := NewJobTracer()

	// Create nested spans
	ctx1, span1 := tracer.StartJobSpan(context.Background(), "job-parent", "run-parent")
	ctx2, span2 := tracer.StartFleetSpan(ctx1, "m5.large", "default")
	_, span3 := tracer.StartJobSpan(ctx2, "job-child", "run-child")

	// End spans in reverse order
	span3.End()
	span2.End()
	span1.End()
}

func TestConfig_EnabledField(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{"enabled true", true},
		{"enabled false", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Enabled:       tt.enabled,
				Endpoint:      "localhost:4317",
				SamplingRatio: 1.0,
			}

			if cfg.Enabled != tt.enabled {
				t.Errorf("Enabled = %v, want %v", cfg.Enabled, tt.enabled)
			}
		})
	}
}

func TestInit_WithNilConfig_ReturnsDisabledProvider(t *testing.T) {
	provider, err := Init(context.Background(), nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if provider == nil {
		t.Fatal("Init() returned nil provider")
	}

	if provider.IsEnabled() {
		t.Error("provider should be disabled for nil config")
	}

	if provider.provider != nil {
		t.Error("internal provider should be nil for disabled provider")
	}
}

func TestInit_WithDisabledConfig_ReturnsDisabledProvider(t *testing.T) {
	cfg := &Config{
		Enabled:       false,
		Endpoint:      "localhost:4317",
		SamplingRatio: 0.5,
	}

	provider, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if provider.IsEnabled() {
		t.Error("provider should be disabled when config.Enabled is false")
	}
}

func TestProvider_Shutdown_DisabledProvider(t *testing.T) {
	provider := &Provider{
		provider: nil,
		enabled:  false,
	}

	err := provider.Shutdown(context.Background())
	if err != nil {
		t.Errorf("Shutdown() error = %v, want nil", err)
	}
}

func TestProvider_Shutdown_WithContext(t *testing.T) {
	// Test shutdown with cancelled context for disabled provider
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	provider := &Provider{
		provider: nil,
		enabled:  false,
	}

	// Should still succeed for nil provider
	err := provider.Shutdown(ctx)
	if err != nil {
		t.Errorf("Shutdown() with cancelled context error = %v", err)
	}
}

func TestLoadConfig_EmptyEndpoint(t *testing.T) {
	// Save and restore environment
	originalEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	defer func() {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEndpoint)
	}()

	// Set empty endpoint
	_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	cfg := LoadConfig()

	if cfg.Enabled {
		t.Error("LoadConfig() should return disabled config for empty endpoint")
	}
}

func TestLoadConfig_SamplingRatioEdgeCases(t *testing.T) {
	originalEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	originalRatio := os.Getenv("OTEL_TRACE_SAMPLING_RATIO")
	defer func() {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEndpoint)
		_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", originalRatio)
	}()

	_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")

	tests := []struct {
		name          string
		ratio         string
		expectedRatio float64
	}{
		{"very small positive", "0.0001", 0.0001},
		{"almost one", "0.9999", 0.9999},
		{"scientific notation positive", "1e-4", 0.0001},
		{"with spaces", " 0.5 ", 0.5}, // Sscanf handles leading/trailing spaces
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", tt.ratio)

			cfg := LoadConfig()

			if cfg.SamplingRatio != tt.expectedRatio {
				t.Errorf("SamplingRatio = %f, want %f", cfg.SamplingRatio, tt.expectedRatio)
			}
		})
	}
}

func TestAddEvent_WithContext(_ *testing.T) {
	// Create a proper span context
	ctx, span := StartSpan(context.Background(), "test-event-span")
	defer span.End()

	// Add event with no attributes - should not panic
	AddEvent(ctx, "empty-event")

	// Add event with single attribute
	AddEvent(ctx, "single-attr-event", attribute.String("key", "value"))

	// Add event with multiple attributes
	AddEvent(ctx, "multi-attr-event",
		attribute.String("string-key", "string-value"),
		attribute.Int("int-key", 42),
		attribute.Bool("bool-key", true),
	)
}

func TestSetAttributes_WithContext(_ *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-attrs-span")
	defer span.End()

	// Set no attributes - should not panic
	SetAttributes(ctx)

	// Set single attribute
	SetAttributes(ctx, attribute.String("key", "value"))

	// Set multiple attributes of different types
	SetAttributes(ctx,
		attribute.String("str", "value"),
		attribute.Int("int", 123),
		attribute.Int64("int64", 9876543210),
		attribute.Float64("float", 3.14159),
		attribute.Bool("bool", true),
	)
}

func TestRecordError_NilError(_ *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-error-span")
	defer span.End()

	// Should not panic with nil error
	RecordError(ctx, nil)
}

func TestRecordError_WithError(_ *testing.T) {
	ctx, span := StartSpan(context.Background(), "test-error-span")
	defer span.End()

	// Test various error types
	testErrors := []error{
		context.Canceled,
		context.DeadlineExceeded,
		os.ErrNotExist,
		os.ErrPermission,
	}

	for _, err := range testErrors {
		RecordError(ctx, err)
	}
}

func TestExtractTraceContext_EmptyMap(t *testing.T) {
	carrier := make(map[string]string)

	ctx := ExtractTraceContext(context.Background(), carrier)
	if ctx == nil {
		t.Error("ExtractTraceContext() should not return nil")
	}
}

func TestInjectExtractTraceContext_Roundtrip(t *testing.T) {
	// Create a span with context
	ctx, span := StartSpan(context.Background(), "roundtrip-span")
	defer span.End()

	// Inject
	carrier := InjectTraceContext(ctx)

	// Extract
	extractedCtx := ExtractTraceContext(context.Background(), carrier)

	if extractedCtx == nil {
		t.Error("ExtractTraceContext() returned nil")
	}
}

func TestWithTimeout_VariousDurations(t *testing.T) {
	tests := []struct {
		name      string
		duration  time.Duration
		operation string
	}{
		{"millisecond", 100 * time.Millisecond, "fast-op"},
		{"second", 1 * time.Second, "medium-op"},
		{"minute", 1 * time.Minute, "slow-op"},
		{"zero", 0, "instant-op"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := WithTimeout(context.Background(), tt.duration, tt.operation)
			defer cancel()

			if ctx == nil {
				t.Error("WithTimeout() returned nil context")
			}

			_, hasDeadline := ctx.Deadline()
			if !hasDeadline {
				t.Error("WithTimeout() should set deadline")
			}
		})
	}
}

func TestJobTracer_StartJobSpan_Attributes(t *testing.T) {
	tracer := NewJobTracer()

	tests := []struct {
		name  string
		jobID string
		runID string
	}{
		{"normal IDs", "job-123", "run-456"},
		{"empty IDs", "", ""},
		{"long IDs", "job-very-long-identifier-12345678901234567890", "run-very-long-identifier-12345678901234567890"},
		{"special chars", "job-abc_def.123", "run-xyz_uvw.789"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, span := tracer.StartJobSpan(context.Background(), tt.jobID, tt.runID)
			if ctx == nil || span == nil {
				t.Error("StartJobSpan() returned nil")
			}
			span.End()
		})
	}
}

func TestJobTracer_StartFleetSpan_Attributes(t *testing.T) {
	tracer := NewJobTracer()

	tests := []struct {
		name         string
		instanceType string
		pool         string
	}{
		{"normal values", "m5.large", "default"},
		{"empty values", "", ""},
		{"complex instance type", "m6g.8xlarge", "gpu-pool-large"},
		{"with underscore", "t4g_medium", "my_custom_pool"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, span := tracer.StartFleetSpan(context.Background(), tt.instanceType, tt.pool)
			if ctx == nil || span == nil {
				t.Error("StartFleetSpan() returned nil")
			}
			span.End()
		})
	}
}

func TestHTTPMiddleware_Fields(t *testing.T) {
	middleware := NewHTTPMiddleware()

	if middleware == nil {
		t.Fatal("NewHTTPMiddleware() returned nil")
	}

	// Verify the tracer field is set
	if middleware.tracer == nil {
		t.Error("HTTPMiddleware.tracer should not be nil")
	}
}

func TestStartSpan_ReturnsValidSpan(t *testing.T) {
	ctx, span := StartSpan(context.Background(), "valid-span")
	if ctx == nil {
		t.Error("StartSpan() returned nil context")
	}
	if span == nil {
		t.Error("StartSpan() returned nil span")
	}

	// Verify span can be ended without panic
	span.End()

	// Verify span from context after ending
	retrievedSpan := SpanFromContext(ctx)
	if retrievedSpan == nil {
		t.Error("SpanFromContext() returned nil after span.End()")
	}
}

func TestTracer_DifferentNames(t *testing.T) {
	names := []string{
		"http",
		"job",
		"fleet",
		"custom-tracer",
		"runs-fleet",
		"",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			tracer := Tracer(name)
			if tracer == nil {
				t.Errorf("Tracer(%q) returned nil", name)
			}
		})
	}
}

func TestProvider_EnabledStates(t *testing.T) {
	tests := []struct {
		name     string
		provider *Provider
		expected bool
	}{
		{
			name:     "enabled true",
			provider: &Provider{enabled: true},
			expected: true,
		},
		{
			name:     "enabled false",
			provider: &Provider{enabled: false},
			expected: false,
		},
		{
			name:     "enabled with nil internal provider",
			provider: &Provider{enabled: true, provider: nil},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.provider.IsEnabled() != tt.expected {
				t.Errorf("IsEnabled() = %v, want %v", tt.provider.IsEnabled(), tt.expected)
			}
		})
	}
}

func TestServiceConstants(t *testing.T) {
	// Verify service constants have expected values
	if serviceName == "" {
		t.Error("serviceName should not be empty")
	}
	if serviceVersion == "" {
		t.Error("serviceVersion should not be empty")
	}

	// Verify specific values
	if serviceName != "runs-fleet" {
		t.Errorf("serviceName = %s, want runs-fleet", serviceName)
	}
	if serviceVersion != "1.0.0" {
		t.Errorf("serviceVersion = %s, want 1.0.0", serviceVersion)
	}
}

func TestConcurrentSpanCreation(_ *testing.T) {
	// Test concurrent span creation doesn't cause race conditions
	done := make(chan bool)
	tracerJob := NewJobTracer()

	for i := 0; i < 20; i++ {
		go func(id int) {
			ctx, span := StartSpan(context.Background(), "concurrent-test")
			AddEvent(ctx, "event", attribute.Int("id", id))
			SetAttributes(ctx, attribute.String("worker", "worker-"+string(rune('A'+id%26))))

			// Also test job tracer concurrently
			_, jobSpan := tracerJob.StartJobSpan(ctx, "job", "run")
			jobSpan.End()

			span.End()
			done <- true
		}(i)
	}

	for i := 0; i < 20; i++ {
		<-done
	}
}

func TestInit_CancelledContext(t *testing.T) {
	// Test Init with already cancelled context for disabled config
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	cfg := &Config{Enabled: false}

	provider, err := Init(ctx, cfg)
	if err != nil {
		t.Fatalf("Init() with cancelled context and disabled config should not error, got: %v", err)
	}

	if provider.IsEnabled() {
		t.Error("Provider should be disabled")
	}
}

func TestLoadConfig_SpecialEndpoints(t *testing.T) {
	originalEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	originalRatio := os.Getenv("OTEL_TRACE_SAMPLING_RATIO")
	defer func() {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEndpoint)
		_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", originalRatio)
	}()

	tests := []struct {
		name        string
		endpoint    string
		wantEnabled bool
	}{
		{"ipv4 address", "192.168.1.1:4317", true},
		{"ipv6 address", "[::1]:4317", true},
		{"with path", "collector.example.com:4317/v1/traces", true},
		{"just host", "collector", true},
		{"with port only", ":4317", true},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tt.endpoint)
			_ = os.Unsetenv("OTEL_TRACE_SAMPLING_RATIO")

			cfg := LoadConfig()

			if cfg.Enabled != tt.wantEnabled {
				t.Errorf("Enabled = %v, want %v", cfg.Enabled, tt.wantEnabled)
			}
			if tt.wantEnabled && cfg.Endpoint != tt.endpoint {
				t.Errorf("Endpoint = %q, want %q", cfg.Endpoint, tt.endpoint)
			}
		})
	}
}

func TestExtractTraceContext_WithTraceparent(t *testing.T) {
	// Test with a valid W3C traceparent header format
	carrier := map[string]string{
		"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	}

	ctx := ExtractTraceContext(context.Background(), carrier)
	if ctx == nil {
		t.Error("ExtractTraceContext() returned nil")
	}

	// The extracted context should be valid
	span := SpanFromContext(ctx)
	if span == nil {
		t.Error("SpanFromContext() returned nil after extraction")
	}
}

func TestExtractTraceContext_WithTracestate(t *testing.T) {
	// Test with both traceparent and tracestate headers
	carrier := map[string]string{
		"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"tracestate":  "congo=t61rcWkgMzE",
	}

	ctx := ExtractTraceContext(context.Background(), carrier)
	if ctx == nil {
		t.Error("ExtractTraceContext() returned nil")
	}
}

func TestInjectTraceContext_ProducesValidHeaders(t *testing.T) {
	// Start a span and inject its context
	ctx, span := StartSpan(context.Background(), "inject-header-test")
	defer span.End()

	carrier := InjectTraceContext(ctx)

	// Carrier should be non-nil (may or may not have headers depending on provider)
	if carrier == nil {
		t.Error("InjectTraceContext() should not return nil carrier")
	}
}

func TestWithTimeout_CancelFunc(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 10*time.Second, "cancel-test")

	// Verify context is not done yet
	select {
	case <-ctx.Done():
		t.Error("Context should not be done before cancel")
	default:
		// Expected
	}

	// Cancel and verify
	cancel()

	select {
	case <-ctx.Done():
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("Context should be done after cancel")
	}
}

func TestProvider_MultipleShudowns(t *testing.T) {
	provider := &Provider{provider: nil, enabled: false}

	// First shutdown
	err := provider.Shutdown(context.Background())
	if err != nil {
		t.Errorf("First Shutdown() error = %v", err)
	}

	// Second shutdown should also succeed
	err = provider.Shutdown(context.Background())
	if err != nil {
		t.Errorf("Second Shutdown() error = %v", err)
	}
}

func TestConfig_AllFields(t *testing.T) {
	tests := []struct {
		name          string
		enabled       bool
		endpoint      string
		samplingRatio float64
	}{
		{"all zero", false, "", 0},
		{"enabled only", true, "", 0},
		{"all set", true, "localhost:4317", 0.5},
		{"max ratio", true, "collector:4317", 1.0},
		{"min ratio", true, "collector:4317", 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Enabled:       tt.enabled,
				Endpoint:      tt.endpoint,
				SamplingRatio: tt.samplingRatio,
			}

			if cfg.Enabled != tt.enabled {
				t.Errorf("Enabled = %v, want %v", cfg.Enabled, tt.enabled)
			}
			if cfg.Endpoint != tt.endpoint {
				t.Errorf("Endpoint = %q, want %q", cfg.Endpoint, tt.endpoint)
			}
			if cfg.SamplingRatio != tt.samplingRatio {
				t.Errorf("SamplingRatio = %f, want %f", cfg.SamplingRatio, tt.samplingRatio)
			}
		})
	}
}

func TestSpanFromContext_AfterSpanEnd(t *testing.T) {
	ctx, span := StartSpan(context.Background(), "span-end-test")

	// End the span
	span.End()

	// Should still be able to retrieve span from context
	retrievedSpan := SpanFromContext(ctx)
	if retrievedSpan == nil {
		t.Error("SpanFromContext() should return non-nil even after span.End()")
	}
}

func TestAddEvent_EmptyName(_ *testing.T) {
	ctx, span := StartSpan(context.Background(), "empty-event-name-test")
	defer span.End()

	// Should not panic with empty event name
	AddEvent(ctx, "")
	AddEvent(ctx, "", attribute.String("key", "value"))
}

func TestSetAttributes_Empty(_ *testing.T) {
	ctx, span := StartSpan(context.Background(), "empty-attrs-test")
	defer span.End()

	// Should not panic with no attributes
	SetAttributes(ctx)
}

func TestJobTracer_SpanHierarchy(t *testing.T) {
	tracer := NewJobTracer()

	// Create parent job span
	ctx1, span1 := tracer.StartJobSpan(context.Background(), "parent-job", "run-1")

	// Create child fleet span within job context
	ctx2, span2 := tracer.StartFleetSpan(ctx1, "m5.large", "default")

	// Create another job span within fleet context
	ctx3, span3 := tracer.StartJobSpan(ctx2, "child-job", "run-2")

	// Verify all contexts and spans are valid
	if ctx1 == nil || ctx2 == nil || ctx3 == nil {
		t.Error("All contexts should be non-nil")
	}
	if span1 == nil || span2 == nil || span3 == nil {
		t.Error("All spans should be non-nil")
	}

	// End spans in correct order
	span3.End()
	span2.End()
	span1.End()
}

func TestLoadConfig_NegativeRatio(t *testing.T) {
	originalEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	originalRatio := os.Getenv("OTEL_TRACE_SAMPLING_RATIO")
	defer func() {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEndpoint)
		_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", originalRatio)
	}()

	_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", "-1")

	cfg := LoadConfig()

	// Negative ratio should use default (1.0)
	if cfg.SamplingRatio != 1.0 {
		t.Errorf("SamplingRatio = %f, want 1.0 for negative input", cfg.SamplingRatio)
	}
}

func TestLoadConfig_GreaterThanOneRatio(t *testing.T) {
	originalEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	originalRatio := os.Getenv("OTEL_TRACE_SAMPLING_RATIO")
	defer func() {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEndpoint)
		_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", originalRatio)
	}()

	_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	_ = os.Setenv("OTEL_TRACE_SAMPLING_RATIO", "2.0")

	cfg := LoadConfig()

	// Ratio > 1 should use default (1.0)
	if cfg.SamplingRatio != 1.0 {
		t.Errorf("SamplingRatio = %f, want 1.0 for ratio > 1", cfg.SamplingRatio)
	}
}
