---
topic: Agent (On-Instance Bootstrap Binary)
last_compiled: 2026-07-09
sources_count: 7
---

# Agent (On-Instance Bootstrap Binary)

## Purpose [coverage: high -- 7 sources]

`cmd/agent` is the binary baked into the runner AMI. One copy executes on every ephemeral EC2 runner instance and is responsible for fetching its job assignment, registering an ephemeral GitHub Actions runner, executing exactly one workflow job, then self-terminating the host. (The K8s pod variant was removed with the K8s runner backend in 2026-06 — the agent is EC2-only.)

Three shell scripts (`scripts/cloud-init-boot.sh` → `scripts/agent-bootstrap.sh`, sharing `scripts/boot-lib.sh`) bootstrap the binary: they read instance identity and tags via IMDSv2, select the secrets backend (SSM or HashiCorp Vault) from tags, write `/opt/runs-fleet/env`, and `systemctl start runs-fleet-agent`.

The agent has no persistent state: it lives only as long as the runner instance.

## Architecture [coverage: high -- 7 sources]

`main.go` boots in four phases plus init, and since PR #387 measures each bootstrap segment:

- **Timing capture**: the very first statement after the panic guard is `timings := bootstrapTimings{boot: readUptime(), start: time.Now()}`. `readUptime()` reads the first field of `/proc/uptime` (seconds since kernel boot — covering kernel + cloud-init + bootstrap scripts, everything before the Go process), returning 0 on any error; `parseUptime` is the pure helper. All later segments are `time.Now()` deltas, which ride Go's monotonic clock.
- **Init** (timed as `config`): `initAgent()` loads AWS config, constructs the secrets store (`secrets.NewStore`), and calls `fetchRunnerConfig` (5 attempts, 2s delay) to read the `RunnerConfig` keyed by `RUNS_FLEET_INSTANCE_ID`. If the store reports `secrets.ErrConfigNotFound`, the instance is in pool standby and the agent exits 0. Telemetry (`agent.NewSQSTelemetry`) is created only when the config carries a `TerminationQueueURL`; an `EC2Terminator` and optional `CloudWatchLogger` follow. `run_id` missing from the config is fatal.
- **Phase 1 — Download** (timed as `runner`): `agent.NewDownloader().DownloadRunner(ctx)` locates the pre-baked runner or fetches the GitHub Actions runner release tarball.
- **Phase 2 — Register** (timed as `register`): `agent.NewRegistrar(ac.secretsStore, logger)` calls the runner's `config.sh` with `--unattended --ephemeral --url <repo> --token <jit>`. Always repo-scoped — never org-scoped — to prevent jobs from leaking across repositories. Then `SetRunnerEnvironment` writes `.env` with `ACTIONS_CACHE_URL`/`ACTIONS_CACHE_TOKEN`, and `engageCache` starts the transparent v2 cache interceptor (`pkg/agent/cacheproxy`) — fail-open at every step, with the host pin installed last, only after the per-instance CA is trusted via `NODE_EXTRA_CA_CERTS`.
- **Job started telemetry**: `timings.applyTo(&jobStatus, jobStartedAt)` stamps the five `Bootstrap*Seconds` fields onto the `JobStatus` before `SendJobStarted`. The total is the `start`→`jobStartedAt` span, left zero if `start` was never captured.
- **Phase 3 — Execute**: after a best-effort tool-cache snapshot (`agent.SnapshotToolCache`), `agent.NewExecutor(logger, safetyMonitor).ExecuteJob(ctx, runnerPath)` runs the job; `SafetyMonitor` enforces `RUNS_FLEET_MAX_RUNTIME_MINUTES` (default 360).
- **Phase 4 — Cleanup & terminate**: `agent.NewCleanup(logger).CleanupRunner(...)` deletes runner artifacts; a second tool-cache snapshot yields `ToolCacheMisses` (only when both snapshots succeeded); `ac.terminator.TerminateInstance(ctx, instanceID, jobStatus)` sends the completion SQS message (with `CacheInterception`/`CacheBytesWritten`) and calls `ec2:TerminateInstances` on the host.

A panic recover at the top of `main()` sleeps 2s then `os.Exit(1)` so logs flush.

## Talks To [coverage: high -- 7 sources]

