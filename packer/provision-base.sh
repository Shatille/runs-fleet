#!/bin/bash
set -e

# Detect architecture
ARCH=$(uname -m)
if [ "$ARCH" = "x86_64" ]; then
  DOCKER_ARCH="x86_64"
  COMPOSE_ARCH="x86_64"
else
  DOCKER_ARCH="aarch64"
  COMPOSE_ARCH="aarch64"
fi
echo "==> Detected architecture: ${ARCH} (Docker: ${DOCKER_ARCH})"

echo "==> Installing base packages"
sudo dnf install -y \
  git \
  tar \
  jq \
  make \
  libicu \
  unzip \
  awscli \
  docker \
  amazon-ssm-agent \
  amazon-cloudwatch-agent

# Gold linker only needed for ARM64 (Go race detector compatibility)
if [ "$ARCH" = "aarch64" ]; then
  echo "==> Installing build dependencies for gold linker"
  sudo dnf install -y \
    gcc \
    gcc-c++ \
    texinfo \
    bison \
    flex \
    zlib-devel

  echo "==> Building gold linker from source (required for Go race detector on ARM64)"
  BINUTILS_VERSION="2.43"
  cd /tmp
  curl -sL "https://ftp.gnu.org/gnu/binutils/binutils-${BINUTILS_VERSION}.tar.xz" -o binutils.tar.xz
  tar xf binutils.tar.xz
  cd binutils-${BINUTILS_VERSION}
  mkdir build && cd build
  ../configure --enable-gold --enable-plugins --prefix=/usr/local
  make -j$(nproc) all-gold
  sudo make install-gold
  sudo ln -sf /usr/local/bin/ld.gold /usr/bin/ld.gold
  cd /tmp && rm -rf binutils*

  echo "==> Verifying gold linker installation"
  /usr/bin/ld.gold --version | head -1
else
  echo "==> Skipping gold linker (not required for amd64)"
fi

echo "==> Enabling SSM agent"
sudo systemctl enable amazon-ssm-agent

echo "==> Enabling Docker"
sudo systemctl enable docker
sudo usermod -aG docker ec2-user

echo "==> Installing Docker Compose (${COMPOSE_ARCH})"
DOCKER_COMPOSE_VERSION="2.24.5"
COMPOSE_BINARY="docker-compose-linux-${COMPOSE_ARCH}"
COMPOSE_URL="https://github.com/docker/compose/releases/download/v${DOCKER_COMPOSE_VERSION}"
# Download and verify checksum (filenames must match for sha256sum -c)
curl -sfL "${COMPOSE_URL}/${COMPOSE_BINARY}.sha256" -o "/tmp/${COMPOSE_BINARY}.sha256" \
  || { echo "Failed to download Docker Compose checksum"; exit 1; }
curl -sfL "${COMPOSE_URL}/${COMPOSE_BINARY}" -o "/tmp/${COMPOSE_BINARY}" \
  || { echo "Failed to download Docker Compose"; exit 1; }
cd /tmp && sha256sum -c "${COMPOSE_BINARY}.sha256" \
  || { echo "Docker Compose checksum mismatch"; rm -f "/tmp/${COMPOSE_BINARY}" "/tmp/${COMPOSE_BINARY}.sha256"; exit 1; }
# Install as standalone binary (docker-compose)
sudo mv "/tmp/${COMPOSE_BINARY}" /usr/local/bin/docker-compose
sudo chmod +x /usr/local/bin/docker-compose
# Install as Docker CLI plugin (docker compose)
sudo mkdir -p /usr/local/lib/docker/cli-plugins
sudo cp /usr/local/bin/docker-compose /usr/local/lib/docker/cli-plugins/docker-compose
rm -f "/tmp/${COMPOSE_BINARY}.sha256"

echo "==> Configuring QEMU binfmt for multi-arch builds"
# Pin to specific version for supply-chain security (--privileged required for /proc/sys/fs/binfmt_misc)
BINFMT_VERSION="qemu-v9.2.0-51"
sudo tee /etc/systemd/system/binfmt-qemu.service > /dev/null <<BINFMT
[Unit]
Description=Register QEMU binfmt handlers for multi-arch container builds
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
ExecStart=/usr/bin/docker run --rm --privileged tonistiigi/binfmt:${BINFMT_VERSION} --install all
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
BINFMT
[ $? -eq 0 ] || { echo "Failed to create binfmt-qemu.service"; exit 1; }
sudo systemctl daemon-reload || { echo "Failed to reload systemd"; exit 1; }
sudo systemctl enable binfmt-qemu.service || { echo "Failed to enable binfmt-qemu.service"; exit 1; }

echo "==> Installing Vault CLI"
VAULT_VERSION="1.18.3"
if [ "$ARCH" = "x86_64" ]; then
  VAULT_ARCH="amd64"
else
  VAULT_ARCH="arm64"
fi
VAULT_ZIP="vault_${VAULT_VERSION}_linux_${VAULT_ARCH}.zip"
curl -sfL "https://releases.hashicorp.com/vault/${VAULT_VERSION}/${VAULT_ZIP}" \
  -o "/tmp/${VAULT_ZIP}" || { echo "Failed to download Vault"; exit 1; }
curl -sfL "https://releases.hashicorp.com/vault/${VAULT_VERSION}/vault_${VAULT_VERSION}_SHA256SUMS" \
  -o /tmp/vault_checksums.txt || { echo "Failed to download checksums"; exit 1; }
CHECKSUM_LINE=$(grep "${VAULT_ZIP}" /tmp/vault_checksums.txt) || { echo "Checksum not found"; exit 1; }
[[ "$CHECKSUM_LINE" =~ ^[a-f0-9]{64}[[:space:]]+vault_ ]] || { echo "Invalid checksum format"; exit 1; }
cd /tmp || { echo "Failed to cd to /tmp"; exit 1; }
echo "$CHECKSUM_LINE" | sha256sum -c || { echo "Checksum verification failed"; rm -f "/tmp/${VAULT_ZIP}" /tmp/vault_checksums.txt; exit 1; }
sudo unzip -o "/tmp/${VAULT_ZIP}" -d /usr/local/bin || { echo "Vault extraction failed"; rm -f "/tmp/${VAULT_ZIP}" /tmp/vault_checksums.txt; exit 1; }
rm "/tmp/${VAULT_ZIP}" /tmp/vault_checksums.txt
sudo chmod +x /usr/local/bin/vault

echo "==> Configuring CloudWatch agent"
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
            "file_path": "/var/log/messages",
            "log_group_name": "/runner/system",
            "log_stream_name": "{instance_id}",
            "retention_in_days": 3
          }
        ]
      }
    }
  },
  "metrics": {
    "namespace": "Runner",
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

echo "==> Enabling CloudWatch agent"
sudo systemctl enable amazon-cloudwatch-agent

echo "==> Cleaning up"
sudo dnf clean all
sudo rm -rf /var/cache/dnf

echo "==> Base AMI provisioning complete"
echo "    - Docker: $(docker --version)"
echo "    - Docker Compose: $(docker-compose --version)"
echo "    - Docker Compose Plugin: $(docker compose version)"
echo "    - QEMU binfmt: enabled at boot"
echo "    - SSM Agent: enabled"
echo "    - CloudWatch Agent: enabled"
