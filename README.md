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
- **Cold-start** (~60s): Webhook → SQS → Provider creates runner → Agent registers with GitHub → Job executes → Self-terminate
- **Warm pool** (~10s): Webhook with `pool=` label → Assign running instance OR start stopped → Job executes → Reconciliation replaces
- **Spot interruption**: EventBridge 2-min warning → Re-queue job with `ForceOnDemand=true`

**Tradeoffs:**
- Spot-first (70% savings) with on-demand fallback and circuit breaker
- Ephemeral instances avoid state accumulation
- Warm pools trade idle cost for <10s latency

## Quick Start

**Prerequisites:** Nix with flakes, direnv (optional), AWS credentials, Terraform infrastructure deployed

```bash
git clone https://github.com/Shavakan/runs-fleet.git && cd runs-fleet

# With direnv
cp .envrc.example .envrc && direnv allow

# Without direnv
nix develop
```

**Build:**

```bash
# Nix (recommended)
nix build .#server        # Server binary
nix build .#agent-amd64   # AMD64 agent
nix build .#agent-arm64   # ARM64 agent
nix build .#docker        # Docker image

# Make (alternative)
make build    # All binaries
make test     # Tests with coverage
make lint     # golangci-lint
```

**Run locally:**

```bash
export AWS_REGION=ap-northeast-1
export RUNS_FLEET_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/...
export RUNS_FLEET_GITHUB_WEBHOOK_SECRET=...
go run cmd/server/main.go
```

## Project Structure

```
cmd/
  server/         # Fargate orchestrator (webhooks, queue processing, fleet mgmt)
  agent/          # EC2/K8s bootstrap binary (registers runner, executes job)
pkg/
  provider/       # Runner provisioning abstraction (EC2, Kubernetes)
  fleet/          # EC2 fleet creation (spot strategy, launch templates)
  pools/          # Warm pool reconciliation (hot/stopped instances)
  github/         # Webhook validation, JIT tokens, label parsing
  runner/         # Runner lifecycle management
  cache/          # S3-backed GitHub Actions cache protocol
  queue/          # SQS FIFO processing (batch, DLQ)
  db/             # DynamoDB state management
  events/         # EventBridge (spot interruptions)
  termination/    # Instance shutdown notifications
  housekeeping/   # Cleanup (orphaned instances, stale SSM)
  circuit/        # Circuit breaker for spot failures
  coordinator/    # Distributed leader election (DynamoDB)
  cost/           # Pricing calculations
  metrics/        # CloudWatch metrics
  tracing/        # OpenTelemetry tracing
  gitops/         # GitOps configuration management
  config/         # Env parsing, AWS clients
  agent/          # Agent-side logic (bootstrap, self-terminate)
```

## Workflow Configuration

Replace GitHub-hosted runners with runs-fleet by changing the `runs-on` field:

```yaml
# Before (GitHub-hosted)
runs-on: ubuntu-latest

# After (runs-fleet)
runs-on: "runs-fleet=${{ github.run_id }}/runner=2cpu-linux-arm64"
```

### Runner Specs

| Spec | Instance | vCPU | RAM | Disk |
|------|----------|------|-----|------|
| `2cpu-linux-arm64` | t4g.medium | 2 | 4GB | 30GB |
| `4cpu-linux-arm64` | c7g.xlarge | 4 | 8GB | 50GB |
| `8cpu-linux-arm64` | c7g.2xlarge | 8 | 16GB | 100GB |
| `2cpu-linux-amd64` | t3.medium | 2 | 4GB | 30GB |
| `4cpu-linux-amd64` | c6i.xlarge | 4 | 8GB | 50GB |
| `8cpu-linux-amd64` | c6i.2xlarge | 8 | 16GB | 100GB |

Add `/large-disk` modifier for 200GB disk: `runner=4cpu-linux-arm64/large-disk`

### Flexible Specs

For advanced instance selection with spot diversification, use flexible specs instead of fixed runner specs:

```yaml
# Flexible: EC2 Fleet chooses best instance from multiple families
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64"

# Architecture-agnostic: EC2 Fleet chooses between ARM64 and AMD64 based on spot availability
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4"
```

