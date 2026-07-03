---
topic: Job State Machine
last_compiled: 2026-07-03
sources_count: 5
---

# Job State Machine

## Purpose [coverage: high -- 5 sources]

runs-fleet tracks each GitHub Actions job through a lifecycle in a single backend: DynamoDB. There is no second state store — the K8s backend and its Valkey-based `pkg/state` package were removed in 2026-06, so job state now lives entirely in the `runs-fleet-jobs` table (`pkg/db/jobs.go`), keyed by typed `JobStatus` constants (`pkg/db/job_status.go`).

A second, related concern is runner identity: binding a job to a concrete GitHub Actions runner registration. `pkg/runner.Manager` owns that — it mints a registration token, builds a deterministic runner name, and writes a `secrets.RunnerConfig` blob the booting EC2 agent reads. It delegates the actual GitHub API call to `pkg/github.Client`.

Together these answer two operational questions: "what state is this job in?" (DynamoDB, `pkg/db`) and "which runner is bound to this instance?" (`pkg/runner` + `pkg/github`).

## Architecture [coverage: high -- 5 sources]

**Job status** is a single `JobStatus` string type stored in the `status` attribute of each DynamoDB job record (primary key `job_id`, GSI on `instance_id`). The constants, defined in `pkg/db/job_status.go`, are:

`JobStatusLaunched`, `JobStatusRunning`, `JobStatusClaiming`, `JobStatusTerminating`, `JobStatusRequeued`, `JobStatusCompleted`, `JobStatusSuccess`, `JobStatusFailed`, `JobStatusError`, `JobStatusOrphaned`.

Transitions are driven entirely by conditional DynamoDB writes in `pkg/db/jobs.go`:

1. `ClaimJob` — compare-and-swap claim as a self-expiring lease (see Key Decisions). Writes `claiming`.
2. `SaveJob` — instance created, job record written with status `launched`.
3. `MarkJobStarted` — agent reports the runner registered and began executing; `launched → running`, conditioned on still being `launched`.
4. `MarkJobComplete` — terminal write (`completed`/`success`/`failed`/`error`), unconditional (accepts whatever the caller passes as the final status string).
5. `MarkJobRequeuedByJobID` — `running`/`launched → requeued`, used on spot interruption or unconfirmed-runner watchdog.
6. `MarkInstanceTerminating` — looks up the instance's active job and sets `terminating`.
7. `FailExhaustedClaim` — `claiming → error`, conditioned on still being `claiming`, when `ClaimJob` re-claim attempts exceed `claimMaxAttempts` (3).

**Runner registration** is backend-agnostic within `pkg/runner`. `Manager`:

1. Receives a `PrepareRunnerRequest` (instance, job, repo, labels, pool, conditions).
2. Calls `GetRegistrationToken` (repo-level) through the `registrationTokenGetter` interface to mint a registration token.
3. Builds a deterministic runner name and a cache token.
4. Writes a `secrets.RunnerConfig` blob keyed by `instanceID` so the booting agent can fetch it.

`CleanupRunner` deletes the secrets entry when the job ends. `pkg/github.Client` (relocated from `pkg/runner/github.go` — see Key Decisions) also exposes `GetWorkflowJobByID`, which translates GitHub's API status (`queued`/`in_progress`/`completed`) plus conclusion into a `WorkflowJobInfo`; this is a separate read path from the DynamoDB status above, used for polling GitHub directly rather than driving job-state transitions.

## Talks To [coverage: high -- 5 sources]

- **DynamoDB** (`pkg/db/jobs.go`) — the `runs-fleet-jobs` table is the sole source of truth for job status. Reads/writes use conditional expressions to make transitions race-safe across concurrent orchestrator instances.
- **GitHub REST API** (`pkg/github/client.go`) — three endpoints:
  - `GET /orgs/{owner}/installation` with fallback to `GET /users/{owner}/installation` for personal accounts.
  - `POST /app/installations/{id}/access_tokens` for installation tokens.
  - `POST /repos/{owner}/{repo}/actions/runners/registration-token` — always repo-level to prevent runners from picking up jobs in other repos in the same org.
  - `GetWorkflowJobByID` (via `go-github`) for status polling.
