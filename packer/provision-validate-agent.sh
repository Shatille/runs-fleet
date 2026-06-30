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

echo "==> Validating boot helper library"
LIB=/opt/runs-fleet/boot-lib.sh
[ -s "$LIB" ] || fail "$LIB missing or empty"
bash -n "$LIB" || fail "$LIB has a shell syntax error"
echo "  OK: $LIB"

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

echo "==> Validating cache-engage helper"
HELPER=/usr/local/sbin/runs-fleet-cache-engage
HOST=results-receiver.actions.githubusercontent.com
ANCHOR=/etc/pki/ca-trust/source/anchors/runs-fleet-cache-ca.crt
[ -x "$HELPER" ] || fail "$HELPER missing or not executable"
OWNMODE=$(stat -c '%U:%G %a' "$HELPER")
[ "$OWNMODE" = "root:root 755" ] || fail "$HELPER wrong owner/mode: $OWNMODE"
bash -n "$HELPER" || fail "$HELPER has a shell syntax error"
# The helper must refuse any host other than the single results host it pins.
# Invoke it directly (not via sudo) to exercise its own validation gate; a bogus
# host is rejected before any root step is attempted.
if "$HELPER" engage bogus.example.com </dev/null 2>/dev/null; then
  fail "$HELPER accepted an unexpected host"
fi
echo "  OK: $HELPER"

echo "==> Validating cache-engage sudoers drop-in"
SUDOERS=/etc/sudoers.d/10-runs-fleet-cache
# /etc/sudoers.d is 0750 root:root, so the (ec2-user) provisioner can't stat
# files under it without sudo — inspect via sudo to avoid a false "missing".
sudo test -f "$SUDOERS" || fail "$SUDOERS missing"
OWNMODE=$(sudo stat -c '%U:%G %a' "$SUDOERS")
[ "$OWNMODE" = "root:root 440" ] || fail "$SUDOERS wrong owner/mode: $OWNMODE"
sudo visudo -cf "$SUDOERS" || fail "$SUDOERS failed visudo validation"
# Exercise the real engage->disengage path end-to-end via passwordless sudo
# (provisioners run as ec2-user): the pin lands and teardown removes it. The
# scoped rule's syntax is covered by visudo -cf above; broad AL2023 NOPASSWD
# sudo is intentionally retained, so this exercises the path without isolating
# which rule grants it. The CA on stdin drives the best-effort trust step.
echo "validate-ca" | sudo -n "$HELPER" engage "$HOST" || fail "engage via sudo failed"
grep -qF "127.0.0.1 $HOST # runs-fleet-cache" /etc/hosts || fail "pin not present after engage"
sudo -n "$HELPER" disengage "$HOST" || fail "disengage via sudo failed"
if grep -qF "$HOST" /etc/hosts; then fail "pin not removed after disengage"; fi
sudo rm -f "$ANCHOR"
sudo update-ca-trust extract 2>/dev/null || true
echo "  OK: $SUDOERS"

echo "==> Agent AMI validation passed"
