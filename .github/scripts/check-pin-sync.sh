#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
provision="${repo_root}/packer/provision-base.sh"
dockerfile="${repo_root}/docker/runner/Dockerfile"
flake="${repo_root}/flake.nix"
ci_workflow="${repo_root}/.github/workflows/ci.yml"

dockerfile_arg() {
  sed -n "s/^ARG $1=\(.*\)$/\1/p" "$dockerfile" | head -n1
}

provision_var() {
  sed -n "s/^$1=\"\([^\"]*\)\".*/\1/p" "$provision" | head -n1
}

provision_buildx_sha() {
  grep -A1 "BUILDX_ARCH=\"$1\"" "$provision" | grep -o '[0-9a-f]\{64\}' | head -n1
}

provision_compose_sha() {
  grep "$1)" "$provision" | grep -o '[0-9a-f]\{64\}' | head -n1
}

flake_golangci_version() {
  sed -n 's/^ *golangciLintVersion = "\([^"]*\)";.*/\1/p' "$flake" | head -n1
}

ci_golangci_version() {
  grep -o 'install\.sh | sh -s -- -b \./bin v[0-9][0-9.]*' "$ci_workflow" \
    | grep -o 'v[0-9][0-9.]*' | head -n1 | sed 's/^v//'
}

# The tool cache key embeds the version too; a stale key restores the previous
# binary and silently defeats a version bump.
ci_golangci_cache_version() {
  grep -o 'golangci-v[0-9][0-9.]*' "$ci_workflow" | head -n1 | sed 's/^golangci-v//'
}

fail=0
check() {
  local name="$1" dockerfile_value="$2" provision_value="$3"
  if [ -z "$dockerfile_value" ] || [ -z "$provision_value" ]; then
    echo "FAIL ${name}: extraction failed (Dockerfile='${dockerfile_value}', provision-base.sh='${provision_value}')"
    fail=1
  elif [ "$dockerfile_value" != "$provision_value" ]; then
    echo "FAIL ${name}: docker/runner/Dockerfile pins '${dockerfile_value}' but packer/provision-base.sh pins '${provision_value}'"
    fail=1
  else
    echo "ok   ${name} = ${dockerfile_value}"
  fi
}

check "buildx version"     "$(dockerfile_arg BUILDX_VERSION)"       "$(provision_var BUILDX_VERSION)"
check "buildx sha (amd64)" "$(dockerfile_arg BUILDX_SHA256_AMD64)"  "$(provision_buildx_sha amd64)"
check "buildx sha (arm64)" "$(dockerfile_arg BUILDX_SHA256_ARM64)"  "$(provision_buildx_sha arm64)"
check "compose version"     "$(dockerfile_arg COMPOSE_VERSION)"      "$(provision_var DOCKER_COMPOSE_VERSION)"
check "compose sha (amd64)" "$(dockerfile_arg COMPOSE_SHA256_AMD64)" "$(provision_compose_sha x86_64)"
check "compose sha (arm64)" "$(dockerfile_arg COMPOSE_SHA256_ARM64)" "$(provision_compose_sha aarch64)"

# A local linter that disagrees with CI's is worse than no local linter: the
# versions report different findings on the same tree.
check_named() {
  local name="$1" a_label="$2" a="$3" b_label="$4" b="$5"
  if [ -z "$a" ] || [ -z "$b" ]; then
    echo "FAIL ${name}: extraction failed (${a_label}='${a}', ${b_label}='${b}')"
    fail=1
  elif [ "$a" != "$b" ]; then
    echo "FAIL ${name}: ${a_label} pins '${a}' but ${b_label} pins '${b}'"
    fail=1
  else
    echo "ok   ${name} = ${a}"
  fi
}

check_named "golangci-lint version" \
  "flake.nix" "$(flake_golangci_version)" \
  ".github/workflows/ci.yml" "$(ci_golangci_version)"

check_named "golangci-lint cache key" \
  "ci.yml install step" "$(ci_golangci_version)" \
  "ci.yml cache key" "$(ci_golangci_cache_version)"

exit "$fail"
