#!/usr/bin/env bash
# Trivy CVE gate, scoped to what runs-fleet can actually remediate.
#
# Reads a Trivy JSON report ($1, or stdin) — already filtered by
# .trivy/trivy.yaml (severity) and .trivy/vex.json (per-CVE not-affected) — and
# fails ONLY on findings we can act on:
#
#   - OS / apt packages    (Class == "os-pkgs")      -> fixed by apt-get upgrade
#   - our own agent binary (Target ~ runs-fleet-agent) -> fixed via go.mod
#
# Findings that live ONLY in third-party prebuilt binaries we install but do not
# build (Docker/containerd cli-plugins, dockerd) or in upstream-bundled
# node_modules (npm's vendored libs) are REPORTED but do NOT fail the gate: we
# cannot rebuild them, so failing would perpetually block image promotion and
# the AMI rebuild until the upstream project republishes. They clear on their
# own once upstream ships a fixed build. See docker/runner/CLAUDE.md.
#
# This is target-scoped, not CVE-id-scoped: a NEW CVE in our agent or an OS
# package still fails the gate. It is not a .trivyignore.
set -euo pipefail

# Buffer the report to a temp file (jq reads it directly, twice) rather than
# slurping it into a shell variable — reports can be large, and this also lets
# the stdin form be read more than once.
report_file="$(mktemp)"
trap 'rm -f "$report_file"' EXIT
cat "${1:-/dev/stdin}" > "$report_file"

# Print "  <target>: <CVE> <severity> <pkg>@<version> (fixed: <fix>)" for every
# vulnerability in Results matching the jq selector passed as $1.
findings() {
  local prog
  prog="$(printf '.Results[] | select(.Vulnerabilities) | %s | .Target as $t | .Vulnerabilities[] | "  \\($t): \\(.VulnerabilityID) \\(.Severity) \\(.PkgName)@\\(.InstalledVersion) (fixed: \\(.FixedVersion // "n/a"))"' "$1")"
  jq -r "$prog" "$report_file"
}

controlled='select(.Class == "os-pkgs" or (.Target | test("runs-fleet-agent")))'
third_party='select((.Class == "os-pkgs" or (.Target | test("runs-fleet-agent"))) | not)'

echo "== Third-party / upstream-bundled CVEs (reported, non-blocking) =="
findings "$third_party" || true
echo ""
echo "== Controllable CVEs (BLOCKING — OS/apt packages or runs-fleet-agent) =="
blocking="$(findings "$controlled")"
if [ -n "$blocking" ]; then
  echo "$blocking"
  echo ""
  echo "GATE: FAIL — remediable HIGH/CRITICAL findings present (bump the apt package or go.mod dependency)."
  exit 1
fi
echo "  (none)"
echo ""
echo "GATE: PASS — no remediable HIGH/CRITICAL findings. Non-blocking upstream findings above (if any) await upstream rebuilds."
