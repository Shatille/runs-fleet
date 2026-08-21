---
topic: Admin API + UI
last_compiled: 2026-08-21
sources_count: 34
---

# Admin API + UI

## Purpose [coverage: high -- 34 sources]

`pkg/admin` is the REST API plus embedded web console fleet operators work
from. It has moved decisively past read-only: alongside the Phase 1 dashboards
(pools, jobs, instances, queues, circuit breaker, cost, metrics, audit) it now
carries the recovery actions an operator needs when the fleet misbehaves —
per-job requeue and reconcile, manual and drip instance replacement, manual
termination with an active-job guard, an on-demand orphaned-instance reaper, a
GitHub-verified hung-jobs panel, and per-job runner logs pulled from S3.

The through-line of the recent work is that the console had to stop reporting
runs-fleet's *own* record as the truth. Our record says `running` whether the
job is genuinely executing or the runner was handed nothing, so
`handler_hung.go` asks GitHub and reports the verdict (#428), `handler_requeue.go`
lets the operator act on it (#429), `handler_ami.go` answers "has the new AMI
rolled out?" without one click per instance (#431/#432), and PR #455's fleet-cost
card answers what the job-priced total structurally cannot.

**The console reads DynamoDB, EC2, SQS and S3 — never CloudWatch.** There is no
`GetMetricData` or `GetMetricStatistics` call anywhere in `pkg/admin`. Every
figure on the metrics and cost pages is computed from job records, pool records
and sampled fleet-cost rows. This is what makes the console entirely unaffected
by PR #456 shipping the CloudWatch metrics backend disabled by default: turning
that backend off removes no data the console ever read.

## Architecture [coverage: high -- 34 sources]

One handler file per resource type, all sharing a single `AuthMiddleware`, all
registered onto one `adminMux` that `cmd/server/main.go` wraps in the per-IP
rate limiter:

- [handler.go](../../pkg/admin/handler.go) — pool CRUD, the `Handler` type, the
  `PoolDB`/`AuditDB` interfaces, hot-pool override validation against
  `config.HotPoolCaps`, and the shared helpers `writeJSON`, `writeError`,
  `logAdminAction`, `recordAdminAction`, `auditDetailsFromAttrs`, `poolDiff`.
  Also registers `GET /api/audit-logs`.
- [handler_audit.go](../../pkg/admin/handler_audit.go) — the audit query endpoint
  over `pkg/db/audit.go`.
- [handler_jobs.go](../../pkg/admin/handler_jobs.go) — jobs list/detail/stats,
  the `stalled` / `elapsed_seconds` derivation, and `GET
  /api/config/trace-url`.
- [handler_job_logs.go](../../pkg/admin/handler_job_logs.go) (#445) — `GET
  /api/jobs/{id}/logs`: lists the agent-shipped runner logs in S3 and returns
  15-minute pre-signed URLs. Methods hang off `JobsHandler`.
- [handler_hung.go](../../pkg/admin/handler_hung.go) (#428) — `HungHandler`: age
  selects candidates, GitHub returns the verdict.
- [handler_requeue.go](../../pkg/admin/handler_requeue.go) (#429, #422) —
  `RequeueHandler`: the fleet-wide requeue sweep plus per-job `requeue` and
  `reconcile`.
- [handler_instances.go](../../pkg/admin/handler_instances.go) — instance list
  and detail, plus `DELETE` manual termination (#422). Registers the routes
  implemented in the two files below.
- [handler_ami.go](../../pkg/admin/handler_ami.go) (#431) — `GET
  /api/instances/amis`: what each architecture would boot today.
- [handler_replace_stale.go](../../pkg/admin/handler_replace_stale.go) (#432,
  #451) — `POST /api/instances/replace-stale`.
- [handler_housekeeping.go](../../pkg/admin/handler_housekeeping.go) —
  orphaned-job cleanup and the on-demand orphaned-instance reaper (#422), plus
  the shared `parseMaxItems` batch cap (#443).
- [handler_queues.go](../../pkg/admin/handler_queues.go),
  [handler_circuit.go](../../pkg/admin/handler_circuit.go),
  [handler_cost.go](../../pkg/admin/handler_cost.go),
  [handler_metrics.go](../../pkg/admin/handler_metrics.go).
- [auth.go](../../pkg/admin/auth.go), [oidc.go](../../pkg/admin/oidc.go),
  [session.go](../../pkg/admin/session.go),
  [handler_auth.go](../../pkg/admin/handler_auth.go) — native OIDC.
- [ratelimit.go](../../pkg/admin/ratelimit.go),
  [ui.go](../../pkg/admin/ui.go).

Every handler exposes `RegisterRoutes(mux *http.ServeMux)`, takes the shared
`*AuthMiddleware`, and wraps each func in `auth.WrapFunc(...)` — except the four
`/api/auth/*` routes, which are registered unwrapped because they *are* the
login flow. AWS/DB access is mediated by narrow per-handler interfaces (`PoolDB`,
`AuditDB`, `JobsDB`, `InstancesDB`, `EC2API`, `SQSAPI`, `CircuitDynamoAPI`,
`CostDB`, `metricsDB`, `onDemandPricer`, `spotPricer`, `costMTDProvider`,
`LogsS3API`, `LogsPresignAPI`, `GitHubJobStatusChecker`,
`OrphanInstanceSweeper`) so each is unit-testable in isolation.

Two wiring patterns recur in [cmd/server/main.go](../../cmd/server/main.go) and
matter:

- **Typed-nil guards.** `orphanSweeper`, `adminJobChecker` and the fleet-cost
  store are each assigned through a local interface variable only when the
  concrete pointer is non-nil — a nil `*housekeeping.Tasks` or `*gh.Client`
  stored directly would make the interface non-nil and the handler would call a
  client that cannot answer instead of reporting itself unavailable.
- **One shared `*fleet.AMIResolver`.** `instancesHandler.SetAMIResolver(ws.amiResolver)`
  hands the console the same resolver the stale-AMI housekeeping sweep uses, so
  the two cannot cache independently and disagree for a whole TTL about which
  AMI is current.

The UI is a Next.js static export under `pkg/admin/ui/`, embedded via `//go:embed
all:ui/out` and served under `/admin`. Pages: pools (root), `jobs`,
`jobs/detail`, `instances`, `instances/detail`, `queues`, `queues/detail`,
`circuit`, `cost`, `audit`, `metrics`, `pools/new`, `pools/edit`. Detail pages
are query-param routes because a static export cannot do dynamic path segments.
Notable client-side pieces: `lib/api.ts`'s `apiFetch` (credentials, a 401 →
`auth-required` event, and a `RequestTimeoutError` that deliberately does *not*
report a timeout as a failure, because a sweep that outran the browser is still
committing writes); `hooks/use-auto-refresh.ts`; `components/ami-card.tsx`,
`hung-jobs-card.tsx`, `job-actions.tsx`, `job-logs-card.tsx`,
`terminate-instance-button.tsx`, `cost-by-repository-table.tsx`.

## Talks To [coverage: high -- 34 sources]

- **DynamoDB**
  - `runs-fleet-pools` via `PoolDB` — pool CRUD, `GetPoolBusyInstanceIDs`, and
    (via `InstancesDB`) `HasLiveInstanceClaim`. Pool records carry
    `last_reconcile_at`/`last_reconcile_result` and the reconcile-resolved
    `effective_desired_running`/`effective_desired_stopped`, all written by
    `pkg/pools/manager.go`, plus the tuner-written read-only `auto_tune` block
    and the admin-settable `override_linger_minutes`/`override_max_hot`.
  - `runs-fleet-jobs` via `JobsDB` (`ListJobsForAdmin`, `GetJobForAdmin`,
    `GetJobStatsForAdmin`), via `CostDB`/`metricsDB`, via `InstancesDB`'s
    `GetJobByInstance`/`MarkInstanceTerminating`, and via
    `housekeeping.OrphanScanAPI`'s raw `Scan`/`UpdateItem`/`GetItem` for the
    sweeps and per-job actions.
  - `runs-fleet-audit` via `AuditDB` (`RecordAudit`, `ListAuditLogs`,
    `HasAuditTable`), gated by `RUNS_FLEET_AUDIT_TABLE`.
  - `runs-fleet-circuit-state` via `CircuitDynamoAPI.Scan`.
  - Sampled fleet-cost day rows via `cost.FleetCostStore`.
- **EC2** — `DescribeInstances` (always with `tag:runs-fleet:managed=true`),
  `TerminateInstances`, `DescribeSpotInstanceRequests`,
  `CancelSpotInstanceRequests`, and launch-template reads through
  `fleet.AMIResolver`.
- **SQS** — `GetQueueAttributes` only, for the main/pool/events/termination/
  housekeeping queues and the main DLQ. Requeues go out through
  `housekeeping.JobRequeuer` onto the main job queue.
- **S3** — `ListObjectsV2` + `PresignGetObject` for runner logs. Wired from
  `CacheBucketName` with an empty prefix, and keys are built by
  `pkg/agent/logship.BuildPrefix`.
- **GitHub** — via `GitHubJobStatusChecker` (hung classification, per-job status)
  and `housekeeping.JobQueuedChecker` (the queued re-confirmation that gates a
  requeue past `launched`).
- **`pkg/cost` / `pkg/fleet`** — `cost.PriceFetcher` (live on-demand),
  `fleet.Manager.SpotPrice` (live spot), `cost.NewJobPricer` for per-job pricing,
  `cost.ComputeFleetMTDIn` for the fleet block, `fleet.GetInstanceSpec` for the
  arch/vCPU shape matrix.
- **`pkg/housekeeping`** — the console is the on-demand driver for the same
  sweeps the scheduler runs. See [housekeeping](housekeeping.md).
- **An OIDC provider** — the orchestrator is its own relying party.
- **NOT CloudWatch.** No metric-read API is called from this package.

## API Surface [coverage: high -- 34 sources]

Routes are Go 1.22+ method+path patterns on `adminMux`; the whole `/api/` tree
sits behind the per-IP `RateLimiter` (`RUNS_FLEET_ADMIN_RATE_LIMIT`, default
60/min).

**Pools** — `GET /api/pools`, `GET /api/pools/{name}`, `POST /api/pools`,
`PUT /api/pools/{name}`, `DELETE /api/pools/{name}`. Create requires
`Content-Type: application/json` and 409s on an existing name; PUT preserves
`Ephemeral` and `LastJobTime` from the prior record and audits a per-field
`poolDiff`; DELETE is 403 unless `Ephemeral == true`. Hot-pool overrides are
three-state (`nil` = auto, `&0` = force cold, a value = forced) and validated
against the fleet-wide `HotPoolCaps`; `auto_tune` is never accepted from a
client.

**Audit** — `GET /api/audit-logs`: `user`, `action`, `since`/`until` (RFC3339,
unparseable silently ignored), `limit` (default 50, max 100), `offset`. 503 when
the audit table is unconfigured.

**Auth** — `GET /api/auth/config`, `GET /api/auth/login`, `GET
/api/auth/callback`, `POST /api/auth/logout`.

**Jobs** — `GET /api/jobs` (`status` validated against the full `db.JobStatus*`
set, `pool`, `since`, `stale=true`, `stale_minutes`, `limit` default 50 max 100,
`offset`), `GET /api/jobs/stats`, `GET /api/jobs/{id}`, `GET /api/config/trace-url`.

**Job logs** — `GET /api/jobs/{id}/logs`: returns `{logs:[{name,size,
last_modified,url}], expires_in_seconds}`. Prefers the per-job S3 prefix and
falls back to the run prefix filtered by `instance_id`, so a job whose agent had
no `job_id` (keyed under `unknown-job`) is still reachable. 503 when no log
source is wired; every successful read is audited as `job_logs.view`.

**Hung jobs** — `GET /api/jobs/hung` (`stale_minutes` default 15, `limit`
default 25 max 50) returns each candidate with GitHub's status/conclusion, the
runner GitHub actually gave the job to, and a `classification` of
`hung` / `running` / `completed_upstream` / `unknown`, plus `candidates`,
`checked`, `truncated`, `github_available`. `GET /api/jobs/{id}/github` is the
single-job form. GitHub calls fan out at `hungCheckConcurrency = 5`.

**Job recovery** — `POST /api/jobs/{id}/requeue` (`force=true` ignores the retry
cap, never the queued confirmation) and `POST /api/jobs/{id}/reconcile`. Both
return a machine-readable `outcome` plus a `details` sentence, and refusals are
409 rather than 500 so the UI can say *why*: `exhausted`, `wrong_status`,
`not_queued`, `github_unknown`, `github_unavailable`, `no_run_id`, `lost_race`
for requeue; `instance_alive`, `no_instance`, `lost_race` for reconcile.

**Instances** — `GET /api/instances` (`pool`, `state` validated against the six
EC2 state names; default set `pending,running,stopping,stopped`) returning
`busy`, `image_id`, `architecture`, `ami_stale`, plus `warnings[]` for per-pool
busy-lookup failures and `ami_current_unknown: true` when the reference AMI could
not be read. `GET /api/instances/amis` → the per-architecture reference AMI with
launch template, version and version-created, plus `unresolved[]`. `GET
/api/instances/{instance_id}` → detail (AZ, subnet, state reason, full tag map);
IDs must match `i-` + 8 or 17 hex chars. `DELETE
/api/instances/{instance_id}[?force=true]` → 409 with the active job attached
unless forced. `POST /api/instances/replace-stale` (`pool`, `max` default 5 max
25, `dry_run`).

**Queues** — `GET /api/queues` (fixed order main, pool, events, termination,
housekeeping; DLQ depth for main) and `GET /api/queues/{queue_name}`.

**Circuit** — `GET /api/circuit`.

**Cost** — `GET /api/cost/summary`, `/daily`, `/by-pool`, `/by-repository`.

**Metrics** — `GET /api/metrics/summary`.

**Housekeeping** — `POST /api/housekeeping/orphaned-jobs`
(`threshold_minutes` default 120 min 10, `max_items` default 100 max 500,
`dry_run`), `POST /api/housekeeping/orphaned-instances` (`dry_run`), `POST
/api/housekeeping/requeue-hung-jobs` (`threshold_minutes` default 15 min 10,
`max_items`, `dry_run`; `launched` only).

**UI** — `GET /admin/...`.

## Data [coverage: high -- 34 sources]

**`PoolResponse`** returns configured, reconcile-resolved and observed numbers
side by side: `desired_running`/`desired_stopped` (config),
`effective_desired_running`/`effective_desired_stopped` (**pointers** —
absent until a pool has reconciled once, so a client can tell "not yet known"
from "the target is zero"), `current_running`/`current_stopped` from the last
reconcile snapshot, live `busy_instances` computed per request,
`last_reconcile_at`/`last_reconcile_result`, and the hot-pool
`override_*`/`auto_tune` pair. The pools table's help text spells out the
consequence: `busy_instances` is live while the counts are a snapshot, so during
a burst busy can briefly exceed running.

**`JobResponse`** carries `status`, `exit_code`, `duration_seconds`, and
`elapsed_seconds` — the live counterpart written for an *unfinished* job, because
`duration_seconds` is only written at completion and a job hung for six hours
would otherwise report nothing. `stalled` is a boolean derived from
`defaultStaleAfter = 15m` (deliberately longer than housekeeping's own 10m
threshold, so the console does not flag a job the sweep has not had a turn at).

**`HungJob`** = `JobResponse` + `github_status`, `github_conclusion`,
`runner_name`, `classification`, and a `detail` string that is only populated for
`unknown` — the code is explicit that unknown is never a guess.

**Cost shapes.** `CostSummaryResponse` carries MTD totals, `cost_per_minute`,
per-family rows, the `RunnerMinuteEntry` matrix keyed by (arch, vCPU), and an
optional `Fleet *FleetCostBlock`. The runner-minute matrix reports the *incurred*
unit price (#419): `cost` / `cost_per_minute` are what this shape actually cost
divided by its own runner-minutes, so they already reflect the spot/on-demand mix
it ran on; `baseline_cost` / `baseline_cost_per_minute` price the identical
minutes at the hosted per-vCPU-minute reference rate and are *not* runs-fleet
spend. `FleetCostBlock` (PR #455) reports `total_cost`, `compute_cost`,
`ebs_cost`, `attributed_cost`/`unattributed_cost`/`attributed_percent`,
`days_covered`/`days_in_period`, `partial`, a prose `warning`, and
`ebs_estimated` (always true).

All four cost endpoints share `monthToDateJobs`: completed jobs since the start
of the current month **in the configured reporting zone**, not UTC, matching the
zone the fleet sampler writes its day keys in. "Finished" is keyed on
`completed_at`, not a status value, because the stored status is GitHub's raw
conclusion (`success`/`failure`/`interrupted`/…) and all of those burned billable
EC2 time. The query is deliberately unlimited — the former `Limit: 10000` cap is
gone from the cost path (it survives only in `handler_metrics.go`).

**`MetricsSummaryResponse`** — trailing-24h job counts, warm-pool hit rate,
`avg_startup_time_seconds` (mean of `StartedAt - CreatedAt`), an always-flagged
`spot_interruption_rate`, and a nullable `cost_mtd_usd`.

**`runs-fleet-audit`** (`pkg/db/audit.go`) — primary key `id` (ULID, generated
inside `RecordAudit` along with the timestamp so callers cannot fabricate
either), GSI `user-index` on `(user, timestamp)`, attributes `action`, `target`,
`result`, `details` (map), `client_ip`, `timestamp`, and a 90-day `ttl`.
`recordAdminAction` is the single write path for both sinks: the structured slog
line and the DynamoDB row, with the variadic `slog.Attr` extras doubling as
`Details`. Actions now recorded: `pool.create`, `pool.update`, `pool.delete`,
`housekeeping.orphaned-jobs`, `housekeeping.orphaned-instances`,
`housekeeping.requeue_hung_jobs`, `job.requeue`, `job.reconcile`,
`instance.terminate`, `instance.replace_stale`, `job_logs.view` — including every
`denied` and `error` branch.

**Statuses the handlers act on.** The orphaned-jobs sweep treats
running/claiming/launched (and `requeued`, aged on `requeued_at`) as candidates;
`requeue-hung-jobs` sweeps `launched` only; the per-job requeue and reconcile
accept `launched`, `running` and `claiming`, with anything past `launched` gated
on GitHub confirming `queued`.

## Key Decisions [coverage: high -- 34 sources]

- **Native OIDC, no external gatekeeper (2026-07, unchanged).**
  `AuthMiddleware` validates a self-contained, HMAC-signed session cookie
  (`session.go`) minted by `handler_auth.go` after a real authorization-code +
  PKCE exchange (`oidc.go`). This replaced Keycloak-gatekeeper header trust,
  which had replaced Bearer tokens: going open-source meant it could no longer
  assume every self-hoster runs a gatekeeper proxy. Sessions are stateless (no
  shared store, matching the multi-replica design) with a fixed TTL and no
  refresh flow. The user reaches handlers via `UserContextKey`/`GroupsContextKey`
  and `GetUsername(ctx)`.
- **Auth requirement derived from config presence, not a mode flag.**
  `NewAuthMiddleware("")` sets `requireAuth = false`, and the session secret is
  empty exactly when the OIDC config is. Config validation demands the OIDC
  fields be all-set or all-empty, so a partial configuration fails at startup
  rather than silently disabling auth; audit entries then record
  `user=anonymous`.
- **The console asks GitHub, because our own record cannot answer (#428).** A
  job whose runner was minted, confirmed, and then handed nothing reads exactly
  like a healthy long build in the jobs table. So age selects candidates and
  GitHub returns the verdict, and a lookup that fails is classified `unknown`
  with the error text attached rather than guessed. `HungHandler` degrades to
  age-only candidates when no GitHub client is wired instead of failing outright.
- **Watching a hang is not enough; the operator must be able to end it (#429).**
  Per-job `requeue`/`reconcile` drop the sweep's staleness threshold — that floor
  exists to stop a *sweep* acting on jobs that may still be starting, and means
  nothing for a row a human picked — but keep every safety the sweep has, and add
  the queued confirmation for anything past `launched`. Refusals are 409 with a
  sentence, so the UI never has to say "failed".
- **Destructive actions fail closed and report what they left alone.** Manual
  termination 409s with the active job attached unless forced; `replace-stale`
  refuses to touch a running instance at all (EC2 does not re-image on start, so
  a running instance picks up the new AMI when it cycles), checks the pool claim
  *before* the job record (the claim is written before the instance is started,
  so it is the only signal covering that window), treats any lookup failure as
  busy, and returns `terminated`/`busy`/`running`/`skipped` lists instead of
  force-killing (#451). Non-terminable candidates are classified *before* the cap
  is applied, so instances that can never be terminated do not consume the budget
  that exists to avoid draining a pool.
- **A destructive action that partly succeeded must say so.** When
  `replace-stale` hits a mid-loop `TerminateInstances` failure, both the audit
  record and the error the operator sees name the instances already destroyed —
  a bare failure would report a destructive action as having done nothing.
- **Bounded batches the console drains (#443).** `parseMaxItems` (default 100,
  max 500) plus a `truncated` flag on every sweep response; the jobs page loops
  until `truncated` clears, capped at `MAX_DRAIN_BATCHES` and with an idle-batch
  guard, because a batch can legitimately clean nothing while real work sits
  further down the scan. `RequestTimeoutError` exists for the same reason: a
  timed-out sweep is still committing writes, and calling it "failed" sends
  operators to re-run work already done.
- **The on-demand reaper deliberately does not take the housekeeping task lock.**
  The scheduled sweep converges on the same idempotent `TerminateInstances`
  call, so an overlap costs a duplicate terminate — while waiting on a lock would
  make the button feel broken.
- **Runner logs load on click, not on mount (#445).** Each fetch is an audited
  read of material that may carry secrets, so opening a job page must not record
  an access. URLs are 15-minute pre-signs, matching the Actions cache's window.
- **Fleet cost is additive and omitted rather than zeroed (PR #455).**
  `summary.total_cost` stays the job-attributed headline; the fleet block is a
  separate optional card. With no sampler store the field is absent and the page
  renders exactly as before, because a `$0.00` sitting beside a non-zero
  attributed cost would read as "the fleet has no overhead". The attributed share
  comes from the sampler's own busy-vs-total instance-seconds, never from
  dividing the job-priced total by the fleet total — job rows are hard-deleted at
  7 days, so that ratio would decay across a month for calendar reasons.
- **One pricing function for every cost surface.** `cost.JobPricer` (live
  on-demand via `cost.PriceFetcher`, live spot via `fleet.Manager`, hard-coded
  table + fixed discount as fallback) backs summary, daily, by-pool,
  by-repository and `CostMTD` — which in turn feeds the metrics summary — so
  every surface reports the same dollar figure, and `pkg/cost.Reporter`'s daily
  report prices the same records with the same math.
- **Cost months are the operator's months.** A UTC boundary would roll the total
  over mid-morning on the 1st and bucket the daily chart against a day starting
  at 09:00 local, so `SetReportLocation` ties the handler to the same zone the
  fleet sampler writes its day keys in.
- **The console and the housekeeping sweep share one `AMIResolver`.** Two would
  cache independently and could disagree for a whole TTL about which AMI is
  current, so the page could call an instance stale that the sweep considers
  fine, or the reverse.
- **`ApproximateAgeOfOldestMessage` is read from the response, never requested.**
  It is a CloudWatch metric rather than an SQS queue attribute, and naming it in
  `GetQueueAttributes` makes SQS reject the entire call with
  `InvalidAttributeName` — the one place the console brushes against CloudWatch
  is a value AWS hands back unasked.
- **Audit: dual-sink, and persistence never blocks the action.** A failed
  `PutItem` is logged and swallowed; a gap in the trail is a lesser failure than
  blocking an admin action. Feature-gating is `HasAuditTable()`, not a nil
  interface, because `main.go` always wires the shared `*db.Client`.
- **Per-handler interfaces over a god struct**, and **auto-refresh on every page
  unconditionally** (#451) rather than behind a per-page toggle, with a
  `document.hidden` check so a background tab costs nothing.
- **Embedded Next.js export for single-binary deployment**, with query-param
  detail routes because a static export cannot do dynamic path segments.

## Gotchas [coverage: high -- 34 sources]

- **No RBAC beyond authentication.** Session claims carry `Groups` and nothing
  checks them — any authenticated user can terminate instances, force-requeue
  jobs, replace stale instances and read runner logs. Known, unaddressed gap.
- **`RUNS_FLEET_ADMIN_OIDC_GROUPS_CLAIM` defaults to `"groups"`**, correct for
  Keycloak/Okta; Auth0 commonly namespaces it and Google has no groups claim at
  all. Worth checking first if group-based logic ever sees zero groups.
- **Rotating `RUNS_FLEET_ADMIN_SESSION_SECRET` invalidates every live session
  immediately** — the HMAC no longer verifies. Expected, given there is no
  refresh flow, but disruptive mid-shift.
- **Runner logs are read from the *cache* bucket.** `main.go` wires
  `SetLogSource(..., ws.cfg.CacheBucketName, "")` — there is no separate
  runner-logs bucket env var, so a deployment with no cache bucket gets a 503 on
  the logs endpoint, and the log objects share a bucket (and lifecycle) with
  Actions cache artifacts.
- **`GetJobLogs` lists a single `ListObjectsV2` page and does not paginate.** A
  job with more than 1000 log objects would silently show only the first page.
  It also pre-signs every object it lists, so a single failed pre-sign 502s the
  whole response rather than returning the rest.
- **The runner-log run-prefix fallback is instance-filtered by substring.** It
  matches `"/" + instanceID + "/"` inside the key, so it depends on the
  `logship` key layout keeping the instance ID as a whole path segment.
- **`GET /api/jobs/hung` mutates a shared slice from concurrent goroutines.**
  Each goroutine writes only its own `jobs[i]`, which is safe by index, but the
  work is unbounded in count (only `hungCheckConcurrency` limits *in-flight*
  calls) and every call shares the request context — a slow GitHub makes the page
  as slow as its slowest of up to 50 lookups.
- **`spot_interruption_rate` over-counts by design** — the share of spot jobs with
  `retry_count > 0` or status `requeued`, which also counts bootstrap failures and
  stale-claim re-claims. It ships flagged and the UI shows a caveat.
- **The metrics page's "Avg Startup" is not the `JobStartupSeconds` metric.**
  `avgStartupSeconds` averages `StartedAt - CreatedAt` from the DynamoDB record
  (assignment → runner started); the backend metric measures GitHub job created →
  started. They intentionally differ.
- **`/api/metrics/summary` still caps at `Limit: 10000`** while the cost
  endpoints no longer do — a very busy 24h window silently truncates the startup
  and interruption figures.
- **Cost figures remain estimates.** Missing instance types price as
  `t4g.medium`; zero/negative durations bill a 0.5h minimum; live price lookups
  fall back to a 3-family table and a fixed spot discount; the runner-minute
  matrix skips zero-duration jobs, uncatalogued instance types, and arches with
  no configured rate, so its `Cost` column can fall short of `total_cost`.
- **Everything cost- or metrics-related is truncated at 7 days by housekeeping.**
  `ExecuteOldJobs` hard-deletes job rows older than 7 days with no archive, so a
  "month-to-date" total computed from the jobs table only ever sees the last
  week. The fleet-cost block is the one MTD figure not subject to this.
- **DynamoDB `Scan` on the hot path.** `ListCircuitStates`, the orphan-job
  finder, the audit scan fallback, and the `JobsDB` admin queries every cost and
  metrics request fans into are all full-table scans with pagination loops.
- **Audit list ordering is only guaranteed on the user-filtered path.** With a
  `user` filter it queries the sorted `user-index` GSI; without one it scans,
  which DynamoDB does not sort — the unfiltered viewer can show entries out of
  order across pages. Offset pagination is in-memory.
- **Unparseable filter params are silently ignored.** Bad `since`/`until`,
  out-of-range `limit`, negative `offset`, and a sub-10-minute
  `threshold_minutes` all fall back to defaults without a 400 — a typo'd filter
  looks like "no filter". `max_items`, `max`, `limit` (hung) and `stale_minutes`
  are the exceptions and *do* 400.
- **`instanceExists` defaults to true on AWS errors.** Anything other than
  `InvalidInstanceID.NotFound` reads as "exists" — safe, but a long EC2 outage
  masks real orphans.
- **Per-pool failures degrade silently in three places.** `ListInstances`
  collects them into `warnings[]` and leaves `busy: false`; `GetInstance`
  degrades `busy` to `false`; `ListPools` **drops** a pool whose `GetPoolConfig`
  errors, with no warning field at all, so the list can be shorter than the real
  pool count. `ListQueues` at least surfaces a wholesale failure as a 502 when
  *every* queue lookup fails.
- **`ami_stale` is absent, not false, when the reference is unknown.** The list
  response sets `ami_current_unknown: true` and leaves `ami_stale` unset rather
  than marking the fleet current — marking it stale on a transient error would
  invite an operator to replace all of it.
- **`PUT` cannot change `Ephemeral` or `LastJobTime`** (copied from the existing
  record), and **pool-name validation only runs on create** — on `PUT` the name
  comes from the URL and is overwritten onto the request before the branch that
  would validate it.
- **UI 503 on a fresh checkout.** `ui.go` checks for `ui/out/index.html` at
  request time; without `make build-admin-ui` you get 503, not 404. The audit
  page similarly renders a "persistence not configured" notice on a 503.
- **SPA fallback masks 404s for routes but not assets.** A non-matching
  `/admin/...` path falls back to root `index.html` with a 200; `isStaticAsset`
  carves out `/_next/` and anything with an extension so a stale chunk URL 404s
  instead of returning HTML under a `.js` name (which renders an unstyled page
  with no visible error).
- **Double-submit guards are `useRef`, not state.** `ConfirmDialog` stays open on
  confirm and its button carries no pending state, so a key repeat can fire a
  destructive action twice before React re-renders; `ami-card.tsx`,
  `job-actions.tsx` and the jobs page each keep an `inFlight` ref for this.

## Sources [coverage: high]

- [pkg/admin/handler.go](../../pkg/admin/handler.go)
- [pkg/admin/handler_audit.go](../../pkg/admin/handler_audit.go)
- [pkg/admin/handler_jobs.go](../../pkg/admin/handler_jobs.go)
- [pkg/admin/handler_job_logs.go](../../pkg/admin/handler_job_logs.go)
- [pkg/admin/handler_hung.go](../../pkg/admin/handler_hung.go)
- [pkg/admin/handler_requeue.go](../../pkg/admin/handler_requeue.go)
- [pkg/admin/handler_instances.go](../../pkg/admin/handler_instances.go)
- [pkg/admin/handler_ami.go](../../pkg/admin/handler_ami.go)
- [pkg/admin/handler_replace_stale.go](../../pkg/admin/handler_replace_stale.go)
- [pkg/admin/handler_queues.go](../../pkg/admin/handler_queues.go)
- [pkg/admin/handler_circuit.go](../../pkg/admin/handler_circuit.go)
- [pkg/admin/handler_cost.go](../../pkg/admin/handler_cost.go)
- [pkg/admin/handler_metrics.go](../../pkg/admin/handler_metrics.go)
- [pkg/admin/handler_housekeeping.go](../../pkg/admin/handler_housekeeping.go)
- [pkg/admin/auth.go](../../pkg/admin/auth.go)
- [pkg/admin/oidc.go](../../pkg/admin/oidc.go)
- [pkg/admin/session.go](../../pkg/admin/session.go)
- [pkg/admin/handler_auth.go](../../pkg/admin/handler_auth.go)
- [pkg/admin/ratelimit.go](../../pkg/admin/ratelimit.go)
- [pkg/admin/ui.go](../../pkg/admin/ui.go)
- [pkg/db/audit.go](../../pkg/db/audit.go)
- [pkg/cost/jobpricing.go](../../pkg/cost/jobpricing.go)
- [cmd/server/main.go](../../cmd/server/main.go)
- [docs/ADMIN_UI_PLAN.md](../../docs/ADMIN_UI_PLAN.md)
- [pkg/admin/ui/app/page.tsx](../../pkg/admin/ui/app/page.tsx)
- [pkg/admin/ui/app/jobs/page.tsx](../../pkg/admin/ui/app/jobs/page.tsx)
- [pkg/admin/ui/app/jobs/detail/page.tsx](../../pkg/admin/ui/app/jobs/detail/page.tsx)
- [pkg/admin/ui/app/instances/page.tsx](../../pkg/admin/ui/app/instances/page.tsx)
- [pkg/admin/ui/app/cost/page.tsx](../../pkg/admin/ui/app/cost/page.tsx)
- [pkg/admin/ui/app/audit/page.tsx](../../pkg/admin/ui/app/audit/page.tsx)
- [pkg/admin/ui/components/ami-card.tsx](../../pkg/admin/ui/components/ami-card.tsx)
- [pkg/admin/ui/components/hung-jobs-card.tsx](../../pkg/admin/ui/components/hung-jobs-card.tsx)
- [pkg/admin/ui/components/job-actions.tsx](../../pkg/admin/ui/components/job-actions.tsx)
- [pkg/admin/ui/components/job-logs-card.tsx](../../pkg/admin/ui/components/job-logs-card.tsx)
- [pkg/admin/ui/components/pool-table.tsx](../../pkg/admin/ui/components/pool-table.tsx)
- [pkg/admin/ui/components/instances-table.tsx](../../pkg/admin/ui/components/instances-table.tsx)
- [pkg/admin/ui/components/terminate-instance-button.tsx](../../pkg/admin/ui/components/terminate-instance-button.tsx)
- [pkg/admin/ui/lib/api.ts](../../pkg/admin/ui/lib/api.ts)
- [pkg/admin/ui/lib/types.ts](../../pkg/admin/ui/lib/types.ts)
- [pkg/admin/ui/hooks/use-auto-refresh.ts](../../pkg/admin/ui/hooks/use-auto-refresh.ts)
