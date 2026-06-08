package cacheproxy

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
)

// helperPath is the root-owned engage helper baked into the AMI. It performs the
// two root-only steps of cache engagement — the /etc/hosts pin and the system
// trust anchor — behind a scoped sudoers rule, so the agent can stay ec2-user.
// A var so tests can redirect it without shelling out to real sudo.
var helperPath = "/usr/local/sbin/runs-fleet-cache-engage"

// runHelper invokes the engage helper under sudo, feeding stdin to it. A var so
// tests can stub the whole invocation and assert the arguments without root.
var runHelper = func(stdin []byte, args ...string) error {
	cmd := exec.Command("sudo", append([]string{helperPath}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cache-engage %v: %w: %s", args, err, out)
	}
	return nil
}

// EngageCacheTrustAndPin installs the per-instance CA into the system trust
// store (best-effort, inside the helper) and pins the results host to the local
// interceptor, via the baked root helper. The CA PEM is passed on stdin so the
// helper never trusts a caller-named file path. Called LAST, only after the
// listener is healthy and NODE_EXTRA_CA_CERTS is written, so traffic is never
// redirected to an untrusted listener.
func EngageCacheTrustAndPin(host string, caPEM []byte) error {
	return runHelper(caPEM, "engage", host)
}

// DisengageCache removes the pin and the trust anchor via the helper. Symmetric
// teardown for EngageCacheTrustAndPin.
func DisengageCache(host string) error {
	return runHelper(nil, "disengage", host)
}

// WriteCACert writes the CA PEM to path for NODE_EXTRA_CA_CERTS. World-readable
// so non-root job processes (Node-based actions) can load it. Runs as ec2-user
// (the path is under the runner working dir), so it needs no privilege.
func WriteCACert(path string, pem []byte) error {
	if err := os.WriteFile(path, pem, 0o644); err != nil {
		return fmt.Errorf("write CA cert: %w", err)
	}
	return nil
}
