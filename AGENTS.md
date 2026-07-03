# runs-fleet

Self-hosted ephemeral GitHub Actions runners on AWS. Orchestrates spot EC2 instances for workflow jobs.

## Wiki

A compiled codebase wiki lives at `wiki/`. Read `wiki/CONTEXT.md` for navigation, then `wiki/INDEX.md` to find the right topic. Prefer wiki articles over scanning raw files when the section's `[coverage: high]` tag is set; fall back to source for `[coverage: low]` sections or code-level questions. Cross-cutting patterns (locking, backend asymmetry, retry semantics) live in `wiki/concepts/`.

## Runner image

The runner container at `docker/runner/` has specific patterns for keeping the Trivy CVE footprint low. **Before adding packages, building from source, patching CVEs, or touching anything in that directory, read `docker/runner/CLAUDE.md` — it covers the base-image policy, package-install order of preference, VEX vs `.trivyignore`, and verification workflow.** Run `make scan-runner` locally before pushing; CI has a Trivy gate that blocks merge.

## Runner AMI

The EC2 instances that host runners are built from a two-layer Packer pipeline in `packer/` (base AMI → runner AMI). **Before adding a package, language toolchain, or system dependency to either layer, read `packer/README.md` — it documents the layer split, which provisioner each kind of change belongs in, and how to add a new package without bloating the wrong layer.** Default rule: stable things (OS packages, toolchains, `actions/runner` deps) go in `packer/provision-base.sh`; only the `runs-fleet-agent` orchestration bits go in `packer/provision-runs-fleet.sh`.

## Stack

- Go 1.25+ (AWS SDK v2, go-github)
- AWS: EC2 Fleet API, SQS FIFO, DynamoDB, S3, SSM, ECS Fargate
- Infra: Terraform, in a separate IaC repository (location kept in Claude memory, not committed here; if that memory is unavailable, ask the user and save it back to memory)
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
- `pkg/github/` - GitHub App API client (auth, registration tokens, workflow-job status) plus webhook validation and label parsing
- `pkg/gitops/` - GitOps integration for pool configuration
- `pkg/housekeeping/` - Cleanup tasks (orphaned instances, stale SSM, old jobs)
- `pkg/logging/` - Structured logging (slog JSON output)
- `pkg/metrics/` - Multi-backend metrics (CloudWatch, Prometheus, Datadog)
- `pkg/pools/` - Warm pool reconciliation (hot/stopped instances)
- `pkg/queue/` - Queue abstraction (SQS implementation)
- `pkg/runner/` - Runner lifecycle management (registration orchestration, secrets config), via a GitHub client injected through a small interface
- `pkg/secrets/` - Secrets backend abstraction (SSM, Vault)
- `pkg/termination/` - Instance termination notifications
- `pkg/tracing/` - OpenTelemetry SDK, W3C TraceContext propagation, span instrumentation

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

## Job Labels

GitHub workflows request runners via labels:

```yaml
runs-on: "runs-fleet"
runs-on: "runs-fleet/cpu=4"
runs-on: "runs-fleet/cpu=4/arch=arm64/pool=default"
runs-on: "runs-fleet/cpu=4+16/ram=8+32/family=c7g+m7g"
runs-on: "runs-fleet/cpu=4/arch=arm64/gen=8"
runs-on: "runs-fleet=${{ github.run_id }}/cpu=4"  # legacy form, still supported
```

The bare `runs-fleet` marker is the only required token; run_id is sourced from
the webhook payload. The legacy `runs-fleet=<run-id>/...` form remains supported
(its run_id segment is optional and ignored — the webhook is authoritative).

### Resource labels

- `cpu=<n>` - vCPU count (defaults to 2x range: cpu=4 matches 4-8 vCPUs)
- `cpu=<min>+<max>` - Explicit vCPU range (cpu=4+4 for exact match)
- `ram=<n>` - Exact RAM in GB
- `ram=<min>+<max>` - RAM range in GB
- `family=<f1>+<f2>` - Instance families (e.g., `c7g+m7g`)
- `gen=<n>` - Instance generation (e.g., `gen=8` for Graviton4). Default: any generation
- `arch=<arch>` - Architecture: `arm64` or `amd64`
- `disk=<size>` - Disk in GiB (1-16384, gp3)

### Routing labels

- `runs-fleet` - Runner marker (required). Legacy `runs-fleet=<run-id>/...` still works; run_id is sourced from the webhook
- `pool=<name>` - Warm pool name (routes to pool queue)
- `spot=false` - Force on-demand (skip spot)

## Key Flows

**Cold-start:**
1. Webhook → SQS main queue → Fleet manager creates spot fleet
2. EC2 boots → Agent fetches config from SSM → Registers with GitHub
3. Job executes → Agent self-terminates

**Warm pool:**
1. Webhook with `pool=` label → Pool queue (batch processing)
2. Pool manager assigns running instance OR starts stopped instance OR overflows to cold-start
3. After job: instance detached, pool reconciliation creates on-demand replacement

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
- `RUNS_FLEET_GITHUB_APP_ID`, `RUNS_FLEET_GITHUB_APP_PRIVATE_KEY` - GitHub App auth
- `RUNS_FLEET_GITHUB_WEBHOOK_SECRET` - HMAC signature validation

