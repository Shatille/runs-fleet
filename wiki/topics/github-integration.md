---
topic: GitHub Integration (Webhooks + JIT Tokens)
last_compiled: 2026-04-30
sources_count: 3
---

# GitHub Integration (Webhooks + JIT Tokens)

## Purpose [coverage: high -- 2 sources]

runs-fleet's GitHub integration is the front door of the orchestrator: it
receives `workflow_job` webhooks from GitHub, authenticates them via
HMAC-SHA256, parses the `runs-on:` label vocabulary into a structured
`JobConfig`, and turns each queued job into an SQS message destined for the
fleet manager. Failure events also flow through the same handler so jobs that
died on lost spot capacity can be re-queued onto on-demand instances.

The two files in scope are the validation/parsing primitives in
`pkg/github/webhook.go` and the request handler glue in
`internal/handler/webhook.go`. Together they cover signature verification,
label parsing, ephemeral pool bootstrapping, enqueue, and retry decisions.

## Architecture [coverage: high -- 2 sources]

```
GitHub webhook (workflow_job)
        │
        ▼
ParseWebhook (pkg/github)
  ├─ check X-GitHub-Event header
  ├─ check X-Hub-Signature-256 prefix
  ├─ read body with 1MB io.LimitedReader cap
  ├─ ValidateSignature (HMAC-SHA256, hmac.Equal)
  └─ github.ParseWebHook → typed event
        │
        ▼
HandleWorkflowJobQueued (internal/handler)
  ├─ ParseLabelsWithAliases → JobConfig
  │    (runs-fleet marker, else config-driven custom-label alias)
  ├─ EnsureEphemeralPool (if pool= label set)
  ├─ build queue.JobMessage
  ├─ q.SendMessage (SQS)
  └─ metrics.PublishJobQueued / PublishQueueDepth

HandleJobFailure (internal/handler)
  ├─ filter on runs-fleet- runner name prefix
  ├─ MarkJobRequeuedByJobID (atomic guard)
  └─ q.SendMessage with ForceOnDemand=true
```

Validation is intentionally separated from event handling: `pkg/github`
exports pure functions (`ValidateSignature`, `ParseWebhook`, `ParseLabels`,
`ParseLabelsWithAliases`, `ResolveFlexibleSpec`, `ParseAliasRules`) so they are
reusable from the HTTP layer, the admin API, and tests. `internal/handler` owns
the side effects (DynamoDB, SQS, metrics).

## Talks To [coverage: high -- 2 sources]

- **GitHub** — inbound webhook deliveries authenticated with the
  `X-Hub-Signature-256` header and `RUNS_FLEET_GITHUB_WEBHOOK_SECRET`. The
  go-github v57 library (`github.com/google/go-github/v57/github`) decodes
  the JSON body into typed events such as `*github.WorkflowJobEvent`.
- **SQS** — `queue.Queue.SendMessage` enqueues `queue.JobMessage` records
  for downstream cold-start and pool processors.
- **DynamoDB** — `db.Client` reads/writes pool config (`GetPoolConfig`,
  `CreateEphemeralPool`, `TouchPoolActivity`) and job records
  (`GetJobByJobID`, `MarkJobRequeuedByJobID`, `HasJobsTable`).
- **Metrics** — `metrics.Publisher` emits `PublishJobQueued` and
  `PublishQueueDepth` after every successful enqueue.
- **`pkg/fleet`** — `ResolveFlexibleSpec` calls
  `fleet.ResolveInstanceTypes` and `fleet.DefaultFlexibleFamilies` to turn
  cpu/ram/family/gen/arch labels into a concrete instance-type list for
  spot diversification.

## API Surface [coverage: high -- 2 sources]

**Package `pkg/github` (`webhook.go`):**

