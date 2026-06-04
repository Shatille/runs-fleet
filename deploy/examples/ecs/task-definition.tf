# Reference Fargate task definition for the runs-fleet orchestrator.
#
# Same posture as the rest of deploy/: illustrative, not state-managed.
# Replace the ARN placeholders and the ECR registry before applying.
#
# Sized to match what runs in production upstream: 1 vCPU / 2 GB. The
# orchestrator's footprint is dominated by AWS SDK clients and a small
# reconcile loop — scale up only if you see CPU saturation under load.

terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}

resource "aws_ecs_task_definition" "orchestrator" {
  family                   = "runs-fleet-orchestrator"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = "1024"
  memory                   = "2048"

  # The role attached to the task itself (orchestrator AWS API calls).
  # Comes from ../../terraform/iam.tf -> aws_iam_role.orchestrator_task.
  task_role_arn = "arn:aws:iam::123456789012:role/runs-fleet-orchestrator"

  # The role ECS uses to pull the image and write logs (separate from
  # the task role). Usually a thin role with AmazonECSTaskExecutionRolePolicy
  # plus permission to read any secrets you reference below.
  execution_role_arn = "arn:aws:iam::123456789012:role/ecsTaskExecutionRole"

  # The orchestrator image is multi-arch; arm64 is cheaper to run on
  # Fargate Spot. Switch to X86_64 if you build amd64-only.
  runtime_platform {
    cpu_architecture        = "ARM64"
    operating_system_family = "LINUX"
  }

  container_definitions = jsonencode([{
    name      = "orchestrator"
    image     = "123456789012.dkr.ecr.ap-northeast-1.amazonaws.com/runs-fleet/orchestrator:vX.Y.Z"
    essential = true

    portMappings = [{
      containerPort = 8080
      protocol      = "tcp"
    }]

    # Minimum required env vars. The full surface lives in
    # pkg/config/config.go; common optional adds are noted inline.
    environment = [
      { name = "AWS_ap-northeast-1", value = "ap-northeast-1" },

      # GitHub App identity. Private key + webhook secret come from
      # Secrets Manager (see `secrets` block below).
      { name = "RUNS_FLEET_GITHUB_APP_ID", value = "<GITHUB_APP_ID>" },

      # SQS queues — same name conventions as deploy/terraform/queues.tf
      # would use once that lands.
      { name = "RUNS_FLEET_QUEUE_URL", value = "https://sqs.ap-northeast-1.amazonaws.com/123456789012/runs-fleet-main" },
      { name = "RUNS_FLEET_POOL_QUEUE_URL", value = "https://sqs.ap-northeast-1.amazonaws.com/123456789012/runs-fleet-pool" },
      { name = "RUNS_FLEET_EVENTS_QUEUE_URL", value = "https://sqs.ap-northeast-1.amazonaws.com/123456789012/runs-fleet-events" },
      { name = "RUNS_FLEET_TERMINATION_QUEUE_URL", value = "https://sqs.ap-northeast-1.amazonaws.com/123456789012/runs-fleet-termination" },

      # DynamoDB — names come from deploy/terraform/dynamodb.tf.
      { name = "RUNS_FLEET_JOBS_TABLE", value = "runs-fleet-jobs" },
      { name = "RUNS_FLEET_POOLS_TABLE", value = "runs-fleet-pools" },
      # Optional GSIs. Unset = Scan fallback (correct, slower). See
      # comments in deploy/terraform/dynamodb.tf for what each unlocks.
      { name = "RUNS_FLEET_JOBS_INSTANCE_ID_GSI", value = "instance-id-index" },
      { name = "RUNS_FLEET_JOBS_POOL_STATUS_GSI", value = "pool-status-index" },

      # EC2 runner fleet config.
      { name = "RUNS_FLEET_VPC_ID", value = "vpc-XXXXXXXX" },
      { name = "RUNS_FLEET_SUBNET_IDS", value = "subnet-AAA,subnet-BBB" },
      { name = "RUNS_FLEET_SECURITY_GROUP_ID", value = "sg-XXXXXXXX" },
      { name = "RUNS_FLEET_INSTANCE_PROFILE_ARN", value = "arn:aws:iam::123456789012:instance-profile/runs-fleet-runner" },
      { name = "RUNS_FLEET_RUNNER_IMAGE", value = "123456789012.dkr.ecr.ap-northeast-1.amazonaws.com/runs-fleet/runner:latest" },

      # Orchestrator's own externally-reachable HTTPS endpoint, served to runners
      # as ACTIONS_CACHE_URL for the S3 Actions cache. Required.
      { name = "RUNS_FLEET_BASE_URL", value = "https://runs-fleet.example.com" },

      # S3 GitHub Actions cache bucket (served via RUNS_FLEET_BASE_URL; pair with
      # RUNS_FLEET_CACHE_SECRET below). Omit the bucket to disable the cache.
      { name = "RUNS_FLEET_CACHE_BUCKET", value = "runs-fleet-cache" },
    ]

    # Sensitive values come from Secrets Manager so they never appear in
    # the task definition itself.
    secrets = [
      { name = "RUNS_FLEET_GITHUB_APP_PRIVATE_KEY", valueFrom = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:runs-fleet/github-app-private-key" },
      { name = "RUNS_FLEET_GITHUB_WEBHOOK_SECRET", valueFrom = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:runs-fleet/webhook-secret" },
      { name = "RUNS_FLEET_CACHE_SECRET", valueFrom = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:runs-fleet/cache-secret" },
      { name = "RUNS_FLEET_ADMIN_SECRET", valueFrom = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:runs-fleet/admin-secret" },
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = "/aws/ecs/runs-fleet-orchestrator"
        awslogs-region        = "ap-northeast-1"
        awslogs-stream-prefix = "ecs"
      }
    }
  }])
}
