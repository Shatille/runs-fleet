package tracing

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type mapCarrier map[string]string

func (c mapCarrier) Get(key string) string   { return c[key] }
func (c mapCarrier) Set(key, value string)    { c[key] = value }
func (c mapCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

var propagator = propagation.TraceContext{}

// InjectTraceContext extracts the current span from ctx and returns a W3C traceparent string.
// Returns empty string if no valid span is in the context.
func InjectTraceContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	carrier := mapCarrier{}
	propagator.Inject(ctx, carrier)
	return carrier["traceparent"]
}

// ExtractTraceContext parses a W3C traceparent string and returns a context
// with the remote span context set, suitable for tracer.Start(ctx, ...) to create child spans.
// Returns background context if traceparent is empty or invalid.
func ExtractTraceContext(traceparent string) context.Context {
	if traceparent == "" {
		return context.Background()
	}
	carrier := mapCarrier{"traceparent": traceparent}
	return propagator.Extract(context.Background(), carrier)
}
