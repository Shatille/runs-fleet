//go:build linux

package main

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// setTCPUserTimeout must set TCP_USER_TIMEOUT to the requested millisecond value
// on a real connection's socket. Build-tagged to Linux; skipped on macOS, which
// is expected and not a failure.
func TestSetTCPUserTimeoutAppliesSockopt(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("expected *net.TCPConn, got %T", conn)
	}
	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}

	const timeout = 20 * time.Second
	if serr := setTCPUserTimeout(timeout)("tcp", ln.Addr().String(), rawConn); serr != nil {
		t.Fatalf("setTCPUserTimeout: %v", serr)
	}

	var got int
	if cerr := rawConn.Control(func(fd uintptr) {
		got, err = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT)
	}); cerr != nil {
		t.Fatalf("control: %v", cerr)
	}
	if err != nil {
		t.Fatalf("getsockopt TCP_USER_TIMEOUT: %v", err)
	}

	if want := int(timeout.Milliseconds()); got != want {
		t.Fatalf("TCP_USER_TIMEOUT = %d ms, want %d ms", got, want)
	}
}
