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

echo "==> Replacing *-minimal packages with full variants"
# Scoped to its own transaction so --allowerasing only affects the known
# minimal-package swaps, not the broader package list below. AL2023 standard
# ships curl-minimal and gnupg2-minimal preinstalled; both conflict with the
# full packages we need.
sudo dnf install -y --allowerasing curl gnupg2

# dl fetches a URL with retries (silent, fail-on-error, follow redirects) so a
# transient release/CDN hiccup — a 5xx like the 504s we have seen, or a reset —
# is retried instead of aborting the whole AMI build. All toolchain downloads
# below go through it.
dl() { curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors "$@"; }

echo "==> Installing base packages"
sudo dnf install -y \
  git \
  git-lfs \
  jq \
  libicu \
  make \
  rsync \
  tar \
  unzip \
  wget \
  zip \
  awscli \
  docker \
  amazon-ssm-agent \
  amazon-cloudwatch-agent

echo "==> Installing git-lfs system-wide hooks"
sudo git lfs install --system

echo "==> Adding GitHub CLI dnf repo"
# Use GitHub's published .repo file directly so the gpgkey URL stays
# upstream-canonical (it lives at /packages/githubcli-archive-keyring.asc,
# not under /rpm/gpg.key — that path 404s).
sudo dnf install -y dnf-plugins-core
sudo dnf config-manager --add-repo https://cli.github.com/packages/rpm/gh-cli.repo

echo "==> Installing GitHub CLI"
sudo dnf install -y gh

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
  dl "https://ftp.gnu.org/gnu/binutils/binutils-${BINUTILS_VERSION}.tar.xz" -o binutils.tar.xz
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

echo "==> Installing AWS Session Manager Plugin"
if [ "$ARCH" = "x86_64" ]; then
  SSM_PLUGIN_ARCH="linux_64bit"
else
  SSM_PLUGIN_ARCH="linux_arm64"
fi
sudo dnf install -y "https://s3.amazonaws.com/session-manager-downloads/plugin/latest/${SSM_PLUGIN_ARCH}/session-manager-plugin.rpm" \
  || { echo "Failed to install session-manager-plugin"; exit 1; }

echo "==> Enabling Docker"
sudo systemctl enable docker
sudo usermod -aG docker ec2-user

echo "==> Installing Docker Compose (${COMPOSE_ARCH})"
DOCKER_COMPOSE_VERSION="5.1.4"
COMPOSE_BINARY="docker-compose-linux-${COMPOSE_ARCH}"
COMPOSE_URL="https://github.com/docker/compose/releases/download/v${DOCKER_COMPOSE_VERSION}"
# Download and verify checksum (filenames must match for sha256sum -c)
dl "${COMPOSE_URL}/${COMPOSE_BINARY}.sha256" -o "/tmp/${COMPOSE_BINARY}.sha256" \
  || { echo "Failed to download Docker Compose checksum"; exit 1; }
dl "${COMPOSE_URL}/${COMPOSE_BINARY}" -o "/tmp/${COMPOSE_BINARY}" \
  || { echo "Failed to download Docker Compose"; exit 1; }
cd /tmp && sha256sum -c "${COMPOSE_BINARY}.sha256" \
  || { echo "Docker Compose checksum mismatch"; rm -f "/tmp/${COMPOSE_BINARY}" "/tmp/${COMPOSE_BINARY}.sha256"; exit 1; }
# Install as standalone binary (docker-compose)
sudo mv "/tmp/${COMPOSE_BINARY}" /usr/local/bin/docker-compose \
  || { echo "Failed to install Docker Compose binary"; exit 1; }
sudo chmod +x /usr/local/bin/docker-compose
# Install as Docker CLI plugin (docker compose)
sudo mkdir -p /usr/local/lib/docker/cli-plugins \
  || { echo "Failed to create Docker CLI plugins directory"; exit 1; }
sudo cp /usr/local/bin/docker-compose /usr/local/lib/docker/cli-plugins/docker-compose \
  || { echo "Failed to install Docker Compose plugin"; exit 1; }
