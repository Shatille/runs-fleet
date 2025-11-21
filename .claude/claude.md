# runs-fleet

Self-hosted ephemeral GitHub Actions runners on AWS. Orchestrates spot EC2 instances for workflow jobs.

## Stack

- Go 1.22+ (AWS SDK v2, go-github)
- AWS: EC2 Fleet API, SQS FIFO, DynamoDB, S3, SSM, ECS Fargate
- Infra: Terraform (separate repo: `shavakan-terraform/terraform/runs-fleet/`)
- Nix: Dev tooling (flake.nix)

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

## Code Layout

- `cmd/server/` - Fargate orchestrator (webhooks, queue processing, fleet management)
- `cmd/agent/` - EC2 instance bootstrap binary (registers runner, executes job)
- `pkg/github/` - GitHub API client (webhooks, JIT tokens, GraphQL)
- `pkg/fleet/` - EC2 fleet orchestration (spot strategy, launch templates)
- `pkg/pools/` - Warm pool reconciliation (hot/stopped instances)
- `pkg/cache/` - S3-backed GitHub Actions cache protocol
- `pkg/queue/` - SQS message processing (FIFO, batch, DLQ)
- `pkg/config/` - Config from env vars, AWS client initialization
- `internal/` - Shared utilities (DynamoDB state, metrics, logging)

## Job Labels

GitHub workflows request runners via labels:

```yaml
runs-on: "runs-fleet=${{ github.run_id }}/runner=2cpu-linux-arm64/pool=default"
```

Label format:
- `runs-fleet=<run-id>` - Workflow run identifier (required)
- `runner=<spec>` - Instance spec: `<cpu>cpu-<os>-<arch>[/<modifier>]`
- `pool=<name>` - Warm pool name (optional, routes to pool queue)
- `private=true` - Use private subnet with static egress
- `spot=false` - Force on-demand (skip spot)

Instance specs:
- `2cpu-linux-arm64` → t4g.medium (2 vCPU, 4GB, 30GB disk)
- `4cpu-linux-arm64` → c7g.xlarge (4 vCPU, 8GB, 50GB disk)
- `4cpu-linux-x64` → c6i.xlarge
- `8cpu-linux-arm64` → c7g.2xlarge (8 vCPU, 16GB, 100GB disk)
- `/large-disk` → 200GB disk

## Key Flows

**Cold-start:**
1. Webhook → SQS main queue → Fleet manager creates spot fleet
2. EC2 boots → Agent fetches config from SSM → Registers with GitHub
3. Job executes → Agent self-terminates

**Warm pool:**
1. Webhook with `pool=` label → Pool queue (batch processing)
2. Pool manager assigns running instance OR starts stopped instance OR overflows to cold-start
3. After job: instance detached, pool reconciliation creates replacement

**Spot interruption:**
1. EventBridge 2-min warning → Mark instance "terminating"
2. Re-queue in-progress job → New instance picks up work

## Data Stores

**DynamoDB:**
- `runs-fleet-locks` - Distributed locks
- `runs-fleet-jobs` - Job state (GSI: next-check-time)

**S3:**
- `runs-fleet-cache` - GitHub Actions cache artifacts (30-day lifecycle)
- `runs-fleet-config` - Runner configs, agent binaries

**SQS:**
- Main queue - Job requests (FIFO)
- Pool queue - Batch warm pool jobs
- Events queue - EventBridge events (spot interruptions, cost reports)
- Termination queue - Instance shutdown notifications
- Housekeeping queue - Cleanup tasks

## Environment Config

