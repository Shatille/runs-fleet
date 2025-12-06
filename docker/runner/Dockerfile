# runs-fleet runner image
# Contains GitHub Actions runner + runs-fleet agent
# Multi-arch: supports amd64 and arm64

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
# Pin to specific digest for reproducible builds (ubuntu:22.04 as of 2024-12)
FROM ubuntu:22.04@sha256:0e5e4a57c2499249aafc3b40fcd541e9a456aab7296681a3994d631587203f97

ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive
ENV RUNNER_ALLOW_RUNASROOT=1

WORKDIR /home/runner

# Install dependencies
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

# Download and install latest GitHub Actions runner
# Fetches latest version from GitHub API and verifies checksum from release
RUN RUNNER_ARCH=$(case "${TARGETARCH}" in \
         "amd64") echo "x64" ;; \
         "arm64") echo "arm64" ;; \
         *) echo "ERROR: Unsupported architecture: ${TARGETARCH}" >&2 && exit 1 ;; esac) \
    && RUNNER_VERSION=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | jq -r '.tag_name' | sed 's/^v//') \
    && echo "Installing GitHub Actions runner v${RUNNER_VERSION} for ${RUNNER_ARCH}" \
    && curl -fsSL -o actions-runner.tar.gz \
       "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" \
    && curl -fsSL -o checksums.txt \
       "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz.sha256" \
    && echo "$(cat checksums.txt)  actions-runner.tar.gz" | sha256sum -c - \
    && tar xzf actions-runner.tar.gz \
    && rm actions-runner.tar.gz checksums.txt \
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
