---
topic: Agent Runtime (pkg/agent library)
last_compiled: 2026-04-30
sources_count: 6
---

# Agent Runtime (pkg/agent library)

## Purpose [coverage: high -- 6 sources]

`pkg/agent` is the runtime library that backs the `cmd/agent` binary running on every ephemeral runner host. It bundles the building blocks that an entry-point binary composes to take a freshly-booted EC2 instance (or K8s pod) from "blank" to "GitHub Actions job executed and reported." The package owns five concerns:

- Fetching `RunnerConfig` (JIT registration token, repo, labels) from a backend (`config.go`).
- Downloading or locating the GitHub Actions runner tarball (`downloader.go`).
- Spawning `run.sh`, streaming its output, and observing exit (`executor.go`).
- Watching wall-clock runtime, disk, and memory while the job runs (`safety.go`).
- Reporting `started` / `success` / `failure` / `interrupted` over a queue (`telemetry.go`).
- Self-terminating the host once the job is done (`termination.go`).

The library exposes interfaces (`ConfigFetcher`, `TelemetryClient`, `InstanceTerminator`) so that EC2 and K8s variants slot in behind the same call sites.

## Architecture [coverage: high -- 6 sources]

Major types and their responsibilities:

| Type | File | Role |
|------|------|------|
| `FileConfigFetcher` / `FetchK8sConfig` | `config.go` | Read `RunnerConfig` from JSON file or K8s ConfigMap+Secret mount |
| `Downloader` | `downloader.go` | Locate pre-baked runner or download `actions-runner-linux-<arch>-<version>.tar.gz` |
| `Executor` | `executor.go` | Run `run.sh` under a process group, pipe stdout/stderr, handle signals |
| `SafetyMonitor` | `safety.go` | Periodically check elapsed runtime, disk, memory; trigger timeout callback |
| `CloudWatchLogger` (referenced) | `executor.go` | Optional sink for streamed runner output (per-line) |
| `SQSTelemetry` | `telemetry.go` | Implements `TelemetryClient` over AWS SQS |
| `Telemetry` (alias) | `telemetry.go` | Backwards-compat alias for `SQSTelemetry` |
| `EC2Terminator` | `termination.go` | Implements `InstanceTerminator`; sends final telemetry then calls `ec2:TerminateInstances` |
| `Terminator` (alias) | `termination.go` | Backwards-compat alias for `EC2Terminator` |

`cmd/agent/main.go` composes them roughly as: fetch config -> `NewDownloader().DownloadRunner` -> `NewSafetyMonitor` + `NewExecutor` (optionally `SetCloudWatchLogger`) -> send `SendJobStarted` -> `ExecuteJob` -> `TerminateInstance` (which also sends the completion event). The K8s variants substitute Valkey-backed telemetry and a pod-deletion terminator (referenced in `telemetry.go` doc comments — `NewValkeyTelemetry` / `NewK8sTerminator` live in sibling files not in this slice).

## Talks To [coverage: high -- 6 sources]

- **GitHub releases API** — `https://api.github.com/repos/actions/runner/releases/latest` (`Downloader.fetchLatestRelease`) to discover the latest tagged release and a matching asset.
- **Asset CDN** — `Asset.BrowserDownloadURL` for the tarball download.
- **Local filesystem** — `/opt/actions-runner` (AMI pre-bake), `/home/runner` (Docker pre-bake), `/etc/runs-fleet/config`, `/etc/runs-fleet/secrets` (K8s mounts), `/proc/meminfo`, `/` (statfs).
- **`tar`** — invoked via `os/exec` to extract the runner tarball.
- **`run.sh`** — GitHub Actions runner entry script, executed under a new process group with `RUNNER_ALLOW_RUNASROOT=1`.
- **AWS SQS** — `sqs:SendMessage` for telemetry events.
- **AWS EC2** — `ec2:TerminateInstances` for host self-termination.
- **CloudWatch Logs** — optional, via `CloudWatchLogger.Log` per-line sink.
- **K8s API / Valkey** — referenced in interface contracts (`InstanceTerminator`, `TelemetryClient`); concrete implementations live in sibling files.

## API Surface [coverage: high -- 6 sources]

Exported identifiers with real signatures:

