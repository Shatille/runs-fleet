# runs-fleet

Self-hosted ephemeral GitHub Actions runners on AWS. Orchestrates spot EC2 instances for workflow jobs.

## Stack

- Go 1.25+ (AWS SDK v2, go-github)
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
- `pkg/admin/` - Admin API and web UI for fleet management
- `pkg/agent/` - Agent runtime logic (job execution, telemetry)
- `pkg/cache/` - S3-backed GitHub Actions cache protocol
- `pkg/circuit/` - Circuit breaker for instance type failures
- `pkg/config/` - Config from env vars, AWS client initialization
- `pkg/cost/` - Cost reporting and pricing calculations
- `pkg/db/` - DynamoDB state management (jobs, pools, locks)
- `pkg/events/` - EventBridge event processing (spot interruptions)
- `pkg/fleet/` - EC2 fleet orchestration (spot strategy, launch templates)
- `pkg/github/` - GitHub API client (webhooks, JIT tokens, GraphQL)
- `pkg/gitops/` - GitOps integration for pool configuration
- `pkg/housekeeping/` - Cleanup tasks (orphaned instances, stale SSM, old jobs)
- `pkg/metrics/` - Multi-backend metrics (CloudWatch, Prometheus, Datadog)
- `pkg/pools/` - Warm pool reconciliation (hot/stopped instances)
- `pkg/provider/` - Compute provider abstraction (ec2/, k8s/ backends)
- `pkg/queue/` - Queue abstraction (SQS for EC2, Valkey for K8s)
- `pkg/runner/` - Runner lifecycle management
- `pkg/secrets/` - Secrets backend abstraction (SSM, Vault)
- `pkg/state/` - State machine for job lifecycle
- `pkg/termination/` - Instance termination notifications
- `pkg/tracing/` - OpenTelemetry distributed tracing

## Distributed Locking

**Purpose**: Enable multi-instance Fargate deployments without global leader election.

**Implementation**: Per-pool reconciliation locks stored in pools DynamoDB table.

**How it works:**
- Each pool has `reconcile_lock_owner` and `reconcile_lock_expires` attributes
- Before reconciling a pool, instance acquires lock with 65s TTL (> 60s reconcile interval)
- Lock acquisition uses DynamoDB conditional writes (atomic)
- On completion or failure, lock is released
- Expired locks are automatically overwritten

**Behavior:**
- Multiple instances can reconcile different pools concurrently
- Same pool is reconciled by only one instance at a time
- Lock expiration prevents deadlocks from crashed instances
- K8s backend: relies on API idempotency (no locks needed)

## Job Labels

GitHub workflows request runners via labels:

```yaml
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4"
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64/pool=default"
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4+16/ram=8+32/family=c7g+m7g"
```

### Resource labels

- `cpu=<n>` - Exact vCPU count
- `cpu=<min>+<max>` - vCPU range for spot diversification
- `ram=<n>` - Exact RAM in GB
- `ram=<min>+<max>` - RAM range in GB
- `family=<f1>+<f2>` - Instance families (e.g., `c7g+m7g`)
- `arch=<arch>` - Architecture: `arm64` or `amd64`
- `disk=<size>` - Disk in GiB (1-16384, gp3)

### Routing labels

- `runs-fleet=<run-id>` - Workflow run identifier (required)
- `pool=<name>` - Warm pool name (routes to pool queue)
- `spot=false` - Force on-demand (skip spot)
- `public=true` - Request public IP (uses public subnet; default: private subnet preferred)
- `backend=<ec2|k8s>` - Override default compute backend
- `region=<region>` - Target AWS region (multi-region support)
- `env=<dev|staging|prod>` - Environment isolation

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
- `runs-fleet-jobs` - Job state (primary key: job_id or instance_id)
- `runs-fleet-pools` - Pool configuration, reconciliation locks, and task locks
- `runs-fleet-circuit-state` - Circuit breaker state for instance types

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

### Core
- `AWS_REGION` - AWS region (default: ap-northeast-1)
- `RUNS_FLEET_MODE` - Compute backend: `ec2` or `k8s` (default: ec2)
- `RUNS_FLEET_GITHUB_APP_ID`, `RUNS_FLEET_GITHUB_APP_PRIVATE_KEY` - GitHub App auth
- `RUNS_FLEET_GITHUB_WEBHOOK_SECRET` - HMAC signature validation

