# Enable parallel builds by default (arm64 and amd64 simultaneously)
MAKEFLAGS += -j2

.PHONY: base base-arm64 base-amd64 runs-fleet runs-fleet-arm64 runs-fleet-amd64 \
	base-arm64-init base-arm64-validate base-arm64-build \
	base-amd64-init base-amd64-validate base-amd64-build \
	runs-fleet-arm64-init runs-fleet-arm64-validate runs-fleet-arm64-build \
	runs-fleet-amd64-init runs-fleet-amd64-validate runs-fleet-amd64-build \
	ami-list clean help all

help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Build targets:"
	@echo "  base            Build base AMIs (both arm64 and amd64)"
	@echo "  base-arm64      Build runner-base AMI for ARM64"
	@echo "  base-amd64        Build runner-base AMI for amd64"
	@echo "  runs-fleet      Build runs-fleet-runner AMIs (both arm64 and amd64)"
	@echo "  runs-fleet-arm64  Build runs-fleet-runner AMI for ARM64"
	@echo "  runs-fleet-amd64    Build runs-fleet-runner AMI for amd64"
	@echo "  all             Build all AMIs (base + runs-fleet, both archs)"
	@echo ""
	@echo "AMI management:"
	@echo "  ami-list  List all AMIs"
	@echo "  clean     Remove old AMIs (keep 2 app, 1 base per arch)"
	@echo ""
	@echo "Parallel builds:"
	@echo "  make -j2 base       Build both base AMIs in parallel"
	@echo "  make -j2 runs-fleet Build both runs-fleet AMIs in parallel"
	@echo ""
	@echo "Build order: make base && make runs-fleet"

BASE_ARM64_TEMPLATE := runner-base-arm64.pkr.hcl
BASE_AMD64_TEMPLATE := runner-base-amd64.pkr.hcl
RUNS_FLEET_ARM64_TEMPLATE := runs-fleet-runner-arm64.pkr.hcl
RUNS_FLEET_AMD64_TEMPLATE := runs-fleet-runner-amd64.pkr.hcl

# The base templates require runner_version/runner_sha256 with no defaults; CI
# supplies them from the "Resolve actions/runner version" step. For local builds
# resolve the same way, so `make base` works without hand-copying digests.
# Override by exporting PKR_VAR_runner_version / PKR_VAR_runner_sha256.
RUNNER_MIN_VERSION := $(shell tr -d '[:space:]' < ../.github/RUNNER_MIN_VERSION)
define resolve_runner
	if [ -z "$$PKR_VAR_runner_version" ] || [ -z "$$PKR_VAR_runner_sha256" ]; then \
		release="$$(gh api repos/actions/runner/releases/latest)" || exit 1; \
		PKR_VAR_runner_version="$$(printf '%s' "$$release" | jq -r '.tag_name | ltrimstr("v")')"; \
		PKR_VAR_runner_sha256="$$(printf '%s' "$$release" | jq -r '.body' \
			| grep -oE "BEGIN SHA linux-$(1) -->[0-9a-f]{64}" | grep -oE '[0-9a-f]{64}' | head -1)"; \
		if [ -z "$$PKR_VAR_runner_version" ] || [ -z "$$PKR_VAR_runner_sha256" ]; then \
			echo "failed to resolve actions/runner version or linux-$(1) checksum" >&2; exit 1; \
		fi; \
		if [ "$$(printf '%s\n%s\n' "$$PKR_VAR_runner_version" "$(RUNNER_MIN_VERSION)" | sort -V | head -1)" \
			!= "$(RUNNER_MIN_VERSION)" ]; then \
			echo "resolved $$PKR_VAR_runner_version is below the floor $(RUNNER_MIN_VERSION)" >&2; exit 1; \
		fi; \
		export PKR_VAR_runner_version PKR_VAR_runner_sha256; \
	fi
endef

# Build all
all: base runs-fleet

# Base AMIs (both architectures)
base: base-arm64 base-amd64

# Runs Fleet Runner AMIs (both architectures)
runs-fleet: runs-fleet-arm64 runs-fleet-amd64

# Base AMI ARM64
# Chain: init → validate → build (sequential within target, parallel across targets with -j)
base-arm64: base-arm64-build

base-arm64-init:
	packer init $(BASE_ARM64_TEMPLATE)

base-arm64-validate: base-arm64-init
	@$(call resolve_runner,arm64); \
	packer validate $(BASE_ARM64_TEMPLATE)

base-arm64-build: base-arm64-validate
	@$(call resolve_runner,arm64); \
	packer build $(BASE_ARM64_TEMPLATE)

# Base AMI amd64
base-amd64: base-amd64-build

