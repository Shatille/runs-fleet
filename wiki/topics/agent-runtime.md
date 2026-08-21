---
topic: Agent Runtime (pkg/agent library)
last_compiled: 2026-08-21
sources_count: 12
---

# Agent Runtime (pkg/agent library)

## Purpose [coverage: high -- 12 sources]

`pkg/agent` is the runtime library that backs the `cmd/agent` binary running on
every ephemeral EC2 runner host. It bundles the building blocks the entry-point
binary composes to take a booted instance from "blank" to "GitHub Actions job
executed, diagnosed, and reported." The package owns these concerns:

- Downloading or locating the GitHub Actions runner tarball (`downloader.go`).
- Registering the runner with GitHub and managing its `.env`
  (`registration.go`) — now a **no-op when a JIT config is present**.
- Spawning `run.sh`, streaming its output, and observing exit (`executor.go`).
- Watching wall-clock runtime, disk, and memory while the job runs
  (`safety.go`).
- Reporting `started` / `success` / `failure` / `timeout` / `interrupted` over
  SQS, including the bootstrap-timing decomposition (`telemetry.go`).
- Self-terminating the host once the job is done or the runtime ceiling is hit
  (`termination.go`).
- Removing runner artifacts post-job (`cleanup.go`).
- Snapshotting the Actions tool cache to report on-demand tool downloads
  (`toolcache.go`).
- Wiring the transparent Docker layer cache into the runner `.env` and reading
  back its outcome (`buildkitcache.go`) — see
  [build-caching](build-caching.md).
