package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNewCleanup(t *testing.T) {
	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	if cleanup.logger != logger {
		t.Error("expected logger to be set")
	}
}

func TestCleanup_CleanupRunner_Success(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "cleanup-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create subdirectories
	workDir := filepath.Join(tmpDir, "_work")
	diagDir := filepath.Join(tmpDir, "_diag")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("failed to create work dir: %v", err)
	}
	if err := os.MkdirAll(diagDir, 0755); err != nil {
		t.Fatalf("failed to create diag dir: %v", err)
	}

	// Create some files
	testFile := filepath.Join(workDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	err = cleanup.CleanupRunner(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify directory was removed
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Error("expected runner directory to be removed")
	}
}

func TestCleanup_CleanupRunner_ContextCancelled(t *testing.T) {
	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := cleanup.CleanupRunner(ctx, "/some/path")
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestCleanup_CleanupRunner_NonExistent(t *testing.T) {
	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	// Should not error on non-existent directory
	err := cleanup.CleanupRunner(context.Background(), "/nonexistent/path/runner")
	if err != nil {
		t.Fatalf("unexpected error for non-existent directory: %v", err)
	}
}

func TestCleanup_removeDirectory_NonExistent(t *testing.T) {
	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	// Should not error on non-existent directory
	err := cleanup.removeDirectory("/nonexistent/path")
	if err != nil {
		t.Errorf("unexpected error for non-existent directory: %v", err)
	}
}

func TestCleanup_removeDirectory_Success(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cleanup-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	err = cleanup.removeDirectory(tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify directory was removed
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Error("expected directory to be removed")
	}
}

func TestCleanup_CleanupTempFiles_Success(t *testing.T) {
	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	// Create some temp files matching patterns
	tmpDir := os.TempDir()
	testFiles := []string{
		filepath.Join(tmpDir, "actions-runner-test.tar.gz"),
		filepath.Join(tmpDir, "download-test"),
		filepath.Join(tmpDir, "cache-test"),
	}

	for _, f := range testFiles {
		if err := os.WriteFile(f, []byte("test"), 0644); err != nil {
			t.Fatalf("failed to create test file %s: %v", f, err)
		}
	}

	// Clean up on test completion
	defer func() {
		for _, f := range testFiles {
			os.Remove(f)
		}
	}()

	err := cleanup.CleanupTempFiles(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCleanup_CleanupTempFiles_ContextCancelled(t *testing.T) {
	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := cleanup.CleanupTempFiles(ctx)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestCleanup_CleanupLogs_Success(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "cleanup-logs-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create diag directory with log files
	diagDir := filepath.Join(tmpDir, "_diag")
	if err := os.MkdirAll(diagDir, 0755); err != nil {
		t.Fatalf("failed to create diag dir: %v", err)
	}

	// Create log files
	logFile := filepath.Join(diagDir, "runner.log")
	if err := os.WriteFile(logFile, []byte("log content"), 0644); err != nil {
		t.Fatalf("failed to create log file: %v", err)
	}

	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	err = cleanup.CleanupLogs(context.Background(), tmpDir, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify log file was removed
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Error("expected log file to be removed")
	}
}

func TestCleanup_CleanupLogs_NoDiagDir(t *testing.T) {
	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	// Should not error when diag directory doesn't exist
	err := cleanup.CleanupLogs(context.Background(), "/nonexistent/path", 0)
	if err != nil {
		t.Fatalf("unexpected error for non-existent diag dir: %v", err)
	}
}

func TestCleanup_CleanupLogs_ContextCancelled(t *testing.T) {
	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := cleanup.CleanupLogs(ctx, "/some/path", 0)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestCleanup_CleanupLogs_NonLogFiles(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "cleanup-logs-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create diag directory
	diagDir := filepath.Join(tmpDir, "_diag")
	if err := os.MkdirAll(diagDir, 0755); err != nil {
		t.Fatalf("failed to create diag dir: %v", err)
	}

	// Create a non-log file
	nonLogFile := filepath.Join(diagDir, "data.txt")
	if err := os.WriteFile(nonLogFile, []byte("data"), 0644); err != nil {
		t.Fatalf("failed to create non-log file: %v", err)
	}

	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	err = cleanup.CleanupLogs(context.Background(), tmpDir, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify non-log file was NOT removed
	if _, err := os.Stat(nonLogFile); os.IsNotExist(err) {
		t.Error("non-log file should NOT be removed")
	}
}
