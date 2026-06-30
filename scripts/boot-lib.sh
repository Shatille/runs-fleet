#!/bin/bash
# Shared boot-time helpers, sourced by cloud-init-boot.sh and agent-bootstrap.sh.
# Centralizes the IMDS and EC2-tag fetches so the two boot scripts can't drift,
# and wraps every early-boot network call in retries/timeouts: a single transient
# hiccup (IMDS not answering yet, DescribeTags throttled during a launch burst)
# must not fail the whole bootstrap and burn the instance.
#
# Sourced, not executed: no `set -e` here (callers own that), and nothing runs at
# source time but the exports and definitions below. get_tag reads the REGION and
# INSTANCE_ID the caller resolves from IMDS first.

# DescribeTags throttling (RequestLimitExceeded) under a launch burst is the
# dominant boot-time failure; adaptive retry adds client-side rate limiting and
# spreads attempts over a wider window than the SDK default of 3.
export AWS_RETRY_MODE=adaptive
export AWS_MAX_ATTEMPTS=10

# retry <max-attempts> <delay-seconds> <cmd> [args...]
# Run cmd, retrying on non-zero exit up to max-attempts with a fixed delay
# between tries. Returns 0 on first success, else the final non-zero status.
retry() {
  local max="$1" delay="$2"
  shift 2
  local n=1
  while true; do
    "$@" && return 0
    [ "$n" -ge "$max" ] && return 1
    sleep "$delay"
    n=$((n + 1))
  done
}

# --retry-connrefused + timeouts ride out the early-boot window where the
# metadata service isn't answering yet; -f turns an HTTP error into a non-zero
# exit (so it's retried), -s keeps the boot log clean. --max-time bounds each
# attempt (it resets per retry), --retry-max-time bounds the whole retry loop.
_imds_curl() {
  curl -sf \
    --retry 5 --retry-delay 1 --retry-connrefused \
    --connect-timeout 1 --max-time 5 --retry-max-time 10 \
    "$@"
}

# imds_token: fetch a fresh IMDSv2 session token.
imds_token() {
  _imds_curl -X PUT "http://169.254.169.254/latest/api/token" \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 300"
}

# imds_get <path> <token>: fetch an IMDS path, e.g. meta-data/instance-id.
imds_get() {
  _imds_curl -H "X-aws-ec2-metadata-token: $2" \
    "http://169.254.169.254/latest/$1"
}

# get_tag <tag-key>: print this instance's value for the tag (empty if the tag
# is absent) and return 0 on a successful DescribeTags call; return non-zero if
# the API call itself keeps failing. Callers that require the tag MUST treat a
# non-zero return as fatal (fail closed) — proceeding with an empty value would
# silently mis-route the agent (e.g. onto the wrong secrets backend). Reads
# REGION and INSTANCE_ID from the caller's environment.
get_tag() {
  local key="$1" value
  if ! value=$(retry 3 2 aws ec2 describe-tags \
    --region "${REGION}" \
    --filters "Name=resource-id,Values=${INSTANCE_ID}" "Name=key,Values=${key}" \
    --query 'Tags[0].Value' \
    --output text); then
    return 1
  fi
  [ "$value" = "None" ] && value=""
  printf '%s' "$value"
}