- **Secrets backend** (`secrets.Store`) — `Put`/`Delete` keyed by EC2 instance ID. Holds the `RunnerConfig` the agent reads at boot.
- **Cache subsystem** (`pkg/cache`) — `cache.GenerateCacheToken` produces a per-job HMAC token scoped to the repo for cache isolation.
- **`pkg/events`** — `MarkJobComplete`/`MarkJobStarted` return `*events.JobInfo` so callers (worker loops, admin API) can act on the post-update record without a second read.

## API Surface [coverage: high -- 5 sources]

From `pkg/db/job_status.go`:

- `type JobStatus string` with constants: `JobStatusLaunched`, `JobStatusRunning`, `JobStatusClaiming`, `JobStatusTerminating`, `JobStatusRequeued`, `JobStatusCompleted`, `JobStatusSuccess`, `JobStatusFailed`, `JobStatusError`, `JobStatusOrphaned`.

From `pkg/db/jobs.go`:

- `func (c *Client) SaveJob(ctx, *JobRecord) error`
- `func (c *Client) ClaimJob(ctx, jobID, runID int64, repo string) error` — returns `ErrJobAlreadyClaimed` or `ErrJobClaimExhausted`.
- `func (c *Client) FailExhaustedClaim(ctx, jobID int64) error`
- `func (c *Client) MarkJobStarted(ctx, jobID int64, startedAt time.Time) (*events.JobInfo, error)`
- `func (c *Client) MarkJobComplete(ctx, jobID int64, status string, exitCode, duration int) (*events.JobInfo, error)`
- `func (c *Client) MarkJobRequeuedByJobID(ctx, jobID int64) (bool, error)`
- `func (c *Client) MarkInstanceTerminating(ctx, instanceID string) error`
- `func (c *Client) DeleteJobClaim(ctx, jobID int64) error`
- `func (c *Client) GetJobByInstance(ctx, instanceID string) (*events.JobInfo, error)` — GSI query with scan fallback.
- `func (c *Client) GetJobByJobID(ctx, jobID int64) (*events.JobInfo, error)`
- `func (c *Client) GetPoolBusyInstanceIDs(ctx, poolName string) ([]string, error)`
- `func (c *Client) QueryPoolJobHistory(ctx, poolName string, since time.Time) ([]JobHistoryEntry, error)`
- `func (c *Client) GetPoolP90Concurrency(ctx, poolName string, windowHours int) (int, error)`
- Admin-facing: `ListJobsForAdmin`, `GetJobForAdmin`, `GetJobStatsForAdmin`.
- Types: `JobRecord`, `JobHistoryEntry`, `AdminJobEntry`, `AdminJobFilter`, `AdminJobStats`.

From `pkg/runner/manager.go`:

- `type Manager struct { github registrationTokenGetter; secretsStore secrets.Store; config ManagerConfig }`
- `type registrationTokenGetter interface { GetRegistrationToken(ctx, repo string) (*github.RegistrationResult, error) }` — unexported, defined in `manager.go`.
- `type ManagerConfig { CacheSecret, BaseURL, TerminationQueueURL string }`
- `type PrepareRunnerRequest { InstanceID, JobID, RunID, Repo, Pool, Conditions string; Labels []string }`
- `func NewManager(githubClient registrationTokenGetter, secretsStore secrets.Store, config ManagerConfig) *Manager`
- `(m *Manager) PrepareRunner(ctx, PrepareRunnerRequest) error`
- `(m *Manager) CleanupRunner(ctx, instanceID) error`
- Internal: `buildRunnerName(pool, repoName, conditions, jobID)` enforces a 64-char cap and appends a 6-char job-ID suffix.

