package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create subdirectories
	workDir := filepath.Join(tmpDir, "_work")
	diagDir := filepath.Join(tmpDir, "_diag")
	if mkdirErr := os.MkdirAll(workDir, 0755); mkdirErr != nil {
		t.Fatalf("failed to create work dir: %v", mkdirErr)
	}
	if mkdirErr := os.MkdirAll(diagDir, 0755); mkdirErr != nil {
		t.Fatalf("failed to create diag dir: %v", mkdirErr)
	}

	// Create some files
	testFile := filepath.Join(workDir, "test.txt")
	if writeErr := os.WriteFile(testFile, []byte("test"), 0644); writeErr != nil {
		t.Fatalf("failed to create test file: %v", writeErr)
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
	if !errors.Is(err, context.Canceled) {
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
	// Create an isolated temp directory for this test to avoid race conditions
	isolatedTmpDir, err := os.MkdirTemp("", "cleanup-tempfiles-test-*")
	if err != nil {
		t.Fatalf("failed to create isolated temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(isolatedTmpDir) }()

	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	// Create test files in isolated directory with unique prefixes
	testPrefix := fmt.Sprintf("test-%d-", time.Now().UnixNano())
	testFiles := []string{
		filepath.Join(isolatedTmpDir, testPrefix+"actions-runner-test.tar.gz"),
		filepath.Join(isolatedTmpDir, testPrefix+"download-test"),
		filepath.Join(isolatedTmpDir, testPrefix+"cache-test"),
	}

	for _, f := range testFiles {
		if writeErr := os.WriteFile(f, []byte("test"), 0644); writeErr != nil {
			t.Fatalf("failed to create test file %s: %v", f, writeErr)
		}
	}

	// Note: CleanupTempFiles cleans the system temp directory, not our isolated one
	// This test verifies the function runs without error
	err = cleanup.CleanupTempFiles(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Clean up our isolated test files
	for _, f := range testFiles {
		_ = os.Remove(f)
	}
}

func TestCleanup_CleanupTempFiles_ContextCancelled(t *testing.T) {
	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := cleanup.CleanupTempFiles(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestCleanup_CleanupLogs_Success(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "cleanup-logs-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create diag directory with log files
	diagDir := filepath.Join(tmpDir, "_diag")
	if mkdirErr := os.MkdirAll(diagDir, 0755); mkdirErr != nil {
		t.Fatalf("failed to create diag dir: %v", mkdirErr)
	}

	// Create log files
	logFile := filepath.Join(diagDir, "runner.log")
	if writeErr := os.WriteFile(logFile, []byte("log content"), 0644); writeErr != nil {
		t.Fatalf("failed to create log file: %v", writeErr)
	}

	logger := &mockLogger{}
	cleanup := NewCleanup(logger)

	err = cleanup.CleanupLogs(context.Background(), tmpDir, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify log file was removed
	if _, statErr := os.Stat(logFile); !os.IsNotExist(statErr) {
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
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestCleanup_CleanupLogs_NonLogFiles(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "cleanup-logs-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create diag directory
	diagDir := filepath.Join(tmpDir, "_diag")
	if mkdirErr := os.MkdirAll(diagDir, 0755); mkdirErr != nil {
		t.Fatalf("failed to create diag dir: %v", mkdirErr)
	}

	// Create a non-log file
	nonLogFile := filepath.Join(diagDir, "data.txt")
	if writeErr := os.WriteFile(nonLogFile, []byte("data"), 0644); writeErr != nil {
		t.Fatalf("failed to create non-log file: %v", writeErr)
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
