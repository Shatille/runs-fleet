package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Cleanup handles cleaning up runner resources.
type Cleanup struct {
	logger Logger
}

// NewCleanup creates a new cleanup handler.
func NewCleanup(logger Logger) *Cleanup {
	return &Cleanup{
		logger: logger,
	}
}

// CleanupRunner removes the runner directory and its contents.
func (c *Cleanup) CleanupRunner(ctx context.Context, runnerPath string) error {
	// Check if context is cancelled
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	c.logger.Printf("Cleaning up runner directory: %s", runnerPath)

	// Remove work directory first (may contain large files)
	workDir := filepath.Join(runnerPath, "_work")
	if err := c.removeDirectory(workDir); err != nil {
		c.logger.Printf("Warning: failed to remove work directory: %v", err)
	}

	// Remove diag directory (runner diagnostics)
	diagDir := filepath.Join(runnerPath, "_diag")
	if err := c.removeDirectory(diagDir); err != nil {
		c.logger.Printf("Warning: failed to remove diag directory: %v", err)
	}

	// Remove runner directory
	if err := c.removeDirectory(runnerPath); err != nil {
		return fmt.Errorf("failed to remove runner directory: %w", err)
	}

	c.logger.Println("Cleanup completed successfully")
	return nil
}

// removeDirectory removes a directory and all its contents.
func (c *Cleanup) removeDirectory(path string) error {
	// Check if path exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	return os.RemoveAll(path)
}

// CleanupTempFiles removes temporary files created during agent execution.
func (c *Cleanup) CleanupTempFiles(ctx context.Context) error {
	// Check if context is cancelled
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Clean up any temp files we created
	tempDir := os.TempDir()

	patterns := []string{
		"actions-runner-*.tar.gz",
		"download-*",
		"cache-*",
	}

	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(tempDir, pattern))
		if err != nil {
			continue
		}

		for _, match := range matches {
			if err := os.Remove(match); err != nil {
				c.logger.Printf("Warning: failed to remove temp file %s: %v", match, err)
			}
		}
	}

	return nil
}

// CleanupLogs removes old log files.
func (c *Cleanup) CleanupLogs(ctx context.Context, runnerPath string, maxAge int) error {
	// Check if context is cancelled
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	diagDir := filepath.Join(runnerPath, "_diag")
	if _, err := os.Stat(diagDir); os.IsNotExist(err) {
		return nil
	}

	// Remove all log files
	err := filepath.Walk(diagDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == ".log" {
			if err := os.Remove(path); err != nil {
				c.logger.Printf("Warning: failed to remove log file %s: %v", path, err)
			}
		}
		return nil
	})

	return err
}
