---
topic: Agent Runtime (pkg/agent library)
last_compiled: 2026-07-09
sources_count: 10
---

# Agent Runtime (pkg/agent library)

## Purpose [coverage: high -- 10 sources]

`pkg/agent` is the runtime library that backs the `cmd/agent` binary running on every ephemeral EC2 runner host. It bundles the building blocks that the entry-point binary composes to take a freshly-booted instance from "blank" to "GitHub Actions job executed and reported." (The K8s/Valkey variants — `config.go`, `NewValkeyTelemetry`, `K8sTerminator` — were removed with the K8s runner backend in 2026-06.) The package owns these concerns:

- Downloading or locating the GitHub Actions runner tarball (`downloader.go`).
- Registering the runner with GitHub and managing its `.env` (`registration.go`).
- Spawning `run.sh`, streaming its output, and observing exit (`executor.go`).
- Watching wall-clock runtime, disk, and memory while the job runs (`safety.go`).
- Reporting `started` / `success` / `failure` / `interrupted` over SQS, including the bootstrap-timing decomposition (`telemetry.go`).
- Self-terminating the host once the job is done (`termination.go`).
- Removing runner artifacts post-job (`cleanup.go`).
- Snapshotting the Actions tool cache to report on-demand tool downloads (`toolcache.go`).
- Streaming runner output to CloudWatch Logs (`logging.go`).
- Transparently intercepting the runner's v2 cache traffic (`cacheproxy/`, with `tlsca/` for the per-instance CA).

The library exposes interfaces (`TelemetryClient`, `InstanceTerminator`, `Logger`) so `cmd/agent` and tests slot implementations in behind the same call sites.

## Architecture [coverage: high -- 10 sources]

Major types and their responsibilities:

| Type | File | Role |
|------|------|------|
| `Downloader` | `downloader.go` | Locate pre-baked runner or download `actions-runner-linux-<arch>-<version>.tar.gz` |
| `Registrar` | `registration.go` | Run `config.sh` (repo-scoped, ephemeral, JIT token); write/append the runner `.env` |
| `Executor` | `executor.go` | Run `run.sh` under a process group, pipe stdout/stderr, handle signals |
| `SafetyMonitor` | `safety.go` | Periodically check elapsed runtime, disk, memory; trigger timeout callback |
| `SQSTelemetry` | `telemetry.go` | Implements `TelemetryClient` over AWS SQS |
| `EC2Terminator` | `termination.go` | Implements `InstanceTerminator`; sends final telemetry then calls `ec2:TerminateInstances` |
| `Terminator` (alias) | `termination.go` | Backwards-compat alias for `EC2Terminator` |
| `Cleanup` | `cleanup.go` | Remove `_work/` and runner artifacts after the job |
| `SnapshotToolCache` / `DiffToolCache` | `toolcache.go` | Before/after tool-cache diff → `ToolCacheMisses` telemetry |
| `CloudWatchLogger` | `logging.go` | Optional per-line sink for streamed runner output |
| `cacheproxy.Proxy` | `cacheproxy/` | Local TLS-intercepting proxy redirecting Actions cache traffic to the orchestrator |

`cmd/agent/main.go` composes them roughly as: fetch config (via `pkg/secrets`) → `NewDownloader().DownloadRunner` → `NewRegistrar` (`RegisterRunner`, `SetRunnerEnvironment`, cache engage) → `SendJobStarted` (carrying the `Bootstrap*Seconds` fields) → `NewSafetyMonitor` + `NewExecutor` `ExecuteJob` → `NewCleanup().CleanupRunner` → `TerminateInstance` (which also sends the completion event).

## Talks To [coverage: high -- 10 sources]

- **GitHub releases API** — `https://api.github.com/repos/actions/runner/releases/latest` (`Downloader.fetchLatestRelease`) to discover the latest tagged release and a matching asset.
- **Asset CDN** — `Asset.BrowserDownloadURL` for the tarball download.
- **GitHub registration endpoint** — via the runner's own `config.sh` (`Registrar.RegisterRunner`).
- **Local filesystem** — `/opt/actions-runner` (AMI pre-bake), `/home/runner` (Docker pre-bake), `/opt/hostedtoolcache` (tool-cache snapshots), `/proc/meminfo`, `/proc/uptime` (read by `cmd/agent`), `/` (statfs).
- **`tar`** — invoked via `os/exec` to extract the runner tarball.
- **`run.sh` / `config.sh`** — GitHub Actions runner entry scripts, executed with `RUNNER_ALLOW_RUNASROOT=1`.
- **AWS SQS** — `sqs:SendMessage` for telemetry events; consumed by `pkg/termination`'s handler, which publishes the bootstrap segments as `AgentBootstrapSeconds` (Pool, Phase) and routes job outcomes.
- **AWS EC2** — `ec2:TerminateInstances` for host self-termination.
- **CloudWatch Logs** — optional, via `CloudWatchLogger.Log` per-line sink.
- **Orchestrator cache endpoint** — `cacheproxy.Proxy` forwards intercepted Actions cache calls to `RunnerConfig.CacheURL`.

