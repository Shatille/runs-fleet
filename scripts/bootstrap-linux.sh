#!/bin/bash
# runs-fleet Linux bootstrap script (container-based)
# Runs on EC2 instance boot via user-data to execute GitHub Actions jobs.
#
# Prerequisites:
# - Amazon Linux 2023 AMI (Docker pre-installed)
# - IAM role with ECR pull, SSM read, EC2 terminate, SQS send permissions
# - Instance tags set by orchestrator with runner configuration
#
# Security Notes:
# - Container runs with --privileged and docker.sock mount to support GitHub Actions
#   workflows that build/run containers (Docker-in-Docker). This grants the runner
#   container near-root access to the host. Workflows from untrusted sources should
#   NOT be run on these runners. Use repository/organization runner groups to restrict
#   which repositories can use these runners.
# - Runner image is pulled from ECR using instance IAM role credentials. The image
#   URL comes from instance tags set by the orchestrator, ensuring only orchestrator-
#   managed instances can run authorized images.

set -euo pipefail

LOG_FILE="/var/log/runs-fleet-bootstrap.log"
exec > >(tee -a "$LOG_FILE") 2>&1

log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

# Global variables
IMDS_TOKEN=""
INSTANCE_ID=""
REGION=""
ACCOUNT_ID=""
TERMINATION_QUEUE_URL=""

# Send failure notification to termination queue
notify_failure() {
    local error_msg="$1"
    if [[ -n "$TERMINATION_QUEUE_URL" && -n "$INSTANCE_ID" && -n "$REGION" ]]; then
        local message
        # Use jq or python3 for safe JSON encoding to prevent injection
        if command -v jq &>/dev/null; then
            message=$(jq -n \
                --arg id "$INSTANCE_ID" \
                --arg status "bootstrap_failed" \
                --arg err "$error_msg" \
                '{instance_id: $id, status: $status, error: $err}')
        elif command -v python3 &>/dev/null; then
            # Pass variables as arguments to avoid shell injection
            message=$(python3 -c 'import sys, json; print(json.dumps({"instance_id": sys.argv[1], "status": "bootstrap_failed", "error": sys.argv[2]}))' "$INSTANCE_ID" "$error_msg" 2>/dev/null)
        else
            # Fallback: escape all JSON metacharacters
            local escaped_msg="${error_msg//\\/\\\\}"  # backslash first
            escaped_msg="${escaped_msg//$'\n'/\\n}"    # newlines
            escaped_msg="${escaped_msg//$'\r'/\\r}"    # carriage returns
            escaped_msg="${escaped_msg//$'\t'/\\t}"    # tabs
            escaped_msg="${escaped_msg//\"/\\\"}"      # quotes
            message="{\"instance_id\":\"$INSTANCE_ID\",\"status\":\"bootstrap_failed\",\"error\":\"$escaped_msg\"}"
        fi
        aws sqs send-message \
            --queue-url "$TERMINATION_QUEUE_URL" \
            --message-body "$message" \
            --message-group-id "$INSTANCE_ID" \
            --region "$REGION" 2>/dev/null || true
        log "Sent failure notification to termination queue"
    fi
}

# Exit with error and notify
fail() {
    local error_msg="$1"
    log "ERROR: $error_msg"
    notify_failure "$error_msg"
    exit 1
}

