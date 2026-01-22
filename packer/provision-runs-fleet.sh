#!/bin/bash
set -e

# Architecture detection (passed from Packer or default to arm64)
ARCH="${RUNNER_ARCH:-arm64}"
echo "==> Building for architecture: ${ARCH}"

echo "==> Creating GitHub Actions runner directory"
sudo mkdir -p /opt/actions-runner
sudo chown ec2-user:ec2-user /opt/actions-runner
cd /opt/actions-runner

RUNNER_VERSION="2.330.0"
echo "==> Downloading GitHub Actions runner v${RUNNER_VERSION} (${ARCH})"
curl -fsSL -o runner.tar.gz \
  "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${ARCH}-${RUNNER_VERSION}.tar.gz"
tar xzf runner.tar.gz
rm runner.tar.gz

echo "==> Installing runner dependencies (manual for AL2023)"
# installdependencies.sh doesn't recognize AL2023, install manually
sudo dnf install -y \
  icu \
  libicu \
  openssl-libs \
  krb5-libs \
  zlib \
  libgcc

echo "==> Installing development tools for CI workflows"
# Install full development toolchain for builds requiring compilation
sudo dnf groupinstall -y "Development Tools"
# Gold linker is built from source in base image for Go race detector support

echo "==> Creating runs-fleet agent directory"
sudo mkdir -p /opt/runs-fleet
sudo chown ec2-user:ec2-user /opt/runs-fleet

echo "==> Extracting agent binary from ECR image"
# Get AWS account ID and region
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
REGION="ap-northeast-1"
ECR_REGISTRY="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com"
RUNNER_IMAGE="${ECR_REGISTRY}/runs-fleet-runner:latest"

# Authenticate to ECR
aws ecr get-login-password --region ${REGION} | sudo docker login --username AWS --password-stdin ${ECR_REGISTRY}

# Translate runner arch to docker arch (x64 -> amd64)
DOCKER_ARCH="${ARCH}"
if [ "${ARCH}" = "x64" ]; then
  DOCKER_ARCH="amd64"
fi

# Pull the runner image for current architecture
sudo docker pull --platform linux/${DOCKER_ARCH} ${RUNNER_IMAGE}

# Extract agent binary from the image
CONTAINER_ID=$(sudo docker create ${RUNNER_IMAGE})
sudo docker cp ${CONTAINER_ID}:/usr/local/bin/runs-fleet-agent /opt/runs-fleet/runs-fleet-agent
sudo docker rm ${CONTAINER_ID}

sudo chmod +x /opt/runs-fleet/runs-fleet-agent
sudo chown ec2-user:ec2-user /opt/runs-fleet/runs-fleet-agent

# Cleanup Docker image to save space
sudo docker rmi ${RUNNER_IMAGE} || true

echo "==> Creating runs-fleet agent systemd service"
sudo tee /etc/systemd/system/runs-fleet-agent.service > /dev/null <<'EOF'
[Unit]
Description=runs-fleet Agent
After=network-online.target amazon-ssm-agent.service docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
User=ec2-user
Group=ec2-user
WorkingDirectory=/opt/runs-fleet
EnvironmentFile=/opt/runs-fleet/env
ExecStart=/opt/runs-fleet/runs-fleet-agent
Restart=no
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

echo "==> Creating agent bootstrap script"
sudo tee /opt/runs-fleet/bootstrap.sh > /dev/null <<'BOOTSTRAP'
#!/bin/bash
set -e

# Fetch instance metadata (IMDSv2)
TOKEN=$(curl -sX PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 300")
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)
REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region)

# Helper to get instance tag value
get_tag() {
  local tag_name="$1"
  aws ec2 describe-tags \
    --region "${REGION}" \
    --filters "Name=resource-id,Values=${INSTANCE_ID}" "Name=key,Values=${tag_name}" \
    --query 'Tags[0].Value' \
    --output text 2>/dev/null | grep -v "^None$" || true
}

# Check secrets backend from instance tags
SECRETS_BACKEND=$(get_tag "runs-fleet:secrets-backend")

