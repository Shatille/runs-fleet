# Workflow Configuration

Replace GitHub-hosted runners with runs-fleet by changing the `runs-on` field:

```yaml
# Before (GitHub-hosted)
runs-on: ubuntu-latest

# After (runs-fleet)
runs-on: "runs-fleet/cpu=2/arch=arm64"
```

The bare `runs-fleet` marker is the only required token; run_id is read from the
webhook payload. The legacy `runs-fleet=${{ github.run_id }}/...` form remains
fully supported (its run_id segment is now optional and ignored — the webhook is
authoritative).

## Label Reference

| Label | Description |
|-------|-------------|
| `runs-fleet` | Runner marker (required). Legacy `runs-fleet=<run-id>/...` still works |
| `cpu=<n>` | vCPU count (default: 2). Defaults to a 2x range (e.g., `cpu=4` → 4-8 vCPUs) |
| `cpu=<min>+<max>` | Explicit vCPU range for spot diversification |
| `ram=<n>` | Minimum RAM in GB (no auto-range, unlike `cpu=`) |
| `ram=<min>+<max>` | Explicit RAM range in GB |
| `arch=<arch>` | Architecture: `arm64` or `amd64` (omit to let runs-fleet pick) |
| `family=<list>` | Instance families (e.g., `family=c7g+m7g`) |
| `gen=<n>` | Instance generation (1-10). E.g., `gen=8` for Graviton4. Default: any generation |
| `disk=<size>` | Disk size in GiB (1-16384) |
| `pool=<name>` | Warm pool name (must be ≤63 chars; alphanumeric, `-`, `_`) |
| `spot=false` | Force on-demand for cold-start (warm pool is always on-demand) |

> **Asymmetry note:** `cpu=N` automatically expands to a `N..2N` range to enable spot diversification. `ram=N` does **not** — it's interpreted as a minimum only, with no upper bound. If you want a bounded RAM range, use `ram=min+max` explicitly.

## Instance Selection

runs-fleet uses flexible specs to select instances dynamically via EC2 Fleet:

```yaml
# 4 vCPUs with bounded diversification (matches 4-8 vCPUs by default)
runs-on: "runs-fleet/cpu=4/arch=arm64"

# Explicit CPU range for wider spot diversification
runs-on: "runs-fleet/cpu=4+16/arch=arm64"

# Pin exact CPU count (still diversifies across families)
runs-on: "runs-fleet/cpu=4+4/arch=arm64"

# Specific families for workload requirements
runs-on: "runs-fleet/cpu=4/family=c7g+m7g/arch=arm64"

# Pin instance generation (e.g., Graviton4)
runs-on: "runs-fleet/cpu=4/arch=arm64/gen=8"

# Architecture-agnostic: runs-fleet picks the cheaper-on-spot arch at launch
runs-on: "runs-fleet/cpu=4"
```

### CPU Range Behavior

When you specify `cpu=N` without a max, runs-fleet defaults to a 2x range (`N` to `2*N` vCPUs) to enable bounded spot diversification:

| Label | Matches |
|-------|---------|
| `cpu=2` | 2-4 vCPUs |
| `cpu=4` | 4-8 vCPUs |
| `cpu=8` | 8-16 vCPUs |
| `cpu=4+4` | Exactly 4 vCPUs (but still picks across compatible families) |
| `cpu=4+16` | 4-16 vCPUs |

EC2 Fleet's `price-capacity-optimized` strategy then selects from the matching pool.

### Architecture omission

When `arch` is omitted, runs-fleet does **not** submit both architectures to EC2 Fleet simultaneously. Instead, it queries average spot prices for each candidate arch and picks the cheaper one up front; only that arch's launch template is submitted to Fleet. If the spot price fetch fails, runs-fleet defaults to `arm64`.

If you want hard control over architecture, set `arch=arm64` or `arch=amd64` explicitly.

**Default families per arch:**
- ARM64: `c8g, m8g, r8g, c7g, m7g, t4g`
- AMD64: `c6i, c7i, m6i, m7i, t3`
- No arch: all of the above

## Custom Disk Size

Disk size: `disk=<size>` in GiB (1-16384):

```yaml
runs-on: "runs-fleet/cpu=4/arch=arm64/disk=200"
```

> **Cost note:** gp3 EBS storage costs ~$0.08/GB-month. Large disks add up quickly (e.g., 1TB = $80/month).

## Examples

### Basic CI workflow

```yaml
name: CI
on: [push, pull_request]

jobs:
  test:
    runs-on: "runs-fleet/cpu=4/arch=arm64"
    steps:
      - uses: actions/checkout@v5
      - run: make test
```

### Fast start with warm pool

```yaml
jobs:
  build:
    # Pool provides ~10s start time vs ~60s cold start
    runs-on: "runs-fleet/cpu=4/arch=arm64/pool=default"
```

### AMD64 for x86-specific builds

```yaml
jobs:
  build-amd64:
    runs-on: "runs-fleet/cpu=4/arch=amd64"
```

### Force on-demand instance (cold-start)

```yaml
jobs:
  critical-deploy:
    # Skip spot for cold-start jobs that can't tolerate interruption
    # (warm pool jobs are always on-demand)
    runs-on: "runs-fleet/cpu=4/arch=arm64/spot=false"
```

> **Note:** `spot=false` is case-sensitive. `spot=False` or `spot=0` will **not** disable spot — only the literal lowercase `false` does.

### Matrix build

```yaml
jobs:
  build:
    strategy:
      matrix:
        arch: [arm64, amd64]
    runs-on: "runs-fleet/cpu=4/arch=${{ matrix.arch }}"
```

### Ephemeral pools (auto-created)

Ephemeral pools are automatically created when a job references a pool that doesn't exist. They inherit instance specs from the first job that references them:

```yaml
jobs:
  build:
    # Pool "my-project" created automatically if it doesn't exist
    # Inherits cpu/ram/arch from this job's labels
    runs-on: "runs-fleet/cpu=4/arch=arm64/pool=my-project"
```

Ephemeral pool behavior:
- Auto-scale `DesiredStopped` based on **p90** concurrent jobs over a 1-hour rolling window. Stopped instances start quickly when claimed, but the pool keeps `DesiredRunning=0` for ephemeral pools — so a sudden one-off concurrency spike beyond p90 still pays the cold-start cost.
- Deleted after 4 hours of inactivity
- Don't override explicitly configured pools in DynamoDB
