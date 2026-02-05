# runs-fleet

[![CI](https://github.com/Shatille/runs-fleet/actions/workflows/ci.yml/badge.svg)](https://github.com/Shatille/runs-fleet/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://gist.githubusercontent.com/Shavakan/089a4c604db357e35ee33f10be5a1bbf/raw/runs-fleet-coverage.json)](https://github.com/Shatille/runs-fleet)

> Self-hosted ephemeral GitHub Actions runners on AWS/K8s

Spot-first EC2 instances or Kubernetes pods for workflow jobs. Warm pools for fast start times.

## Usage

Replace GitHub-hosted runners:

```yaml
# Basic (ARM64, 4 vCPUs, spot instance)
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64"

# Fast start with warm pool (~10s vs ~60s cold)
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64/pool=default"

# Spot diversification across instance sizes
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4+16/family=c7g+m7g/arch=arm64"

# Force on-demand for critical jobs
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64/spot=false"
```

| Label | Description |
|-------|-------------|
| `runs-fleet=<run-id>` | Workflow run identifier (required) |
| `cpu=<n>` or `cpu=<min>+<max>` | vCPU count or range (default: 2) |
| `arch=<arch>` | `arm64` or `amd64` |
| `pool=<name>` | Warm pool for fast start |
| `spot=false` | Force on-demand |

See [docs/USAGE.md](docs/USAGE.md) for full label reference and examples. See [docs/CONFIGURATION.md](docs/CONFIGURATION.md) for environment variables.

## Architecture

```
GitHub Webhook → API Gateway → SQS FIFO → Orchestrator (Fargate)
                                              ↓
                              EC2 Spot Fleet  OR  Kubernetes Pods
                                              ↓
                              Agent (register, execute, terminate)
```

- **Cold-start** (~60s): Webhook → create instance → register runner → execute → terminate
- **Warm pool** (~10s): Webhook → assign running instance → execute → replace

## Development

```bash
# Setup
git clone https://github.com/Shatille/runs-fleet.git && cd runs-fleet
cp .envrc.example .envrc && direnv allow

# Build
nix build .#server         # or: make build
nix build .#agent-arm64

# Test
make test && make lint
```

## License

MIT
