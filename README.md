# runs-fleet

[![CI](https://github.com/Shavakan/runs-fleet/actions/workflows/ci.yml/badge.svg)](https://github.com/Shavakan/runs-fleet/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://gist.githubusercontent.com/Shavakan/089a4c604db357e35ee33f10be5a1bbf/raw/runs-fleet-coverage.json)](https://github.com/Shavakan/runs-fleet)

> Self-hosted ephemeral GitHub Actions runners on AWS/K8s, orchestrating spot EC2 instances or Kubernetes pods for workflow jobs

Inspired by [runs-on](https://github.com/runs-on/runs-on). Fleet orchestration with warm pools, S3 caching, and spot-first cost optimization.

## Architecture

```
GitHub Webhook → API Gateway → SQS FIFO
                                   ↓
                Orchestrator (Go on Fargate)
                ├── Queue processors (main, pool, events)
                ├── Pool manager (warm instances)
                └── Provider (EC2 Fleet / Kubernetes)
                                   ↓
                EC2 Spot/On-demand Fleet  OR  Kubernetes Pods
                                   ↓
                Agent (bootstrap, execute, self-terminate)
```

**Flows:**
- **Cold-start** (~60s): Webhook → SQS → Provider creates runner → Agent registers → Job executes → Self-terminate
- **Warm pool** (~10s): Webhook with `pool=` label → Assign running instance → Job executes → Reconciliation replaces
- **Spot interruption**: EventBridge 2-min warning → Re-queue job with `ForceOnDemand=true`

**Tradeoffs:** Spot-first (70% savings) with on-demand fallback. Ephemeral instances avoid state accumulation. Warm pools trade idle cost for <10s latency.

## Quick Start

**Prerequisites:** Nix with flakes, direnv, AWS credentials, Terraform infrastructure deployed

```bash
git clone https://github.com/Shavakan/runs-fleet.git && cd runs-fleet
cp .envrc.example .envrc && direnv allow
```

**Build:**

```bash
# Nix (recommended)
nix build .#server        # Server binary
nix build .#agent-arm64   # ARM64 agent
nix build .#docker        # Docker image

# Make (alternative)
make build && make test && make lint
```

**Run locally:**

```bash
export AWS_REGION=ap-northeast-1
export RUNS_FLEET_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/...
go run cmd/server/main.go
```

## Project Structure

```
cmd/
  server/         # Fargate orchestrator (webhooks, queue processing)
  agent/          # EC2/K8s bootstrap binary (registers runner, executes job)
pkg/
  provider/       # Runner provisioning abstraction (EC2, Kubernetes)
  fleet/          # EC2 fleet creation (spot strategy, launch templates)
  pools/          # Warm pool reconciliation (hot/stopped/ephemeral instances)
  github/         # Webhook validation, JIT tokens, label parsing
  cache/          # S3-backed GitHub Actions cache protocol
  queue/          # SQS FIFO processing (batch, DLQ)
  db/             # DynamoDB state management
  housekeeping/   # Cleanup (orphaned instances, stale SSM, ephemeral pools)
  coordinator/    # Distributed leader election (DynamoDB)
  config/         # Env parsing, AWS clients
```

## Usage

Replace GitHub-hosted runners with runs-fleet:

```yaml
# Basic spec (defaults to 2 vCPUs if cpu omitted)
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64"

# Spot diversification with ranges
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4+16/family=c7g+m7g/arch=arm64"

# Warm pool (~10s start)
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/pool=default"
```

| Label | Description |
|-------|-------------|
| `cpu=<n>` | vCPU count (default: 2) |
| `cpu=<min>+<max>` | vCPU range for spot diversification |
| `arch=<arch>` | `arm64` or `amd64` (omit for both) |
| `pool=<name>` | Warm pool name (auto-creates ephemeral pool if missing) |
| `spot=false` | Force on-demand |

See [docs/USAGE.md](docs/USAGE.md) for full spec table and examples.

## Configuration

All configuration is via environment variables. See `.envrc.example` for a template.

### Core (Required)

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_GITHUB_APP_ID` | GitHub App ID |
| `RUNS_FLEET_GITHUB_APP_PRIVATE_KEY` | GitHub App private key (PEM format) |
| `RUNS_FLEET_GITHUB_WEBHOOK_SECRET` | Webhook HMAC secret |
| `AWS_REGION` | AWS region (default: `ap-northeast-1`) |
| `RUNS_FLEET_MODE` | Backend mode: `ec2` or `k8s` (default: `ec2`) |

### Queues (SQS - EC2 mode)

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_QUEUE_URL` | Main job queue URL (required for EC2) |
| `RUNS_FLEET_QUEUE_DLQ_URL` | Dead letter queue URL |
| `RUNS_FLEET_POOL_QUEUE_URL` | Warm pool batch queue URL |
| `RUNS_FLEET_EVENTS_QUEUE_URL` | EventBridge events queue (spot interruptions) |
| `RUNS_FLEET_TERMINATION_QUEUE_URL` | Instance termination notifications |
| `RUNS_FLEET_HOUSEKEEPING_QUEUE_URL` | Cleanup task scheduling |

### DynamoDB

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_JOBS_TABLE` | | Job state tracking |
| `RUNS_FLEET_POOLS_TABLE` | | Pool configurations |
| `RUNS_FLEET_LOCKS_TABLE` | | Distributed leader election |
| `RUNS_FLEET_CIRCUIT_BREAKER_TABLE` | `runs-fleet-circuit-state` | Circuit breaker state |

### S3 & SNS

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_CACHE_BUCKET` | GitHub Actions cache artifacts |
| `RUNS_FLEET_COST_REPORT_BUCKET` | Cost report storage |
| `RUNS_FLEET_COST_REPORT_SNS_TOPIC` | Cost report notifications |

### EC2 Backend (required when `RUNS_FLEET_MODE=ec2`)

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_VPC_ID` | | VPC ID (required) |
| `RUNS_FLEET_PUBLIC_SUBNET_IDS` | | Comma-separated subnet IDs (required) |
| `RUNS_FLEET_PRIVATE_SUBNET_IDS` | | Private subnets (optional) |
| `RUNS_FLEET_SECURITY_GROUP_ID` | | Security group ID (required) |
| `RUNS_FLEET_INSTANCE_PROFILE_ARN` | | IAM instance profile ARN (required) |
| `RUNS_FLEET_RUNNER_IMAGE` | | ECR image URL for runners (required) |
| `RUNS_FLEET_KEY_NAME` | | EC2 key pair name (optional) |
| `RUNS_FLEET_SPOT_ENABLED` | `true` | Enable spot instances |
| `RUNS_FLEET_MAX_RUNTIME_MINUTES` | `360` | Max job runtime (1-1440) |
| `RUNS_FLEET_LAUNCH_TEMPLATE_NAME` | `runs-fleet-runner` | EC2 launch template |
| `RUNS_FLEET_TAGS` | | Custom EC2 tags (JSON object) |
| `RUNS_FLEET_LOG_LEVEL` | `info` | Log verbosity |

### Kubernetes Backend (required when `RUNS_FLEET_MODE=k8s`)

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_KUBE_NAMESPACE` | | Runner namespace (required) |
| `RUNS_FLEET_KUBE_RUNNER_IMAGE` | | Runner container image (required) |
| `RUNS_FLEET_KUBE_CONFIG` | | Kubeconfig path (empty = in-cluster) |
| `RUNS_FLEET_KUBE_SERVICE_ACCOUNT` | `runs-fleet-runner` | Runner ServiceAccount |
| `RUNS_FLEET_KUBE_NODE_SELECTOR` | | Node selector (`key=value,...`) |
| `RUNS_FLEET_KUBE_TOLERATIONS` | | Tolerations (JSON array) |
| `RUNS_FLEET_KUBE_IDLE_TIMEOUT_MINUTES` | `10` | Pod idle timeout |
| `RUNS_FLEET_KUBE_RELEASE_NAME` | `runs-fleet` | Helm release name |
| `RUNS_FLEET_KUBE_STORAGE_CLASS` | | PVC storage class |

### Kubernetes DinD (Docker-in-Docker)

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_KUBE_DIND_IMAGE` | `docker:dind` | DinD sidecar image |
| `RUNS_FLEET_KUBE_DAEMON_JSON_CONFIGMAP` | | ConfigMap for daemon.json |
| `RUNS_FLEET_KUBE_DOCKER_WAIT_SECONDS` | `120` | Docker daemon startup wait (10-300) |
| `RUNS_FLEET_KUBE_DOCKER_GROUP_GID` | `123` | Docker socket GID (1-65535) |
| `RUNS_FLEET_KUBE_REGISTRY_MIRROR` | | Registry mirror URL |

### Valkey Queue (K8s mode)

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_VALKEY_ADDR` | `valkey:6379` | Valkey/Redis address (required for K8s) |
| `RUNS_FLEET_VALKEY_PASSWORD` | | Valkey password |
| `RUNS_FLEET_VALKEY_DB` | `0` | Database number |

### Distributed Coordinator

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_COORDINATOR_ENABLED` | `false` | Enable leader election |
| `RUNS_FLEET_INSTANCE_ID` | | Unique instance ID (required if coordinator enabled) |

### Cache

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_CACHE_SECRET` | HMAC secret for cache auth |
| `RUNS_FLEET_CACHE_URL` | Cache service URL (passed to runners) |

### Metrics

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_METRICS_NAMESPACE` | `RunsFleet` | Metric namespace/prefix |
| `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` | `true` | Enable CloudWatch metrics |
| `RUNS_FLEET_METRICS_PROMETHEUS_ENABLED` | `false` | Enable Prometheus `/metrics` endpoint |
| `RUNS_FLEET_METRICS_PROMETHEUS_PATH` | `/metrics` | Prometheus endpoint path |
| `RUNS_FLEET_METRICS_DATADOG_ENABLED` | `false` | Enable Datadog DogStatsD |
| `RUNS_FLEET_METRICS_DATADOG_ADDR` | `127.0.0.1:8125` | DogStatsD address |
| `RUNS_FLEET_METRICS_DATADOG_TAGS` | | Global tags (comma-separated) |

## Admin UI

Web-based pool configuration management at `/admin/`:

- View all pools with key metrics
- Create, edit, and delete pools
- Delete restricted to ephemeral pools only

**API Endpoints:**
```
GET    /api/pools          # List all pools
GET    /api/pools/{name}   # Get pool config
POST   /api/pools          # Create pool
PUT    /api/pools/{name}   # Update pool
DELETE /api/pools/{name}   # Delete pool (ephemeral only)
```

**Build:**
```bash
make build-admin-ui   # Build Next.js static export
make build-server     # Build server (includes UI)
```

The UI is embedded in the Go binary as static files.

## Development

```bash
make test           # Run tests (or: nix flake check)
make lint           # golangci-lint
make docker-build   # Build Docker image
make docker-push    # Push to ECR
```

**Deploy:** Infrastructure in `shavakan-terraform/terraform/runs-fleet/`. Push image to ECR, then `terraform apply`.

## Cost Model

Target: ~$55-65/month for 100 jobs/day @ 10 min avg runtime

| Component | Cost |
|-----------|------|
| EC2 spot | ~$15-20/month |
| Fargate orchestrator | $36/month (1 vCPU, 2GB) |
| S3 cache + services | $3-8/month |

Compare to GitHub hosted: $80/month.

## Security

- Webhook HMAC-SHA256 validation
- IMDSv2 required, least-privilege IAM
- S3 pre-signed URLs (15-min), encrypted EBS
- Secrets in AWS Secrets Manager

## License

MIT — Inspired by [runs-on](https://github.com/runs-on/runs-on) by Cyril Rohr.
