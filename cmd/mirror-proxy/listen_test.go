package main

import (
	"errors"
	"net"
	"testing"
)

func TestParseListenAddrsSingle(t *testing.T) {
	got, err := parseListenAddrs("127.0.0.1:8989")
	if err != nil {
		t.Fatalf("parseListenAddrs() error = %v", err)
	}
	if len(got) != 1 || got[0] != "127.0.0.1:8989" {
		t.Errorf("parseListenAddrs() = %v, want [127.0.0.1:8989]", got)
	}
}

func TestParseListenAddrsSplitsAndTrims(t *testing.T) {
	got, err := parseListenAddrs(" 127.0.0.1:8989 , 172.17.0.1:8989 ")
	if err != nil {
		t.Fatalf("parseListenAddrs() error = %v", err)
	}
	assertAddrs(t, got, []string{"127.0.0.1:8989", "172.17.0.1:8989"})
}

// A repeated address would make the second bind fail with "address already in
// use", turning a harmless duplicate in the unit's flag into a crash loop.
func TestParseListenAddrsDedupsPreservingOrder(t *testing.T) {
	got, err := parseListenAddrs("172.17.0.1:8989,127.0.0.1:8989,172.17.0.1:8989")
	if err != nil {
		t.Fatalf("parseListenAddrs() error = %v", err)
	}
	assertAddrs(t, got, []string{"172.17.0.1:8989", "127.0.0.1:8989"})
}

func TestParseListenAddrsRejectsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", ",", "127.0.0.1:8989,", ",127.0.0.1:8989"} {
		if _, err := parseListenAddrs(in); err == nil {
			t.Errorf("parseListenAddrs(%q) error = nil, want an error rather than a silently smaller listen set", in)
		}
	}
}

func TestPortOf(t *testing.T) {
	got, err := portOf("127.0.0.1:8989")
	if err != nil {
		t.Fatalf("portOf() error = %v", err)
	}
	if got != "8989" {
		t.Errorf("portOf() = %q, want %q", got, "8989")
	}
	if _, err := portOf("127.0.0.1"); err == nil {
		t.Error("portOf() error = nil, want an error for an address with no port")
	}
}

// The bridge gateway is discovered rather than hard-coded: dockerd picks the
// bridge subnet from a pool and avoids collisions with the host's networks, so
// a literal 172.17.0.1 is only dockerd's *usual* first choice, not a promise.
func TestDiscoverBridgeAddrUsesTheInterfacesIPv4(t *testing.T) {
	got, err := discoverBridgeAddr(stubAddrs("172.18.0.1/16"), "docker0", "8989")
	if err != nil {
		t.Fatalf("discoverBridgeAddr() error = %v", err)
	}
	if got != "172.18.0.1:8989" {
		t.Errorf("discoverBridgeAddr() = %q, want %q", got, "172.18.0.1:8989")
	}
}

func TestDiscoverBridgeAddrSkipsIPv6(t *testing.T) {
	got, err := discoverBridgeAddr(stubAddrs("fe80::1/64", "172.17.0.1/16"), "docker0", "8989")
	if err != nil {
		t.Fatalf("discoverBridgeAddr() error = %v", err)
	}
	if got != "172.17.0.1:8989" {
		t.Errorf("discoverBridgeAddr() = %q, want %q", got, "172.17.0.1:8989")
	}
}

func TestDiscoverBridgeAddrErrorsWhenInterfaceIsAbsent(t *testing.T) {
	missing := func(string) ([]net.Addr, error) { return nil, errors.New("no such network interface") }
	if _, err := discoverBridgeAddr(missing, "docker0", "8989"); err == nil {
		t.Error("discoverBridgeAddr() error = nil, want an error when the bridge does not exist")
	}
}

func TestDiscoverBridgeAddrErrorsWhenInterfaceHasNoIPv4(t *testing.T) {
	if _, err := discoverBridgeAddr(stubAddrs("fe80::1/64"), "docker0", "8989"); err == nil {
		t.Error("discoverBridgeAddr() error = nil, want an error when the bridge has no IPv4 address")
	}
}

func TestListenAllOpensEveryAddress(t *testing.T) {
	// Bind ":0" twice rather than reserving-then-releasing concrete ports: the
	// kernel hands out two distinct free ports and neither can be stolen in a
	// gap, so nothing here races another test binding the same port.
	listeners, err := listenAll([]string{"127.0.0.1:0", "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("listenAll() error = %v", err)
	}
	defer closeAll(listeners)

	if len(listeners) != 2 {
		t.Fatalf("listenAll() opened %d listeners, want 2", len(listeners))
	}
	if listeners[0].Addr().String() == listeners[1].Addr().String() {
		t.Errorf("listenAll() opened the same address twice: %s", listeners[0].Addr())
	}
}

// The proxy must not report a half-bound mirror as healthy: BuildKit would
// reach the address that did bind and silently fall back to Docker Hub on the
// one that did not.
func TestListenAllFailsAndReleasesWhenOneAddressIsUnbindable(t *testing.T) {
	// Hold a listener open and ask listenAll for the same address. Unlike an
	// unroutable IP, "already in use" fails on every host — a host with
	// net.ipv4.ip_nonlocal_bind=1 would happily bind a foreign address.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve an in-use address: %v", err)
	}
	defer func() { _ = taken.Close() }()

	listeners, err := listenAll([]string{"127.0.0.1:0", taken.Addr().String()})
	if err == nil {
		closeAll(listeners)
		t.Fatal("listenAll() error = nil, want an error when an address cannot be bound")
	}
	if listeners != nil {
		t.Errorf("listenAll() = %v, want nil listeners on error", listeners)
	}
}

// A leak would make the retry that systemd's Restart=always drives fail on an
// address the previous attempt still held.
func TestListenAllReleasesOpenedListenersOnFailure(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve an in-use address: %v", err)
	}
	defer func() { _ = taken.Close() }()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a probe address: %v", err)
	}
	good := probe.Addr().String()
	if err = probe.Close(); err != nil {
		t.Fatalf("release probe address: %v", err)
	}

	if _, err = listenAll([]string{good, taken.Addr().String()}); err == nil {
		t.Fatal("listenAll() error = nil, want an error")
	}

	reclaimed, err := net.Listen("tcp", good)
	if err != nil {
		t.Fatalf("listenAll() leaked its already-opened listener on %s: %v", good, err)
	}
	if cerr := reclaimed.Close(); cerr != nil {
		t.Errorf("close reclaimed listener: %v", cerr)
	}
}

func stubAddrs(cidrs ...string) func(string) ([]net.Addr, error) {
	return func(string) ([]net.Addr, error) {
		addrs := make([]net.Addr, 0, len(cidrs))
		for _, c := range cidrs {
			ip, ipnet, err := net.ParseCIDR(c)
			if err != nil {
				panic(err)
			}
			addrs = append(addrs, &net.IPNet{IP: ip, Mask: ipnet.Mask})
		}
		return addrs, nil
	}
}

func assertAddrs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func closeAll(listeners []net.Listener) {
	for _, l := range listeners {
		_ = l.Close()
	}
}
