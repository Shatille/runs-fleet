---
topic: Agent (On-Instance Bootstrap Binary)
last_compiled: 2026-08-21
sources_count: 9
---

# Agent (On-Instance Bootstrap Binary)

## Purpose [coverage: high -- 9 sources]

`cmd/agent` is the binary baked into the runner AMI at
`/opt/runs-fleet/runs-fleet-agent`. One copy executes on every ephemeral EC2
runner instance and is responsible for **acquiring** its job assignment,
registering an ephemeral GitHub Actions runner, executing exactly one workflow
job, shipping that job's logs to S3, then self-terminating the host. (The K8s
pod variant was removed with the K8s runner backend in 2026-06 — the agent is
EC2-only.)

Two shell scripts bootstrap it —
[scripts/cloud-init-boot.sh](../../scripts/cloud-init-boot.sh) →
[scripts/agent-bootstrap.sh](../../scripts/agent-bootstrap.sh), sharing
[scripts/boot-lib.sh](../../scripts/boot-lib.sh). They read instance identity
and tags via IMDSv2, select the secrets backend (SSM or HashiCorp Vault) from
tags, write `/opt/runs-fleet/env`, and `systemctl start runs-fleet-agent`.

The agent has **no persistent state**: it lives only as long as the runner
instance.

Since PR #399 the agent also implements **standby mode**
([cmd/agent/standby.go](../../cmd/agent/standby.go)) — see Architecture. This is
what makes a hot- or warm-pool spare possible: a *running* instance holding a
live agent that polls for the config the orchestrator will write when it assigns
that instance a job.

## Architecture [coverage: high -- 9 sources]

### Standby mode (`cmd/agent/standby.go`, PR #399)

Standby replaces what used to be a fixed `5 × 2s` config-fetch-then-`exit(0)`.
`standbyWaitForConfig(ctx, store, instanceID, deadline, logger)` polls
`secrets.Store.Get(ctx, instanceID)` until a config appears, the deadline
passes, or the context is cancelled. It is the **uniform not-found path for
every instance**: a cold-start instance whose config hasn't been written yet and
a hot-pool spare waiting to be assigned wait the same way.

Polling cadence (`standbyPollDelay(n)`):

| phase | attempts | delay |
|---|---|---|
| fast | `n < standbyFastPolls` (15) | exactly `standbyFastDelay` = 2s (≈30s total, covering the config-write window) |
| slow | thereafter | `standbySlowDelay` = 5s ± `standbyJitterPct` (20%) |

The jitter exists so many idle spares don't hammer the secrets backend in
lockstep.

Three exit paths, all mapped to codes in `main` (never `os.Exit` inside the
loop, so the loop is unit-testable under `synctest`):

- **config found** → return it; the job runs.
- **deadline passed** → `errStandbyDeadline`; `main` logs "exiting for
  reconciler to reclaim" and **exits 0**. The deadline
  (`RUNS_FLEET_STANDBY_DEADLINE_MINUTES`, default 120) is documented as a
  *failsafe against a leaked instance polling forever* — the reconciler is the
  primary decay path for an unused spare.
- **context cancelled** → `ctx.Err()`; `main` exits 0.

Any transient backend error is treated exactly like not-found (log once and keep
polling), so a single SSM/Vault blip cannot strand a standby instance. Only
`secrets.ErrConfigNotFound` is silent; anything else logs "will retry".

The poll runs under a **SIGTERM-aware** context —
`signal.NotifyContext(ctx, SIGTERM, SIGINT)` — so an instance stop (the
reconciler banking an idle spare) cancels the wait and the agent exits 0
promptly instead of being killed mid-sleep. `stop()` is called **immediately
after** the poll returns, so signal handling is scoped to standby only: once a
job config is found the job runs under the plain background context, and a
SIGTERM never aborts a job in flight.

Standby also healed a standing bug: an agent that exhausted its old short retry
budget used to `exit 0` and leave the instance a **zombie** until the
unconfirmed-runner watchdog reaped it minutes later.