base-amd64-init:
	packer init $(BASE_AMD64_TEMPLATE)

base-amd64-validate: base-amd64-init
	@$(call resolve_runner,x64); \
	packer validate $(BASE_AMD64_TEMPLATE)

base-amd64-build: base-amd64-validate
	@$(call resolve_runner,x64); \
	packer build $(BASE_AMD64_TEMPLATE)

# Runs Fleet Runner AMI ARM64
runs-fleet-arm64: runs-fleet-arm64-build

runs-fleet-arm64-init:
	packer init $(RUNS_FLEET_ARM64_TEMPLATE)

runs-fleet-arm64-validate: runs-fleet-arm64-init
	packer validate $(RUNS_FLEET_ARM64_TEMPLATE)

runs-fleet-arm64-build: runs-fleet-arm64-validate
	packer build $(RUNS_FLEET_ARM64_TEMPLATE)

# Runs Fleet Runner AMI amd64
runs-fleet-amd64: runs-fleet-amd64-build

runs-fleet-amd64-init:
	packer init $(RUNS_FLEET_AMD64_TEMPLATE)

runs-fleet-amd64-validate: runs-fleet-amd64-init
	packer validate $(RUNS_FLEET_AMD64_TEMPLATE)

runs-fleet-amd64-build: runs-fleet-amd64-validate
	packer build $(RUNS_FLEET_AMD64_TEMPLATE)

# AMI management
ami-list:
	@echo "==> Base AMIs (ARM64):"
	@aws ec2 describe-images \
		--owners self \
		--filters "Name=name,Values=runner-base-arm64-*" \
		--query 'sort_by(Images, &CreationDate)[*].[ImageId,Name,CreationDate]' \
		--output table
	@echo ""
	@echo "==> Base AMIs (amd64):"
	@aws ec2 describe-images \
		--owners self \
		--filters "Name=name,Values=runner-base-amd64-*" \
		--query 'sort_by(Images, &CreationDate)[*].[ImageId,Name,CreationDate]' \
		--output table
	@echo ""
	@echo "==> Runs Fleet Runner AMIs (ARM64):"
	@aws ec2 describe-images \
		--owners self \
		--filters "Name=name,Values=runs-fleet-runner-arm64-*" \
		--query 'sort_by(Images, &CreationDate)[*].[ImageId,Name,CreationDate]' \
		--output table
	@echo ""
	@echo "==> Runs Fleet Runner AMIs (amd64):"
	@aws ec2 describe-images \
		--owners self \
		--filters "Name=name,Values=runs-fleet-runner-amd64-*" \
		--query 'sort_by(Images, &CreationDate)[*].[ImageId,Name,CreationDate]' \
		--output table

clean:
	@echo "==> Cleaning up old runs-fleet-runner ARM64 AMIs (keeping latest 2)"
	@aws ec2 describe-images \
		--owners self \
		--filters "Name=name,Values=runs-fleet-runner-arm64-*" \
		--query 'sort_by(Images, &CreationDate)[:-2].[ImageId]' \
		--output text | grep -v '^None$$' | xargs -r -n1 sh -c 'echo "Deregistering $$0" && aws ec2 deregister-image --image-id $$0'
	@echo ""
	@echo "==> Cleaning up old runs-fleet-runner amd64 AMIs (keeping latest 2)"
	@aws ec2 describe-images \
		--owners self \
		--filters "Name=name,Values=runs-fleet-runner-amd64-*" \
		--query 'sort_by(Images, &CreationDate)[:-2].[ImageId]' \
		--output text | grep -v '^None$$' | xargs -r -n1 sh -c 'echo "Deregistering $$0" && aws ec2 deregister-image --image-id $$0'
	@echo ""
	@echo "==> Cleaning up old runner-base ARM64 AMIs (keeping latest 1)"
	@aws ec2 describe-images \
		--owners self \
		--filters "Name=name,Values=runner-base-arm64-*" \
		--query 'sort_by(Images, &CreationDate)[:-1].[ImageId]' \
		--output text | grep -v '^None$$' | xargs -r -n1 sh -c 'echo "Deregistering $$0" && aws ec2 deregister-image --image-id $$0'
	@echo ""
	@echo "==> Cleaning up old runner-base amd64 AMIs (keeping latest 1)"
	@aws ec2 describe-images \
		--owners self \
		--filters "Name=name,Values=runner-base-amd64-*" \
		--query 'sort_by(Images, &CreationDate)[:-1].[ImageId]' \
		--output text | grep -v '^None$$' | xargs -r -n1 sh -c 'echo "Deregistering $$0" && aws ec2 deregister-image --image-id $$0'
