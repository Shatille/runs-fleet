# Admin UI Plan

Status and remaining work for the runs-fleet admin UI. The original plan tracked a build-out from basic pool CRUD to a full operational dashboard; most of Phase 1 and the auth migration have since shipped. This document is re-scoped around what is left.

## Status (as of 2026-07-03)

- **Auth**: ✅ Native OIDC — the orchestrator is its own OIDC relying party (authorization-code flow + PKCE against any standards-compliant issuer). No external gatekeeper or reverse proxy required; a self-hoster points `RUNS_FLEET_ADMIN_OIDC_*` at their IdP directly. Sessions are a self-contained HMAC-signed cookie (no shared session store, no refresh tokens — fixed TTL, re-login on expiry). Superseded the earlier Keycloak-gatekeeper-header-trust model, which in turn had superseded the original Bearer-token auth.
- **Phase 1 (read dashboards)**: ✅ Complete. Jobs, Pool status (incl. `last_reconcile_at`), Circuit breaker, Audit viewer, Instance/Queue detail, Cost daily + by-pool breakdowns, and the Metrics summary are all built.
- **Phase 2 (write actions)**: 🟡 Started. Manual instance termination has shipped; four
  actions remain (circuit reset, force reconcile, DLQ redrive, generalized housekeeping
  trigger).
- **Phase 3 (advanced)**: ❌ Not started.

The UI is a Next.js (static export) + React + TypeScript + Tailwind app, embedded via `//go:embed` in `pkg/admin/ui.go`. Backend handlers live in `pkg/admin/handler_*.go`, wired in `cmd/server/main.go`.

---

## Shipped

| Area | Endpoint(s) / change | Evidence |
|------|----------------------|----------|
| Native OIDC auth | Authorization-code + PKCE flow, HMAC-signed session cookie, `/api/auth/{login,callback,logout,config}` | `pkg/admin/oidc.go`, `pkg/admin/session.go`, `pkg/admin/handler_auth.go`, `pkg/admin/auth.go` |
| Pool CRUD | `GET/POST /api/pools`, `GET/PUT/DELETE /api/pools/{name}` | `handler.go:118-122` |
| Pool status enhancement | `current_running` / `current_stopped` / `busy_instances` in pool response | `handler.go:59-61,148,391` |
| Jobs dashboard | `GET /api/jobs`, `/api/jobs/stats`, `/api/jobs/{id}` | `handler_jobs.go:76-78` |
| Instances list | `GET /api/instances` (EC2 `tag:runs-fleet:managed`, busy cross-ref) | `handler_instances.go:59` |
| Queues list | `GET /api/queues` (visible/in-flight/delayed + main DLQ) | `handler_queues.go:62` |
| Circuit breaker status | `GET /api/circuit` | `handler_circuit.go:62` |
| Cost summary | `GET /api/cost/summary` (MTD, spot/on-demand split, per-family) | `handler_cost.go:64` |
| Audit logging | Structured slog line (`logAdminAction`) on pool CRUD and housekeeping's orphaned-jobs cleanup, with user identity + client IP | `handler.go:495-519` |
| Audit persistence + viewer | DynamoDB-backed (`RUNS_FLEET_AUDIT_TABLE`, 90-day TTL, ULID id), `GET /api/audit-logs` (user/action/since/until/limit/offset filters), `/admin/audit/` UI page | `pkg/db/audit.go`, `pkg/admin/handler_audit.go`, `ui/app/audit/page.tsx` |
| Pool reconcile status | `last_reconcile_at`/`last_reconcile_result` on the pools table + pool response, recorded by the reconcile loop, shown as a "Last Reconcile" column | `pkg/db/pool_config.go`, `pkg/pools/manager.go`, `handler.go`, `ui/components/pool-table.tsx` |
| Instance detail | `GET /api/instances/{instance_id}` (placement, AMI, subnet, arch, tags) + detail page | `handler_instances.go`, `ui/app/instances/detail/page.tsx` |
| Queue detail | `GET /api/queues/{queue_name}` (+ oldest-message age) + detail page | `handler_queues.go`, `ui/app/queues/detail/page.tsx` |
| Cost daily + by-pool | `GET /api/cost/daily`, `GET /api/cost/by-pool` (shared `priceJob`) + chart & table | `handler_cost.go`, `ui/app/cost/page.tsx` |
| Metrics summary | `GET /api/metrics/summary` (jobs 24h, warm-pool hit rate, avg startup, spot-interruption estimate, cost MTD) + `/admin/metrics/` page | `handler_metrics.go`, `ui/app/metrics/page.tsx` |
| Manual instance termination | `DELETE /api/instances/{instance_id}[?force=true]` + Terminate action on the instances list and detail pages | `handler_instances.go`, `ui/components/terminate-instance-button.tsx` |

