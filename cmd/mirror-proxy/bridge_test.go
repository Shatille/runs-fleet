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

// The wiring Rami's finding is really about: a bridge that fails to bind must
// take the BuildKit config with it. A config surviving a failed bind names an
// address nothing serves, and BuildKit answers that by pulling from Docker Hub
// without saying so.
func TestAddBridgeRemovesTheConfigWhenTheBridgeFailsToBind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buildkitd.toml")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	required := requiredListener(t)

	listeners, addrs := addBridge(bridgeParams{
		iface:      "docker0",
		portSource: "127.0.0.1:8989",
		configPath: path,
		addrsOf:    stubAddrs("172.17.0.1/16"),
		listen:     func(string, string) (net.Listener, error) { return nil, errors.New("address already in use") },
		logger:     discardLogger(),
	}, []net.Listener{required}, []string{"127.0.0.1:8989"})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want the config removed so BuildKit is not aimed at an unbound address", path, err)
	}
	if len(listeners) != 1 || len(addrs) != 1 {
		t.Errorf("addBridge() = %d listeners / %d addrs, want the required set untouched at 1/1", len(listeners), len(addrs))
	}
}

// The required loopback listener survives a bridge failure: dropping it would
// also cost dockerd the mirror it was already using.
func TestAddBridgeKeepsTheRequiredListenerWhenTheBridgeIsAbsent(t *testing.T) {
	required := requiredListener(t)
	missing := func(string) ([]net.Addr, error) { return nil, errors.New("no such network interface") }

	listeners, addrs := addBridge(bridgeParams{
		iface:      "docker0",
		portSource: "127.0.0.1:8989",
		addrsOf:    missing,
		listen:     func(string, string) (net.Listener, error) { return nil, errors.New("unreachable") },
		logger:     discardLogger(),
	}, []net.Listener{required}, []string{"127.0.0.1:8989"})

	if len(listeners) != 1 || listeners[0] != required {
		t.Errorf("addBridge() dropped the required listener: %v", listeners)
	}
	if len(addrs) != 1 || addrs[0] != "127.0.0.1:8989" {
		t.Errorf("addBridge() addrs = %v, want the required set untouched", addrs)
	}
}

func TestAddBridgeAppendsTheBridgeAndWritesTheBoundAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buildkitd.toml")
	rules := map[string]string{"registry-1.docker.io": "docker-hub"}
	required := requiredListener(t)

	listeners, addrs := addBridge(bridgeParams{
		iface:      "docker0",
		portSource: "127.0.0.1:8989",
		configPath: path,
		rules:      rules,
		addrsOf:    stubAddrs("172.18.0.1/16"),
		listen:     func(network, _ string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") },
		logger:     discardLogger(),
	}, []net.Listener{required}, []string{"127.0.0.1:8989"})
	defer closeAll(listeners)

	if len(listeners) != 2 {
		t.Fatalf("addBridge() opened %d listeners, want the required one plus the bridge", len(listeners))
	}
	if len(addrs) != 2 || addrs[1] != testBridgeAddr {
		t.Fatalf("addBridge() addrs = %v, want the bridge address appended", addrs)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// The config must name the address that actually bound, not the one the
	// flags asked for; a second independent lookup could resolve a different
	// one.
	if want := buildkitdConfig(addrs[1], rules); string(got) != want {
		t.Errorf("config = %q, want it to name the bound address: %q", got, want)
	}
}

func requiredListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("open the required listener: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}
