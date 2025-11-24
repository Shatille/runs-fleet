//go:build !linux && !darwin

package agent

import (
	"log"
	"sync"
)

var diskCheckWarningOnce sync.Once

type syscallStatfs struct {
	Bsize  int64
	Bavail uint64
}

func statfs(path string, stat *syscallStatfs) error {
	// WARNING: On unsupported platforms (e.g., Windows), actual disk space checking is not available.
	// This stub returns a dummy value of 10GB available space.
	// For production use, this agent should run on Linux or macOS where accurate disk checking is implemented.
	diskCheckWarningOnce.Do(func() {
		log.Println("WARNING: Disk space check not available on this platform (Windows or other unsupported OS), assuming sufficient space")
	})
	stat.Bsize = 4096
	stat.Bavail = 10 * 1024 * 1024 * 1024 / 4096 // 10GB assumed
	return nil
}
