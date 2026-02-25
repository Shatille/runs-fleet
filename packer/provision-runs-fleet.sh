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

echo "==> Installing Java (required for sbt)"
sudo dnf install -y java-21-amazon-corretto-headless || { echo "Java installation failed"; exit 1; }

echo "==> Installing sbt"
SBT_VERSION="1.10.7"
SBT_SHA256="32c15233c636c233ee25a2c31879049db7021cfef70807c187515c39b96b0fe6"
curl -fsSL --max-time 300 -o /tmp/sbt.tgz "https://github.com/sbt/sbt/releases/download/v${SBT_VERSION}/sbt-${SBT_VERSION}.tgz" \
  || { echo "sbt download failed"; exit 1; }
echo "${SBT_SHA256}  /tmp/sbt.tgz" | sha256sum -c || { echo "sbt checksum verification failed"; rm /tmp/sbt.tgz; exit 1; }
sudo tar xzf /tmp/sbt.tgz -C /usr/local || { echo "sbt extraction failed"; rm /tmp/sbt.tgz; exit 1; }
sudo ln -sf /usr/local/sbt/bin/sbt /usr/local/bin/sbt || { echo "sbt symlink creation failed"; exit 1; }
rm /tmp/sbt.tgz

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

echo "==> Installing agent bootstrap script"
sudo cp /tmp/agent-bootstrap.sh /opt/runs-fleet/agent-bootstrap.sh
sudo chmod +x /opt/runs-fleet/agent-bootstrap.sh
sudo chown ec2-user:ec2-user /opt/runs-fleet/agent-bootstrap.sh

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
echo "    - sbt: v${SBT_VERSION}"
