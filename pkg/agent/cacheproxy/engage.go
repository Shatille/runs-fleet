package cacheproxy

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// HostsPath is the hosts file the pin functions edit. It is a var so tests can
// point it at a temp file.
var HostsPath = "/etc/hosts"

// systemAnchorPath is where the CA is installed for the AL2023 system trust
// store. A var so tests can redirect it.
var systemAnchorPath = "/etc/pki/ca-trust/source/anchors/runs-fleet-cache-ca.crt"

const hostsTag = "# runs-fleet-cache"

// PinResultsHost maps host to 127.0.0.1 in the hosts file so the runner's cache
// client reaches the local interceptor. Idempotent. The agent calls this LAST,
// only after the CA is trusted, so traffic is never redirected to a listener
// the client doesn't trust.
func PinResultsHost(host string) (err error) {
	lines, err := readLines(HostsPath)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if isPinFor(l, host) {
			return nil
		}
	}
	f, openErr := os.OpenFile(HostsPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if openErr != nil {
		return fmt.Errorf("open hosts file: %w", openErr)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close hosts file: %w", cerr)
		}
	}()
	if _, werr := fmt.Fprintf(f, "127.0.0.1 %s %s\n", host, hostsTag); werr != nil {
		return fmt.Errorf("write hosts pin: %w", werr)
	}
	return nil
}

// UnpinResultsHost removes the pin for host, preserving all other entries.
func UnpinResultsHost(host string) error {
	lines, err := readLines(HostsPath)
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if isPinFor(l, host) {
			continue
		}
		kept = append(kept, l)
	}
	if len(kept) == 0 {
		return os.WriteFile(HostsPath, []byte{}, 0o644)
	}
	return os.WriteFile(HostsPath, []byte(strings.Join(kept, "\n")+"\n"), 0o644)
}

func isPinFor(line, host string) bool {
	if !strings.Contains(line, hostsTag) {
		return false
	}
	for _, f := range strings.Fields(line) {
		if f == host {
			return true
		}
	}
	return false
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read hosts file: %w", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n"), nil
}

// WriteCACert writes the CA PEM to path for NODE_EXTRA_CA_CERTS. World-readable
// so non-root job processes (Node-based actions) can load it.
func WriteCACert(path string, pem []byte) error {
	if err := os.WriteFile(path, pem, 0o644); err != nil {
		return fmt.Errorf("write CA cert: %w", err)
	}
	return nil
}

// InstallSystemTrust adds the CA to the AL2023 system trust store so non-Node
// clients also trust the interceptor. Requires root.
func InstallSystemTrust(pem []byte) error {
	if err := os.WriteFile(systemAnchorPath, pem, 0o644); err != nil {
		return fmt.Errorf("write trust anchor: %w", err)
	}
	if out, err := exec.Command("update-ca-trust", "extract").CombinedOutput(); err != nil {
		return fmt.Errorf("update-ca-trust: %w: %s", err, out)
	}
	return nil
}