Critical env vars:
- `AWS_REGION` - ap-northeast-1
- `RUNS_FLEET_GITHUB_ORG` - GitHub org
- `RUNS_FLEET_GITHUB_APP_ID`, `RUNS_FLEET_GITHUB_APP_PRIVATE_KEY` - GitHub App auth
- `RUNS_FLEET_GITHUB_WEBHOOK_SECRET` - HMAC signature validation
- `RUNS_FLEET_QUEUE_URL` - Main SQS queue
- `RUNS_FLEET_POOL_QUEUE_URL` - Pool queue
- `RUNS_FLEET_*_TABLE` - DynamoDB tables
- `RUNS_FLEET_*_BUCKET` - S3 buckets
- `RUNS_FLEET_VPC_ID`, `RUNS_FLEET_*_SUBNET_IDS` - Network config
- `RUNS_FLEET_SPOT_ENABLED` - Enable spot instances (default: true)

## Development Commands

```bash
# Run server locally (requires AWS credentials)
export AWS_REGION=ap-northeast-1
export RUNS_FLEET_QUEUE_URL=...
go run cmd/server/main.go

# Build agent binaries
make build-agent

# Build Docker image
make docker-build

# Run tests
go test ./...

# Deploy to ECS (via Terraform in separate repo)
cd ~/workspace/shavakan-terraform/terraform/runs-fleet
terraform apply
```

## Conventions

**Error handling:**
- Wrap errors with context: `fmt.Errorf("failed to create fleet: %w", err)`
- Return errors up stack, handle at service boundary
- Log errors with structured fields (use internal/logging)

**AWS SDK:**
- Clients initialized in `pkg/config/config.go`
- Use context.Context for cancellation
- Retry logic: SDK defaults (exponential backoff)
- Paginate large result sets (ListInstances, DescribeFleetInstances)

**Queue processing:**
- FIFO guarantees ordering per message group ID
- Batch size: 10 messages max
- Visibility timeout: 5 minutes
- DLQ after 3 receive attempts

**Metrics:**
- Publish to CloudWatch via internal/metrics
- Namespace: `RunsFleet`
- Dimensions: `Environment=production`, `Service=orchestrator`

**Pool reconciliation:**
- Loop interval: 60 seconds
- Hot pool: instances stay running
- Stopped pool: instances stopped, batch-started on-demand (max 50)
- Idle timeout: 60 minutes (configurable per pool)

## Code Writing Workflow

**Mandatory process for all code changes:**

1. **Test-Driven Development (TDD)**
   - Write failing test first
   - Implement minimal code to pass
   - Refactor while keeping tests green
   - No code without corresponding tests

2. **Pre-commit validation**
   - Run `make lint` - must pass with zero warnings
   - Run `make test` - all tests must pass
   - Fix all failures before proceeding

3. **Code review**
   - Invoke `code-reviewer` agent (shavakan-agents:code-reviewer)
   - Address ALL comments and findings
   - Re-request review after changes
   - Iterate until agent produces zero actionable comments

4. **Commit**
   - Use `git-commit` skill (shavakan-skills:git-commit)
   - Do NOT use manual git commands for commits

**Non-negotiable:** Code that fails lint, tests, or review does not get committed.

## Design Principles

- Spot-first with on-demand fallback (cost optimization)
- Ephemeral instances (no reuse, no state accumulation)
- Eventual consistency (DynamoDB, SQS, EC2 API)
- Graceful degradation (spot interruptions, API throttling)
- Right-sized instances (ARM preferred, minimal disk)

## Cost Model

Target: ~$55-65/month for 100 jobs/day @ 10 min avg runtime
- EC2 spot: ~$15-20/month
- Fargate orchestrator: $36/month (1 vCPU, 2GB)
- S3 cache: $1-5/month
- Supporting services: $2-3/month

Compare to GitHub hosted runners: $80/month

## Security

- Webhook HMAC-SHA256 validation
- IMDSv2 required on EC2
- Least-privilege IAM (instance profile, task role)
- S3 pre-signed URLs (15-min expiry)
- Encrypted EBS volumes
- Secrets in AWS Secrets Manager (not env vars)

## Roadmap Status

Phase 1 (Cold-Start MVP): In progress
Phase 2 (Warm Pools): Not started
Phase 3 (S3 Cache): Not started
Phase 4 (Production Hardening): Not started

See README.md for detailed roadmap.
