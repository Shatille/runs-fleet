package tracing

import (
	"context"
	"fmt"
	"runtime/debug"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/credentials/insecure"
)

// Setup initializes an OpenTelemetry TracerProvider based on the provided configuration.
// Returns a noop TracerProvider when tracing is disabled or no endpoint is configured.
func Setup(ctx context.Context, cfg Config) (trace.TracerProvider, error) {
	if !cfg.Enabled || cfg.Endpoint == "" {
		return noop.NewTracerProvider(), nil
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithTLSCredentials(insecure.NewCredentials()))
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	version := serviceVersion()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(version),
			semconv.DeploymentEnvironment(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	return tp, nil
}

// Shutdown gracefully flushes and shuts down the TracerProvider.
// Safe to call on noop providers.
func Shutdown(ctx context.Context, tp trace.TracerProvider) error {
	if stp, ok := tp.(*sdktrace.TracerProvider); ok {
		return stp.Shutdown(ctx)
	}
	return nil
}

func serviceVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
