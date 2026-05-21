# runs-fleet runner image
# Built on the official GitHub Actions runner image. Multi-arch (amd64/arm64).

ARG TARGETARCH
ARG RUNNER_BASE_TAG=2.334.0

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

# Stage 2: Official GitHub Actions runner base
# Provides: actions-runner at /home/runner, Node.js externals, sudo, common
# workflow tooling. Docker tooling is replaced below with apt-managed packages
# so future security updates land via `apt-get upgrade` instead of requiring
# image rebuilds against frozen upstream binaries.
FROM ghcr.io/actions/actions-runner:${RUNNER_BASE_TAG}

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

# Install Docker tooling from Docker's official apt repo so it tracks the
# upstream release cadence (and stdlib refreshes) automatically.
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
        containerd.io \
        docker-buildx-plugin \
        docker-compose-plugin; \
    apt-get upgrade -y; \
    rm -rf /var/lib/apt/lists/*

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

COPY docker/runner/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

USER runner

ENTRYPOINT ["/entrypoint.sh"]
