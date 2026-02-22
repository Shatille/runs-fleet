# runs-fleet runner image
# Contains GitHub Actions runner + runs-fleet agent
# Multi-arch: supports amd64 and arm64

ARG TARGETARCH

# Stage 1: Build agent binary
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETARCH
ARG VERSION=dev

WORKDIR /build

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build agent for target architecture
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /bin/runs-fleet-agent ./cmd/agent

# Stage 1b: Build Docker CLI from source (separate stage to avoid go.mod conflicts)
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS docker-builder

ARG TARGETARCH
ARG DOCKER_CLI_VERSION=v27.5.1

RUN apk add --no-cache git
RUN git clone --depth 1 --branch ${DOCKER_CLI_VERSION} https://github.com/docker/cli.git /docker-cli

WORKDIR /docker-cli

# Docker CLI uses vendor.mod/vendor.sum; patch vendored x/crypto CVEs before build
RUN cp vendor.mod go.mod && cp vendor.sum go.sum \
    && go get golang.org/x/crypto@v0.35.0 \
    && go mod vendor
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -mod=vendor \
    -trimpath -ldflags "-s -w" \
    -o /bin/docker ./cmd/docker

# Stage 2: Runner image
FROM ubuntu:24.04

ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive
ENV RUNNER_ALLOW_RUNASROOT=1

WORKDIR /home/runner