### Lifecycle in `main` ([cmd/agent/main.go](../../cmd/agent/main.go))

1. **Panic guard + timing capture.** A `defer recover()` sleeps 2s (so logs
   flush) then `os.Exit(1)`. The first statement after it is
   `timings := bootstrapTimings{boot: readUptime(), start: time.Now()}`.
   `readUptime()` reads the first field of `/proc/uptime` — seconds since kernel
   boot, i.e. kernel + cloud-init + bootstrap scripts, everything before the Go
   process — returning 0 on any error; `parseUptime` is the pure helper.
2. **Required env.** `RUNS_FLEET_INSTANCE_ID` missing → `log.Fatal`.
   `maxRuntimeMinutes` and `standbyDeadline` are resolved here.
3. **`initStore(ctx)`** — AWS config (region from `AWS_REGION`, defaulting to
   `ap-northeast-1`) plus `secrets.NewStore`. Deliberately *only* these two:
   they are all standby needs. A `Close()`-able store is deferred.
4. **Standby poll** (above).
5. **Metric rebase.** On success, `configFoundAt := time.Now()` and
   `timings.boot = 0; timings.config = 0; timings.start = configFoundAt`.
   Meaningful acquisition latency starts when the config appears, not at process
   start — rebasing keeps the standby wait (and a hot spare's long-ago boot) out
   of the bootstrap total so the metric cohort stays clean.
6. **`completeInit(ac, runnerConfig, logger)`** — builds the config-dependent
   components: `SQSTelemetry` (only when the config carries a
   `TerminationQueueURL`) and always an `EC2Terminator`. Split from `initStore`
   so an unassigned standby spare never creates SQS clients it will never use.
7. **Validation.** `run_id` missing from the config is fatal. `jobID` comes from
   `resolveJobID` (`RunnerConfig.JobID`, or `""`).
8. **Safety monitor.** `NewSafetyMonitor(maxRuntimeMinutes, logger)` with a
   timeout callback that calls `terminator.TerminateOnTimeout` — without it "the
   monitor only logs the violation: an instance that outlives max runtime bills
   until the orchestrator's age sweep notices it 10 minutes later."
9. **`runAgent`** runs the phases:
   - **Phase 1 — Download** (timed as `runner`): `DownloadRunner(ctx)` locates
     the pre-baked runner or fetches the release tarball. Failure →
     `terminateWithError(..., "download_failed", err)`.
   - **Phase 2 — Register** (timed as `register`): `RegisterRunner` (a **no-op
     when the config carries a JIT config**), then `SetRunnerEnvironment` writes
     `.env` with `ACTIONS_CACHE_URL`/`ACTIONS_CACHE_TOKEN`, then
     `WriteBuildkitCacheEnv` adds the buildx-shim S3 cache vars, then
     `engageCache` starts the transparent v2 cache interceptor. Registration
     failure → `terminateWithError(..., "registration_failed", err)`; the three
     `.env`/cache steps are all best-effort warnings.
   - **Job-started telemetry**: `timings.applyTo(&jobStatus, jobStartedAt)`
     stamps the five `Bootstrap*Seconds` fields before `SendJobStarted`.
   - **Phase 3 — Execute**: pre-job `SnapshotToolCache`, then
     `ExecuteJobWithConfig(ctx, runnerPath, ac.runnerConfig.JITConfig)`. A nil
     result is synthesized as `ExitCode: -1`.
   - **`shipRunnerLogs`** — **before** cleanup, because cleanup removes `_diag`.
   - **Phase 4 — Cleanup & terminate**: `CleanupRunner`, second tool-cache
     snapshot (misses reported only when *both* snapshots succeeded), then
     `terminator.TerminateInstance` with the assembled `JobStatus` (tool-cache
     misses, cache interception, build-cache outcome, log-upload outcome,
     `CacheBytesWritten`), which sends the completion SQS message and calls
     `ec2:TerminateInstances` on the host.

`engageCache` is fail-open at every step and returns
`engaged` / `failed` / `disabled`. The host `/etc/hosts` pin is installed
**last**, only after the per-instance CA is trusted via `NODE_EXTRA_CA_CERTS`,
so traffic is never redirected to an untrusted listener. Its teardown closure is
`defer`red.

### Boot scripts

`boot-lib.sh` is *sourced, not executed* — no `set -e` of its own, and nothing
runs at source time. It exports `AWS_RETRY_MODE=adaptive` and
`AWS_MAX_ATTEMPTS=10` (DescribeTags `RequestLimitExceeded` under a launch burst
is named as the dominant boot-time failure), and provides `retry`,
`_imds_curl` / `imds_token` / `imds_get`, `imds_bootstrap` (exports `TOKEN`,
`INSTANCE_ID`, `REGION`), `get_tag`, and `system_is_stopping`.

`agent-bootstrap.sh` fails **closed** on tag reads: a failed `DescribeTags` must
not be read as "no backend tag" and fall through to the SSM default. Vault path
components are scrubbed to `[a-zA-Z0-9/_-]` and rejected on `..`; `VAULT_ADDR`
must match `^https://[a-zA-Z0-9.-]+(:[0-9]+)?$`. It then scrubs any stale
`# runs-fleet-cache` `/etc/hosts` pin left by a prior boot, checks
`system_is_stopping`, and `systemctl start runs-fleet-agent` +
`systemctl is-failed`.

`cloud-init-boot.sh` wraps the bootstrap, capturing stdout/stderr to
`/tmp/agent-bootstrap-$$.log`. On non-zero exit it checks `system_is_stopping`
**first** — a stop landing mid-bootstrap is a benign casualty, so it skips both
notification and self-termination and lets the instance become a banked warm
spare. Otherwise it sends a `bootstrap_failed` SQS message carrying
`tail -c 800` of the log (newlines→spaces, non-printables stripped, `jq --arg`
escaped) to the `runs-fleet:termination-queue-url` tag's queue, then
self-terminates via `retry 3 2 aws ec2 terminate-instances`.

### AMI validation

[packer/provision-validate-agent.sh](../../packer/provision-validate-agent.sh)
is a post-bake smoke test that runs inside the Packer builder *before* the
snapshot, so a defective AMI fails the build instead of being promoted to the
launch-template default. For the agent specifically it asserts:

- `/opt/runs-fleet/runs-fleet-agent` exists, is executable, and is an ELF of the
  **expected arch** — this catches "the most common silent-failure mode:
  `docker pull --platform` falling back to the host arch when the requested
  platform is missing from `:latest`";
- the binary can actually be `exec`'d (`sudo timeout 1 "$AGENT"`, grepping the
  output for `exec format error` — exit code is ignored, since it will fail on
  missing config);
