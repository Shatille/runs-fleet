package tracing

import (
	"testing"
)

func TestTracer_ReturnsNonNil(t *testing.T) {
	t.Parallel()
	tr := Tracer()
	if tr == nil {
		t.Fatal("Tracer() returned nil")
	}
}

func TestTracer_ConsistentName(t *testing.T) {
	t.Parallel()
	tr1 := Tracer()
	tr2 := Tracer()
	if tr1 != tr2 {
		t.Error("Tracer() returned different instances for the same instrumentation name")
	}
}