### Built but not in the original plan

- **Per-IP rate limiter** wrapping the whole `/api/` mux — `pkg/admin/ratelimit.go`, wired at `cmd/server/main.go:454`. (`RUNS_FLEET_ADMIN_RATE_LIMIT`, default 60/min.)
- **Trace-UI link endpoint** `GET /api/config/trace-url` + `trace_id` on job responses — `handler_jobs.go:79`. (`RUNS_FLEET_TRACE_UI_URL`.)
- **Dark-mode toggle** in the UI.

### Corrections vs. the original plan

- Endpoint paths differ from the original draft: it's `/api/circuit` (not `/api/circuit-breaker`) and `/api/jobs/{id}` (not `{job_id}`).
- The UI is a **top-nav** layout (`ui/app/layout.tsx`), not a sidebar. The root page `ui/app/page.tsx` is the **Pools list**, not a separate metrics dashboard home. There is no `sidebar.tsx` / `metric-card.tsx` / `queue-card.tsx`; dashboard cards are ad hoc inside `components/job-stats.tsx`.

---

## Remaining Work

Phase 1 (read dashboards) is complete. The `spot_interruption_rate` in
`GET /api/metrics/summary` ships as a **best-effort estimate** (share of spot
jobs that were retried/requeued) and is flagged `spot_interruption_rate_estimated:
true`; it over-counts because retry_count also bumps on bootstrap failures and
stale-claim re-claims. An exact figure depends on the Phase 3 spot-interruption
history.

### Phase 2 — write actions (mostly unbuilt)

All should call `recordAdminAction` (pkg/admin/handler.go) once implemented, same as pool CRUD and housekeeping's orphaned-jobs cleanup, so they land in the persisted audit trail automatically.

| Action | Endpoint | Notes |
|--------|----------|-------|
| Circuit breaker reset | `POST /api/circuit/{instance_type}/reset` | Reset a tripped breaker. `circuit.Breaker.ResetCircuit` already exists; inject the `*Breaker` rather than reaching for `CircuitHandler`'s direct-DynamoDB client, which would leave the orchestrator's in-process breaker cache stale for up to a minute |
| Force pool reconciliation | `POST /api/pools/{name}/reconcile` | Enqueue to pool queue or invoke reconciler |
| DLQ redrive | `POST /api/queues/{queue_name}/redrive` | SQS `StartMessageMoveTask` |
| Housekeeping trigger | `POST /api/housekeeping/run` | Generalize the single-task endpoints (`orphaned-jobs`, `orphaned-instances`, each taking `dry_run`) toward a multi-task `{"tasks":[...]}` body that also covers stale SSM and old jobs |

#### Per-job recovery and the instance reaper (shipped)

`POST /api/jobs/{id}/requeue` re-dispatches one operator-chosen job through the same
`housekeeping` code path as the fleet-wide sweep — `launched` only, bounded by
`MaxRequeueRetries`, terminating an alive-but-dead-agent instance before the send. The
staleness threshold is skipped: it exists to stop a *sweep* acting on jobs that may still be
starting, and means nothing for a row someone picked. Refusals come back as **409** with the
reason (`wrong_status`, `exhausted`, `lost_race`) rather than a generic failure.

`POST /api/jobs/{id}/reconcile` is the targeted form of the orphaned-jobs sweep: it retires a
job whose instance is gone and refuses with **409** while the instance is alive, since a job
record is the only thing tying a live runner to its work.

`POST /api/housekeeping/orphaned-instances[?dry_run=true]` runs the scheduled reaper's five
detection phases on demand. It deliberately skips the housekeeping task lock — the scheduled
sweep converges on the same idempotent `TerminateInstances`, so an overlap costs a duplicate
call, while waiting on a 60s lock would make the button feel broken. It reports **503** when
housekeeping is disabled (no pools table), since there is no sweeper to call.

All three record through `recordAdminAction`, as does the bulk requeue sweep, which used to
log without reaching the persisted trail.

#### Instance termination semantics (shipped)

