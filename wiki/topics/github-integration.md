---
topic: GitHub Integration (Webhooks + JIT Tokens)
last_compiled: 2026-04-30
sources_count: 2
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
  ├─ ParseLabels → JobConfig
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
`ResolveFlexibleSpec`) so they are reusable from the HTTP layer, the admin
API, and tests. `internal/handler` owns the side effects (DynamoDB,
SQS, metrics).

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
- `ResolveFlexibleSpec(cfg *JobConfig) error`
- Constants: `ArchARM64 = "arm64"`, `ArchAMD64 = "amd64"`
- Type: `JobConfig` — fields include `RunID`, `InstanceType`,
  `InstanceTypes`, `Pool`, `Spot`, `Region`, `Environment`, `OS`, `Arch`,
  `Backend`, `CPUMin`/`CPUMax`, `RAMMin`/`RAMMax`, `Families`, `Gen`,
  `OriginalLabel`, `StorageGiB`, `PublicIP`.

**Package `internal/handler` (`webhook.go`):**

- `HandleWorkflowJobQueued(ctx, *github.WorkflowJobEvent, queue.Queue, *db.Client, metrics.Publisher) (*queue.JobMessage, error)`
- `HandleJobFailure(ctx, *github.WorkflowJobEvent, queue.Queue, *db.Client, metrics.Publisher) (bool, error)`
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

**Label format** parsed by `ParseLabels`:

```
runs-fleet=<run-id>/cpu=4/arch=arm64/pool=default/spot=true
runs-fleet=<run-id>/cpu=4+16/ram=8+32/family=c7g+m7g/arch=arm64
```

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
  `(nil, nil)` when `ParseLabels` finds no `runs-fleet=` label, so jobs
  not destined for the fleet are ignored without producing errors.
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
- [internal/handler/webhook.go](../../internal/handler/webhook.go)
