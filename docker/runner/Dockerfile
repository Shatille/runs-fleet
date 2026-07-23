# runs-fleet runner image
# Built on the official GitHub Actions runner image. Multi-arch (amd64/arm64).

ARG TARGETARCH
ARG RUNNER_BASE_TAG=2.335.1

# Stage 1: Build the runs-fleet agent binary
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETARCH
ARG VERSION=dev

WORKDIR /build

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /bin/runs-fleet-agent ./cmd/agent

# The transparent buildx layer-cache shim. Extracted from this image by the
# runner AMI provisioner and installed as the docker-buildx CLI plugin on the
# EC2 host; it is not used inside the container itself.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /bin/runs-fleet-buildx-shim ./cmd/buildx-shim

# Stage 2: Official GitHub Actions runner base
# Provides: actions-runner at /home/runner, Node.js externals, sudo, common
# workflow tooling. Docker tooling is replaced below with apt-managed packages
# so future security updates land via `apt-get upgrade` instead of requiring
# image rebuilds against frozen upstream binaries.
# Named so build-runner.yml's no-cache-filters can force these layers fresh
# each CI build (BuildKit filters match declared stage names only).
FROM ghcr.io/actions/actions-runner:${RUNNER_BASE_TAG} AS runtime

ARG TARGETARCH

USER root

ENV DEBIAN_FRONTEND=noninteractive

# Remove the upstream-bundled Docker tooling (manually installed by the runner
# image; not apt-tracked, so it never gets security updates). We reinstall from
# Docker's official apt repo below.
RUN set -ex; \
    rm -f /usr/bin/docker /usr/bin/dockerd /usr/bin/docker-proxy \
          /usr/bin/containerd /usr/bin/containerd-shim-runc-v2 /usr/bin/ctr \
          /usr/bin/runc; \
    rm -rf /usr/local/lib/docker /usr/libexec/docker

