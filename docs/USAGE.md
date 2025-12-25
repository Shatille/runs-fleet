# Workflow Configuration

Replace GitHub-hosted runners with runs-fleet by changing the `runs-on` field:

```yaml
# Before (GitHub-hosted)
runs-on: ubuntu-latest

# After (runs-fleet)
runs-on: "runs-fleet=${{ github.run_id }}/runner=2cpu-linux-arm64"
```

## Runner Specs

| Spec | Instance | vCPU | RAM | Disk |
|------|----------|------|-----|------|
| `2cpu-linux-arm64` | t4g.medium | 2 | 4GB | 30GB |
| `4cpu-linux-arm64` | c7g.xlarge | 4 | 8GB | 50GB |
| `8cpu-linux-arm64` | c7g.2xlarge | 8 | 16GB | 100GB |
| `16cpu-linux-arm64` | c7g.4xlarge | 16 | 32GB | 200GB |
| `32cpu-linux-arm64` | c7g.8xlarge | 32 | 64GB | 400GB |
| `2cpu-linux-amd64` | t3.medium | 2 | 4GB | 30GB |
| `4cpu-linux-amd64` | c6i.xlarge | 4 | 8GB | 50GB |
| `8cpu-linux-amd64` | c6i.2xlarge | 8 | 16GB | 100GB |
| `16cpu-linux-amd64` | c6i.4xlarge | 16 | 32GB | 200GB |
| `32cpu-linux-amd64` | c6i.8xlarge | 32 | 64GB | 400GB |

Custom disk size: `disk=<size>` in GiB (1-16384), e.g., `runner=4cpu-linux-arm64/disk=200`

> **Cost note:** gp3 EBS storage costs ~$0.08/GB-month. Large disks add up quickly (e.g., 1TB = $80/month).

## Flexible Specs

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
| `disk=<size>` | Disk size in GiB (1-16384) |

When `arch` is omitted, runs-fleet creates multiple EC2 Fleet launch template configurations (one per architecture) and lets EC2 choose based on `price-capacity-optimized` strategy. This maximizes spot availability across both ARM64 and AMD64 instance pools.

**Default families by architecture:**
- ARM64: c7g, m7g, t4g
- AMD64: c6i, c7i, m6i, m7i, t3
- No arch: all of the above

## Label Reference

| Label | Description |
|-------|-------------|
| `runs-fleet=<run-id>` | Workflow run identifier (required) |
| `runner=<spec>` | `<cpu>cpu-<os>-<arch>[/<modifier>]` |
| `pool=<name>` | Warm pool for fast start (~10s vs ~60s) |
| `private=true` | Private subnet with static egress IP |
| `spot=false` | Force on-demand (skip spot instances) |

## Examples

### Basic CI workflow

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

### Fast start with warm pool

```yaml
jobs:
  build:
    # Pool provides ~10s start time vs ~60s cold start
    runs-on: "runs-fleet=${{ github.run_id }}/runner=4cpu-linux-arm64/pool=default"
```

### AMD64 for x86-specific builds

```yaml
jobs:
  build-amd64:
    runs-on: "runs-fleet=${{ github.run_id }}/runner=4cpu-linux-amd64"
```

### Private networking (static egress IP)

```yaml
jobs:
  deploy:
    # Use private subnet when calling APIs that whitelist IPs
    runs-on: "runs-fleet=${{ github.run_id }}/runner=2cpu-linux-arm64/private=true"
```

### Force on-demand instance

```yaml
jobs:
  critical-deploy:
    # Skip spot for critical jobs that can't tolerate interruption
    runs-on: "runs-fleet=${{ github.run_id }}/runner=4cpu-linux-arm64/spot=false"
```

### Matrix build

```yaml
jobs:
  build:
    strategy:
      matrix:
        arch: [arm64, amd64]
    runs-on: "runs-fleet=${{ github.run_id }}/runner=4cpu-linux-${{ matrix.arch }}"
```

### Ephemeral pools (auto-created)

Ephemeral pools are automatically created when a job references a pool that doesn't exist. They inherit instance specs from the first job and auto-scale based on demand:

```yaml
jobs:
  build:
    # Pool "my-project" created automatically if it doesn't exist
    # Inherits cpu/ram/arch from this job's labels
    runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64/pool=my-project"
```

Ephemeral pools:
- Auto-scale `DesiredRunning` based on peak concurrent jobs (1-hour window)
- Deleted after 4 hours of inactivity
- Don't override explicitly configured pools in DynamoDB
