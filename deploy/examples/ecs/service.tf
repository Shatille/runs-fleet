# ECS service for the orchestrator task.
#
# The orchestrator's webhook port (8080) needs to be reachable from
# GitHub. The simplest setup puts it behind an ALB on public subnets
# with a TLS cert; below assumes that ALB + listener + target group
# already exist (typical in larger Terraform repos).

resource "aws_ecs_service" "orchestrator" {
  name            = "runs-fleet-orchestrator"
  cluster         = "arn:aws:ecs:ap-northeast-1:123456789012:cluster/CLUSTER_NAME"
  task_definition = aws_ecs_task_definition.orchestrator.arn
  desired_count   = 2 # safe to scale horizontally; per-pool reconciliation is locked in DynamoDB
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = ["subnet-AAA", "subnet-BBB"]
    security_groups  = ["sg-orchestrator"]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = "arn:aws:elasticloadbalancing:ap-northeast-1:123456789012:targetgroup/runs-fleet-orchestrator/HASH"
    container_name   = "orchestrator"
    container_port   = 8080
  }

  # Rolling deploy. On SIGTERM the orchestrator fails readiness and holds the
  # listener open for RUNS_FLEET_SHUTDOWN_DRAIN_DELAY_SECONDS so the ALB
  # deregisters this task before it stops accepting webhooks, then drains
  # in-flight reconciles before exit. Set the target group's
  # deregistration_delay >= that drain delay, and keep
  # deployment_minimum_healthy_percent = 100 so old tasks keep serving until
  # replacements are healthy (a capacity-constrained placement must not leave
  # zero healthy tasks behind the ALB — that returns 5xx to GitHub webhooks and
  # strands jobs with no runner).
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }
}