rm -f "/tmp/${COMPOSE_BINARY}.sha256"

echo "==> Installing Node.js ecosystem"
NODE_VERSION="22.13.1"
if [ "$ARCH" = "x86_64" ]; then
  NODE_ARCH="x64"
else
  NODE_ARCH="arm64"
fi
NODE_DIST="node-v${NODE_VERSION}-linux-${NODE_ARCH}"
NODE_URL="https://nodejs.org/dist/v${NODE_VERSION}"
# Download and verify checksum
dl "${NODE_URL}/SHASUMS256.txt" -o /tmp/node-shasums.txt \
  || { echo "Failed to download Node.js checksums"; exit 1; }
dl "${NODE_URL}/${NODE_DIST}.tar.xz" -o "/tmp/${NODE_DIST}.tar.xz" \
  || { echo "Failed to download Node.js"; exit 1; }
NODE_CHECKSUM=$(grep "${NODE_DIST}.tar.xz" /tmp/node-shasums.txt | cut -d' ' -f1) \
  || { echo "Node.js checksum not found"; exit 1; }
echo "${NODE_CHECKSUM}  /tmp/${NODE_DIST}.tar.xz" | sha256sum -c \
  || { echo "Node.js checksum mismatch"; rm -f "/tmp/${NODE_DIST}.tar.xz" /tmp/node-shasums.txt; exit 1; }
# Extract and install
sudo tar -xJf "/tmp/${NODE_DIST}.tar.xz" -C /usr/local --strip-components=1 \
  || { echo "Failed to extract Node.js"; exit 1; }
rm -f "/tmp/${NODE_DIST}.tar.xz" /tmp/node-shasums.txt
# Install yarn and pnpm globally (pinned versions for reproducibility)
YARN_VERSION="1.22.22"
PNPM_VERSION="9.15.4"
sudo /usr/local/bin/npm install -g "yarn@${YARN_VERSION}" "pnpm@${PNPM_VERSION}" \
  || { echo "Failed to install yarn/pnpm"; exit 1; }
# Create symlinks in /usr/bin for PATH compatibility with GitHub Actions
sudo ln -sf /usr/local/bin/node /usr/bin/node
sudo ln -sf /usr/local/bin/npm /usr/bin/npm
sudo ln -sf /usr/local/bin/npx /usr/bin/npx
sudo ln -sf /usr/local/bin/yarn /usr/bin/yarn
sudo ln -sf /usr/local/bin/pnpm /usr/bin/pnpm
sudo ln -sf /usr/local/bin/pnpx /usr/bin/pnpx

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

echo "==> Configuring Docker buildx for multi-arch builds"
# Create systemd service to set up buildx builder after binfmt is registered
sudo tee /etc/systemd/system/buildx-setup.service > /dev/null <<'BUILDX'
[Unit]
Description=Configure Docker buildx multi-arch builder
After=binfmt-qemu.service docker.service
Requires=docker.service
Wants=binfmt-qemu.service

[Service]
Type=oneshot
User=ec2-user
Group=docker
ExecStart=/bin/bash -c 'docker buildx create --name multiarch --driver docker-container --bootstrap --use || true'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
BUILDX
[ $? -eq 0 ] || { echo "Failed to create buildx-setup.service"; exit 1; }
sudo systemctl daemon-reload || { echo "Failed to reload systemd"; exit 1; }
sudo systemctl enable buildx-setup.service || { echo "Failed to enable buildx-setup.service"; exit 1; }

echo "==> Installing Vault CLI"
VAULT_VERSION="1.18.3"
if [ "$ARCH" = "x86_64" ]; then
  VAULT_ARCH="amd64"
else
  VAULT_ARCH="arm64"
fi
VAULT_ZIP="vault_${VAULT_VERSION}_linux_${VAULT_ARCH}.zip"
dl "https://releases.hashicorp.com/vault/${VAULT_VERSION}/${VAULT_ZIP}" \
  -o "/tmp/${VAULT_ZIP}" || { echo "Failed to download Vault"; exit 1; }
