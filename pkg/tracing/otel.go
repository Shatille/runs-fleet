// Package tracing provides OpenTelemetry integration for distributed tracing.
package tracing

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	serviceName    = "runs-fleet"
	serviceVersion = "1.0.0"
)

// Config holds tracing configuration.
type Config struct {
	Enabled       bool
	Endpoint      string
	SamplingRatio float64
}

// LoadConfig loads tracing configuration from environment variables.
func LoadConfig() *Config {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return &Config{Enabled: false}
	}

	samplingRatio := 1.0 // Default to sample everything
	if ratio := os.Getenv("OTEL_TRACE_SAMPLING_RATIO"); ratio != "" {
		var r float64
		if _, err := fmt.Sscanf(ratio, "%f", &r); err == nil && r >= 0 && r <= 1 {
			samplingRatio = r
		}
	}

	return &Config{
		Enabled:       true,
		Endpoint:      endpoint,
		SamplingRatio: samplingRatio,
	}
}

// Provider wraps the OpenTelemetry trace provider with optional graceful shutdown.
type Provider struct {
	provider *sdktrace.TracerProvider
	enabled  bool
}

// Init initializes the OpenTelemetry trace provider.
// Returns a no-op provider if tracing is disabled.
func Init(ctx context.Context, cfg *Config) (*Provider, error) {
	if cfg == nil || !cfg.Enabled {
		log.Println("OpenTelemetry tracing disabled")
		return &Provider{enabled: false}, nil
	}

	log.Printf("Initializing OpenTelemetry tracing with endpoint: %s", cfg.Endpoint)

	client := otlptracegrpc.NewClient(
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(), // Use insecure for local development
	)
	exporter, err := otlptrace.New(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
			attribute.String("environment", os.Getenv("RUNS_FLEET_ENVIRONMENT")),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	var sampler sdktrace.Sampler
	if cfg.SamplingRatio >= 1.0 {
		sampler = sdktrace.AlwaysSample()
	} else if cfg.SamplingRatio <= 0 {
		sampler = sdktrace.NeverSample()
	} else {
		sampler = sdktrace.TraceIDRatioBased(cfg.SamplingRatio)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	log.Println("OpenTelemetry tracing initialized successfully")
	return &Provider{provider: provider, enabled: true}, nil
}

// Shutdown gracefully shuts down the trace provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.provider == nil {
		return nil
	}
	return p.provider.Shutdown(ctx)
}

// IsEnabled returns whether tracing is enabled.
func (p *Provider) IsEnabled() bool {
	return p.enabled
}

// Tracer returns a tracer for the given package name.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// StartSpan starts a new span with the given name.
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return Tracer(serviceName).Start(ctx, name, opts...)
}

// SpanFromContext returns the current span from context.
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

// AddEvent adds an event to the current span.
func AddEvent(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.AddEvent(name, trace.WithAttributes(attrs...))
	}
}

// SetAttributes sets attributes on the current span.
func SetAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetAttributes(attrs...)
	}
}

// RecordError records an error on the current span.
func RecordError(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() && err != nil {
		span.RecordError(err)
	}
}

// InjectTraceContext injects trace context into a carrier.
func InjectTraceContext(ctx context.Context) map[string]string {
	carrier := make(map[string]string)
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(carrier))
	return carrier
}

// ExtractTraceContext extracts trace context from a carrier.
func ExtractTraceContext(ctx context.Context, carrier map[string]string) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(carrier))
}

// HTTPMiddleware instruments HTTP handlers with tracing.
type HTTPMiddleware struct {
	tracer trace.Tracer
}

// NewHTTPMiddleware creates a new HTTP tracing middleware.
func NewHTTPMiddleware() *HTTPMiddleware {
	return &HTTPMiddleware{
		tracer: Tracer("http"),
	}
}

// JobTracer provides tracing for job processing operations.
type JobTracer struct {
	tracer trace.Tracer
}

// NewJobTracer creates a new job tracer.
func NewJobTracer() *JobTracer {
	return &JobTracer{
		tracer: Tracer("job"),
	}
}

// StartJobSpan starts a span for job processing.
func (t *JobTracer) StartJobSpan(ctx context.Context, jobID, runID string) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, "process_job",
		trace.WithAttributes(
			attribute.String("job.id", jobID),
			attribute.String("job.run_id", runID),
		),
	)
}

// StartFleetSpan starts a span for fleet creation.
func (t *JobTracer) StartFleetSpan(ctx context.Context, instanceType, pool string) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, "create_fleet",
		trace.WithAttributes(
			attribute.String("fleet.instance_type", instanceType),
			attribute.String("fleet.pool", pool),
		),
	)
}

// WithTimeout wraps a context with timeout and adds it as a span attribute.
func WithTimeout(ctx context.Context, timeout time.Duration, operation string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	SetAttributes(ctx, attribute.String("operation", operation))
	return ctx, cancel
}
