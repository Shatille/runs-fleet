---
topic: Admin API + UI
last_compiled: 2026-07-21
sources_count: 31
---

# Admin API + UI

## Purpose [coverage: high -- 30 sources]

`pkg/admin` exposes a REST API plus a self-contained web dashboard for fleet
operators. It provides the day-to-day surface for working with the runs-fleet
control plane: pool configuration (CRUD), inspecting jobs, instances (list and
detail), SQS queues (list and detail), circuit breaker state, cost reporting
(month-to-date summary, daily series, per-pool breakdown), an aggregate
metrics summary, a queryable audit log, and two operator-triggered
housekeeping actions. The dashboard is the operational counterpart to the
orchestrator running in `cmd/server/`.

The current code completes Phase 1 ("Visibility") of `docs/ADMIN_UI_PLAN.md`:
all read dashboards -- jobs, pools (including reconcile status), instances,
queues, circuit, cost, metrics, audit viewer -- are built (PR #384,
2026-07-07), and the audit trail is persisted to DynamoDB with a query API
and UI page (PR #383, 2026-07-06). Phase 2 (write actions) remains largely
unbuilt: only orphaned-job cleanup and hung-job requeue are wired; manual
instance termination, circuit reset, forced reconciliation, and DLQ redrive
are still planned.

## Architecture [coverage: high -- 31 sources]

The package is organised one handler file per resource type, all sharing a
single `AuthMiddleware` instance:

- `handler.go` -- pool CRUD, the `Handler` type, shared helpers
  (`writeJSON`, `writeError`, `logAdminAction`, `recordAdminAction`,
  `poolDiff`), validation, request/response DTOs, and the `AuditDB`
  interface. Also registers `GET /api/audit-logs`.
- `handler_audit.go` -- `Handler.ListAuditLogs`, the audit query endpoint
  over `pkg/db/audit.go`.
- `handler_jobs.go` -- `JobsHandler`, jobs list / detail / stats, plus
  `GET /api/config/trace-url` (trace-UI deep links; `trace_id` on job
  responses).
- `handler_instances.go` -- `InstancesHandler`, EC2-backed instance listing
  and single-instance detail.
- `handler_queues.go` -- `QueuesHandler`, SQS queue depth/DLQ snapshot for
  the list and a per-queue detail with oldest-message age; both resolve
  names through a shared `queueByName` mapping so they cannot drift.
- `handler_circuit.go` -- `CircuitHandler`, scans the circuit-state table.
- `handler_cost.go` -- `CostHandler`, month-to-date cost summary, daily
  series, and by-pool breakdown. Per-job pricing was extracted to the shared
  `cost.JobPricer` (`pkg/cost/jobpricing.go`, PR #390): each compute func
  opens with `cost.NewJobPricer(h.onDemand, h.spot)` (live on-demand/spot
  prices with hard-coded fallback); the handler's former `priceJob` /
  `jobPricing` / `spotResult` are gone. Family/shape/pool aggregation stayed
  in the handler.
- `handler_metrics.go` -- `MetricsHandler`, the aggregate operational
  snapshot (`/api/metrics/summary`); reuses `CostHandler.CostMTD`.
- `handler_housekeeping.go` -- `HousekeepingHandler`, orphaned-job cleanup.
- `handler_requeue.go` -- `RequeueHandler`, operator-triggered requeue of
  hung (launched-but-never-confirmed) jobs.
- `auth.go` -- session-cookie-validating `AuthMiddleware` (as of 2026-07;
  previously Keycloak gatekeeper header extraction).
- `oidc.go`, `session.go`, `handler_auth.go` -- native OIDC: discovery/
  exchange/verify, HMAC-signed session cookies, and the
  `/api/auth/{login,callback,logout,config}` endpoints (added 2026-07).
- `ratelimit.go` -- per-IP token-bucket `RateLimiter`; `cmd/server/main.go`
  wraps the entire admin `/api/` mux with it
  (`RUNS_FLEET_ADMIN_RATE_LIMIT`, default 60/min).
- `ui.go` -- serves the embedded Next.js static export.

Each handler has its own `RegisterRoutes(mux *http.ServeMux)` method, takes
the shared `*AuthMiddleware`, and wraps every handler func with
`auth.WrapFunc(...)`. AWS SDK access is mediated by small per-handler
interfaces (`PoolDB`, `AuditDB`, `JobsDB`, `EC2API`, `SQSAPI`,
`CircuitDynamoAPI`, `CostDB`, `metricsDB`, `onDemandPricer`, `spotPricer`,
`costMTDProvider`, housekeeping's `OrphanEC2API`/`OrphanScanAPI`) so each can
be unit-tested independently.

Logging is split: regular operational logs use
`logging.WithComponent(LogTypeAdmin, "<handler>")`, while audit events go
through the dedicated `auditLog = logging.WithComponent(LogTypeAdmin, "audit")`
sink in `handler.go` -- and, since PR #383, are additionally persisted to
DynamoDB via `recordAdminAction` (see Data).

The UI is a Next.js app under `pkg/admin/ui/` whose static export lives at
`pkg/admin/ui/out/`. `ui.go` embeds it via `//go:embed all:ui/out` and serves
it under `/admin` with SPA-style fallback to `index.html`. If the bundle has
not been built, the handler returns 503 with the message "Admin UI not built.
Run: make build-admin-ui". Pages live under `ui/app/` (pools at the root,
plus `jobs`, `jobs/detail`, `instances`, `instances/detail`, `queues`,
`queues/detail`, `circuit`, `cost`, `metrics`, `audit`); detail pages are
query-param routes (static export cannot do dynamic path segments). Shared
formatting helpers (`formatRelativeTime`, `formatTimestamp`,
`formatDuration`) live in `ui/lib/format.ts`; API response types in
`ui/lib/types.ts`. The cost page's daily bar chart is hand-rolled -- no chart
dependency was added.

## Talks To [coverage: high -- 31 sources]

- **DynamoDB**
  - `runs-fleet-pools` via `PoolDB` (`ListPools`, `GetPoolConfig`,
    `SavePoolConfig`, `DeletePoolConfig`, `GetPoolBusyInstanceIDs`). Pool
    records now carry `last_reconcile_at`/`last_reconcile_result`, written
    by the pool reconcile loop (`pkg/pools/manager.go`) via a targeted
    `UpdatePoolReconcileResult`.
  - `runs-fleet-jobs` via `JobsDB` (admin-specific scan/query helpers
    `ListJobsForAdmin`, `GetJobForAdmin`, `GetJobStatsForAdmin`), via
    housekeeping's `OrphanScanAPI` raw `Scan` + `UpdateItem`, and via
    `CostDB`/`metricsDB` (the cost and metrics handlers derive everything
    from completed-job records -- no separate telemetry store).
  - `runs-fleet-audit` via `AuditDB` (`RecordAudit`, `ListAuditLogs`,
    `HasAuditTable`) -- implemented in `pkg/db/audit.go` (PR #383),
    provisioned in `deploy/terraform/dynamodb.tf`, gated by
    `RUNS_FLEET_AUDIT_TABLE`.
  - `runs-fleet-circuit-state` via `CircuitDynamoAPI.Scan`.
- **EC2** -- `DescribeInstances` for the instances list and detail views
  (`runs-fleet:managed=true` tag filter) and for housekeeping/requeue
  cross-checks of candidate jobs.
- **SQS** -- `GetQueueAttributes` for visible/in-flight/delayed counts (and
  oldest-message age) on the main, pool, events, termination, and
  housekeeping queues, plus the main queue DLQ. The requeue handler also
  re-enqueues jobs into the main queue via `housekeeping.JobRequeuer`.
- **Live EC2 pricing via `pkg/cost`** -- `CostHandler` takes an
  `onDemandPricer` (satisfied by `pkg/cost.PriceFetcher`) and a `spotPricer`
  (satisfied by `pkg/fleet.Manager.SpotPrice`) and delegates the pricing math
  to `cost.JobPricer` (PR #390); both pricers are nil-safe with fallback to
  the hard-coded price table and fixed spot discount.
- **An OIDC provider** (Keycloak, Auth0, Okta, Google, or any standards-
  compliant issuer) -- the orchestrator is its own OIDC relying party as of
  2026-07 (authorization-code + PKCE, `pkg/admin/oidc.go`). No external
  gatekeeper or reverse proxy in front is required.

## API Surface [coverage: high -- 30 sources]

All routes are registered against an `http.ServeMux` using Go 1.22+ method +
path patterns. Every route is wrapped by `AuthMiddleware`, and the whole
`/api/` mux sits behind the per-IP `RateLimiter`.

Pools (`handler.go::Handler.RegisterRoutes`):

- `GET /api/pools` -- list pools with `current_running`, `current_stopped`,
  live `busy_instances` count, and `last_reconcile_at` /
  `last_reconcile_result`.
- `GET /api/pools/{name}` -- single pool, same enrichment.
- `POST /api/pools` -- create. Validates name, requires
  `Content-Type: application/json`, body cap 1 MB, 409 if exists.
- `PUT /api/pools/{name}` -- update. Preserves `Ephemeral` and `LastJobTime`
  from existing record; logs a per-field diff via `poolDiff`.
- `DELETE /api/pools/{name}` -- only allowed when `Ephemeral == true`;
  returns 403 otherwise.
- `GET /api/audit-logs` -- query the persisted audit trail. Filters: `user`,
  `action`, `since`/`until` (RFC3339; unparseable values silently ignored),
  `limit` (default 50, max 100), `offset`. Returns
  `{entries, limit, offset}`; 503 when `RUNS_FLEET_AUDIT_TABLE` is unset.

Jobs (`handler_jobs.go::JobsHandler.RegisterRoutes`):

- `GET /api/jobs` -- filters: `status`, `pool`, `since` (RFC3339), `limit`
  (default 50, max 100), `offset`. Returns `{jobs, total, limit, offset}`.
- `GET /api/jobs/stats` -- 24-hour default window via `since` query.
  Computes `hit_rate = warm_pool_hit / completed`.
- `GET /api/jobs/{id}` -- 64-bit integer job ID.
- `GET /api/config/trace-url` -- the configured trace-UI base URL
  (`RUNS_FLEET_TRACE_UI_URL`) so the UI can deep-link `trace_id`s.

Instances (`handler_instances.go::InstancesHandler.RegisterRoutes`):

- `GET /api/instances` -- filters: `pool`, `state` (default set
  `pending,running,stopping,stopped`). Always filters by tag
  `runs-fleet:managed=true`. Cross-references each pool's busy instance IDs
  to populate the `busy` flag; failures per pool are reported in a
  `warnings` array rather than failing the whole request.
- `GET /api/instances/{instance_id}` -- single-instance detail: list fields
  plus availability zone, image ID, subnet, architecture, state reason, and
  the full tag map. IDs must match `i-` + 8 or 17 hex chars (400 otherwise);
  the managed-tag filter means unmanaged and unknown IDs both 404.

Queues (`handler_queues.go::QueuesHandler.RegisterRoutes`):

- `GET /api/queues` -- iterates main, pool, events, termination,
  housekeeping in fixed order. For the main queue it also fetches DLQ depth
  (`dlq_messages`). Per-queue failures are logged but skipped silently.
- `GET /api/queues/{queue_name}` -- single queue by name via the shared
  `queueByName` mapping (unknown or unconfigured names 404); reports
  `oldest_message_age_seconds` alongside the depth counts.

Circuit breaker (`handler_circuit.go::CircuitHandler.RegisterRoutes`):

- `GET /api/circuit` -- paginated `Scan` of the circuit-state table.
  Returns an empty list with a `message` if `tableName` is empty.

Cost (`handler_cost.go::CostHandler.RegisterRoutes`):

- `GET /api/cost/summary` -- month-to-date totals over completed jobs:
  spot/on-demand split, spot savings, avg cost per job, per-family
  breakdown, plus a runner-minute cost matrix keyed by (arch, vCPU) with
  the configured per-vCPU-minute rates.
- `GET /api/cost/daily` -- per-day series for the current UTC month,
  bucketed by job `CreatedAt` and zero-filled through today so the UI can
  chart a continuous series; daily totals sum to the summary's total.
- `GET /api/cost/by-pool` -- month-to-date cost grouped by pool, poolless
  cold-start jobs under the `"cold-start"` pseudo-pool, sorted by total
  cost descending.

Metrics (`handler_metrics.go::MetricsHandler.RegisterRoutes`):

- `GET /api/metrics/summary` -- trailing-24h job counts
  (total/completed/failed/in-progress), warm-pool hit rate,
  `avg_startup_time_seconds`, a best-effort `spot_interruption_rate`
  (always flagged `spot_interruption_rate_estimated: true`), and
  `cost_mtd_usd` (omitted -- not zero -- when the cost lookup fails).

Housekeeping (`handler_housekeeping.go`, `handler_requeue.go`):

- `POST /api/housekeeping/orphaned-jobs` -- query params
  `threshold_minutes` (default 120, minimum 10) and `dry_run=true`. Scans
  the jobs table for `status in (running, claiming) AND created_at < cutoff`,
  batch-checks via `DescribeInstances` (batch size 100), and conditionally
  flips status to `orphaned` with `completed_at = now`.
- `POST /api/housekeeping/requeue-hung-jobs` -- query params
  `threshold_minutes` (default 15, minimum 10) and `dry_run=true`.
  Re-dispatches a fresh runner for jobs stuck in `launched` (runner never
  confirmed) by terminating the dead instance and re-enqueuing into the
  main queue, bounded by `housekeeping.MaxRequeueRetries`. Never touches
  `running` jobs or the GitHub-side job/run.

UI (`ui.go::UIHandler`):

- `GET /admin/...` -- serves the embedded Next.js export with directory ->
  `index.html` resolution and SPA fallback to root `index.html`.

## Data [coverage: high -- 31 sources]

**Pool response shape** (`PoolResponse` in `handler.go`) -- the API returns
both desired and observed counts: `desired_running`, `desired_stopped`,
`current_running`, `current_stopped`, plus `busy_instances` computed from
`PoolDB.GetPoolBusyInstanceIDs(pool)` at request time, and reconcile status
(`last_reconcile_at` as a nullable timestamp, `last_reconcile_result` as
`"success"` / `"failed: <err>"`). Resource specs (`cpu_min/max`,
`ram_min/max`, `families`, `arch`) and `schedules` are passed through
verbatim.

**Job response shape** (`JobResponse` in `handler_jobs.go`) -- exposes
`status`, `exit_code`, `duration_seconds`, `warm_pool_hit`, `retry_count`,
`trace_id`, plus `created_at`/`started_at`/`completed_at` (omitted when
zero). The underlying `db.AdminJobEntry` and `db.AdminJobStats` are
admin-specific projections that live in `pkg/db`.

**`runs-fleet-audit` table** (`pkg/db/audit.go`,
`deploy/terraform/dynamodb.tf`) -- implemented in PR #383, closely following
the plan. Primary key `id` (ULID, time-sortable, generated on write along
with the timestamp -- callers never fabricate either); GSI `user-index` on
`(user, timestamp)`; attributes `action`, `target`, `result`, `details`
(map), `client_ip`, `timestamp` (RFC3339 string), and a `ttl` epoch-seconds
attribute set 90 days out (`auditRetention`) so DynamoDB TTL reaps entries
with no cleanup job. `RUNS_FLEET_AUDIT_TABLE` unset disables persistence
entirely (slog-only); `HasAuditTable()` is the presence check.
`ListAuditLogs` takes the sorted GSI query path when a `user` filter is
given (falling back to `Scan` on GSI validation errors), otherwise scans;
`action`/`since`/`until` become FilterExpressions and `offset`/`limit` are
applied in memory.

**Audit write path** -- `recordAdminAction` (in `handler.go`) does both
sinks: the structured slog line via `logAdminAction` (fields `audit=true`,
`user`, `action`, `pool_name`/target, `result`, `remote_addr`;
`success`→INFO, `denied`→WARN, `error`→ERROR) and DynamoDB persistence,
where the same variadic `slog.Attr` extras double as the persisted entry's
`Details` map (`auditDetailsFromAttrs`). Persistence failures are logged but
never fail the parent request. Actions recorded: `pool.create`,
`pool.update` (with the `poolDiff` change string), `pool.delete`, and
`housekeeping.orphaned-jobs`; every denied/error branch is recorded too, and
`user` falls back to `"anonymous"` when auth is disabled.

**Cost response shapes** (`handler_cost.go`) -- `CostSummaryResponse`
carries the MTD totals, per-family `FamilyBreakdownEntry` rows, and the
runner-minute matrix (`RunnerMinuteEntry` per (arch, vCPU), plus the rate
map). `CostDailyResponse.Days` is zero-filled `CostDayEntry` rows;
`CostByPoolResponse.Pools` is `CostPoolEntry` rows. All three endpoints
share `monthToDateJobs` (completed jobs since the start of the current UTC
month, `Limit: 10000`) and a per-request `cost.JobPricer` (memoizes live
on-demand/spot lookups per instance type; jobs with no instance type price
as `t4g.medium`; durations ≤ 0 bill as 0.5h minimum).

**Metrics summary shape** (`MetricsSummaryResponse` in
`handler_metrics.go`) -- `jobs_24h` counts from `GetJobStatsForAdmin`,
`warm_pool_hit_rate` = warm-pool hits / completed, `avg_startup_time_seconds`
averaged over `StartedAt - CreatedAt` where both are set and sanely ordered,
and the estimated spot-interruption share (spot jobs with `retry_count > 0`
or status `requeued`). `cost_mtd_usd` is a nullable pointer so the UI can
distinguish "unavailable" from a real $0.00.

**Job statuses observed in handlers** -- the housekeeping handler treats
`running` and `claiming` as the "in-flight" set when looking for orphans and
writes `orphaned` via a conditional update so racing runners cannot lose
real completions; the requeue handler deliberately sweeps only `launched`.

**Circuit DynamoDB record** (`circuitRecord`) -- has fields
`instance_type`, `state`, `interruption_count`, `first_interruption_at`,
`last_interruption_at`, `opened_at`, `auto_reset_at`. The API renames
`interruption_count -> failure_count` and `auto_reset_at -> reset_at` for
display.

## Key Decisions [coverage: high -- 31 sources]

- **Native OIDC, no external gatekeeper (2026-07).** `auth.go`'s
  `AuthMiddleware` validates a self-contained, HMAC-signed session cookie
  (`pkg/admin/session.go`) minted by `handler_auth.go` after a real
  authorization-code + PKCE exchange (`pkg/admin/oidc.go`) against the
  configured OIDC provider. This replaced the earlier Keycloak-gatekeeper
  header-trust model, which itself had replaced the original Bearer-token
  auth -- the project going open-source meant it could no longer assume
  every self-hoster would stand up their own gatekeeper proxy. Sessions
  are stateless (no shared session store, matching the orchestrator's
  multi-Fargate-replica design) and have no refresh-token renewal: a fixed
  TTL, then re-login. The middleware exposes the user via context
  (`UserContextKey`, `GroupsContextKey`) and a `GetUsername(ctx)` helper --
  unchanged from the header-trust era, so audit call sites needed
  no changes.
- **Auth requirement derived from OIDC config presence, not a mode flag.**
  `NewAuthMiddleware("")` sets `requireAuth = false`; the session secret is
  empty exactly when `RUNS_FLEET_ADMIN_OIDC_ISSUER_URL` (and the rest of
  the OIDC config) is unset, letting you run the server locally without an
  IdP. Config validation requires the OIDC fields to be all-set or
  all-empty -- a partial configuration fails at startup rather than
  silently disabling auth. Audit entries record `user=anonymous` when auth
  is disabled.
- **Audit trail: dual-sink, persistence never blocks the action (PR #383).**
  `recordAdminAction` keeps the slog line (for CloudWatch Insights /
  log-based alerting) and adds DynamoDB persistence for `GET
  /api/audit-logs`. A failed `PutItem` is logged and swallowed -- a gap in
  the audit trail is a lesser failure than blocking an admin action.
  Feature-gating follows the table-name convention: `HasAuditTable()` (the
  env-configured table name being non-empty) is the signal, not a nil
  interface -- `main.go` always wires the shared `*db.Client` whether or
  not `RUNS_FLEET_AUDIT_TABLE` is set. IDs are ULIDs and timestamps are
  generated inside `RecordAudit` so callers can never fabricate them, and
  the 90-day TTL keeps the table small enough that offset pagination is
  done in memory rather than with DynamoDB cursors.
- **Phase 1 stayed strictly read-only, computed from existing stores
  (PR #384).** The reconcile-status, detail-view, cost-breakdown, and
  metrics features all read what the control plane already writes
  (jobs/pools tables, EC2 tags, SQS attributes) rather than introducing new
  telemetry infrastructure -- the one schema addition is the pool record's
  `last_reconcile_at`/`last_reconcile_result`, written by the reconcile
  loop while it still holds the pool lock. Where an exact figure would need
  new history (spot interruptions), the API ships an explicitly flagged
  estimate instead of silently wrong data.
- **One pricing function for all cost endpoints -- now shared with the
  daily report (PR #390).** `cost.JobPricer` (extracted behavior-preserving
  from `CostHandler.priceJob`; `handler_cost_test.go` unchanged) is the
  single source of per-job pricing (live on-demand via `cost.PriceFetcher`,
  live spot via `fleet.Manager`, hard-coded table + fixed discount as
  fallback), shared by summary, daily, and by-pool -- and `CostMTD` feeds
  the metrics summary -- so every surface reports the same dollar figure.
  The extraction lets `pkg/cost.Reporter`'s daily cost report price the
  same job records with the same math. Daily buckets by `CreatedAt`
  deliberately match the `monthToDateJobs` filter dimension so day totals
  sum to the summary total.
- **Embedded Next.js UI for single-binary deployment.** `//go:embed
  all:ui/out` packages the static export into the Go binary; deployment is
  one artifact. If the export is missing, the handler returns a 503 with the
  build instruction inline. Detail pages use query params, not dynamic
  routes, because the export is static.
- **Per-handler interfaces over a god struct.** Each handler defines a
  narrow interface for its AWS / DB needs (`PoolDB`, `AuditDB`, `JobsDB`,
  `CostDB`, `CircuitDynamoAPI`, `onDemandPricer`, `spotPricer`, etc.).
  Ergonomic for tests and keeps coupling local.
- **Phase 1 read-only first, Phase 2 actions later.** `ADMIN_UI_PLAN.md`
  prioritised visibility before destructive ops (manual termination,
  circuit reset, forced reconcile, DLQ redrive). Phase 1 is now complete;
  the only write actions are the two housekeeping sweeps
  (orphaned-jobs cleanup, hung-job requeue), both with `dry_run` support.
- **Per-IP rate limiting across the whole API.** `RateLimiter.Wrap`
  encloses the admin mux in `main.go` (`RUNS_FLEET_ADMIN_RATE_LIMIT`,
  default 60 req/min, X-Forwarded-For-aware), so expensive new endpoints
  inherit protection automatically.
- **Soft delete restriction on pools.** `DeletePool` rejects non-ephemeral
  pools with a 403 + audit `denied/non-ephemeral`. Long-lived pools must be
  decommissioned via the GitOps flow, not the API.
- **Consistent failure surface.** Every handler returns
  `ErrorResponse{Error, Details}` JSON; per-handler `writeError`/`writeJSON`
  helpers centralise this.

## Gotchas [coverage: high -- 30 sources]

- **No RBAC beyond authentication.** Session claims carry `Groups` (from the
  ID token's groups claim, via `GroupsContextKey`), but nothing checks them
  -- any authenticated user can hit every endpoint, including destructive
  writes (pool delete, requeue, cleanup). This is a known, separate gap
  (see `ADMIN_UI_PLAN.md`'s RBAC note), not something this auth model
  change addresses.
- **The groups claim name varies by provider and is easy to get wrong on
  first setup.** `RUNS_FLEET_ADMIN_OIDC_GROUPS_CLAIM` defaults to `"groups"`
  (correct for Keycloak/Okta); Auth0 commonly namespaces it (e.g.
  `https://example.com/groups`) and Google has no groups claim at all --
  worth checking first if group-based logic ever silently sees zero groups.
- **Sessions don't survive a session-secret rotation.** Rotating
  `RUNS_FLEET_ADMIN_SESSION_SECRET` invalidates every existing session
  cookie immediately (the HMAC signature no longer verifies) -- expected
  and fine given there's no refresh flow to preserve, but worth knowing
  before rotating in a live deployment.
- **The metrics page's "Avg Startup" is not the `JobStartupSeconds`
  metric.** `avgStartupSeconds` in `handler_metrics.go` averages
  `StartedAt - CreatedAt` from the DynamoDB job record -- i.e. assignment →
  runner started. The backend `JobStartupSeconds` metric (added later, PR
  #387) measures GitHub job created → started. The two intentionally
  differ; don't "fix" one to match the other.
- **`spot_interruption_rate` over-counts by design.** It is the share of
  spot jobs with `retry_count > 0` or status `requeued`, which also counts
  bootstrap failures and stale-claim re-claims. It ships flagged
  (`spot_interruption_rate_estimated: true`) and the UI shows a caveat
  banner; an exact figure awaits Phase 3's stored interruption history.
- **Cost figures are estimates, bounded at 10k jobs.** Fallbacks stack up:
  missing instance type prices as `t4g.medium`, zero/negative durations
  bill as a 0.5h minimum, live price lookups fall back to the 3-family
  hard-coded table and fixed 70% spot discount, and `monthToDateJobs` caps
  at `Limit: 10000` -- a very busy month silently truncates. The
  runner-minute matrix separately skips zero-duration jobs, unknown
  instance types, and arches without a configured rate, so its total is not
  the EC2 total.
- **Audit list ordering is only guaranteed on the user-filtered path.**
  `ListAuditLogs` with a `user` filter queries the `user-index` GSI (sorted
  by timestamp range key); without it, results come from a `Scan`, which
  DynamoDB does not sort -- so the unfiltered viewer page can show entries
  out of order across pages. Offset pagination is in-memory and relies on
  the 90-day TTL keeping the table small.
- **`requeue-hung-jobs` audit entries are slog-only.** `RequeueHandler`
  still uses its own private `auditLog` helper rather than
  `recordAdminAction`, so hung-job requeues never appear in
  `GET /api/audit-logs` -- only pool CRUD and orphaned-jobs cleanup are
  persisted. (The plan says future write actions should call
  `recordAdminAction`.)
- **Unparseable audit/jobs filter params are silently ignored.** Bad
  `since`/`until` RFC3339 values, out-of-range `limit`, or negative
  `offset` fall back to defaults without a 400 -- deliberate, matching
  `ListJobs`'s `since` handling, but a typo'd filter looks like "no
  filter".
- **DynamoDB `Scan` on the hot path.** `ListCircuitStates`, the orphan-job
  finder, the audit scan fallback, and the underlying `JobsDB` admin
  queries (which every cost and metrics request fans into) all use
  full-table scans with pagination loops. Fine at current scale, but cost
  and latency grow linearly with table size.
- **`orphaned-jobs` / `requeue-hung-jobs` minimum threshold is 10 minutes
  -- silently.** If the caller passes `threshold_minutes` below 10 (or a
  non-integer), the handlers silently keep their defaults (120 and 15
  minutes respectively) rather than rejecting.
- **Instance "exists" defaults to true on AWS errors.** In
  `instanceExists`, anything other than `InvalidInstanceID.NotFound` (e.g.
  throttling, transient API outage) is treated as "instance exists". Safe
  default -- avoids false-positive cleanups -- but means a long EC2 outage
  will mask real orphans.
- **Pool busy-instance lookup can fail per-pool without failing the call.**
  `ListInstances` collects warnings into a `warnings: []string` field
  instead of returning 500; the `busy` flag for those instances will read
  `false`. Operators should check the warnings array. `GetInstance`
  likewise degrades `busy` to `false` on a failed lookup.
- **`ListPools` swallows per-pool errors.** If `GetPoolConfig` errors on one
  pool name, it is logged and the pool is dropped from the response (not
  surfaced as a partial-failure warning). The list response can be silently
  shorter than the actual pool count. `ListQueues` behaves the same way for
  per-queue attribute failures.
- **PUT preserves `Ephemeral` and `LastJobTime` from the prior record.**
  `UpdatePool` copies these two fields from `existing` after building the
  config, so they cannot be mutated through the API. `DeletePool` then keys
  off `Ephemeral` to gate deletion.
- **Pool name validation is permissive on update.** `validatePoolRequest`
  enforces the `^[a-zA-Z0-9][a-zA-Z0-9_-]*$` and 63-char limit only when
  `isCreate=true`; on PUT the name comes from the URL path and is overwritten
  onto the request before validation skips that branch.
- **UI 503 on a fresh checkout.** `ui.go` checks for `ui/out/index.html` at
  request time; without `make build-admin-ui` you get
  `503 Service Unavailable` rather than a 404. Similarly, the audit page
  renders a "persistence not configured" notice when the API returns 503
  for an unset `RUNS_FLEET_AUDIT_TABLE`.
- **SPA fallback masks 404s.** If a `/admin/...` path does not match any file
  or directory index, `UIHandler` falls back to root `index.html` with a 200
  so the Next.js client router can resolve. Genuine missing assets look like
  HTML responses to consumers.

## Sources [coverage: high]

- [pkg/admin/handler.go](../../pkg/admin/handler.go)
- [pkg/admin/handler_audit.go](../../pkg/admin/handler_audit.go)
- [pkg/admin/handler_jobs.go](../../pkg/admin/handler_jobs.go)
- [pkg/admin/handler_instances.go](../../pkg/admin/handler_instances.go)
- [pkg/admin/handler_queues.go](../../pkg/admin/handler_queues.go)
- [pkg/admin/handler_circuit.go](../../pkg/admin/handler_circuit.go)
- [pkg/admin/handler_cost.go](../../pkg/admin/handler_cost.go)
- [pkg/cost/jobpricing.go](../../pkg/cost/jobpricing.go)
- [pkg/admin/handler_metrics.go](../../pkg/admin/handler_metrics.go)
- [pkg/admin/handler_housekeeping.go](../../pkg/admin/handler_housekeeping.go)
- [pkg/admin/handler_requeue.go](../../pkg/admin/handler_requeue.go)
- [pkg/admin/auth.go](../../pkg/admin/auth.go)
- [pkg/admin/oidc.go](../../pkg/admin/oidc.go)
- [pkg/admin/session.go](../../pkg/admin/session.go)
- [pkg/admin/handler_auth.go](../../pkg/admin/handler_auth.go)
- [pkg/admin/ratelimit.go](../../pkg/admin/ratelimit.go)
- [pkg/admin/ui.go](../../pkg/admin/ui.go)
- [pkg/db/audit.go](../../pkg/db/audit.go)
- [cmd/server/main.go](../../cmd/server/main.go)
- [deploy/terraform/dynamodb.tf](../../deploy/terraform/dynamodb.tf)
- [docs/ADMIN_UI_PLAN.md](../../docs/ADMIN_UI_PLAN.md)
- [pkg/admin/ui/app/audit/page.tsx](../../pkg/admin/ui/app/audit/page.tsx)
- [pkg/admin/ui/app/cost/page.tsx](../../pkg/admin/ui/app/cost/page.tsx)
- [pkg/admin/ui/app/metrics/page.tsx](../../pkg/admin/ui/app/metrics/page.tsx)
- [pkg/admin/ui/app/instances/detail/page.tsx](../../pkg/admin/ui/app/instances/detail/page.tsx)
- [pkg/admin/ui/app/queues/detail/page.tsx](../../pkg/admin/ui/app/queues/detail/page.tsx)
- [pkg/admin/ui/components/audit-logs-table.tsx](../../pkg/admin/ui/components/audit-logs-table.tsx)
- [pkg/admin/ui/components/instances-table.tsx](../../pkg/admin/ui/components/instances-table.tsx)
- [pkg/admin/ui/components/pool-table.tsx](../../pkg/admin/ui/components/pool-table.tsx)
- [pkg/admin/ui/lib/format.ts](../../pkg/admin/ui/lib/format.ts)
- [pkg/admin/ui/lib/types.ts](../../pkg/admin/ui/lib/types.ts)