- `ValidateSignature(payload []byte, signatureHeader string, secret string) error`
- `ParseWebhook(r *http.Request, secret string) (interface{}, error)`
- `ParseLabels(labels []string) (*JobConfig, error)`
- `ParseLabelsWithAliases(labels []string, resolver *AliasResolver) (*JobConfig, error)`
- `ResolveFlexibleSpec(cfg *JobConfig) error`
- `ParseAliasRules(jsonStr string) (*AliasResolver, error)` (`alias.go`)
- Type `AliasResolver` with `Resolve(label string) (spec string, ok bool)` and `Len() int`; type `AliasRule` (`Match`, `Regex`, `Spec`).
- Constants: `ArchARM64 = "arm64"`, `ArchAMD64 = "amd64"`
- Type: `JobConfig` — fields include `RunID`, `InstanceType`,
  `InstanceTypes`, `Pool`, `Spot`, `Region`, `Environment`, `OS`, `Arch`,
  `Backend`, `CPUMin`/`CPUMax`, `RAMMin`/`RAMMax`, `Families`, `Gen`,
  `OriginalLabel`, `StorageGiB`, `PublicIP`.

**Package `internal/handler` (`webhook.go`):**

- `HandleWorkflowJobQueued(ctx, *github.WorkflowJobEvent, queue.Queue, *db.Client, *gh.AliasResolver) (*queue.JobMessage, error)`
- `HandleJobFailure(ctx, *github.WorkflowJobEvent, queue.Queue, *db.Client, *gh.AliasResolver) (bool, error)`
- `EnsureEphemeralPool(ctx, PoolDBClient, *gh.JobConfig) error`
- `BuildRunnerLabel(*queue.JobMessage) string`
- Interface `PoolDBClient` — `GetPoolConfig`, `CreateEphemeralPool`,
  `TouchPoolActivity` (extracted for testing).
- Constants: `maxJobRetries = 2`, `runnerNamePrefix = "runs-fleet-"`.

The HTTP route itself is wired up outside these two files; this layer is the
business logic invoked by the route handler once an event is received.

## Data [coverage: high -- 2 sources]

**Inbound webhook headers:**

- `X-GitHub-Event` — required, used by `github.ParseWebHook` to dispatch
  to a typed payload struct.
- `X-Hub-Signature-256` — required, must start with `sha256=`. The hex
  digest is compared with `hmac.Equal` against
  `HMAC-SHA256(secret, payload)`.

**Body limits:** the reader is wrapped in
`&io.LimitedReader{R: r.Body, N: config.MaxBodySize + 1}` and rejected with
"request body exceeds 1MB limit" when over `config.MaxBodySize`.

**Label format** parsed by `ParseLabels`. The marker is recognized in three
forms — `findMarkerLabel` matches the bare `runs-fleet`, the new
`runs-fleet/...` form, and the legacy `runs-fleet=<run-id>/...` form (while
rejecting unrelated labels that merely share the prefix, e.g. `runs-fleet-foo`).
The spec is parsed identically for all three; run_id is **optional** in the
label (only the legacy form populates `JobConfig.RunID`):

```
runs-fleet                                            (marker only, all defaults)
runs-fleet/cpu=4/arch=arm64/pool=default/spot=false   (marker + spec, no run-id)
runs-fleet=<run-id>/cpu=4+16/ram=8+32/family=c7g+m7g  (legacy, run-id carried)
```

run_id is sourced from the webhook payload (`event.GetWorkflowJob().GetRunID()`)
in `HandleWorkflowJobQueued`, not the label — the legacy label run-id is ignored.

Recognized keys: `cpu`, `ram` (both accept `min+max` ranges via
`parseRange`/`parseRangeFloat`), `family` (split on `+`), `gen`
(bounded 1..10), `arch` (`arm64`/`amd64`), `pool`, `spot`, `region`,
`env` (`dev`/`staging`/`prod`), `backend` (`ec2`/`k8s`), `disk`
(bounded 1..16384), `public` (parsed via `strconv.ParseBool`). Unknown keys
are silently ignored. Defaults: `Spot=true`, `OS="linux"`,
`CPUMin=2`, `CPUMax=2*CPUMin`.

