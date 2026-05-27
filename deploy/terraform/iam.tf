# Minimal IAM for a runs-fleet fork.
#
# Three roles are required:
#
#   1. Runner instance role + instance profile (attached to every EC2 runner).
#   2. Orchestrator task role (attached to the Fargate task or K8s
#      ServiceAccount that runs cmd/server/).
#   3. CI / Packer role (assumed via GitHub OIDC by the workflows under
#      .github/workflows/).
#
# This file is illustrative — minimum permissions per role, not a
# production-ready terraform module. Resource ARNs reference placeholder
# variables you replace before applying. Bucket / table / queue resources
# themselves are out of scope and left to your existing infra provisioning.

terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}

variable "account_id" {
  description = "AWS account ID the fork is deployed into."
  type        = string
}

variable "region" {
  description = "AWS region."
  type        = string
  default     = "ap-northeast-1"
}

variable "github_org" {
  description = "GitHub organization or user that owns the fork repo."
  type        = string
}

variable "github_repo" {
  description = "Fork repository name (without owner)."
  type        = string
}

variable "ecr_runner_repository" {
  description = "ECR repository path for the runner container image."
  type        = string
  default     = "runs-fleet/runner"
}

variable "ecr_orchestrator_repository" {
  description = "ECR repository path for the orchestrator container image."
  type        = string
  default     = "runs-fleet/orchestrator"
}

variable "cache_bucket_name" {
  description = "S3 bucket name for the GitHub Actions cache."
  type        = string
}

variable "config_bucket_name" {
  description = "S3 bucket name for runner configs and agent binaries."
  type        = string
}

variable "managed_resource_tag" {
  description = "Tag the orchestrator stamps on EC2 instances; scopes lifecycle permissions to managed resources."
  type = object({
    key   = string
    value = string
  })
  default = {
    key   = "runs-fleet:managed"
    value = "true"
  }
}

variable "packer_created_by_tag_value" {
  description = "`created-by` tag Packer stamps on its temporary builder resources; scopes the CI role's destroy permissions."
  type        = string
  default     = "runs-fleet-packer"
}

locals {
  ecr_runner_arn       = "arn:aws:ecr:${var.region}:${var.account_id}:repository/${var.ecr_runner_repository}"
  ecr_orchestrator_arn = "arn:aws:ecr:${var.region}:${var.account_id}:repository/${var.ecr_orchestrator_repository}"
  sqs_arn_prefix       = "arn:aws:sqs:${var.region}:${var.account_id}:runs-fleet-"
  ddb_arn_prefix       = "arn:aws:dynamodb:${var.region}:${var.account_id}:table/runs-fleet-"
  cache_bucket_arn     = "arn:aws:s3:::${var.cache_bucket_name}"
  config_bucket_arn    = "arn:aws:s3:::${var.config_bucket_name}"
  ssm_param_arn        = "arn:aws:ssm:${var.region}:${var.account_id}:parameter/runs-fleet/*"
  instance_arn         = "arn:aws:ec2:${var.region}:${var.account_id}:instance/*"
}

# ─── 1. Runner instance role ─────────────────────────────────────────────────
# Attached to every EC2 runner launched from the runs-fleet launch templates.
# Needs to: fetch its own config from SSM, pull its container image, R/W the
# cache bucket and read the config bucket, send termination notifications,
# self-terminate when the job completes.

resource "aws_iam_role" "runner" {
  name = "runs-fleet-runner"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "runner" {
  name = "runs-fleet-runner"
  role = aws_iam_role.runner.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["ecr:GetAuthorizationToken"]
        Resource = "*"
      },
      {
        Effect   = "Allow"
        Action   = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"]
        Resource = local.ecr_runner_arn
      },
      {
        # Agent fetches per-instance config blob from SSM at boot.
        Effect   = "Allow"
        Action   = ["ssm:GetParameter", "ssm:GetParameters"]
        Resource = local.ssm_param_arn
      },
      {
        # Read agent binary + bootstrap data from the config bucket.
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = "${local.config_bucket_arn}/*"
      },
      {
        # GitHub Actions cache: pre-signed URLs are issued by the orchestrator,
        # but the runner needs direct R/W to the bucket as fallback.
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject"]
        Resource = "${local.cache_bucket_arn}/*"
      },
      {
        # Termination notification only — runner does not consume queues.
        Effect   = "Allow"
        Action   = ["sqs:SendMessage"]
        Resource = "${local.sqs_arn_prefix}termination"
      },
      {
        Effect   = "Allow"
        Action   = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "*"
      },
      {
        Effect   = "Allow"
        Action   = ["ec2:DescribeInstances", "ec2:DescribeTags"]
        Resource = "*"
      },
      {
        # Self-terminate, scoped to instances bearing the managed tag.
        Effect   = "Allow"
        Action   = ["ec2:TerminateInstances"]
        Resource = local.instance_arn
        Condition = {
          StringEquals = {
            "aws:ResourceTag/${var.managed_resource_tag.key}" = var.managed_resource_tag.value
          }
        }
      },
    ]
  })
}