dl "https://releases.hashicorp.com/vault/${VAULT_VERSION}/vault_${VAULT_VERSION}_SHA256SUMS" \
  -o /tmp/vault_checksums.txt || { echo "Failed to download checksums"; exit 1; }
CHECKSUM_LINE=$(grep "${VAULT_ZIP}" /tmp/vault_checksums.txt) || { echo "Checksum not found"; exit 1; }
[[ "$CHECKSUM_LINE" =~ ^[a-f0-9]{64}[[:space:]]+vault_ ]] || { echo "Invalid checksum format"; exit 1; }
cd /tmp || { echo "Failed to cd to /tmp"; exit 1; }
echo "$CHECKSUM_LINE" | sha256sum -c || { echo "Checksum verification failed"; rm -f "/tmp/${VAULT_ZIP}" /tmp/vault_checksums.txt; exit 1; }
sudo unzip -o "/tmp/${VAULT_ZIP}" -d /usr/local/bin || { echo "Vault extraction failed"; rm -f "/tmp/${VAULT_ZIP}" /tmp/vault_checksums.txt; exit 1; }
rm "/tmp/${VAULT_ZIP}" /tmp/vault_checksums.txt
sudo chmod +x /usr/local/bin/vault

echo "==> Installing yq"
YQ_VERSION="4.53.3"
if [ "$ARCH" = "x86_64" ]; then
  YQ_ARCH="amd64"
else
  YQ_ARCH="arm64"
fi
YQ_BINARY="yq_linux_${YQ_ARCH}"
YQ_URL="https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}"
cleanup_yq_tmp() { rm -f /tmp/yq /tmp/yq_checksums.txt /tmp/yq_hashes_order.txt; }
trap cleanup_yq_tmp EXIT
dl "${YQ_URL}/${YQ_BINARY}" -o /tmp/yq \
  || { echo "Failed to download yq"; exit 1; }
dl "${YQ_URL}/checksums" -o /tmp/yq_checksums.txt \
  || { echo "Failed to download yq checksums"; exit 1; }
dl "${YQ_URL}/checksums_hashes_order" -o /tmp/yq_hashes_order.txt \
  || { echo "Failed to download yq hash order"; exit 1; }
# yq's `checksums` file is "<filename> <hash1> <hash2> ..." with one column per
# algorithm; `checksums_hashes_order` lists which algorithm sits at each column
# (in order). Field 1 is the filename, so SHA-256 is at field (row+1).
SHA256_ROW=$(awk '/^SHA-256$/ {print NR; exit}' /tmp/yq_hashes_order.txt)
[ -n "$SHA256_ROW" ] || { echo "yq SHA-256 row not found in hash order"; exit 1; }
SHA256_FIELD=$((SHA256_ROW + 1))
YQ_CHECKSUM=$(awk -v fname="${YQ_BINARY}" -v f="$SHA256_FIELD" \
  '$1 == fname {print $f; exit}' /tmp/yq_checksums.txt)
[[ "$YQ_CHECKSUM" =~ ^[a-f0-9]{64}$ ]] \
  || { echo "Invalid yq SHA-256: $YQ_CHECKSUM"; exit 1; }
echo "${YQ_CHECKSUM}  /tmp/yq" | sha256sum -c \
  || { echo "yq checksum verification failed"; exit 1; }
sudo install -m 0755 /tmp/yq /usr/local/bin/yq \
  || { echo "yq installation failed"; exit 1; }
trap - EXIT
cleanup_yq_tmp

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

echo "==> Installing GitHub Actions runner OS dependencies"
# AL2023 isn't recognized by actions/runner's installdependencies.sh, so the
# runtime libraries are installed explicitly. libicu is already pulled in via
# the base packages above; the rest are net-new.
sudo dnf install -y \
  openssl-libs \
  krb5-libs \
  zlib \
  libgcc

