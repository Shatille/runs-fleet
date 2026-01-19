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

# Install Docker CLI (for docker-in-docker workflows)
RUN curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends docker-ce-cli \
    && rm -rf /var/lib/apt/lists/*

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

# Patch CVE-2026-23745: Update vulnerable node-tar package in runner's bundled Node.js
# Note: This patches the JavaScript tar package (node-tar), not GNU tar (system binary)
# SHA512 from https://registry.npmjs.org/tar/7.5.3 (.dist.integrity)
RUN set -ex; \
    TAR_INTEGRITY="sha512-ENg5JUHUm2rDD7IvKNFGzyElLXNjachNLp6RaGf4+JOgxXHkqA+gq81ZAMCUmtMtqBsoU62lcp6S27g1LCYGGQ=="; \
    curl --proto '=https' --tlsv1.2 -fsSL https://registry.npmjs.org/tar/-/tar-7.5.3.tgz -o /tmp/tar-7.5.3.tgz; \
    ACTUAL=$(openssl dgst -sha512 -binary /tmp/tar-7.5.3.tgz | base64 -w0); \
    EXPECTED=$(echo "$TAR_INTEGRITY" | sed 's/^sha512-//'); \
    if [ "$ACTUAL" != "$EXPECTED" ]; then echo "ERROR: SHA512 mismatch" >&2; exit 1; fi; \
    echo 0 > /tmp/patched_count; \
    rm -f /tmp/patch_errors; \
    find /home/runner/externals -type d -name "tar" -path "*/node_modules/*" -exec sh -c 'set -e; \
        for pkg_dir; do \
            if [ -f "$pkg_dir/package.json" ]; then \
                rm -rf "$pkg_dir"; \
                mkdir -p "$pkg_dir"; \
                tar -xzf /tmp/tar-7.5.3.tgz -C "$pkg_dir" --strip-components=1 || { echo "FAILED: $pkg_dir" >> /tmp/patch_errors; exit 1; }; \
                COUNT=$(cat /tmp/patched_count); \
                echo $((COUNT + 1)) > /tmp/patched_count; \
            fi; \
        done' _ {} +; \
    if [ -f /tmp/patch_errors ]; then echo "ERROR: Patch extraction failed" >&2; cat /tmp/patch_errors >&2; exit 1; fi; \
    PATCHED=$(cat /tmp/patched_count 2>/dev/null || echo 0); \
    rm -f /tmp/tar-7.5.3.tgz /tmp/patched_count /tmp/patch_errors; \
    echo "Patched $PATCHED node-tar package(s)"; \
    if [ "$PATCHED" -eq 0 ]; then echo "ERROR: No tar packages found - CVE-2026-23745 unpatched" >&2; exit 1; fi

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
