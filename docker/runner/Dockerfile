# runs-fleet runner image
# Contains GitHub Actions runner + runs-fleet agent
# Multi-arch: supports amd64 and arm64

ARG RUNNER_VERSION=2.321.0
ARG TARGETARCH

# Stage 1: Build agent binary
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

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
# Pin to specific digest for reproducible builds (ubuntu:24.04 as of 2025-12)
FROM ubuntu:24.04@sha256:c35e29c9450151419d9448b0fd75374fec4fff364a27f176fb458d472dfc9e54

ARG RUNNER_VERSION
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

# Download and install GitHub Actions runner with checksum verification
# Checksums from https://github.com/actions/runner/releases/tag/v2.321.0
# Update these when changing RUNNER_VERSION
# Supported architectures: amd64, arm64 only
RUN RUNNER_ARCH_DOWNLOAD=$(case "${TARGETARCH}" in \
         "amd64") echo "x64" ;; \
         "arm64") echo "arm64" ;; \
         *) echo "ERROR: Unsupported architecture: ${TARGETARCH}" >&2 && exit 1 ;; esac) \
    && EXPECTED_CHECKSUM=$(case "${TARGETARCH}" in \
         "amd64") echo "ba46ba7ce3a4d7236b16fbe44419fb453bc08f866b24f04d549ec89f1722a29e" ;; \
         "arm64") echo "62cc5735d63057d8d07441507c3d6974e90c1854bdb33e9c8b26c0da086336e1" ;; esac) \
    && curl -fsSL -o actions-runner.tar.gz \
       "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH_DOWNLOAD}-${RUNNER_VERSION}.tar.gz" \
    && echo "${EXPECTED_CHECKSUM}  actions-runner.tar.gz" | sha256sum -c - \
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