echo "==> Installing development tools for CI workloads"
# Full GCC/make/etc. toolchain so CI jobs can compile native deps without an
# extra apt-get/dnf step. Gold linker is built from source above (ARM64 only)
# for Go race detector support.
sudo dnf groupinstall -y "Development Tools"

echo "==> Installing Java"
sudo dnf install -y java-21-amazon-corretto-headless \
  || { echo "Java installation failed"; exit 1; }

echo "==> Installing sbt"
SBT_VERSION="1.10.7"
SBT_SHA256="32c15233c636c233ee25a2c31879049db7021cfef70807c187515c39b96b0fe6"
dl --max-time 300 -o /tmp/sbt.tgz "https://github.com/sbt/sbt/releases/download/v${SBT_VERSION}/sbt-${SBT_VERSION}.tgz" \
  || { echo "sbt download failed"; exit 1; }
echo "${SBT_SHA256}  /tmp/sbt.tgz" | sha256sum -c \
  || { echo "sbt checksum verification failed"; rm /tmp/sbt.tgz; exit 1; }
sudo tar xzf /tmp/sbt.tgz -C /usr/local \
  || { echo "sbt extraction failed"; rm /tmp/sbt.tgz; exit 1; }
sudo ln -sf /usr/local/sbt/bin/sbt /usr/local/bin/sbt \
  || { echo "sbt symlink creation failed"; exit 1; }
rm /tmp/sbt.tgz

echo "==> Installing Python (3.11, 3.12, 3.13) + pipx"
# AL2023's system python3 is 3.9 and is used by dnf/OS tooling, so /usr/bin/python3 is
# left untouched. Install the newer namespaced interpreters CI needs, and expose an
# unversioned `python`/`pip` (default 3.12) via /usr/local/bin, which precedes
# /usr/bin on PATH — so `python` resolves (jobs were hitting "command not found").
PYTHON_VERSIONS=("3.11" "3.12" "3.13")
PYTHON_DEFAULT="3.12"
for v in "${PYTHON_VERSIONS[@]}"; do
  # -devel ships Python.h etc. so jobs can compile C extensions from sdist (numpy,
  # psycopg2, …); gcc is already present via Development Tools.
  sudo dnf install -y "python${v}" "python${v}-pip" "python${v}-devel" \
    || { echo "Python ${v} installation failed"; exit 1; }
done
sudo ln -sf "/usr/bin/python${PYTHON_DEFAULT}" /usr/local/bin/python
sudo ln -sf "/usr/bin/pip${PYTHON_DEFAULT}" /usr/local/bin/pip

echo "==> Installing pipx"
# Prefer the distro package; fall back to pip. The system interpreter is marked
# externally-managed (PEP 668), so the pip path needs --break-system-packages.
sudo dnf install -y pipx \
  || sudo "python${PYTHON_DEFAULT}" -m pip install --break-system-packages pipx \
  || { echo "pipx installation failed"; exit 1; }
# Global pipx home/bin, writable by the job user. Jobs run `pipx install <tool>` and
# expect the launcher on PATH (GitHub-hosted has pipx's bin dir on PATH); the
# runs-fleet-agent unit sets PIPX_HOME/PIPX_BIN_DIR here and puts /opt/pipx/bin on the
# job PATH. Pre-created (owned by ec2-user) since /opt is root-owned.
sudo mkdir -p /opt/pipx/bin
sudo chown -R ec2-user:ec2-user /opt/pipx

echo "==> Pre-populating the Actions Python tool cache"
# actions/setup-python can't fetch CPython for AL2023 (no build in its manifest and
# GitHub won't add one), so expose the installed interpreters where the action looks:
#   $AGENT_TOOLSDIRECTORY/Python/<x.y.z>/<platform>/  (+ a 0-byte <platform>.complete)
# The runs-fleet-agent unit sets AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache. The cache
# entries symlink the dnf interpreters, which run from their real /usr prefix, so no
# relocation is needed — setup-python just prepends <platform>/bin to PATH.
if [ "$ARCH" = "x86_64" ]; then
  TOOLCACHE_PLATFORM="x64"