From `pkg/github/client.go`:

- `type Client struct { ... }`
- `func NewClient(appID, privateKeyBase64 string) (*Client, error)` — accepts PKCS1 or PKCS8 PEM.
- `(c *Client) GetRegistrationToken(ctx, repo string) (*RegistrationResult, error)` — repo-level only; satisfies `runner.registrationTokenGetter`.
- `(c *Client) GetWorkflowJobByID(ctx, repo string, jobID int64) (*WorkflowJobInfo, error)`
- Types: `RegistrationResult { Token string; IsOrg bool /* deprecated */ }`, `WorkflowJobInfo { Status, Conclusion string }`.

## Data [coverage: high -- 5 sources]

**Job record** (`jobRecord` in `pkg/db/jobs.go`, primary key `instance_id` on write / `job_id` as partition key for status ops):

```go
JobID, RunID           int64/int64
Repo, InstanceType     string
Pool                   string  // omitempty
Spot, WarmPoolHit      bool
RetryCount             int
Status                 string  // omitempty; JobStatus value
CreatedAt              string  // RFC3339
SpotRequestID          string  // omitempty
PersistentSpot         bool    // omitempty
TraceID                string  // omitempty, extracted from W3C traceparent
```

**Runner config** (written by `Manager.PrepareRunner` to `secrets.Store` keyed by `instanceID`):

```go
secrets.RunnerConfig{
    Org, Repo, RunID, JITToken, RunnerName, JobID,
    CacheToken, CacheURL, TerminationQueueURL,
    RunnerGroup string,  // omitempty
    Labels      []string,
    IsOrg       bool,
}
```