- `boot-lib.sh`, `agent-bootstrap.sh`, and the cloud-init per-boot script
  (`/var/lib/cloud/scripts/per-boot/runs-fleet-bootstrap.sh`) exist and pass
  `bash -n`;
- the systemd unit passes `systemd-analyze verify` and contains
  `AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache`, `PIPX_BIN_DIR=/opt/pipx/bin`, and
  a `PATH` prepending `/opt/pipx/bin`;
- the cache-engage helper is `root:root 755`, syntactically valid, **rejects any
  host other than the one results host it pins**, and its sudoers drop-in is
  `root:root 440` and passes `visudo -cf` — then exercises the real
  engage→disengage path end to end.

## Talks To [coverage: high -- 9 sources]

- **IMDSv2** (`http://169.254.169.254/latest/…`) — instance ID and region via
  `boot-lib.sh`; every call wrapped in `--retry-connrefused` + bounded timeouts.
- **AWS EC2 `DescribeTags`** — `get_tag` reads
  `runs-fleet:secrets-backend`, `runs-fleet:vault-*`, and
  `runs-fleet:termination-queue-url`.
- **AWS SSM Parameter Store** — default secrets backend; the runner config for
  this instance ID.
- **HashiCorp Vault** — alternate backend; auth method selectable via
  `runs-fleet:vault-auth-method`, KV v1 and v2 both supported, version
  auto-detected when the tag is unset.
