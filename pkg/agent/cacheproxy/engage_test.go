package cacheproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempHosts(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := HostsPath
	HostsPath = path
	t.Cleanup(func() { HostsPath = old })
}

func readHosts(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(HostsPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPinResultsHostAppendsOnceAndPreserves(t *testing.T) {
	tempHosts(t, "127.0.0.1 localhost\n::1 localhost\n")
	host := "results-receiver.actions.githubusercontent.com"

	if err := PinResultsHost(host); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := PinResultsHost(host); err != nil { // idempotent
		t.Fatalf("pin again: %v", err)
	}
	got := readHosts(t)
	if !strings.Contains(got, "127.0.0.1 localhost") || !strings.Contains(got, "::1 localhost") {
		t.Error("existing hosts entries not preserved")
	}
	if n := strings.Count(got, host); n != 1 {
		t.Errorf("pin appears %d times, want exactly 1 (idempotent)", n)
	}
}

func TestUnpinResultsHostRemovesOnlyPin(t *testing.T) {
	host := "results-receiver.actions.githubusercontent.com"
	tempHosts(t, "127.0.0.1 localhost\n")
	if err := PinResultsHost(host); err != nil {
		t.Fatal(err)
	}
	if err := UnpinResultsHost(host); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	got := readHosts(t)
	if strings.Contains(got, host) {
		t.Error("pin not removed")
	}
	if !strings.Contains(got, "127.0.0.1 localhost") {
		t.Error("unrelated entry was removed")
	}
}

func TestWriteCACert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := WriteCACert(path, []byte("PEMDATA")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "PEMDATA" {
		t.Errorf("read back = %q, %v", data, err)
	}
}
