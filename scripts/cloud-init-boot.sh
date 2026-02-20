#!/bin/bash
set -e

TOKEN=$(curl -sX PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 300")
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)
REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region)

echo "[$(date)] runs-fleet boot script starting for ${INSTANCE_ID}"

if /opt/runs-fleet/agent-bootstrap.sh; then
  echo "[$(date)] Bootstrap completed"
  exit 0
fi

echo "[$(date)] Bootstrap failed, notifying and self-terminating"

TAG_ERR="/tmp/tag-err-$$"
TERMINATION_QUEUE_URL=$(aws ec2 describe-tags \
  --region "${REGION}" \
  --filters "Name=resource-id,Values=${INSTANCE_ID}" "Name=key,Values=runs-fleet:termination-queue-url" \
  --query 'Tags[0].Value' \
  --output text 2>"${TAG_ERR}" | grep -v "^None$" || true)
[ -s "${TAG_ERR}" ] && echo "[$(date)] WARN: Failed to fetch termination queue tag: $(cat "${TAG_ERR}")"
rm -f "${TAG_ERR}"

if [ -n "$TERMINATION_QUEUE_URL" ]; then
  MESSAGE=$(jq -n \
    --arg id "$INSTANCE_ID" \
    --arg status "bootstrap_failed" \
    --arg err "agent bootstrap failed on boot" \
    '{instance_id: $id, status: $status, error: $err}')
  aws sqs send-message \
    --queue-url "$TERMINATION_QUEUE_URL" \
    --message-body "$MESSAGE" \
    --message-group-id "$INSTANCE_ID" \
    --region "$REGION" 2>/dev/null || true
fi

aws ec2 terminate-instances \
  --instance-ids "$INSTANCE_ID" \
  --region "$REGION" 2>/dev/null || echo "CRITICAL: Failed to self-terminate instance ${INSTANCE_ID}"
