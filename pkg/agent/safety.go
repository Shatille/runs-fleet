package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var memoryCheckWarningOnce sync.Once

const (
	// DefaultCheckInterval is how often to check resource usage.
	DefaultCheckInterval = 30 * time.Second
	// MinDiskSpaceWarning is the minimum disk space before warning (500MB).
	MinDiskSpaceWarning = 500 * 1024 * 1024
	// MinMemoryWarning is the minimum available memory before warning (200MB).
	MinMemoryWarning = 200 * 1024 * 1024
)

// SafetyMonitor monitors resource usage and enforces safety limits.
type SafetyMonitor struct {
	maxRuntime    time.Duration
	checkInterval time.Duration
	startTime     time.Time
	logger        Logger
	onTimeout     func()
}

// NewSafetyMonitor creates a new safety monitor.
func NewSafetyMonitor(maxRuntime time.Duration, logger Logger) *SafetyMonitor {
	return &SafetyMonitor{
		maxRuntime:    maxRuntime,
		checkInterval: DefaultCheckInterval,
		startTime:     time.Now(),
		logger:        logger,
	}
}

// SetTimeoutCallback sets a callback to invoke on timeout.
func (s *SafetyMonitor) SetTimeoutCallback(cb func()) {
	s.onTimeout = cb
}

// Monitor starts monitoring resources in the background.
// It checks disk space, memory, and runtime periodically.
func (s *SafetyMonitor) Monitor(ctx context.Context) {
	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.check()
		}
	}
}

// check performs safety checks.
func (s *SafetyMonitor) check() {
	elapsed := time.Since(s.startTime)
	if elapsed > s.maxRuntime {
		s.logger.Printf("SAFETY: Maximum runtime exceeded (%v > %v)", elapsed, s.maxRuntime)
		if s.onTimeout != nil {
			s.onTimeout()
		}
		return
	}

	remaining := s.maxRuntime - elapsed
	if remaining < 10*time.Minute {
		s.logger.Printf("SAFETY: Warning - only %v remaining before max runtime", remaining)
	}

	if err := s.checkDiskSpace(); err != nil {
		s.logger.Printf("SAFETY: Disk space issue: %v", err)
	}

	if err := s.checkMemory(); err != nil {
		s.logger.Printf("SAFETY: Memory issue: %v", err)
	}
}

// checkDiskSpace checks available disk space.
func (s *SafetyMonitor) checkDiskSpace() error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return fmt.Errorf("failed to check disk space: %w", err)
	}

	available := stat.Bavail * uint64(stat.Bsize)
	if available < MinDiskSpaceWarning {
		return fmt.Errorf("low disk space: %d bytes available", available)
	}

	return nil
}

// checkMemory checks available memory by reading /proc/meminfo.
// WARNING: This only works on Linux systems. On other platforms, this check is skipped.
func (s *SafetyMonitor) checkMemory() error {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		// Non-Linux systems may not have /proc/meminfo
		// Log warning once to inform operators that memory checking is unavailable
		memoryCheckWarningOnce.Do(func() {
			s.logger.Printf("WARNING: Memory check not available on this platform (no /proc/meminfo), skipping memory monitoring")
		})
		return nil
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	var memAvailable int64 = -1

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				val, err := strconv.ParseInt(fields[1], 10, 64)
				if err == nil {
					// Value is in KB, convert to bytes
					memAvailable = val * 1024
					break
				}
			}
		}
	}

	if memAvailable >= 0 && memAvailable < MinMemoryWarning {
		return fmt.Errorf("low memory: %d bytes available", memAvailable)
	}

	return nil
}

// GetRemainingTime returns the remaining time before max runtime.
func (s *SafetyMonitor) GetRemainingTime() time.Duration {
	elapsed := time.Since(s.startTime)
	if elapsed >= s.maxRuntime {
		return 0
	}
	return s.maxRuntime - elapsed
}

// IsExpired returns true if the max runtime has been exceeded.
func (s *SafetyMonitor) IsExpired() bool {
	return time.Since(s.startTime) > s.maxRuntime
}