### EC2 Backend
- `RUNS_FLEET_QUEUE_URL` - Main SQS queue (required for EC2)
- `RUNS_FLEET_POOL_QUEUE_URL` - Pool queue
- `RUNS_FLEET_EVENTS_QUEUE_URL` - EventBridge events queue
- `RUNS_FLEET_TERMINATION_QUEUE_URL` - Termination notifications queue
- `RUNS_FLEET_HOUSEKEEPING_QUEUE_URL` - Housekeeping tasks queue
- `RUNS_FLEET_JOBS_TABLE`, `RUNS_FLEET_POOLS_TABLE` - DynamoDB tables
- `RUNS_FLEET_CACHE_BUCKET` - S3 cache bucket
- `RUNS_FLEET_VPC_ID`, `RUNS_FLEET_*_SUBNET_IDS` - Network config
- `RUNS_FLEET_SECURITY_GROUP_ID`, `RUNS_FLEET_INSTANCE_PROFILE_ARN` - EC2 config
- `RUNS_FLEET_RUNNER_IMAGE` - ECR image URL for runners
- `RUNS_FLEET_SPOT_ENABLED` - Enable spot instances (default: true)

### K8s Backend
- `RUNS_FLEET_VALKEY_ADDR` - Valkey/Redis address (required for K8s)
- `RUNS_FLEET_VALKEY_PASSWORD` - Valkey password (optional)
- `RUNS_FLEET_KUBE_NAMESPACE` - Runner namespace
- `RUNS_FLEET_KUBE_RUNNER_IMAGE` - Runner container image

### Secrets Backend
- `RUNS_FLEET_SECRETS_BACKEND` - `ssm` or `vault` (default: ssm)
- `VAULT_ADDR` - Vault server address (required if vault backend)
- `VAULT_AUTH_METHOD` - `aws`, `kubernetes`, `approle`, `token` (default: aws)

### Metrics
- `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` - CloudWatch metrics (default: true)
- `RUNS_FLEET_METRICS_PROMETHEUS_ENABLED` - Prometheus /metrics endpoint
- `RUNS_FLEET_METRICS_DATADOG_ENABLED` - Datadog DogStatsD metrics

### Cache & Admin
- `RUNS_FLEET_CACHE_SECRET` - HMAC secret for cache auth
- `RUNS_FLEET_ADMIN_SECRET` - Admin API authentication

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
- Log errors with structured fields

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
- Publish to CloudWatch, Prometheus, or Datadog via pkg/metrics
- Namespace: `RunsFleet` (CloudWatch) or `runs_fleet` (Prometheus/Datadog)
- Dimensions: `PoolName`, `TaskType`, `InstanceType` (metric-specific)

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

## Git Workflow

**Maintain linear history at all times:**

1. **Rebase over merge**
   - NEVER create merge commits
   - Use `git pull --rebase` instead of `git pull`
   - Rebase feature branches on target branch before PR

2. **PR/Branch strategy**
   - Fast-forward merges only to main
   - Squash commits if branch has WIP/fixup commits
   - Each PR represents one logical change
   - Delete branches after merge

3. **Commit atomicity**
   - Each commit must be complete and functional
   - Commit should pass all tests independently
   - One logical change per commit (single responsibility)
   - No "fix typo" or "forgot to add file" commits (amend or rebase)

**Pre-push checklist:**
- `git log --oneline --graph` shows straight line (no branches/merges)
- Each commit message follows conventional commits format
- Each commit passes `make lint && make test`

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

**Cost reporting caveats (pkg/cost/):**
- Hard-coded pricing for 3 instance families only (t4g, c7g, m7g)
- 70% spot discount is fixed assumption
- Regional price variations not included
- Data transfer and S3 request costs excluded
- Estimates only, not exact billing

## Security

- Webhook HMAC-SHA256 validation
- IMDSv2 required on EC2
- Least-privilege IAM (instance profile, task role)
- S3 pre-signed URLs (15-min expiry)
- Encrypted EBS volumes
- Secrets in AWS Secrets Manager (not env vars)

## Roadmap Status

Phase 1 (Cold-Start MVP): ✅ Complete
Phase 2 (Warm Pools): ✅ Complete
Phase 3 (S3 Cache): ✅ Complete
Phase 4 (Production Hardening): ✅ Complete
Phase 5 (Concurrent Processing): ✅ Complete

See README.md for detailed roadmap.
