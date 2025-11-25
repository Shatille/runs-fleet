# runs-fleet

Self-hosted ephemeral GitHub Actions runners on AWS, inspired by [runs-on](https://github.com/runs-on/runs-on).

Fleet orchestration system that creates ephemeral EC2 spot instances for GitHub Actions workflows, with warm pool support, S3 caching, and multi-instance type support.

## Architecture

```
GitHub Webhook → API Gateway → SQS FIFO Queue
                                    ↓
                    Orchestrator (Go on ECS Fargate)
                    ├── Queue Processor (main queue)
                    ├── Pool Manager (warm pools)
                    ├── Fleet Manager (EC2 API)
                    └── Cache Server (S3 backend)
                                    ↓
                    EC2 Fleet API
                    ├── Spot instances (price-capacity-optimized)
                    └── On-demand fallback
                                    ↓
                    Runner Instance (ephemeral)
                    ├── Agent binary
                    ├── GitHub Actions runner
                    └── Custom AMI (Python 3.13.9, Node.js 24)
```

## Components

### Server (`cmd/server/`)

Fargate service that orchestrates the entire runner lifecycle.

**Responsibilities:**
- Receive GitHub workflow job webhooks via API Gateway
- Parse job labels to determine instance requirements
- Manage SQS queue processing (FIFO + batch)
- Create/manage EC2 fleets with spot+on-demand strategy
- Reconcile warm pools (hot and stopped instances)
- Serve S3-backed cache API for GitHub Actions
- Track job state in DynamoDB
- Handle spot interruption notifications
- Cleanup stale instances and resources

**HTTP Endpoints:**
- `POST /webhook` - GitHub webhook receiver
- `GET /_artifacts/*` - GitHub Actions cache API (download)
- `POST /_apis/artifactcache/*` - GitHub Actions cache API (upload)
- `GET /health` - Health check for ECS

**Queue Processors:**
- Main queue: Job requests (FIFO) ✅
- Pool queue: Batch processing for warm pool jobs ✅
- Events queue: EventBridge events (spot interruptions, cost reports) ✅
- Housekeeping queue: Cleanup tasks 🚧 *Planned*
- Termination queue: Instance shutdown handling 🚧 *Planned*

### Agent (`cmd/agent/`)

Lightweight binary that runs on EC2 instances to bootstrap the GitHub Actions runner.

**Responsibilities:**
- Fetch runner configuration from SSM Parameter Store
- Download and install GitHub Actions runner binary
- Register runner with GitHub using JIT token
- Execute workflow job
- Report telemetry to server
- Self-terminate instance after job completion

**Environment Variables:**
- `RUNS_FLEET_INSTANCE_ID` - EC2 instance ID
- `RUNS_FLEET_SSM_PARAMETER` - Path to runner config in SSM
- `RUNS_FLEET_LOG_GROUP` - CloudWatch log group
- `RUNS_FLEET_MAX_RUNTIME` - Maximum job runtime (safety)
- `RUNS_FLEET_S3_CACHE_ENDPOINT` - Cache server endpoint

## Package Structure

### `pkg/github`
- GitHub API client (REST + GraphQL)
- Webhook signature validation
- Runner registration token generation
- JIT token management

### `pkg/fleet`
- EC2 fleet creation (CreateFleet API)
- Spot allocation strategy (price-capacity-optimized)
- On-demand fallback logic
- Instance type selection based on job labels
- Launch template management
- Instance termination

### `pkg/pools`
- Warm pool reconciliation loop
- Hot pools (running instances ready for jobs)
- Stopped pools (pre-warmed, start on-demand)
- Pool size management
- Instance assignment to jobs
- Batch start operations (up to 50 instances)

### `pkg/cache`
- S3-backed GitHub Actions cache protocol
- Cache key/version management
- Pre-signed URL generation
- Lifecycle policies

### `pkg/queue`
- SQS message processing
- FIFO ordering guarantees
- Batch processing (up to 10 messages)
- Dead letter queue handling
- Message retry logic

### `pkg/config`
- Configuration from environment variables
- AWS SDK clients (EC2, SQS, DynamoDB, S3, SSM)
- Region detection
- Credential management

### `pkg/db/`
- DynamoDB state management (pool configuration)
- Database client abstraction

### `pkg/metrics/`
- CloudWatch metrics publishing
- Performance tracking

## Job Label Format

Inspired by runs-on, jobs specify requirements via labels:

```yaml
runs-on: "runs-fleet=${{ github.run_id }}/runner=2cpu-linux-arm64/pool=default"
```

**Label components:**
- `runs-fleet=<run-id>` - Unique identifier for this workflow run
- `runner=<spec>` - Instance specification
  - Format: `<cpu>cpu-<os>-<arch>[/<modifier>]`
  - Examples: `2cpu-linux-arm64`, `4cpu-linux-x64`, `8cpu-linux-x64/large-disk`
- `pool=<name>` - Optional warm pool name (e.g., `default`, `heavy-builds`)
- `private=true` - Launch in private subnet with static egress IP
- `spot=false` - Force on-demand (skip spot)

**Instance specifications:**
- `2cpu-linux-arm64` → t4g.medium (2 vCPU, 4 GB, 30 GB disk)
- `4cpu-linux-arm64` → c7g.xlarge (4 vCPU, 8 GB, 50 GB disk)
- `4cpu-linux-x64` → c6i.xlarge (4 vCPU, 8 GB, 50 GB disk)
- `8cpu-linux-arm64` → c7g.2xlarge (8 vCPU, 16 GB, 100 GB disk)
- `/large-disk` modifier → 200 GB disk instead of default

## Infrastructure Requirements

Managed via Terraform in `shavakan-terraform/terraform/runs-fleet/`:

### AWS Resources
- **SQS Queues** (FIFO + dead letter queues)
  - Main queue (job requests) ✅
  - Pool queue (batch processing) ✅
  - Events queue (EventBridge events) ✅
  - Housekeeping queue (cleanup) 🚧 *Planned*
  - Termination queue (shutdown notifications) 🚧 *Planned*
- **DynamoDB Tables**
  - Locks table (distributed locking)
  - Workflow jobs table (job state tracking, GSI for next-check queries)
- **S3 Buckets**
  - Cache bucket (GitHub Actions cache artifacts)
  - Config bucket (runner configurations, agent binary)
- **ECS Fargate Service**
  - Task definition (1 vCPU, 2 GB)
  - Auto-scaling (min 1, max 10 tasks)
  - Service discovery (for cache endpoint)
- **API Gateway**
  - REST API for GitHub webhooks
  - Routes to Fargate ALB target group
- **EventBridge Rules**
  - EC2 Spot Instance Interruption Warning
  - EC2 Instance State-change Notification
  - Scheduled cost reports (daily)
- **IAM Roles**
  - Fargate task role (EC2, SQS, DynamoDB, S3, SSM permissions)
  - EC2 instance role (SSM, CloudWatch Logs, S3 cache access)
- **CloudWatch**
  - Log groups (server, agent)
  - Custom metrics (queue depth, fleet size, job duration)
  - Alarms (queue age, fleet errors)
- **VPC Resources**
  - Public subnets (default runners)
  - Private subnets (optional, for `private=true` jobs)
  - Security groups (Fargate, EC2 runners)

## Data Flow

### Cold-Start Job Flow
1. GitHub sends webhook on `workflow_job.queued` event
2. API Gateway routes to Fargate service `/webhook` endpoint
3. Server validates webhook signature, parses job labels
4. Server enqueues job to SQS main queue (FIFO)
5. Queue processor picks up message, determines instance type
6. Fleet manager creates EC2 fleet (spot with on-demand fallback)
7. Fleet manager stores runner config in SSM Parameter Store
8. EC2 instance boots with user data pointing to agent
9. Agent fetches config from SSM, registers with GitHub
10. GitHub assigns job to runner, workflow executes
11. Agent self-terminates instance after job completion
12. EventBridge notifies server of termination 🚧 *Planned: Housekeeping queue cleanup*

### Warm Pool Job Flow
1. GitHub sends webhook with `pool=default` label (or dependabot job)
2. Server routes to pool queue instead of main queue
3. Pool queue processor batches up to 10 jobs
4. Pool manager checks pool inventory (running > stopped > cold)
5. If available: assign instance, upload runner config to SSM
6. If stopped: batch-start instances (up to 50 via EC2 API)
7. If exhausted: overflow to main queue for cold-start
8. Instance picks up config, registers, executes job
9. After completion, instance detached from pool (not recycled)
10. Pool reconciliation loop creates replacement instance

### Spot Interruption Flow
1. AWS sends 2-minute warning via EventBridge
2. Events queue processor receives notification
3. Server marks instance as "terminating" in DynamoDB
4. If job in-progress: Server re-queues job to main queue
5. Instance terminates, new instance picks up re-queued job
6. GitHub runner handles graceful shutdown (if time permits)

## Warm Pool Management

**Pool types:**
- **Hot pools:** Instances stay running, ready immediately (<10s job start)
- **Stopped pools:** Instances pre-warmed then stopped, started on-demand (~30s job start)

**Reconciliation loop (every 60s):**
1. Query DynamoDB for configured pools
2. For each pool:
   - Count running instances
   - Count stopped instances
   - Calculate deficit vs desired size
   - Create/terminate instances to match desired state
3. Cleanup abandoned instances (>max-idle-time)

**Pool configuration (DynamoDB):**
```json
{
  "pool_name": "default",
  "instance_type": "t4g.medium",
  "desired_running": 2,
  "desired_stopped": 5,
  "max_idle_minutes": 60
}
```

## S3 Cache Layer

Implements GitHub Actions cache protocol:

**Cache upload:**
```
POST /_apis/artifactcache/caches
→ Returns cache ID and pre-signed S3 upload URL

PATCH /_apis/artifactcache/caches/{id}
→ Commits cache entry
```

**Cache download:**
```
GET /_apis/artifactcache/cache?keys=<keys>&version=<version>
→ Returns cache metadata

GET /_artifacts/{cache_id}/{file}
→ Redirects to pre-signed S3 download URL
```

**S3 bucket structure:**
```
s3://runs-fleet-cache/
├── caches/
│   └── {org}/{repo}/{key}/{version}/
│       └── cache.tgz
└── metadata/
    └── {org}/{repo}/{key}.json
```

## Cost Optimization

**Spot instance strategy:**
- Primary: Spot with `price-capacity-optimized` allocation
- Fallback: On-demand if spot unavailable or interrupted >3 times
- Circuit breaker: Disable spot for 15 minutes after 5 failures

**Instance sizing:**
- ARM (t4g/c7g) preferred over x86 (~20% cost savings)
- Right-sized disk (30/50/100/200 GB based on job requirements)
- EBS gp3 with baseline throughput (no IOPS provisioning)

**Warm pool economics:**
- Hot pools: $0.0168/hour per t4g.medium = $12.10/month per instance
- Stopped pools: $0.003/GB-month for 30GB EBS = $0.09/month per instance
- Trade-off: Job start latency vs idle cost

**Expected costs (100 jobs/day, avg 10 min runtime):**
- EC2 spot: ~$15-20/month (ephemeral instances)
- Fargate orchestrator: $36/month (1 vCPU, 2 GB)
- S3 cache: $1-5/month (depends on cache size)
- Supporting services (SQS, DynamoDB, CloudWatch): $2-3/month
- **Total: ~$55-65/month**

Compare to:
- GitHub hosted runners (100 jobs × 10 min × $0.008/min): $80/month
- Runs-on license + AWS: $98/month ($73 AWS + $25 license)
- Always-on t4g.medium: $12-15/month (but no burst capacity)

## Development

**Prerequisites:**
- Go 1.22+
- AWS CLI configured with credentials
- Docker (for building container images)

**Local development:**
```bash
# Run server locally (requires AWS credentials)
export AWS_REGION=ap-northeast-1
export RUNS_FLEET_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/...
export RUNS_FLEET_GITHUB_WEBHOOK_SECRET=...
go run cmd/server/main.go

# Build agent binary
GOOS=linux GOARCH=arm64 go build -o bin/agent-linux-arm64 cmd/agent/main.go
GOOS=linux GOARCH=amd64 go build -o bin/agent-linux-amd64 cmd/agent/main.go

# Build Docker image
docker build -t runs-fleet:latest .

# Run tests
go test ./...
```

**Deployment:**
```bash
# Build and push to ECR
aws ecr get-login-password --region ap-northeast-1 | \
  docker login --username AWS --password-stdin <account>.dkr.ecr.ap-northeast-1.amazonaws.com
docker build -t <account>.dkr.ecr.ap-northeast-1.amazonaws.com/runs-fleet:latest .
docker push <account>.dkr.ecr.ap-northeast-1.amazonaws.com/runs-fleet:latest

# Apply Terraform (from shavakan-terraform repo)
cd ~/workspace/shavakan-terraform/terraform/runs-fleet
terraform apply
```

## Configuration

Server configuration via environment variables:

```bash
# AWS
AWS_REGION=ap-northeast-1

# GitHub
RUNS_FLEET_GITHUB_ORG=Shavakan
RUNS_FLEET_GITHUB_WEBHOOK_SECRET=<secret>
RUNS_FLEET_GITHUB_APP_ID=<app-id>
RUNS_FLEET_GITHUB_APP_PRIVATE_KEY=<key>

# Infrastructure
RUNS_FLEET_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/.../main
RUNS_FLEET_POOL_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/.../pool
RUNS_FLEET_EVENTS_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/.../events
RUNS_FLEET_LOCKS_TABLE=runs-fleet-locks
RUNS_FLEET_JOBS_TABLE=runs-fleet-jobs
RUNS_FLEET_CACHE_BUCKET=runs-fleet-cache
RUNS_FLEET_CONFIG_BUCKET=runs-fleet-config

# EC2
RUNS_FLEET_VPC_ID=vpc-...
RUNS_FLEET_PUBLIC_SUBNET_IDS=subnet-...,subnet-...,subnet-...
RUNS_FLEET_PRIVATE_SUBNET_IDS=subnet-...,subnet-...,subnet-...
RUNS_FLEET_SECURITY_GROUP_ID=sg-...
RUNS_FLEET_INSTANCE_PROFILE_ARN=arn:aws:iam::...:instance-profile/runs-fleet-runner
RUNS_FLEET_KEY_NAME=shavakan_190914

# Behavior
RUNS_FLEET_SPOT_ENABLED=true
RUNS_FLEET_MAX_RUNTIME_MINUTES=360
RUNS_FLEET_LOG_LEVEL=info
```

## Monitoring

**CloudWatch Metrics:**
- `RunsFleet/QueueDepth` - Jobs waiting for instances
- `RunsFleet/FleetSize` - Active runner instances
- `RunsFleet/JobDuration` - Job execution time
- `RunsFleet/SpotInterruptions` - Spot instance terminations
- `RunsFleet/CacheHitRate` - S3 cache effectiveness
- `RunsFleet/PoolUtilization` - Warm pool usage

**Alarms:**
- Queue age >300 seconds (jobs backing up)
- Fleet errors >5 in 5 minutes (EC2 API failures)
- Spot interruption rate >20% (consider on-demand)

## Debugging Access

### SSH Access

Runner instances can be accessed via SSH for debugging purposes. This requires Terraform infrastructure configuration:

**Prerequisites:**
1. SSH security group rule allowing port 22 from your IP (configured in Terraform)
2. EC2 key pair specified in launch template
3. Instance running in public subnet (or via bastion host)

**Connect via SSH:**
```bash
# Find the instance IP from the job logs or AWS console
ssh -i ~/.ssh/your-key.pem ec2-user@<instance-public-ip>

# For debugging a running job, check the runner directory
ls -la /home/ec2-user/actions-runner/
cat /home/ec2-user/actions-runner/_diag/Runner_*.log
```

### AWS Systems Manager Session Manager

For instances in private subnets or when SSH is not configured, use AWS Systems Manager Session Manager:

**Prerequisites:**
- IAM instance profile with SSM permissions (AmazonSSMManagedInstanceCore policy)
- SSM agent installed and running on the instance (pre-installed on Amazon Linux 2)

**Connect via Session Manager:**
```bash
# Using AWS CLI
aws ssm start-session --target <instance-id> --region <region>

# Or via AWS Console:
# 1. Navigate to EC2 > Instances
# 2. Select the runner instance
# 3. Click "Connect" > "Session Manager"
```

**Advantages of Session Manager:**
- No need for SSH keys or bastion hosts
- Works with private subnet instances
- All sessions are logged to CloudWatch/S3
- IAM-based access control
- No inbound security group rules required

**Debugging commands:**
```bash
# Check agent status
systemctl status runs-fleet-agent

# View agent logs
journalctl -u runs-fleet-agent -f

# View runner configuration
cat /etc/runs-fleet/config.json

# Check disk space
df -h

# View running processes
ps aux | grep runner
```

## Security

**GitHub webhook validation:**
- HMAC-SHA256 signature verification
- Replay attack prevention (timestamp checking)

**EC2 instance security:**
- IMDSv2 required (no IMDSv1)
- Instance profile with least-privilege IAM policy
- Security group restricts inbound to SSH (optional) and egress to HTTPS
- Encrypted EBS volumes

**S3 cache security:**
- Pre-signed URLs with 15-minute expiration
- Bucket encryption at rest (AES-256)
- Lifecycle policy (delete after 30 days)
- Access restricted to runner instances via IAM

**Secrets management:**
- GitHub webhook secret in AWS Secrets Manager
- GitHub App private key in AWS Secrets Manager
- No secrets in container image or environment variables (reference ARNs only)

## Roadmap

**Phase 1: Cold-Start MVP** ✅ Complete
- [x] Architecture design
- [x] GitHub webhook receiver
- [x] Job label parser
- [x] SQS queue processor
- [x] EC2 fleet orchestrator (spot + on-demand)
- [x] Agent binary skeleton (registration/execution pending)
- [x] Basic Terraform infrastructure

**Phase 2: Warm Pools** ✅ Complete
- [x] Pool reconciliation loop
- [x] Hot pool management
- [x] Stopped pool management
- [x] Batch instance operations
- [x] Pool configuration in DynamoDB

**Phase 3: S3 Cache Layer** ✅ Complete
- [x] GitHub Actions cache protocol implementation
- [x] Pre-signed URL generation
- [x] Cache key/version management
- [x] Lifecycle policies

**Phase 4: Production Hardening** ✅ Complete
- [x] Comprehensive error handling
- [x] CloudWatch metrics + alarms
- [x] Spot interruption handling
- [x] Cost reporting
- [x] Integration tests

**Phase 5: Concurrent Processing** ✅ Complete
- [x] Graceful shutdown handling
- [x] Subnet rotation for private instances
- [x] Concurrent message processing

**Phase 6: Agent Implementation** ✅ Complete
- [x] Runner binary download with S3 caching
- [x] SSM-based runner registration with GitHub
- [x] Job execution with graceful SIGTERM shutdown
- [x] Safety monitoring (max runtime, disk space, memory)
- [x] Telemetry and self-termination

**Phase 7: Queue Processors** ✅ Complete
- [x] Termination handler for job completion tracking
- [x] Housekeeping tasks (orphaned cleanup, stale SSM, old job archival)
- [x] Pool auditing and metrics
- [x] Scheduled cleanup via EventBridge

**Phase 8: Operational Excellence** ✅ Complete
- [x] Circuit breaker for spot interruption protection
- [x] Forced on-demand retry after interruptions
- [x] Daily cost reporting with markdown output
- [x] Cache hit/miss metrics
- [x] Housekeeping metrics (orphaned instances, SSM cleanup, pool utilization)

**Known Limitations:**
- CloudWatch Dashboard and Alarms require Terraform infrastructure configuration
- SSH debugging access requires Terraform security group configuration
- EventBridge rule for daily cost reports requires Terraform configuration

## License

MIT

## Acknowledgments

Deeply inspired by [runs-on](https://github.com/runs-on/runs-on) by Cyril Rohr. This is an open-source alternative implementing the concepts revealed in their CloudFormation templates.
