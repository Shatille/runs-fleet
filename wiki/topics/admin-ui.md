---
topic: Admin API + UI
last_compiled: 2026-04-30
sources_count: 9
---

# Admin API + UI

## Purpose [coverage: high -- 9 sources]

`pkg/admin` exposes a REST API plus a self-contained web dashboard for fleet
operators. It provides the day-to-day surface for working with the runs-fleet
control plane: pool configuration (CRUD), inspecting jobs, listing managed EC2
instances, monitoring SQS queue depth, viewing circuit breaker state, and
triggering housekeeping actions. The dashboard is the operational counterpart
to the orchestrator running in `cmd/server/`.

The current code matches Phase 1 ("Visibility") of `docs/ADMIN_UI_PLAN.md`:
read-only dashboards over pools, jobs, instances, queues, and circuit
breakers, plus the first Phase 2 write action (orphaned-job cleanup).
Remaining write operations -- manual instance termination, circuit reset,
forced reconciliation, DLQ redrive -- are planned but not yet implemented in
the handlers reviewed.

## Architecture [coverage: high -- 9 sources]

The package is organised one handler file per resource type, all sharing a
single `AuthMiddleware` instance:

- `handler.go` -- pool CRUD, the `Handler` type, shared helpers
  (`writeJSON`, `writeError`, `auditLog`, `poolDiff`), validation, and
  request/response DTOs.
- `handler_jobs.go` -- `JobsHandler`, jobs list / detail / stats.
- `handler_instances.go` -- `InstancesHandler`, EC2-backed instance listing.
- `handler_queues.go` -- `QueuesHandler`, SQS queue depth/DLQ snapshot.
- `handler_circuit.go` -- `CircuitHandler`, scans the circuit-state table.
- `handler_housekeeping.go` -- `HousekeepingHandler`, orphaned-job cleanup.
- `auth.go` -- Keycloak gatekeeper header extraction middleware.
- `ui.go` -- serves the embedded Next.js static export.

Each handler has its own `RegisterRoutes(mux *http.ServeMux)` method, takes
the shared `*AuthMiddleware`, and wraps every handler func with
`auth.WrapFunc(...)`. AWS SDK access is mediated by small per-handler
interfaces (`PoolDB`, `JobsDB`, `EC2API`, `SQSAPI`, `CircuitDynamoAPI`,
`HousekeepingEC2API`, `HousekeepingDynamoAPI`) so each can be unit-tested
independently.

Logging is split: regular operational logs use
`logging.WithComponent(LogTypeAdmin, "<handler>")`, while audit events go
through the dedicated `auditLog = logging.WithComponent(LogTypeAdmin, "audit")`
sink in `handler.go`.

The UI is a Next.js app under `pkg/admin/ui/` whose static export lives at
`pkg/admin/ui/out/`. `ui.go` embeds it via `//go:embed all:ui/out` and serves
it under `/admin` with SPA-style fallback to `index.html`. If the bundle has
not been built, the handler returns 503 with the message "Admin UI not built.
Run: make build-admin-ui".

## Talks To [coverage: high -- 9 sources]

- **DynamoDB**
  - `runs-fleet-pools` via `PoolDB` (`ListPools`, `GetPoolConfig`,
    `SavePoolConfig`, `DeletePoolConfig`, `GetPoolBusyInstanceIDs`).
  - `runs-fleet-jobs` via `JobsDB` (admin-specific scan/query helpers
    `ListJobsForAdmin`, `GetJobForAdmin`, `GetJobStatsForAdmin`) and via
    `HousekeepingDynamoAPI` raw `Scan` + `UpdateItem`.
  - `runs-fleet-circuit-state` via `CircuitDynamoAPI.Scan`.
  - Planned: `runs-fleet-audit` table (per `ADMIN_UI_PLAN.md`); not yet
    implemented. Audit currently goes to slog.
- **EC2** -- `DescribeInstances` for the instances dashboard
  (`runs-fleet:managed=true` tag filter) and for housekeeping cross-checks of
  candidate orphan jobs.
- **SQS** -- `GetQueueAttributes` for visible/in-flight/delayed counts on the
  main, pool, events, termination, and housekeeping queues, plus the main
  queue DLQ.
- **Keycloak gatekeeper** -- the trust boundary. The middleware reads
  `X-Auth-Request-User`, `X-Auth-Request-Email`, and `X-Auth-Request-Groups`
  set by the proxy in front; it does not validate JWTs itself.

## API Surface [coverage: high -- 9 sources]

All routes are registered against an `http.ServeMux` using Go 1.22+ method +
path patterns. Every route is wrapped by `AuthMiddleware`.

Pools (`handler.go::Handler.RegisterRoutes`):

- `GET /api/pools` -- list pools with `current_running`, `current_stopped`,
  and live `busy_instances` count.
- `GET /api/pools/{name}` -- single pool, same enrichment.
- `POST /api/pools` -- create. Validates name, requires
  `Content-Type: application/json`, body cap 1 MB, 409 if exists.
- `PUT /api/pools/{name}` -- update. Preserves `Ephemeral` and `LastJobTime`
  from existing record; logs a per-field diff via `poolDiff`.