### Fleet
- `RUNS_FLEET_QUEUE_URL` - Main SQS queue (required)
- `RUNS_FLEET_POOL_QUEUE_URL` - Pool queue
- `RUNS_FLEET_EVENTS_QUEUE_URL` - EventBridge events queue
- `RUNS_FLEET_TERMINATION_QUEUE_URL` - Termination notifications queue
- `RUNS_FLEET_HOUSEKEEPING_QUEUE_URL` - Housekeeping tasks queue
- `RUNS_FLEET_JOBS_TABLE`, `RUNS_FLEET_POOLS_TABLE` - DynamoDB tables
- `RUNS_FLEET_JOBS_POOL_STATUS_GSI` - Optional DynamoDB GSI name for pool busy instance queries
- `RUNS_FLEET_CACHE_BUCKET` - S3 cache bucket
- `RUNS_FLEET_VPC_ID`, `RUNS_FLEET_SUBNET_IDS` - Network config
- `RUNS_FLEET_SECURITY_GROUP_ID`, `RUNS_FLEET_INSTANCE_PROFILE_ARN` - EC2 config
- `RUNS_FLEET_RUNNER_IMAGE` - ECR image URL for runners
- `RUNS_FLEET_SPOT_ENABLED` - Enable spot instances (default: true)

### Secrets Backend
- `RUNS_FLEET_SECRETS_BACKEND` - `ssm` or `vault` (default: ssm)
- `VAULT_ADDR` - Vault server address (required if vault backend)
- `VAULT_AUTH_METHOD` - `aws`, `kubernetes`, `approle`, `token` (default: aws)

### Metrics
- `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` - CloudWatch metrics (default: true)
- `RUNS_FLEET_METRICS_PROMETHEUS_ENABLED` - Prometheus /metrics endpoint
- `RUNS_FLEET_METRICS_DATADOG_ENABLED` - Datadog DogStatsD metrics

### Tracing
- `RUNS_FLEET_TRACING_ENABLED` - Enable OpenTelemetry tracing (default: false)
- `RUNS_FLEET_OTEL_ENDPOINT` - OTLP gRPC collector endpoint (required when tracing enabled)
- `RUNS_FLEET_OTEL_INSECURE` - Use insecure gRPC connection (default: true)
- `RUNS_FLEET_OTEL_SERVICE_NAME` - Service name for traces (default: runs-fleet)

### Cache & Admin
- `RUNS_FLEET_CACHE_SECRET` - HMAC secret for cache auth
- `RUNS_FLEET_ADMIN_OIDC_ISSUER_URL`, `_CLIENT_ID`, `_CLIENT_SECRET`, `_REDIRECT_URL`, `_SCOPES`, `_GROUPS_CLAIM` - Admin API OIDC auth (native relying party, no external gatekeeper); leave unset to disable auth (local dev)
- `RUNS_FLEET_ADMIN_SESSION_SECRET`, `RUNS_FLEET_ADMIN_SESSION_TTL_MINUTES` - Admin session cookie signing key and TTL

## Development Commands

```bash
# Run server locally (requires AWS credentials)
export AWS_REGION=ap-northeast-1
export RUNS_FLEET_QUEUE_URL=...
go run cmd/server/main.go

# Build agent binaries (Nix)
nix build .#agent-arm64
nix build .#agent-amd64

# Build Docker image
make docker-build

# Run tests
go test ./...

# Deploy to ECS via Terraform — the IaC lives in a separate repository.
# Its location and the ECS cluster/service names are kept in Claude memory;
# if that memory is unavailable, ask the user for them and save them back to memory.
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

**Job status:**
- Use typed `db.JobStatus*` constants (not bare strings) for all status comparisons and updates
- Defined in `pkg/db/job_status.go`: Running, Claiming, Terminating, Requeued, Completed, Success, Failed, Error, Orphaned

**Tracing:**
- Spans instrumented at webhook receipt, worker processing, fleet creation, spot interruption, and termination
- W3C TraceContext propagated through SQS message attributes
- Noop provider when tracing is disabled (zero overhead)

**Pool reconciliation:**
- Loop interval: 60 seconds
- Hot pool: instances stay running
- Stopped pool: instances stopped, batch-started on-demand (max 50)
- Idle timeout: 60 minutes (configurable per pool)

**Testing time-dependent code:**
- Default: tests of goroutines + timers/tickers + channels use Go 1.25
  `testing/synctest`. Wrap the body in `synctest.Test(t, func(t *testing.T){ ... })`;
  advance the per-bubble fake clock with `time.Sleep`; settle with `synctest.Wait()`.
  Replace `time.After(...)` safety deadlines and "give it a moment" sleeps with
  `synctest.Wait()` + a non-blocking `select { case <-done: ...; default: ... }`.
  No production clock abstraction needed — stdlib `time.NewTicker`/`time.Now` run
  against the fake clock unchanged. See `pkg/housekeeping/runner_test.go`.
- Constraints: a bubbled test is **not** `t.Parallel()`; every goroutine started
  in the bubble must exit before the bubble returns (drive cancellation to
  completion: `cancel()` → `synctest.Wait()`); bubble goroutines may only block on
  in-memory primitives — not real network/disk. `synctest.Wait()` establishes
  happens-before (it returns only once the other goroutines are durably blocked),
  so reading state they wrote before blocking is race-clean — channels/atomics are
  fine but not required for that.
- When NOT synctest: tests that hit real I/O (`httptest`, real `client.Do`) keep the
  existing seams — overridable interval/delay vars (`FleetRetryBaseDelay`,
  `baseRetryDelay`, `eventsTickInterval`, `checkInterval`) and tick-channel injection
  (`internal/worker`'s `RunWorkerLoopWithTicker`). Pure timestamp fabrication
  (`time.Now().Add(-d)`) needs nothing.

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

- Cold-start: spot-first with on-demand fallback (cost optimization)
- Warm pool: on-demand only (stop/start reliability over negligible spot savings)
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

See `docs/ROADMAP.md` for candidate work beyond Phase 5.