**Outbound `queue.JobMessage`** fields populated by
`HandleWorkflowJobQueued`: `JobID`, `RunID`, `Repo`, `InstanceType`,
`InstanceTypes`, `Pool`, `Spot`, `OriginalLabel`, `Region`, `Environment`,
`OS`, `Arch`, `StorageGiB`, `PublicIP`, plus the flexible spec
(`CPUMin`/`CPUMax`/`RAMMin`/`RAMMax`/`Families`/`Gen`).

## Custom label aliases (transparent runner migration) [coverage: high -- 1 source]

`ParseLabelsWithAliases` lets runs-fleet claim jobs whose `runs-on:` carries an
**externally-defined custom label** — e.g. a label inherited from another
self-hosted runner system such as Actions Runner Controller (ARC) —
**without changing the workflow**.

**Intent.** This feature exists to support *transparent transfer of existing
self-hosted runners* (such as ARC) onto runs-fleet. Teams keep their current
`runs-on:` labels verbatim; runs-fleet learns to recognize and serve them, so
the original runners can be sunset without touching any workflow manifest.

How it works:

- Operators supply a JSON array of alias rules via `RUNS_FLEET_LABEL_ALIASES`,
  parsed and validated once at startup by `gh.ParseAliasRules` (an
  uncompilable regex or unparsable literal spec fails the boot). The custom
  label vocabulary is deployment-specific, so it lives in config, **never in
  source** — an empty/unset value reproduces pre-alias behavior exactly.
- Each rule maps a label (literal, or `regex:true` with `${1}`/`${name}`
  capture substitution) to an ordinary runs-fleet spec string
  (e.g. `cpu=8+8/arch=arm64`), reusing the same
  `parseLabelParts`/`ResolveFlexibleSpec` path as the native marker. Arch
  synonyms are normalized (`x64`/`x86_64`→`amd64`, `aarch64`→`arm64`).
- The native `runs-fleet` marker always takes precedence; the resolver is only
  consulted when no marker is present, and the first matching rule wins.
- The matched custom label is preserved as `OriginalLabel`, so the booted
  runner registers under that exact label (`config.sh --labels`, no
  `--no-default-labels`) and GitHub dispatches the job to it. Both
  `runs-on: gpu-large` and `runs-on: [self-hosted, gpu-large]` match, because
  GitHub auto-adds `self-hosted`/`Linux`/`ARM64`.