- **GitHub API** — runner registration via `config.sh` on the token path; on the
  JIT path GitHub already created the registration when it minted the config.
- **AWS SQS** — `SQSTelemetry` sends `JobStatus` messages to the termination
  queue named in the runner config; `cloud-init-boot.sh` sends
  `bootstrap_failed` to the queue from the instance tag.
  [pkg/termination](../../pkg/termination)'s handler consumes both.
- **AWS EC2 `TerminateInstances`** — `EC2Terminator` on the normal and timeout
  paths; `cloud-init-boot.sh` on bootstrap failure.
- **AWS S3** — `logship` uploads the runner's `_diag` logs to
  `RunnerConfig.RunnerLogsBucket`.
- **Localhost cache interceptor** — `cacheproxy.Proxy` intercepts the runner's
  Actions cache traffic and forwards it to `RunnerConfig.CacheURL`.
- **`/usr/local/sbin/runs-fleet-cache-engage`** — the AMI-baked root helper for
  the CA trust + `/etc/hosts` pin, invoked via a scoped sudoers drop-in.
- **CloudWatch Logs** — **no longer used**; PR #446 removed the path entirely
  (the instance role never had `logs:*`).

## API Surface [coverage: medium -- 4 sources]

The agent has **no CLI flags**. All configuration arrives via environment
(written into `/opt/runs-fleet/env` by `agent-bootstrap.sh`, plus `Environment=`
lines in the systemd unit) and the `RunnerConfig` fetched from the secrets
backend.

| Env var | Source | Purpose |
|---------|--------|---------|
| `RUNS_FLEET_INSTANCE_ID` | bootstrap | **Required**; keys the secrets lookup |
| `AWS_REGION` | bootstrap | Defaults to `ap-northeast-1` |
| `RUNS_FLEET_MAX_RUNTIME_MINUTES` | — | `SafetyMonitor` ceiling (default 360) |
| `RUNS_FLEET_STANDBY_DEADLINE_MINUTES` | — | Standby budget before a clean exit 0 (default 120) |
| `AGENT_TOOLSDIRECTORY` | systemd unit | Tool-cache dir for miss snapshots (default `/opt/hostedtoolcache`) |
| `PIPX_HOME` / `PIPX_BIN_DIR` / `PATH` | systemd unit | pipx launchers on the job PATH |
| `RUNS_FLEET_SECRETS_BACKEND`, `VAULT_*` | bootstrap | Backend selection and Vault connection params |

Job-specific values (`run_id`, `job_id`, `jit_token`, `jit_config`,
`runner_name`, `labels`, `cache_url`, `cache_token`,
`termination_queue_url`, `buildkit_cache_*`, `runner_logs_*`) come from the
`RunnerConfig` JSON, **not** env vars.

Exit codes:

| code | cause |
|---|---|
| `0` | normal path (ends in self-termination); standby deadline reached; standby cancelled by SIGTERM/SIGINT |
| `1` | panic recovery |
| `log.Fatal` | missing `RUNS_FLEET_INSTANCE_ID`, `initStore` failure, missing `run_id` |

Note that a `download_failed` / `registration_failed` path does **not** exit
non-zero — `terminateWithError` sends a `failure` `JobStatus` and terminates the
host, and `runAgent` simply returns.

## Data [coverage: medium -- 4 sources]

State the agent owns on the runner host:

- **`/opt/runs-fleet/env`** — bootstrap-written env file (backend selection;
  Vault connection details in Vault mode). Loaded via the unit's
  `EnvironmentFile=`.