else
  TOOLCACHE_PLATFORM="arm64"
fi
for v in "${PYTHON_VERSIONS[@]}"; do
  full=$("/usr/bin/python${v}" -c 'import platform; print(platform.python_version())') \
    || { echo "Python ${v} version probe failed"; exit 1; }
  dest="/opt/hostedtoolcache/Python/${full}/${TOOLCACHE_PLATFORM}"
  sudo mkdir -p "${dest}/bin"
  sudo ln -sf "/usr/bin/python${v}" "${dest}/bin/python${v}"
  sudo ln -sf "/usr/bin/python${v}" "${dest}/bin/python3"
  sudo ln -sf "/usr/bin/python${v}" "${dest}/bin/python"
  if [ -x "/usr/bin/pip${v}" ]; then
    sudo ln -sf "/usr/bin/pip${v}" "${dest}/bin/pip${v}"
    sudo ln -sf "/usr/bin/pip${v}" "${dest}/bin/pip3"
    sudo ln -sf "/usr/bin/pip${v}" "${dest}/bin/pip"
  fi
  sudo touch "/opt/hostedtoolcache/Python/${full}/${TOOLCACHE_PLATFORM}.complete"
done
sudo chown -R ec2-user:ec2-user /opt/hostedtoolcache

echo "==> Installing Ruby (3.2, 3.4) + bundler"
# AL2023 namespaces Ruby: ruby3.2/ruby3.4 install /usr/bin/ruby<ver> plus versioned
# companions /usr/bin/ruby<ver>-gem, -bundle, -bundler (unversioned ruby/gem/bundle
# are only wired via `alternatives`, which can be null). Expose an unversioned
# ruby/gem/bundle (default 3.4) via /usr/local/bin so direct calls resolve without
# depending on alternatives state.
RUBY_VERSIONS=("3.2" "3.4")
RUBY_DEFAULT="3.4"
for v in "${RUBY_VERSIONS[@]}"; do
  sudo dnf install -y "ruby${v}" "ruby${v}-rubygems" "ruby${v}-rubygem-bundler" \
    || { echo "Ruby ${v} installation failed"; exit 1; }
done
# ruby<ver>-devel are NOT co-installable on AL2023: unlike the namespaced runtimes
# (and unlike python<ver>-devel), they all own the unversioned /usr/include/ruby
# headers + /usr/lib64/libruby.so and conflict. Install headers for the default (3.4)
# only, so native gems (nokogiri, pg, …) build against it; other versions stay
# runtime-only.
sudo dnf install -y "ruby${RUBY_DEFAULT}-devel" \
  || { echo "Ruby ${RUBY_DEFAULT}-devel installation failed"; exit 1; }
sudo ln -sf "/usr/bin/ruby${RUBY_DEFAULT}" /usr/local/bin/ruby
sudo ln -sf "/usr/bin/ruby${RUBY_DEFAULT}-gem" /usr/local/bin/gem
sudo ln -sf "/usr/bin/ruby${RUBY_DEFAULT}-bundle" /usr/local/bin/bundle
sudo ln -sf "/usr/bin/ruby${RUBY_DEFAULT}-bundler" /usr/local/bin/bundler

echo "==> Pre-populating the Actions Ruby tool cache"
# Same rationale as Python: ruby/setup-ruby can't fetch a Ruby for AL2023, so expose
# the installed interpreters at $AGENT_TOOLSDIRECTORY/Ruby/<x.y.z>/<platform>/ with a
# .complete marker. Entries symlink the versioned dnf binaries (their embedded /usr
# prefix stays valid when run via the symlink). TOOLCACHE_PLATFORM is set above.
for v in "${RUBY_VERSIONS[@]}"; do
  full=$("/usr/bin/ruby${v}" -e 'print RUBY_VERSION') \
    || { echo "Ruby ${v} version probe failed"; exit 1; }
  dest="/opt/hostedtoolcache/Ruby/${full}/${TOOLCACHE_PLATFORM}"
  sudo mkdir -p "${dest}/bin"
  sudo ln -sf "/usr/bin/ruby${v}" "${dest}/bin/ruby"
  sudo ln -sf "/usr/bin/ruby${v}-gem" "${dest}/bin/gem"
  sudo ln -sf "/usr/bin/ruby${v}-bundle" "${dest}/bin/bundle"
  sudo ln -sf "/usr/bin/ruby${v}-bundler" "${dest}/bin/bundler"
  sudo touch "/opt/hostedtoolcache/Ruby/${full}/${TOOLCACHE_PLATFORM}.complete"
