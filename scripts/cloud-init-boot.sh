#!/bin/bash
set -e
# shellcheck source-path=SCRIPTDIR source=boot-lib.sh
source /opt/runs-fleet/boot-lib.sh

imds_bootstrap || exit 1

echo "[$(date)] runs-fleet boot script starting for ${INSTANCE_ID}"

# When the AMI was baked against an ECR pull-through cache, the binfmt/buildx
# units reference it and the token baked in at build time has long expired.
# Re-authenticate before those units run. Best-effort: the images they need are
# already in the AMI's image store, so a failure here costs a re-pull at worst
# and must not strand the instance.
ECR_MIRROR_REGISTRY_FILE="/opt/runs-fleet/ecr-mirror-registry"
if [ -s "$ECR_MIRROR_REGISTRY_FILE" ]; then
  ECR_MIRROR_REGISTRY=$(cat "$ECR_MIRROR_REGISTRY_FILE")
  if aws ecr get-login-password --region "$REGION" 2>/dev/null \
    | docker login --username AWS --password-stdin "$ECR_MIRROR_REGISTRY" >/dev/null 2>&1; then
    echo "[$(date)] Authenticated to ${ECR_MIRROR_REGISTRY}"
  else
    echo "[$(date)] WARN: could not authenticate to ${ECR_MIRROR_REGISTRY}; pulls fall back to Docker Hub"
  fi
fi

# Downstream extension point. Upstream ships an empty stub; forks rewrite it
# from the BOOT_HOOK secret for per-boot state that cannot be baked into the AMI
# — typically refreshing registry credentials that expire between bake and boot.
# Runs before the agent so dockerd is configured before the first job pull.
#
# Deliberately best-effort: an unreachable mirror or an expired credential must
# degrade to pulling from the upstream registry, never strand the instance. The
# `||` branch is what keeps `set -e` from turning a hook failure into a boot
# failure — which would otherwise self-terminate the instance below.
BOOT_HOOK="/opt/runs-fleet/boot-hook.sh"
if [ -s "$BOOT_HOOK" ]; then
  echo "[$(date)] Running downstream boot hook"
  bash -euo pipefail "$BOOT_HOOK" || echo "[$(date)] WARN: boot hook failed; continuing without it"
fi

# Capture bootstrap output so a failure can report *why* (the orchestrator only
# sees this SQS message; terminated instances retain no console log). Still echo
# it to the console log for the success path and local debugging.
BOOT_LOG="/tmp/agent-bootstrap-$$.log"
# `|| true` on the reads: under `set -e` a failed cat/tail (e.g. the log file
# couldn't be created on a full disk) must not abort the script before the
# notification + self-termination below, which would leave a zombie instance.
if /opt/runs-fleet/agent-bootstrap.sh >"$BOOT_LOG" 2>&1; then
  cat "$BOOT_LOG" 2>/dev/null || true
  rm -f "$BOOT_LOG"
  echo "[$(date)] Bootstrap completed"
  exit 0
fi

# A stop that lands mid-bootstrap (e.g. a warm-pool spare being banked, or a spot
# interruption during boot) shuts the OS down before the agent finishes starting;
# the resulting non-zero exit is a benign casualty of the stop, not a bootstrap
# failure. Recognize it and get out of the way — no failure notification, no
# self-termination — so the instance completes its stop and becomes a banked warm
# spare instead of being terminated (which churns the pool). Checked here, at the
# decision point, so it also covers a shutdown that aborted an earlier boot step.
if system_is_stopping; then
  echo "[$(date)] System is shutting down mid-bootstrap; skipping failure notification and self-termination (instance will stop and become a warm spare)"
  rm -f "$BOOT_LOG"
  exit 0
fi

cat "$BOOT_LOG" 2>/dev/null || true
echo "[$(date)] Bootstrap failed, notifying and self-terminating"

# Use the tail of the bootstrap output as the failure reason. Bounded and
# stripped to printable chars so the SQS message stays small/well-formed; jq
# --arg JSON-escapes it. agent-bootstrap.sh prints backend selection + validation
# errors only (secrets are written to a file, never stdout), so this is safe.
REASON=$(tail -c 800 "$BOOT_LOG" 2>/dev/null | tr '\n' ' ' | tr -cd '[:print:]')
[ -n "$REASON" ] || REASON="agent bootstrap failed on boot"
rm -f "$BOOT_LOG"

# Best-effort notification: if the queue tag can't be read we just skip it.
TERMINATION_QUEUE_URL=$(get_tag "runs-fleet:termination-queue-url" || true)
[ -n "$TERMINATION_QUEUE_URL" ] || echo "[$(date)] WARN: no termination-queue-url tag; skipping notification"

if [ -n "$TERMINATION_QUEUE_URL" ]; then
  MESSAGE=$(jq -n \
    --arg id "$INSTANCE_ID" \
    --arg status "bootstrap_failed" \
    --arg err "$REASON" \
    '{instance_id: $id, status: $status, error: $err}')
  SQS_ERR="/tmp/sqs-err-$$"
  if ! retry 3 2 aws sqs send-message \
    --queue-url "$TERMINATION_QUEUE_URL" \
    --message-body "$MESSAGE" \
    --message-group-id "$INSTANCE_ID" \
    --region "$REGION" 2>"${SQS_ERR}"; then
    echo "[$(date)] WARN: Failed to send SQS notification: $(cat "${SQS_ERR}" 2>/dev/null)"
  fi
  rm -f "${SQS_ERR}"
fi

# Self-terminate with retries: a transient EC2 API failure here would otherwise
# leave a zombie instance billing until housekeeping reaps it.
TERM_ERR="/tmp/terminate-err-$$"
if ! retry 3 2 aws ec2 terminate-instances \
  --instance-ids "$INSTANCE_ID" \
  --region "$REGION" 2>"${TERM_ERR}"; then
  echo "CRITICAL: Failed to self-terminate instance ${INSTANCE_ID}: $(cat "${TERM_ERR}" 2>/dev/null)"
fi
rm -f "${TERM_ERR}"
