# runs-fleet

[![CI](https://github.com/Shatille/runs-fleet/actions/workflows/ci.yml/badge.svg)](https://github.com/Shatille/runs-fleet/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://gist.githubusercontent.com/Shavakan/089a4c604db357e35ee33f10be5a1bbf/raw/runs-fleet-coverage.json)](https://github.com/Shatille/runs-fleet)

> Self-hosted ephemeral GitHub Actions runners on AWS

Spot-first EC2 instances for workflow jobs. Warm pools for fast start times.

## Usage

Replace GitHub-hosted runners:

```yaml
# Basic (ARM64, 4 vCPUs, spot instance)
runs-on: "runs-fleet/cpu=4/arch=arm64"

# Fast start with warm pool (~10s vs ~60s cold)
runs-on: "runs-fleet/cpu=4/arch=arm64/pool=default"

# Spot diversification across instance sizes
runs-on: "runs-fleet/cpu=4+16/family=c7g+m7g/arch=arm64"

# Force on-demand for critical jobs
runs-on: "runs-fleet/cpu=4/arch=arm64/spot=false"

# Bare marker (all defaults: ~2-4 vCPUs, spot)
runs-on: "runs-fleet"
```

| Label | Description |
|-------|-------------|
| `runs-fleet` | Marker (required). `runs-fleet=<run-id>/...` legacy form still works |
| `cpu=<n>` or `cpu=<min>+<max>` | vCPU count or range (default: 2) |
| `arch=<arch>` | `arm64` or `amd64` |
| `pool=<name>` | Warm pool for fast start |
| `spot=false` | Force on-demand |

The `runs-fleet` marker is all that is required; run_id is read from the webhook
payload. The legacy `runs-fleet=${{ github.run_id }}/...` form remains fully
supported.

**Migrating from existing runners?** runs-fleet can also serve jobs that target
your *current* custom labels (e.g. labels inherited from another self-hosted
runner system such as ARC) **without editing any workflow**. Map those labels to
specs in the `RUNS_FLEET_LABEL_ALIASES` config and runs-fleet will claim and
register for them. See [docs/CONFIGURATION.md](docs/CONFIGURATION.md).

See [docs/USAGE.md](docs/USAGE.md) for full label reference and examples. See [docs/CONFIGURATION.md](docs/CONFIGURATION.md) for environment variables.

## Architecture

```
GitHub Webhook → API Gateway → SQS FIFO → Orchestrator (Fargate)
                                              ↓
                                   EC2 Spot Fleet
                                              ↓
                              Agent (register, execute, terminate)
```

- **Cold-start** (~60s): Webhook → create instance → register runner → execute → terminate
- **Warm pool** (~10s): Webhook → assign running instance → execute → replace

## Forking

Running runs-fleet on your own AWS account requires three configuration surfaces. None of them live in this repo's source — they live in your fork's GitHub Settings, your AWS account, and your deployment manifests.

### 1. GitHub Actions secrets and variables

In `Settings → Secrets and variables → Actions` on the fork:

**Secrets:**

| Name | Used by | Required | Notes |
|---|---|---|---|
| `AWS_IAM_ROLE_ARN` | every workflow | yes | IAM role assumed via GitHub OIDC. See `deploy/terraform/iam.tf` for a minimal policy. |
| `PACKER_VPC_ID` | `build-amis.yml` | yes | VPC where Packer launches its temporary builder instances. |
| `ECR_REPOSITORY_RUNNER` | `build-runner.yml`, `build-amis.yml` | yes | ECR repo path for the runner image, e.g. `runs-fleet/runner`. |
| `PROVISION_BASE_HOOK` | `build-amis.yml` (base job) | no | Optional shell script materialized into the AMI just before cleanup — for account-specific tooling that can't live in upstream. |
| `GIST_TOKEN` | `ci.yml` | no | PAT for publishing coverage gist. Drop the badge from README if unused. |

**Variables:**

| Name | Used by | Notes |
|---|---|---|
| `AMI_EXTRA_TAGS` | `build-amis.yml` | JSON object merged onto AMI tags — e.g. `{"Environment":"prod","Team":"infra"}`. |
| `RUNNER_BASE_TAG` | `build-runner.yml` | Override the GitHub Actions runner version baked into the image. |

### 2. AWS infrastructure

The orchestrator and Packer workflows expect these resources to already exist in the target AWS account:

- ECR repositories matching `secrets.ECR_REPOSITORY_RUNNER` and the orchestrator image path.
- SQS FIFO + standard queues: main (FIFO), pool, events, termination, housekeeping — each work queue paired with a dead-letter queue (main-dlq is FIFO; pool-dlq and housekeeping-dlq are standard). See `deploy/terraform/queues.tf`.
- DynamoDB tables: `runs-fleet-jobs`, `runs-fleet-pools`, `runs-fleet-circuit-state`.
- S3 bucket for the GitHub Actions cache.
- Security group named `runs-fleet-runner` (discovered by Packer via filter) and a matching instance profile.
- Launch templates `runs-fleet-runner-amd64` / `runs-fleet-runner-arm64` (the `build-amis.yml` workflow writes new versions to these; create them once with any placeholder AMI).
- Three IAM roles — see `deploy/terraform/iam.tf` for the minimum policies.

### 3. Helm deployment values

The orchestrator runs from `deploy/helm/runs-fleet/`. Set at minimum:

```yaml
mode: ec2
aws:
  vpcId: vpc-...
  subnetIds: [subnet-..., subnet-...]
  securityGroupId: sg-...
  instanceProfileArn: arn:aws:iam::<account>:instance-profile/runs-fleet-runner
  runnerImage: <account>.dkr.ecr.<region>.amazonaws.com/runs-fleet/runner:latest
  queueUrl: https://sqs.<region>.amazonaws.com/<account>/runs-fleet-main.fifo
  # ...remaining queue URLs and table names
github:
  appId: "<id>"
  existingSecret: runs-fleet-secrets   # or set privateKey/webhookSecret directly
```

See `deploy/helm/runs-fleet/values.yaml` for the full schema. The chart's pre-render checks fail loudly when required keys are missing — that's the contract.

### 4. Bootstrap

Self-hosted runner AMIs are built by self-hosted runners — a chicken/egg. The first AMI build must use GitHub-hosted runners:

```
Actions → Build AMIs → Run workflow → use_fallback_runners: true
```

After the first successful AMI lands in the launch template, subsequent builds run on self-hosted runners automatically.

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

# Scan the runner image for CVEs (matches CI Trivy gate)
make scan-runner
```

Changes to the runner image (`docker/runner/`) follow conventions documented in `docker/runner/CLAUDE.md` — base-image policy, package-install preferences, and OpenVEX usage for upstream-blocked CVEs.

## License

MIT
