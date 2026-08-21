---
topic: Job State Machine
last_compiled: 2026-08-21
sources_count: 20
---

# Job State Machine

## Purpose [coverage: high -- 20 sources]

runs-fleet tracks each GitHub Actions job through a lifecycle in a single
backend: DynamoDB. Job state lives entirely in the `runs-fleet-jobs` table
([pkg/db/jobs.go](../../pkg/db/jobs.go)), keyed by typed `JobStatus` constants
([pkg/db/job_status.go](../../pkg/db/job_status.go)).

A second, related concern is runner identity: getting a GitHub Actions runner
that will actually execute *this* job. `pkg/runner.Manager`
([pkg/runner/manager.go](../../pkg/runner/manager.go)) owns that — it mints a
credential, builds a collision-resistant runner name, and writes a
`secrets.RunnerConfig` blob the booting EC2 agent reads. Since PR #424 it
prefers a GitHub **JIT config** ([pkg/github/jitconfig.go](../../pkg/github/jitconfig.go))
over a plain registration token, falling back to the token on any failure.

The hard operational truth this topic exists to record: **GitHub dispatch is
label-matched, not job-bound, and no credential in this codebase changes that.**
Everything else here — requeue sweeps, still-queued redispatch,
`OriginalLabel` propagation, orphaned-runner deregistration, spot-reclaim
re-runs — is the machinery that converges a fleet whose runners can be handed
each other's work.

## Architecture [coverage: high -- 20 sources]

### Status vocabulary — two of them

`JobStatus` is a string type stored in the `status` attribute of each job record
(partition key `job_id`). [pkg/db/job_status.go](../../pkg/db/job_status.go)
defines eleven lifecycle constants:

`JobStatusLaunched`, `JobStatusRunning`, `JobStatusClaiming`,
`JobStatusTerminating`, `JobStatusRequeued`, `JobStatusCompleted`,
`JobStatusSuccess`, `JobStatusFailed`, `JobStatusError`, `JobStatusOrphaned`,
`JobStatusCancelled`.

It then defines three more for a *different* vocabulary — the agent's:

```go
// Agent-reported terminal statuses. The agent writes its own vocabulary
// (pkg/agent/telemetry.go) straight through MarkJobComplete, so the table holds
// "failure" rather than JobStatusFailed and carries "timeout"/"interrupted",
// which have no JobStatus constant.
JobStatusAgentFailure     JobStatus = "failure"
JobStatusAgentTimeout     JobStatus = "timeout"
JobStatusAgentInterrupted JobStatus = "interrupted"
```

