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
# Unpinned to allow apt-get upgrade to fetch security patches (CVE-2025-68973)
FROM ubuntu:24.04

ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive
ENV RUNNER_ALLOW_RUNASROOT=1

WORKDIR /home/runner

# Install dependencies and apply security updates
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
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
ARG RUNNER_VERSION=2.330.0
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