```go
// config.go
type ConfigFetcher interface {
    FetchConfig(ctx context.Context, source string) (*secrets.RunnerConfig, error)
}
type FileConfigFetcher struct { /* ... */ }
func NewFileConfigFetcher(logger Logger) *FileConfigFetcher
func (f *FileConfigFetcher) FetchConfig(_ context.Context, source string) (*secrets.RunnerConfig, error)

type K8sConfigPaths struct { ConfigDir, SecretDir string }
func DefaultK8sConfigPaths() K8sConfigPaths
func FetchK8sConfig(_ context.Context, paths K8sConfigPaths, logger Logger) (*secrets.RunnerConfig, error)
func IsK8sEnvironment() bool

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
func (s *SafetyMonitor) GetRemainingTime() time.Duration
func (s *SafetyMonitor) IsExpired() bool
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
type TelemetrySQSAPI interface {
    SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}
type JobStatus struct {
    InstanceID, JobID, Status string
    ExitCode, DurationSeconds int
    StartedAt, CompletedAt    time.Time
    Error, InterruptedBy      string
}
type SQSTelemetry struct { /* ... */ }
type Telemetry = SQSTelemetry // deprecated alias
func NewSQSTelemetry(cfg aws.Config, queueURL string, logger Logger) *SQSTelemetry
func (t *Telemetry) SendJobStarted(ctx context.Context, status JobStatus) error
func (t *Telemetry) SendJobCompleted(ctx context.Context, status JobStatus) error
func (t *Telemetry) SendJobTimeout(ctx context.Context, status JobStatus) error
func (t *Telemetry) SendJobFailure(ctx context.Context, status JobStatus) error
func (t *Telemetry) SendWithTimeout(status JobStatus, timeout time.Duration) error
func DetermineCompletionStatus(interruptedBy string, exitCode int) string
const (
    StatusStarted, StatusSuccess, StatusFailure,
    StatusTimeout, StatusInterrupted = "started", "success", "failure", "timeout", "interrupted"
)

// termination.go
type InstanceTerminator interface {
    TerminateInstance(ctx context.Context, instanceID string, status JobStatus) error
}
type EC2API interface {
    TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}
type EC2Terminator struct { /* ... */ }
type Terminator = EC2Terminator // deprecated alias
func NewEC2Terminator(cfg aws.Config, telemetry TelemetryClient, logger Logger) *EC2Terminator
func (t *EC2Terminator) TerminateInstance(ctx context.Context, instanceID string, status JobStatus) error
func (t *EC2Terminator) TerminateWithStatus(instanceID, status string, exitCode int, duration time.Duration, errorMsg string) error
func (t *EC2Terminator) TerminateOnPanic(instanceID, jobID string, panicValue interface{})
const TelemetryTimeout = 30 * time.Second
```

`NewValkeyTelemetry` and `NewK8sTerminator` are referenced as the K8s-side counterparts in interface doc comments, satisfying the same `TelemetryClient` / `InstanceTerminator` contracts.

## Data [coverage: high -- 6 sources]

**`JobStatus`** is the single envelope used for telemetry. Field tags show on-wire shape:

```json
{
  "instance_id": "i-0abc...",
  "job_id": "12345678",
  "status": "started|success|failure|timeout|interrupted",
  "exit_code": 0,
  "duration_seconds": 42,
  "started_at": "2026-04-30T10:00:00Z",
  "completed_at": "2026-04-30T10:00:42Z",
  "error": "...",
  "interrupted_by": "SIGTERM"
}
```

**`JobResult`** is the in-process output of `Executor.ExecuteJob` (not serialized). It feeds `JobStatus` via `Duration.Seconds()` and `InterruptedBy`.

**`RunnerConfig`** comes from `pkg/secrets`; populated fields touched here: `Repo`, `Org`, `RunnerGroup`, `JobID`, `Labels`, `JITToken`, `CacheToken`.

**Local file paths:**

- `/opt/actions-runner` — AMI-prebaked runner directory (`runnerDir`).
- `/home/runner` — Docker-prebaked runner directory (`prebakedRunnerDir`).
- `<runnerDir>/bin/Runner.Listener` — sentinel binary used by `isValidRunnerDir` to validate a pre-baked install.
- `<runnerPath>/run.sh` — invoked by `Executor.ExecuteJob` with `cmd.Dir = runnerPath`.
- `/etc/runs-fleet/config/{repo,org,runner_group,job_id,labels}` — K8s ConfigMap mount.
- `/etc/runs-fleet/secrets/{jit_token,cache_token}` — K8s Secret mount.
- `/proc/meminfo`, `/` — read by `SafetyMonitor`.

## Key Decisions [coverage: high -- 6 sources]