done
sudo chown -R ec2-user:ec2-user /opt/hostedtoolcache

# Pre-bake Node/Go/Java into the Actions tool cache. Unlike Python/Ruby these already
# work on AL2023 via setup-* (generic upstream downloads), so this is a pure speed/egress
# optimization: on ephemeral runners the per-runner _work/_tool cache is fresh every
# registration, so each job otherwise re-downloads its toolchain. Entries use the upstream
# generic-Linux builds laid out where setup-* looks; a miss just falls back to a working
# download. We resolve the latest patch of each pinned line at build (checksummed) rather
# than hard-pinning a patch we can't hand-verify — setup-* matches by major.minor anyway.
toolcache_extract() {
  # toolcache_extract <tool> <version> <tarball>: unpack the tarball's single top-level
  # dir into $AGENT_TOOLSDIRECTORY/<tool>/<version>/<platform>/ and mark it complete.
  local tool="$1" version="$2" tarball="$3"
  local dest="/opt/hostedtoolcache/${tool}/${version}/${TOOLCACHE_PLATFORM}"
  # All three upstream tarballs (node/go/temurin) have exactly one top-level dir that
  # --strip-components=1 removes; bail rather than scatter files if that ever changes.
  local tops
  tops=$(tar -tf "$tarball" | awk -F/ '$1 != "" {print $1}' | sort -u | wc -l)
  [ "$tops" -eq 1 ] \
    || { echo "${tool} ${version}: expected 1 top-level dir in archive, got ${tops}"; exit 1; }
  # Start from a clean dir so a partial dir from a prior failed run can't mix in.
  sudo rm -rf "$dest"
  sudo mkdir -p "$dest"
  sudo tar -xf "$tarball" -C "$dest" --strip-components=1 \
    || { echo "${tool} ${version} extraction failed"; exit 1; }
  sudo touch "/opt/hostedtoolcache/${tool}/${version}/${TOOLCACHE_PLATFORM}.complete"
}

