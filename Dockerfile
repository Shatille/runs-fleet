# Multi-stage build for runs-fleet orchestrator server
# Optimized for cross-compilation with buildx

# Stage 1: Build admin UI
FROM --platform=$BUILDPLATFORM node:22-alpine AS ui-builder

WORKDIR /build/pkg/admin/ui

# Copy UI source and build
COPY pkg/admin/ui/package*.json ./
RUN npm ci
COPY pkg/admin/ui/ ./
RUN npm run build

# Stage 2: Build server binary
# Use BUILDPLATFORM to run Go compiler natively (not under QEMU)
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

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

# Copy built UI from ui-builder stage
COPY --from=ui-builder /build/pkg/admin/ui/out ./pkg/admin/ui/out

# Build server binary for target architecture (cross-compile)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /bin/runs-fleet-server ./cmd/server

# Stage 3: Final runtime image
FROM alpine:3.19

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Copy server binary from builder
COPY --from=builder /bin/runs-fleet-server /app/server

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
