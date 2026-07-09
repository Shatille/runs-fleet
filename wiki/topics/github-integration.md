---
topic: GitHub Integration (Webhooks + App API Client)
last_compiled: 2026-07-09
sources_count: 3
---

# GitHub Integration (Webhooks + App API Client)

## Purpose [coverage: medium -- 3 sources]

`pkg/github` is runs-fleet's entire surface for talking to GitHub: it is both
the front door (receiving `workflow_job` webhooks) and the back channel
(calling GitHub's REST API as a GitHub App). Three files make up the package:

- [pkg/github/webhook.go](../../pkg/github/webhook.go) — HMAC-SHA256 webhook
  validation and `runs-on:` label parsing into a structured `JobConfig`.
- [pkg/github/alias.go](../../pkg/github/alias.go) — custom-label aliasing so
  runs-fleet can transparently absorb workloads from another self-hosted
  runner system.
- [pkg/github/client.go](../../pkg/github/client.go) — a GitHub App API
  client: JWT generation, installation-token exchange/caching, repo-level
  registration-token issuance, and workflow-job status polling.

The package's scope only recently became this broad. Until PR #380
(2026-07), `client.go`'s contents lived in `pkg/runner/github.go`, and this
article covered only webhook validation and label parsing — it also
incorrectly claimed the package issued **JIT (just-in-time) runner tokens**
via GitHub's `GenerateOrgJITConfig` API. That was never true: nothing in this
codebase has ever called that API. A `GetJITConfig` method existed on the
client at one point but was dead code (never called in production) and was
deleted in the same commit that relocated the file (see Gotchas). The correct
description of what this package does is: webhook receipt + label parsing,
and a GitHub App client built around the older repo-level
registration-token endpoint — not JIT.

Downstream consumers: `internal/handler` (webhook business logic),
`cmd/server/main.go` (wires the App client into runner registration and
housekeeping), and `pkg/runner.Manager` (calls the client to obtain
registration tokens for booting agents).

## Architecture [coverage: medium -- 3 sources]

```
GitHub webhook (workflow_job)
        │
        ▼
ParseWebhook (pkg/github/webhook.go)
  ├─ check X-GitHub-Event header
  ├─ check X-Hub-Signature-256 prefix
  ├─ read body with 1MB io.LimitedReader cap
  ├─ ValidateSignature (HMAC-SHA256, hmac.Equal)
  └─ github.ParseWebHook → typed event
        │
        ▼
ParseLabelsWithAliases (pkg/github/webhook.go + alias.go)
  ├─ findMarkerLabel: native "runs-fleet" marker, else
  ├─ resolveAlias: configured AliasResolver match (alias.go)
  ├─ parseLabelParts: cpu/ram/family/gen/arch/pool/spot/disk
  └─ ResolveFlexibleSpec → fleet.ResolveInstanceTypes

Runner registration / housekeeping (pkg/github/client.go)
  ├─ generateJWT (RS256, App ID + private key)
  ├─ getInstallationInfo → cached per-owner installation token
  │    (mutex-protected, refreshed 5 min before expiry)
  ├─ GetRegistrationToken → repo-level registration token
  │    (retry/backoff on secondary rate limits + 5xx)
  └─ GetWorkflowJobByID → workflow-job status
       (used by housekeeping's stale-job detection)
```

Validation, parsing, and the API client are all pure/self-contained in
`pkg/github`: webhook functions take a payload and secret; `Client` methods
take a `context.Context` and return typed results. Side effects (DynamoDB,
SQS, metrics, EC2) live in `internal/handler`, `pkg/runner`, and
`cmd/server`, which call into this package rather than the reverse.

## Talks To [coverage: medium -- 3 sources]

- **GitHub (inbound)** — webhook deliveries authenticated with the
  `X-Hub-Signature-256` header and `RUNS_FLEET_GITHUB_WEBHOOK_SECRET`.
  go-github v57 (`github.com/google/go-github/v57/github`) decodes the JSON
  body into typed events such as `*github.WorkflowJobEvent`.
- **GitHub (outbound)** — `Client` calls the REST API directly over
  `net/http` for JWT-authenticated installation lookup/token-mint requests,
  and via a short-lived `go-github` client (`WithAuthToken`) for
  `GetWorkflowJobByID`.
- **`pkg/fleet`** — `ResolveFlexibleSpec` calls `fleet.ResolveInstanceTypes`
  and `fleet.DefaultFlexibleFamilies` to turn cpu/ram/family/gen/arch labels
  into a concrete instance-type list for spot diversification. Since PR #385
  (2026-07) the default family lists exclude the burstable t3/t4g tiers (see
  Data and Gotchas).
- **`pkg/runner`** — `Manager` holds a `registrationTokenGetter` (satisfied
  by `*github.Client`) and calls `GetRegistrationToken` when booting a
  runner.
- **`pkg/housekeeping`** — `Tasks.gitHubChecker` (interface
  `GitHubJobChecker`) is satisfied by an adapter in `cmd/server/main.go`
  that calls `Client.GetWorkflowJobByID`, feeding the stale-job reconciler.
- **`internal/handler`** — calls `ParseLabelsWithAliases`,
  `HandleWorkflowJobQueued`, `HandleJobFailure` as the webhook business
  logic layer (DynamoDB, SQS, metrics side effects live there, not in this
  package). Since PR #387 (2026-07), `workflow_job` in_progress events are
  also consumed for startup-latency metrics (`HandleWorkflowJobInProgress`,
  filtered by runner-name prefix + a successful label parse) — covered by
  the internal-services and observability articles.

## API Surface [coverage: medium -- 3 sources]

**`webhook.go`:**

- `ValidateSignature(payload []byte, signatureHeader string, secret string) error`
- `ParseWebhook(r *http.Request, secret string) (interface{}, error)`
- `ParseLabels(labels []string) (*JobConfig, error)`
- `ParseLabelsWithAliases(labels []string, resolver *AliasResolver) (*JobConfig, error)`
- `ResolveFlexibleSpec(cfg *JobConfig) error`
- Constants: `ArchARM64 = "arm64"`, `ArchAMD64 = "amd64"`
- Type `JobConfig` — `RunID`, `InstanceType`, `InstanceTypes`, `Pool`,
  `Spot`, `Arch`, `CPUMin`/`CPUMax`, `RAMMin`/`RAMMax`, `Families`, `Gen`,
  `OriginalLabel`, `AliasLabel`, `StorageGiB`.

**`alias.go`:**

- `ParseAliasRules(jsonStr string) (*AliasResolver, error)`
- Type `AliasResolver` with `Resolve(label string) (spec string, ok bool)`
  and `Len() int`
- Type `AliasRule` — `Match`, `Regex`, `Spec`

**`client.go`:**

- `NewClient(appID string, privateKeyBase64 string) (*Client, error)`
- `(*Client) GetRegistrationToken(ctx context.Context, repo string) (*RegistrationResult, error)`
- `(*Client) GetWorkflowJobByID(ctx context.Context, repo string, jobID int64) (*WorkflowJobInfo, error)`
- Type `RegistrationResult` — `Token string`, `IsOrg bool` (deprecated field,
  see Gotchas)
- Type `WorkflowJobInfo` — `Status string`, `Conclusion string`
- Unexported: `generateJWT`, `getInstallationInfo`/`fetchInstallationInfo`/
  `cachedInstallationInfo`/`storeInstallationInfo`, `getInstallationToken`,
  `isRetryableError`, `isSecondaryRateLimited`, `retryDelay`/`backoffDelay`/
  `retryAfterDelay`

The HTTP route for webhooks is wired up in `internal/handler` /
`cmd/server`, outside this package; `pkg/github` is the business logic and
API client invoked by those layers.

## Data [coverage: medium -- 3 sources]

**Inbound webhook headers:**

- `X-GitHub-Event` — required, dispatches to a typed payload via
  `github.ParseWebHook`.
- `X-Hub-Signature-256` — required, must start with `sha256=`; compared with
  `hmac.Equal` against `HMAC-SHA256(secret, payload)`.

**Body limits:** wrapped in `&io.LimitedReader{R: r.Body, N: config.MaxBodySize + 1}`,
rejected over `config.MaxBodySize` (1MB).

**Label format**, parsed by `ParseLabels`/`ParseLabelsWithAliases`. The
marker is recognized in three forms (bare `runs-fleet`, `runs-fleet/...`,
legacy `runs-fleet=<run-id>/...`); the spec parses identically in all three,
and run_id is optional (only the legacy form populates `JobConfig.RunID`,
and it's ignored downstream in favor of the webhook payload's run ID):

```
runs-fleet                                            (marker only, all defaults)
runs-fleet/cpu=4/arch=arm64/pool=default/spot=false   (marker + spec, no run-id)
runs-fleet=<run-id>/cpu=4+16/ram=8+32/family=c7g+m7g  (legacy, run-id carried)
```

Recognized keys: `cpu`, `ram` (both accept `min+max` ranges), `family`
(split on `+`), `gen` (bounded 1..10), `arch` (`arm64`/`amd64`, with
`aarch64`/`x64`/`x86_64` synonyms normalized), `pool`, `spot`, `disk`
(bounded 1..16384). Unknown keys are silently ignored. Defaults:
`Spot=true`, `CPUMin=2`, `CPUMax=2*CPUMin`. When no `family=` is given,
`fleet.DefaultFlexibleFamilies(arch)` supplies the per-arch defaults —
amd64 `c6i/c7i/m6i/m7i`, arm64 `c8g/m8g/r8g/c7g/m7g`, both when arch is
unset — which exclude the burstable t3/t4g tiers as of PR #385 (2026-07);
explicit `family=t3`/`family=t4g` still resolves them from the catalog. A
spec that resolves to zero instance types makes `ResolveFlexibleSpec`
return "no instance types match the specified cpu/ram/family requirements".

**GitHub App API state (`client.go`):**

- `tokenCache map[string]*installationInfo` — installation tokens keyed by
  owner, mutex-protected (`tokenMu`), refreshed 5 minutes
  (`tokenRefreshBuffer`) before actual expiry. `expiresAt` comes from the
  App API's `expires_at` field, or falls back to `tokenDefaultTTL` (50 min)
  when absent/unparseable.
- `RegistrationResult.Token` — a repo-level registration token (POST
  `/repos/{owner}/{repo}/actions/runners/registration-token`), consumed by
  the agent's `config.sh --url ... --token ...` at boot.
- `WorkflowJobInfo{Status, Conclusion}` — polled via GitHub's
  `GetWorkflowJobByID`, consumed by `pkg/housekeeping`'s stale-job
  reconciler through the `GitHubJobChecker` interface.

## Key Decisions [coverage: medium -- 3 sources]

- **HMAC-SHA256 webhook validation.** `ValidateSignature` rejects empty
  secrets, signatures missing the `sha256=` prefix, and uses `hmac.Equal`
  for constant-time comparison.
- **Repo-level registration tokens, not org-level.** `GetRegistrationToken`
  always hits the repo-scoped endpoint so a runner only ever picks up jobs
  from the specific repository; org-level registration would let a runner
  pick up jobs from any repo in the org, causing job misassignment.
- **Installation-token caching.** `getInstallationInfo` caches one token per
  owner and reuses it until it nears expiry, collapsing the per-runner
  GET-installation + POST-access-token calls into one mint per token
  lifetime — those calls are JWT-authed App requests that are the ones most
  likely to trip GitHub's secondary rate limits.
- **1MB body cap.** Defends against memory exhaustion from oversized
  webhook payloads via `io.LimitedReader`.
- **Silent skip on missing label.** `HandleWorkflowJobQueued` (in
  `internal/handler`) returns `(nil, nil)` when no `runs-fleet` marker or
  alias match is found, so jobs not destined for the fleet are ignored
  without producing errors.
- **Retry/backoff on secondary rate limits and 5xx.** `GetRegistrationToken`
  and `GetWorkflowJobByID` retry up to `maxRetries` (3) with exponential
  backoff plus jitter, honoring a server-supplied `Retry-After` or
  `X-RateLimit-Reset` when present (capped at `maxRetryAfterDelay`, 60s). A
  plain 403 without a rate-limit signal is treated as a permission error and
  is not retried.
- **2026-07, PR #380: relocate the GitHub App client into `pkg/github`.**
  The client previously lived in `pkg/runner/github.go`, where the
  package's documented role ("runner registration and secrets") didn't
  match what the code actually did (JWT auth, token caching, GitHub REST
  calls — a GitHub API client, not runner-lifecycle logic). The move also
  introduced `registrationTokenGetter`, an unexported one-method interface
  in `pkg/runner.Manager` (`GetRegistrationToken(ctx, repo) (*github.RegistrationResult, error)`),
  sized to exactly what `Manager` calls. The interface exists ahead of a
  possible second git-hosting provider (e.g. non-GitHub) so `Manager` could
  accept a different implementation without changing `pkg/runner` — no
  second provider exists today, and the interface was deliberately kept
  minimal rather than over-built for hypothetical future needs.
- **2026-07, PR #385: burstable families dropped from label-resolution
  defaults.** Price-weighted spot selection made t3.medium dominate
  family-less requests (a benchmark measured CI 2.25x slower on it), so
  `fleet.DefaultFlexibleFamilies` no longer includes t3 (amd64) or t4g
  (arm64). The decision lives in `pkg/fleet` (see the fleet-orchestration
  article), but its observable effect is here: what a `runs-on:` label
  without `family=` resolves to.
- **Custom label aliases (transparent runner migration).**
  `ParseLabelsWithAliases` lets runs-fleet claim jobs whose `runs-on:`
  carries an externally-defined custom label (e.g. inherited from Actions
  Runner Controller) without changing the workflow. Rules are supplied as a
  JSON array via `RUNS_FLEET_LABEL_ALIASES`, validated once at startup
  (`ParseAliasRules`) so a bad regex or spec fails the boot rather than the
  first matching job. The native `runs-fleet` marker always takes
  precedence over aliases; the matched label is preserved as
  `JobConfig.OriginalLabel` so the booted runner registers under it and
  GitHub dispatches correctly, and it doubles as an auto-created warm-pool
  name (when it's a legal pool name and the alias spec sets no explicit
  `pool=`) so migrated workloads get fast restarts.

## Gotchas [coverage: medium -- 3 sources]

- **This is not the JIT registration flow, despite the naming.**
  `RegistrationResult` has an `IsOrg` field (deprecated, always `false`
  today — repo-level registration is always used) and downstream,
  `secrets.RunnerConfig.JITToken` (`pkg/secrets/store.go`) carries the token
  this package issues. Neither name reflects a real JIT flow: `client.go`
  calls the older repo-level `.../actions/runners/registration-token`
  endpoint, which returns a short-lived *registration* token that the agent
  passes to `config.sh --token`, not a JIT config blob from GitHub's
  `POST .../actions/runners/generate-jitconfig` API. A real `GetJITConfig`
  method existed on this client once (added early, alongside the original
  `pkg/runner/github.go`) but was never called anywhere in production; it
  was deleted as dead code in the same commit (PR #380) that relocated this
  file. If a genuine JIT flow is ever added, expect new methods, not a
  rename of the existing ones.
- **Import alias required outside this package.** The package is named
  `github`, which collides with `github.com/google/go-github/v57/github`.
  Every caller that also imports go-github aliases this package's import
  (`gh "github.com/Shavakan/runs-fleet/pkg/github"` — see
  `cmd/server/main.go`, `internal/handler/webhook.go`) to avoid ambiguity;
  callers that don't need go-github directly (`pkg/pools/manager.go`,
  `pkg/runner/manager.go`) leave it unaliased.
- **Webhook secret must be set.** Empty `RUNS_FLEET_GITHUB_WEBHOOK_SECRET`
  causes `ValidateSignature` to return "webhook secret not configured" —
  every webhook 4xxs until the env var is supplied.
- **Signature header prefix.** GitHub always sends `sha256=<hex>`; a
  deliverer or proxy that strips the prefix produces "signature missing
  sha256= prefix", not a generic 401.
- **1MB hard cap.** Large `workflow_job` payloads can exceed
  `config.MaxBodySize`; the `+1` byte read is the signal used to detect
  overflow.
- **Unknown label keys are silently ignored** by `parseLabelParts`, so
  typos like `arc=arm64` or `pol=default` don't error — they just don't
  take effect.
- **Family-less `gen=3` (amd64) and `gen=4` (arm64) always error.** Those
  generations contain only burstable families (t3 / t4g), which PR #385
  removed from the defaults, so the spec resolves to zero instance types
  and `ResolveFlexibleSpec` errors — the job is skipped with a log, the
  same path as an impossible cpu request. Add an explicit `family=t3` /
  `family=t4g` to opt back in (the types remain in the catalog).
- **Installation-token cache is per-process.** Multiple Fargate tasks each
  hold their own `tokenCache`; there is no shared cache, so horizontal
  scaling multiplies installation-token mint calls (bounded by the 50-min
  TTL / 5-min refresh buffer per task, not shared across tasks).
- **Retryable 403 vs permission error.** A 403 is only retried when it
  carries a `Retry-After` header or an exhausted `X-RateLimit-Remaining: 0`
  budget (`isSecondaryRateLimited`); a plain permission-denied 403 fails
  immediately.
- **Competition during alias-based migration.** While both the original
  runner system (e.g. ARC) and runs-fleet are live for an aliased label,
  they compete for the same queued jobs — whichever registers/claims a
  runner first wins, and the loser's ephemeral runner self-terminates.

## Sources [coverage: medium]

- [pkg/github/webhook.go](../../pkg/github/webhook.go)
- [pkg/github/client.go](../../pkg/github/client.go)
- [pkg/github/alias.go](../../pkg/github/alias.go)
