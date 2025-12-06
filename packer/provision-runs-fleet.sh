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
curl -fsSL -o checksums.txt \
  "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${ARCH}-${RUNNER_VERSION}.tar.gz.sha256"
echo "$(cat checksums.txt)  runner.tar.gz" | sha256sum -c -
tar xzf runner.tar.gz
rm runner.tar.gz checksums.txt

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

# Pull the runner image for current architecture
sudo docker pull --platform linux/${ARCH} ${RUNNER_IMAGE}

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

# Fetch configuration from SSM parameter (set by orchestrator)
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

# Parse config JSON and write environment file
cat > /opt/runs-fleet/env <<EOF
RUNS_FLEET_RUN_ID=$(echo "$CONFIG" | jq -r '.run_id')
RUNS_FLEET_JOB_ID=$(echo "$CONFIG" | jq -r '.job_id // empty')
RUNS_FLEET_INSTANCE_ID=${INSTANCE_ID}
AWS_REGION=${REGION}
RUNS_FLEET_SSM_PARAMETER=${SSM_PARAM}
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