# Install the Docker engine + CLI from Docker's official apt repo so they track
# the upstream release cadence (and stdlib refreshes) automatically.
RUN set -ex; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl gnupg; \
    install -m 0755 -d /etc/apt/keyrings; \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
        -o /etc/apt/keyrings/docker.asc; \
    chmod a+r /etc/apt/keyrings/docker.asc; \
    . /etc/os-release; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
        > /etc/apt/sources.list.d/docker.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        docker-ce \
        docker-ce-cli \
        containerd.io; \
    apt-get upgrade -y; \
    rm -rf /var/lib/apt/lists/*

# The buildx/compose cli-plugins come from their upstream GitHub releases, not
# Docker's apt repo: the apt packaging lags the plugin projects by months and
# ships builds on long-fixed golang.org/x/* and Go stdlib versions (25+ HIGH
# findings vs 1 at the current upstream releases). Expected digests are pinned
# here rather than fetched from the release's checksums.txt so the trust
# anchor is this repo, not the release page (upstream publishes no artifact
# signatures). Bump procedure: update the version ARGs together with all four
# digests and re-run `make scan-runner` (see docker/runner/CLAUDE.md). The
# host AMI pins the same buildx and compose versions in
# packer/provision-base.sh — bump both places together.
ARG BUILDX_VERSION=0.35.0
ARG COMPOSE_VERSION=5.3.1
ARG BUILDX_SHA256_AMD64=d41ece72044243b4f58b343441ae37446d9c29a7d6b5e11c61847bbcf8f7dfda
ARG BUILDX_SHA256_ARM64=c4248d6cbc4a619a7e0b4609c11e509ad4ac0b475e1c64817c0ac20c5d90c766
ARG COMPOSE_SHA256_AMD64=f9ebc6ebdb19d769b793c245a736caaeb198c62587f13b25c660c13b4987f959
ARG COMPOSE_SHA256_ARM64=aa611e811d0ea25897839c404bfb5bf93ce706dc51c500a4457890f5d0606a86
RUN set -ex; \
    case "${TARGETARCH}" in \
        amd64) COMPOSE_ARCH=x86_64; BUILDX_SHA256="${BUILDX_SHA256_AMD64}"; COMPOSE_SHA256="${COMPOSE_SHA256_AMD64}" ;; \
        arm64) COMPOSE_ARCH=aarch64; BUILDX_SHA256="${BUILDX_SHA256_ARM64}"; COMPOSE_SHA256="${COMPOSE_SHA256_ARM64}" ;; \
        *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /usr/libexec/docker/cli-plugins /tmp/plugins; \
    cd /tmp/plugins; \
    curl -fsSLO "https://github.com/docker/buildx/releases/download/v${BUILDX_VERSION}/buildx-v${BUILDX_VERSION}.linux-${TARGETARCH}"; \
    echo "${BUILDX_SHA256} *buildx-v${BUILDX_VERSION}.linux-${TARGETARCH}" | sha256sum -c; \
    install -m 0755 "buildx-v${BUILDX_VERSION}.linux-${TARGETARCH}" \
        /usr/libexec/docker/cli-plugins/docker-buildx; \
    curl -fsSLO "https://github.com/docker/compose/releases/download/v${COMPOSE_VERSION}/docker-compose-linux-${COMPOSE_ARCH}"; \
    echo "${COMPOSE_SHA256} *docker-compose-linux-${COMPOSE_ARCH}" | sha256sum -c; \
    install -m 0755 "docker-compose-linux-${COMPOSE_ARCH}" \
        /usr/libexec/docker/cli-plugins/docker-compose; \
    cd /; \
    rm -rf /tmp/plugins

# Drop the Node.js 20 distribution. GitHub deprecated Node 20 actions on 2025-09-19;
# forced cutover to Node 24 lands on 2026-06-02 and Node 20 leaves the runner on
# 2026-09-16. The ENV below redirects all `runs.using: node20` actions to node24
# via GitHub's official transition flag, so the bundled node20 tree (and its npm
# CVE surface) can go now. Workflows needing Node 20 specifically should use
# actions/setup-node@v5 to install their own.
ENV FORCE_JAVASCRIPT_ACTIONS_TO_NODE24=true
RUN rm -rf /home/runner/externals/node20

# Refresh the bundled npm to the latest release so its transitive node_modules
# (tar, minimatch, glob, picomatch, ...) come from the current npm tree rather
# than whatever was frozen when the runner image was built. npm self-upgrade
# handles integrity verification — no version pins or hashes maintained here.
RUN /home/runner/externals/node24/bin/node \
        /home/runner/externals/node24/lib/node_modules/npm/bin/npm-cli.js \
        install -g npm@latest

# Pre-create /opt/actions-runner so the agent's fallback download path is writable
RUN mkdir -p /opt/actions-runner && chown runner:runner /opt/actions-runner

# Tighten sudo: base image grants %sudo NOPASSWD:ALL; restrict to apt for workflow
# package installs only. Ephemeral containers mitigate residual risk.
RUN printf 'runner ALL=(ALL) NOPASSWD:/usr/bin/apt-get,/usr/bin/apt\n' \
    > /etc/sudoers.d/runner-restricted \
    && chmod 0440 /etc/sudoers.d/runner-restricted \
    && sed -i 's|^%sudo.*NOPASSWD:ALL.*|%sudo ALL=(ALL:ALL) NOPASSWD:/usr/bin/apt-get,/usr/bin/apt|' /etc/sudoers

COPY --from=builder /bin/runs-fleet-agent /usr/local/bin/runs-fleet-agent
RUN chmod +x /usr/local/bin/runs-fleet-agent

# Carried for the runner AMI provisioner to extract onto the EC2 host; unused in
# the container. See packer/provision-runs-fleet.sh.
COPY --from=builder /bin/runs-fleet-buildx-shim /usr/local/bin/runs-fleet-buildx-shim
RUN chmod +x /usr/local/bin/runs-fleet-buildx-shim

COPY docker/runner/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

USER runner

ENTRYPOINT ["/entrypoint.sh"]
