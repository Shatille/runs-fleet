package tracing

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestSetup_DisabledReturnsNoop(t *testing.T) {
	tp, err := Setup(context.Background(), Config{
		Enabled: false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := tp.(noop.TracerProvider); !ok {
		t.Fatalf("expected noop.TracerProvider, got %T", tp)
	}
}

func TestSetup_EmptyEndpointReturnsNoop(t *testing.T) {
	tp, err := Setup(context.Background(), Config{
		Enabled:  true,
		Endpoint: "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := tp.(noop.TracerProvider); !ok {
		t.Fatalf("expected noop.TracerProvider, got %T", tp)
	}
}

func TestSetup_ValidConfigReturnsSDKProvider(t *testing.T) {
	tp, err := Setup(context.Background(), Config{
		Enabled:     true,
		Endpoint:    "localhost:4317",
		Insecure:    true,
		ServiceName: "test-service",
		Environment: "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() {
		if err := Shutdown(context.Background(), tp); err != nil {
			t.Logf("failed to shutdown tracer provider: %v", err)
		}
	}()

	if _, ok := tp.(*sdktrace.TracerProvider); !ok {
		t.Fatalf("expected *sdktrace.TracerProvider, got %T", tp)
	}
}

func TestShutdown_NoopProviderIsSafe(t *testing.T) {
	tp := noop.NewTracerProvider()
	if err := Shutdown(context.Background(), tp); err != nil {
		t.Fatalf("unexpected error shutting down noop provider: %v", err)
	}
}

func TestShutdown_SDKProvider(t *testing.T) {
	tp, err := Setup(context.Background(), Config{
		Enabled:     true,
		Endpoint:    "localhost:4317",
		Insecure:    true,
		ServiceName: "test-service",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := Shutdown(context.Background(), tp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseConfig_Defaults(t *testing.T) {
	// Clear any env vars that might interfere
	for _, key := range []string{
		"RUNS_FLEET_TRACING_ENABLED",
		"RUNS_FLEET_OTEL_ENDPOINT",
		"RUNS_FLEET_OTEL_INSECURE",
		"RUNS_FLEET_OTEL_SERVICE_NAME",
		"RUNS_FLEET_ENV",
	} {
		t.Setenv(key, "")
	}

	cfg := ParseConfig()

	if cfg.Enabled {
		t.Error("expected Enabled=false by default")
	}
	if cfg.Endpoint != "" {
		t.Errorf("expected empty Endpoint, got %q", cfg.Endpoint)
	}
	if !cfg.Insecure {
		t.Error("expected Insecure=true by default")
	}
	if cfg.ServiceName != "runs-fleet" {
		t.Errorf("expected ServiceName='runs-fleet', got %q", cfg.ServiceName)
	}
	if cfg.Environment != "" {
		t.Errorf("expected empty Environment, got %q", cfg.Environment)
	}
}

func TestParseConfig_FromEnv(t *testing.T) {
	t.Setenv("RUNS_FLEET_TRACING_ENABLED", "true")
	t.Setenv("RUNS_FLEET_OTEL_ENDPOINT", "otel-collector:4317")
	t.Setenv("RUNS_FLEET_OTEL_INSECURE", "false")
	t.Setenv("RUNS_FLEET_OTEL_SERVICE_NAME", "my-service")
	t.Setenv("RUNS_FLEET_ENV", "production")

	cfg := ParseConfig()

	if !cfg.Enabled {
		t.Error("expected Enabled=true")
	}
	if cfg.Endpoint != "otel-collector:4317" {
		t.Errorf("expected Endpoint='otel-collector:4317', got %q", cfg.Endpoint)
	}
	if cfg.Insecure {
		t.Error("expected Insecure=false")
	}
	if cfg.ServiceName != "my-service" {
		t.Errorf("expected ServiceName='my-service', got %q", cfg.ServiceName)
	}
	if cfg.Environment != "production" {
		t.Errorf("expected Environment='production', got %q", cfg.Environment)
	}
}
