package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		input    string
		want     slog.Level
		wantWarn bool
	}{
		{"debug", slog.LevelDebug, false},
		{"DEBUG", slog.LevelDebug, false},
		{"info", slog.LevelInfo, false},
		{"INFO", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"ERROR", slog.LevelError, false},
		{"", slog.LevelInfo, false},       // unset defaults to info, no warning
		{"verbose", slog.LevelInfo, true}, // invalid defaults to info, with warning
	}
	for _, tc := range cases {
		got, warn := parseLevel(tc.input)
		if got != tc.want {
			t.Errorf("parseLevel(%q) level = %v, want %v", tc.input, got, tc.want)
		}
		if (warn != "") != tc.wantWarn {
			t.Errorf("parseLevel(%q) warn = %q, wantWarn = %v", tc.input, warn, tc.wantWarn)
		}
	}
}

func TestNewBaseHandler_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	h, warn := newBaseHandler(&buf, slog.LevelInfo, "json")
	if warn != "" {
		t.Errorf("unexpected warning for json format: %q", warn)
	}
	slog.New(h).Info("hello")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected JSON output, got: %s", buf.String())
	}
	if entry["msg"] != "hello" {
		t.Errorf("expected msg=hello, got %v", entry["msg"])
	}
}

func TestNewBaseHandler_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	h, warn := newBaseHandler(&buf, slog.LevelInfo, "text")
	if warn != "" {
		t.Errorf("unexpected warning for text format: %q", warn)
	}
	slog.New(h).Info("hello")

	out := buf.String()
	// Text output is the key=value form, which is not valid JSON.
	if json.Valid(bytes.TrimSpace(buf.Bytes())) {
		t.Errorf("expected non-JSON text output, got JSON: %s", out)
	}
	if !strings.Contains(out, "msg=hello") {
		t.Errorf("expected key=value text output, got: %s", out)
	}
}

func TestNewBaseHandler_InvalidFormatDefaultsToJSON(t *testing.T) {
	var buf bytes.Buffer
	h, warn := newBaseHandler(&buf, slog.LevelInfo, "xml")
	if warn == "" {
		t.Error("expected a warning for an invalid format")
	}
	slog.New(h).Info("hello")

	if !json.Valid(bytes.TrimSpace(buf.Bytes())) {
		t.Errorf("expected invalid format to fall back to JSON, got: %s", buf.String())
	}
}

func TestNewBaseHandler_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	h, _ := newBaseHandler(&buf, slog.LevelDebug, "json")
	slog.New(h).Debug("debug line")
	if buf.Len() == 0 {
		t.Error("expected debug output at debug level")
	}

	buf.Reset()
	h2, _ := newBaseHandler(&buf, slog.LevelWarn, "json")
	slog.New(h2).Info("info line")
	if buf.Len() > 0 {
		t.Errorf("expected no info output at warn level, got: %s", buf.String())
	}
}

func TestLazyHandler_UsesCurrentDefault(t *testing.T) {
	// Simulate package-level logger created before Init()
	logger := WithComponent(LogTypeServer, "test")

	// Now configure JSON handler (like Init() does)
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	})

	logger.Info(context.Background(), "test message", slog.String("key", "value"))

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected JSON log output, got: %s", buf.String())
	}

	if entry["msg"] != "test message" {
		t.Errorf("expected msg=test message, got %v", entry["msg"])
	}
	if entry[KeyLogType] != LogTypeServer {
		t.Errorf("expected log_type=%s, got %v", LogTypeServer, entry[KeyLogType])
	}
	if entry[KeyComponent] != "test" {
		t.Errorf("expected component=test, got %v", entry[KeyComponent])
	}
	if entry["key"] != "value" {
		t.Errorf("expected key=value, got %v", entry["key"])
	}
}

func TestLazyHandler_WithAdditionalAttrs(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	})

	logger := WithComponent(LogTypeQueue, "worker").With(slog.String("pool", "default"))
	logger.Info(context.Background(), "processing")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected JSON log output, got: %s", buf.String())
	}

	if entry[KeyLogType] != LogTypeQueue {
		t.Errorf("expected log_type=%s, got %v", LogTypeQueue, entry[KeyLogType])
	}
	if entry["pool"] != "default" {
		t.Errorf("expected pool=default, got %v", entry["pool"])
	}
}

func TestLazyHandler_AttributeOrdering(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	})

	logger := WithComponent(LogTypeServer, "test")
	logger.Info(context.Background(), "msg", slog.String("inline", "val"))

	raw := buf.String()
	logTypeIdx := strings.Index(raw, `"log_type"`)
	componentIdx := strings.Index(raw, `"component"`)
	inlineIdx := strings.Index(raw, `"inline"`)

	if logTypeIdx < 0 || componentIdx < 0 || inlineIdx < 0 {
		t.Fatalf("missing expected fields in output: %s", raw)
	}
	if logTypeIdx > inlineIdx || componentIdx > inlineIdx {
		t.Errorf("preAttrs should appear before inline attrs, got: %s", raw)
	}
}

func TestLazyHandler_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	})

	logger := WithComponent(LogTypeServer, "test")
	logger.Info(context.Background(), "should not appear")

	if buf.Len() > 0 {
		t.Errorf("expected no output for info at warn level, got: %s", buf.String())
	}

	logger.Warn(context.Background(), "should appear")

	if buf.Len() == 0 {
		t.Error("expected output for warn at warn level")
	}
}
