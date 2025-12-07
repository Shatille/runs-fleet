.PHONY: init build test coverage lint clean docker-build docker-push docker-build-runner docker-push-runner

# Variables
BINARY_SERVER=bin/runs-fleet-server
CONTAINER_CLI?=$(shell command -v podman 2>/dev/null || echo docker)
DOCKER_IMAGE?=runs-fleet
DOCKER_TAG?=latest
AWS_REGION?=ap-northeast-1
AWS_ACCOUNT_ID?=$(shell aws sts get-caller-identity --query Account --output text)
ECR_REGISTRY?=$(AWS_ACCOUNT_ID).dkr.ecr.$(AWS_REGION).amazonaws.com
CPUS?=$(shell nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)

# Initialize project
init:
	@echo "Initializing project..."
	@mkdir -p bin
	go mod download
	go mod verify
	@echo "Project initialized successfully"

# Build server binary
build-server:
	@echo "Building server..."
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux go build -ldflags '-extldflags "-static"' \
		-o $(BINARY_SERVER) ./cmd/server

# Build all binaries (alias for build-server)
build: build-server

# Run tests
test:
	@echo "Running tests with $(CPUS) CPUs..."
	go test -race -parallel=$(CPUS) ./...

# Run tests with coverage
coverage:
	@echo "Running tests with coverage ($(CPUS) CPUs)..."
	go test -race -parallel=$(CPUS) -coverprofile=coverage.out -covermode=atomic ./...

# Run linter
LINT_TIMEOUT?=5m
lint:
	@echo "Running linter with $(CPUS) CPUs..."
	CGO_ENABLED=0 golangci-lint run --concurrency=$(CPUS) --timeout=$(LINT_TIMEOUT)

# Clean build artifacts
clean:
	@echo "Cleaning..."
	rm -rf bin/
	rm -f coverage.out

# Build Docker image
docker-build:
	@echo "Building Docker image..."
	$(CONTAINER_CLI) build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

# Push to ECR
docker-push: docker-build
	@echo "Logging into ECR..."
	aws ecr get-login-password --region $(AWS_REGION) | \
		$(CONTAINER_CLI) login --username AWS --password-stdin $(ECR_REGISTRY)
	@echo "Tagging image..."
	$(CONTAINER_CLI) tag $(DOCKER_IMAGE):$(DOCKER_TAG) $(ECR_REGISTRY)/$(DOCKER_IMAGE):$(DOCKER_TAG)
	@echo "Pushing to ECR..."
	$(CONTAINER_CLI) push $(ECR_REGISTRY)/$(DOCKER_IMAGE):$(DOCKER_TAG)

# Build runner Docker image
RUNNER_IMAGE?=runs-fleet-runner
RUNNER_TAG?=latest
RUNNER_VERSION?=2.330.0

# Build runner image for local architecture only (for testing)
docker-build-runner:
	@echo "Building runner Docker image (local arch)..."
	$(CONTAINER_CLI) build \
		--build-arg RUNNER_VERSION=$(RUNNER_VERSION) \
		--build-arg VERSION=$(RUNNER_TAG) \
		-f docker/runner/Dockerfile \
		-t $(RUNNER_IMAGE):$(RUNNER_TAG) \
		.

# Push runner image to ECR (multi-arch build with fast-fail)
# Builds architectures in parallel, fails immediately if either fails
docker-push-runner:
	@echo "Logging into ECR..."
	aws ecr get-login-password --region $(AWS_REGION) | \
		$(CONTAINER_CLI) login --username AWS --password-stdin $(ECR_REGISTRY)
	@echo "Building runner Docker image (multi-arch with fast-fail)..."
	@$(MAKE) -j2 --output-sync=target \
		_build-runner-amd64 \
		_build-runner-arm64
	@echo "Creating and pushing multi-arch manifest..."
	$(CONTAINER_CLI) buildx imagetools create -t $(ECR_REGISTRY)/$(RUNNER_IMAGE):$(RUNNER_TAG) \
		$(ECR_REGISTRY)/$(RUNNER_IMAGE):$(RUNNER_TAG)-amd64 \
		$(ECR_REGISTRY)/$(RUNNER_IMAGE):$(RUNNER_TAG)-arm64

_build-runner-amd64:
	$(CONTAINER_CLI) buildx build \
		--platform linux/amd64 \
		--build-arg RUNNER_VERSION=$(RUNNER_VERSION) \
		--build-arg VERSION=$(RUNNER_TAG) \
		-f docker/runner/Dockerfile \
		-t $(ECR_REGISTRY)/$(RUNNER_IMAGE):$(RUNNER_TAG)-amd64 \
		--push \
		.

_build-runner-arm64:
	$(CONTAINER_CLI) buildx build \
		--platform linux/arm64 \
		--build-arg RUNNER_VERSION=$(RUNNER_VERSION) \
		--build-arg VERSION=$(RUNNER_TAG) \
		-f docker/runner/Dockerfile \
		-t $(ECR_REGISTRY)/$(RUNNER_IMAGE):$(RUNNER_TAG)-arm64 \
		--push \
		.


# Run server locally
run-server:
	@echo "Running server locally..."
	go run cmd/server/main.go

# Update dependencies
deps:
	@echo "Updating dependencies..."
	go mod tidy
	go mod verify

# Generate mocks (if using mockgen)
mocks:
	@echo "Generating mocks..."
	go generate ./...

# Full CI pipeline
ci: deps lint test build

# Help
help:
	@echo "Available targets:"
	@echo "  init                    - Initialize project (download deps, setup)"
	@echo "  build-server            - Build server binary"
	@echo "  build                   - Build all binaries"
	@echo "  test                    - Run tests"
	@echo "  coverage                - Run tests with coverage"
	@echo "  lint                    - Run golangci-lint"
	@echo "  clean                   - Remove build artifacts"
	@echo "  docker-build            - Build orchestrator Docker image"
	@echo "  docker-push             - Build and push orchestrator image to ECR"
	@echo "  docker-build-runner     - Build runner Docker image (local arch)"
	@echo "  docker-push-runner      - Build and push runner image to ECR (multi-arch)"
	@echo "  run-server              - Run server locally"
	@echo "  deps                    - Update dependencies"
	@echo "  ci                      - Run full CI pipeline"
	@echo "  help                    - Show this help"