- `DELETE /api/pools/{name}` -- only allowed when `Ephemeral == true`;
  returns 403 otherwise.

Jobs (`handler_jobs.go::JobsHandler.RegisterRoutes`):

- `GET /api/jobs` -- filters: `status`, `pool`, `since` (RFC3339), `limit`
  (default 50, max 100), `offset`. Returns `{jobs, total, limit, offset}`.
- `GET /api/jobs/stats` -- 24-hour default window via `since` query.
  Computes `hit_rate = warm_pool_hit / completed`.
- `GET /api/jobs/{id}` -- 64-bit integer job ID.

Instances (`handler_instances.go::InstancesHandler.RegisterRoutes`):

- `GET /api/instances` -- filters: `pool`, `state` (default set
  `pending,running,stopping,stopped`). Always filters by tag
  `runs-fleet:managed=true`. Cross-references each pool's busy instance IDs
  to populate the `busy` flag; failures per pool are reported in a
  `warnings` array rather than failing the whole request.

Queues (`handler_queues.go::QueuesHandler.RegisterRoutes`):

- `GET /api/queues` -- iterates main, pool, events, termination,
  housekeeping. For the main queue it also fetches DLQ depth and reports it
  as `dlq_messages`. Per-queue failures are logged but skipped silently.

Circuit breaker (`handler_circuit.go::CircuitHandler.RegisterRoutes`):

- `GET /api/circuit` -- paginated `Scan` of the circuit-state table.
  Returns an empty list with a `message` if `tableName` is empty.

Housekeeping (`handler_housekeeping.go::HousekeepingHandler.RegisterRoutes`):

- `POST /api/housekeeping/orphaned-jobs` -- query params
  `threshold_minutes` (default 120, minimum 10) and `dry_run=true`. Scans
  the jobs table for `status in (running, claiming) AND created_at < cutoff`,
  batch-checks via `DescribeInstances` (batch size 100), and conditionally
  flips status to `orphaned` with `completed_at = now`.

UI (`ui.go::UIHandler`):

- `GET /admin/...` -- serves the embedded Next.js export with directory ->
  `index.html` resolution and SPA fallback to root `index.html`.

## Data [coverage: high -- 9 sources]

**Pool response shape** (`PoolResponse` in `handler.go`) -- the API returns
both desired and observed counts: `desired_running`, `desired_stopped`,
`current_running`, `current_stopped`, plus `busy_instances` computed from
`PoolDB.GetPoolBusyInstanceIDs(pool)` at request time. Resource specs
(`cpu_min/max`, `ram_min/max`, `families`, `arch`) and `schedules` are passed
through verbatim.

**Job response shape** (`JobResponse` in `handler_jobs.go`) -- exposes
`status`, `exit_code`, `duration_seconds`, `warm_pool_hit`, `retry_count`,
plus `created_at`/`started_at`/`completed_at` (omitted when zero). The
underlying `db.AdminJobEntry` and `db.AdminJobStats` are admin-specific
projections that live in `pkg/db`.

**Job statuses observed in handlers** -- the housekeeping handler treats
`running` and `claiming` as the "in-flight" set when looking for orphans, and
writes `orphaned` as the terminal value via a conditional update
(`#status = :running OR #status = :claiming`) so racing runners cannot lose
real completions.

**Audit logging** -- `Handler.auditLog` writes structured slog records with
fields `audit=true`, `user`, `action` (`pool.create`, `pool.update`,
`pool.delete`), `pool_name`, `result` (`success` / `denied` / `error`),
`remote_addr`, plus action-specific extras such as `reason`, `error`, or the
`changes` string from `poolDiff`. `success` logs at INFO, `denied` at WARN,
`error` at ERROR. `pool.update` includes a per-field diff string like
`desired_running: 2 -> 4; arch: "" -> "arm64"`.

**Planned `runs-fleet-audit` table** (per `docs/ADMIN_UI_PLAN.md`, not yet
implemented in code): composite key `(timestamp, id)` with ULID, GSI on
`(user, timestamp)`, attributes `action`, `target`, `result`, `details`
(map), `client_ip`, and a 90-day TTL.

**Circuit DynamoDB record** (`circuitRecord`) -- has fields
`instance_type`, `state`, `interruption_count`, `first_interruption_at`,
`last_interruption_at`, `opened_at`, `auto_reset_at`. The API renames
`interruption_count -> failure_count` and `auto_reset_at -> reset_at` for
display.

## Key Decisions [coverage: high -- 9 sources]

- **Keycloak gatekeeper as the trust boundary.** `auth.go` does not validate
  tokens; it trusts the proxy-set headers `X-Auth-Request-User`,
  `X-Auth-Request-Email`, `X-Auth-Request-Groups`. `ADMIN_UI_PLAN.md`
  documents this as an explicit simplification away from the previous
  Bearer-token model. The middleware exposes the user via context
  (`UserContextKey`, `GroupsContextKey`) and a `GetUsername(ctx)` helper.