echo "==> Pre-baking Node into the tool cache (20, 22)"
NODE_TC_INDEX=$(dl https://nodejs.org/dist/index.json) \
  || { echo "Node index download failed"; exit 1; }
for major in 20 22; do
  ver=$(printf '%s' "$NODE_TC_INDEX" | jq -r --arg m "v${major}." \
    'map(.version) | map(select(startswith($m))) | first // empty')
  [ -n "$ver" ] || { echo "no Node ${major}.x in index"; exit 1; }
  dist="node-${ver}-linux-${NODE_ARCH}"
  dl "https://nodejs.org/dist/${ver}/SHASUMS256.txt" -o /tmp/node-tc-sha.txt \
    || { echo "Node ${ver} checksums download failed"; exit 1; }
  dl "https://nodejs.org/dist/${ver}/${dist}.tar.xz" -o "/tmp/${dist}.tar.xz" \
    || { echo "Node ${ver} download failed"; exit 1; }
  sum=$(grep "${dist}.tar.xz" /tmp/node-tc-sha.txt | cut -d' ' -f1)
  echo "${sum}  /tmp/${dist}.tar.xz" | sha256sum -c \
    || { echo "Node ${ver} checksum mismatch"; exit 1; }
  toolcache_extract node "${ver#v}" "/tmp/${dist}.tar.xz"
  rm -f "/tmp/${dist}.tar.xz" /tmp/node-tc-sha.txt
done

echo "==> Pre-baking Go into the tool cache (1.24, 1.25)"
if [ "$ARCH" = "x86_64" ]; then GO_ARCH="amd64"; else GO_ARCH="arm64"; fi
GO_TC_INDEX=$(dl "https://go.dev/dl/?mode=json&include=all") \
  || { echo "Go index download failed"; exit 1; }
go_default_dir=""
for minor in 1.24 1.25; do
  file=$(printf '%s' "$GO_TC_INDEX" | jq -c --arg m "go${minor}." --arg a "$GO_ARCH" \
    '[ .[] | select(.stable) | select(.version | startswith($m))
       | .files[] | select(.os=="linux" and .arch==$a and .kind=="archive") ] | first // empty')
  [ -n "$file" ] || { echo "no Go ${minor}.x archive found"; exit 1; }
  gfn=$(printf '%s' "$file" | jq -r .filename)
  gsha=$(printf '%s' "$file" | jq -r .sha256)
  gfull=$(printf '%s' "$file" | jq -r '.version | ltrimstr("go")')
  dl "https://go.dev/dl/${gfn}" -o "/tmp/${gfn}" \
    || { echo "Go ${gfn} download failed"; exit 1; }
  echo "${gsha}  /tmp/${gfn}" | sha256sum -c \
    || { echo "Go ${gfn} checksum mismatch"; exit 1; }
  toolcache_extract go "$gfull" "/tmp/${gfn}"
  rm -f "/tmp/${gfn}"
  go_default_dir="/opt/hostedtoolcache/go/${gfull}/${TOOLCACHE_PLATFORM}"
done
# Host has no `go` on PATH; expose the newest baked one (setup-go adds its own per job).
# Fail closed: the symlinks must point at a Go we actually extracted, never an empty path.
{ [ -n "$go_default_dir" ] && [ -x "${go_default_dir}/bin/go" ]; } \
  || { echo "Go tool-cache default not populated"; exit 1; }
sudo ln -sf "${go_default_dir}/bin/go" /usr/local/bin/go
sudo ln -sf "${go_default_dir}/bin/gofmt" /usr/local/bin/gofmt

echo "==> Pre-baking Temurin JDK into the tool cache (17, 21)"
if [ "$ARCH" = "x86_64" ]; then ADOPT_ARCH="x64"; else ADOPT_ARCH="aarch64"; fi
for major in 17 21; do
  meta=$(dl "https://api.adoptium.net/v3/assets/feature_releases/${major}/ga?architecture=${ADOPT_ARCH}&image_type=jdk&jvm_impl=hotspot&os=linux&vendor=eclipse&page_size=1&sort_order=DESC") \
    || { echo "Temurin ${major} metadata fetch failed"; exit 1; }
  # jq exits 0 with empty output for a missing field, so check each value explicitly
  # (a jq parse crash still aborts under set -e). Confirm a binary exists for this
  # arch/os filter before indexing binaries[0].
  bin_count=$(printf '%s' "$meta" | jq '.[0].binaries | length // 0')
  [ "$bin_count" -gt 0 ] || { echo "no Temurin ${major} binary for ${ADOPT_ARCH}/linux"; exit 1; }
  link=$(printf '%s' "$meta" | jq -r '.[0].binaries[0].package.link // empty')
  [ -n "$link" ] || { echo "no Temurin ${major} download link in metadata"; exit 1; }
  jsha=$(printf '%s' "$meta" | jq -r '.[0].binaries[0].package.checksum // empty')
  [ -n "$jsha" ] || { echo "no Temurin ${major} checksum in metadata"; exit 1; }
  semver=$(printf '%s' "$meta" | jq -r '.[0].version_data.semver // empty')
  [ -n "$semver" ] || { echo "no Temurin ${major} semver in metadata"; exit 1; }
  # Defense-in-depth: the link comes from the API response and the checksum comes from
  # the same response, so the checksum can't be the only guard — pin the download to
  # Adoptium's GitHub release host before fetching.
  case "$link" in
    https://github.com/adoptium/*) : ;;
    *) echo "Temurin ${major} link has unexpected host: $link"; exit 1 ;;
  esac
  # setup-java's dir uses the semver with '+' replaced by '-' (e.g. 21.0.4+7 -> 21.0.4-7).
  norm="${semver//+/-}"
  dl "$link" -o "/tmp/temurin-${major}.tar.gz" \
    || { echo "Temurin ${major} download failed"; exit 1; }
  echo "${jsha}  /tmp/temurin-${major}.tar.gz" | sha256sum -c \
    || { echo "Temurin ${major} checksum mismatch"; exit 1; }
  toolcache_extract "Java_Temurin-Hotspot_jdk" "$norm" "/tmp/temurin-${major}.tar.gz"
  rm -f "/tmp/temurin-${major}.tar.gz"
