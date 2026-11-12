#!/bin/bash
# Removes the kernel that `dnf upgrade --releasever=latest` superseded, so the
# Trivy gate does not read it from the RPM DB. `dnf remove --oldinstallonly`
# cannot do this: it always skips the running kernel, which on a build instance
# is the superseded one. Must run after every step that may load a kernel
# module, because it deletes the running kernel's /lib/modules; that is safe
# only because the image is snapshotted and never rebooted in place.
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

OLD=$(sudo dnf repoquery --installonly --latest-limit=-1 -q) \
  || fail "could not list installonly packages"
if [ -n "$OLD" ]; then
  echo "==> Removing superseded installonly packages:"
  echo "$OLD"
  # shellcheck disable=SC2086
  sudo dnf remove -y --setopt protect_running_kernel=false $OLD \
    || fail "could not remove superseded installonly packages"
else
  echo "==> No superseded installonly packages to remove"
fi

# AMI promotion has no boot-health gate, so refuse to snapshot an image whose
# default boot entry is not the one remaining, complete kernel.
KERNELS=$(rpm -q kernel --qf '%{VERSION}-%{RELEASE}.%{ARCH}\n') \
  || fail "no package named kernel is installed"
[ "$(wc -l <<<"$KERNELS")" -eq 1 ] \
  || fail "expected exactly one installed kernel, found: $(tr '\n' ' ' <<<"$KERNELS")"
DEFAULT=$(sudo grubby --default-kernel) \
  || fail "grubby could not resolve a default boot kernel"
[ "$DEFAULT" = "/boot/vmlinuz-${KERNELS}" ] \
  || fail "default boot kernel is ${DEFAULT}, expected /boot/vmlinuz-${KERNELS}"
for f in "$DEFAULT" "/boot/initramfs-${KERNELS}.img" "/lib/modules/${KERNELS}/modules.dep"; do
  sudo test -s "$f" || fail "${f} is missing or empty"
done
echo "==> Image will boot ${KERNELS}"
