package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewExecutor(t *testing.T) {
	logger := &mockLogger{}
	safety := NewSafetyMonitor(1*time.Hour, logger)

	executor := NewExecutor(logger, safety)

	if executor == nil {
		t.Fatal("NewExecutor() returned nil")
		return
	}
	if executor.logger == nil {
		t.Error("NewExecutor() did not set logger")
	}
	if executor.safetyMonitor == nil {
		t.Error("NewExecutor() did not set safetyMonitor")
	}
}

func TestNewExecutor_NoSafetyMonitor(t *testing.T) {
	logger := &mockLogger{}

	executor := NewExecutor(logger, nil)

	if executor == nil {
		t.Fatal("NewExecutor() returned nil")
		return
	}
	if executor.safetyMonitor != nil {
		t.Error("NewExecutor() should allow nil safetyMonitor")
	}
}

func TestExecutor_SetCloudWatchLogger(t *testing.T) {
	logger := &mockLogger{}
	executor := NewExecutor(logger, nil)

	if executor.cloudWatchLogger != nil {
		t.Error("cloudWatchLogger should be nil initially")
	}

	// Create a mock CloudWatch logger
	cwLogger := &CloudWatchLogger{}
	executor.SetCloudWatchLogger(cwLogger)

	if executor.cloudWatchLogger != cwLogger {
		t.Error("SetCloudWatchLogger() did not set cloudWatchLogger")
	}
}