done

sudo chown -R ec2-user:ec2-user /opt/hostedtoolcache

echo "==> Downloading GitHub Actions runner"
RUNNER_VERSION="2.334.0"
if [ "$ARCH" = "x86_64" ]; then
  RUNNER_PLATFORM="x64"
else
  RUNNER_PLATFORM="arm64"
fi
sudo mkdir -p /opt/actions-runner
sudo chown ec2-user:ec2-user /opt/actions-runner
dl -o /tmp/actions-runner.tar.gz \
  "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_PLATFORM}-${RUNNER_VERSION}.tar.gz" \
  || { echo "actions-runner download failed"; exit 1; }
tar xzf /tmp/actions-runner.tar.gz -C /opt/actions-runner \
  || { echo "actions-runner extraction failed"; rm /tmp/actions-runner.tar.gz; exit 1; }
rm /tmp/actions-runner.tar.gz

# Downstream extension point.
# The build uploads packer/provision-base-hook.sh to /tmp/provision-base-hook.sh
# unconditionally. Upstream ships an empty stub; downstream forks rewrite it
# from a CI secret. If the file is non-empty, run it before cleanup so its
# changes are part of the snapshot.
HOOK="/tmp/provision-base-hook.sh"
if [ -s "$HOOK" ]; then
  echo "==> Running downstream provision hook"
  sudo bash "$HOOK"
fi

echo "==> Cleaning up"
sudo dnf clean all
sudo rm -rf /var/cache/dnf

echo "==> Base AMI provisioning complete"
echo "    - Docker: $(docker --version)"
echo "    - Docker Compose: $(docker-compose --version)"
echo "    - Docker Compose Plugin: $(docker compose version)"
echo "    - Node.js: $(node --version)"
echo "    - npm: $(npm --version)"
echo "    - yarn: $(yarn --version)"
echo "    - pnpm: $(pnpm --version)"
echo "    - git-lfs: $(git lfs version)"
echo "    - gh: $(gh --version | head -1)"
echo "    - yq: $(yq --version)"
echo "    - curl: $(curl --version | head -1)"
echo "    - QEMU binfmt: enabled at boot"
echo "    - Docker buildx: multi-arch builder configured"
echo "    - SSM Agent: enabled"
echo "    - Session Manager Plugin: $(session-manager-plugin --version)"
echo "    - CloudWatch Agent: enabled"
echo "    - Java: $(java -version 2>&1 | head -1)"
echo "    - sbt: v${SBT_VERSION}"
echo "    - Python: $(python --version 2>&1) (default); versions ${PYTHON_VERSIONS[*]} in tool cache"
echo "    - pipx: $(pipx --version 2>&1)"
echo "    - Ruby: $(ruby --version 2>&1) (default); versions ${RUBY_VERSIONS[*]} in tool cache"
echo "    - Go: $(go version 2>&1) (default)"
echo "    - Tool cache: Node 20/22, Go 1.24/1.25, Temurin JDK 17/21 (${TOOLCACHE_PLATFORM})"
echo "    - GitHub Actions runner: v${RUNNER_VERSION} (linux-${RUNNER_PLATFORM})"
