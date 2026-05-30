//go:build !linux

package main

import (
	"syscall"
	"time"
)

// setTCPUserTimeout is a no-op on non-Linux platforms (the dev machine is
// macOS); TCP_USER_TIMEOUT is a Linux-only socket option. Production runs on
// Linux, where the linux-tagged implementation applies it.
func setTCPUserTimeout(_ time.Duration) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, _ syscall.RawConn) error {
		return nil
	}
}
