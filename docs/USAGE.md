# Workflow Configuration

Replace GitHub-hosted runners with runs-fleet by changing the `runs-on` field:

```yaml
# Before (GitHub-hosted)
runs-on: ubuntu-latest

# After (runs-fleet)
runs-on: "runs-fleet=${{ github.run_id }}/cpu=2/arch=arm64"
```

## Label Reference

| Label | Description |
|-------|-------------|
| `runs-fleet=<run-id>` | Workflow run identifier (required) |
| `cpu=<n>` | vCPU count (default: 2). Defaults to 2x range (e.g., `cpu=4` → 4-8 vCPUs) |
| `cpu=<min>+<max>` | Explicit vCPU range for spot diversification |
| `ram=<n>` | Minimum RAM in GB |
| `ram=<min>+<max>` | RAM range in GB |
| `arch=<arch>` | Architecture: `arm64` or `amd64` (omit for both) |
| `family=<list>` | Instance families (e.g., `family=c7g+m7g`) |
| `disk=<size>` | Disk size in GiB (1-16384) |
| `pool=<name>` | Warm pool for fast start (~10s vs ~60s) |
| `spot=false` | Force on-demand (skip spot instances) |
| `public=true` | Request public IP (uses public subnet; default: private) |
| `backend=<ec2\|k8s>` | Override default compute backend |
| `region=<region>` | Target AWS region (multi-region deployments) |
| `env=<env>` | Environment isolation: `dev`, `staging`, or `prod` |

## Instance Selection

runs-fleet uses flexible specs to select instances dynamically via EC2 Fleet:

```yaml
# 4 vCPUs with bounded diversification (matches 4-8 vCPUs by default)
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64"

# Explicit CPU range for wider spot diversification
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4+16/arch=arm64"

# Exact CPU match (no diversification)
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4+4/arch=arm64"

# Specific families for workload requirements
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/family=c7g+m7g/arch=arm64"

# Architecture-agnostic: EC2 Fleet chooses ARM64 or AMD64 based on spot availability
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4"
```

### CPU Range Behavior

When you specify `cpu=N` without a max, runs-fleet defaults to a 2x range (`N` to `2*N` vCPUs) to enable bounded spot diversification:

| Label | Matches |
|-------|---------|
| `cpu=2` | 2-4 vCPUs |
| `cpu=4` | 4-8 vCPUs |
| `cpu=8` | 8-16 vCPUs |
| `cpu=4+4` | Exactly 4 vCPUs |
| `cpu=4+16` | 4-16 vCPUs |

This bounded diversification improves spot availability without risk of selecting excessively large instances. EC2 Fleet's `price-capacity-optimized` strategy selects from the available pool.

When `arch` is omitted, runs-fleet creates multiple EC2 Fleet launch template configurations (one per architecture) and lets EC2 choose based on `price-capacity-optimized` strategy. This maximizes spot availability across both ARM64 and AMD64 instance pools.

**Default families by architecture:**
- ARM64: c7g, m7g, t4g
- AMD64: c6i, c7i, m6i, m7i, t3
- No arch: all of the above

## Custom Disk Size

Disk size: `disk=<size>` in GiB (1-16384):

```yaml
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64/disk=200"
```

> **Cost note:** gp3 EBS storage costs ~$0.08/GB-month. Large disks add up quickly (e.g., 1TB = $80/month).

## Examples

### Basic CI workflow

```yaml
name: CI
on: [push, pull_request]

jobs:
  test:
    runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64"
    steps:
      - uses: actions/checkout@v4
      - run: make test
```

### Fast start with warm pool

```yaml
jobs:
  build:
    # Pool provides ~10s start time vs ~60s cold start
    runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64/pool=default"
```

### AMD64 for x86-specific builds

```yaml
jobs:
  build-amd64:
    runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=amd64"
```

### Force on-demand instance

```yaml
jobs:
  critical-deploy:
    # Skip spot for critical jobs that can't tolerate interruption
    runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64/spot=false"
```

### Request public IP

```yaml
jobs:
  external-access:
    # Request public IP when job needs direct internet access without NAT
    # Default behavior uses private subnets (no public IPv4 cost)
    runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=arm64/public=true"
```

### Matrix build

```yaml
jobs:
  build:
    strategy:
      matrix:
        arch: [arm64, amd64]
    runs-on: "runs-fleet=${{ github.run_id }}/cpu=4/arch=${{ matrix.arch }}"
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
