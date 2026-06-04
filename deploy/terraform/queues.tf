# Reference SQS queues for the runs-fleet orchestrator.
#
# Same caveats as iam.tf / dynamodb.tf: illustrative sample, not a production
# module. Queue names are part of the orchestrator's contract with its infra —
# they must match the RUNS_FLEET_*_QUEUE_URL values set on the task (see
# deploy/examples/ecs/task-definition.tf).
#
# Every standard work queue is paired with a dead-letter queue so a message
# that keeps failing stops recycling on the visibility timeout and is moved
# aside after maxReceiveCount redeliveries. A FIFO queue can only dead-letter
# to a FIFO DLQ and a standard queue only to a standard DLQ, so the types are
# matched per pair.

locals {
  # Redeliveries before a message is moved to its dead-letter queue.
  dlq_max_receive_count = 3
  # Dead-letter retention — long, so failed messages remain inspectable.
  dlq_retention_seconds = 1209600 # 14 days
  # Work-queue retention — short; the orchestrator reschedules its own tasks.
  work_retention_seconds = 3600 # 1 hour
}

# ─── main (FIFO job requests) ────────────────────────────────────────────────
resource "aws_sqs_queue" "main_dlq" {
  name                        = "runs-fleet-main-dlq.fifo"
  fifo_queue                  = true
  content_based_deduplication = true
  message_retention_seconds   = local.dlq_retention_seconds
}

resource "aws_sqs_queue" "main" {
  name                        = "runs-fleet-main.fifo"
  fifo_queue                  = true
  content_based_deduplication = true
  visibility_timeout_seconds  = 300
  message_retention_seconds   = local.work_retention_seconds

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.main_dlq.arn
    maxReceiveCount     = local.dlq_max_receive_count
  })
}

# ─── pool (standard, batched warm-pool jobs) ─────────────────────────────────
resource "aws_sqs_queue" "pool_dlq" {
  name                      = "runs-fleet-pool-dlq"
  message_retention_seconds = local.dlq_retention_seconds
}

resource "aws_sqs_queue" "pool" {
  name                       = "runs-fleet-pool"
  visibility_timeout_seconds = 300
  message_retention_seconds  = local.work_retention_seconds

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.pool_dlq.arn
    maxReceiveCount     = local.dlq_max_receive_count
  })
}

# ─── termination (standard, instance shutdown notifications) ─────────────────
resource "aws_sqs_queue" "termination" {
  name                       = "runs-fleet-termination"
  visibility_timeout_seconds = 60
  message_retention_seconds  = local.work_retention_seconds
}

# ─── events (standard, EventBridge spot interruptions + cost reports) ────────
# An EventBridge rule delivers here; grant that rule sqs:SendMessage with a
# separate queue policy (omitted from this example).
resource "aws_sqs_queue" "events" {
  name                       = "runs-fleet-events"
  visibility_timeout_seconds = 300
  message_retention_seconds  = local.work_retention_seconds
}
