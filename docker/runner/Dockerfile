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
FROM ubuntu:22.04

ARG RUNNER_VERSION
ARG TARGETARCH
ARG RUNNER_ARCH=${TARGETARCH}

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

# Create runner user
RUN useradd -m -s /bin/bash runner \
    && echo "runner ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

# Download and install GitHub Actions runner
RUN RUNNER_ARCH_DOWNLOAD=$(case "${TARGETARCH}" in "amd64") echo "x64" ;; "arm64") echo "arm64" ;; *) echo "x64" ;; esac) \
    && curl -fsSL -o actions-runner.tar.gz \
       "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH_DOWNLOAD}-${RUNNER_VERSION}.tar.gz" \
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

USER runner

ENTRYPOINT ["/entrypoint.sh"]