# Install dependencies and apply security updates
# NOTE: upgrade runs AFTER install to patch newly-installed packages (e.g., gnupg CVEs)
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    jq \
    sudo \
    unzip \
    wget \
    zip \
    gnupg \
    lsb-release \
    software-properties-common \
    && apt-get upgrade -y \
    && rm -rf /var/lib/apt/lists/*

# Docker CLI built from source (controls Go version for CVE-2025-68121 fix)
COPY --from=docker-builder /bin/docker /usr/local/bin/docker

# Create runner user with restricted sudo (apt only for workflow package installs)
# SECURITY NOTE: Sudo access is intentional to support workflows needing build deps.
# - Restricted from NOPASSWD:ALL (GitHub default) to apt-only commands
# - Ephemeral containers mitigate persistence risk
# - High-security deployments: consider restricting apt sources or using allowlisted packages
RUN useradd -m -s /bin/bash runner \
    && echo "runner ALL=(ALL) NOPASSWD:/usr/bin/apt-get,/usr/bin/apt" >> /etc/sudoers

# Download and install GitHub Actions runner
ARG RUNNER_VERSION=2.331.0
RUN set -ex \
    && case "${TARGETARCH}" in \
         "amd64") RUNNER_ARCH="x64" ;; \
         "arm64") RUNNER_ARCH="arm64" ;; \
         *) echo "ERROR: Unsupported architecture: ${TARGETARCH}" >&2 && exit 1 ;; \
       esac \
    && RUNNER_URL="https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" \
    && echo "Downloading: ${RUNNER_URL}" \
    && curl -fsSL --retry 3 --retry-delay 5 -o actions-runner.tar.gz "${RUNNER_URL}" \
    && tar xzf actions-runner.tar.gz \
    && rm actions-runner.tar.gz \
    && ./bin/installdependencies.sh \
    && chown -R runner:runner /home/runner

# Final security update for all installed packages (CVE-2025-15467: OpenSSL stack buffer overflow)
RUN apt-get update && apt-get upgrade -y && rm -rf /var/lib/apt/lists/*

# Patch node-tar CVEs (CVE-2026-23950, CVE-2026-24842): Update vulnerable package in runner's bundled Node.js
# Note: This patches the JavaScript tar package (node-tar), not GNU tar (system binary)
# SHA512 from https://registry.npmjs.org/tar/7.5.7 (.dist.integrity)
RUN set -ex; \
    TAR_INTEGRITY="sha512-fov56fJiRuThVFXD6o6/Q354S7pnWMJIVlDBYijsTNx6jKSE4pvrDTs6lUnmGvNyfJwFQQwWy3owKz1ucIhveQ=="; \
    curl --proto '=https' --tlsv1.2 -fsSL https://registry.npmjs.org/tar/-/tar-7.5.7.tgz -o /tmp/tar-7.5.7.tgz; \
    ACTUAL=$(openssl dgst -sha512 -binary /tmp/tar-7.5.7.tgz | base64 -w0); \
    EXPECTED=$(echo "$TAR_INTEGRITY" | sed 's/^sha512-//'); \
    if [ "$ACTUAL" != "$EXPECTED" ]; then echo "ERROR: SHA512 mismatch" >&2; exit 1; fi; \
    echo 0 > /tmp/patched_count; \
    rm -f /tmp/patch_errors; \
    find /home/runner/externals -type d -name "tar" -path "*/node_modules/*" -exec sh -c 'set -e; \
        for pkg_dir; do \
            if [ -f "$pkg_dir/package.json" ]; then \
                rm -rf "$pkg_dir"; \
                mkdir -p "$pkg_dir"; \
                tar -xzf /tmp/tar-7.5.7.tgz -C "$pkg_dir" --strip-components=1 || { echo "FAILED: $pkg_dir" >> /tmp/patch_errors; exit 1; }; \
                COUNT=$(cat /tmp/patched_count); \
                echo $((COUNT + 1)) > /tmp/patched_count; \
            fi; \
        done' _ {} +; \
    if [ -f /tmp/patch_errors ]; then echo "ERROR: Patch extraction failed" >&2; cat /tmp/patch_errors >&2; exit 1; fi; \
    PATCHED=$(cat /tmp/patched_count 2>/dev/null || echo 0); \
    rm -f /tmp/tar-7.5.7.tgz /tmp/patched_count /tmp/patch_errors; \
    echo "Patched $PATCHED node-tar package(s)"; \
    if [ "$PATCHED" -eq 0 ]; then echo "ERROR: No tar packages found - node-tar CVEs unpatched" >&2; exit 1; fi

# Patch @isaacs/brace-expansion CVE-2026-25547: Uncontrolled Resource Consumption
# SHA512 from https://registry.npmjs.org/@isaacs/brace-expansion/5.0.1 (.dist.integrity)
RUN set -ex; \
    PKG_INTEGRITY="sha512-WMz71T1JS624nWj2n2fnYAuPovhv7EUhk69R6i9dsVyzxt5eM3bjwvgk9L+APE1TRscGysAVMANkB0jh0LQZrQ=="; \
    curl --proto '=https' --tlsv1.2 -fsSL https://registry.npmjs.org/@isaacs/brace-expansion/-/brace-expansion-5.0.1.tgz -o /tmp/brace-expansion-5.0.1.tgz; \
    ACTUAL=$(openssl dgst -sha512 -binary /tmp/brace-expansion-5.0.1.tgz | base64 -w0); \
    EXPECTED=$(echo "$PKG_INTEGRITY" | sed 's/^sha512-//'); \
    if [ "$ACTUAL" != "$EXPECTED" ]; then echo "ERROR: SHA512 mismatch" >&2; exit 1; fi; \
    echo 0 > /tmp/patched_count; \
    rm -f /tmp/patch_errors; \
    find /home/runner/externals -type d -name "brace-expansion" -path "*/node_modules/@isaacs/*" -exec sh -c 'set -e; \
        for pkg_dir; do \
            if [ -f "$pkg_dir/package.json" ]; then \
                rm -rf "$pkg_dir"; \
                mkdir -p "$pkg_dir"; \
                tar -xzf /tmp/brace-expansion-5.0.1.tgz -C "$pkg_dir" --strip-components=1 || { echo "FAILED: $pkg_dir" >> /tmp/patch_errors; exit 1; }; \
                COUNT=$(cat /tmp/patched_count); \
                echo $((COUNT + 1)) > /tmp/patched_count; \
            fi; \
        done' _ {} +; \
    if [ -f /tmp/patch_errors ]; then echo "ERROR: Patch extraction failed" >&2; cat /tmp/patch_errors >&2; exit 1; fi; \
    PATCHED=$(cat /tmp/patched_count 2>/dev/null || echo 0); \
    rm -f /tmp/brace-expansion-5.0.1.tgz /tmp/patched_count /tmp/patch_errors; \
    echo "Patched $PATCHED @isaacs/brace-expansion package(s)"; \
    if [ "$PATCHED" -eq 0 ]; then echo "WARNING: No @isaacs/brace-expansion packages found" >&2; fi

# Copy agent binary
COPY --from=builder /bin/runs-fleet-agent /usr/local/bin/runs-fleet-agent
RUN chmod +x /usr/local/bin/runs-fleet-agent

# Copy entrypoint script
COPY docker/runner/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Set ownership
RUN chown -R runner:runner /home/runner

# Pre-create the actions-runner directory that the agent will use at runtime
# The agent downloads/extracts to /opt/actions-runner, so runner user needs write access
RUN mkdir -p /opt/actions-runner && chown runner:runner /opt/actions-runner

USER runner

ENTRYPOINT ["/entrypoint.sh"]