- An aliased label also becomes its own **warm pool**: unless the rule's spec
  sets an explicit `pool=`, the matched label (when it's a legal pool name) is
  set as `cfg.Pool`, so `HandleWorkflowJobQueued` auto-creates an ephemeral pool
  for it (`DesiredRunning=0, DesiredStopped=1`). Migrated workloads (e.g. each
  `shared-Ncpu-arch`) get their own warm pool and fast restarts with no idle
  cost; a label that isn't a valid pool name just cold-starts.
- For observability, `JobConfig.AliasLabel` records the matched custom label
  (empty for native markers) and is emitted on the webhook "job enqueued" log
  line as `alias_label` — so aliased jobs are distinguishable in logs and you
  can see which alias each came in through (useful while watching a migration).

**Caveat — competition during migration.** While both fleets are live, the
original runners (e.g. ARC) and runs-fleet **compete** for the same queued jobs;
whichever registers/claims a runner first wins, and the loser's ephemeral runner
self-terminates.

**Advice.** Once runs-fleet is proven to be serving the jobs correctly,
**scale down the original runners** to stop the duplicate provisioning, then
remove them entirely.

Example — one regex rule deriving the spec for a whole family of labels
(`ci-<N>x-<arch>`) from its captures, pinning vCPU exactly for parity:

```json
[{"match":"^ci-(\\d+)x-(amd64|arm64)$","regex":true,"spec":"cpu=${1}+${1}/arch=${2}"}]
```

## Key Decisions [coverage: high -- 2 sources]

- **HMAC-SHA256 webhook validation.** `ValidateSignature` rejects empty
  secrets, signatures missing the `sha256=` prefix, and uses
  `hmac.Equal` for constant-time comparison.
- **GitHub App auth (vs PAT).** Auth is configured through
  `RUNS_FLEET_GITHUB_APP_ID` and `RUNS_FLEET_GITHUB_APP_PRIVATE_KEY` so
  installation tokens can mint **JIT runner registration tokens**, which
  are scoped to a single ephemeral runner.
- **JIT tokens (single use, ephemeral runners).** Each runner registers
  with a freshly minted JIT token and self-deregisters; this is what
  allows the fleet to remain "no reuse, no state accumulation" as called
  out in the project design principles.
- **1MB body cap.** Defends against memory exhaustion from oversized
  payloads via `io.LimitedReader`.
- **Silent skip on missing label.** `HandleWorkflowJobQueued` returns
  `(nil, nil)` when `ParseLabels` finds no `runs-fleet` marker, so jobs
  not destined for the fleet are ignored without producing errors. A
  mistyped run_id no longer drops the job: run_id comes from the webhook,
  so the label run_id is never re-parsed.
- **Ephemeral pool bootstrap.** When a job carries a `pool=` label and
  no pool config exists, `EnsureEphemeralPool` creates one with
  `DesiredRunning=0`, `DesiredStopped=1`, `IdleTimeoutMinutes=30` so
  pool-aware jobs work without pre-provisioning.
- **Retry policy.** `HandleJobFailure` re-queues at most `maxJobRetries`
  (2) times, gates the requeue with the atomic
  `MarkJobRequeuedByJobID` so duplicate failure events cannot double-
  enqueue, and forces `Spot=false` + `ForceOnDemand=true` on the retry.
- **Recent fix: httpClient transport corruption when using go-github
  `WithAuthToken`** (commit `cb37fba`). The shared `*http.Client`
  transport was being mutated when the GitHub client was constructed
  with `WithAuthToken`; the fix preserves a clean transport so JIT-token
  minting and webhook delivery share a healthy connection pool.

## Gotchas [coverage: medium -- 2 sources]

- **Webhook secret must be set.** Empty
  `RUNS_FLEET_GITHUB_WEBHOOK_SECRET` causes
  `ValidateSignature` to return "webhook secret not configured" — every
  webhook will 4xx until the env var is supplied.
- **Signature header prefix.** GitHub always sends `sha256=<hex>`; if
  the deliverer or proxy strips the prefix you get
  `signature missing sha256= prefix`, not a generic 401.
- **`X-GitHub-Event` is mandatory.** Missing the header short-circuits
  before the body is read.
- **1MB hard cap.** Very large `workflow_run` payloads or attached
  metadata can exceed `config.MaxBodySize`. The `+1` byte read is the
  signal used to detect overflow.
- **Unknown label keys are silently ignored** by `parseLabelParts`, so
  typos like `arc=arm64` or `pol=default` will not error — they just
  don't take effect.
- **Windows/arm64 mismatch.** `ParseLabels` rejects
  `os=windows` combined with a non-amd64 `arch=`, returning
  "windows runners only support amd64 architecture".
- **Replay protection is HMAC only.** runs-fleet does not deduplicate
  by `X-GitHub-Delivery`; downstream idempotency relies on
  DynamoDB job records and the `MarkJobRequeuedByJobID` conditional
  write.
- **Runner-name filter on failure path.** `HandleJobFailure` only acts
  when `runnerName` starts with `runs-fleet-`, so jobs that failed
  before a runner attached produce no requeue.
- **GitHub App rate limits and JIT token expiry.** Installation tokens
  rotate on a 1-hour window and JIT registration tokens are short-lived
  by design — the agent must register promptly after boot.

## Sources [coverage: high]

- [pkg/github/webhook.go](../../pkg/github/webhook.go)
- [pkg/github/alias.go](../../pkg/github/alias.go)
- [internal/handler/webhook.go](../../internal/handler/webhook.go)
