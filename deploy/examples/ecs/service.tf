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

  # Rolling deploy. The orchestrator handles graceful shutdown (SIGTERM
  # drains in-flight reconciles before exit), so the standard rolling
  # update strategy is safe.
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }
}