# Get IMDSv2 token with error handling
get_imds_token() {
    local token
    token=$(curl -sS --fail -X PUT "http://169.254.169.254/latest/api/token" \
        -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" 2>/dev/null) || return 1
    if [[ -z "$token" ]]; then
        return 1
    fi
    echo "$token"
}

# Initialize IMDS token (call once at startup)
init_imds_token() {
    IMDS_TOKEN=$(get_imds_token) || {
        log "ERROR: Failed to get IMDS token - IMDSv2 may not be available"
        exit 1
    }
    if [[ -z "$IMDS_TOKEN" ]]; then
        log "ERROR: IMDS token is empty"
        exit 1
    fi
}

# Get instance metadata using cached token
get_metadata() {
    local path=$1
    curl -sS --fail -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" \
        "http://169.254.169.254/latest/meta-data/$path" 2>/dev/null || echo ""
}

# Get instance tag with retry for eventual consistency
get_instance_tag_with_retry() {
    local tag_name=$1
    local max_attempts=${2:-5}
    local attempt=1
    local value=""

    while [[ $attempt -le $max_attempts ]]; do
        value=$(curl -sS -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" \
            "http://169.254.169.254/latest/meta-data/tags/instance/$tag_name" 2>/dev/null || echo "")

        if [[ -n "$value" ]]; then
            echo "$value"
            return 0
        fi

        if [[ $attempt -lt $max_attempts ]]; then
            local sleep_time=$((2 ** attempt))
            log "Tag '$tag_name' not found, retrying in ${sleep_time}s (attempt $attempt/$max_attempts)..."
            sleep "$sleep_time"
        fi
        ((attempt++))
    done

    echo ""
    return 1
}

# Validate JSON string
is_valid_json() {
    local json="$1"
    if command -v jq &>/dev/null; then
        echo "$json" | jq -e . >/dev/null 2>&1
    elif command -v python3 &>/dev/null; then
        echo "$json" | python3 -c "import sys, json; json.load(sys.stdin)" 2>/dev/null
    else
        # Cannot validate without jq or python3, assume valid
        return 0
    fi
}

log "=== runs-fleet Linux Bootstrap Starting ==="

# Initialize IMDS token first
init_imds_token

# Gather instance metadata
INSTANCE_ID=$(get_metadata "instance-id")
if [[ -z "$INSTANCE_ID" ]]; then
    fail "Failed to get instance ID from IMDS"
fi

REGION=$(get_metadata "placement/region")
if [[ -z "$REGION" ]]; then
    fail "Failed to get region from IMDS"
fi

# Get Account ID from identity document with JSON validation
IDENTITY_DOC=$(get_metadata "identity-credentials/ec2/info")
if [[ -z "$IDENTITY_DOC" ]]; then
    fail "Failed to get identity document from IMDS"
fi

if ! is_valid_json "$IDENTITY_DOC"; then
    fail "Identity document is not valid JSON"
fi

if command -v jq &>/dev/null; then
    ACCOUNT_ID=$(echo "$IDENTITY_DOC" | jq -r '.AccountId // empty' 2>/dev/null || echo "")
elif command -v python3 &>/dev/null; then
    ACCOUNT_ID=$(echo "$IDENTITY_DOC" | python3 -c "import sys, json; d=json.load(sys.stdin); print(d.get('AccountId', ''))" 2>/dev/null || echo "")
else
    fail "Neither jq nor python3 available for JSON parsing"
fi

if [[ -z "$ACCOUNT_ID" || ! "$ACCOUNT_ID" =~ ^[0-9]{12}$ ]]; then
    fail "Failed to get valid AWS Account ID (got: '$ACCOUNT_ID')"
fi

log "Instance: $INSTANCE_ID in $REGION (account: $ACCOUNT_ID)"

# Get configuration from instance tags (set by orchestrator)
# Tags are the authoritative source - no SSM fallback to prevent configuration bypass
log "Reading configuration from instance tags..."

TERMINATION_QUEUE_URL=$(get_instance_tag_with_retry "runs-fleet:termination-queue-url" 3) || true
RUN_ID=$(get_instance_tag_with_retry "runs-fleet:run-id" 10) || fail "Run ID not found in instance tags after retries"
RUNNER_IMAGE=$(get_instance_tag_with_retry "runs-fleet:runner-image" 10) || fail "Runner image not found in instance tags after retries"
CACHE_URL=$(get_instance_tag_with_retry "runs-fleet:cache-url" 3) || true

# Secrets backend configuration (for Vault support)
SECRETS_BACKEND=$(get_instance_tag_with_retry "runs-fleet:secrets-backend" 3) || true
VAULT_ADDR=$(get_instance_tag_with_retry "runs-fleet:vault-addr" 3) || true
VAULT_KV_MOUNT=$(get_instance_tag_with_retry "runs-fleet:vault-kv-mount" 3) || true
VAULT_KV_VERSION=$(get_instance_tag_with_retry "runs-fleet:vault-kv-version" 3) || true
VAULT_BASE_PATH=$(get_instance_tag_with_retry "runs-fleet:vault-base-path" 3) || true
VAULT_AUTH_METHOD=$(get_instance_tag_with_retry "runs-fleet:vault-auth-method" 3) || true
VAULT_AWS_ROLE=$(get_instance_tag_with_retry "runs-fleet:vault-aws-role" 3) || true

if [[ -z "$RUN_ID" ]]; then
    fail "Run ID not found in instance tags"
fi

if [[ -z "$RUNNER_IMAGE" ]]; then
    fail "Runner image not configured in instance tags"
fi

# Validate runner image URL format matches Go orchestrator validation
# Pattern: <12-digit-account>.dkr.ecr.<region>.amazonaws.com/<repo>[:<tag>][@sha256:<digest>]
ECR_URL_PATTERN="^[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com/[a-z0-9][a-z0-9._/-]*[a-z0-9](:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$"
if [[ ! "$RUNNER_IMAGE" =~ $ECR_URL_PATTERN ]]; then
    fail "Invalid ECR URL format: $RUNNER_IMAGE"
fi

# Verify image is from this account's ECR (defense in depth)
EXPECTED_ECR_PREFIX="${ACCOUNT_ID}.dkr.ecr."
if [[ ! "$RUNNER_IMAGE" =~ ^${EXPECTED_ECR_PREFIX} ]]; then
    fail "Runner image must be from account ECR (expected prefix: $EXPECTED_ECR_PREFIX, got: $RUNNER_IMAGE)"
fi

log "Configuration:"
log "  RUN_ID: $RUN_ID"
log "  RUNNER_IMAGE: $RUNNER_IMAGE"
log "  TERMINATION_QUEUE_URL: ${TERMINATION_QUEUE_URL:-not set}"
log "  SECRETS_BACKEND: ${SECRETS_BACKEND:-ssm}"
if [[ "${SECRETS_BACKEND:-}" == "vault" ]]; then
    log "  VAULT_ADDR: ${VAULT_ADDR:-not set}"
    log "  VAULT_AUTH_METHOD: ${VAULT_AUTH_METHOD:-aws}"
fi

# Start Docker with exponential backoff
start_docker() {
    if systemctl is-active --quiet docker; then
        return 0
    fi

    log "Starting Docker..."
    systemctl start docker

    local max_attempts=6
    local attempt=1
    while [[ $attempt -le $max_attempts ]]; do
        if systemctl is-active --quiet docker; then
            log "Docker started successfully"
            return 0
        fi
        local sleep_time=$((2 ** attempt))
        log "Waiting for Docker to start (attempt $attempt/$max_attempts, next retry in ${sleep_time}s)..."
        sleep "$sleep_time"
        ((attempt++))
    done

    return 1
}

if ! start_docker; then
    fail "Docker failed to start after retries"
fi

# Validate and authenticate to ECR
ECR_REGISTRY="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com"

if [[ ! "$ECR_REGISTRY" =~ ^[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com$ ]]; then
    fail "Invalid ECR registry format: $ECR_REGISTRY"
fi

log "Authenticating to ECR: $ECR_REGISTRY"
if ! aws ecr get-login-password --region "$REGION" 2>/dev/null | \
    docker login --username AWS --password-stdin "$ECR_REGISTRY" >/dev/null 2>&1; then
    fail "ECR authentication failed"
fi

# Pull runner image with error handling
log "Pulling runner image: $RUNNER_IMAGE"
if ! docker pull "$RUNNER_IMAGE"; then
    fail "Failed to pull runner image: $RUNNER_IMAGE"
fi

# Build environment variables for container
ENV_ARGS=(
    -e "RUNS_FLEET_INSTANCE_ID=$INSTANCE_ID"
    -e "RUNS_FLEET_RUN_ID=$RUN_ID"
    -e "AWS_REGION=$REGION"
    -e "AWS_DEFAULT_REGION=$REGION"
)

[[ -n "${TERMINATION_QUEUE_URL:-}" ]] && ENV_ARGS+=(-e "RUNS_FLEET_TERMINATION_QUEUE_URL=$TERMINATION_QUEUE_URL")
[[ -n "${CACHE_URL:-}" ]] && ENV_ARGS+=(-e "RUNS_FLEET_CACHE_URL=$CACHE_URL")

# Secrets backend configuration
[[ -n "${SECRETS_BACKEND:-}" ]] && ENV_ARGS+=(-e "RUNS_FLEET_SECRETS_BACKEND=$SECRETS_BACKEND")
[[ -n "${VAULT_ADDR:-}" ]] && ENV_ARGS+=(-e "VAULT_ADDR=$VAULT_ADDR")
[[ -n "${VAULT_KV_MOUNT:-}" ]] && ENV_ARGS+=(-e "VAULT_KV_MOUNT=$VAULT_KV_MOUNT")
[[ -n "${VAULT_KV_VERSION:-}" ]] && ENV_ARGS+=(-e "VAULT_KV_VERSION=$VAULT_KV_VERSION")
[[ -n "${VAULT_BASE_PATH:-}" ]] && ENV_ARGS+=(-e "VAULT_BASE_PATH=$VAULT_BASE_PATH")
[[ -n "${VAULT_AUTH_METHOD:-}" ]] && ENV_ARGS+=(-e "VAULT_AUTH_METHOD=$VAULT_AUTH_METHOD")
[[ -n "${VAULT_AWS_ROLE:-}" ]] && ENV_ARGS+=(-e "VAULT_AWS_ROLE=$VAULT_AWS_ROLE")

# Run container
# Note: --privileged and docker.sock mount are required for GitHub Actions workflows
# that build/run containers. See security notes at top of script.
log "Starting runner container..."
docker run --rm \
    --name runs-fleet-runner \
    --network host \
    --privileged \
    "${ENV_ARGS[@]}" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    "$RUNNER_IMAGE"

EXIT_CODE=$?
log "Runner container exited with code: $EXIT_CODE"

# Container handles termination via agent, but ensure cleanup
log "=== Bootstrap Complete ==="
