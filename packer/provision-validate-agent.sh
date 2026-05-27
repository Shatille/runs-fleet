#!/bin/bash
# Post-bake smoke test. Runs inside the Packer builder after provision-runs-fleet.sh
# and before the AMI snapshot, so a defective AMI fails the build instead of being
# promoted to the launch template default. Cheapest checks only — anything that
# requires AWS/Vault/GitHub credentials belongs in an integration test.
set -euo pipefail

ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  EXPECTED_ELF="x86-64" ;;
  aarch64) EXPECTED_ELF="aarch64" ;;
  *) echo "FAIL: unsupported arch: $ARCH" >&2; exit 1 ;;
esac

fail() { echo "FAIL: $*" >&2; exit 1; }

echo "==> Validating agent binary"
AGENT=/opt/runs-fleet/runs-fleet-agent
[ -x "$AGENT" ] || fail "$AGENT missing or not executable"
file "$AGENT" | grep -q "ELF" || fail "$AGENT is not an ELF binary: $(file -b "$AGENT")"
# Catches the most common silent-failure mode: docker pull --platform falling
# back to the host arch when the requested platform is missing from :latest.
file "$AGENT" | grep -q "$EXPECTED_ELF" \
  || fail "$AGENT arch mismatch (expected $EXPECTED_ELF): $(file -b "$AGENT")"
# Loader sanity: exec the binary briefly. We don't care about exit code (it'll
# fail on missing config), only that the kernel can load it. A wrong-arch binary
# that `file` somehow misidentified will surface here as "exec format error".
set +e
out=$(sudo timeout 1 "$AGENT" 2>&1)
set -e
echo "$out" | grep -qi "exec format error" \
  && fail "agent binary cannot be exec'd on $ARCH: $out"
echo "  OK: $(file -b "$AGENT")"

echo "==> Validating bootstrap script"
BOOT=/opt/runs-fleet/agent-bootstrap.sh
[ -x "$BOOT" ] || fail "$BOOT missing or not executable"
[ -s "$BOOT" ] || fail "$BOOT is empty"
bash -n "$BOOT" || fail "$BOOT has a shell syntax error"
echo "  OK: $BOOT"

echo "==> Validating cloud-init per-boot script"
PERBOOT=/var/lib/cloud/scripts/per-boot/runs-fleet-bootstrap.sh
[ -x "$PERBOOT" ] || fail "$PERBOOT missing or not executable"
bash -n "$PERBOOT" || fail "$PERBOOT has a shell syntax error"
echo "  OK: $PERBOOT"

echo "==> Validating systemd unit"
UNIT=/etc/systemd/system/runs-fleet-agent.service
[ -f "$UNIT" ] || fail "$UNIT missing"
# systemd-analyze verify catches typos in [Unit]/[Service]/[Install] keys,
# bad ExecStart paths, broken After=/Requires= references, etc.
sudo systemd-analyze verify "$UNIT" || fail "$UNIT failed systemd verification"
echo "  OK: $UNIT"

echo "==> Agent AMI validation passed"