- **Backwards-compat opt-out for local dev.** `NewAuthMiddleware("")` sets
  `requireAuth = false`, so empty `RUNS_FLEET_ADMIN_SECRET` lets you run the
  server locally without Keycloak; non-empty value flips it to required. The
  pool handler still records audit entries with `user=anonymous` in that
  case.
- **Audit logging is mandatory and built into every write path.** Every
  branch of `CreatePool`, `UpdatePool`, `DeletePool` writes an audit entry,
  including denied/error paths. This is non-configurable -- there is no flag
  to suppress it.
- **Embedded Next.js UI for single-binary deployment.** `//go:embed
  all:ui/out` packages the static export into the Go binary; deployment is
  one artifact. If the export is missing, the handler returns a 503 with the
  build instruction inline.
- **Per-handler interfaces over a god struct.** Each handler defines a
  narrow interface for its AWS / DB needs (`PoolDB`, `JobsDB`,
  `CircuitDynamoAPI`, `HousekeepingEC2API`, etc.). Ergonomic for tests and
  keeps coupling local.
- **Phase 1 read-only first, Phase 2 actions later.** `ADMIN_UI_PLAN.md`
  prioritises visibility (jobs, instances, pools, queues, circuit) before
  destructive ops (manual termination, circuit reset, forced reconcile, DLQ
  redrive). The current code matches that: only one Phase 2 action --
  orphaned-job cleanup -- is wired.
- **Soft delete restriction on pools.** `DeletePool` rejects non-ephemeral
  pools with a 403 + audit `denied/non-ephemeral`. Long-lived pools must be
  decommissioned via the GitOps flow, not the API.
- **Consistent failure surface.** Every handler returns
  `ErrorResponse{Error, Details}` JSON; per-handler `writeError`/`writeJSON`
  helpers centralise this.

## Gotchas [coverage: high -- 9 sources]

- **The middleware is only safe behind a trusted proxy.** `auth.go` reads
  user identity directly from request headers. If `/admin` or `/api` is ever
  exposed without the Keycloak gatekeeper in front, anyone can spoof
  `X-Auth-Request-User`. There is no JWT verification or signature check in
  this package.
- **`requireAuth` is derived from a single string flag.**
  `NewAuthMiddleware(adminSecret)` enables enforcement when `adminSecret !=
  ""`. The variable name (`adminSecret`) is a leftover from the legacy
  Bearer-token model; the value is no longer used as a secret, only as a
  boolean toggle.
- **DynamoDB `Scan` on the hot path.** `ListCircuitStates`, the orphan-job
  finder, and the underlying `JobsDB` admin queries all use full-table scans
  with pagination loops. Fine at current scale, but cost and latency grow
  linearly with table size; `ADMIN_UI_PLAN.md` flags possible GSIs as future
  work.
- **`orphaned-jobs` minimum threshold is 10 minutes -- silently.** If the
  caller passes `threshold_minutes` below 10 (or a non-integer), the handler
  silently keeps the 120-minute default rather than rejecting. The
  short-circuit lives in the parser: `mins, err := strconv.Atoi(...);
  err == nil && mins >= 10`.
- **Instance "exists" defaults to true on AWS errors.** In
  `instanceExists`, anything other than `InvalidInstanceID.NotFound` (e.g.
  throttling, transient API outage) is treated as "instance exists". Safe
  default -- avoids false-positive cleanups -- but means a long EC2 outage
  will mask real orphans.
- **Pool busy-instance lookup can fail per-pool without failing the call.**
  `ListInstances` collects warnings into a `warnings: []string` field
  instead of returning 500; the `busy` flag for those instances will read
  `false`. Operators should check the warnings array.
- **`ListPools` swallows per-pool errors.** If `GetPoolConfig` errors on one
  pool name, it is logged and the pool is dropped from the response (not
  surfaced as a partial-failure warning). The list response can be silently
  shorter than the actual pool count.
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
  `503 Service Unavailable` rather than a 404.
- **SPA fallback masks 404s.** If a `/admin/...` path does not match any file
  or directory index, `UIHandler` falls back to root `index.html` with a 200
  so the Next.js client router can resolve. Genuine missing assets look like
  HTML responses to consumers.
- **Audit table is planned, not built.** Today audit events go only to slog.
  A separate sink (`auditLog` logger) makes them filterable, but there is no
  query API yet -- the `GET /api/audit-logs` endpoint in the plan is not
  implemented in any of the source files reviewed.

## Sources [coverage: high]

- [pkg/admin/handler.go](../../pkg/admin/handler.go)
- [pkg/admin/handler_circuit.go](../../pkg/admin/handler_circuit.go)
- [pkg/admin/handler_housekeeping.go](../../pkg/admin/handler_housekeeping.go)
- [pkg/admin/handler_instances.go](../../pkg/admin/handler_instances.go)
- [pkg/admin/handler_jobs.go](../../pkg/admin/handler_jobs.go)
- [pkg/admin/handler_queues.go](../../pkg/admin/handler_queues.go)
- [pkg/admin/auth.go](../../pkg/admin/auth.go)
- [pkg/admin/ui.go](../../pkg/admin/ui.go)
- [docs/ADMIN_UI_PLAN.md](../../docs/ADMIN_UI_PLAN.md)