func TestExecutor_ExecuteJob_Success(t *testing.T) {
	logger := &mockLogger{}
	executor := NewExecutor(logger, nil)

	// Create a temporary runner directory with a run.sh script
	tmpDir := t.TempDir()
	runScript := filepath.Join(tmpDir, "run.sh")

	// Create a simple run.sh that exits successfully
	scriptContent := `#!/bin/bash
echo "Runner started"
echo "Running job..." >&2
exit 0
`
	if err := os.WriteFile(runScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to create run.sh: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := executor.ExecuteJob(ctx, tmpDir)
	if err != nil {
		t.Fatalf("ExecuteJob() error = %v", err)
	}

	if result.ExitCode != 0 {
		t.Errorf("expected ExitCode 0, got %d", result.ExitCode)
	}
	if result.Error != nil {
		t.Errorf("expected no error, got %v", result.Error)
	}
	if result.InterruptedBy != "" {
		t.Errorf("expected no interruption, got '%s'", result.InterruptedBy)
	}
	if result.Duration <= 0 {
		t.Error("expected positive duration")
	}
}

func TestExecutor_ExecuteJob_NonZeroExit(t *testing.T) {
	logger := &mockLogger{}
	executor := NewExecutor(logger, nil)

	tmpDir := t.TempDir()
	runScript := filepath.Join(tmpDir, "run.sh")

	scriptContent := `#!/bin/bash
echo "Job failed"
exit 42
`
	if err := os.WriteFile(runScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to create run.sh: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := executor.ExecuteJob(ctx, tmpDir)
	if err != nil {
		t.Fatalf("ExecuteJob() error = %v", err)
	}

	if result.ExitCode != 42 {
		t.Errorf("expected ExitCode 42, got %d", result.ExitCode)
	}
}

func TestExecutor_ExecuteJob_RunScriptNotFound(t *testing.T) {
	logger := &mockLogger{}
	executor := NewExecutor(logger, nil)

	tmpDir := t.TempDir()
	// No run.sh created

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := executor.ExecuteJob(ctx, tmpDir)
	if err == nil {
		t.Error("ExecuteJob() expected error when run.sh is missing")
	}
	if !strings.Contains(err.Error(), "run.sh not found") {
		t.Errorf("expected 'run.sh not found' error, got: %v", err)
	}
}

func TestExecutor_ExecuteJob_ContextCancelled(t *testing.T) {
	logger := &mockLogger{}
	executor := NewExecutor(logger, nil)

	tmpDir := t.TempDir()
	runScript := filepath.Join(tmpDir, "run.sh")

	// Create a script that runs for a long time
	scriptContent := `#!/bin/bash
trap '' TERM  # Initially ignore SIGTERM for test setup
sleep 60
`
	if err := os.WriteFile(runScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to create run.sh: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	result, err := executor.ExecuteJob(ctx, tmpDir)
	if err != nil {
		t.Fatalf("ExecuteJob() error = %v", err)
	}

	if result.InterruptedBy != "context_cancelled" {
		t.Errorf("expected InterruptedBy 'context_cancelled', got '%s'", result.InterruptedBy)
	}
}

func TestExecutor_ExecuteJob_WithSafetyMonitor(t *testing.T) {
	logger := &mockLogger{}
	safety := NewSafetyMonitor(5*time.Minute, logger)
	executor := NewExecutor(logger, safety)

	tmpDir := t.TempDir()
	runScript := filepath.Join(tmpDir, "run.sh")

	scriptContent := `#!/bin/bash
exit 0
`
	if err := os.WriteFile(runScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to create run.sh: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := executor.ExecuteJob(ctx, tmpDir)
	if err != nil {
		t.Fatalf("ExecuteJob() error = %v", err)
	}

	if result.ExitCode != 0 {
		t.Errorf("expected ExitCode 0, got %d", result.ExitCode)
	}
}

func TestExecutor_streamOutput(t *testing.T) {
	logger := &mockLogger{}
	executor := NewExecutor(logger, nil)

	t.Run("normal output", func(t *testing.T) {
		testLogger := &mockLogger{}
		executor.logger = testLogger

		reader := strings.NewReader("line1\nline2\nline3\n")
		executor.streamOutput(reader, "stdout")

		if len(testLogger.messages) != 3 {
			t.Errorf("expected 3 log messages, got %d", len(testLogger.messages))
		}
	})

	t.Run("empty output", func(t *testing.T) {
		testLogger := &mockLogger{}
		executor.logger = testLogger

		reader := strings.NewReader("")
		executor.streamOutput(reader, "stdout")

		if len(testLogger.messages) != 0 {
			t.Errorf("expected 0 log messages, got %d", len(testLogger.messages))
		}
	})

	t.Run("with CloudWatch logger", func(t *testing.T) {
		testLogger := &mockLogger{}
		executor.logger = testLogger

		// Create a mock CloudWatch logger that captures output
		cwLogger := &CloudWatchLogger{}
		executor.SetCloudWatchLogger(cwLogger)

		reader := strings.NewReader("test output\n")
		executor.streamOutput(reader, "stdout")

		// The output should still go to the regular logger
		if len(testLogger.messages) != 1 {
			t.Errorf("expected 1 log message, got %d", len(testLogger.messages))
		}
	})

	t.Run("long lines", func(t *testing.T) {
		testLogger := &mockLogger{}
		executor.logger = testLogger

		// Create a very long line (but within buffer limits)
		longLine := strings.Repeat("x", 10000)
		reader := strings.NewReader(longLine + "\n")
		executor.streamOutput(reader, "stdout")

		if len(testLogger.messages) != 1 {
			t.Errorf("expected 1 log message for long line, got %d", len(testLogger.messages))
		}
	})
}

func TestExecutor_streamOutput_ReaderError(t *testing.T) {
	logger := &mockLogger{}
	executor := NewExecutor(logger, nil)

	// Create a reader that returns an error
	errReader := &errorReader{err: io.ErrUnexpectedEOF}

	executor.streamOutput(errReader, "stderr")

	// Should log the error
	hasErrorLog := false
	for _, msg := range logger.messages {
		if strings.Contains(msg, "Error reading") {
			hasErrorLog = true
			break
		}
	}
	if !hasErrorLog {
		t.Error("expected error log for reader error")
	}
}

func TestJobResult_Structure(t *testing.T) {
	now := time.Now()
	result := JobResult{
		ExitCode:      1,
		StartedAt:     now.Add(-1 * time.Minute),
		CompletedAt:   now,
		Duration:      1 * time.Minute,
		InterruptedBy: "signal",
		Error:         nil,
	}

	if result.ExitCode != 1 {
		t.Errorf("expected ExitCode 1, got %d", result.ExitCode)
	}
	if result.Duration != 1*time.Minute {
		t.Errorf("expected Duration 1m, got %v", result.Duration)
	}
	if result.InterruptedBy != "signal" {
		t.Errorf("expected InterruptedBy 'signal', got '%s'", result.InterruptedBy)
	}
}

func TestGracefulShutdownTimeoutConstant(t *testing.T) {
	if GracefulShutdownTimeout != 90*time.Second {
		t.Errorf("expected GracefulShutdownTimeout 90s, got %v", GracefulShutdownTimeout)
	}
}

// errorReader is a reader that always returns an error after reading some data.
type errorReader struct {
	err      error
	readOnce bool
}

func (r *errorReader) Read(p []byte) (n int, err error) {
	if !r.readOnce {
		r.readOnce = true
		copy(p, "some data\n")
		return 10, nil
	}
	return 0, r.err
}