- **Runner workspace** — the runner tree under `runnerPath`
  (`/opt/actions-runner` on a baked AMI), including `config.sh`, `.env`,
  per-job `_work/`, `_diag/`, plus `runs-fleet-cache-ca.pem`,
  `_rf_cache_staging/`, and `_rf_buildkit_cache_outcome`.
- **`/proc/uptime`** — read once at process start for the `boot` segment (then
  zeroed by the standby rebase).
- **`/etc/hosts`** — the `# runs-fleet-cache` pin, installed by the cache-engage
  helper and torn down on exit; scrubbed at boot by `agent-bootstrap.sh` in case
  a prior agent was SIGKILLed before teardown.
- **`/tmp/agent-bootstrap-$$.log`** — the bootstrap capture whose tail becomes
  the `bootstrap_failed` reason.
- **S3** — `runner-logs/<run_id>/<job_id>/<instance_id>/*.log.gz`, the one
  artifact that outlives the instance.

No long-lived local state survives termination.

Bootstrap timing constants in `main.go`: `logShipTimeout` = 60s,
`logShipMaxFileBytes` = `128 << 20`.

## Key Decisions [coverage: high -- 9 sources]

- **Standby exists so a pool spare can hold a live agent (PR #399).** A hot-pool
  claim assigns a *running* instance without calling `StartInstances` — see
  [warm-pools](warm-pools.md) — which only works if something on that host is
  already waiting for a config to appear. Standby is that something.
- **One uniform not-found path, not two.** A cold-start instance whose config
  hasn't landed yet and a pool spare awaiting assignment poll identically. The
  alternative (a special-cased standby probe in the bootstrap script) was
  explicitly rejected: `agent-bootstrap.sh` defers the decision to the agent
  rather than probing the backend itself.
- **Fast-then-jittered polling.** ~30s of exact 2s polls covers the
  config-write window; after that 5s ± 20% sheds backend load and de-syncs a
  fleet of idle spares.
- **The standby deadline is a failsafe, not the decay mechanism.** The
  reconciler is the primary path for reclaiming an unused spare; the 120-minute
  deadline only stops a *leaked* instance polling forever. It is deliberately
  inside the 4h ephemeral-pool-cleanup window.
- **Transient backend errors are indistinguishable from not-found, on purpose.**
  A single SSM/Vault blip must not strand a standby instance, so everything
  except an explicit `ErrConfigNotFound` is logged and retried rather than
  escalated.
- **Signal handling is scoped to standby only.** `signal.NotifyContext` is
  installed for the poll and `stop()`ped the moment a config is found, so a
  SIGTERM can cancel a *wait* but never a job in flight. This is a different
  mechanism from `pkg/agent`'s `Executor`, which installs its own handler to
  forward signals into the runner's process group.
- **`os.Exit` stays out of the standby loop.** `standbyWaitForConfig` returns a
  sentinel and `main` maps outcomes to exit codes, so the loop is testable under
  `synctest`.
- **`initStore` / `completeInit` split.** An unassigned spare that never gets a
  job creates no SQS clients. (Pre-#446 this also covered a CloudWatch logger.)
- **Acquisition latency is rebased to config-discovery, not process start.**
  Otherwise a hot spare's hours-old boot and its standby wait would dominate
  `bootstrap_total_seconds` and make the metric meaningless across cold-start
  and hot-spare cohorts.
- **The runtime ceiling terminates, it doesn't just warn.** The comment on the
  timeout callback names the cost of the old behavior: an instance that outlives
  max runtime bills until the age sweep notices ~10 minutes later.
  `TerminateOnTimeout` reports the distinct `timeout` result rather than routing
  through `SendJobCompleted`, which would recompute status from the exit code and
  collapse it into a generic failure.
- **Bootstrap timing as five flat fields, not a map (PR #387).** `boot` (from
  `/proc/uptime` — the only way to see the pre-process segment), `config`
  (`initStore` + secrets fetch), `runner` (`DownloadRunner`), `register`
  (`RegisterRunner` + `.env` writes + `engageCache`). Named `omitempty` fields
  close the phase enum at compile time on both ends, bounding metric
  cardinality; `total` is sent explicitly (rebased start → `SendJobStarted`)
  rather than summed, so untimed gaps stay visible.
- **JIT config supersedes token registration.** When `RunnerConfig.JITConfig` is
  set, `RegisterRunner` returns immediately and the config is handed to `run.sh`
  via `ACTIONS_RUNNER_INPUT_JITCONFIG` — in the environment, never argv (argv is
  world-readable via `/proc/<pid>/cmdline`). GitHub binds a JIT registration to
  a single job, so the runner cannot be handed a different queued job that
  merely shares its labels.
- **Repo-scoped registration only** on the token path: `RegisterRunner` refuses
  to run without `config.Repo`, because org-level registration would let runners
  pick up jobs across repositories.
- **Every `.env` writer runs before Phase 3.** `run.sh` reads `.env` at startup;
  the ordering comment notes it held when `config.sh` created the runner
  directory and still holds on the JIT path, where the directory comes from the
  tarball extract.
- **Cache interception is fail-open and engaged last.** Any failure leaves the
  runner talking to GitHub's cache directly; the host pin is installed only
  after the CA is trusted.
- **Logs ship before cleanup, and shipping can never fail the job.** See
  [agent-runtime](agent-runtime.md) — `Ship` returns an outcome string, bounded
  by a 60s timeout because the instance is still billable and self-termination
  is waiting.
- **Self-termination with a *reason* on bootstrap failure.** Terminated
  instances retain no console log, so the `bootstrap_failed` SQS message is the
  only diagnostic channel; hence the 800-byte log tail.
- **A stop landing mid-bootstrap is not a failure.** Both `agent-bootstrap.sh`
  and `cloud-init-boot.sh` check `system_is_stopping` — the former before
  `systemctl start` (which would fail with a destructive-transaction error), the
  latter at the failure decision point (covering a shutdown that aborted an
  earlier step). Getting out of the way lets the instance become a banked warm
  spare instead of churning the pool.
- **Boot-time tag reads fail closed.** `get_tag`'s doc comment is explicit:
  callers that require a tag must treat a non-zero return as fatal, because
  proceeding with an empty value would silently mis-route the agent onto the
  wrong secrets backend.
- **Adaptive AWS retry at boot.** `AWS_RETRY_MODE=adaptive` +
  `AWS_MAX_ATTEMPTS=10` in `boot-lib.sh` adds client-side rate limiting and
  spreads attempts wider than the SDK default of 3, because DescribeTags
  throttling during a launch burst is the dominant boot-time failure.
- **`|| true` on the log reads in `cloud-init-boot.sh` is deliberate.** Under
  `set -e` a failed `cat`/`tail` (e.g. the log couldn't be created on a full
  disk) would abort the script before the notification and self-termination,
  leaving a zombie instance.

## Gotchas [coverage: high -- 7 sources]

- **A standby spare burns up to 2 hours before exiting.** The default
  `RUNS_FLEET_STANDBY_DEADLINE_MINUTES` is 120, and the instance bills the whole
  time. It is designed to be reclaimed by the reconciler well before then; if
  the reconciler is blind to the pool (see the `ListPools` truncation history in
  [warm-pools](warm-pools.md)), the deadline is the only backstop.
- **A standby spare with a *stale* config never enters standby at all.** If a
  config already exists for the instance, the very first `store.Get` succeeds and
  the agent proceeds to run against it. That is why the hot-pool claim path
  re-checks `HasRunnerConfig` and skips any running instance carrying one — the
  agent reads its config once and never revalidates.
- **The standby rebase zeroes `boot` and `config` unconditionally on success.**
  `timings.boot = 0; timings.config = 0` runs on *every* path where a config was
  found, and `omitempty` then drops both fields from the wire. So a current
  agent never reports `bootstrap_boot_seconds` or `bootstrap_config_seconds` at
  all: absence is the normal case, not evidence of a pre-#387 agent, and the
  only bootstrap segments a dashboard can rely on today are `runner`,
  `register`, and `total`. `readUptime()` is still called (and its result still
  discarded) at process start.
- **Agent changes deploy only via the AMI cascade.** The binary is extracted
  from the ECR orchestrator image by Packer (`provision-runs-fleet.sh` does
  `docker cp …/runs-fleet-agent`), so a merged `cmd/agent` change reaches
  production only after Build Runner Image → Build AMIs both run. Merging the Go
  change alone deploys nothing. Stopped pool instances additionally never
  re-image, so they hold their creation AMI until explicitly replaced.
- **The agent runs as `ec2-user`, not root.** The systemd unit sets
  `User=ec2-user`, which is why the CA trust and `/etc/hosts` pin go through a
  root helper behind a sudoers drop-in rather than being done inline.
- **`Restart=no`.** A crashed agent is not restarted; the instance sits until
  the panic guard's `exit(1)` and then until a housekeeping sweep or the
  reconciler reaps it.
- **`readUptime` returns 0 off Linux or on any read/parse error**, so the `boot`
  phase silently disappears rather than failing the agent (production is AL2023,
  so this only bites tests and local runs).
- **64-char runner name limit.** GitHub rejects runner names >64 chars; naming
  logic appends a short job-ID suffix rather than the full ID. Verify any new
  naming logic against this limit.
- **`runs-fleet-agent` systemd unit must exist on the AMI.**
  `agent-bootstrap.sh` calls `systemctl start runs-fleet-agent` and checks
  `is-failed`; the unit is installed by the Packer runs-fleet layer and its
  contents are asserted by `provision-validate-agent.sh`.
- **IMDSv2 hop limits.** Bootstrap uses IMDSv2 tokens; some IaC defaults
  restrict hops to 1, which can break containerized agents on EC2.
- **Vault tag validation is strict.** `agent-bootstrap.sh` errors out when
  required `runs-fleet:vault-*` tags are unreadable, and path components are
  validated against traversal — do not pass dynamic user input through these
  tags.
- **The `bootstrap_failed` notification is best-effort and silent when the tag
  is missing.** No `runs-fleet:termination-queue-url` tag means the instance
  self-terminates with only a `WARN` line on a console log nobody will read.
- **`terminateWithError` fabricates both timestamps as `time.Now()`**, so a
  download- or registration-failure record shows a zero-length job.
- **A nil `ac.terminator` would panic on the failure paths** — it never is,
  because `completeInit` always constructs an `EC2Terminator` (only `telemetry`
  is conditional on `TerminationQueueURL`), and `EC2Terminator.terminate`
  nil-checks its telemetry.
- **Cleanup and telemetry are best-effort.** `.env` writes, buildkit-cache env,
  cache engage, tool-cache snapshots, log shipping, and `CleanupRunner` failures
  all only warn — none abort the job.
- **`system_is_stopping` matches on the string, not the exit code.**
  `systemctl is-system-running` exits non-zero for several states (including
  `degraded`), so an exit-code check would misfire; the inner non-zero is masked
  by the command substitution, keeping it safe under the caller's `set -e`.

## Sources [coverage: high]

- [cmd/agent/main.go](../../cmd/agent/main.go)
- [cmd/agent/standby.go](../../cmd/agent/standby.go)
- [scripts/cloud-init-boot.sh](../../scripts/cloud-init-boot.sh)
- [scripts/agent-bootstrap.sh](../../scripts/agent-bootstrap.sh)
- [scripts/boot-lib.sh](../../scripts/boot-lib.sh)
- [packer/provision-validate-agent.sh](../../packer/provision-validate-agent.sh)
- [packer/provision-runs-fleet.sh](../../packer/provision-runs-fleet.sh)
- [pkg/agent/registration.go](../../pkg/agent/registration.go)
- [pkg/secrets/store.go](../../pkg/secrets/store.go)
