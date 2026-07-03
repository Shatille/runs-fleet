# Roadmap

Phases 1-5 (Cold-Start MVP, Warm Pools, S3 Cache, Production Hardening,
Concurrent Processing) are complete — see `.claude/CLAUDE.md`'s Roadmap
Status section. This document tracks candidate work beyond that, from a
2026-07-03 sweep of the orchestrator and admin UI.

Prioritized by leverage: items that surface data already collected (no new
instrumentation) before items that need new design work, before nice-to-haves.

## Tier 1 — surface what already exists

- **Threshold-based alerting on top of existing metrics.** 50+ metrics
  already publish to CloudWatch/Prometheus/Datadog, but nothing pages a
  human when one crosses a threshold — no CloudWatch Alarms, no Datadog
  monitors, no PagerDuty integration anywhere. Cost reports go to SNS as
  informational markdown, not alerts.
- **Cost-anomaly detection.** A real incident (unfiltered instance-claim
  rows minting unbounded metric series, ~$900/mo spike) went undetected
  until the bill arrived — no proactive threshold check exists in the cost
  pipeline.
- **Whole-pool health detection.** Housekeeping catches individual orphaned
  instances and stale jobs, but nothing detects "this pool is systemically
  broken" (e.g. every instance failing to launch off a bad AMI) — it just
  keeps retrying individual jobs instead of blocking new assignments.
- **Runner tarball checksum validation.** `VerifyChecksum` is already
  exported in `pkg/agent` but never called — the GitHub releases tarball
  download has zero supply-chain verification and no retry on transient
  failure.
- **Admin UI: surface already-collected telemetry.** `RunnerToolCacheMiss`
  metric (informs AMI bake decisions), circuit-breaker `FirstInterruptionAt`/
  `OpenedAt`/reset countdown, per-pool cost attribution, housekeeping task
  run history, and per-pool lock-wait/reconcile-latency — all tracked, none
  shown in `pkg/admin`'s API or UI. See `docs/ADMIN_UI_PLAN.md` for the
  admin-UI-specific backlog this overlaps with.

## Tier 2 — real gaps, more design work

- **Circuit breaker refinement.** Blocks an entire instance type globally on
  3 interruptions/15min — no zone-level granularity (a single-AZ capacity
  blip blocks the type everywhere) and no half-open probationary state
  (the state is defined, never used).
- **Cross-region fallback.** Region is a single hard default
  (`ap-northeast-1`); no automatic cross-region retry when a type is
  exhausted regionally.
- **GitHub workflow-cancellation handling.** A cancelled workflow run still
  burns instance time until housekeeping's 10-minute stale-job sweep catches
  it — no direct reaction to the webhook's `cancelled` action.
- **Scheduled AMI rebuilds for security patches.** Packer builds are fully
  manual today — no scheduled job rebuilds base/runner AMIs for upstream
  CVEs, and no periodic check for AMI drift on already-running instances.
- **Admin backend hardening.** No RBAC beyond Keycloak-header authentication
  (any authenticated user can hit write endpoints); N+1 query in
  `ListInstances`; unbounded/hard-capped queries in circuit-state scan and
  cost-summary job listing; inconsistent pagination across list endpoints;
  one global per-IP rate-limit bucket for all of `/api/`.
- **Admin frontend consistency.** Auto-refresh behavior differs per page
  (some toggleable, some forced-on, cost page has none); no real-time
  updates anywhere (poll-only, up to 15s stale); duplicated status-badge and
  time-formatting logic across components; sortable table headers aren't
  keyboard-operable.

## Tier 3 — worth naming, lower urgency

- **JIT runner registration.** Still uses the older reusable
  repo-level registration-token flow (`pkg/github.Client.GetRegistrationToken`),
  not GitHub's actual JIT API (`GenerateOrgJITConfig`) — a real `GetJITConfig`
  method existed once, was never called in production, deleted as dead code
  in PR #380. Genuine hardening (shorter-lived, single-use credentials) but
  not urgent: these instances are already short-lived, so the exposure
  window is small.
- **DynamoDB Scan → Query/GSI.** `GetJobByInstance`, `QueryPoolJobHistory`,
  `GetPoolBusyInstanceIDs` all Scan instead of Query. Fine at current volume
  (~1000s of items), becomes a real cost/latency problem past ~10k jobs/day.

## Also flagged, not yet triaged

- `compute-providers` wiki topic overlaps `fleet-orchestration` now that
  `pkg/provider` (the K8s abstraction) is gone — candidate to merge, human
  decision needed (see `wiki/schema.md`).
- 12 wiki topics still reflect pre-2026-06 state and need a `--full` recompile
  (`/wiki-compile --full`) to pick up newer feature work.
- No org-level GitHub runner-groups API usage (repo-level registration only)
  — would matter if org-wide runner capacity management becomes a need.
- No cache eviction/quota enforcement in `pkg/cache` beyond an external S3
  lifecycle policy — a misconfigured or missing Terraform rule would let
  caches grow unbounded with no in-service guard.
