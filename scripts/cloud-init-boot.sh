#!/bin/bash
set -e
# shellcheck source-path=SCRIPTDIR source=boot-lib.sh
source /opt/runs-fleet/boot-lib.sh

TOKEN=$(imds_token) || { echo "ERROR: Failed to fetch IMDSv2 token"; exit 1; }
INSTANCE_ID=$(imds_get meta-data/instance-id "$TOKEN") || { echo "ERROR: Failed to fetch instance-id"; exit 1; }
REGION=$(imds_get meta-data/placement/region "$TOKEN") || { echo "ERROR: Failed to fetch region"; exit 1; }

echo "[$(date)] runs-fleet boot script starting for ${INSTANCE_ID}"

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
