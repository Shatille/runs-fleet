package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

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

	logger.Info("test message", slog.String("key", "value"))

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
	logger.Info("processing")

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
	logger.Info("msg", slog.String("inline", "val"))

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
	logger.Info("should not appear")

	if buf.Len() > 0 {
		t.Errorf("expected no output for info at warn level, got: %s", buf.String())
	}

	logger.Warn("should appear")

	if buf.Len() == 0 {
		t.Error("expected output for warn at warn level")
	}
}
