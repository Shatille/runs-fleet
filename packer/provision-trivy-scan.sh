#!/bin/bash
# Vulnerability scan for the Packer-built filesystem before AMI snapshot.
# Reuses the same Trivy version, config, and VEX file as the container path
# (.trivy/trivy.yaml and .trivy/vex.json, uploaded to /tmp/ by Packer).
# Failure here aborts the Packer build before an AMI is registered.
set -e

TRIVY_VERSION="0.70.0"

ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  TRIVY_ARCH="64bit" ;;
  aarch64) TRIVY_ARCH="ARM64" ;;
  *) echo "Unsupported arch for Trivy: $ARCH" >&2; exit 1 ;;
esac

echo "==> Installing Trivy v${TRIVY_VERSION} (${TRIVY_ARCH})"
TRIVY_TARBALL="trivy_${TRIVY_VERSION}_Linux-${TRIVY_ARCH}.tar.gz"
TRIVY_URL="https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}"

# --retry-all-errors so a transient GitHub release-CDN error (e.g. a 504 on the
# tarball, which we have observed) is retried rather than failing the AMI build.
curl -sfL --retry 5 --retry-delay 3 --retry-all-errors "${TRIVY_URL}/${TRIVY_TARBALL}" -o "/tmp/${TRIVY_TARBALL}" \
  || { echo "Failed to download Trivy"; exit 1; }
curl -sfL --retry 5 --retry-delay 3 --retry-all-errors "${TRIVY_URL}/trivy_${TRIVY_VERSION}_checksums.txt" \
  -o /tmp/trivy_checksums.txt \
  || { echo "Failed to download Trivy checksums"; exit 1; }

CHECKSUM_LINE=$(grep "${TRIVY_TARBALL}\$" /tmp/trivy_checksums.txt) \
  || { echo "Trivy checksum line not found"; exit 1; }
[[ "$CHECKSUM_LINE" =~ ^[a-f0-9]{64}[[:space:]]+trivy_ ]] \
  || { echo "Invalid Trivy checksum format"; exit 1; }
(cd /tmp && echo "$CHECKSUM_LINE" | sha256sum -c) \
  || { echo "Trivy checksum verification failed"; rm -f "/tmp/${TRIVY_TARBALL}" /tmp/trivy_checksums.txt; exit 1; }

sudo tar -xzf "/tmp/${TRIVY_TARBALL}" -C /usr/local/bin trivy \
  || { echo "Trivy extraction failed"; exit 1; }
rm -f "/tmp/${TRIVY_TARBALL}" /tmp/trivy_checksums.txt
sudo chmod 0755 /usr/local/bin/trivy

[ -f /tmp/trivy.yaml ] || { echo "Missing /tmp/trivy.yaml (Packer file provisioner)"; exit 1; }
[ -f /tmp/vex.json ]   || { echo "Missing /tmp/vex.json (Packer file provisioner)"; exit 1; }

[ -f /tmp/gate.sh ] || { echo "Missing /tmp/gate.sh (Packer file provisioner)"; exit 1; }

echo "==> Scanning filesystem for HIGH/CRITICAL CVEs (severity / pkg types from .trivy/trivy.yaml)"
# --ignore-unfixed: don't fail on CVEs with no upstream fix available
# --scanners vuln: skip secrets/misconfig (those would need separate baselines)
# --skip-dirs: avoid noise from runtime/cache directories that don't represent
#   packages baked into the AMI. /var/lib/docker holds pre-baked image layers
#   (see provision-base.sh) — runtime artifacts scanned upstream by their
#   publishers, not host software; without the skip their nested package DBs
#   and Go binaries would flood (or block) the gate. /opt/hostedtoolcache holds
#   pre-baked toolchain distributions that setup-go/setup-node would otherwise
#   download at job time; the only hits there are go.mod files inside the Go
#   toolchains' own source trees (x/crypto pins for internal codegen tools,
#   the stdlib's vendored x/net) — even the newest patched Go release flags,
#   so no baked version can ever scan clean.
# No --exit-code here: the report is emitted as JSON and gate.sh decides
# pass/fail, failing only on findings we can remediate (OS/apt packages, our
# own agent binary). CVEs that live only in third-party prebuilt binaries
# (Docker/containerd, npm-bundled libs) are reported but non-blocking.
sudo trivy filesystem \
  --config /tmp/trivy.yaml \
  --vex /tmp/vex.json \
  --ignore-unfixed \
  --no-progress \
  --scanners vuln \
  --format json \
  --output /tmp/trivy-report.json \
  --skip-dirs /proc \
  --skip-dirs /sys \
  --skip-dirs /dev \
  --skip-dirs /run \
  --skip-dirs /var/cache \
  --skip-dirs /var/lib/docker \
  --skip-dirs /opt/hostedtoolcache \
  --skip-dirs /root/.cache \
  --skip-dirs /home/ec2-user/.cache \
  /

echo "==> Applying CVE gate (/tmp/gate.sh)"
sudo bash /tmp/gate.sh /tmp/trivy-report.json

echo "==> Removing Trivy and uploaded config files to keep AMI lean"
sudo rm -rf /usr/local/bin/trivy \
            /root/.cache/trivy \
            /home/ec2-user/.cache/trivy \
            /tmp/trivy.yaml \
            /tmp/vex.json \
            /tmp/gate.sh \
            /tmp/trivy-report.json

echo "==> Trivy gate passed; AMI has no remediable HIGH/CRITICAL CVEs"
