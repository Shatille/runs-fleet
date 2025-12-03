#!/bin/bash
set -e

# runs-fleet runner entrypoint
# This script is executed when the runner pod starts

# Export instance ID from hostname if not set (K8s pod name)
export RUNS_FLEET_INSTANCE_ID="${RUNS_FLEET_INSTANCE_ID:-$(hostname)}"

echo "Starting runs-fleet runner..."
echo "Instance ID: ${RUNS_FLEET_INSTANCE_ID}"

# Run the runs-fleet agent
# The agent handles:
# - Config loading from ConfigMap/Secret (K8s) or SSM (EC2)
# - Runner registration with GitHub
# - Job execution
# - Telemetry and cleanup
exec /usr/local/bin/runs-fleet-agent "$@"