resource "aws_iam_role_policy_attachment" "runner_ssm" {
  # SSM Session Manager (Packer builder + ad-hoc debugging).
  role       = aws_iam_role.runner.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "runner" {
  name = "runs-fleet-runner"
  role = aws_iam_role.runner.name
}

# ─── 2. Orchestrator task role ───────────────────────────────────────────────
# Attached to the Fargate task or Kubernetes ServiceAccount (via IRSA) that
# runs cmd/server/. The trust policy below is the Fargate variant; for EKS
# IRSA, replace `Principal.Service` with the OIDC `Federated` principal of
# your cluster's OIDC provider plus a `Condition` on
# `<oidc>:sub`/`<oidc>:aud`.
#
# If you host on Fargate you'll also need a separate `runs-fleet-fargate-
# execution` role with `AmazonECSTaskExecutionRolePolicy` attached so ECS
# can pull the orchestrator image and write its log streams; that role is
# omitted from this example because the policy is canned.

resource "aws_iam_role" "orchestrator" {
  name = "runs-fleet-orchestrator"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "orchestrator" {
  name = "runs-fleet-orchestrator"
  role = aws_iam_role.orchestrator.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        # Read-only EC2 inspection used by fleet manager, spot pricing,
        # subnet/route discovery, etc.
        Effect = "Allow"
        Action = [
          "ec2:DescribeImages",
          "ec2:DescribeInstances",
          "ec2:DescribeInstanceTypes",
          "ec2:DescribeInstanceTypeOfferings",
          "ec2:DescribeSubnets",
          "ec2:DescribeRouteTables",
          "ec2:DescribeVolumes",
          "ec2:DescribeSnapshots",
          "ec2:DescribeSpotPriceHistory",
          "ec2:DescribeSpotInstanceRequests",
          "ec2:DescribeLaunchTemplates",
          "ec2:DescribeLaunchTemplateVersions",
        ]
        Resource = "*"
      },
      {
        # Fleet creation + lifecycle.
        Effect = "Allow"
        Action = [
          "ec2:CreateFleet",
          "ec2:DeleteFleets",
          "ec2:DescribeFleets",
          "ec2:DescribeFleetInstances",
          "ec2:RunInstances",
          "ec2:CreateTags",
        ]
        Resource = "*"
      },
      {
        # Lifecycle limited to instances bearing the managed tag.
        Effect   = "Allow"
        Action   = ["ec2:StartInstances", "ec2:StopInstances", "ec2:TerminateInstances"]
        Resource = local.instance_arn
        Condition = {
          StringEquals = {
            "aws:ResourceTag/${var.managed_resource_tag.key}" = var.managed_resource_tag.value
          }
        }
      },
      {
        Effect   = "Allow"
        Action   = ["ec2:CancelSpotInstanceRequests"]
        Resource = "arn:aws:ec2:${var.region}:${var.account_id}:spot-instances-request/*"
      },
      {
        # Pass the runner instance profile when calling RunInstances/CreateFleet.
        Effect   = "Allow"
        Action   = ["iam:PassRole"]
        Resource = aws_iam_role.runner.arn
        Condition = {
          StringEquals = { "iam:PassedToService" = "ec2.amazonaws.com" }
        }
      },
      {
        # First-time spot fleet creation needs to provision the spot service
        # linked role if it doesn't already exist in the account.
        Effect   = "Allow"
        Action   = ["iam:CreateServiceLinkedRole"]
        Resource = "arn:aws:iam::*:role/aws-service-role/spot.amazonaws.com/AWSServiceRoleForEC2Spot"
        Condition = {
          StringEquals = { "iam:AWSServiceName" = "spot.amazonaws.com" }
        }
      },
      {
        Effect = "Allow"
        Action = [
          "sqs:ReceiveMessage",
          "sqs:DeleteMessage",
          "sqs:SendMessage",
          "sqs:GetQueueAttributes",
          "sqs:StartMessageMoveTask",
        ]
        Resource = "${local.sqs_arn_prefix}*"
      },
      {
        Effect = "Allow"
        Action = [
          "dynamodb:GetItem",
          "dynamodb:PutItem",
          "dynamodb:UpdateItem",
          "dynamodb:DeleteItem",
          "dynamodb:Query",
          "dynamodb:Scan",
          "dynamodb:BatchWriteItem",
          "dynamodb:TransactWriteItems",
        ]
        Resource = ["${local.ddb_arn_prefix}*", "${local.ddb_arn_prefix}*/index/*"]
      },
      {
        # SSM parameters for per-runner config blobs.
        Effect = "Allow"
        Action = [
          "ssm:GetParameter",
          "ssm:GetParameters",
          "ssm:GetParametersByPath",
          "ssm:PutParameter",
          "ssm:DeleteParameter",
          "ssm:AddTagsToResource",
        ]
        Resource = local.ssm_param_arn
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket"]
        Resource = [local.cache_bucket_arn, "${local.cache_bucket_arn}/*", local.config_bucket_arn, "${local.config_bucket_arn}/*"]
      },
      {
        Effect   = "Allow"
        Action   = ["cloudwatch:PutMetricData", "cloudwatch:GetMetricData"]
        Resource = "*"
      },
      {
        Effect   = "Allow"
        Action   = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "*"
      },
    ]
  })
}

