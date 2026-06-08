#!/bin/bash
set -e

# Translate the kernel arch into Docker's platform naming for the ECR pull.
# The actions/runner binary itself, its OS deps, and the language toolchains
# now live in the base AMI (see packer/provision-base.sh).
case "$(uname -m)" in
  x86_64)  DOCKER_ARCH="amd64" ;;
  aarch64) DOCKER_ARCH="arm64" ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
echo "==> Building runs-fleet layer for ${DOCKER_ARCH}"

echo "==> Creating runs-fleet agent directory"
sudo mkdir -p /opt/runs-fleet
sudo chown ec2-user:ec2-user /opt/runs-fleet

echo "==> Extracting agent binary from ECR image"
# Get AWS account ID and region
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
REGION="ap-northeast-1"
ECR_REGISTRY="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com"
ECR_REPOSITORY="${ECR_REPOSITORY:-runs-fleet-runner}"
RUNNER_IMAGE="${ECR_REGISTRY}/${ECR_REPOSITORY}:latest"

# Authenticate to ECR
aws ecr get-login-password --region ${REGION} | sudo docker login --username AWS --password-stdin ${ECR_REGISTRY}

# Pull the runner image for current architecture
sudo docker pull --platform linux/${DOCKER_ARCH} ${RUNNER_IMAGE}

# Extract agent binary from the image
CONTAINER_ID=$(sudo docker create ${RUNNER_IMAGE})
sudo docker cp ${CONTAINER_ID}:/usr/local/bin/runs-fleet-agent /opt/runs-fleet/runs-fleet-agent
sudo docker rm ${CONTAINER_ID}

sudo chmod +x /opt/runs-fleet/runs-fleet-agent
sudo chown ec2-user:ec2-user /opt/runs-fleet/runs-fleet-agent

# The transparent cache interceptor binds 127.0.0.1:443 (the results host is
# pinned there); grant the agent CAP_NET_BIND_SERVICE so it can bind a low port
# without running as root.
echo "==> Granting agent CAP_NET_BIND_SERVICE for the cache interceptor"
sudo setcap 'cap_net_bind_service=+ep' /opt/runs-fleet/runs-fleet-agent
# Full engagement also edits /etc/hosts (pin the results host) and the system
# trust store — both root-only. Rather than run the agent as root, bake a
# root-owned engage helper and grant ec2-user a scoped NOPASSWD sudoers rule for
# exactly that helper; the agent (ec2-user) invokes it after its listener is
# healthy and removes the pin on teardown, so fail-open is preserved. The helper
# lives in /usr/local/sbin (root-owned), NOT under /opt/runs-fleet (ec2-user-owned),
# so it can't be swapped out to escalate through the sudoers grant.
echo "==> Installing root-owned cache-engage helper"
sudo install -m 0755 -o root -g root /tmp/cache-engage.sh /usr/local/sbin/runs-fleet-cache-engage

echo "==> Installing scoped sudoers drop-in for cache-engage"
sudo install -m 0440 -o root -g root /tmp/sudoers-cache-engage /etc/sudoers.d/10-runs-fleet-cache
# Fail the build (before snapshot) on a malformed drop-in rather than at boot.
sudo visudo -cf /etc/sudoers.d/10-runs-fleet-cache

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

echo "==> Installing cloud-init per-boot script"
sudo mkdir -p /var/lib/cloud/scripts/per-boot
sudo cp /tmp/cloud-init-boot.sh /var/lib/cloud/scripts/per-boot/runs-fleet-bootstrap.sh
sudo chmod +x /var/lib/cloud/scripts/per-boot/runs-fleet-bootstrap.sh

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
echo "    - Agent binary: extracted from ECR"
echo "    - Systemd service: runs-fleet-agent.service"
