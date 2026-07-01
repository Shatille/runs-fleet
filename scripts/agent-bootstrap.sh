#!/bin/bash
set -e
export PATH="/usr/local/bin:$PATH"
# shellcheck source-path=SCRIPTDIR source=boot-lib.sh
source /opt/runs-fleet/boot-lib.sh

TOKEN=$(imds_token) || { echo "ERROR: Failed to fetch IMDSv2 token"; exit 1; }
INSTANCE_ID=$(imds_get meta-data/instance-id "$TOKEN") || { echo "ERROR: Failed to fetch instance-id"; exit 1; }
REGION=$(imds_get meta-data/placement/region "$TOKEN") || { echo "ERROR: Failed to fetch region"; exit 1; }

# Fail closed: a failed DescribeTags must not be read as "no backend tag" and
# silently fall through to the SSM default, booting the agent on the wrong
# backend. A genuinely absent tag (empty value) legitimately means "use SSM".
if ! SECRETS_BACKEND=$(get_tag "runs-fleet:secrets-backend"); then
  echo "ERROR: Failed to read secrets-backend tag (DescribeTags failed)"
  exit 1
fi

# The agent reads its full runner config directly from the configured secrets
# backend (Vault via AWS-IMDS auth, or SSM directly). The bootstrap only provides
# backend selection + connection params — no config fetch, no secrets on disk.
# Warm-pool standby (booted before a job is assigned) is handled by the agent,
# which exits cleanly when its config is absent.

if [ "$SECRETS_BACKEND" = "vault" ]; then
  echo "Using Vault secrets backend"

  # Required Vault params fail closed on a DescribeTags error (a genuinely empty
  # value is caught by the validation below); optional params tolerate a read
  # failure and let the agent fall back to its defaults.
  if ! VAULT_ADDR=$(get_tag "runs-fleet:vault-addr"); then
    echo "ERROR: Failed to read vault-addr tag (DescribeTags failed)"; exit 1
  fi
  if ! VAULT_KV_MOUNT=$(get_tag "runs-fleet:vault-kv-mount"); then
    echo "ERROR: Failed to read vault-kv-mount tag (DescribeTags failed)"; exit 1
  fi
  VAULT_KV_MOUNT=$(printf '%s' "$VAULT_KV_MOUNT" | tr -cd 'a-zA-Z0-9/_-')
  if [[ "$VAULT_KV_MOUNT" =~ \.\. ]]; then
    echo "ERROR: Invalid VAULT_KV_MOUNT: contains path traversal"
    exit 1
  fi
  if ! VAULT_BASE_PATH=$(get_tag "runs-fleet:vault-base-path"); then
    echo "ERROR: Failed to read vault-base-path tag (DescribeTags failed)"; exit 1
  fi
  VAULT_BASE_PATH=$(printf '%s' "$VAULT_BASE_PATH" | tr -cd 'a-zA-Z0-9/_-')
  if [[ "$VAULT_BASE_PATH" =~ \.\. ]]; then
    echo "ERROR: Invalid VAULT_BASE_PATH: contains path traversal"
    exit 1
  fi
  if ! VAULT_AWS_ROLE=$(get_tag "runs-fleet:vault-aws-role"); then
    echo "ERROR: Failed to read vault-aws-role tag (DescribeTags failed)"; exit 1
  fi
  VAULT_KV_VERSION=$(get_tag "runs-fleet:vault-kv-version" || true)
  VAULT_AUTH_METHOD=$(get_tag "runs-fleet:vault-auth-method" || true)

  if [ -z "$VAULT_ADDR" ] || [[ ! "$VAULT_ADDR" =~ ^https://[a-zA-Z0-9.-]+(:[0-9]+)?$ ]]; then
    echo "ERROR: Invalid or missing VAULT_ADDR (must be https://hostname or https://hostname:port)"
    exit 1
  fi
  if [ -z "$VAULT_AWS_ROLE" ] || [ -z "$VAULT_KV_MOUNT" ] || [ -z "$VAULT_BASE_PATH" ]; then
    echo "ERROR: Missing required Vault configuration (VAULT_AWS_ROLE, VAULT_KV_MOUNT, or VAULT_BASE_PATH)"
    exit 1
  fi

  # Connection params only; VAULT_KV_VERSION omitted when unset so the agent
  # auto-detects (matches the orchestrator's kvVersion=0 default).
  {
    echo "RUNS_FLEET_SECRETS_BACKEND=vault"
    echo "RUNS_FLEET_INSTANCE_ID=${INSTANCE_ID}"
    echo "AWS_REGION=${REGION}"
    echo "VAULT_ADDR=${VAULT_ADDR}"
    echo "VAULT_KV_MOUNT=${VAULT_KV_MOUNT}"
    echo "VAULT_BASE_PATH=${VAULT_BASE_PATH}"
    if [ -n "$VAULT_AUTH_METHOD" ]; then
      echo "VAULT_AUTH_METHOD=${VAULT_AUTH_METHOD}"
    fi
    echo "VAULT_AWS_ROLE=${VAULT_AWS_ROLE}"
    echo "VAULT_AWS_REGION=${REGION}"
    if [ -n "$VAULT_KV_VERSION" ]; then
      echo "VAULT_KV_VERSION=${VAULT_KV_VERSION}"
    fi
  } > /opt/runs-fleet/env
else
  echo "Using SSM secrets backend"
  cat > /opt/runs-fleet/env <<EOF
RUNS_FLEET_INSTANCE_ID=${INSTANCE_ID}
AWS_REGION=${REGION}
EOF
fi

# Scrub any stale cache-interceptor /etc/hosts pin left by a prior boot (e.g. an
# agent SIGKILLed before teardown on a reused warm-pool instance) so the results
# host resolves to the real GitHub IP on this fresh boot, not loopback.
sed -i '/# runs-fleet-cache/d' /etc/hosts

# If a stop is already in progress (a warm-pool spare being banked), starting the
# agent now would collide with the queued shutdown (systemd destructive
# transaction). Bow out cleanly so the boot shim doesn't read it as a failure; any
# stop that begins after this check is caught by the shim's own guard.
if system_is_stopping; then
  echo "System is shutting down; skipping agent service start"
  exit 0
fi

systemctl start runs-fleet-agent
if systemctl is-failed --quiet runs-fleet-agent; then
  echo "ERROR: runs-fleet-agent service failed to start"
  exit 1
fi
echo "Agent service started for instance ${INSTANCE_ID}"
