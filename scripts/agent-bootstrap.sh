#!/bin/bash
set -e
export PATH="/usr/local/bin:$PATH"

TOKEN=$(curl -sfX PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 300") || { echo "ERROR: Failed to fetch IMDSv2 token"; exit 1; }
INSTANCE_ID=$(curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id) || { echo "ERROR: Failed to fetch instance-id"; exit 1; }
REGION=$(curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region) || { echo "ERROR: Failed to fetch region"; exit 1; }

get_tag() {
  local tag_name="$1"
  local result
  if ! result=$(aws ec2 describe-tags \
    --region "${REGION}" \
    --filters "Name=resource-id,Values=${INSTANCE_ID}" "Name=key,Values=${tag_name}" \
    --query 'Tags[0].Value' \
    --output text 2>/tmp/get-tag-err-$$); then
    [ -s /tmp/get-tag-err-$$ ] && echo "WARN: Failed to fetch tag ${tag_name}: $(cat /tmp/get-tag-err-$$)" >&2
    rm -f /tmp/get-tag-err-$$
    return 0
  fi
  rm -f /tmp/get-tag-err-$$
  echo "$result" | grep -v "^None$" || true
}

SECRETS_BACKEND=$(get_tag "runs-fleet:secrets-backend")

# The agent reads its full runner config directly from the configured secrets
# backend (Vault via AWS-IMDS auth, or SSM directly). The bootstrap only provides
# backend selection + connection params — no config fetch, no secrets on disk.
# Warm-pool standby (booted before a job is assigned) is handled by the agent,
# which exits cleanly when its config is absent.

if [ "$SECRETS_BACKEND" = "vault" ]; then
  echo "Using Vault secrets backend"

  VAULT_ADDR=$(get_tag "runs-fleet:vault-addr")
  VAULT_KV_MOUNT=$(get_tag "runs-fleet:vault-kv-mount" | tr -cd 'a-zA-Z0-9/_-')
  if [[ "$VAULT_KV_MOUNT" =~ \.\. ]]; then
    echo "ERROR: Invalid VAULT_KV_MOUNT: contains path traversal"
    exit 1
  fi
  VAULT_KV_VERSION=$(get_tag "runs-fleet:vault-kv-version")
  VAULT_BASE_PATH=$(get_tag "runs-fleet:vault-base-path" | tr -cd 'a-zA-Z0-9/_-')
  if [[ "$VAULT_BASE_PATH" =~ \.\. ]]; then
    echo "ERROR: Invalid VAULT_BASE_PATH: contains path traversal"
    exit 1
  fi
  VAULT_AUTH_METHOD=$(get_tag "runs-fleet:vault-auth-method")
  VAULT_AWS_ROLE=$(get_tag "runs-fleet:vault-aws-role")

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

systemctl start runs-fleet-agent
if systemctl is-failed --quiet runs-fleet-agent; then
  echo "ERROR: runs-fleet-agent service failed to start"
  exit 1
fi
echo "Agent service started for instance ${INSTANCE_ID}"