**Runner name** is derived deterministically:
- Prefix `runs-fleet-runner-`
- Then `<pool>` if pool-bound, else `<repoName>[-<conditions>]` (e.g. `arm64-cpu4-ram16`).
- Last 6 chars of `jobID` appended for uniqueness across same-label jobs in one workflow run.
- Truncated to 64 chars (GitHub's limit).

## Key Decisions [coverage: high -- 5 sources]

- **pkg/state (Valkey) removed, 2026-06** — the K8s runner backend was removed from the codebase entirely; its Valkey-backed `ValkeyStateStore` state store went with it. Job state is now DynamoDB-only. There is no dual-backend split to reconcile; `pkg/db/jobs.go` is the single source of truth.
- **pkg/github relocation, 2026-07 (PR #380)** — `pkg/runner/github.go` moved to `pkg/github/client.go`; `GitHubClient` renamed to `Client`, `NewGitHubClient` renamed to `NewClient`. This is a package-boundary fix: the GitHub API client is not runner-specific logic, and giving it its own package makes room for a possible future second git-hosting provider without overloading `pkg/runner`. No second provider exists today.
- **`registrationTokenGetter` interface seam** — `pkg/runner.Manager` depends on the unexported `registrationTokenGetter` interface (single method, `GetRegistrationToken`), not a concrete `*github.Client`. `*github.Client` satisfies it today; the seam exists so a future non-GitHub git-hosting provider could satisfy the same interface without any change to `Manager`.
- **Self-expiring claim lease** — `ClaimJob` is a compare-and-swap on `created_at`: a claim is re-claimable if the prior record is `requeued`/`terminating`, or a stale `claiming` lease older than `claimStaleThreshold` (100s, above the 90s message-process timeout and below the 120s SQS visibility timeout, so the first redelivery finds the lease already expired). Re-claims are capped at `claimMaxAttempts` (3); beyond that `ClaimJob` returns `ErrJobClaimExhausted` and the caller calls `FailExhaustedClaim` to mark the job terminal.
- **Repo-level registration only** — `GetRegistrationToken` always hits `/repos/{owner}/{repo}/actions/runners/registration-token`. Org-level registration was rejected because runners would otherwise pick up jobs from any repo in the org, causing misassignment (commented at the call site in `pkg/github/client.go`).
- **Bearer for JWT, token for installation** — JWT requests use `Authorization: Bearer`, installation-token requests use `Authorization: token`. Mixing them silently fails because `go-github`'s `WithAuthToken` only emits the `token` prefix.
- **Isolated `http.Client` for go-github calls** — `GetWorkflowJobByID` constructs a new `http.Client` per call rather than reusing `c.httpClient`; `WithAuthToken` mutates the transport with a token-injecting `RoundTripper`, which would corrupt subsequent calls on a shared client.
- **Idempotent cleanup** — `CleanupRunner` is just `secrets.Delete`; safe to call on any path (job done, instance terminated, spot interruption).

## Gotchas [coverage: high -- 5 sources]

- **`JITToken` field name is misleading** — `secrets.RunnerConfig.JITToken` holds a plain repo-level registration token returned by `GetRegistrationToken`, not an actual JIT (`--jitconfig`) config. A real `GetJITConfig` method existed at one point, calling GitHub's `GenerateOrgJITConfig` API, but was deleted as dead code — it was never called in production; GitHub runner registration in this codebase has always used the registration-token flow. The field name is a holdover from that earlier design and should not be taken as a sign the JIT API is in use.
- **Clock skew on JWT** — `generateJWT` backdates `iat` by 60 seconds. Without that buffer, GitHub rejects tokens from hosts with even slight clock drift.
- **Retry policy** — `GetRegistrationToken` and `GetWorkflowJobByID` retry up to `maxRetries` (3) on 429s, 5xx, and network errors with exponential backoff (`baseRetryDelay * 2^attempt`, capped at `maxRetryDelay = 10s`) plus jitter (50–100% of the calculated delay), and honor a server `Retry-After`/rate-limit-reset header (capped at 60s) when present. Non-retryable errors (e.g., a plain 403 permission error) short-circuit immediately.
- **Runner-name collisions** — Multiple matrix jobs with identical `runs-on` labels in the same workflow run used to collide. The 6-char `jobID` suffix prevents that, but it's only the *last* 6 chars; if two jobs in the same run had identical trailing IDs the truncation could collide (extremely unlikely with GitHub job IDs, but worth knowing).
- **64-char truncation** — `buildRunnerName` truncates after appending the suffix. A long pool/repo/conditions combination can chop the unique suffix; verify name uniqueness if you change the prefix scheme.
- **User vs Org installations** — `getInstallationInfo` first tries `/orgs/{owner}/installation`, falls back to `/users/{owner}/installation` on 404. Personal-account repos work, but only via the fallback path. `RegistrationResult.IsOrg` is documented deprecated and now always `false` because registration is always repo-level.
- **`MarkJobComplete` takes a bare status string, not `JobStatus`** — unlike every other transition in `pkg/db/jobs.go`, it accepts `status string` directly and writes it unconditionally; callers are responsible for passing one of the typed `JobStatus*` constants (as strings) rather than an arbitrary value.
- **GitHub job status vs internal job status are different axes** — `WorkflowJobInfo` (from `GetWorkflowJobByID`) carries GitHub's own `Status`/`Conclusion` strings (`queued`/`in_progress`/`completed`, `success`/`failure`/...); this is not the same enum as DynamoDB's `JobStatus` (`launched`/`running`/`claiming`/...). There's no single source of truth combining the two — the orchestrator maps between them at the call site.

## Sources [coverage: high]
- [pkg/db/job_status.go](../../pkg/db/job_status.go)
- [pkg/db/jobs.go](../../pkg/db/jobs.go)
- [pkg/runner/manager.go](../../pkg/runner/manager.go)
- [pkg/github/client.go](../../pkg/github/client.go)
- [pkg/secrets/store.go](../../pkg/secrets/store.go)