# ─── 3. CI / Packer role ─────────────────────────────────────────────────────
# Assumed by the GitHub OIDC provider when the workflows under
# .github/workflows/ run. Builds container images, builds AMIs (via Packer),
# writes new launch template versions, deregisters old AMIs after promotion.
#
# Trust restricted to the specific fork repo + main branch by default.
# Loosen the `sub` pattern only if you need other branches to assume the role.

data "aws_iam_openid_connect_provider" "github" {
  url = "https://token.actions.githubusercontent.com"
}

resource "aws_iam_role" "ci" {
  name = "runs-fleet-ci"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = data.aws_iam_openid_connect_provider.github.arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "token.actions.githubusercontent.com:aud" = "sts.amazonaws.com"
        }
        StringLike = {
          "token.actions.githubusercontent.com:sub" = "repo:${var.github_org}/${var.github_repo}:ref:refs/heads/main"
        }
      }
    }]
  })
}

resource "aws_iam_role_policy" "ci" {
  name = "runs-fleet-ci"
  role = aws_iam_role.ci.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        # ECR push for both images.
        Effect = "Allow"
        Action = [
          "ecr:GetAuthorizationToken",
          "ecr:BatchGetImage",
          "ecr:GetDownloadUrlForLayer",
          "ecr:InitiateLayerUpload",
          "ecr:UploadLayerPart",
          "ecr:CompleteLayerUpload",
          "ecr:PutImage",
          "ecr:BatchDeleteImage",
          "ecr:DescribeImages",
        ]
        Resource = [local.ecr_runner_arn, local.ecr_orchestrator_arn]
      },
      {
        # Read-only EC2 inspection for Packer subnet/SG discovery and AMI
        # source lookups.
        Effect = "Allow"
        Action = [
          "ec2:DescribeImages",
          "ec2:DescribeInstances",
          "ec2:DescribeInstanceStatus",
          "ec2:DescribeInstanceTypes",
          "ec2:DescribeInstanceTypeOfferings",
          "ec2:DescribeKeyPairs",
          "ec2:DescribeLaunchTemplates",
          "ec2:DescribeLaunchTemplateVersions",
          "ec2:DescribeRegions",
          "ec2:DescribeSecurityGroups",
          "ec2:DescribeSnapshots",
          "ec2:DescribeSubnets",
          "ec2:DescribeTags",
          "ec2:DescribeVolumes",
        ]
        Resource = "*"
      },
      {
        # Launch template updates after a successful AMI build.
        Effect = "Allow"
        Action = [
          "ec2:CreateLaunchTemplateVersion",
          "ec2:ModifyLaunchTemplate",
        ]
        Resource = "arn:aws:ec2:${var.region}:${var.account_id}:launch-template/*"
        Condition = {
          StringLike = {
            "aws:ResourceTag/Name" = "runs-fleet-runner-*"
          }
        }
      },
      {
        # Packer SSM tunnel + ami-amazon-linux-latest source AMI lookup.
        Effect = "Allow"
        Action = [
          "ssm:DescribeSessions",
          "ssm:GetConnectionStatus",
          "ssm:ResumeSession",
          "ssm:StartSession",
          "ssm:TerminateSession",
        ]
        Resource = "*"
      },
      {
        Effect   = "Allow"
        Action   = ["ssm:GetParameter"]
        Resource = "arn:aws:ssm:${var.region}::parameter/aws/service/ami-amazon-linux-latest/*"
      },
      {
        # Packer builder lifecycle, scoped to packer-tagged resources.
        Effect = "Allow"
        Action = [
          "ec2:DeleteSnapshot",
          "ec2:DeleteVolume",
          "ec2:GetPasswordData",
          "ec2:StopInstances",
          "ec2:TerminateInstances",
        ]
        Resource = [
          "arn:aws:ec2:${var.region}:${var.account_id}:instance/*",
          "arn:aws:ec2:${var.region}:${var.account_id}:snapshot/*",
          "arn:aws:ec2:${var.region}:${var.account_id}:volume/*",
        ]
        Condition = {
          StringEquals = {
            "aws:ResourceTag/created-by" = var.packer_created_by_tag_value
          }
        }
      },
      {
        # AMI snapshot + register.
        Effect = "Allow"
        Action = [
          "ec2:CopyImage",
          "ec2:CreateImage",
          "ec2:CreateSnapshot",
          "ec2:CreateVolume",
          "ec2:DeregisterImage",
          "ec2:ModifyImageAttribute",
          "ec2:RegisterImage",
        ]
        Resource = [
          "arn:aws:ec2:${var.region}:${var.account_id}:image/*",
          "arn:aws:ec2:${var.region}::image/*",
          "arn:aws:ec2:${var.region}:${var.account_id}:instance/*",
          "arn:aws:ec2:${var.region}:${var.account_id}:snapshot/*",
          "arn:aws:ec2:${var.region}::snapshot/*",
          "arn:aws:ec2:${var.region}:${var.account_id}:volume/*",
        ]
      },
      {
        # Packer creates and tears down ephemeral keypairs prefixed `packer_`.
        Effect = "Allow"
        Action = [
          "ec2:CreateKeyPair",
          "ec2:DeleteKeyPair",
        ]
        Resource = "arn:aws:ec2:${var.region}:${var.account_id}:key-pair/packer_*"
      },
      {
        Effect   = "Allow"
        Action   = ["ec2:RunInstances", "ec2:CreateTags"]
        Resource = "*"
      },
      {
        # Pass the runner instance profile to the Packer builder.
        Effect   = "Allow"
        Action   = ["iam:GetInstanceProfile"]
        Resource = "arn:aws:iam::${var.account_id}:instance-profile/runs-fleet-*"
      },
      {
        Effect   = "Allow"
        Action   = ["iam:PassRole"]
        Resource = "arn:aws:iam::${var.account_id}:role/runs-fleet-*"
        Condition = {
          StringEquals = { "iam:PassedToService" = "ec2.amazonaws.com" }
        }
      },
    ]
  })
}

output "runner_instance_profile_arn" {
  description = "Pass to aws.instanceProfileArn in the Helm chart values."
  value       = aws_iam_instance_profile.runner.arn
}

output "orchestrator_role_arn" {
  description = "Attach to the Fargate task definition or annotate the K8s ServiceAccount with this ARN."
  value       = aws_iam_role.orchestrator.arn
}

output "ci_role_arn" {
  description = "Set as the AWS_IAM_ROLE_ARN GitHub Actions secret on the fork."
  value       = aws_iam_role.ci.arn
}
