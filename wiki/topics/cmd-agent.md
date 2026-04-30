---
topic: Agent (On-Instance Bootstrap Binary)
last_compiled: 2026-04-30
sources_count: 4
---

# Agent (On-Instance Bootstrap Binary)

## Purpose [coverage: high -- 4 sources]

`cmd/agent` is the long-running binary baked into the runner AMI / container image. One copy executes on every ephemeral runner — EC2 instance or Kubernetes pod — and is responsible for fetching its job assignment, registering an ephemeral GitHub Actions runner, executing exactly one workflow job, then self-terminating the host.

Two cloud-init shell scripts (`scripts/cloud-init-boot.sh` → `scripts/agent-bootstrap.sh`) bootstrap the binary on EC2: they read instance tags via IMDSv2, fetch encrypted job config from SSM or HashiCorp Vault, write `/opt/runs-fleet/env`, and `systemctl start runs-fleet-agent`. On K8s, the binary reads its config from mounted files via `agent.FetchK8sConfig`.

The agent has no persistent state: it lives only as long as the runner instance.

## Architecture [coverage: high -- 4 sources]

`main.go` boots in four phases plus init:

- **Init**: `IsK8sEnvironment()` decides backend; `getInstanceID()` resolves identity (env var on EC2, hostname on K8s); `initEC2Mode()` or `initK8sMode()` constructs an `agentConfig` containing telemetry client, instance terminator, runner config, AWS config, optional CloudWatch logger, and a cleanup func.
- **Phase 1 — Download**: `agent.NewDownloader().DownloadRunner(ctx)` fetches the GitHub Actions runner release tarball.
- **Phase 2 — Register**: `agent.NewRegistrar(ac.secretsStore, logger)` (or `NewRegistrarWithoutSecrets` on K8s) calls the runner's `config.sh` with `--unattended --ephemeral --url <repo> --token <jit>`. Always repo-scoped — never org-scoped — to prevent jobs from leaking across repositories. Then `SetRunnerEnvironment` writes `.env` with `ACTIONS_CACHE_URL` and `ACTIONS_CACHE_TOKEN`.
- **Phase 3 — Execute**: `agent.NewExecutor(logger, safetyMonitor).ExecuteJob(ctx, runnerPath)` runs the job; `SafetyMonitor` enforces `RUNS_FLEET_MAX_RUNTIME_MINUTES` (default 360, range 1-1440).
- **Phase 4 — Cleanup & terminate**: `agent.NewCleanup(logger).CleanupRunner(...)` deletes runner artifacts; `ac.terminator.TerminateInstance(ctx, instanceID, jobStatus)` sends an SQS termination message and (on EC2) calls `ec2:TerminateInstances` on the host.

A panic recover at the top of `main()` sleeps 2s then `os.Exit(1)` so logs flush.

## Talks To [coverage: high -- 4 sources]

- **IMDSv2** (`http://169.254.169.254/latest/...`) — fetch instance ID, region, and tags during bootstrap.
- **AWS SSM Parameter Store** — read `/runs-fleet/runners/{INSTANCE_ID}/config` on EC2 (default secrets backend).
- **HashiCorp Vault** — alternate secrets backend; AppRole / AWS / K8s auth selectable via tag `runs-fleet:vault-auth-method`. KV v1 and v2 supported.
- **GitHub API** — runner registration via `config.sh` using a JIT token from the orchestrator.
- **AWS SQS** — telemetry client (`agent.NewSQSTelemetry`) sends `JobStatus` messages to `RUNS_FLEET_TERMINATION_QUEUE_URL` (group ID = instance ID).
- **AWS EC2** — `EC2Terminator` calls `TerminateInstances` on self.
- **Kubernetes API** — in-cluster client; `K8sTerminator` deletes the pod after job completion.
- **Valkey** — alternative telemetry transport on K8s (`agent.NewValkeyTelemetry`, key `runs-fleet:termination`).
- **CloudWatch Logs** — optional log streaming when `RUNS_FLEET_LOG_GROUP` is set.

## API Surface [coverage: medium -- 2 sources]

The agent has **no CLI flags** — all configuration arrives via environment and files written by `agent-bootstrap.sh`:

| Env var | Purpose |
|---------|---------|
| `RUNS_FLEET_INSTANCE_ID` | Required on EC2; hostname is used on K8s |
| `RUNS_FLEET_RUN_ID` | GitHub workflow run ID; falls back to runner config |
| `RUNS_FLEET_TERMINATION_QUEUE_URL` | SQS FIFO queue for status telemetry |
| `RUNS_FLEET_VALKEY_ADDR`, `_PASSWORD`, `_DB` | Valkey telemetry on K8s |
| `RUNS_FLEET_MAX_RUNTIME_MINUTES` | SafetyMonitor cap (default 360) |
| `RUNS_FLEET_LOG_GROUP` | Optional CloudWatch log group |
| `RUNS_FLEET_CACHE_URL`, `RUNS_FLEET_CACHE_TOKEN` | Forwarded to runner as `ACTIONS_CACHE_*` |
| `AWS_REGION` | Defaults to `ap-northeast-1` |

Exit codes: `1` on panic recovery; otherwise the agent always reaches `TerminateInstance` and returns `0`.

## Data [coverage: medium -- 2 sources]

State the agent owns on the runner host:

- **`/opt/runs-fleet/env`** — bootstrap-written env file (job config in Vault mode; minimal handoff in SSM mode).
- **Runner workspace** — extracted GitHub Actions runner under `runnerPath`, including `config.sh`, `.env`, and per-job working directory.
- **`/var/run/secrets/kubernetes.io/serviceaccount/namespace`** — read on K8s to resolve the pod's namespace (fallback `default`).
- **CloudWatch log stream** — `{instanceID}/{runID}` if `RUNS_FLEET_LOG_GROUP` set.

No long-lived state survives termination.

## Key Decisions [coverage: high -- 4 sources]

- **Repo-scoped registration only.** `registration.go:80` enforces `config.Repo != ""` and refuses org-level registration to prevent runners from picking up jobs across repositories.
- **JIT (Just-In-Time) tokens, not static.** Each runner registration uses a single-use token minted by the orchestrator; combined with `--ephemeral`, the runner deregisters itself after one job.
- **Bootstrap extracted into `agent-bootstrap.sh`.** Recent commit `01ddbf8` moved bootstrap out of inline cloud-init to fix runner names exceeding 64 chars; cloud-init now just calls the script.
- **Two telemetry transports.** SQS on EC2 (FIFO with `MessageGroupId = instanceID` for ordering); Valkey on K8s. Both implement the same `TelemetryClient` interface.
- **Pool standby semantics.** When neither SSM nor Vault has config for the instance, the bootstrap exits 0 — the instance stays warm in a pool until a job message creates its config.
- **Self-termination via `aws ec2 terminate-instances`.** On bootstrap failure, `cloud-init-boot.sh` notifies SQS then self-terminates so the orchestrator can replace the instance.

## Gotchas [coverage: high -- 3 sources]

- **64-char runner name limit.** GitHub rejects runner names >64 chars; recent fix appends a short job-ID suffix instead of the full ID. Verify any new naming logic against this limit.
- **`runs-fleet-agent` systemd unit must exist on the AMI.** `agent-bootstrap.sh` calls `systemctl start runs-fleet-agent`; absent the unit, exit code 1 is silent at the cloud-init level.
- **Spot interruption mid-job.** `SafetyMonitor` and `JobResult.InterruptedBy` capture spot signals; the orchestrator re-queues based on the termination queue message.
- **IMDSv2 hop limits.** Bootstrap uses TTL 300s tokens; some IaC defaults restrict hops to 1, which can break containerized agents on EC2.
- **Vault path traversal guard.** `agent-bootstrap.sh` rejects `..` in `VAULT_KV_MOUNT` and `VAULT_BASE_PATH`; do not pass dynamic user input through these tags.
- **SSM ParameterNotFound is treated as standby.** Permanent errors (`AccessDenied`, `InvalidKeyId`, `ValidationException`) abort retries immediately; transient errors retry up to 5x with 2s sleep.
- **Cleanup is best-effort.** Cleanup, env-write, and CloudWatch start failures only warn — they do not abort the job.

## Sources [coverage: high]

- [cmd/agent/main.go](../../cmd/agent/main.go)
- [scripts/agent-bootstrap.sh](../../scripts/agent-bootstrap.sh)
- [scripts/cloud-init-boot.sh](../../scripts/cloud-init-boot.sh)
- [pkg/agent/registration.go](../../pkg/agent/registration.go)