- **IMDSv2** (`http://169.254.169.254/latest/...`) — bootstrap scripts fetch instance ID, region, and tags (`boot-lib.sh`).
- **AWS SSM Parameter Store** — read `/runs-fleet/runners/{INSTANCE_ID}/config` (default secrets backend).
- **HashiCorp Vault** — alternate secrets backend; auth method selectable via tag `runs-fleet:vault-auth-method`. KV v1 and v2 supported.
- **GitHub API** — runner registration via `config.sh` using a JIT token minted by the orchestrator.
- **AWS SQS** — `agent.NewSQSTelemetry` sends `JobStatus` messages to the termination queue named in the runner config; `pkg/termination`'s handler consumes them and publishes the bootstrap segments as `AgentBootstrapSeconds` distributions (dimensions: Pool, Phase).
- **AWS EC2** — `EC2Terminator` calls `TerminateInstances` on self; `cloud-init-boot.sh` does the same on bootstrap failure.
- **CloudWatch Logs** — optional log streaming when `RUNS_FLEET_LOG_GROUP` is set (stream `{instanceID}/{runID}`).
- **Localhost cache interceptor** — `cacheproxy.Proxy` intercepts the runner's Actions cache traffic and forwards it to the orchestrator's cache endpoint (`RunnerConfig.CacheURL`).

## API Surface [coverage: medium -- 3 sources]

