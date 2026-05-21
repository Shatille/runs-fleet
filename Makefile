.PHONY: init build test coverage lint clean docker-build docker-push docker-build-runner docker-push-runner build-admin-ui scan-runner sbom-runner

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

# Build admin UI
build-admin-ui:
	@echo "Building admin UI..."
	cd pkg/admin/ui && npm ci && npm run build

# Build server binary
build-server: build-admin-ui
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
	rm -rf pkg/admin/ui/.next pkg/admin/ui/out pkg/admin/ui/node_modules

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
RUNNER_BASE_TAG?=2.334.0

# Build runner image for local architecture only (for testing)
docker-build-runner:
	@echo "Building runner Docker image (local arch)..."
	$(CONTAINER_CLI) build \
		--build-arg RUNNER_BASE_TAG=$(RUNNER_BASE_TAG) \
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
		--build-arg RUNNER_BASE_TAG=$(RUNNER_BASE_TAG) \
		--build-arg VERSION=$(RUNNER_TAG) \
		-f docker/runner/Dockerfile \
		-t $(ECR_REGISTRY)/$(RUNNER_IMAGE):$(RUNNER_TAG)-amd64 \
		--push \
		.

_build-runner-arm64:
	$(CONTAINER_CLI) buildx build \
		--platform linux/arm64 \
		--build-arg RUNNER_BASE_TAG=$(RUNNER_BASE_TAG) \
		--build-arg VERSION=$(RUNNER_TAG) \
		-f docker/runner/Dockerfile \
		-t $(ECR_REGISTRY)/$(RUNNER_IMAGE):$(RUNNER_TAG)-arm64 \
		--push \
		.


# Scan the runner image with Trivy via container so no host install is needed.
# Saves the image to a tarball under bin/ and runs Trivy against it. HIGH/CRITICAL
# only by default; override SEVERITY=HIGH,CRITICAL,MEDIUM,LOW for a full report.
TRIVY_IMAGE?=aquasec/trivy:0.70.0
RUNNER_TAR=bin/runs-fleet-runner.tar

scan-runner: docker-build-runner
	@mkdir -p bin
	@echo "Saving image to $(RUNNER_TAR)..."
	$(CONTAINER_CLI) save $(RUNNER_IMAGE):$(RUNNER_TAG) -o $(RUNNER_TAR)
	@echo "Scanning with Trivy via $(TRIVY_IMAGE) using .trivy/trivy.yaml + VEX..."
	$(CONTAINER_CLI) run --rm \
		-v $(PWD)/bin:/work:ro \
		-v $(PWD)/.trivy:/trivy:ro \
		$(TRIVY_IMAGE) image \
			--config /trivy/trivy.yaml \
			--vex /trivy/vex.json \
			--ignore-unfixed \
			--exit-code 1 \
			--no-progress \
			--input /work/runs-fleet-runner.tar

# Generate a CycloneDX SBOM for the runner image. Output: bin/runs-fleet-runner.sbom.json
sbom-runner: docker-build-runner
	@mkdir -p bin
	@echo "Saving image to $(RUNNER_TAR)..."
	$(CONTAINER_CLI) save $(RUNNER_IMAGE):$(RUNNER_TAG) -o $(RUNNER_TAR)
	@echo "Generating CycloneDX SBOM via $(TRIVY_IMAGE)..."
	$(CONTAINER_CLI) run --rm -v $(PWD)/bin:/work $(TRIVY_IMAGE) image \
		--format cyclonedx --no-progress \
		--input /work/runs-fleet-runner.tar \
		-o /work/runs-fleet-runner.sbom.json
	@echo "SBOM written to bin/runs-fleet-runner.sbom.json"

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
	@echo "  build-admin-ui          - Build admin UI (Next.js static export)"
	@echo "  build-server            - Build server binary (includes UI)"
	@echo "  build                   - Build all binaries"
	@echo "  test                    - Run tests"
	@echo "  coverage                - Run tests with coverage"
	@echo "  lint                    - Run golangci-lint"
	@echo "  clean                   - Remove build artifacts"
	@echo "  docker-build            - Build orchestrator Docker image"
	@echo "  docker-push             - Build and push orchestrator image to ECR"
	@echo "  docker-build-runner     - Build runner Docker image (local arch)"
	@echo "  docker-push-runner      - Build and push runner image to ECR (multi-arch)"
	@echo "  scan-runner             - Build + Trivy scan the runner image (HIGH/CRITICAL)"
	@echo "  sbom-runner             - Build + generate CycloneDX SBOM for the runner image"
	@echo "  run-server              - Run server locally"
	@echo "  deps                    - Update dependencies"
	@echo "  ci                      - Run full CI pipeline"
	@echo "  help                    - Show this help"