- **Pre-baked first, download fallback.** `DownloadRunner` checks `/opt/actions-runner` and `/home/runner` for a valid `Runner.Listener` before hitting the GitHub releases API, so a baked AMI or runner image incurs zero network cost.
- **Process group + signal forwarding.** `Executor` sets `Setpgid: true` and forwards SIGTERM/SIGINT to `-cmd.Process.Pid` (the whole group), so child Actions steps die with the runner. Graceful shutdown waits up to `GracefulShutdownTimeout` (90s) before SIGKILL.
- **Stream output.** Every line scanned from `stdout`/`stderr` is logged and, if `SetCloudWatchLogger` was called, also pushed to CloudWatch. Scanner buffer is bumped to 1 MiB to tolerate long log lines.
- **Per-process safety timeout.** `SafetyMonitor.Monitor` runs a ticker (`DefaultCheckInterval` = 30s) inside the executor's context and invokes `onTimeout` when elapsed > `maxRuntime`. It also warns when <10 minutes remain.
- **Pluggable telemetry transport.** `TelemetryClient` is an interface; `SQSTelemetry` is the EC2 implementation, and a `NewValkeyTelemetry` exists for the K8s backend. `Telemetry`/`Terminator` are kept as deprecated aliases for backward compatibility.
- **Telemetry retry, terminate regardless.** SQS sends use up to 3 attempts with `1<<attempt * telemetryRetryBaseDelay` backoff. `EC2Terminator.TerminateInstance` logs a warning on telemetry failure but proceeds with `TerminateInstances` anyway — the host must die.
- **Telemetry-then-terminate ordering.** `EC2Terminator` sends `SendJobCompleted` first, sleeps `ec2TerminationDelay` (2s) to flush, then calls EC2. The `TelemetryTimeout` (30s) caps how long completion blocks the shutdown path.
- **Panic-safe shutdown.** `TerminateOnPanic` constructs a `failure` `JobStatus` with the recovered value and triggers termination from `defer recover()` in `cmd/agent`.
- **Best-effort cleanup.** Every `Close`/`Remove` is wrapped in `_ = ...` or logged-and-continued; the agent prefers shutting down dirty over hanging.

## Gotchas [coverage: high -- 6 sources]

- **GitHub asset name version pinning.** `assetName` is built as `actions-runner-linux-<x64|arm64>-<TagName[1:]>.tar.gz`. The `[1:]` slice assumes `TagName` always starts with `v` (e.g., `v2.319.1`); a tag without that prefix would yield a malformed name.
- **No checksum on the download path.** `VerifyChecksum` is exported but `DownloadRunner` does not call it — the tarball is extracted unchecked. Operators relying on integrity must invoke it themselves.
- **No download retry.** `downloadFile` performs one HTTP GET via `http.DefaultClient`. A flaky network 5xx aborts the boot.
- **`tar` shells out.** Extraction uses `exec.Command("tar", ...)`; the host image must ship a compatible `tar`.
- **Memory check is Linux-only.** `SafetyMonitor.checkMemory` opens `/proc/meminfo`; on macOS / non-Linux it logs a one-time warning (`memoryCheckWarningOnce`) and skips. Disk check uses `syscall.Statfs` which is also Unix-only.
- **`run.sh` env minimalism.** `Executor` sets only `RUNNER_ALLOW_RUNASROOT=1` on top of `os.Environ()`; anything the runner needs (proxy vars, etc.) must already be in the parent env.
- **K8s config required-fields.** `FetchK8sConfig` errors out if `jit_token` or `repo` is missing, but treats `org`, `runner_group`, `job_id`, `labels`, `cache_token` as optional with warning logs.
- **Labels parsing fallback.** `splitLabels` is a hand-rolled CSV trimmer used only when the labels file isn't valid JSON; trailing commas yield empty entries that are then filtered.
- **Context cancellation kills the runner.** If the parent `ctx` is cancelled mid-job, `ExecuteJob` SIGTERMs the process group, waits up to `GracefulShutdownTimeout`, then SIGKILLs. The returned `JobResult.Error` is `ctx.Err()`, not the runner's exit.
- **Signal handler is global.** `signal.Notify` is called inside `ExecuteJob`; running multiple executors concurrently in one process would race on signals (not the intended use, but worth noting).
- **K8s pod deletion timing.** `K8sTerminator` (referenced) relies on the pod's own exit; if telemetry to Valkey hangs past `TelemetryTimeout`, the pod still terminates because the agent process exits — no analogue of the 2s `ec2TerminationDelay` is needed.
- **Partial cleanup states.** `DownloadRunner` removes the tarball after extraction but does not roll back the `runnerDir` on a failed extract; a retry would extract over the partial tree.

## Sources [coverage: high]

- [`pkg/agent/config.go`](/Users/shavakan/workspace/runs-fleet/pkg/agent/config.go)
- [`pkg/agent/downloader.go`](/Users/shavakan/workspace/runs-fleet/pkg/agent/downloader.go)
- [`pkg/agent/executor.go`](/Users/shavakan/workspace/runs-fleet/pkg/agent/executor.go)
- [`pkg/agent/safety.go`](/Users/shavakan/workspace/runs-fleet/pkg/agent/safety.go)
- [`pkg/agent/telemetry.go`](/Users/shavakan/workspace/runs-fleet/pkg/agent/telemetry.go)
- [`pkg/agent/termination.go`](/Users/shavakan/workspace/runs-fleet/pkg/agent/termination.go)
