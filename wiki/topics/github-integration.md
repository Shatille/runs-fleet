---
topic: GitHub Integration (Webhooks + App API Client)
last_compiled: 2026-08-21
sources_count: 5
---

# GitHub Integration (Webhooks + App API Client)

## Purpose [coverage: high -- 5 sources]

`pkg/github` is runs-fleet's entire surface for talking to GitHub: it is both
the front door (receiving `workflow_job` webhooks) and the back channel
(calling GitHub's REST API as a GitHub App). Five non-test files:

- [pkg/github/webhook.go](../../pkg/github/webhook.go) — HMAC-SHA256 webhook
  validation and `runs-on:` label parsing into a structured `JobConfig`.
- [pkg/github/alias.go](../../pkg/github/alias.go) — custom-label aliasing so
  runs-fleet can transparently absorb workloads from another self-hosted
  runner system.
- [pkg/github/client.go](../../pkg/github/client.go) — the GitHub App client
  core: JWT generation, installation-token exchange/caching, repo-level
  registration-token issuance, workflow-job status polling, and the shared
  retry/backoff helpers every other file in the package uses.
- [pkg/github/jitconfig.go](../../pkg/github/jitconfig.go) — **new, PR #424
  (commit 48a0002)** — mints GitHub just-in-time (JIT) runner configs and
  resolves runner-group names to the numeric IDs the JIT endpoint requires.
- [pkg/github/runners.go](../../pkg/github/runners.go) — **new, PR #436
  (commit 70f467c)** — lists and deletes self-hosted runner *registrations*,
  the raw capability behind the orphaned-runner sweep.
- [pkg/github/rerun.go](../../pkg/github/rerun.go) — **new, PR #454
  (commit d74efec)** — asks GitHub to re-run a single workflow job, the only
  recovery route for a job whose runner a spot reclaim killed.

**Correction to prior wiki text (including `wiki/schema.md`).** Earlier
compiles asserted that "no JIT tokens are issued anywhere in this codebase."
That was accurate for the codebase as of 2026-07 but is now **out of date**:
`pkg/github/jitconfig.go` calls
`POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig`
([pkg/github/jitconfig.go:85](../../pkg/github/jitconfig.go)) and the config it
returns is the primary registration path as of PR #424. `schema.md`'s
`github-integration` topic line needs updating.

Downstream consumers: `internal/handler` (webhook business logic),
`cmd/server/main.go` (wires the App client into runner registration,
housekeeping, and the spot-interruption path via three small adapters),
`pkg/runner.Manager` (registration tokens + JIT config),
`pkg/housekeeping` (workflow-job status, runner deregistration), and
`pkg/events` (job re-run after interruption).

## Architecture [coverage: high -- 5 sources]

```
GitHub webhook (workflow_job)
        │
        ▼
ParseWebhook (webhook.go)
  ├─ check X-GitHub-Event header
  ├─ check X-Hub-Signature-256 has "sha256=" prefix
  ├─ read body with 1MB io.LimitedReader cap
  ├─ ValidateSignature (HMAC-SHA256, hmac.Equal)
  └─ github.ParseWebHook → typed event
        │
        ▼
ParseLabelsWithAliases (webhook.go + alias.go)
  ├─ findMarkerLabel: bare "runs-fleet" / "runs-fleet/..." / legacy "runs-fleet=..."
  ├─ else resolveAlias: configured AliasResolver, first match wins (alias.go)
  ├─ labelSpecParts: strip marker (and legacy run-id) → spec segments
  ├─ parseLabelParts: cpu/ram/family/gen/arch/pool/spot/disk
  └─ ResolveFlexibleSpec → fleet.ResolveInstanceTypes

Runner registration (client.go + jitconfig.go)
  ├─ generateJWT (RS256, App ID + private key, 10-min exp, 60s skew buffer)
  ├─ getInstallationInfo → cached per-owner installation token
  │    (mutex-protected, refreshed 5 min before expiry; org endpoint first,
  │     404 → user endpoint for personal accounts)
  ├─ ResolveRunnerGroupID → numeric group ID, cached, Default on any failure
  ├─ GenerateJITConfig → encoded_jit_config (primary path)
  └─ GetRegistrationToken → repo-level registration token (fallback path)

Reconciliation / recovery (client.go + runners.go + rerun.go)
  ├─ GetWorkflowJobByID → Status, Conclusion, RunnerName
  ├─ ListRunners / DeleteRunner → orphaned-registration sweep
  └─ RerunJob → recover a job a spot reclaim killed
```

Every outbound method follows the same hand-written shape rather than an SDK
retry layer: a `for attempt := 0; attempt <= maxRetries; attempt++` loop that
re-fetches the installation token each attempt, issues one HTTP request,
decides retryability from the response (`isRetryableError`), and computes the
next delay with `backoffDelay` (server `Retry-After` / `X-RateLimit-Reset`
capped at 60s, else exponential backoff with jitter). `jitconfig.go`
formalizes the per-attempt outcome as a small `jitAttemptError{err, retryable,
delay}` value so the retry loop and the request body-handling stay separate
([pkg/github/jitconfig.go:132](../../pkg/github/jitconfig.go)).

The package does **no logging of its own** — it is a pure API client. That
constraint is why `ResolveRunnerGroupID` returns its Default-group
substitution as data (`RunnerGroupResolution.FallbackErr`) instead of
swallowing it: only the caller (`pkg/runner.Manager.mintJITConfig`) can make
it visible.

## Talks To [coverage: high -- 5 sources]

- **GitHub (inbound)** — webhook deliveries authenticated with
  `X-Hub-Signature-256` and `RUNS_FLEET_GITHUB_WEBHOOK_SECRET`. go-github v57
  (`github.com/google/go-github/v57/github`) decodes the JSON body into typed
  events such as `*github.WorkflowJobEvent`.
- **GitHub (outbound)** — mostly raw `net/http` against `c.baseURL`
  (default `https://api.github.com`, overridable with `WithBaseURL` for
  GitHub Enterprise Server or a test server). Two methods use a short-lived
  `go-github` client via `WithAuthToken`: `GetWorkflowJobByID` and
  `RerunJob` (which calls `Actions.RerunJobByID`); both re-point
  `ghClient.BaseURL` when `baseURL != defaultBaseURL`.
- **`pkg/fleet`** — `ResolveFlexibleSpec` calls `fleet.ResolveInstanceTypes`
  and `fleet.DefaultFlexibleFamilies` to turn cpu/ram/family/gen/arch labels
  into a concrete instance-type list for spot diversification.
- **`pkg/runner`** — `Manager` holds a `registrationTokenGetter` interface
  and *separately* type-asserts it to the optional `jitConfigGenerator`
  (`GenerateJITConfig` + `ResolveRunnerGroupID`); satisfied by
  `*github.Client`. See [pkg/runner/manager.go:46-62](../../pkg/runner/manager.go).
- **`pkg/housekeeping`** — three seams:
  `GitHubJobChecker` (via `Client.GetWorkflowJobByID`) feeds the stale-job
  reconciler; `RunnerRegistry` (`ListRunners`/`DeleteRunner`, adapted in
  [cmd/server/main.go:967-986](../../cmd/server/main.go)) feeds
  `ExecuteOrphanedRunners`; and `AMIReference` is unrelated to this package.
- **`pkg/events`** — `JobRerunner` interface (`GetWorkflowJobState` +
  `RerunJob`) is satisfied by an `eventsRerunAdapter` in
  [cmd/server/main.go:891](../../cmd/server/main.go); the spot-interruption
  handler polls until GitHub concludes the job, then re-runs it.
- **`internal/handler`** — calls `ParseLabelsWithAliases`,
  `HandleWorkflowJobQueued`, `HandleJobFailure`, and (since PR #387)
  `HandleWorkflowJobInProgress` for startup-latency metrics. All side
  effects (DynamoDB, SQS, metrics) live there, not here.
- **`pkg/secrets`** — not imported by this package, but the shape of
  `secrets.RunnerConfig` is the contract: `JITConfig` (from
  `GenerateJITConfig`) supersedes `RegistrationToken` (from
  `GetRegistrationToken`) when both are present.

## API Surface [coverage: high -- 5 sources]

**`webhook.go`:**

```go
func ValidateSignature(payload []byte, signatureHeader, secret string) error
func ParseWebhook(r *http.Request, secret string) (interface{}, error)
func ParseLabels(labels []string) (*JobConfig, error)
func ParseLabelsWithAliases(labels []string, resolver *AliasResolver) (*JobConfig, error)
func ResolveFlexibleSpec(cfg *JobConfig) error

const ArchARM64 = "arm64"; const ArchAMD64 = "amd64"

type JobConfig struct {
    RunID         string  // only set by the legacy runs-fleet=<run-id> form
    InstanceType  string
    Pool          string
    Spot          bool
    Arch          string
    InstanceTypes []string
    CPUMin, CPUMax int
    RAMMin, RAMMax float64
    Families      []string
    Gen           int
    OriginalLabel string  // the runs-on label as written (marker OR alias)
    AliasLabel    string  // non-empty only when an alias rule matched
    StorageGiB    int
}
```

**`alias.go`:**

```go
func ParseAliasRules(jsonStr string) (*AliasResolver, error)
type AliasRule struct{ Match string; Regex bool; Spec string }
type AliasResolver struct{ /* ordered compiled rules */ }
func (r *AliasResolver) Resolve(label string) (spec string, ok bool)
func (r *AliasResolver) Len() int
```

Unexported but load-bearing: `isValidPoolName` (`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`,
≤63 chars — mirrors `validPoolName` in `cmd/server/main.go`) and
`validateSpec` (parses + resolves a literal spec at config-load time).

**`client.go`:**

```go
func NewClient(appID, privateKeyBase64 string, opts ...Option) (*Client, error)
func WithBaseURL(url string) Option
func (c *Client) GetRegistrationToken(ctx, repo string) (*RegistrationResult, error)
func (c *Client) GetWorkflowJobByID(ctx, repo string, jobID int64) (*WorkflowJobInfo, error)

type RegistrationResult struct{ Token string; IsOrg bool /* deprecated, always false */ }
type WorkflowJobInfo struct{ Status, Conclusion, RunnerName string }
```

**`jitconfig.go`:**

```go
type JITConfigRequest struct{ Name string; RunnerGroupID int64; Labels []string; WorkFolder string }
type RunnerGroupResolution struct{ ID int64; FallbackErr error }

func (c *Client) GenerateJITConfig(ctx, repo string, req JITConfigRequest) (string, error)
func (c *Client) ResolveRunnerGroupID(ctx, repo, groupName string) (*RunnerGroupResolution, error)
```

**`runners.go`:**

```go
type Runner struct{ ID int64; Name, Status string; Busy bool }
func (c *Client) ListRunners(ctx, repo string) ([]Runner, error)
func (c *Client) DeleteRunner(ctx, repo string, runnerID int64) error
```

**`rerun.go`:**

```go
func (c *Client) RerunJob(ctx, repo string, jobID int64) error
```

Shared unexported machinery: `splitRepo`, `generateJWT`,
`getInstallationInfo`/`fetchInstallationInfo`/`cachedInstallationInfo`/
`storeInstallationInfo`, `getInstallationToken`, `isRetryableError`,
`isSecondaryRateLimited`, `retryDelay`/`backoffDelay`/`retryAfterDelay`,
`jitAttemptError`, `runnerGroupCacheKey`/`cachedRunnerGroupID`/
`storeRunnerGroupID`/`storeRunnerGroupIDFor`.

## Data [coverage: high -- 5 sources]

**Inbound webhook headers:** `X-GitHub-Event` (required, dispatches to a typed
payload) and `X-Hub-Signature-256` (required, must start with `sha256=`,
compared with `hmac.Equal`). Body wrapped in
`&io.LimitedReader{R: r.Body, N: config.MaxBodySize + 1}`; the `+1` byte is
the overflow signal for the 1MB cap.

**Label format.** The **bare `runs-fleet` marker is the only required
token**. `findMarkerLabel` accepts exactly three shapes and rejects
prefix-lookalikes (`runs-fleet-foo`, `runs-fleetish`):

```
runs-fleet                                            (marker only, all defaults)
runs-fleet/cpu=4/arch=arm64/pool=default/spot=false   (marker + spec, no run-id)
runs-fleet=<run-id>/cpu=4+16/ram=8+32/family=c7g+m7g  (legacy, run-id carried)
```

The spec parses identically in all three. `run_id` comes from the **webhook
payload**, which is authoritative; the legacy form still populates
`JobConfig.RunID` from the segment after `runs-fleet=`
([pkg/github/webhook.go](../../pkg/github/webhook.go) `labelSpecParts`), and
that value is ignored downstream.

Recognized keys: `cpu`, `ram` (both accept `min+max` ranges), `family` (split
on `+`), `gen` (bounded 1..10), `arch` (`arm64`/`amd64`, with
`aarch64`/`x64`/`x86_64` normalized by `normalizeArch`), `pool`, `spot`
(anything other than the literal `false` means spot), `disk` (bounded
1..16384). Unknown keys are silently ignored. Defaults: `Spot=true`,
`CPUMin=2`, `CPUMax=2*CPUMin`.

**Pool naming.** `JobConfig.Pool` comes from an explicit `pool=` segment, or —
on the alias path only — defaults to the matched alias label when that label
passes `isValidPoolName` and the spec set no `pool=`. It is never derived from
a run id, which is what keeps pool-dimensioned metrics from exploding in
cardinality.

**Label aliases.** `RUNS_FLEET_LABEL_ALIASES` carries a JSON array of
`{match, regex, spec}` rules, parsed once at startup by `ParseAliasRules`
(wired via `config.LabelAliasesJSON`). Purpose: a job migrating off another
runner system (e.g. an ARC scale-set label) keeps its existing `runs-on:`
unchanged and runs-fleet claims it anyway. Regex rules may reference capture
groups in the spec (`$1`, `${1}`, `${name}`, expanded via
`regexp.Regexp.ExpandString`); literal rules have their spec validated at load
so a typo fails the boot rather than the first matching job. Regex rules skip
spec validation because a spec with unexpanded placeholders cannot be parsed.

**GitHub App API state:**

- `tokenCache map[string]*installationInfo` — installation tokens keyed by
  **owner**, mutex-protected (`tokenMu`), refreshed `tokenRefreshBuffer`
  (5 min) before expiry. `expiresAt` from the API's `expires_at`, else
  `tokenDefaultTTL` (50 min).
- `runnerGroupCache map[string]runnerGroupEntry` — keyed by
  `repo + "\x00" + groupName`, mutex-protected (`runnerGroupMu`). Two TTLs:
  `runnerGroupCacheTTL` (30 min) for a successful lookup,
  `runnerGroupFallbackTTL` (**1 min**) for a cached Default substitution — the
  asymmetry is deliberate, because caching a failure misroutes every dispatch
  for that repo into Default until it expires.
- `defaultRunnerGroupID = 1` — GitHub's built-in "Default" group, present on
  every repo, so it is a safe universal fallback.

**Credentials produced:**

| Method | Endpoint | Consumed as |
| --- | --- | --- |
| `GenerateJITConfig` | `POST /repos/{repo}/actions/runners/generate-jitconfig` | `secrets.RunnerConfig.JITConfig` → `ACTIONS_RUNNER_INPUT_JITCONFIG` on `run.sh` |
| `GetRegistrationToken` | `POST /repos/{repo}/actions/runners/registration-token` | `secrets.RunnerConfig.RegistrationToken` (json tag `jit_token`) → `config.sh --token` |

**Pagination bounds in `runners.go`:** `runnersPageSize = 100` (GitHub's max)
and `runnerListPageCap = 20`, so one `ListRunners` call walks at most 2000
registrations; a repo with a larger backlog drains across sweep cycles.

## Key Decisions [coverage: high -- 5 sources]

- **PR #424 (commit 48a0002): JIT config as the primary registration path.**
  Token registration binds a runner to a **label set**, not to a job, so
  GitHub's scheduler could hand a freshly registered runner any other queued
  job that shared its labels — measured at **13% of runners** serving
  someone else's work. A JIT config makes the runner ephemeral (GitHub
  deregisters it after one job) and is generated per dispatch. The agent
  branches on presence: `pkg/agent/registration.go`'s `RegisterRunner` is a
  no-op when `JITConfig != ""` (running `config.sh --replace` anyway could
  disturb the JIT registration GitHub already created), and
  `cmd/agent/main.go` passes the config through
  `ExecuteJobWithConfig`.
- **JIT failures degrade to the token path, never to an error.**
  `Manager.mintJITConfig` returns `""` on every failure shape — a provider
  that doesn't implement `jitConfigGenerator`, a group that won't resolve, a
  `GenerateJITConfig` error — because "a runner that never boots is strictly
  worse than a steal-able one"
  ([pkg/runner/manager.go:176-181](../../pkg/runner/manager.go)). Token
  registration therefore remains fully live code, not dead legacy.
- **Runner group is a placement preference, not a requirement.**
  `ResolveRunnerGroupID` substitutes Default on *any* failure — no access to
  the runner-groups endpoint, name absent from the list — rather than
  returning an error, because "losing it must not cost the job its runner."
  The substitution is surfaced as `FallbackErr` so it stays observable. Group
  matching is case-insensitive (`strings.EqualFold`). An empty group name
  short-circuits to Default with **no API call**.
- **JIT config and API error bodies are never logged.** `GenerateJITConfig`
  reduces every non-2xx to `"jitconfig request failed with status %d"`
  because GitHub's error body can echo the config back;
  `doJITConfigRequest` always closes the body via `defer`. `RerunJob` does
  the same with its status-only error, so an installation token cannot reach
  a log. A 2xx carrying an empty `encoded_jit_config` is treated as a
  non-retryable error, since it would boot a runner that can never register.
- **PR #436 (commit 70f467c): deregister runners that never ran a job.**
  GitHub auto-deletes an ephemeral runner **only after it completes work**, so
  a runner whose instance was terminated before it took a job stays
  registered forever. The unconfirmed-runner watchdog kills at 5 minutes and
  requeues, minting a fresh registration each time — observed at **360 of 369
  registrations in one repo**. `runners.go` supplies the primitives; the
  policy (name-prefix ownership, offline-and-not-busy, durable sighting
  ages, a 200-delete-per-cycle cap) lives in
  [pkg/housekeeping/orphaned_runners.go](../../pkg/housekeeping/orphaned_runners.go).
- **PR #437 (commit 1aa1c24): the offline window is derived, not guessed.**
  `"offline"` does **not** mean dead. A registration exists from the moment
  its JIT config is minted — before the instance boots — so a perfectly live
  runner reads offline for its whole startup, and an agent may sit in standby
  for the standby deadline before taking a job. `Tasks.minOfflineAge()`
  therefore returns `deadAssignmentAge()` — the configured runtime ceiling
  (floored at the 2h standby allowance) plus 2h of slack — rather than a
  constant, so a deploy that raises the ceiling for a slower job class cannot
  leave the sweep deleting live runners
  ([pkg/housekeeping/tasks.go:718-739](../../pkg/housekeeping/tasks.go)).
- **`DeleteRunner` treats 404 as success.** The registration is gone, which is
  all the caller wanted. A runner that picked up work between listing and
  delete is removed by GitHub itself when that job ends, so the race resolves
  to the same state either way.
- **PR #454 (commit d74efec): re-run, don't re-queue, after a spot reclaim.**
  A reclaim kills the runner mid-job and GitHub concludes the job failed
  before any replacement can register; because registration binds to a label
  and not a job, GitHub never hands the dead job to the replacement. `RerunJob`
  deliberately targets the **single job** (`Actions.RerunJobByID`) rather than
  the run's rerun-failed-jobs endpoint, which would also re-run jobs that
  failed for real. Dependent jobs still come along, which is what recovers the
  gate jobs a reclaim cascades into. The caller
  ([pkg/events/rerun.go](../../pkg/events/rerun.go)) gates it hard: spot
  interruption path only, `RetryCount == 0` only, conclusion must be exactly
  `"failure"`, and it waits at most `rerunWaitBudget` (45s, polled every 5s)
  for GitHub to conclude — GitHub's median was 29s but p90 was 169s, so the
  slow tail is left to the re-queue.
- **PR #435 (commit 7375d06): carry the requested `runs-on` label through
  every re-dispatch.** GitHub dispatch is **exact label-set membership**, so a
  requeue that drops the original label produces a runner no job can match —
  runners idle while jobs starve. `JobConfig.OriginalLabel` is the field that
  survives: persisted as `original_label` on the DynamoDB job record
  ([pkg/db/jobs.go:76](../../pkg/db/jobs.go)) and read back by the requeue
  path ([pkg/housekeeping/requeue.go:104](../../pkg/housekeeping/requeue.go)),
  the failure/requeue handler, and the workers. `OriginalLabel` carries both
  the marker form and the alias form, which is exactly why `AliasLabel` exists
  separately — it is the observability discriminator that says *which* alias
  was hit.
- **Repo-level registration tokens, not org-level.** Org-level registration
  would let a runner pick up jobs from any repo in the org.
- **Installation-token caching** collapses the per-runner
  GET-installation + POST-access-token pair (the JWT-authed App requests most
  likely to trip GitHub's secondary rate limits) into one mint per token
  lifetime. `fetchInstallationInfo` tries `/orgs/{owner}/installation` first
  and falls back to `/users/{owner}/installation` on 404 for personal
  accounts.
- **Retry/backoff on secondary rate limits and 5xx.** Up to `maxRetries` (3)
  extra attempts with exponential backoff plus jitter (`baseRetryDelay` 500ms,
  capped at `maxRetryDelay` 10s), honoring `Retry-After` or an exhausted
  `X-RateLimit-Reset` when present (capped at `maxRetryAfterDelay`, 60s). A
  plain 403 without a rate-limit signal is a permission error and is not
  retried.
- **1MB body cap and HMAC-SHA256 validation** — `io.LimitedReader` against
  memory exhaustion; `hmac.Equal` for constant-time comparison; empty secret
  and a missing `sha256=` prefix are distinct errors.
- **Silent skip on missing label.** `HandleWorkflowJobQueued` (in
  `internal/handler`) returns `(nil, nil)` when no marker or alias matches, so
  jobs not destined for the fleet are ignored without producing errors.
- **PR #385: burstable families dropped from label-resolution defaults.** The
  decision lives in `pkg/fleet` (see
  [fleet-orchestration](fleet-orchestration.md)) but its observable effect is
  here: `DefaultFlexibleFamilies` is what a `runs-on:` label without `family=`
  resolves to.
- **No ReDoS guard on alias regexes, deliberately.** Patterns come only from
  operator config parsed once at startup, never from request data, and Go's
  RE2 engine matches in linear time with no backtracking
  ([pkg/github/alias.go](../../pkg/github/alias.go)).

## Gotchas [coverage: high -- 5 sources]

- **JIT config does NOT bind the runner to a job — the two doc comments in
  this repo disagree, and `jitconfig.go` is the correct one.**
  [pkg/github/jitconfig.go:41-48](../../pkg/github/jitconfig.go) states it
  plainly: the generate-jitconfig API takes only name, group, and labels, so
  "GitHub's scheduler hands the runner whichever queued job matches those
  labels. When several jobs share a label set, runners still serve each
  other's jobs." What JIT actually guarantees is *ephemerality* (exactly one
  job, then GitHub deregisters) plus a unique per-dispatch runner name.
  `pkg/runner/manager.go:53-57` and `pkg/secrets/store.go:30-35` both claim
  the stronger property ("bound by GitHub to the single job it was minted
  for") — that is overstated. Convergence still depends on the recovery
  machinery: the termination handler's still-queued redispatch and the
  stale-jobs sweep (PRs #429/#430), plus `RerunJob` (#454) for reclaims.
- **`WorkflowJobInfo.RunnerName` is who *actually* took the job**, which need
  not be the runner runs-fleet minted for it — that is precisely the signal
  the job-theft investigation used.
- **`RegistrationResult.IsOrg` is dead** (always `false`), and
  `secrets.RunnerConfig.RegistrationToken` keeps the json tag `jit_token` on
  purpose: it is a wire contract with agents already in flight that read
  configs written before the rename. The name was always a misnomer — the
  token is a registration token, not a JIT credential. The genuinely-JIT field
  is the separate `jit_config`.
- **`RunnerGroup` is not reachable from config.** `ManagerConfig.RunnerGroup`
  exists and is consulted on the JIT path, but `cmd/server/main.go` leaves it
  empty — there is no `RUNS_FLEET_RUNNER_GROUP` env var. Production runners
  therefore always land in Default, resolved without an API call. Wiring it up
  is a standalone change.
- **Import alias required outside this package.** The package is named
  `github` and collides with `github.com/google/go-github/v57/github`. Callers
  that also import go-github alias it (`gh "github.com/Shavakan/runs-fleet/pkg/github"`
  — see `cmd/server/main.go`, `internal/handler/webhook.go`); callers that
  don't leave it unaliased.
- **Webhook secret must be set.** Empty
  `RUNS_FLEET_GITHUB_WEBHOOK_SECRET` makes `ValidateSignature` return
  "webhook secret not configured" — every webhook 4xxs until it is supplied.
- **Signature header prefix.** A deliverer or proxy that strips `sha256=`
  produces "signature missing sha256= prefix", not a generic 401.
- **Unknown label keys are silently ignored** by `parseLabelParts`, so typos
  like `arc=arm64` or `pol=default` don't error — they just don't take effect.
- **`spot=` is truthy by default.** Only the literal string `false` disables
  spot; `spot=0`, `spot=no`, `spot=FALSE` all leave `Spot=true`.
- **Family-less `gen=3` (amd64) and `gen=4` (arm64) always error.** Those
  generations contain only burstable families (t3 / t4g), which PR #385
  removed from the defaults, so the spec resolves to zero instance types and
  `ResolveFlexibleSpec` errors — the job is skipped with a log. Add an
  explicit `family=t3` / `family=t4g` to opt back in (the types remain in the
  catalog).
- **Installation-token cache is per-process.** Multiple Fargate tasks each
  hold their own `tokenCache`; there is no shared cache, so horizontal
  scaling multiplies mint calls (bounded per task by the 50-min TTL / 5-min
  refresh buffer, not shared across tasks). Same for `runnerGroupCache`.
- **Retryable 403 vs permission error.** A 403 is retried only when it carries
  `Retry-After` or an exhausted `X-RateLimit-Remaining: 0` budget
  (`isSecondaryRateLimited`); a plain permission-denied 403 fails immediately.
- **A misconfigured runner-group name costs one GET per minute per repo,
  silently.** The 1-minute `runnerGroupFallbackTTL` means a permanently
  unresolvable group keeps re-querying GitHub roughly every minute forever,
  and unless the caller logs `FallbackErr` nothing surfaces it — every runner
  just lands in Default.
- **`ListRunners` gives up all pages on one failure.** A page error returns
  `nil, err` and discards the pages already accumulated, so the sweep skips
  that repo entirely for the cycle rather than acting on a partial listing.
- **`RerunJob` re-runs *dependent* jobs too.** That is intentional (it
  recovers cascaded gate jobs) but means one reclaimed job can re-queue a
  subtree of the workflow, and the 45s conclude-wait blocks an events-worker
  message the whole time — sized to leave room inside the 90s
  `config.MessageProcessTimeout`.
- **Competition during alias-based migration.** While both the original runner
  system (e.g. ARC) and runs-fleet are live for an aliased label, they compete
  for the same queued jobs — whichever claims a runner first wins, and the
  loser's ephemeral runner self-terminates.

## Sources [coverage: high]

- [pkg/github/webhook.go](../../pkg/github/webhook.go)
- [pkg/github/client.go](../../pkg/github/client.go)
- [pkg/github/jitconfig.go](../../pkg/github/jitconfig.go)
- [pkg/github/runners.go](../../pkg/github/runners.go)
- [pkg/github/rerun.go](../../pkg/github/rerun.go)
- [pkg/github/alias.go](../../pkg/github/alias.go)
- [pkg/runner/manager.go](../../pkg/runner/manager.go)
- [pkg/secrets/store.go](../../pkg/secrets/store.go)
- [pkg/agent/registration.go](../../pkg/agent/registration.go)
- [pkg/housekeeping/orphaned_runners.go](../../pkg/housekeeping/orphaned_runners.go)
- [pkg/events/rerun.go](../../pkg/events/rerun.go)
- [pkg/db/jobs.go](../../pkg/db/jobs.go)
- [pkg/housekeeping/requeue.go](../../pkg/housekeeping/requeue.go)
- [cmd/server/main.go](../../cmd/server/main.go)
