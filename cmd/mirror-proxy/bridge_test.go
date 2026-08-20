package main

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
)

const testBridgeAddr = "172.18.0.1:8989"

func TestBindBridgeBindsTheDiscoveredGateway(t *testing.T) {
	var asked string
	listen := func(network, address string) (net.Listener, error) {
		asked = address
		return net.Listen(network, "127.0.0.1:0")
	}

	l, addr := bindBridge("docker0", "127.0.0.1:8989", stubAddrs("172.18.0.1/16"), listen, discardLogger())
	if l == nil {
		t.Fatal("bindBridge() listener = nil, want a listener")
	}
	defer func() { _ = l.Close() }()

	if addr != testBridgeAddr {
		t.Errorf("bindBridge() addr = %q, want %q", addr, testBridgeAddr)
	}
	if asked != testBridgeAddr {
		t.Errorf("bindBridge() bound %q, want it to bind the address it reports", asked)
	}
}

func TestBindBridgeDisabledByEmptyInterface(t *testing.T) {
	refuse := func(string, string) (net.Listener, error) {
		t.Fatal("bindBridge() attempted a bind with the bridge disabled")
		return nil, nil
	}
	l, addr := bindBridge("", "127.0.0.1:8989", stubAddrs("172.17.0.1/16"), refuse, discardLogger())
	assertBridgeSkipped(t, l, addr)
}

// Every failure path must report an empty address, because main passes it to
// reconcileBuildkitdConfig: a non-empty address there points BuildKit at a
// port nothing serves, which BuildKit turns into a silent Docker Hub pull.
func TestBindBridgeReportsNoAddressOnEveryFailure(t *testing.T) {
	ok := func(network, _ string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") }
	fail := func(string, string) (net.Listener, error) { return nil, errors.New("address already in use") }
	missing := func(string) ([]net.Addr, error) { return nil, errors.New("no such network interface") }

	t.Run("port undeliverable from -listen", func(t *testing.T) {
		l, addr := bindBridge("docker0", "127.0.0.1", stubAddrs("172.17.0.1/16"), ok, discardLogger())
		assertBridgeSkipped(t, l, addr)
	})
	t.Run("bridge absent", func(t *testing.T) {
		l, addr := bindBridge("docker0", "127.0.0.1:8989", missing, ok, discardLogger())
		assertBridgeSkipped(t, l, addr)
	})
	t.Run("bridge has no IPv4", func(t *testing.T) {
		l, addr := bindBridge("docker0", "127.0.0.1:8989", stubAddrs("fe80::1/64"), ok, discardLogger())
		assertBridgeSkipped(t, l, addr)
	})
	t.Run("bind refused", func(t *testing.T) {
		l, addr := bindBridge("docker0", "127.0.0.1:8989", stubAddrs("172.17.0.1/16"), fail, discardLogger())
		assertBridgeSkipped(t, l, addr)
	})
}

func TestReconcileBuildkitdConfigWritesTheBoundAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buildkitd.toml")
	rules := map[string]string{"registry-1.docker.io": "docker-hub"}

	reconcileBuildkitdConfig(path, "172.17.0.1:8989", rules, discardLogger())

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if want := buildkitdConfig("172.17.0.1:8989", rules); string(got) != want {
		t.Errorf("config = %q, want %q", got, want)
	}
}

// A config left behind after the bridge failed names an address nothing
// listens on, which looks like a working mirror to the buildx shim.
func TestReconcileBuildkitdConfigRemovesTheConfigWhenNothingBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buildkitd.toml")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}

	reconcileBuildkitdConfig(path, "", nil, discardLogger())

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want the stale config removed", path, err)
	}
}

func TestReconcileBuildkitdConfigDisabledByEmptyPath(t *testing.T) {
	dir := t.TempDir()

	reconcileBuildkitdConfig("", "172.17.0.1:8989", nil, discardLogger())

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		t.Errorf("reconcileBuildkitdConfig() wrote %v with the config path disabled", entries)
	}
}

func assertBridgeSkipped(t *testing.T, l net.Listener, addr string) {
	t.Helper()
	if l != nil {
		_ = l.Close()
		t.Error("bindBridge() listener != nil, want nil")
	}
	if addr != "" {
		t.Errorf("bindBridge() addr = %q, want \"\" so the BuildKit config is removed rather than left dangling", addr)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