- **Shipping the runner's `_diag` logs to S3 before cleanup wipes them**
  (`logship/logship.go`, PR #445).
- Transparently intercepting the runner's v2 Actions cache traffic
  (`cacheproxy/`, with `tlsca/` for the per-instance CA).

`logging.go` (the `CloudWatchLogger`) **was deleted** in PR #446: the instance
role grants no `logs:*` action, so the agent had been retrying `PutLogEvents`
into `AccessDenied` since it was added and neither `/runs-fleet/runner` nor
`/runner/system` was ever created. Runner output now has exactly one off-host
route: `logship` to S3.

The library exposes interfaces (`TelemetryClient`, `InstanceTerminator`,
`Logger`, `logship.PutObjectAPI`) so `cmd/agent` and tests slot implementations
in behind the same call sites.

## Architecture [coverage: high -- 12 sources]

| Type | File | Role |
|------|------|------|
| `Downloader` | `downloader.go` | Locate pre-baked runner (`/opt/actions-runner`, `/home/runner`) or download `actions-runner-linux-<arch>-<version>.tar.gz` |
| `Registrar` | `registration.go` | Run `config.sh` (repo-scoped, ephemeral, `--replace`) unless a JIT config makes it unnecessary; write/append the runner `.env`; write the buildkit-cache env |
| `Executor` | `executor.go` | Run `run.sh` under a process group, pipe stdout/stderr, handle signals, pass the JIT config via env |
| `SafetyMonitor` | `safety.go` | Periodically check elapsed runtime, disk, memory; fire a *latched* timeout callback |
| `SQSTelemetry` | `telemetry.go` | Implements `TelemetryClient` over AWS SQS |
| `EC2Terminator` | `termination.go` | Implements `InstanceTerminator`; sends final telemetry then calls `ec2:TerminateInstances` |
| `Terminator` (alias) | `termination.go` | Deprecated alias for `EC2Terminator` |
| `Cleanup` | `cleanup.go` | Remove `_work/`, `_diag/`, then the runner dir |
| `SnapshotToolCache` / `DiffToolCache` | `toolcache.go` | Before/after tool-cache diff → `ToolCacheMisses` telemetry |
| `Registrar.WriteBuildkitCacheEnv` / `ReadBuildCacheOutcome` | `buildkitcache.go` | Plumb the buildx shim's S3 cache config and read back its coarse outcome |
| `logship.Shipper` | `logship/logship.go` | Gzip + `PutObject` each `_diag` `Worker_*.log` / `Runner_*.log` to S3 |
| `cacheproxy.Proxy` | `cacheproxy/` | Local TLS-intercepting proxy redirecting Actions cache traffic to the orchestrator |

`cmd/agent/main.go` composes them as: `initStore` → **standby poll** →
`completeInit` → `DownloadRunner` → `RegisterRunner` + `SetRunnerEnvironment` +
`WriteBuildkitCacheEnv` + `engageCache` → `SendJobStarted` (carrying the
`Bootstrap*Seconds` fields) → tool-cache snapshot → `NewSafetyMonitor` +
`ExecuteJobWithConfig` → **`shipRunnerLogs`** → `CleanupRunner` → second
tool-cache snapshot → `TerminateInstance` (which also sends the completion
event). See [cmd-agent](cmd-agent.md) for the phase-by-phase detail.

### Runner-log shipping (`logship`, PR #445 / `01c048b`)

The package doc states the problem directly: GitHub expires a job's own logs,
and a *superseded attempt* returns `BlobNotFound` within hours — so a failed or
re-run job becomes undiagnosable. `Shipper.Ship(ctx, runnerPath)`:

1. returns `OutcomeDisabled` immediately when `Config.Bucket` is empty;
2. `diagLogs` globs `<runnerPath>/_diag/Worker_*.log` and `Runner_*.log`;
   a missing `_diag` or zero matches → `OutcomeSkipped`;
3. applies `Config.Timeout` to the whole batch;
4. per file: `os.Stat`, skip if over `MaxFileBytes`, gzip in memory, then
   `PutObject` with `ContentEncoding: gzip`, `ServerSideEncryption: AES256`, and
   `repo` / `job-id` / `run-id` object metadata;
5. aggregates: zero uploads → `OutcomeFailed`; any failure or skip →
   `OutcomePartial`; otherwise `OutcomeUploaded`.

**`Ship` never returns an error** — only an outcome string. The doc comment
gives the reason: a failed upload must not fail the job, and a slow one must not
delay self-termination.

Key layout is a single definition shared with readers:

```
BuildPrefix(prefix, runID, jobID) = prefix + runID + "/" + jobID + "/"   (or prefix + runID + "/" when jobID == "")
BuildKey(prefix, runID, jobID, instanceID, name) = BuildPrefix(...) + instanceID + "/" + name
```

`DefaultPrefix` is `"runner-logs/"`, chosen to sit beside the Actions cache's
`caches/` and the buildx shim's `buildkit/` in the same bucket. An empty
`JobID` becomes the literal segment `unknown-job` so the run-level prefix still
lists the object. `cmd/agent` supplies `Timeout = 60s` (`logShipTimeout` — it
runs while the instance is still billable and self-termination is waiting) and
`MaxFileBytes = 128 << 20` (`logShipMaxFileBytes` — skip a pathological log
rather than spending the whole timeout compressing it).

The outcome rides the completion telemetry as `JobStatus.LogUpload`, which
[pkg/termination/handler.go](../../pkg/termination/handler.go) turns into
`PublishRunnerLogUpload` — described in that file as "the only signal that a
fleet missing `s3:PutObject` is discarding every job's logs."

### Tool-cache miss telemetry

`SnapshotToolCache(dir)` walks the tool-cache dir and keys on the
**`<platform>.complete` markers** the Actions tool cache writes when an install
finishes — not the version directories — so a partial or aborted download is
never counted. It accepts only paths with **exactly two separators**, producing
keys of the form `<Tool>/<version>/<platform>`. A root-level `os.Stat` failure
is returned as an error; per-entry errors are skipped so one bad entry can't
abort the walk.

`DiffToolCache(before, after)` returns the sorted set difference and truncates
to `maxToolCacheMisses = 50` — "real jobs install a handful of versions; the cap
bounds the SQS payload and the metric cardinality against a job that writes many
bogus `.complete` markers into the shared cache dir."

The **agent ships the full key**; normalization happens orchestrator-side in
`parseToolCacheMiss` ([pkg/termination/handler.go:721](../../pkg/termination/handler.go)):
`strings.Split` on `/`, require exactly 3 non-empty parts (an exact
accept/reject, since the agent only emits two-separator keys), then

- **arch** = the platform segment with a `linux-` / `darwin-` / `windows-`
  prefix stripped, because some `setup-*` actions write `linux-x64` where most
  write `x64`;
- **version** normalized to **major.minor** with any build suffix cut at the
  first `-` or `+`: `3.10.14` → `3.10`, `21.0.4-7` → `21.0`.

So the metric dimensions are `(tool, major.minor, arch)`.

### Buildkit-cache plumbing

`Registrar.WriteBuildkitCacheEnv(runnerPath, cfg)` appends
`RUNS_FLEET_BUILDKIT_CACHE_{BUCKET,REGION,PREFIX,OUTCOME}` to the runner `.env`
and returns the outcome-file path (`<runnerPath>/_rf_buildkit_cache_outcome`),
or `""` when `cfg.BuildkitCacheBucket` is empty *or any append fails*.
`ReadBuildCacheOutcome(path)` reads the **last non-empty line** and keeps only
the prefix before a `:` — the shim writes finer detail after the colon (e.g.
`skipped:no-cache-builder`) and only the coarse token
(`engaged`/`skipped`/`failed`/`disabled`) becomes a metric dimension. An empty
path or unreadable file reports `disabled` (the shim was never invoked, i.e. no
`docker build` ran). See [build-caching](build-caching.md).

### Execution and the JIT path

`ExecuteJob` is now a thin wrapper over `ExecuteJobWithConfig(ctx, runnerPath,
jitConfig)`. A non-empty JIT config is passed **in the child environment** as
`ACTIONS_RUNNER_INPUT_JITCONFIG` (the form GitHub's own actions-runner-controller
uses), explicitly preferred over a `--jitconfig <blob>` CLI arg because argv is
world-readable through `/proc/<pid>/cmdline`. An empty config runs `run.sh`
unchanged — the pre-existing token-registration path.

Correspondingly, `Registrar.RegisterRunner` **returns early** when
`config.JITConfig != ""`: GitHub created the registration when it minted the
config, so `config.sh` would fail (no token) and, with `--replace`, could
disturb the JIT registration the runner is about to use.

## Talks To [coverage: high -- 12 sources]

- **GitHub releases API** —
  `https://api.github.com/repos/actions/runner/releases/latest`
  (`Downloader.fetchLatestRelease`), then `Asset.BrowserDownloadURL` for the
  tarball. Skipped entirely when a pre-baked runner is found.
- **GitHub registration endpoint** — via the runner's own `config.sh`
  (`Registrar.RegisterRunner`), only on the token path.
- **AWS SQS** — `sqs:SendMessage` for telemetry; consumed by
  [pkg/termination](../../pkg/termination)'s handler, which publishes the
  bootstrap segments as `AgentBootstrapSeconds` and routes job outcomes. See
  [events-and-termination](events-and-termination.md).
- **AWS EC2** — `ec2:TerminateInstances` for host self-termination.
- **AWS S3** — `s3:PutObject` under `<RunnerLogsBucket>/<RunnerLogsPrefix>` for
  `_diag` logs (`logship`). A fleet whose instance role lacks `s3:PutObject`
  silently discards every job's logs; the `LogUpload` metric is the only
  signal.
- **Local filesystem** — `/opt/actions-runner` (AMI pre-bake), `/home/runner`
  (Docker pre-bake), `/opt/hostedtoolcache` (tool-cache snapshots),
  `/proc/meminfo`, `/` (statfs), `<runnerPath>/_diag`.
- **`tar`** — invoked via `os/exec` to extract the runner tarball.
- **`run.sh` / `config.sh`** — GitHub Actions runner entry scripts, executed
  with `RUNNER_ALLOW_RUNASROOT=1`.
- **Orchestrator cache endpoint** — `cacheproxy.Proxy` forwards intercepted
  Actions cache calls to `RunnerConfig.CacheURL`. See
  [cache-service](cache-service.md).
- **`/usr/local/sbin/runs-fleet-cache-engage`** — the AMI-baked root helper
  `cacheproxy.EngageCacheTrustAndPin` / `DisengageCache` shell out to.
- **[pkg/secrets](../../pkg/secrets)** — `secrets.Store` and
  `secrets.RunnerConfig` supply everything job-specific.
- **CloudWatch Logs** — **no longer used** (PR #446).

## API Surface [coverage: high -- 8 sources]

```go
// downloader.go
type Downloader struct {
    HTTPClient *http.Client
    // unexported: prebakedPaths, skipPrebakedCheck, releasesURL
}
func NewDownloader() *Downloader
func (d *Downloader) DownloadRunner(ctx context.Context) (string, error)
type Release struct { TagName string; Assets []Asset }
type Asset   struct { Name, BrowserDownloadURL string }
func VerifyChecksum(filePath, expectedChecksum string) error

// registration.go
type Logger interface {
    Printf(format string, v ...interface{})
    Println(v ...interface{})
}
type Registrar struct { /* secretsStore, logger */ }
func NewRegistrar(secretsStore secrets.Store, logger Logger) *Registrar
func (r *Registrar) FetchConfig(ctx context.Context, runnerID string) (*secrets.RunnerConfig, error)
func (r *Registrar) RegisterRunner(ctx context.Context, config *secrets.RunnerConfig, runnerPath string) error
func (r *Registrar) SetRunnerEnvironment(runnerPath, cacheURL, cacheToken string) error
func (r *Registrar) AppendRunnerEnv(runnerPath, key, value string) error

// buildkitcache.go
const (
    BuildCacheEngaged  = "engaged"
    BuildCacheSkipped  = "skipped"
    BuildCacheFailed   = "failed"
    BuildCacheDisabled = "disabled"
)
func (r *Registrar) WriteBuildkitCacheEnv(runnerPath string, cfg *secrets.RunnerConfig) string
func ReadBuildCacheOutcome(outcomeFile string) string

// executor.go
type JobResult struct {
    ExitCode      int
    StartedAt     time.Time
    CompletedAt   time.Time
    Duration      time.Duration
    InterruptedBy string
    Error         error
}
type Executor struct { /* logger, safetyMonitor */ }
func NewExecutor(logger Logger, safety *SafetyMonitor) *Executor
func (e *Executor) ExecuteJob(ctx context.Context, runnerPath string) (*JobResult, error)
func (e *Executor) ExecuteJobWithConfig(ctx context.Context, runnerPath, jitConfig string) (*JobResult, error)
const GracefulShutdownTimeout = 90 * time.Second

// safety.go
type SafetyMonitor struct { /* ... */ }
func NewSafetyMonitor(maxRuntime time.Duration, logger Logger) *SafetyMonitor
func (s *SafetyMonitor) SetTimeoutCallback(cb func())
func (s *SafetyMonitor) SetCheckCallback(cb func())
func (s *SafetyMonitor) Monitor(ctx context.Context)
const (
    DefaultCheckInterval = 30 * time.Second
    MinDiskSpaceWarning  = 500 * 1024 * 1024
    MinMemoryWarning     = 200 * 1024 * 1024
)

// telemetry.go
type TelemetryClient interface {
    SendJobStarted(ctx context.Context, status JobStatus) error
    SendJobCompleted(ctx context.Context, status JobStatus) error
    SendJobTimeout(ctx context.Context, status JobStatus) error
}
type TelemetrySQSAPI interface {
    SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}
type JobStatus struct { /* see Data */ }
type SQSTelemetry struct { /* ... */ }
func NewSQSTelemetry(cfg aws.Config, queueURL string, logger Logger) *SQSTelemetry
func (t *SQSTelemetry) SendJobStarted(ctx context.Context, status JobStatus) error
func (t *SQSTelemetry) SendJobCompleted(ctx context.Context, status JobStatus) error
func (t *SQSTelemetry) SendJobTimeout(ctx context.Context, status JobStatus) error
func (t *SQSTelemetry) SendJobFailure(ctx context.Context, status JobStatus) error
func (t *SQSTelemetry) SendWithTimeout(status JobStatus, timeout time.Duration) error
func DetermineCompletionStatus(interruptedBy string, exitCode int) string
const (
    StatusStarted, StatusSuccess, StatusFailure,
    StatusTimeout, StatusInterrupted = "started", "success", "failure", "timeout", "interrupted"
)

// termination.go
type InstanceTerminator interface {
    TerminateInstance(ctx context.Context, instanceID string, status JobStatus) error
    TerminateOnTimeout(ctx context.Context, instanceID string, status JobStatus) error
}
type EC2API interface {
    TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}
type EC2Terminator struct { /* ... */ }
type Terminator = EC2Terminator // deprecated alias
func NewEC2Terminator(cfg aws.Config, telemetry TelemetryClient, logger Logger) *EC2Terminator
func (t *EC2Terminator) TerminateInstance(ctx context.Context, instanceID string, status JobStatus) error
func (t *EC2Terminator) TerminateOnTimeout(ctx context.Context, instanceID string, status JobStatus) error
func (t *EC2Terminator) TerminateWithStatus(instanceID, status string, exitCode int, duration time.Duration, errorMsg string) error
func (t *EC2Terminator) TerminateOnPanic(instanceID, jobID string, panicValue interface{})
const TelemetryTimeout = 30 * time.Second

// cleanup.go
type Cleanup struct { /* ... */ }
func NewCleanup(logger Logger) *Cleanup
func (c *Cleanup) CleanupRunner(ctx context.Context, runnerPath string) error
func (c *Cleanup) CleanupTempFiles(ctx context.Context) error
func (c *Cleanup) CleanupLogs(ctx context.Context, runnerPath string, _ int) error

// toolcache.go
const DefaultToolCacheDir = "/opt/hostedtoolcache"
func SnapshotToolCache(dir string) (map[string]struct{}, error)
func DiffToolCache(before, after map[string]struct{}) []string
```

```go
// logship/logship.go
const (
    OutcomeUploaded = "uploaded"
    OutcomePartial  = "partial"
    OutcomeFailed   = "failed"
    OutcomeSkipped  = "skipped"
    OutcomeDisabled = "disabled"
)
const DefaultPrefix = "runner-logs/"

type PutObjectAPI interface {
    PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}
type Logger interface { Printf(format string, v ...any) }

type Config struct {
    Bucket       string // empty disables shipping entirely
    Prefix       string
    RunID        string
    JobID        string
    InstanceID   string
    Repo         string
    MaxFileBytes int64
    Timeout      time.Duration
}

type Shipper struct { /* s3, cfg, logger */ }
func New(awsCfg aws.Config, cfg Config, logger Logger) *Shipper
func NewWithClient(client PutObjectAPI, cfg Config, logger Logger) *Shipper
func (s *Shipper) Ship(ctx context.Context, runnerPath string) string // never errors
func BuildKey(prefix, runID, jobID, instanceID, name string) string
func BuildPrefix(prefix, runID, jobID string) string
```

Both the `Telemetry = SQSTelemetry` alias and `agent.CloudWatchLogger` /
`Executor.SetCloudWatchLogger` are gone; only the `Terminator` alias remains.

## Data [coverage: high -- 7 sources]

**`JobStatus`** is the single telemetry envelope. On-wire shape:

```json
{
  "instance_id": "i-0abc...",
  "job_id": "12345678",
  "status": "started|success|failure|timeout|interrupted",
  "exit_code": 0,
  "duration_seconds": 42,
  "started_at": "2026-08-21T10:00:00Z",
  "completed_at": "2026-08-21T10:00:42Z",
  "error": "...",
  "interrupted_by": "SIGTERM",
  "tool_cache_misses": ["Python/3.12.4/arm64"],
  "cache_interception": "engaged|failed|disabled",
  "build_cache_interception": "engaged|skipped|failed|disabled",
  "log_upload": "uploaded|partial|failed|skipped|disabled",
  "cache_bytes_written": 1048576,
  "bootstrap_boot_seconds": 21.4,
  "bootstrap_config_seconds": 0.9,
  "bootstrap_runner_seconds": 0.1,
  "bootstrap_register_seconds": 3.2,
  "bootstrap_total_seconds": 4.6
}
```

Everything from `error` down is `omitempty`. The five `bootstrap_*_seconds`
fields (PR #387) ride the `started` message; `tool_cache_misses`,
`cache_interception`, `build_cache_interception`, `log_upload`, and
`cache_bytes_written` ride the completion message. `log_upload` and
`build_cache_interception` follow the same discipline as the bootstrap fields —
absent from a pre-rollout agent, which the orchestrator treats as *no
measurement*, not as a zero.

**`JobResult`** is the in-process output of `Executor.ExecuteJobWithConfig` (not
serialized). It feeds `JobStatus` via `Duration.Seconds()`, `ExitCode`, and
`InterruptedBy`.

**`RunnerConfig`** comes from [pkg/secrets](../../pkg/secrets); fields this
package touches: `Repo`, `RunID`, `JobID`, `RegistrationToken` (json tag
`jit_token` — a deliberately preserved misnomer, kept as a wire contract with
in-flight agents), `JITConfig`, `RunnerName`, `Labels`, `RunnerGroup`,
`CacheToken`, `CacheURL`, `TerminationQueueURL`, `BuildkitCache{Bucket,Region,Prefix}`,
`RunnerLogs{Bucket,Prefix}`.

**Local file paths:**

- `/opt/actions-runner` — AMI-prebaked runner directory (`runnerDir`).
- `/home/runner` — Docker-prebaked runner directory (`prebakedRunnerDir`).
- `<runnerDir>/bin/Runner.Listener` — sentinel binary `isValidRunnerDir` checks
  (must be a non-directory with the owner-execute bit set).
- `<runnerPath>/run.sh` — invoked with `cmd.Dir = runnerPath`.
- `<runnerPath>/.env` — written by `SetRunnerEnvironment`, appended by
  `AppendRunnerEnv` (`NODE_EXTRA_CA_CERTS`) and `WriteBuildkitCacheEnv`.
- `<runnerPath>/_rf_buildkit_cache_outcome` — the buildx shim's append-only
  outcome log.
- `<runnerPath>/_rf_cache_staging`, `<runnerPath>/runs-fleet-cache-ca.pem` —
  cache-interceptor artifacts.
- `<runnerPath>/_diag/Worker_*.log`, `Runner_*.log` — the only files `logship`
  uploads.
- `/opt/hostedtoolcache/<Tool>/<version>/<platform>.complete` — completion
  markers keyed by `SnapshotToolCache`.
- `/proc/meminfo`, `/` — read by `SafetyMonitor`.

**S3 object layout** (`logship`):
`runner-logs/<run_id>/<job_id|unknown-job>/<instance_id>/<Worker_*.log>.gz`,
gzip-encoded, AES256 SSE, with `repo` / `job-id` / `run-id` user metadata.

## Key Decisions [coverage: high -- 12 sources]

- **PR #445: runner logs go to S3, keyed so a reader can find them from a GitHub
  job URL.** The problem is not that logs are missing at the time of failure —
  it's that GitHub expires them, and a *superseded* attempt returns
  `BlobNotFound` within hours, which is exactly the attempt you need. `BuildKey`
  / `BuildPrefix` are documented as "the single definition of the layout"
  precisely because readers derive the same key independently.
- **PR #446: one off-host log route, not a dormant second one.** The
  `CloudWatchLogger` path was gated on `RUNS_FLEET_LOG_GROUP`, unset everywhere,
  and the instance role had no `logs:*` grant to make it work. Deleting it also
  removed the AMI's CloudWatch-logs collection from both Packer layers (metrics
  collection stayed).
- **`Ship` returns an outcome string, never an error.** Two constraints
  compose: a failed upload must not fail the job, and a slow one must not delay
  self-termination while the instance bills. Hence also the 60s
  `logShipTimeout` and the 128 MiB per-file cap.
- **Logs ship *before* cleanup.** `shipRunnerLogs` is called before
  `cleanup.CleanupRunner`, which removes `_diag` — the ordering is load-bearing
  and noted on both `Ship` and `shipRunnerLogs`.
- **The `LogUpload` metric exists for a permission failure mode.** Its comment
  in `pkg/termination/handler.go` names it: "the only signal that a fleet
  missing `s3:PutObject` is discarding every job's logs."
- **Tool-cache misses key on `.complete` markers, not version directories**, so
  a partial or aborted download is never reported as a miss. Snapshot keys
  require exactly two path separators, which makes `parseToolCacheMiss`'s
  3-part split an exact accept/reject rather than a heuristic.
- **Version normalization to major.minor happens orchestrator-side, not in the
  agent.** The agent ships the full `<Tool>/<version>/<platform>` key; the
  handler bounds metric cardinality by cutting at the first `-`/`+` and keeping
  two version segments. That split keeps the agent's payload informative while
  the metric stays low-cardinality — and it means the *cache keys the setup-\*
  actions actually request* (exact patches included) must be read from the
  action's own source, not inferred from this metric.
- **JIT config travels in the environment, never argv.** `argv` is
  world-readable through `/proc/<pid>/cmdline`; the env var
  `ACTIONS_RUNNER_INPUT_JITCONFIG` is the form GitHub's own
  actions-runner-controller uses.
- **A JIT config makes `RegisterRunner` a no-op.** Running `config.sh` anyway
  would fail (no token) and, with `--replace`, could disturb the JIT
  registration the runner is about to use. A JIT registration is bound by GitHub
  to a single job, so the runner cannot be handed a different queued job that
  merely shares its labels — this is the fix for the 13% runner-theft rate
  (PR #424).
- **`TerminateOnTimeout` is a distinct path from `TerminateInstance`.**
  `SendJobCompleted` recomputes the status from the exit code and would collapse
  a runtime-ceiling breach into a generic operational failure, so timeout gets
  its own send.
- **The safety timeout is latched.** `SafetyMonitor.check` sets `s.timedOut`
  before invoking the callback because termination is asynchronous — without the
  latch it would re-issue termination every 30s for as long as the instance
  lives.
- **Pre-baked first, download fallback.** `DownloadRunner` checks
  `/opt/actions-runner` and `/home/runner` for a valid `Runner.Listener` before
  hitting the GitHub releases API, so a baked AMI or runner image incurs zero
  network cost — and a near-zero `bootstrap_runner_seconds`.
- **Bootstrap segments as five additive `omitempty` fields, not a phase map
  (PR #387).** The enum is closed at compile time on both ends, which bounds
  `AgentBootstrapSeconds` cardinality against arbitrary agent-supplied keys.
  `bootstrap_total_seconds` is sent explicitly rather than derived, so the gap
  between the sum of parts and the total (untimed work) stays observable.
- **Operational status, not workflow result.** `DetermineCompletionStatus` maps
  `run.sh` outcomes to *our* runner's health: `interrupted_by` set →
  `interrupted`; exit 0 → `success` **even when the workflow's steps failed**
  (the ephemeral runner's `run.sh` exits 0 whenever it operated correctly,
  because `ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED` is not set); non-zero →
  `failure` (operational error). The termination handler maps `success` to
  "served" and `failure` to "error".
- **Repo-scoped registration only.** `RegisterRunner` refuses to run without
  `config.Repo` — org-level registration would let runners pick up jobs from any
  repo in the org.
- **Process group + signal forwarding.** `Executor` sets `Setpgid: true` and
  forwards SIGTERM/SIGINT to the whole group, so child Actions steps die with
  the runner. Graceful shutdown waits up to `GracefulShutdownTimeout` (90s)
  before SIGKILL.
- **Telemetry retry, terminate regardless.** SQS sends use up to 3 attempts
  with `1<<attempt * telemetryRetryBaseDelay` backoff.
  `EC2Terminator.terminate` logs a warning on telemetry failure but proceeds
  with `TerminateInstances` anyway — the host must die.
- **Telemetry-then-terminate ordering.** Send first (capped by
  `TelemetryTimeout`, 30s), sleep `ec2TerminationDelay` (2s) to let the message
  flush, then call EC2.
- **Panic-safe shutdown.** `TerminateOnPanic` constructs a `failure` `JobStatus`
  with the recovered value and drives termination from `cmd/agent`'s
  `defer recover()`.
- **Best-effort everywhere on the way out.** Every `Close`/`Remove` is wrapped
  in `_ = …` or logged-and-continued; the agent prefers shutting down dirty over
  hanging.
- **Retry base delays are package-level `var`s, not consts**
  (`registrationRetryBaseDelay`, `telemetryRetryBaseDelay`, `ec2TerminationDelay`),
  overridden in tests — the network-bound seam pattern this repo uses where
  `synctest` doesn't apply.

## Gotchas [coverage: high -- 12 sources]

- **`logship` uploads only two glob patterns.** `Worker_*.log` and
  `Runner_*.log` under `_diag`. Anything else the runner writes there (crash
  dumps, `Runner_*.log.1` rotations that don't match, job-step artifacts under
  `_work`) is not shipped, so "logs are in S3" does not mean "everything is in
  S3".
- **A whole-file gzip happens in memory.** `gzipFile` builds a
  `bytes.Buffer` per file, so the 128 MiB `MaxFileBytes` cap is also the memory
  ceiling per upload — and a file over the cap is *skipped*, downgrading the
  batch to `partial`.
- **`OutcomeSkipped` covers two very different cases.** No `_diag` directory at
  all (a job that never started a worker) and a present-but-empty glob both
  report `skipped`, indistinguishable in the metric from an over-cap file
  (which reports `partial`).
- **`unknown-job` collides across jobs of the same run.** When the runner config
  carried no `job_id`, every such job in a run writes under
  `runner-logs/<run_id>/unknown-job/<instance_id>/`. The instance-ID segment is
  what keeps the objects distinct.
- **Rollout skew is safe but lossy.** During an AMI rollout, old agents omit
  `log_upload`, `build_cache_interception`, and the `bootstrap_*_seconds`
  fields; old orchestrators ignore them. No failures either way, but dashboards
  see a mixed population — and a segment reported as exactly 0 is
  indistinguishable from "not measured" (the handler publishes only positive
  bootstrap values).
- **This package ships via the AMI cascade.** `pkg/agent` is compiled into the
  agent binary Packer extracts from the ECR image; changes here reach runners
  only after Build Runner Image → Build AMIs, not on orchestrator deploy. See
  [cmd-agent](cmd-agent.md) and [infrastructure](infrastructure.md).
- **GitHub asset-name version pinning.** `assetName` is built as
  `actions-runner-linux-<x64|arm64>-<TagName[1:]>.tar.gz`. The `[1:]` slice
  assumes `TagName` always starts with `v`; a tag without that prefix would
  yield a malformed name.
- **No checksum on the download path.** `VerifyChecksum` is exported but
  `DownloadRunner` never calls it — the tarball is extracted unchecked.
- **No download retry.** `downloadFile` performs one HTTP GET. A flaky 5xx
  aborts the boot; the termination handler then requeues the job as
  `bootstrap_failed` (bounded).
- **`tar` shells out.** Extraction uses `exec.Command("tar", …)`; the host image
  must ship a compatible `tar`.
- **Memory check is Linux-only.** `checkMemory` opens `/proc/meminfo`; elsewhere
  it logs once via `memoryCheckWarningOnce` and skips. `checkDiskSpace` uses
  `syscall.Statfs`, also Unix-only. Neither failure ever fails the job — they
  only log at `SAFETY:`.
- **`memoryCheckWarningOnce` is a package-level `sync.Once`.** The warning fires
  once per *process*, not per monitor, which matters only in tests running
  several monitors.
- **`WriteBuildkitCacheEnv` returns `""` on a *partial* write.** If the first
  `AppendRunnerEnv` succeeds and the second fails, the `.env` keeps the
  half-written vars but the agent reports `disabled` — the outcome file path is
  lost, so the shim's real outcome is never read back.
- **`ReadBuildCacheOutcome` cannot distinguish "shim never ran" from "outcome
  file unreadable"**; both report `disabled`.
- **`run.sh` env minimalism.** `Executor` adds only `RUNNER_ALLOW_RUNASROOT=1`
  (plus the JIT config when present) on top of `os.Environ()`; anything else the
  runner needs must already be in the parent env or the `.env` file — which
  `run.sh` reads at startup, so every `.env` writer must run before
  `ExecuteJobWithConfig`.
- **Context cancellation kills the runner.** If the parent `ctx` is cancelled
  mid-job, `ExecuteJobWithConfig` SIGTERMs the process group, waits up to 90s,
  then SIGKILLs; `JobResult.Error` is `ctx.Err()` and `InterruptedBy` is
  `"context_cancelled"`, not the runner's exit.
- **Signal handling is global.** `signal.Notify` is called inside
  `ExecuteJobWithConfig`; two concurrent executors in one process would race on
  signals (not the intended use). Note this is *separate* from `cmd/agent`'s
  standby-scoped `signal.NotifyContext`, which is deliberately stopped before
  the job runs.
- **Tool-cache diff needs both snapshots.** A failed pre-job snapshot leaves an
  empty baseline that would mis-report every pre-baked tool as a miss;
  `cmd/agent` therefore diffs only when both snapshots succeeded.
- **The 50-miss cap silently truncates.** `DiffToolCache` sorts then slices, so
  a job installing more than 50 entries reports only the alphabetically first
  50 — the cap is a cardinality guard, not a sample.
- **Partial cleanup states.** `DownloadRunner` removes the tarball after
  extraction but does not roll back `runnerDir` on a failed extract; a retry
  would extract over the partial tree.
- **`Cleanup.CleanupLogs` deletes every `.log` under `_diag`** and is unrelated
  to `logship`. It is not on `cmd/agent`'s path (which calls only
  `CleanupRunner`), but calling it before shipping would destroy exactly the
  files `logship` uploads.

## Sources [coverage: high]

- [pkg/agent/downloader.go](../../pkg/agent/downloader.go)
- [pkg/agent/registration.go](../../pkg/agent/registration.go)
- [pkg/agent/executor.go](../../pkg/agent/executor.go)
- [pkg/agent/safety.go](../../pkg/agent/safety.go)
- [pkg/agent/telemetry.go](../../pkg/agent/telemetry.go)
- [pkg/agent/termination.go](../../pkg/agent/termination.go)
- [pkg/agent/cleanup.go](../../pkg/agent/cleanup.go)
- [pkg/agent/toolcache.go](../../pkg/agent/toolcache.go)
- [pkg/agent/buildkitcache.go](../../pkg/agent/buildkitcache.go)
- [pkg/agent/logship/logship.go](../../pkg/agent/logship/logship.go)
- [pkg/termination/handler.go](../../pkg/termination/handler.go)
- [cmd/agent/main.go](../../cmd/agent/main.go)
