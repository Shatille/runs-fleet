//go:build !linux

package agent

type syscallStatfs struct {
	Bsize  int64
	Bavail uint64
}

func statfs(path string, stat *syscallStatfs) error {
	// On non-Linux systems, assume sufficient space
	stat.Bsize = 4096
	stat.Bavail = 10 * 1024 * 1024 * 1024 / 4096 // 10GB
	return nil
}