The agent has **no CLI flags** — all configuration arrives via environment (written by `agent-bootstrap.sh` into the systemd unit's env) and the `RunnerConfig` fetched from the secrets backend:

| Env var | Purpose |
|---------|---------|
| `RUNS_FLEET_INSTANCE_ID` | Required; keys the secrets lookup |
| `RUNS_FLEET_MAX_RUNTIME_MINUTES` | SafetyMonitor cap (default 360) |
| `RUNS_FLEET_LOG_GROUP` | Optional CloudWatch log group |
| `AGENT_TOOLSDIRECTORY` | Tool cache dir for miss snapshots (default `/opt/hostedtoolcache`) |
| `AWS_REGION` | Defaults to `ap-northeast-1` |

Job-specific values (`run_id`, `job_id`, `jit_token`, `runner_name`, `labels`, `cache_url`, `cache_token`, `termination_queue_url`) come from the `RunnerConfig` JSON, not env vars.

Exit codes: `0` on pool standby (`ErrConfigNotFound`) and on the normal path (which ends in self-termination); `1` on panic recovery; `log.Fatal` on missing `RUNS_FLEET_INSTANCE_ID`, missing `run_id`, or init failure.

## Data [coverage: medium -- 3 sources]

State the agent owns on the runner host:

- **`/opt/runs-fleet/env`** — bootstrap-written env file (backend selection; Vault connection details in Vault mode).
- **Runner workspace** — extracted GitHub Actions runner under `runnerPath`, including `config.sh`, `.env`, per-job `_work/`, plus the cache-interceptor artifacts `runs-fleet-cache-ca.pem` and `_rf_cache_staging/`.
- **`/proc/uptime`** — read once at process start for the `boot` bootstrap segment.
- **CloudWatch log stream** — `{instanceID}/{runID}` if `RUNS_FLEET_LOG_GROUP` set.

No long-lived state survives termination.

## Key Decisions [coverage: high -- 7 sources]

- **Repo-scoped registration only.** `registration.go` enforces `config.Repo != ""` and refuses org-level registration to prevent runners from picking up jobs across repositories.
- **JIT (Just-In-Time) tokens, not static.** Each runner registration uses a single-use token minted by the orchestrator; combined with `--ephemeral`, the runner deregisters itself after one job.
- **Bootstrap timing as five flat fields, not a map (PR #387).** `bootstrapTimings` captures `boot` (from `/proc/uptime` — the only way to see the pre-process segment: kernel, cloud-init, bootstrap scripts), `config` (around `initAgent`: AWS config + secrets fetch), `runner` (around `DownloadRunner`), and `register` (around `RegisterRunner` + `SetRunnerEnvironment` + `engageCache`). Five named `omitempty` fields on `JobStatus` instead of a `map[string]float64` make the phase enum closed at compile time, bounding metric cardinality — the orchestrator also fixes its own phase list rather than trusting agent-supplied keys. The `total` is sent explicitly (process start → `SendJobStarted`) rather than summed from parts, so untimed gaps (telemetry setup, CloudWatch logger start, tool-cache snapshot) don't silently vanish. Warm-pool resume keeps the semantics: a stopped instance's kernel clock restarts at zero on start, so `/proc/uptime` measures the resume path exactly as it measures a cold boot.
- **Pool standby semantics.** When the secrets backend has no config for the instance, the agent exits 0 — the instance stays warm in a pool until a job message creates its config. `agent-bootstrap.sh` defers this decision to the agent rather than probing the backend itself.
- **Self-termination with a reason on bootstrap failure.** `cloud-init-boot.sh` captures the bootstrap log, sends its tail (800 bytes, printable-stripped) as a `bootstrap_failed` SQS message, then self-terminates with retries — terminated instances retain no console log, so the SQS message is the only diagnostic channel. Exception: if `system_is_stopping` (warm-pool banking or a spot stop landing mid-bootstrap), the script gets out of the way so the instance becomes a banked warm spare instead of churning the pool.
- **Cache interception is fail-open and engaged last.** `engageCache` reports `engaged`/`failed`/`disabled` in telemetry; any failure leaves the runner talking to GitHub's cache directly, and the host pin is installed only after the CA is trusted so traffic is never redirected to an untrusted listener.

## Gotchas [coverage: high -- 6 sources]

- **Agent changes deploy only via the AMI cascade.** The binary is extracted from the ECR orchestrator image by Packer (`packer/provision-runs-fleet.sh` does `docker cp .../runs-fleet-agent`), so a merged `cmd/agent` change reaches production only after Build Runner Image → Build AMIs both run. Merging the Go change alone deploys nothing.
- **Old-agent / new-orchestrator compatibility is deliberate.** The five `Bootstrap*Seconds` fields are `omitempty`; pre-#387 agents simply omit them and the termination handler skips non-positive segments. Conversely, old orchestrators ignore the unknown JSON fields. Both directions of skew during an AMI rollout are safe — but it also means a genuinely-zero segment is indistinguishable from "not measured".
- **`readUptime` returns 0 off Linux or on any read/parse error** — the `boot` phase silently disappears from metrics rather than failing the agent (production is AL2023, so this only bites tests and local runs).
- **64-char runner name limit.** GitHub rejects runner names >64 chars; naming logic appends a short job-ID suffix instead of the full ID. Verify any new naming logic against this limit.
- **`runs-fleet-agent` systemd unit must exist on the AMI.** `agent-bootstrap.sh` calls `systemctl start runs-fleet-agent` and checks `is-failed`; the unit is installed by the Packer runner layer.
- **Spot interruption mid-job.** `SafetyMonitor` and `JobResult.InterruptedBy` capture interruption signals; the orchestrator re-queues based on the termination queue message.
- **IMDSv2 hop limits.** Bootstrap uses IMDSv2 tokens; some IaC defaults restrict hops to 1, which can break containerized agents on EC2.
- **Vault tag validation is strict.** `agent-bootstrap.sh` errors out when required `runs-fleet:vault-*` tags are unreadable, and path components are validated against traversal; do not pass dynamic user input through these tags.
- **Config-not-found retries before standby.** `fetchRunnerConfig` retries all errors — including `ErrConfigNotFound` — for 5 attempts × 2s before the standby check, so a standby instance burns ~8s before exiting 0.
- **Cleanup is best-effort.** Cleanup, env-write, CloudWatch start, tool-cache snapshot, and cache-interceptor failures only warn — they do not abort the job.

## Sources [coverage: high]

- [cmd/agent/main.go](../../cmd/agent/main.go)
- [scripts/cloud-init-boot.sh](../../scripts/cloud-init-boot.sh)
- [scripts/agent-bootstrap.sh](../../scripts/agent-bootstrap.sh)
- [scripts/boot-lib.sh](../../scripts/boot-lib.sh)
- [pkg/agent/registration.go](../../pkg/agent/registration.go)
- [pkg/secrets/store.go](../../pkg/secrets/store.go)
- [packer/provision-runs-fleet.sh](../../packer/provision-runs-fleet.sh)
