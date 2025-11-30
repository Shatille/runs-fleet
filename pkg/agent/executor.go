package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	// GracefulShutdownTimeout is how long to wait for graceful shutdown.
	GracefulShutdownTimeout = 90 * time.Second
)

// JobResult represents the result of a job execution.
type JobResult struct {
	ExitCode      int
	StartedAt     time.Time
	CompletedAt   time.Time
	Duration      time.Duration
	InterruptedBy string
	Error         error
}

// Executor handles running the GitHub Actions runner process.
type Executor struct {
	logger          Logger
	safetyMonitor   *SafetyMonitor
	cloudWatchLogger *CloudWatchLogger
}

// NewExecutor creates a new job executor.
func NewExecutor(logger Logger, safety *SafetyMonitor) *Executor {
	return &Executor{
		logger:        logger,
		safetyMonitor: safety,
	}
}

// SetCloudWatchLogger sets the CloudWatch logger for streaming output.
func (e *Executor) SetCloudWatchLogger(cwLogger *CloudWatchLogger) {
	e.cloudWatchLogger = cwLogger
}

// ExecuteJob runs the GitHub Actions runner and monitors it.
func (e *Executor) ExecuteJob(ctx context.Context, runnerPath string) (*JobResult, error) {
	runScript := filepath.Join(runnerPath, "run.sh")

	// Verify run.sh exists
	if _, err := os.Stat(runScript); err != nil {
		return nil, fmt.Errorf("run.sh not found: %w", err)
	}

	result := &JobResult{
		StartedAt: time.Now(),
	}

	// Create command
	cmd := exec.CommandContext(ctx, runScript)
	cmd.Dir = runnerPath
	cmd.Env = append(os.Environ(),
		"RUNNER_ALLOW_RUNASROOT=1",
	)

	// Set up process group for signal handling
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Get stdout and stderr pipes
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	// Start the process
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start runner: %w", err)
	}

	e.logger.Printf("Runner process started with PID %d", cmd.Process.Pid)

	// Start safety monitor
	safetyCtx, safetyCancel := context.WithCancel(ctx)
	defer safetyCancel()

	if e.safetyMonitor != nil {
		go e.safetyMonitor.Monitor(safetyCtx)
	}

	// Stream output
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		e.streamOutput(stdout, "stdout")
	}()

	go func() {
		defer wg.Done()
		e.streamOutput(stderr, "stderr")
	}()

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	// Wait for completion or signals
	doneChan := make(chan error, 1)
	go func() {
		doneChan <- cmd.Wait()
	}()

	var interruptedBy string
	select {
	case err := <-doneChan:
		// Process completed normally
		wg.Wait()
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)

		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				result.ExitCode = exitErr.ExitCode()
			} else {
				result.ExitCode = -1
				result.Error = err
			}
		} else {
			result.ExitCode = 0
		}

	case sig := <-sigChan:
		// Signal received - graceful shutdown
		e.logger.Printf("Received signal %v, initiating graceful shutdown...", sig)
		interruptedBy = sig.String()
		result.InterruptedBy = interruptedBy

		// Send SIGTERM to process group
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}

		// Wait for graceful shutdown with timeout
		select {
		case err := <-doneChan:
			wg.Wait()
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)

			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					result.ExitCode = exitErr.ExitCode()
				} else {
					result.ExitCode = -1
					result.Error = err
				}
			}
			e.logger.Println("Runner exited gracefully after signal")

		case <-time.After(GracefulShutdownTimeout):
			// Force kill
			e.logger.Println("Graceful shutdown timeout, force killing runner...")
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			<-doneChan
			wg.Wait()
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			result.ExitCode = -1
			result.Error = fmt.Errorf("killed after timeout")
		}

	case <-ctx.Done():
		// Context cancelled
		e.logger.Println("Context cancelled, terminating runner...")
		interruptedBy = "context_cancelled"
		result.InterruptedBy = interruptedBy

		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}

		select {
		case <-doneChan:
		case <-time.After(GracefulShutdownTimeout):
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			<-doneChan
		}

		wg.Wait()
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		result.ExitCode = -1
		result.Error = ctx.Err()
	}

	signal.Stop(sigChan)
	safetyCancel()

	return result, nil
}

// streamOutput reads from a reader and logs each line.
func (e *Executor) streamOutput(r io.Reader, source string) {
	scanner := bufio.NewScanner(r)
	// Increase buffer size for long lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		msg := fmt.Sprintf("[%s] %s", source, line)
		e.logger.Printf("%s", msg)

		// Also send to CloudWatch if configured
		if e.cloudWatchLogger != nil {
			e.cloudWatchLogger.Log(msg)
		}
	}

	if err := scanner.Err(); err != nil {
		e.logger.Printf("Error reading %s: %v", source, err)
	}
}
