# Admin UI Plan

Status and remaining work for the runs-fleet admin UI. The original plan tracked a build-out from basic pool CRUD to a full operational dashboard; most of Phase 1 and the auth migration have since shipped. This document is re-scoped around what is left.

## Status (as of 2026-07-03)

- **Auth**: ✅ Native OIDC — the orchestrator is its own OIDC relying party (authorization-code flow + PKCE against any standards-compliant issuer). No external gatekeeper or reverse proxy required; a self-hoster points `RUNS_FLEET_ADMIN_OIDC_*` at their IdP directly. Sessions are a self-contained HMAC-signed cookie (no shared session store, no refresh tokens — fixed TTL, re-login on expiry). Superseded the earlier Keycloak-gatekeeper-header-trust model, which in turn had superseded the original Bearer-token auth.
- **Phase 1 (read dashboards)**: 🟡 Mostly built. Jobs, Pool status, Circuit breaker, and the Audit viewer are complete; Instances/Queues/Cost have their list/summary endpoints but not the planned detail/breakdown sub-endpoints; Metrics summary is not built.
- **Phase 2 (write actions)**: 🟡 Essentially unbuilt — one divergent housekeeping endpoint exists.
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

### Built but not in the original plan

- **Per-IP rate limiter** wrapping the whole `/api/` mux — `pkg/admin/ratelimit.go`, wired at `cmd/server/main.go:454`. (`RUNS_FLEET_ADMIN_RATE_LIMIT`, default 60/min.)
- **Trace-UI link endpoint** `GET /api/config/trace-url` + `trace_id` on job responses — `handler_jobs.go:79`. (`RUNS_FLEET_TRACE_UI_URL`.)
- **Dark-mode toggle** in the UI.

### Corrections vs. the original plan

- Endpoint paths differ from the original draft: it's `/api/circuit` (not `/api/circuit-breaker`) and `/api/jobs/{id}` (not `{job_id}`).
- The UI is a **top-nav** layout (`ui/app/layout.tsx`), not a sidebar. The root page `ui/app/page.tsx` is the **Pools list**, not a separate metrics dashboard home. There is no `sidebar.tsx` / `metric-card.tsx` / `queue-card.tsx`; dashboard cards are ad hoc inside `components/job-stats.tsx`.

---

## Remaining Work

### Phase 1 — finish read dashboards

#### Instance detail
`GET /api/instances/{instance_id}` — single-instance view. Only the list endpoint exists today.

#### Queue detail
`GET /api/queues/{queue_name}` — single-queue view. Only the list endpoint exists today.

#### Cost breakdowns
`GET /api/cost/daily` and `GET /api/cost/by-pool` — only `/api/cost/summary` exists. UI: daily cost chart + per-pool breakdown table.

#### Metrics summary
`GET /api/metrics/summary` — not built. `GET /api/jobs/stats` already covers job counts + warm-pool hit rate; the remaining gap is avg startup time, spot-interruption rate, and cost MTD in one aggregate.

```json
{
  "jobs_24h": { "total": 150, "completed": 140, "failed": 5, "in_progress": 5 },
  "warm_pool_hit_rate": 0.85,
  "avg_startup_time_seconds": 45,
  "spot_interruption_rate": 0.02,
  "cost_mtd_usd": 52.30
}
```

#### Pool reconcile timestamp
Add `last_reconcile_at` (+ `last_reconcile_result`) to the pools table and surface it in the pool status response/UI. Not yet present anywhere.

### Phase 2 — write actions (mostly unbuilt)

All should call `recordAdminAction` (pkg/admin/handler.go) once implemented, same as pool CRUD and housekeeping's orphaned-jobs cleanup, so they land in the persisted audit trail automatically.

| Action | Endpoint | Notes |
|--------|----------|-------|
| Manual instance termination | `DELETE /api/instances/{instance_id}` | Confirm runs-fleet-managed; warn if instance has an active job; UI confirmation |
| Circuit breaker reset | `POST /api/circuit/{instance_type}/reset` | Reset a tripped breaker |
| Force pool reconciliation | `POST /api/pools/{name}/reconcile` | Enqueue to pool queue or invoke reconciler |
| DLQ redrive | `POST /api/queues/{queue_name}/redrive` | SQS `StartMessageMoveTask` |
| Housekeeping trigger | `POST /api/housekeeping/run` | Generalize the existing single-task `POST /api/housekeeping/orphaned-jobs` (which takes `threshold_minutes` / `dry_run`) toward a multi-task `{"tasks":[...]}` body covering orphaned instances, stale SSM, old jobs |

### Phase 3 — advanced

- **SSE real-time updates** `GET /api/events` — replace the current polling (`hooks/use-auto-refresh.ts`) for job/instance/queue changes.
- **Spot interruption history** `GET /api/spot-interruptions` — store EventBridge interruptions in DynamoDB for capacity planning.
- **Cache metrics** `GET /api/cache/stats` — S3 cache hit/miss rates (the cache subsystem at `pkg/cache/` currently exposes no admin stats).

---

## Suggested Order

| Priority | Item | Effort |
|----------|------|--------|
| 1 | Pool `last_reconcile_at` | S |
| 2 | Instance detail + Queue detail endpoints | S |
| 3 | Cost `daily` + `by-pool` | M |
| 4 | Metrics summary | M |
| 5 | Manual instance termination | S |
| 6 | Force reconciliation | S |
| 7 | Circuit breaker reset | S |
| 8 | DLQ redrive | S |
| 9 | Generalized housekeeping trigger | S |
| 10 | SSE real-time updates | L |
| 11 | Spot interruption history | M |
| 12 | Cache metrics | S |

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
├── handler_metrics.go   # GET /api/metrics/summary
└── handler_actions.go   # instance terminate, circuit reset, force reconcile, DLQ redrive
```
