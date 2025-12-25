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
# Fixed spec
runs-on: "runs-fleet=${{ github.run_id }}/runner=4cpu-linux-arm64"

# Flexible spec (spot diversification)
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64"

# Warm pool (~10s start)
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/pool=default"
```

| Label | Description |
|-------|-------------|
| `runner=<spec>` | Fixed spec: `2cpu-linux-arm64`, `4cpu-linux-amd64`, etc. |
| `cpu=<n>` | Flexible: minimum vCPUs |
| `arch=<arch>` | `arm64` or `amd64` (omit for both) |
| `pool=<name>` | Warm pool name (auto-creates ephemeral pool if missing) |
| `spot=false` | Force on-demand |

See [docs/USAGE.md](docs/USAGE.md) for full spec table, flexible labels, and examples.

## Configuration

Required env vars (see `.envrc.example`):

```bash
# GitHub App
RUNS_FLEET_GITHUB_APP_ID, RUNS_FLEET_GITHUB_APP_PRIVATE_KEY, RUNS_FLEET_GITHUB_WEBHOOK_SECRET

# AWS (from Terraform)
AWS_REGION, RUNS_FLEET_QUEUE_URL, RUNS_FLEET_POOL_QUEUE_URL
RUNS_FLEET_JOBS_TABLE, RUNS_FLEET_POOLS_TABLE, RUNS_FLEET_CACHE_BUCKET
RUNS_FLEET_VPC_ID, RUNS_FLEET_PUBLIC_SUBNET_IDS, RUNS_FLEET_INSTANCE_PROFILE_ARN
```

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
