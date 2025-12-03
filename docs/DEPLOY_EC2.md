# EC2 Deployment Guide

Deploy runs-fleet on ECS Fargate with EC2 Fleet for runners.

## Prerequisites

- AWS account with appropriate permissions
- Terraform (for infrastructure)
- GitHub App configured

## 1. Create GitHub App

1. Go to GitHub Settings → Developer settings → GitHub Apps → New GitHub App
2. Configure:
   - **Webhook URL**: `https://<your-domain>/webhook`
   - **Webhook secret**: Generate with `openssl rand -hex 32`
   - **Permissions**:
     - Repository: Actions (read), Administration (read/write), Metadata (read)
     - Organization: Self-hosted runners (read/write)
   - **Events**: Workflow job
3. Generate and download private key
4. Install app on target repositories/organization
5. Store credentials in AWS Secrets Manager

## 2. Provision Infrastructure

Required AWS resources:

| Resource | Purpose |
|----------|---------|
| VPC + Subnets | Network isolation |
| Security Group | Runner traffic rules |
| SQS FIFO Queues | Job queuing (main, pool, events, termination, housekeeping) |
| DynamoDB Tables | State storage (jobs, locks, pools) |
| S3 Buckets | Cache artifacts |
| IAM Roles | ECS task role, EC2 instance profile |
| ECS Cluster + Service | Orchestrator runtime |
| API Gateway | Webhook endpoint |
| Launch Template | EC2 runner configuration |
| ECR Repository | Runner container images |

## 3. Build and Push Images

```bash
# Build and push orchestrator image
make docker-push

# Build and push runner image (multi-arch)
make docker-push-runner
```

Both EC2 and K8s deployments use the same runner container image.

## 4. Create Launch Template

Create a launch template with:

- **AMI**: Amazon Linux 2023 (Docker pre-installed)
- **Instance profile**: IAM role with ECR pull, SSM read, EC2 terminate permissions
- **User-data**: Copy contents of `scripts/bootstrap-linux.sh` to the launch template user-data field
- **Instance metadata options**: Enable IMDSv2 required, enable instance tags in metadata

**Important**: The user-data script (`scripts/bootstrap-linux.sh`) must be added to the launch template. This script:
- Authenticates to ECR using instance credentials
- Pulls the runner container image
- Reads configuration from instance tags set by the orchestrator
- Runs the container with the appropriate environment variables

Required IAM permissions for instance profile:
```json
{
  "Effect": "Allow",
  "Action": [
    "ecr:GetAuthorizationToken",
    "ecr:BatchGetImage",
    "ecr:GetDownloadUrlForLayer",
    "ssm:GetParameter",
    "sqs:SendMessage",
    "ec2:TerminateInstances",
    "ec2:DescribeTags"
  ],
  "Resource": "*"
}
```

## 5. Configure Environment

ECS task definition environment variables:

```bash
# Required
RUNS_FLEET_GITHUB_WEBHOOK_SECRET=<secret>
RUNS_FLEET_GITHUB_APP_ID=<app-id>
RUNS_FLEET_GITHUB_APP_PRIVATE_KEY=<pem-contents>

# Runner image (ECR URL)
RUNS_FLEET_RUNNER_IMAGE=<account>.dkr.ecr.<region>.amazonaws.com/runs-fleet-runner:latest

# AWS Resources
RUNS_FLEET_QUEUE_URL=https://sqs.<region>.amazonaws.com/<account>/runs-fleet-main.fifo
RUNS_FLEET_POOL_QUEUE_URL=https://sqs.<region>.amazonaws.com/<account>/runs-fleet-pool.fifo
RUNS_FLEET_EVENTS_QUEUE_URL=https://sqs.<region>.amazonaws.com/<account>/runs-fleet-events.fifo
RUNS_FLEET_TERMINATION_QUEUE_URL=https://sqs.<region>.amazonaws.com/<account>/runs-fleet-termination.fifo
RUNS_FLEET_JOBS_TABLE=runs-fleet-jobs
RUNS_FLEET_LOCKS_TABLE=runs-fleet-locks
RUNS_FLEET_POOLS_TABLE=runs-fleet-pools
RUNS_FLEET_CACHE_BUCKET=runs-fleet-cache

# EC2 Configuration
RUNS_FLEET_VPC_ID=vpc-xxx
RUNS_FLEET_PUBLIC_SUBNET_IDS=subnet-xxx,subnet-yyy
RUNS_FLEET_PRIVATE_SUBNET_IDS=subnet-aaa,subnet-bbb
RUNS_FLEET_SECURITY_GROUP_ID=sg-xxx
RUNS_FLEET_INSTANCE_PROFILE_ARN=arn:aws:iam::<account>:instance-profile/runs-fleet-runner

# Optional
RUNS_FLEET_SPOT_ENABLED=true
RUNS_FLEET_CACHE_SECRET=<32-char-secret>
RUNS_FLEET_COORDINATOR_ENABLED=true
RUNS_FLEET_INSTANCE_ID=${HOSTNAME}
```

## 6. Deploy ECS Service

```bash
# Apply Terraform
cd terraform/runs-fleet
terraform apply

# Force new deployment (after image push)
aws ecs update-service \
  --cluster runs-fleet \
  --service runs-fleet-orchestrator \
  --force-new-deployment
```

## 7. Configure Webhook

1. Get API Gateway endpoint URL
2. Update GitHub App webhook URL to: `https://<api-gateway-id>.execute-api.<region>.amazonaws.com/webhook`

## 8. Test

Add workflow to a repository:

```yaml
name: Test runs-fleet
on: push
jobs:
  test:
    runs-on: "runs-fleet=${{ github.run_id }}/runner=2cpu-linux-arm64"
    steps:
      - run: echo "Hello from runs-fleet"
```

## How It Works

1. Orchestrator receives webhook, creates runner config in SSM
2. EC2 Fleet launches instance with launch template
3. Instance boots, user-data script:
   - Authenticates to ECR
   - Pulls runner container image
   - Runs container with config from instance tags
4. Container agent:
   - Fetches full config from SSM
   - Registers with GitHub
   - Executes job
   - Sends telemetry to SQS
   - Terminates instance via EC2 API

## Troubleshooting

```bash
# Check orchestrator logs
aws logs tail /ecs/runs-fleet-orchestrator --follow

# Check SQS queue depth
aws sqs get-queue-attributes \
  --queue-url <queue-url> \
  --attribute-names ApproximateNumberOfMessages

# List running instances
aws ec2 describe-instances \
  --filters "Name=tag:runs-fleet:managed,Values=true" \
  --query 'Reservations[].Instances[].[InstanceId,State.Name,LaunchTime]'

# Check instance user-data logs
aws ssm start-session --target <instance-id>
cat /var/log/runs-fleet-bootstrap.log

# Check DynamoDB jobs
aws dynamodb scan --table-name runs-fleet-jobs --limit 10
```

## Cost Optimization

- Enable spot instances: `RUNS_FLEET_SPOT_ENABLED=true`
- Use ARM instances where possible (cheaper)
- Set S3 lifecycle policies (30-day expiry for cache)
- Configure warm pools to balance cost vs latency
