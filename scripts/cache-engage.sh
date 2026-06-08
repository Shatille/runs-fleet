#!/bin/bash
# Root-owned cache-interceptor engage/disengage helper, baked into the runner
# AMI and invoked by the (ec2-user) agent through a scoped sudoers rule
# (/etc/sudoers.d/10-runs-fleet-cache). It performs ONLY the two root-only steps
# of cache engagement: the system trust anchor (best-effort) and the /etc/hosts
# pin. It self-validates its arguments so the NOPASSWD grant cannot be turned
# into an arbitrary-write primitive: the host is fixed, and the CA PEM is read
# from stdin (never a caller-named path).
set -euo pipefail

readonly HOSTS_FILE="/etc/hosts"
readonly HOSTS_TAG="# runs-fleet-cache"
readonly ANCHOR="/etc/pki/ca-trust/source/anchors/runs-fleet-cache-ca.crt"
# The only host this helper will ever pin. Must match cacheproxy.DefaultResultsHost.
readonly EXPECTED_HOST="results-receiver.actions.githubusercontent.com"

die() { echo "cache-engage: $*" >&2; exit 1; }

require_expected_host() {
  [ "${1:-}" = "$EXPECTED_HOST" ] || die "refusing unexpected host: ${1:-<empty>}"
}

# install_anchor trusts the per-instance CA (read from stdin) system-wide. It is
# best-effort: NODE_EXTRA_CA_CERTS already covers the Node v2 cache client, so a
# trust-store failure must never block the pin or fail the job.
install_anchor() {
  if [ -t 0 ]; then
    echo "cache-engage: no CA on stdin (skipping trust)" >&2
    return 0
  fi
  local tmp
  tmp="$(mktemp)" || { echo "cache-engage: mktemp failed (skipping trust)" >&2; return 0; }
  # A truncated/malformed write lands in $tmp; update-ca-trust discards invalid
  # PEM, so best-effort trust degrades to a no-op rather than a bad anchor.
  cat > "$tmp" || true
  if [ ! -s "$tmp" ]; then
    rm -f "$tmp"
    echo "cache-engage: empty CA on stdin (skipping trust)" >&2
    return 0
  fi
  install -m 0644 -o root -g root "$tmp" "$ANCHOR" 2>/dev/null \
    || echo "cache-engage: anchor install failed (continuing)" >&2
  rm -f "$tmp"
  update-ca-trust extract 2>/dev/null \
    || echo "cache-engage: update-ca-trust failed (continuing)" >&2
  return 0
}

# pin_hosts redirects the results host to the local interceptor. Idempotent.
pin_hosts() {
  local host="$1"
  # Match the full pin (IP included) so a stale wrong-IP line doesn't masquerade
  # as our pin and silently leave the host pointing somewhere other than the
  # local interceptor.
  if grep -qF "127.0.0.1 $host $HOSTS_TAG" "$HOSTS_FILE"; then
    return 0
  fi
  printf '127.0.0.1 %s %s\n' "$host" "$HOSTS_TAG" >> "$HOSTS_FILE"
}

unpin_hosts() {
  sed -i "/${HOSTS_TAG}/d" "$HOSTS_FILE"
}

remove_anchor() {
  rm -f "$ANCHOR" 2>/dev/null || true
  update-ca-trust extract 2>/dev/null || true
  return 0
}

main() {
  local cmd="${1:-}"
  case "$cmd" in
    engage)
      require_expected_host "${2:-}"
      install_anchor    # reads CA PEM from stdin (best-effort)
      pin_hosts "$2"    # load-bearing step — must succeed
      ;;
    disengage)
      require_expected_host "${2:-}"
      unpin_hosts
      remove_anchor
      ;;
    *)
      die "unknown subcommand: ${cmd:-<empty>}"
      ;;
  esac
}

main "$@"
