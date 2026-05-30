//go:build linux

package main

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// setTCPUserTimeout returns a net.Dialer.Control hook that sets the Linux
// TCP_USER_TIMEOUT socket option on every dialed connection. The kernel then
// tears the connection down once transmitted data stays unacknowledged for
// longer than timeout, instead of retrying for ~15 minutes (tcp_retries2). This
// bounds the write/ACK-phase wedge that ResponseHeaderTimeout cannot cover.
func setTCPUserTimeout(timeout time.Duration) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(timeout.Milliseconds()))
		}); err != nil {
			return err
		}
		return serr
	}
}