`DELETE /api/instances/{instance_id}` refuses with **409** when the jobs table reports an
active job (`running`/`launched`, via `db.GetJobByInstance`), returning the blocking
`job_id`/`run_id`/`repo` so the UI can name it in the confirmation. `?force=true` proceeds
and flips that job to `terminating` — the same `db.MarkInstanceTerminating` call the
spot-interruption path makes. A persistent spot request is cancelled before the terminate
so it cannot resurrect the instance (shared
`housekeeping.CancelSpotRequestForInstance`). The active-job lookup **fails closed**: on a
DynamoDB error nothing is terminated, since mistaking a lookup failure for "no job running"
is how a build gets killed silently. Without `RUNS_FLEET_JOBS_TABLE` the endpoint returns
**503** rather than terminating with its only safeguard disabled.

Ordering is deliberate: the job is marked **after** EC2 accepts the terminate, and a mark
failure is reported as success (with the reason in `message` and the audit `details`) rather
than as a failed termination. Marking first would strand the job at `terminating` with a
live instance if the terminate then failed — a state nothing reconciles, since
`FindOrphanedJobCandidates` scans `running`/`claiming`/`launched` only while
`occupiesInstance` counts `terminating` as busy for `maxConcurrencyRuntime`, so
reconciliation would hold the instance for two hours. In this order the residual state is
`running`/`launched` with a dead instance, which the orphaned-jobs sweep is built for.

Deliberately out of scope, so they don't read as oversights:

- **No requeue.** A forced kill does not re-dispatch the job; the GitHub job fails when its
  runner disappears and re-running is the operator's call. Requeue would need the job queue
  injected plus the `MaxRequeueRetries` accounting that `pkg/housekeeping/requeue.go` owns.
- **No `ReleaseInstanceClaim`.** `pools.Manager.terminateInstances` doesn't release claims
  either — the row expires on its 5-minute TTL and `ExecuteExpiredInstanceClaims` sweeps it.
- **No pool-count writeback.** `UpdatePoolState` self-heals on the next reconcile pass,
  which also launches the replacement for a pool instance.

### Phase 3 — advanced

- **SSE real-time updates** `GET /api/events` — replace the current polling (`hooks/use-auto-refresh.ts`) for job/instance/queue changes.
- **Spot interruption history** `GET /api/spot-interruptions` — store EventBridge interruptions in DynamoDB for capacity planning.
- **Cache metrics** `GET /api/cache/stats` — S3 cache hit/miss rates (the cache subsystem at `pkg/cache/` currently exposes no admin stats).

---

## Suggested Order

| Priority | Item | Effort |
|----------|------|--------|
| 1 | Force reconciliation | S |
| 2 | Circuit breaker reset | S |
| 3 | DLQ redrive | S |
| 4 | Generalized housekeeping trigger | S |
| 5 | SSE real-time updates | L |
| 6 | Spot interruption history | M |
| 7 | Cache metrics | S |

Phase 1 (Pool `last_reconcile_at`, Instance/Queue detail, Cost daily + by-pool,
Metrics summary) and manual instance termination are shipped — see the Shipped table above.

**Effort**: S = 1-2 days, M = 3-5 days, L = 1+ week.

---

## Cross-cutting notes

- **Trust boundary**: the orchestrator itself is the OIDC relying party (authorization-code + PKCE); it verifies the ID token and mints its own signed session cookie. Auth is required whenever `RUNS_FLEET_ADMIN_OIDC_ISSUER_URL` (and the rest of the required OIDC config) is set; leaving it all unset disables auth (local dev).
- **Rate limiting**: already enforced per-IP across `/api/`; expensive new endpoints inherit it.
- **RBAC (future)**: session claims already carry `Groups` (from the ID token's groups claim) into request context via `GroupsContextKey`, but nothing currently checks them — every authenticated user can hit every endpoint, including writes. Group-based gating on write endpoints is a real, separate gap, not yet implemented.
- **Testing**: unit tests with mocked AWS clients; integration tests against test DynamoDB/SQS; Playwright for critical UI flows.

## New backend files (as Phase 2/remaining lands)

```
pkg/admin/
├── handler_metrics.go     # GET /api/metrics/summary          (shipped)
├── handler_instances.go   # DELETE /api/instances/{id}        (shipped — the terminate
│                          #   action lives with the other instance routes rather than in
│                          #   a separate handler_actions.go: it reuses the same EC2
│                          #   client, ID regex, and managed-tag 404 semantics)
└── handler_actions.go     # circuit reset, force reconcile, DLQ redrive
```