| Label | Description |
|-------|-------------|
| `cpu=<n>` | Minimum vCPUs required |
| `cpu=<min>-<max>` | vCPU range (e.g., `cpu=4-8`) |
| `ram=<n>` | Minimum RAM in GB |
| `arch=<arch>` | Architecture: `arm64` or `amd64` (omit for both) |
| `family=<list>` | Instance families (e.g., `family=c7g+m7g`) |

When `arch` is omitted, runs-fleet creates multiple EC2 Fleet launch template configurations (one per architecture) and lets EC2 choose based on `price-capacity-optimized` strategy. This maximizes spot availability across both ARM64 and AMD64 instance pools.

**Default families by architecture:**
- ARM64: c7g, m7g, t4g
- AMD64: c6i, c7i, m6i, m7i, t3
- No arch: all of the above

### Label Reference

| Label | Description |
|-------|-------------|
| `runs-fleet=<run-id>` | Workflow run identifier (required) |
| `runner=<spec>` | `<cpu>cpu-<os>-<arch>[/<modifier>]` |
| `pool=<name>` | Warm pool for fast start (~10s vs ~60s) |
| `private=true` | Private subnet with static egress IP |
| `spot=false` | Force on-demand (skip spot instances) |

### Examples

**Basic CI workflow:**

```yaml
name: CI
on: [push, pull_request]

jobs:
  test:
    runs-on: "runs-fleet=${{ github.run_id }}/runner=4cpu-linux-arm64"
    steps:
      - uses: actions/checkout@v4
      - run: make test
```

**Fast start with warm pool:**

```yaml
jobs:
  build:
    # Pool provides ~10s start time vs ~60s cold start
    runs-on: "runs-fleet=${{ github.run_id }}/runner=4cpu-linux-arm64/pool=default"
```

**AMD64 for x86-specific builds:**

```yaml
jobs:
  build-amd64:
    runs-on: "runs-fleet=${{ github.run_id }}/runner=4cpu-linux-amd64"
```

**Private networking (static egress IP):**

```yaml
jobs:
  deploy:
    # Use private subnet when calling APIs that whitelist IPs
    runs-on: "runs-fleet=${{ github.run_id }}/runner=2cpu-linux-arm64/private=true"
```

**Force on-demand instance:**

```yaml
jobs:
  critical-deploy:
    # Skip spot for critical jobs that can't tolerate interruption
    runs-on: "runs-fleet=${{ github.run_id }}/runner=4cpu-linux-arm64/spot=false"
```

**Matrix build:**

```yaml
jobs:
  build:
    strategy:
      matrix:
        arch: [arm64, amd64]
    runs-on: "runs-fleet=${{ github.run_id }}/runner=4cpu-linux-${{ matrix.arch }}"
```

## Configuration

Required env vars (see `.envrc.example` for complete list):

```bash
# GitHub App
RUNS_FLEET_GITHUB_APP_ID, RUNS_FLEET_GITHUB_APP_PRIVATE_KEY, RUNS_FLEET_GITHUB_WEBHOOK_SECRET

# AWS (from Terraform)
AWS_REGION, RUNS_FLEET_QUEUE_URL, RUNS_FLEET_POOL_QUEUE_URL, RUNS_FLEET_EVENTS_QUEUE_URL
RUNS_FLEET_LOCKS_TABLE, RUNS_FLEET_JOBS_TABLE, RUNS_FLEET_CACHE_BUCKET, RUNS_FLEET_CONFIG_BUCKET
RUNS_FLEET_VPC_ID, RUNS_FLEET_PUBLIC_SUBNET_IDS, RUNS_FLEET_SECURITY_GROUP_ID, RUNS_FLEET_INSTANCE_PROFILE_ARN

# Behavior
RUNS_FLEET_SPOT_ENABLED=true, RUNS_FLEET_MAX_RUNTIME_MINUTES=360
```

## Development

```bash
make test           # Run tests (or: nix flake check)
make lint           # Lint
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
| S3 cache | $1-5/month |
| Supporting services | $2-3/month |

Compare to GitHub hosted: $80/month. Cost reporting in `pkg/cost/` covers t4g, c7g, m7g families with fixed 70% spot discount assumption.

## Security

- Webhook HMAC-SHA256 validation
- IMDSv2 required, least-privilege IAM
- S3 pre-signed URLs (15-min), encrypted EBS
- Secrets in AWS Secrets Manager

## License

MIT — Inspired by [runs-on](https://github.com/runs-on/runs-on) by Cyril Rohr.