## API Surface [coverage: high -- 6 sources]

Exported identifiers with real signatures (core files):

```go
// downloader.go
type Downloader struct {
    HTTPClient  *http.Client
    // unexported: prebakedPaths, skipPrebakedCheck, releasesURL
}
func NewDownloader() *Downloader
func (d *Downloader) DownloadRunner(ctx context.Context) (string, error)
type Release struct { TagName string; Assets []Asset }
type Asset   struct { Name, BrowserDownloadURL string }
func VerifyChecksum(filePath, expectedChecksum string) error

// registration.go
type Logger interface { Printf(string, ...interface{}); Println(...interface{}) }
type Registrar struct { /* secretsStore, logger */ }
func NewRegistrar(secretsStore secrets.Store, logger Logger) *Registrar
func (r *Registrar) FetchConfig(ctx context.Context, runnerID string) (*secrets.RunnerConfig, error)
func (r *Registrar) RegisterRunner(ctx context.Context, config *secrets.RunnerConfig, runnerPath string) error
func (r *Registrar) SetRunnerEnvironment(runnerPath, cacheURL, cacheToken string) error
func (r *Registrar) AppendRunnerEnv(runnerPath, key, value string) error

// executor.go
type JobResult struct {
    ExitCode      int
    StartedAt     time.Time
    CompletedAt   time.Time
    Duration      time.Duration
    InterruptedBy string
    Error         error
}
type Executor struct { /* ... */ }
func NewExecutor(logger Logger, safety *SafetyMonitor) *Executor
func (e *Executor) SetCloudWatchLogger(cwLogger *CloudWatchLogger)
func (e *Executor) ExecuteJob(ctx context.Context, runnerPath string) (*JobResult, error)
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
}
type JobStatus struct {
    InstanceID, JobID, Status string
    ExitCode, DurationSeconds int
    StartedAt, CompletedAt    time.Time
    Error, InterruptedBy      string
    ToolCacheMisses           []string
    CacheInterception         string
    CacheBytesWritten         int64
    BootstrapBootSeconds      float64
    BootstrapConfigSeconds    float64
    BootstrapRunnerSeconds    float64
    BootstrapRegisterSeconds  float64
    BootstrapTotalSeconds     float64
}
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
}
type EC2Terminator struct { /* ... */ }
type Terminator = EC2Terminator // deprecated alias
func NewEC2Terminator(cfg aws.Config, telemetry TelemetryClient, logger Logger) *EC2Terminator
func (t *EC2Terminator) TerminateInstance(ctx context.Context, instanceID string, status JobStatus) error
func (t *EC2Terminator) TerminateWithStatus(instanceID, status string, exitCode int, duration time.Duration, errorMsg string) error
func (t *EC2Terminator) TerminateOnPanic(instanceID, jobID string, panicValue interface{})
const TelemetryTimeout = 30 * time.Second

// cleanup.go
type Cleanup struct { /* ... */ }
func NewCleanup(logger Logger) *Cleanup
func (c *Cleanup) CleanupRunner(ctx context.Context, runnerPath string) error

// toolcache.go
const DefaultToolCacheDir = "/opt/hostedtoolcache"
func SnapshotToolCache(dir string) (map[string]struct{}, error)
func DiffToolCache(before, after map[string]struct{}) []string
```

The `Telemetry = SQSTelemetry` alias that existed in earlier revisions is gone; only the `Terminator` alias remains.

## Data [coverage: high -- 6 sources]

**`JobStatus`** is the single envelope used for telemetry. Field tags show on-wire shape:

