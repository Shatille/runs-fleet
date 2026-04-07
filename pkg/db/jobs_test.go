package db

import "testing"

func TestExtractTraceID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		traceparent string
		want        string
	}{
		{"valid traceparent", "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01", "0102030405060708090a0b0c0d0e0f10"},
		{"empty", "", ""},
		{"too few parts", "00-abc-01", ""},
		{"too many parts", "00-abc-def-01-extra", ""},
		{"short trace_id", "00-abc-0102030405060708-01", ""},
		{"valid all zeros", "00-00000000000000000000000000000000-0102030405060708-01", "00000000000000000000000000000000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractTraceID(tt.traceparent)
			if got != tt.want {
				t.Errorf("extractTraceID(%q) = %q, want %q", tt.traceparent, got, tt.want)
			}
		})
	}
}
