package tracing

import (
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestInjectTraceContext_NoSpan(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	result := InjectTraceContext(ctx)
	if result != "" {
		t.Errorf("InjectTraceContext with no span = %q, want empty string", result)
	}
}

func TestInjectTraceContext_WithSpan(t *testing.T) {
	t.Parallel()

	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     false,
	})

	ctx := trace.ContextWithSpanContext(t.Context(), sc)
	result := InjectTraceContext(ctx)

	expected := "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01"
	if result != expected {
		t.Errorf("InjectTraceContext = %q, want %q", result, expected)
	}
}

func TestExtractTraceContext_Empty(t *testing.T) {
	t.Parallel()

	ctx := ExtractTraceContext("")
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		t.Error("ExtractTraceContext with empty string should return context without valid span")
	}
}

func TestExtractTraceContext_Invalid(t *testing.T) {
	t.Parallel()

	ctx := ExtractTraceContext("not-a-traceparent")
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		t.Error("ExtractTraceContext with invalid string should return context without valid span")
	}
}

func TestExtractTraceContext_Valid(t *testing.T) {
	t.Parallel()

	tp := "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01"
	ctx := ExtractTraceContext(tp)
	sc := trace.SpanContextFromContext(ctx)

	if !sc.IsValid() {
		t.Fatal("expected valid span context")
	}
	if !sc.IsRemote() {
		t.Error("extracted span context should be remote")
	}

	wantTraceID := "0102030405060708090a0b0c0d0e0f10"
	if sc.TraceID().String() != wantTraceID {
		t.Errorf("TraceID = %q, want %q", sc.TraceID().String(), wantTraceID)
	}

	wantSpanID := "0102030405060708"
	if sc.SpanID().String() != wantSpanID {
		t.Errorf("SpanID = %q, want %q", sc.SpanID().String(), wantSpanID)
	}

	if !sc.IsSampled() {
		t.Error("expected sampled flag to be set")
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	traceID := trace.TraceID{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99}
	spanID := trace.SpanID{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})

	ctx := trace.ContextWithSpanContext(t.Context(), sc)
	tp := InjectTraceContext(ctx)
	if tp == "" {
		t.Fatal("InjectTraceContext returned empty string for valid span")
	}

	extracted := ExtractTraceContext(tp)
	extractedSC := trace.SpanContextFromContext(extracted)

	if extractedSC.TraceID() != traceID {
		t.Errorf("round-trip TraceID = %s, want %s", extractedSC.TraceID(), traceID)
	}
	if extractedSC.SpanID() != spanID {
		t.Errorf("round-trip SpanID = %s, want %s", extractedSC.SpanID(), spanID)
	}
}