```json
{
  "instance_id": "i-0abc...",
  "job_id": "12345678",
  "status": "started|success|failure|timeout|interrupted",
  "exit_code": 0,
  "duration_seconds": 42,
  "started_at": "2026-07-09T10:00:00Z",
  "completed_at": "2026-07-09T10:00:42Z",
  "error": "...",
  "interrupted_by": "SIGTERM",
  "tool_cache_misses": ["Python/3.12.4/arm64"],
  "cache_interception": "engaged|failed|disabled",
  "cache_bytes_written": 1048576,
  "bootstrap_boot_seconds": 21.4,
  "bootstrap_config_seconds": 0.9,
  "bootstrap_runner_seconds": 0.1,
  "bootstrap_register_seconds": 3.2,
  "bootstrap_total_seconds": 4.6
}
```

All fields from `error` down are `omitempty`. The five `bootstrap_*_seconds` fields (PR #387) ride the `started` message; the tool-cache / cache-interception fields ride the completion message.

**`JobResult`** is the in-process output of `Executor.ExecuteJob` (not serialized). It feeds `JobStatus` via `Duration.Seconds()` and `InterruptedBy`.

**`RunnerConfig`** comes from `pkg/secrets`; fields touched here: `Repo`, `RunID`, `JITToken`, `RunnerName`, `JobID`, `Labels`, `RunnerGroup`, `CacheToken`, `CacheURL`, `TerminationQueueURL`.

**Local file paths:**

- `/opt/actions-runner` — AMI-prebaked runner directory (`runnerDir`).
- `/home/runner` — Docker-prebaked runner directory (`prebakedRunnerDir`).
- `<runnerDir>/bin/Runner.Listener` — sentinel binary used by `isValidRunnerDir` to validate a pre-baked install (must be an executable file).
- `<runnerPath>/run.sh` — invoked by `Executor.ExecuteJob` with `cmd.Dir = runnerPath`.
- `<runnerPath>/.env` — written by `SetRunnerEnvironment`, appended by `AppendRunnerEnv` (e.g. `NODE_EXTRA_CA_CERTS`).
- `/opt/hostedtoolcache/<Tool>/<version>/<platform>.complete` — completion markers keyed by `SnapshotToolCache`.
- `/proc/meminfo`, `/` — read by `SafetyMonitor`.

## Key Decisions [coverage: high -- 10 sources]

- **Pre-baked first, download fallback.** `DownloadRunner` checks `/opt/actions-runner` and `/home/runner` for a valid `Runner.Listener` before hitting the GitHub releases API, so a baked AMI or runner image incurs zero network cost — and a near-zero `bootstrap_runner_seconds`.
- **Bootstrap segments as additive `omitempty` fields (PR #387).** `JobStatus` gained exactly five named float fields rather than a phase map: the enum is closed at compile time on both ends (agent field names, orchestrator's fixed phase list), which bounds `AgentBootstrapSeconds` metric cardinality against arbitrary agent-supplied keys. `bootstrap_total_seconds` is sent explicitly instead of being derived, so the gap between the sum of parts and the total (untimed work) stays observable. `omitempty` makes absence — old agent, unmeasured phase, non-Linux — indistinguishable from "no measurement", which the orchestrator treats as skip-don't-publish.
- **Operational status, not workflow result.** `DetermineCompletionStatus` maps run.sh outcomes to *our* runner's health: `interrupted_by` set → `interrupted`; exit 0 → `success` even when the workflow's steps failed (that's the client's outcome, surfaced via GitHub, not ours); non-zero → `failure` (operational error).
- **Repo-scoped registration only.** `RegisterRunner` refuses to run without `config.Repo` — org-level registration would let runners pick up jobs from any repo in the org.
- **Process group + signal forwarding.** `Executor` sets `Setpgid: true` and forwards SIGTERM/SIGINT to the whole group, so child Actions steps die with the runner. Graceful shutdown waits up to `GracefulShutdownTimeout` (90s) before SIGKILL.
- **Stream output.** Every line scanned from `stdout`/`stderr` is logged and, if `SetCloudWatchLogger` was called, also pushed to CloudWatch. Scanner buffer is bumped to 1 MiB to tolerate long log lines.
- **Per-process safety timeout.** `SafetyMonitor.Monitor` runs a ticker (`DefaultCheckInterval` = 30s) inside the executor's context and invokes `onTimeout` when elapsed > `maxRuntime`. It also warns on low disk (<500 MB) and memory (<200 MB).
- **Telemetry retry, terminate regardless.** SQS sends use up to 3 attempts with `1<<attempt * telemetryRetryBaseDelay` backoff. `EC2Terminator.TerminateInstance` logs a warning on telemetry failure but proceeds with `TerminateInstances` anyway — the host must die.
- **Telemetry-then-terminate ordering.** `EC2Terminator` sends `SendJobCompleted` first (capped by `TelemetryTimeout`, 30s), sleeps `ec2TerminationDelay` (2s) to flush, then calls EC2.
- **Panic-safe shutdown.** `TerminateOnPanic` constructs a `failure` `JobStatus` with the recovered value and triggers termination from `defer recover()` in `cmd/agent`.
- **Tool-cache misses via completion markers.** `SnapshotToolCache` keys on `<platform>.complete` markers (not version directories) so partial/aborted downloads don't count; `maxToolCacheMisses` (50) caps the SQS payload and metric cardinality.
- **Best-effort cleanup.** Every `Close`/`Remove` is wrapped in `_ = ...` or logged-and-continued; the agent prefers shutting down dirty over hanging.

## Gotchas [coverage: high -- 10 sources]

- **Rollout skew is safe but lossy.** During an AMI rollout, old agents omit the `bootstrap_*_seconds` fields and old orchestrators ignore them — no failures either way, but dashboards see a mixed population, and a segment reported as exactly 0 is skipped by the orchestrator (`publishBootstrapSeconds` publishes only positive values).
- **This package ships via the AMI cascade.** `pkg/agent` is compiled into the agent binary that Packer extracts from the ECR image; changes here reach runners only after the Build Runner Image → Build AMIs pipeline, not on orchestrator deploy (see [cmd-agent](cmd-agent.md)).
- **GitHub asset name version pinning.** `assetName` is built as `actions-runner-linux-<x64|arm64>-<TagName[1:]>.tar.gz`. The `[1:]` slice assumes `TagName` always starts with `v` (e.g., `v2.319.1`); a tag without that prefix would yield a malformed name.
- **No checksum on the download path.** `VerifyChecksum` is exported but `DownloadRunner` does not call it — the tarball is extracted unchecked. Operators relying on integrity must invoke it themselves.
- **No download retry.** `downloadFile` performs one HTTP GET via `http.DefaultClient`. A flaky network 5xx aborts the boot (the termination handler then requeues the job as `bootstrap_failed`, max 2 requeues).
- **`tar` shells out.** Extraction uses `exec.Command("tar", ...)`; the host image must ship a compatible `tar`.
- **Memory check is Linux-only.** `SafetyMonitor.checkMemory` opens `/proc/meminfo`; on macOS / non-Linux it logs a one-time warning (`memoryCheckWarningOnce`) and skips. Disk check uses `syscall.Statfs` which is also Unix-only.
- **`run.sh` env minimalism.** `Executor` sets only `RUNNER_ALLOW_RUNASROOT=1` on top of `os.Environ()`; anything the runner needs beyond the `.env` file must already be in the parent env.
- **Context cancellation kills the runner.** If the parent `ctx` is cancelled mid-job, `ExecuteJob` SIGTERMs the process group, waits up to `GracefulShutdownTimeout`, then SIGKILLs. The returned `JobResult.Error` is `ctx.Err()`, not the runner's exit.
- **Signal handler is global.** `signal.Notify` is called inside `ExecuteJob`; running multiple executors concurrently in one process would race on signals (not the intended use, but worth noting).
- **Tool-cache diff needs both snapshots.** A failed pre-job snapshot leaves an empty baseline that would mis-report every pre-baked tool as a miss; `cmd/agent` therefore diffs only when both snapshots succeeded.
- **Partial cleanup states.** `DownloadRunner` removes the tarball after extraction but does not roll back the `runnerDir` on a failed extract; a retry would extract over the partial tree.

## Sources [coverage: high]

- [`pkg/agent/downloader.go`](../../pkg/agent/downloader.go)
- [`pkg/agent/registration.go`](../../pkg/agent/registration.go)
- [`pkg/agent/executor.go`](../../pkg/agent/executor.go)
- [`pkg/agent/safety.go`](../../pkg/agent/safety.go)
- [`pkg/agent/telemetry.go`](../../pkg/agent/telemetry.go)
- [`pkg/agent/termination.go`](../../pkg/agent/termination.go)
- [`pkg/agent/cleanup.go`](../../pkg/agent/cleanup.go)
- [`pkg/agent/toolcache.go`](../../pkg/agent/toolcache.go)
- [`pkg/agent/logging.go`](../../pkg/agent/logging.go)
- [`pkg/termination/handler.go`](../../pkg/termination/handler.go)
