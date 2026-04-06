package tracing

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// Tracer returns the package-level tracer for runs-fleet instrumentation.
func Tracer() trace.Tracer {
	return otel.Tracer("runs-fleet")
}
