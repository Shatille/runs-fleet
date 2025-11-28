# Multi-stage build for runs-fleet server and agent
# Optimized for cross-compilation with buildx

# Stage 1: Build Go binaries
# Use BUILDPLATFORM to run Go compiler natively (not under QEMU)
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder

ARG TARGETARCH
ARG VERSION=dev

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git ca-certificates

# Copy go mod files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build server binary for target architecture (cross-compile)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /bin/runs-fleet-server ./cmd/server

# Build agent binaries for both architectures
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /bin/runs-fleet-agent-linux-amd64 ./cmd/agent

RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /bin/runs-fleet-agent-linux-arm64 ./cmd/agent

# Stage 2: Final runtime image
FROM alpine:3.19

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Copy binaries from builder
COPY --from=builder /bin/runs-fleet-server /app/server
COPY --from=builder /bin/runs-fleet-agent-linux-amd64 /app/agent-linux-amd64
COPY --from=builder /bin/runs-fleet-agent-linux-arm64 /app/agent-linux-arm64

# Create non-root user
RUN addgroup -g 1000 runs-fleet && \
    adduser -D -u 1000 -G runs-fleet runs-fleet

USER runs-fleet

# Expose HTTP port
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# Run server
ENTRYPOINT ["/app/server"]
