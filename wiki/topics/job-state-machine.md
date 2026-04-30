---
topic: Job State Machine
last_compiled: 2026-04-30
sources_count: 3
---

# Job State Machine

## Purpose [coverage: medium -- 3 sources]

runs-fleet tracks each GitHub Actions job through a lifecycle: `queued → assigned → running → completed | failed | terminating | requeued`. Two sub-systems collaborate to make that lifecycle real:

- `pkg/state` provides storage primitives. The Valkey-backed `ValkeyStateStore` is the K8s-mode counterpart to the EC2 mode's DynamoDB jobs table (`pkg/db/jobs.go`).
- `pkg/runner` couples each job to a concrete GitHub Actions runner identity: it mints registration tokens, builds runner names, persists per-instance configuration to the secrets backend, and queries job status against the GitHub API.

Together they answer two operational questions: "what state is this job in?" and "which runner is bound to this instance/pod?".

## Architecture [coverage: medium -- 3 sources]

State is stored in two backends depending on `RUNS_FLEET_MODE`:

- **EC2 mode**: DynamoDB (see `pkg/db/jobs.go`, not in scope here) — durable, conditional-write driven, used for cold-start and warm-pool flows.
- **K8s mode**: Valkey/Redis via `state.ValkeyStateStore`. Pool configuration and live pool state live under a configurable `keyPrefix` (default `runs-fleet:`).

`pkg/runner` is backend-agnostic. It exposes a `Manager` that takes a `secrets.Store` (SSM for EC2, Vault/other for alternative deployments) and a `GitHubClient`. The Manager:

1. Receives a `PrepareRunnerRequest` describing the job (instance, repo, labels, pool, conditions).
2. Calls `GitHubClient.GetRegistrationToken` (repo-level) to mint a registration token.
3. Builds a deterministic runner name and a cache token.
4. Writes a `secrets.RunnerConfig` blob keyed by `instanceID` so the booting agent can fetch it.

When the job ends, `CleanupRunner` deletes the secrets entry. Job-state polling lives in `GitHubClient.GetWorkflowJobByID`, which translates GitHub's API status (`queued`/`in_progress`/`completed`) plus conclusion into a `WorkflowJobInfo` the orchestrator can map to internal states.

## Talks To [coverage: medium -- 3 sources]

- **Valkey/Redis** (`pkg/state/valkey.go`) — `go-redis/v9` client. Used by K8s mode for K8s pool config (`pool:<name>`), the pool index set (`pools:index`), and ephemeral live state (`pool-state:<name>`, 24h TTL).
- **GitHub REST API** (`pkg/runner/github.go`) — three endpoints:
  - `GET /orgs/{owner}/installation` with fallback to `GET /users/{owner}/installation` for personal accounts.
  - `POST /app/installations/{id}/access_tokens` for installation tokens.
  - `POST /repos/{owner}/{repo}/actions/runners/registration-token` — always repo-level to prevent runners from picking up jobs in other repos in the same org.
  - `GenerateOrgJITConfig` (via `go-github`) for JIT runner configs.
  - `GetWorkflowJobByID` (via `go-github`) for status polling.
- **Secrets backend** (`secrets.Store`) — `Put`/`Delete` keyed by EC2 instance ID. Holds the `RunnerConfig` the agent reads at boot.
- **Cache subsystem** (`pkg/cache`) — `cache.GenerateCacheToken` produces a per-job HMAC token scoped to the repo for cache isolation.

## API Surface [coverage: medium -- 3 sources]

From `pkg/state/valkey.go`:

- `type ValkeyStateStore struct { client *redis.Client; keyPrefix string }`
- `func NewValkeyStateStoreWithClient(client *redis.Client, keyPrefix string) *ValkeyStateStore`
- `(s *ValkeyStateStore) GetK8sPoolConfig(ctx, poolName) (*K8sPoolConfig, error)`
- `(s *ValkeyStateStore) SaveK8sPoolConfig(ctx, *K8sPoolConfig) error` — atomic via `TxPipeline` (MULTI/EXEC).
- `(s *ValkeyStateStore) DeleteK8sPoolConfig(ctx, poolName) error` — atomic.
- `(s *ValkeyStateStore) ListK8sPools(ctx) ([]string, error)`
- `(s *ValkeyStateStore) UpdateK8sPoolState(ctx, poolName, arm64Running, amd64Running int) error`
- `(s *ValkeyStateStore) Close() error`
- Types: `K8sPoolConfig`, `K8sPoolSchedule`, `K8sResourceRequests`.

From `pkg/runner/manager.go`:

- `type Manager struct { ... }`
- `type ManagerConfig { CacheSecret, CacheURL, TerminationQueueURL string }`
- `type PrepareRunnerRequest { InstanceID, JobID, RunID, Repo, Pool, Conditions string; Labels []string }`
- `func NewManager(githubClient *GitHubClient, secretsStore secrets.Store, config ManagerConfig) *Manager`
- `(m *Manager) PrepareRunner(ctx, PrepareRunnerRequest) error`
- `(m *Manager) CleanupRunner(ctx, instanceID) error`
- `(m *Manager) GetRegistrationToken(ctx, repo) (*RegistrationResult, error)` — used by the K8s backend before pod creation.
- Internal: `buildRunnerName(pool, repoName, conditions, jobID)` enforces a 64-char cap and appends a 6-char job-ID suffix.

From `pkg/runner/github.go`:

- `type GitHubClient struct { ... }`
- `func NewGitHubClient(appID, privateKeyBase64 string) (*GitHubClient, error)` — accepts PKCS1 or PKCS8 PEM.
- `(c *GitHubClient) GetJITConfig(ctx, org, runnerName, labels) (string, error)`
- `(c *GitHubClient) GetRegistrationToken(ctx, repo) (*RegistrationResult, error)` — repo-level only.
- `(c *GitHubClient) GetWorkflowJobByID(ctx, repo, jobID) (*WorkflowJobInfo, error)`
- Types: `RegistrationResult { Token string; IsOrg bool /* deprecated */ }`, `WorkflowJobInfo { Status, Conclusion string }`.

## Data [coverage: medium -- 3 sources]

**Valkey key layout** (`keyPrefix` default `runs-fleet:`):

| Key | Type | Contents |
| --- | --- | --- |
| `pool:<name>` | string | JSON-encoded `K8sPoolConfig` |
| `pools:index` | set | Set of known pool names |
| `pool-state:<name>` | string (24h TTL) | `{arm64_running, amd64_running, updated_at}` |

`K8sPoolConfig` shape:

```go
PoolName, Arm64Replicas, Amd64Replicas, IdleTimeoutMinutes
Schedules []K8sPoolSchedule  // name, start_hour, end_hour, days_of_week, replicas
Resources K8sResourceRequests // cpu, memory
UpdatedAt time.Time          // stamped on every Save
```

**Runner config** (written by `Manager.PrepareRunner` to `secrets.Store` keyed by `instanceID`):

```go
secrets.RunnerConfig{
    Org, Repo, RunID, JITToken, RunnerName, JobID,
    CacheToken, CacheURL, TerminationQueueURL,
    Labels []string,
    IsOrg  bool,
}
```

**Runner name** is derived deterministically:
- Prefix `runs-fleet-runner-`
- Then `<pool>` if pool-bound, else `<repoName>[-<conditions>]` (e.g. `arm64-cpu4-ram16`).
- Last 6 chars of `jobID` appended for uniqueness across same-label jobs in one workflow run.
- Truncated to 64 chars (GitHub's limit).

## Key Decisions [coverage: medium -- 3 sources]

- **Dual storage** — DynamoDB for EC2 (durable, conditional writes power the lock semantics already documented for pools) and Valkey for K8s (low-latency, fits naturally with K8s controllers). The `Manager` abstracts both: it does not touch state directly, only secrets + GitHub.
- **Atomic Valkey writes** — `SaveK8sPoolConfig` and `DeleteK8sPoolConfig` use `TxPipeline` (MULTI/EXEC) so the index set and the config blob never diverge.
- **Repo-level registration only** — `GetRegistrationToken` always hits `/repos/{owner}/{repo}/actions/runners/registration-token`. Org-level registration was rejected because runners would otherwise pick up jobs from any repo in the org, causing misassignment (commented at the call site).
- **Bearer for JWT, token for installation** — JWT requests use `Authorization: Bearer`, installation-token requests use `Authorization: token`. Mixing them silently fails because `go-github`'s `WithAuthToken` only emits the `token` prefix.
- **Isolated `http.Client` for go-github** — JIT and job-status calls construct a new `http.Client` per call rather than reusing `c.httpClient`; `WithAuthToken` mutates the transport with a token-injecting `RoundTripper`, which would corrupt subsequent calls on the shared client.
- **JIT vs registration token** — K8s mode prefers JIT configs (single-shot, no manual deregister). EC2 mode uses registration tokens written to SSM and consumed by the agent.
- **Idempotent cleanup** — `CleanupRunner` is just `secrets.Delete`; safe to call on any path (job done, instance terminated, spot interruption).

## Gotchas [coverage: medium -- 3 sources]

- **Clock skew on JWT** — `generateJWT` backdates `iat` by 60 seconds. Without that buffer, GitHub rejects tokens from hosts with even slight clock drift.
- **Retry policy** — `GetRegistrationToken` and `GetWorkflowJobByID` retry up to `maxRetries` (3) on 429s, 5xx, and network errors with exponential backoff (`baseRetryDelay * 2^attempt`, capped at `maxRetryDelay = 10s`) plus jitter (50–100% of the calculated delay). Non-retryable errors short-circuit immediately.
- **Runner-name collisions** — Multiple matrix jobs with identical `runs-on` labels in the same workflow run used to collide. The 6-char `jobID` suffix prevents that, but it's only the *last* 6 chars; if two jobs in the same run had identical trailing IDs the truncation could collide (extremely unlikely with GitHub job IDs, but worth knowing).
- **64-char truncation** — `buildRunnerName` truncates after appending the suffix. A long pool/repo/conditions combination can chop the unique suffix; verify name uniqueness if you change the prefix scheme.
- **User vs Org installations** — `getInstallationInfo` first tries `/orgs/{owner}/installation`, falls back to `/users/{owner}/installation` on 404. Personal-account repos work, but only via the fallback path. `RegistrationResult.IsOrg` is documented deprecated and now always `false` because registration is always repo-level.
- **`pool-state:<name>` TTL** — 24h. If the K8s reconciler stops writing for >24h, monitoring queries return nothing; this is intentional but easy to mistake for "pool deleted."
- **Eventual consistency** — Valkey ops aren't strongly serialized across replicas; `TxPipeline` keeps the *index/config* pair consistent on the primary, but readers replaying old replicas can briefly see stale data. The orchestrator tolerates this because reconciliation is idempotent.
- **GitHub job status mapping** — `WorkflowJobInfo` only carries `Status` and `Conclusion` strings. Any internal state (assigned, requeued, terminating) is computed by the orchestrator from these plus instance/pod state — there's no single source of truth combining the two.

## Sources [coverage: medium]
- [pkg/state/valkey.go](../../pkg/state/valkey.go)
- [pkg/runner/manager.go](../../pkg/runner/manager.go)
- [pkg/runner/github.go](../../pkg/runner/github.go)
