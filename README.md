# runs-fleet

> Self-hosted ephemeral GitHub Actions runners on AWS, orchestrating spot EC2 instances for workflow jobs

Inspired by [runs-on](https://github.com/runs-on/runs-on). Fleet orchestration system with warm pools, S3 caching, and spot-first cost optimization.

## Architecture

```
GitHub Webhook → API Gateway → SQS FIFO
                                   ↓
                Orchestrator (Go on Fargate)
                ├── Queue processors (main, pool, events)
                ├── Pool manager (warm instances)
                └── Fleet manager (EC2 API)
                                   ↓
                EC2 Spot/On-demand Fleet
                                   ↓
                Runner Instances (ephemeral)
                └── Agent binary (bootstrap, self-terminate)
```

**Design tradeoffs:**
- Spot-first (70% cost savings) with on-demand fallback and circuit breaker for interruption storms
- Ephemeral instances (no reuse) to avoid state accumulation, simplified cleanup
- Warm pools trade idle cost for <10s job start latency vs ~60s cold-start
- S3 cache reduces GitHub API calls and speeds up workflows with dependencies

## Quick Start

**Prerequisites:**
- Nix with flakes enabled
- direnv (optional but recommended)
- AWS credentials configured
- Terraform infrastructure deployed (see `shavakan-terraform/terraform/runs-fleet/`)

**Setup:**

```bash
# Clone and enter directory
git clone https://github.com/Shavakan/runs-fleet.git
cd runs-fleet

# If using direnv
cp .envrc.example .envrc
# Edit .envrc with your AWS config
direnv allow

# Enter Nix dev shell (if not using direnv)
nix develop
```

**Build:**

```bash
# Using Nix (recommended)
nix build .#server        # Server binary
nix build .#agent-amd64   # AMD64 agent
nix build .#agent-arm64   # ARM64 agent
nix build .#docker        # Docker image

# Or using Make
make build                # Build all binaries
make test                 # Run tests with coverage
make lint                 # Run golangci-lint
```

**Run locally:**

```bash
# Requires AWS credentials and infrastructure deployed
export AWS_REGION=ap-northeast-1
export RUNS_FLEET_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/...
export RUNS_FLEET_GITHUB_WEBHOOK_SECRET=your-secret
go run cmd/server/main.go
```

## Project Structure

```
cmd/
  server/         # Fargate orchestrator (webhooks, queue processing, fleet mgmt)
  agent/          # EC2 bootstrap binary (registers runner, executes job)
pkg/
  github/         # Webhook validation, label parsing
  fleet/          # EC2 fleet creation (spot strategy, launch templates)
  pools/          # Warm pool reconciliation (hot/stopped instances)
  cache/          # S3-backed GitHub Actions cache protocol
  queue/          # SQS message processing (FIFO, batch, DLQ)
  db/             # DynamoDB state management
  events/         # EventBridge processing (spot interruptions)
  termination/    # Instance shutdown notifications
  housekeeping/   # Cleanup tasks (orphaned instances, stale SSM)
  cost/           # Cost reporting and pricing calculations
  config/         # Env var parsing, AWS client initialization
```

## Job Label Format

Workflows specify requirements via labels:

```yaml
runs-on: "runs-fleet=${{ github.run_id }}/runner=2cpu-linux-arm64/pool=default"
```

**Labels:**
- `runs-fleet=<run-id>` - Workflow run identifier (required)
- `runner=<spec>` - Instance spec: `<cpu>cpu-<os>-<arch>[/<modifier>]`
- `pool=<name>` - Warm pool name (routes to pool queue for fast start)
- `private=true` - Use private subnet with static egress
- `spot=false` - Force on-demand (skip spot)

**Instance specs:**
- `2cpu-linux-arm64` → t4g.medium (2 vCPU, 4GB, 30GB disk)
- `4cpu-linux-arm64` → c7g.xlarge (4 vCPU, 8GB, 50GB disk)
- `4cpu-linux-x64` → c6i.xlarge
- `8cpu-linux-arm64` → c7g.2xlarge (8 vCPU, 16GB, 100GB disk)
- `/large-disk` → 200GB disk

## Flows

**Cold-start:**
1. Webhook → SQS → Fleet manager creates spot fleet (~60s)
2. EC2 boots → Agent fetches config from SSM → Registers with GitHub
3. Job executes → Agent self-terminates

**Warm pool:**
1. Webhook with `pool=` label → Pool queue (batch processing)
2. Pool manager assigns running instance (~10s) OR starts stopped instance (~30s) OR overflows to cold-start
3. After job: instance detached, reconciliation loop creates replacement

**Spot interruption:**
1. EventBridge 2-min warning → Mark instance terminating
2. Re-queue in-progress job with `ForceOnDemand=true` → New instance picks up

## Configuration

Required environment variables:

```bash
# GitHub
RUNS_FLEET_GITHUB_ORG=Shavakan
RUNS_FLEET_GITHUB_APP_ID=<app-id>
RUNS_FLEET_GITHUB_APP_PRIVATE_KEY=<key>
RUNS_FLEET_GITHUB_WEBHOOK_SECRET=<secret>

# AWS Infrastructure (from Terraform)
AWS_REGION=ap-northeast-1
RUNS_FLEET_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/.../main
RUNS_FLEET_POOL_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/.../pool
RUNS_FLEET_EVENTS_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/.../events
RUNS_FLEET_TERMINATION_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/.../termination
RUNS_FLEET_HOUSEKEEPING_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/.../housekeeping
RUNS_FLEET_LOCKS_TABLE=runs-fleet-locks
RUNS_FLEET_JOBS_TABLE=runs-fleet-jobs
RUNS_FLEET_CACHE_BUCKET=runs-fleet-cache
RUNS_FLEET_CONFIG_BUCKET=runs-fleet-config

# EC2
RUNS_FLEET_VPC_ID=vpc-...
RUNS_FLEET_PUBLIC_SUBNET_IDS=subnet-...,subnet-...,subnet-...
RUNS_FLEET_SECURITY_GROUP_ID=sg-...
RUNS_FLEET_INSTANCE_PROFILE_ARN=arn:aws:iam::...:instance-profile/runs-fleet-runner

# Behavior
RUNS_FLEET_SPOT_ENABLED=true
RUNS_FLEET_MAX_RUNTIME_MINUTES=360
```

See `.envrc.example` for complete list.

## Development

**Common tasks:**

```bash
# Run tests
make test          # or: nix flake check

# Lint
make lint

# Build Docker image
make docker-build

# Push to ECR
make docker-push AWS_REGION=ap-northeast-1
```

**Deployment:**

Infrastructure managed via Terraform in separate repo (`shavakan-terraform/terraform/runs-fleet/`).

```bash
# Build and push image to ECR
make docker-push

# Apply Terraform
cd ~/workspace/shavakan-terraform/terraform/runs-fleet
terraform apply
```

## Cost Model

Target: ~$55-65/month for 100 jobs/day @ 10 min avg runtime

- EC2 spot: ~$15-20/month (ephemeral instances)
- Fargate orchestrator: $36/month (1 vCPU, 2GB)
- S3 cache: $1-5/month
- Supporting services: $2-3/month

Compare to GitHub hosted runners: $80/month

**Cost reporting caveats:**
- Hard-coded pricing for 3 instance families (t4g, c7g, m7g) in pkg/cost/
- 70% spot discount is fixed assumption
- Regional price variations not included
- Data transfer and S3 request costs excluded

## Monitoring

**CloudWatch metrics:**
- `RunsFleet/QueueDepth` - Jobs waiting for instances
- `RunsFleet/FleetSize` - Active runners
- `RunsFleet/JobDuration` - Execution time
- `RunsFleet/SpotInterruptions` - Termination rate
- `RunsFleet/CacheHitRate` - Cache effectiveness
- `RunsFleet/PoolUtilization` - Warm pool usage

## Security

- Webhook HMAC-SHA256 validation
- IMDSv2 required on EC2
- Least-privilege IAM roles
- S3 pre-signed URLs (15-min expiry)
- Encrypted EBS volumes
- Secrets in AWS Secrets Manager

## License

MIT

## Acknowledgments

Deeply inspired by [runs-on](https://github.com/runs-on/runs-on) by Cyril Rohr.
