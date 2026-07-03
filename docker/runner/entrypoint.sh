#!/bin/bash
set -e

# runs-fleet runner entrypoint
# Container entrypoint that execs the runs-fleet agent (EC2 host / Fargate).

# Fall back to the hostname when the instance ID isn't set in the environment.
export RUNS_FLEET_INSTANCE_ID="${RUNS_FLEET_INSTANCE_ID:-$(hostname)}"

echo "Starting runs-fleet runner..."
echo "Instance ID: ${RUNS_FLEET_INSTANCE_ID}"

# Run the runs-fleet agent
# The agent handles:
# - Config loading from the configured secrets backend (SSM or Vault)
# - Runner registration with GitHub
# - Job execution
# - Telemetry and cleanup
exec /usr/local/bin/runs-fleet-agent "$@"