`IsTerminal()` spans both and deliberately excludes `requeued` and
`terminating` (the instance may still take work) and `orphaned` (that status is
stamped on a *swallowed* EC2 lookup error, so a transient API fault must not
mark a live instance's job terminal and get the instance reaped).

### Transitions

Driven by conditional DynamoDB writes in `pkg/db/jobs.go`:

1. `ClaimJob` — compare-and-swap claim as a self-expiring lease. Writes
   `claiming`.
2. `SaveJob` — instance created; the record is written (PutItem) with status
   `launched`, `created_at` re-stamped as assignment time, and
   `original_label`.
3. `MarkJobStarted` — `launched → running` when the agent reports the runner
   registered and began executing, conditioned on still being `launched`;
   returns the post-update record via `ReturnValueAllNew` so the termination
   handler can compute `InstanceProvisionSeconds` with no second read (PR #387).
4. `MarkJobComplete` — terminal write, now taking an `instanceID` and
   **conditioned on `instance_id = :iid`**, so a stale agent cannot clobber a
   record already re-dispatched to another instance. Writes whatever status
   string the caller passes (see Gotchas), plus `completed_at`,
   `exit_code`, `duration_seconds`.
5. `MarkJobCancelled` — `launched → cancelled` (PR #417), for a job GitHub
   cancelled before its runner ever confirmed; conditioned on `launched` so a
   concurrently-confirming runner keeps ownership.
6. `MarkJobRequeuedByJobID` — `running`/`launched → requeued`; returns
   `(false, nil)` rather than an error on a lost condition.
7. `MarkInstanceTerminating` — looks up the instance's active job and sets
   `terminating`.
8. `FailExhaustedClaim` — `claiming → error` after `claimMaxAttempts` (3).
9. `housekeeping.MarkJobOrphaned` — `→ orphaned` + `completed_at`, conditioned
   on **the exact status the caller observed** and refusing any status outside
   `reconcilableStatuses` (`running`, `claiming`, `launched`, `requeued`).

### Runner acquisition

`Manager.PrepareRunner` ([pkg/runner/manager.go:93](../../pkg/runner/manager.go)):

1. Parse `owner/repo`; build `runnerName` via `buildRunnerName`.
2. `GetRegistrationToken(ctx, repo)` through the `registrationTokenGetter`
   interface (always repo-level).
3. `mintJITConfig` — if the injected client also satisfies the *optional*
   `jitConfigGenerator` interface, resolve the runner group name to its numeric
   ID and call `GenerateJITConfig`. Every failure degrades to `""`, leaving the
   agent on the token path.
4. Generate a cache token (`cache.GenerateCacheToken`), plus the buildkit-cache
   and runner-logs blocks when configured.
5. `secretsStore.Put(ctx, instanceID, config)` — both the JIT config *and* the
   registration token are stored, so a mint failure costs a steal-able runner
   rather than the whole dispatch.

On the instance, `Registrar.RegisterRunner`
([pkg/agent/registration.go:76](../../pkg/agent/registration.go)) is a **no-op**
when `JITConfig` is set — GitHub created the registration when it minted the
config, and running `config.sh --replace` anyway would disturb it. `Executor.
ExecuteJobWithConfig` ([pkg/agent/executor.go:70](../../pkg/agent/executor.go))
passes the blob via the `ACTIONS_RUNNER_INPUT_JITCONFIG` **environment
variable** rather than `--jitconfig <blob>`, because argv is world-readable
through `/proc/<pid>/cmdline`.

### Recovery paths

Four distinct mechanisms exist because a job can get stuck in four ways:

- **Still-queued redispatch at termination**
  ([pkg/termination/handler.go:755](../../pkg/termination/handler.go)
  `redispatchIfStillQueued`) — the agent is shutting down but GitHub still has
  the job queued, so re-drive it.
- **Auto-requeue of jobs hung `running`** (PR #430) — the stale-jobs sweep
  ([pkg/housekeeping/tasks.go:1256](../../pkg/housekeeping/tasks.go)
  `ExecuteStaleJobs` → `reconcileStaleJobsViaGitHub`) re-dispatches records that
  read `running` while GitHub still reports the job `queued`, through the same
  `requeueCandidate` path as the operator action, with a `stale_queued` metric
  reason and `maxStaleQueuedRequeues` draining a mass hang gradually.
- **Retirement of `requeued` strays** (PR #423) — the orphan scan now admits
  `requeued`, aged on `requeued_at` rather than `created_at`.
- **Re-run after an unrecoverable spot reclaim** (PR #454) —
  [pkg/events/rerun.go](../../pkg/events/rerun.go) +
  [pkg/github/rerun.go](../../pkg/github/rerun.go).

## Talks To [coverage: high -- 20 sources]

- **DynamoDB** (`pkg/db/jobs.go`) — `runs-fleet-jobs` is the sole source of
  truth for job status; every transition uses a conditional expression. See
  [state-storage](state-storage.md) for the table schema and reaper behavior.
- **GitHub REST API** ([pkg/github/client.go](../../pkg/github/client.go),
  [jitconfig.go](../../pkg/github/jitconfig.go),
  [runners.go](../../pkg/github/runners.go),
  [rerun.go](../../pkg/github/rerun.go)):
  - `GET /orgs/{owner}/installation`, falling back to
    `GET /users/{owner}/installation` for personal accounts.
  - `POST /app/installations/{id}/access_tokens`.
  - `POST /repos/{owner}/{repo}/actions/runners/registration-token` — always
    repo-level.
  - `POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig` (PR #424).
  - `GET /repos/{owner}/{repo}/actions/runner-groups` — name → numeric ID.
  - `GET /repos/{owner}/{repo}/actions/runners` (paginated) and
    `DELETE .../runners/{id}` (PR #436).
  - `POST /repos/{owner}/{repo}/actions/jobs/{id}/rerun` via go-github's
    `Actions.RerunJobByID` (PR #454).
  - `GetWorkflowJobByID` for status polling.
- **Secrets backend** (`secrets.Store`) — `Put`/`Delete` keyed by EC2 instance
  ID; holds the `RunnerConfig` the agent reads at boot.
- **`pkg/agent`** — `registration.go` decides token-vs-JIT; `executor.go` hands
  the JIT blob to `run.sh` via env; `telemetry.go` reports the terminal status
  that lands verbatim in the `status` attribute.
- **`pkg/housekeeping`** — `ExecuteStaleJobs`, `ExecuteOrphanedJobs`,
  `ExecuteOrphanedRunners`, `RequeueHungJobs`/`RequeueJob`, `ExecuteOldJobs`.
  The orphaned-runner sweep gets its repo set from `db.ListActiveRepos` and its
  offline ages from the `__runner_offline:` sighting rows.
- **`pkg/events`** — the spot-interruption path requeues, then calls
  `recoverInterruptedJob` to re-run if GitHub concluded failure.
  `events.JobInfo` is the shared read model returned by `MarkJobStarted` /
  `MarkJobComplete` / `GetJobBy*`.
- **`pkg/termination`** — consumes the agent's `SendJobStarted`, calls
  `MarkJobStarted`, publishes `InstanceProvisionSeconds`, and owns the
  still-queued redispatch.
- **`internal/handler`** — `BuildRunnerLabel` decides the label a dispatch
  registers under.

## API Surface [coverage: high -- 20 sources]

### `pkg/db/job_status.go`

`type JobStatus string`; `(JobStatus).String()`; `(JobStatus).IsTerminal() bool`;
eleven lifecycle constants plus `JobStatusAgentFailure`, `JobStatusAgentTimeout`,
`JobStatusAgentInterrupted`.

### `pkg/db/jobs.go`

- `SaveJob(ctx, *JobRecord) error`
- `ClaimJob(ctx, jobID, runID int64, repo string) error` —
  `ErrJobAlreadyClaimed`, `ErrJobClaimExhausted`
- `FailExhaustedClaim(ctx, jobID int64) error`
- `MarkJobStarted(ctx, jobID int64, startedAt time.Time) (*events.JobInfo, error)`
- `MarkJobComplete(ctx, jobID int64, instanceID, status string, exitCode, duration int) (*events.JobInfo, error)`
- `MarkJobCancelled(ctx, jobID int64) (bool, error)`
- `MarkJobRequeuedByJobID(ctx, jobID int64) (bool, error)`
- `MarkInstanceTerminating(ctx, instanceID string) error`
- `UpdateJobMetrics`, `DeleteJobClaim`, `LastJobCompletionForInstance`
- `GetJobByJobID`, `GetJobByInstance`, `GetPoolBusyInstanceIDs`,
  `QueryPoolJobHistory`, `GetPoolP90Concurrency`
- Admin-facing: `ListJobsForAdmin`, `GetJobForAdmin`, `GetJobStatsForAdmin`
- Types: `JobRecord` (carries `OriginalLabel`), `JobHistoryEntry`,
  `AdminJobEntry`, `AdminJobFilter`, `AdminJobStats`

### `pkg/runner/manager.go`

- `type Manager struct { github registrationTokenGetter; secretsStore secrets.Store; config ManagerConfig }`
- `type registrationTokenGetter interface { GetRegistrationToken(ctx, repo string) (*github.RegistrationResult, error) }` — unexported.
- `type jitConfigGenerator interface { GenerateJITConfig(ctx, repo string, req github.JITConfigRequest) (string, error); ResolveRunnerGroupID(ctx, repo, groupName string) (*github.RunnerGroupResolution, error) }`
  — unexported and **optional**: `mintJITConfig` type-asserts `m.github` against
  it, so a provider that doesn't implement it keeps working on tokens.
- `type ManagerConfig { CacheSecret, BaseURL, TerminationQueueURL, BuildkitCacheBucket, BuildkitCacheRegion, RunnerLogsBucket, RunnerGroup string }`
- `type PrepareRunnerRequest { InstanceID, JobID, RunID, Repo, Pool, Conditions string; Labels []string }`
- `NewManager(githubClient registrationTokenGetter, secretsStore secrets.Store, config ManagerConfig) *Manager`
- `(m *Manager) PrepareRunner(ctx, PrepareRunnerRequest) error`
- `(m *Manager) CleanupRunner(ctx, instanceID string) error`
- Internal: `mintJITConfig`, `buildRunnerName(pool, repoName, conditions, jobID, instanceID)`,
  `runnerNameMaxLen = 64`.

### `pkg/github/client.go`

- `NewClient(appID, privateKeyBase64 string) (*Client, error)` (PKCS1 or PKCS8),
  plus `Option` functional options.
- `GetRegistrationToken(ctx, repo) (*RegistrationResult, error)` — repo-level
  only; satisfies `runner.registrationTokenGetter`.
- `GetWorkflowJobByID(ctx, repo string, jobID int64) (*WorkflowJobInfo, error)`
- Types: `RegistrationResult { Token string; IsOrg bool /* deprecated, always false */ }`,
  `WorkflowJobInfo { Status, Conclusion string }`.

### `pkg/github/jitconfig.go` (PR #424)

- `GenerateJITConfig(ctx, repo string, req JITConfigRequest) (string, error)` —
  returns the opaque `encoded_jit_config`. Validates `Name`, a positive
  `RunnerGroupID`, and at least one label. Retries up to `maxRetries` honoring
  `Retry-After`.
- `ResolveRunnerGroupID(ctx, repo, groupName string) (*RunnerGroupResolution, error)`
  — empty name resolves to Default with no API call.
- Types: `JITConfigRequest { Name string; RunnerGroupID int64; Labels []string; WorkFolder string }`,
  `RunnerGroupResolution { ID int64; FallbackErr error }`.
- Constants: `defaultRunnerGroupID = 1`, `runnerGroupCacheTTL = 30 * time.Minute`,
  `runnerGroupFallbackTTL = time.Minute`.

### `pkg/github/runners.go` (PR #436)

- `ListRunners(ctx, repo string) ([]Runner, error)` — paginated,
  `runnersPageSize = 100`, capped at `runnerListPageCap = 20` pages.
- `DeleteRunner(ctx, repo string, runnerID int64) error` — **404 is success.**
- `type Runner { ID int64; Name, Status string; Busy bool }`.

### `pkg/github/rerun.go` + `pkg/events/rerun.go` (PR #454)

- `(*github.Client) RerunJob(ctx, repo string, jobID int64) error`
- `type events.JobRerunner interface { GetWorkflowJobState(ctx, repo string, jobID int64) (WorkflowJobState, error); RerunJob(ctx, repo string, jobID int64) error }`
- `(*events.Handler) SetGitHub(gh JobRerunner)`; internal
  `recoverInterruptedJob`, `awaitConcludedJob`.
- `type events.WorkflowJobState { Status, Conclusion string }`.
- Overridable-for-test vars: `rerunWaitBudget = 45 * time.Second`,
  `rerunPollInterval = 5 * time.Second`.
- Wired in [cmd/server/main.go](../../cmd/server/main.go) via
  `eventsRerunAdapter`, which maps `GetWorkflowJobByID` onto
  `GetWorkflowJobState`.

### `pkg/housekeeping` recovery surface

- `FindRequeueableJobs(ctx, scanAPI, jobsTable, threshold, statuses, opts...) ([]RequeueableJob, bool, error)`
  — the bool is `truncated`.
- `BuildRequeueMessage(job RequeueableJob) *queue.JobMessage` — carries
  `OriginalLabel` forward.
- `RequeueHungJobs(ctx, deps, opts) (RequeueResult, error)`,
  `RequeueJob(ctx, deps, jobID, opts) (SingleRequeueResult, error)`,
  `GetRequeueableJob`.
- `MarkJobOrphaned(ctx, dynamoClient, jobsTable, jobID int64, observedStatus string) (bool, error)`
- `ExecuteOrphanedRunners(ctx) error` plus `SetRunnerRegistry(RunnerRegistry, ActiveReposFunc, RunnerSightingStore)`.
- Constants: `MaxRequeueRetries = 2`, `requeueReasonOperator = "operator_requeue"`,
  `requeueReasonStaleQueued = "stale_queued"`, `runnerNamePrefix = "runs-fleet-runner-"`,
  `maxRunnerDeregistrations = 200`.

### `internal/handler`

- `BuildRunnerLabel(ctx, job *queue.JobMessage) string` — returns
  `job.OriginalLabel` when set; otherwise synthesizes
  `runs-fleet=<runID>[/pool=…]` and **warns when `RetryCount > 0`**.

## Data [coverage: high -- 20 sources]

**Job record** (`jobRecord`, partition key `job_id`; `instance_id` is an
attribute plus an optional GSI key):

```go
JobID, RunID           int64
Repo, InstanceType     string
Pool                   string  // omitempty
Spot, WarmPoolHit      bool
RetryCount             int
Status                 string  // omitempty; a JobStatus value OR an agent status
CreatedAt              string  // RFC3339
SpotRequestID          string  // omitempty
PersistentSpot         bool    // omitempty
TraceID                string  // omitempty, extracted from W3C traceparent
OriginalLabel          string  // omitempty; the runs-on label the workflow asked for
```

Mutated by transitions: `started_at`, `completed_at`, `requeued_at`,
`exit_code`, `duration_seconds`.

**Surfaced read model** (`events.JobInfo`, from `unmarshalJobInfo`):

```go
events.JobInfo{
    JobID, RunID   int64
    Repo, InstanceType, Pool string
    Spot           bool
    RetryCount     int
    WarmPoolHit    bool      // PR #387
    HotPoolHit     bool      // running hot-pool spare, no boot
    CreatedAt      time.Time // assignment time; zero if unparseable
    OriginalLabel  string    // PR #435
}
```

`OriginalLabel` carries the comment that explains its whole existence:

> A requeue must re-register under it: GitHub matches runners by exact label
> set, so the synthesized fallback produces a runner this job can never be
> handed to.

It is set by `github.ParseLabels` / `ParseLabelsWithAliases`
([pkg/github/webhook.go:150-166](../../pkg/github/webhook.go)) — from the native
`runs-fleet` marker, or from a matched **alias** label (an externally-defined
custom label such as an ARC scale-set label, preserved verbatim so the booted
runner registers under it). It then flows through `queue.JobMessage`
(`original_label`), `db.JobRecord`, `events.JobInfo`,
`housekeeping.RequeueableJob`, and back out through `BuildRequeueMessage`,
`termination/handler.go`, and all four worker loops.

**Runner config** (written to `secrets.Store` keyed by `instanceID`) — see
[state-storage](state-storage.md) for the full struct. The two credential
fields:

- `RegistrationToken` (JSON tag `jit_token`, frozen wire contract) — for
  `config.sh --token`.
- `JITConfig` (JSON tag `jit_config`, omitempty) — supersedes the token when set.

**Runner name** (`buildRunnerName`) now carries **two** suffixes:

- Prefix `runs-fleet-runner-`, then `<pool>` if pool-bound, else
  `<repoName>[-<conditions>]`.
- Last 6 chars of `jobID` — distinguishes jobs sharing identical `runs-on`
  labels.
- Last 5 chars of `instanceID` — distinguishes **duplicate dispatches of the
  same job**. The agent registers with `--replace`, so two instances sharing a
  name would evict each other's GitHub registration and fail the job.
- Truncated to `runnerNameMaxLen` (64), trimming the *name* rather than the
  suffixes.

## Key Decisions [coverage: high -- 20 sources]

- **JIT config over registration token (PR #424, 48a0002)** — `PrepareRunner`
  mints a JIT config when the injected client supports it. What it buys is
  real: the runner becomes ephemeral without a `--ephemeral` flag, `config.sh`
  is skipped entirely, and the credential lives in the child's environment
  rather than argv. What it does **not** buy is job binding — see the first
  Gotcha. Both credentials are stored so a mint failure degrades rather than
  fails.
- **JIT capability is a separate optional interface.** `jitConfigGenerator` sits
  beside `registrationTokenGetter` rather than extending it, so a provider that
  cannot mint JIT configs keeps working. `mintJITConfig` type-asserts and
  returns `""` on a failed assertion.
- **Runner-group failures resolve to Default, never to an error.** A job with no
  runner starves, so an unresolvable group name is a placement preference lost,
  not a dispatch lost. The substitution is reported via
  `RunnerGroupResolution.FallbackErr` because `pkg/github` does no logging of
  its own — `pkg/runner` logs it under the single message
  `"runner group unresolved"` with an `outcome` field, so alerting greps one
  string.
- **Asymmetric cache TTLs for group resolution.** 30 minutes for a hit,
  **1 minute** for a Default substitution: caching a success is nearly free, but
  caching a failure misroutes every dispatch for that repo until it expires.
- **`OriginalLabel` is carried through every re-dispatch (PR #435, 7375d06).**
  GitHub dispatch is exact label-set membership. A requeue that lost the label
  its first dispatch carried registers a runner the starving job can never be
  handed to — the recovery burns an instance and the job keeps waiting.
  `BuildRunnerLabel` therefore treats a `RetryCount > 0` dispatch with no
  `OriginalLabel` as "worth an alert, not a silent degrade".
- **Aliased labels double as their own warm pool.** When an alias matches and
  the spec set no explicit pool, the alias label becomes the pool name (if
  legal), so migrated ARC workloads get fast restarts.
- **Deregister runners that never ran a job (PR #436, 70f467c; window fix
  #437).** GitHub auto-deletes an ephemeral runner only after it *completes*
  work, so a runner whose instance was terminated first stays registered
  forever — **observed at 360 of 369 registrations in one repo.** The sweep
  deletes only offline, non-busy registrations bearing `runs-fleet-runner-`,
  and only after a *durably recorded* continuous-offline age.
- **The offline threshold is derived, not hardcoded** (PR #437, 1aa1c24).
  `minOfflineAge()` returns `deadAssignmentAge()`, itself derived from the
  configured runtime ceiling, because the validator permits up to 24h and a
  deploy that raised the ceiling for a slower job class would otherwise leave
  the threshold below it — and the sweep would start deleting live runners.
- **Offline sightings must be durable, not in-process.** The orchestrator runs
  multiple replicas and the housekeeping task lock serializes only a single
  tick, so consecutive sweeps are usually run by different replicas; a
  per-process counter would never accumulate and the sweep would never delete
  anything. Hence `__runner_offline:` rows in the pools table, stamped with
  `if_not_exists` so the *original* stamp survives.
- **Deleting on an unknown age is the one mistake that costs a job.** A failed
  `RecordRunnerOffline` leaves the registration alone; the deletes are capped at
  `maxRunnerDeregistrations` (200) per cycle; `DeleteRunner` treats 404 as
  success; and the whole sweep is a no-op unless all three dependencies
  (registry, repo source, sighting store) are wired.
- **Auto-requeue jobs hung `running` while GitHub still has them queued
  (PR #430, c311300).** Observed up to **12.5 hours** of on-demand burn. The
  sweep reuses the operator action's `requeueCandidate` path: re-read the
  status, re-confirm queued with GitHub immediately before the irreversible
  terminate, flip the record under a condition, then send. In-progress jobs are
  left alone; `MaxRequeueRetries` bounds per-job churn; `maxStaleQueuedRequeues`
  drains a mass hang across cycles.
- **Retire jobs stranded in `requeued` (PR #423, 3277c9e).** A job is flipped to
  `requeued` *before* its re-dispatch message goes out, so the record is
  claimable when a worker picks it up. If that re-dispatch is never claimed,
  nothing ever looks at the record again: the orphan sweep scanned
  running/claiming/launched, the stale-jobs sweep running/claiming, the requeue
  sweep launched, and the old-jobs GC filters on `completed_at` — which a
  `requeued` record never gets, and a DynamoDB filter on a missing attribute
  never matches, so it wasn't even deleted. **472 stranded records, 4.3% of the
  jobs table, the oldest from March.** The orphan scan now admits `requeued`
  aged on `requeued_at` (the only thing separating a live re-dispatch from one
  that never landed, since the requeue terminates the instance), retired records
  get `completed_at` so the 7-day GC finally collects them, and
  `reconcilableStatuses` follows so the per-job Reconcile button doesn't refuse
  exactly the records the sweep was taught to find.
- **`MarkJobOrphaned` is pinned to the observed status.** Its condition used to
  accept any live status, so a candidate could be retired minutes later after a
  worker had claimed it — which matters more now that the scan admits
  `requeued`, a status that is re-claimable by design while `orphaned` is not.
  It now conditions on the exact status the caller read, refuses a settled one
  outright, and **reports whether the write landed**, so sweeps count records
  they actually retired rather than counting a lost condition as a retirement.
- **Re-run, don't re-queue, after a spot reclaim (PR #454, d74efec).** A reclaim
  kills the runner mid-job and GitHub concludes the job failed before any
  replacement can register; a re-queued runner is always too late because
  registration binds to a label, not a job. `recoverInterruptedJob` waits up to
  `rerunWaitBudget` (45s) for GitHub to conclude, re-runs **only** on
  `conclusion == "failure"` (a success means it finished despite the reclaim; a
  cancellation is somebody's deliberate stop), and only once per job
  (`RetryCount > 0` short-circuits). The 45s budget is deliberate: GitHub
  concluded reclaimed jobs at a median of 29s but p90 of 169s, and the events
  worker is blocked for the duration inside a 90s `MessageProcessTimeout` — the
  slow tail is left to the re-queue.
- **`RerunJob` targets one job, not the run's rerun-failed-jobs endpoint** —
  that would also re-run jobs that failed for real, spending capacity to
  reproduce genuine failures. Dependent jobs still come along, which recovers
  the gate jobs a reclaim cascades into.
- **`MarkJobComplete` is conditioned on `instance_id`.** An agent with a stale
  boot-time config reports a `job_id` whose record has since been re-dispatched
  (the re-claim rewrites the item without `instance_id` until the new `SaveJob`
  lands); an unconditional write would clobber the live retry.
- **Self-expiring claim lease.** `ClaimJob` is a compare-and-swap on
  `created_at`: re-claimable if the prior record is `requeued`/`terminating`, or
  a stale `claiming` lease older than `claimStaleThreshold` (100s — above the
  90s `MessageProcessTimeout` and below the 120s SQS visibility timeout, so the
  first redelivery finds the lease already expired). Capped at
  `claimMaxAttempts` (3), then `FailExhaustedClaim`.
- **Repo-level registration only.** Org-level would let runners pick up jobs
  from any repo in the org. Also enforced agent-side:
  `Registrar.RegisterRunner` requires `config.Repo`.
- **Instance-ID suffix in the runner name.** Duplicate dispatches of one job
  would otherwise collide, and `--replace` makes a collision mutually fatal.
- **`pkg/github` relocation, 2026-07 (PR #380)** — `pkg/runner/github.go` moved
  to `pkg/github/client.go`; the package now also holds `jitconfig.go`,
  `runners.go`, `rerun.go`, `webhook.go`, and `alias.go`.
- **DB record as cross-instance rendezvous (PR #387)** — see the
  [db-record-as-rendezvous](../concepts/db-record-as-rendezvous.md) concept and
  [state-storage](state-storage.md).

## Gotchas [coverage: high -- 20 sources]

- **A JIT config does NOT bind a runner to a job — and three source files still
  claim it does.** This is the single most load-bearing correction in this
  article. `generate-jitconfig` accepts only `name`, `runner_group_id`,
  `labels`, and `work_folder`; GitHub's scheduler still hands the runner
  whichever *queued job matches those labels*. PR #424's premise was a
  misreading, corrected in PR #430's second commit
  ("docs(github): a JIT config makes a runner ephemeral, not job-bound"):

  > This misreading is why the JIT switch (#424) changed the shape of runner
  > theft instead of ending it.

  The corrected statement lives at
  [pkg/github/jitconfig.go:40-48](../../pkg/github/jitconfig.go). But the old
  claim survives in three places that were never updated —
  [pkg/runner/manager.go:53-58](../../pkg/runner/manager.go) ("gets job-bound
  runners: GitHub ties a JIT runner to the single job it was minted for"),
  [pkg/secrets/store.go:30-36](../../pkg/secrets/store.go) ("bound by GitHub to
  the single job it was minted for"), and
  [pkg/agent/executor.go:61-64](../../pkg/agent/executor.go) ("bound it to a
  single job"). Trust `jitconfig.go`. Recovery still depends entirely on the
  requeue sweeps (#429/#430) and the still-queued redispatch — as
  `JITConfigRequest`'s own doc comment says.
- **Runner theft was measured at 13% of runners** before #424 and was *reshaped,
  not eliminated*. A runner minted for one job routinely serves another; the
  replacement runner then idles while its record reads `running` and GitHub
  still has the job queued. That state had no sweep covering it until #430.
- **"offline" does not mean dead.** A registration exists from the moment its
  JIT config is minted — *before* the instance boots — so a perfectly live
  runner reads `offline` for its entire startup, and an agent may sit in standby
  for the standby deadline before taking a job. This is why the
  orphaned-runner sweep needs a durably-recorded continuous-offline age derived
  from the runtime ceiling, not a status snapshot.
- **Stored status is often GitHub's / the agent's vocabulary, not a `JobStatus`
  constant.** `MarkJobComplete` takes a bare `status string` and writes it
  unconditionally; `pkg/agent/telemetry.go` emits
  `success`/`failure`/`timeout`/`interrupted`. Filtering on
  `status == "completed"` matched **1 job / 0.0 hours instead of 8,102 / 432.3
  hours** on live July data, which broke the admin Cost tab, the Metrics tab's
  cost figure, and the daily cost report (PR #403). Use `AdminJobFilter`'s
  `CompletedOnly` (attribute-existence on `completed_at`) instead — and never
  add a new consumer that filters terminal jobs by status string.
- **GitHub job status and internal job status are different axes.**
  `WorkflowJobInfo` / `events.WorkflowJobState` carry GitHub's
  `queued`/`in_progress`/`completed` plus a conclusion; `JobStatus` carries
  `launched`/`running`/`claiming`/…. Nothing merges them; callers map at the
  call site.
- **`MarkJobComplete` and `MarkJobStarted` return `(nil, nil)` on a lost
  condition.** For `MarkJobStarted` that means a late message; for
  `MarkJobComplete` it means a stale agent reporting on a re-dispatched record.
  Callers must handle nil without treating it as an error.
- **`MarkJobRequeuedByJobID` returns `(false, nil)`, not an error**, on
  conditional failure — "already requeued / terminating" is a benign no-op.
- **A requeue flip is not automatically recoverable.** `rollbackRequeueFlip`
  ([pkg/housekeeping/requeue.go:677](../../pkg/housekeeping/requeue.go)) notes
  that a failure there is not recoverable automatically, precisely because
  `requeued` is a status no sweep scanned before #423.
- **The still-queued confirmation is point-in-time.** `confirmStillQueued` reads
  GitHub immediately before the irreversible terminate, but GitHub can dispatch
  the job in the window between the check and the kill.
- **`jobHasStatus` treats unknown state as "no".** A read error or a missing row
  counts as not matching, so a caller filtering on specific statuses fails
  closed.
- **`ListRunners` is capped at 20 pages** (`runnerListPageCap`), 100 per page. A
  repo with more than ~2000 registrations is swept across several cycles rather
  than letting one repo consume the whole GitHub API budget.
- **`DeleteRunner` treats 404 as success**, and a runner that picked up work
  between the listing and the delete is deleted by GitHub itself once that job
  ends — the race resolves to the same state either way.
- **JIT credentials must never reach a log, an error string, or a resource
  tag.** `doJITConfigRequest` reduces every non-2xx to a status-only error
  because the body may echo the config back; `RerunJob` does the same for
  installation tokens; the credential goes in the agent's child environment,
  never argv; and `SSMStore` deliberately leaves the credential parameter
  untagged.
- **`RunnerConfig.RegistrationToken`'s JSON tag is still `jit_token`.** The Go
  field was renamed (it never held a JIT credential) but the tag is a frozen
  wire contract with agents already in flight. Don't "fix" it.
- **A 2xx with an empty `encoded_jit_config` is a hard failure**, not an empty
  string — it would boot a runner that can never register.
- **`RegistrationResult.IsOrg` is deprecated and always `false`** because
  registration is always repo-level. `getInstallationInfo` still needs the
  `/users/{owner}/installation` fallback for personal-account repos.
- **Clock skew on JWT** — `generateJWT` backdates `iat` by 60 seconds; without
  it GitHub rejects tokens from hosts with slight drift.
- **Retry policy** — GitHub calls retry up to `maxRetries` (3) on 429s, 5xx, and
  network errors with exponential backoff (`baseRetryDelay * 2^attempt`, capped
  at `maxRetryDelay = 10s`) plus jitter, honoring `Retry-After`/rate-limit-reset
  (capped at 60s). `baseRetryDelay` is a `var`, overridable in tests.
- **`GetWorkflowJobByID` and `RerunJob` each construct a fresh `http.Client`**
  rather than reusing `c.httpClient`: go-github's `WithAuthToken` mutates the
  transport with a token-injecting `RoundTripper`, which would corrupt
  subsequent calls on a shared client.
- **64-char truncation trims the name, not the suffixes.** A long
  pool/repo/conditions combination gets chopped; the job-ID and instance-ID
  suffixes survive. Verify uniqueness if you change the prefix scheme.
- **`JobInfo.CreatedAt` is zero-on-error, not fail-on-error** —
  `unmarshalJobInfo` swallows an RFC3339 parse failure so a malformed
  `created_at` never blocks a state transition. Every consumer must self-guard
  on the zero value.
- **The stale `jobRecord` comment** — [pkg/db/jobs.go:46](../../pkg/db/jobs.go)
  still says "Primary key is instance_id"; the hash key is `job_id`.
- **`ManagerConfig.RunnerGroup` is not reachable from config.**
  [cmd/server/main.go](../../cmd/server/main.go) leaves it empty because there
  is no `RUNS_FLEET_RUNNER_GROUP` env var; production runners use Default. The
  whole group-resolution path is therefore exercised only on the empty-name
  fast path today.

## Sources [coverage: high]
- [pkg/db/jobs.go](../../pkg/db/jobs.go)
- [pkg/db/job_status.go](../../pkg/db/job_status.go)
- [pkg/runner/manager.go](../../pkg/runner/manager.go)
- [pkg/github/client.go](../../pkg/github/client.go)
- [pkg/github/jitconfig.go](../../pkg/github/jitconfig.go)
- [pkg/github/runners.go](../../pkg/github/runners.go)
- [pkg/github/rerun.go](../../pkg/github/rerun.go)
- [pkg/github/webhook.go](../../pkg/github/webhook.go)
- [pkg/events/rerun.go](../../pkg/events/rerun.go)
- [pkg/events/handler.go](../../pkg/events/handler.go)
- [pkg/housekeeping/requeue.go](../../pkg/housekeeping/requeue.go)
- [pkg/housekeeping/orphans.go](../../pkg/housekeeping/orphans.go)
- [pkg/housekeeping/orphaned_runners.go](../../pkg/housekeeping/orphaned_runners.go)
- [pkg/housekeeping/tasks.go](../../pkg/housekeeping/tasks.go)
- [pkg/agent/registration.go](../../pkg/agent/registration.go)
- [pkg/agent/executor.go](../../pkg/agent/executor.go)
- [pkg/secrets/store.go](../../pkg/secrets/store.go)
- [pkg/termination/handler.go](../../pkg/termination/handler.go)
- [internal/handler/webhook.go](../../internal/handler/webhook.go)
- [cmd/server/main.go](../../cmd/server/main.go)
