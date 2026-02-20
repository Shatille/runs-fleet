#!/bin/bash
set -e
export PATH="/usr/local/bin:$PATH"

TOKEN=$(curl -sX PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 300")
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)
REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region)

get_tag() {
  local tag_name="$1"
  aws ec2 describe-tags \
    --region "${REGION}" \
    --filters "Name=resource-id,Values=${INSTANCE_ID}" "Name=key,Values=${tag_name}" \
    --query 'Tags[0].Value' \
    --output text 2>/dev/null | grep -v "^None$" || true
}

SECRETS_BACKEND=$(get_tag "runs-fleet:secrets-backend")

CONFIG=""

if [ "$SECRETS_BACKEND" = "vault" ]; then
  echo "Using Vault secrets backend"

  VAULT_ADDR=$(get_tag "runs-fleet:vault-addr")
  VAULT_KV_MOUNT=$(get_tag "runs-fleet:vault-kv-mount" | tr -cd 'a-zA-Z0-9/_-')
  VAULT_KV_VERSION=$(get_tag "runs-fleet:vault-kv-version")
  VAULT_BASE_PATH=$(get_tag "runs-fleet:vault-base-path" | tr -cd 'a-zA-Z0-9/_-')
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

  echo "Authenticating with Vault..."
  VAULT_TOKEN_FILE="/tmp/.vault-token-$$"
  VAULT_AUTH_ERR="/tmp/.vault-auth-err-$$"
  touch "${VAULT_TOKEN_FILE}"
  chmod 600 "${VAULT_TOKEN_FILE}"
  vault login -token-only -method=aws \
    -address="${VAULT_ADDR}" \
    role="${VAULT_AWS_ROLE}" \
    2>"${VAULT_AUTH_ERR}" > "${VAULT_TOKEN_FILE}"

  if [ ! -s "${VAULT_TOKEN_FILE}" ]; then
    echo "ERROR: Failed to authenticate with Vault"
    [ -s "${VAULT_AUTH_ERR}" ] && echo "Vault auth error: $(cat "${VAULT_AUTH_ERR}")"
    rm -f "${VAULT_TOKEN_FILE}" "${VAULT_AUTH_ERR}"
    exit 1
  fi
  rm -f "${VAULT_AUTH_ERR}"

  if [ "$VAULT_KV_VERSION" = "1" ]; then
    VAULT_API_PATH="v1/${VAULT_KV_MOUNT}/${VAULT_BASE_PATH}/${INSTANCE_ID}"
  else
    VAULT_API_PATH="v1/${VAULT_KV_MOUNT}/data/${VAULT_BASE_PATH}/${INSTANCE_ID}"
  fi

  VAULT_PATH="${VAULT_KV_MOUNT}/${VAULT_BASE_PATH}/${INSTANCE_ID}"
  echo "Fetching config from Vault: ${VAULT_PATH}"

  RESPONSE=""
  for i in {1..5}; do
    HTTP_CODE=$(curl -s -w "%{http_code}" -o /tmp/vault-resp-$$ \
      -H "X-Vault-Token: $(cat "${VAULT_TOKEN_FILE}")" \
      "${VAULT_ADDR}/${VAULT_API_PATH}" 2>/dev/null) || HTTP_CODE="000"

    if [ "$HTTP_CODE" = "200" ]; then
      RESPONSE=$(cat /tmp/vault-resp-$$)
      rm -f /tmp/vault-resp-$$
      break
    fi
    rm -f /tmp/vault-resp-$$

    if [ "$HTTP_CODE" = "404" ]; then
      rm -f "${VAULT_TOKEN_FILE}"
      echo "No job config found in Vault, instance in pool standby"
      exit 0
    fi

    echo "Attempt $i: Vault returned HTTP ${HTTP_CODE}, retrying..."
    sleep 2
  done

  rm -f "${VAULT_TOKEN_FILE}"

  if [ -z "$RESPONSE" ]; then
    echo "No job config found in Vault after retries, instance in pool standby"
    exit 0
  fi

  if [ "$VAULT_KV_VERSION" = "1" ]; then
    CONFIG=$(echo "$RESPONSE" | jq -re '.data // empty') || { echo "ERROR: Failed to extract config from Vault response"; exit 1; }
  else
    CONFIG=$(echo "$RESPONSE" | jq -re '.data.data // empty') || { echo "ERROR: Failed to extract config from Vault response"; exit 1; }
  fi

  if [ -z "$CONFIG" ] || [ "$CONFIG" = "null" ]; then
    echo "ERROR: Vault response contained no config data"
    exit 1
  fi
else
  SSM_PARAM="/runs-fleet/runners/${INSTANCE_ID}/config"
  echo "Checking SSM for config: ${SSM_PARAM}"

  for i in {1..5}; do
    RESULT=$(aws ssm get-parameter \
      --name "${SSM_PARAM}" \
      --with-decryption \
      --region "${REGION}" \
      --query 'Parameter.Value' \
      --output text 2>&1) && {
      if [ -n "$RESULT" ] && [ "$RESULT" != "null" ]; then
        CONFIG="$RESULT"
        break
      fi
    }
    if echo "$RESULT" | grep -q "ParameterNotFound"; then
      echo "No job config found, instance in pool standby"
      exit 0
    fi
    echo "Attempt $i: Waiting for SSM parameter..."
    sleep 2
  done

  if [ -z "$CONFIG" ] || [ "$CONFIG" == "null" ]; then
    echo "No job config found after retries, instance in pool standby"
    exit 0
  fi
fi

if ! echo "$CONFIG" | jq empty 2>/dev/null; then
  echo "ERROR: Configuration is not valid JSON"
  exit 1
fi

if [ "$SECRETS_BACKEND" = "vault" ]; then
  # Vault: agent can't re-authenticate, so pass all config via env vars
  cat > /opt/runs-fleet/env <<EOF
RUNS_FLEET_SECRETS_BACKEND=env
RUNS_FLEET_RUN_ID=$(echo "$CONFIG" | jq -r '.run_id')
RUNS_FLEET_JOB_ID=$(echo "$CONFIG" | jq -r '.job_id // empty')
RUNS_FLEET_INSTANCE_ID=${INSTANCE_ID}
AWS_REGION=${REGION}
RUNS_FLEET_CACHE_URL=$(echo "$CONFIG" | jq -r '.cache_url // empty')
RUNS_FLEET_CACHE_TOKEN=$(echo "$CONFIG" | jq -r '.cache_token // empty')
RUNS_FLEET_JIT_TOKEN=$(echo "$CONFIG" | jq -r '.jit_token')
RUNS_FLEET_ORG=$(echo "$CONFIG" | jq -r '.org')
RUNS_FLEET_REPO=$(echo "$CONFIG" | jq -r '.repo // empty')
RUNS_FLEET_LABELS=$(echo "$CONFIG" | jq -r '.labels | if type == "array" then join(",") else . end')
RUNS_FLEET_TERMINATION_QUEUE_URL=$(echo "$CONFIG" | jq -r '.termination_queue_url // empty')
RUNS_FLEET_RUNNER_NAME=$(echo "$CONFIG" | jq -r '.runner_name // empty')
RUNS_FLEET_IS_ORG=$(echo "$CONFIG" | jq -r '.is_org // false')
EOF
else
  # SSM: agent reads full config from SSM directly (preserves all fields including runner_name)
  cat > /opt/runs-fleet/env <<EOF
RUNS_FLEET_INSTANCE_ID=${INSTANCE_ID}
AWS_REGION=${REGION}
EOF
fi

systemctl start runs-fleet-agent
echo "Agent service started for instance ${INSTANCE_ID}"
