# Compile Log

## 2026-07-03 (admin UI auth model)

**Topics updated:** admin-ui (patched auth-related sections only, not a full recompile)

**New topics:** none

**Concepts updated:** none

**Notes:** Replaced Keycloak-gatekeeper header-trust auth with native OIDC (authorization-code + PKCE, HMAC-signed session cookie) — the project going open-source meant it could no longer assume every self-hoster runs a gatekeeper proxy. Updated `admin-ui.md`'s Architecture, Talks To, Key Decisions, and Gotchas sections; `docs/CONFIGURATION.md` and `docs/ADMIN_UI_PLAN.md` updated to match. `admin-ui` is one of the 12 topics still due for a full `--full` recompile (unrelated to this change).

## 2026-07-03

**Topics updated:** cmd-server, compute-providers, queue-processing, job-state-machine, warm-pools, internal-services, github-integration

**New topics:** none

**Concepts updated:** per-resource-locking (one sentence corrected from present to past tense re: the removed K8s backend)

**New concepts:** none

**Sources scanned:** 152 commits / 284 changed files since last compile (`git log --since=2026-04-30`)

**Sources changed:** 7 topics had structurally deleted or relocated source files; the other 12 tracked topics also touched files in this window but were scoped out of this pass (see Notes)

**Notes:** Root cause of the drift: the K8s runner backend was removed upstream (2026-06), deleting `pkg/provider/`, `pkg/state/valkey.go`, `pkg/pools/k8s_manager.go`, `pkg/queue/valkey.go`, and `internal/worker/k8s.go` outright. Separately, `pkg/runner/github.go` was relocated to `pkg/github/client.go` and renamed (PR #380, 2026-07-03), and a dead `GetJITConfig` method was deleted — the old `github-integration` and `job-state-machine` articles both incorrectly described a JIT-token registration flow that never actually ran in production. Flagged `compute-providers` as an obsolescence/merge candidate against `fleet-orchestration` (pkg/provider no longer exists; not merged, human decision needed — schema.md updated to note this). Found and reconciled a state-tracking bug: `.compile-state.json` listed a 4th concept, `asymmetric-backend-abstraction`, whose file never existed on disk; `schema.md`'s own Concepts section already correctly listed only 3. This was a scoped incremental compile — the remaining 12 topics still need a `--full` recompile to pick up feature work merged in this window (admin cost page, Python/Ruby AL2023 runners, cache-interception v2, `pkg/tracing`, tool-cache metrics).

## 2026-04-30

**Topics updated:** project-overview, cmd-server, cmd-agent, agent-runtime, fleet-orchestration, warm-pools, compute-providers, queue-processing, job-state-machine, state-storage, github-integration, cache-service, events-and-termination, observability, housekeeping, admin-ui, config-bootstrap, internal-services, infrastructure

**New topics:** all 19 (initial compilation)

**Concepts updated:** per-resource-locking, two-track-reliability, asymmetric-backend-abstraction, idempotent-retry-over-rollback

**New concepts:** all 4 (initial compilation)

**Sources scanned:** 84

**Sources changed:** 84 (first run)

**Notes:** Codebase-mode first compile. Topics consolidated from 25 packages into 19 surfaces (e.g., db+circuit+secrets merged into state-storage, metrics+logging+cost merged into observability). Compilation discovered several drift items worth tracking: runner version pinned in 3 places (Docker, Makefile, Packer); npm sentinel hash in flake.nix; cost reporter prices are us-east-1 but default region is ap-northeast-1; Vault secrets backend partially implemented with a planned audit-log table not yet built.
