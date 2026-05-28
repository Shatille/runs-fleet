# Reference DynamoDB tables for the runs-fleet orchestrator.
#
# Same caveats as iam.tf: illustrative sample, not a production module.
# Schema and index names are part of the orchestrator's contract with its
# infra — changing a name here or in pkg/db/* requires changing both
# together.
#
# All tables are PAY_PER_REQUEST. At ~100 jobs/day this is well under the
# crossover point for PROVISIONED, and removes the need to keep capacity
# tuned. Switch only if sustained throughput justifies the math.

# runs-fleet-jobs
# ---
# Primary key: job_id (GitHub Actions workflow job ID).
#
# GSIs:
#   - pool-created-at-index: REQUIRED. QueryPoolJobHistory in
#     pkg/db/jobs.go queries it for autoscaling decisions. Without it,
#     that query falls back to a Scan and the orchestrator logs a WARN
#     every reconcile loop.
#
#   - instance-id-index: optional. Lets GetJobByInstance avoid a Scan.
#     Wire it up by setting
#     `RUNS_FLEET_JOBS_INSTANCE_ID_GSI=instance-id-index` on the
#     orchestrator. Unset -> Scan fallback (correct, slower).
#
#   - pool-status-index: optional. Lets GetPoolBusyInstanceIDs avoid a
#     Scan. Wire it up by setting
#     `RUNS_FLEET_JOBS_POOL_STATUS_GSI=pool-status-index` on the
#     orchestrator. Unset -> Scan fallback (correct, slower).
resource "aws_dynamodb_table" "jobs" {
  name         = "runs-fleet-jobs"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "job_id"

  attribute {
    name = "job_id"
    type = "N"
  }

  attribute {
    name = "pool"
    type = "S"
  }

  attribute {
    name = "created_at"
    type = "S"
  }

  attribute {
    name = "instance_id"
    type = "S"
  }

  attribute {
    name = "status"
    type = "S"
  }

  global_secondary_index {
    name            = "pool-created-at-index"
    hash_key        = "pool"
    range_key       = "created_at"
    projection_type = "ALL"
  }

  global_secondary_index {
    name            = "instance-id-index"
    hash_key        = "instance_id"
    projection_type = "ALL"
  }

  global_secondary_index {
    name            = "pool-status-index"
    hash_key        = "pool"
    range_key       = "status"
    projection_type = "ALL"
  }

  point_in_time_recovery {
    enabled = true
  }
}

# runs-fleet-pools
# ---
# Primary key: pool_name. One physical table, three logical concerns:
#   - pool configuration rows (one per pool name)
#   - reconciliation locks (rows keyed by lock-specific names)
#   - instance claim rows (rows keyed by claim-specific names)
#
# All three share the pool_name partition; no GSIs needed.
resource "aws_dynamodb_table" "pools" {
  name         = "runs-fleet-pools"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pool_name"

  attribute {
    name = "pool_name"
    type = "S"
  }

  point_in_time_recovery {
    enabled = true
  }
}

# runs-fleet-circuit-state
# ---
# Primary key: instance_type. One row per instance type the circuit
# breaker has tracked. State transitions and counts live in the item; no
# GSIs needed.
resource "aws_dynamodb_table" "circuit_state" {
  name         = "runs-fleet-circuit-state"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "instance_type"

  attribute {
    name = "instance_type"
    type = "S"
  }

  point_in_time_recovery {
    enabled = true
  }
}