if [ "$SECRETS_BACKEND" = "vault" ]; then
  echo "Using Vault secrets backend"

  # Read Vault configuration from instance tags
  VAULT_ADDR=$(get_tag "runs-fleet:vault-addr")
  VAULT_KV_MOUNT=$(get_tag "runs-fleet:vault-kv-mount")
  VAULT_KV_VERSION=$(get_tag "runs-fleet:vault-kv-version")
  VAULT_BASE_PATH=$(get_tag "runs-fleet:vault-base-path")
  VAULT_AUTH_METHOD=$(get_tag "runs-fleet:vault-auth-method")
  VAULT_AWS_ROLE=$(get_tag "runs-fleet:vault-aws-role")

  # Validate required Vault config (must be https://hostname or https://hostname:port, no path)
  if [ -z "$VAULT_ADDR" ] || [[ ! "$VAULT_ADDR" =~ ^https://[a-zA-Z0-9.-]+(:[0-9]+)?$ ]]; then
    echo "ERROR: Invalid or missing VAULT_ADDR (must be https://hostname or https://hostname:port)"
    exit 1
  fi
  if [ -z "$VAULT_AWS_ROLE" ] || [ -z "$VAULT_KV_MOUNT" ] || [ -z "$VAULT_BASE_PATH" ]; then
    echo "ERROR: Missing required Vault configuration (VAULT_AWS_ROLE, VAULT_KV_MOUNT, or VAULT_BASE_PATH)"
    exit 1
  fi

  # Authenticate with Vault using AWS IAM (write token to secure file)
  echo "Authenticating with Vault..."
  VAULT_TOKEN_FILE="/tmp/.vault-token-$$"
  touch "${VAULT_TOKEN_FILE}"
  chmod 600 "${VAULT_TOKEN_FILE}"
  vault login -method=aws \
    -address="${VAULT_ADDR}" \
    role="${VAULT_AWS_ROLE}" \
    -token-only 2>/dev/null > "${VAULT_TOKEN_FILE}"

  if [ ! -s "${VAULT_TOKEN_FILE}" ]; then
    rm "${VAULT_TOKEN_FILE}" 2>/dev/null || echo "WARNING: Failed to remove Vault token file"
    echo "ERROR: Failed to authenticate with Vault"
    exit 1
  fi

  # Fetch config from Vault
  VAULT_PATH="${VAULT_KV_MOUNT}/${VAULT_BASE_PATH}/${INSTANCE_ID}"
  if [ "$VAULT_KV_VERSION" = "1" ]; then
    VAULT_API_PATH="v1/${VAULT_KV_MOUNT}/${VAULT_BASE_PATH}/${INSTANCE_ID}"
  else
    VAULT_API_PATH="v1/${VAULT_KV_MOUNT}/data/${VAULT_BASE_PATH}/${INSTANCE_ID}"
  fi

  echo "Fetching config from Vault: ${VAULT_PATH}"

  for i in {1..10}; do
    RESPONSE=$(curl -sf -H @- "${VAULT_ADDR}/${VAULT_API_PATH}" 2>/dev/null \
      <<< "X-Vault-Token: $(cat "${VAULT_TOKEN_FILE}")") && break
    echo "Attempt $i: Waiting for Vault config..."
    sleep 3
  done

  rm "${VAULT_TOKEN_FILE}" 2>/dev/null || echo "WARNING: Failed to remove Vault token file"

  if [ -z "$RESPONSE" ]; then
    echo "ERROR: Failed to fetch config from Vault"
    exit 1
  fi

  # Extract config (KV v1 vs v2 have different response structures)
  if [ "$VAULT_KV_VERSION" = "1" ]; then
    CONFIG=$(echo "$RESPONSE" | jq -re '.data') || { echo "ERROR: Failed to extract config from Vault response"; exit 1; }
  else
    CONFIG=$(echo "$RESPONSE" | jq -re '.data.data') || { echo "ERROR: Failed to extract config from Vault response"; exit 1; }
  fi
else
  # Default: Fetch configuration from SSM parameter
  SSM_PARAM="/runs-fleet/runners/${INSTANCE_ID}/config"
  echo "Fetching config from SSM: ${SSM_PARAM}"

  for i in {1..10}; do
    CONFIG=$(aws ssm get-parameter \
      --name "${SSM_PARAM}" \
      --with-decryption \
      --region "${REGION}" \
      --query 'Parameter.Value' \
      --output text 2>/dev/null) && break
    echo "Attempt $i: Waiting for SSM parameter..."
    sleep 3
  done

  if [ -z "$CONFIG" ] || [ "$CONFIG" == "null" ]; then
    echo "ERROR: Failed to fetch config from SSM parameter ${SSM_PARAM}"
    exit 1
  fi
fi

# Validate CONFIG contains valid JSON
if ! echo "$CONFIG" | jq empty 2>/dev/null; then
  echo "ERROR: Configuration is not valid JSON"
  exit 1
fi

# Parse config JSON and write environment file
cat > /opt/runs-fleet/env <<EOF
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
EOF

echo "Bootstrap complete"
BOOTSTRAP
sudo chmod +x /opt/runs-fleet/bootstrap.sh

echo "==> Creating cloud-init script for first boot"
sudo tee /var/lib/cloud/scripts/per-instance/runs-fleet-bootstrap.sh > /dev/null <<'CLOUDINIT'
#!/bin/bash
set -e

# Run bootstrap to fetch config from SSM
/opt/runs-fleet/bootstrap.sh

# Start the agent service (binary is pre-baked in AMI)
systemctl start runs-fleet-agent
CLOUDINIT
sudo chmod +x /var/lib/cloud/scripts/per-instance/runs-fleet-bootstrap.sh

echo "==> Updating CloudWatch agent config for runs-fleet"
sudo tee /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json > /dev/null <<'CWCONFIG'
{
  "agent": {
    "metrics_collection_interval": 60,
    "run_as_user": "root"
  },
  "logs": {
    "logs_collected": {
      "files": {
        "collect_list": [
          {
            "file_path": "/opt/actions-runner/_diag/*.log",
            "log_group_name": "/runs-fleet/runner",
            "log_stream_name": "{instance_id}/runner",
            "retention_in_days": 3
          },
          {
            "file_path": "/var/log/messages",
            "log_group_name": "/runs-fleet/runner",
            "log_stream_name": "{instance_id}/system",
            "retention_in_days": 3
          }
        ]
      }
    }
  },
  "metrics": {
    "namespace": "RunsFleet/Runner",
    "metrics_collected": {
      "cpu": {
        "measurement": ["cpu_usage_active"],
        "metrics_collection_interval": 60
      },
      "mem": {
        "measurement": ["mem_used_percent"],
        "metrics_collection_interval": 60
      },
      "disk": {
        "measurement": ["disk_used_percent"],
        "resources": ["/"],
        "metrics_collection_interval": 60
      }
    },
    "append_dimensions": {
      "InstanceId": "${aws:InstanceId}"
    }
  }
}
CWCONFIG

echo "==> Cleaning up"
sudo rm -rf /tmp/*

echo "==> runs-fleet runner AMI provisioning complete"
echo "    - GitHub Runner: v${RUNNER_VERSION}"
echo "    - Agent binary: extracted from ECR"
echo "    - Systemd service: runs-fleet-agent.service"
