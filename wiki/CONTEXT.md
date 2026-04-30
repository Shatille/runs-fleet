# Codebase Wiki — Navigation Guide

This project has a compiled knowledge wiki. Use it instead of scanning raw files.

## How to use this wiki

1. **Start at [INDEX.md](./INDEX.md)** — scan the topic table to find relevant modules. The "Quick navigation by task" section maps common tasks (orchestrator work, K8s mode, cost questions) to topics.
2. **Read 1-3 topic articles** relevant to your task. Each article uses the same 8 sections (Purpose, Architecture, Talks To, API Surface, Data, Key Decisions, Gotchas, Sources).
3. **Check coverage tags** on each section heading:
   - `[coverage: high -- N sources]` — trust this section, skip raw files
   - `[coverage: medium -- N sources]` — good overview; check raw sources for implementation detail
   - `[coverage: low -- N sources]` — read the raw source files listed in Sources
4. **Check [concepts/](./concepts/)** for cross-cutting patterns (per-resource locking, two-track reliability, backend asymmetry, idempotent retry). Concepts are interpretive — they answer "why is the code shaped this way?"
5. **Read raw source files** when you need code-level detail (exact syntax, field types, edge-case branches).

## When NOT to use the wiki

- Writing new code (read the actual source files for exact syntax/types).
- Debugging a specific function (go to the file directly).
- Verifying a current behavior before acting on it (the wiki is a snapshot — changes may have shipped after compile).
- The wiki article says `[coverage: low]` for what you need.

## Notable findings from this compilation

The first compile surfaced several details worth knowing:

- **Drift to fix:** runner version is pinned in three places (`docker/runner/Dockerfile` 2.331.0, `Makefile` 2.330.0, `packer/provision-runs-fleet.sh` 2.330.0). Bumping requires touching all three.
- **Sentinel value:** `flake.nix` ships `npmDepsHash = "sha256-AAA..."` — admin-ui Nix build will fail until replaced.
- **Cost reporter skew:** hardcoded prices are us-east-1 2024, but default region is ap-northeast-1. Cost reports systematically under-report by region.
- **Backend abstraction is partial:** only K8s lives behind the `Provider` interface; EC2 wires `pkg/fleet` directly into the orchestrator.
- **Phase 2 admin actions are not built yet:** docs/ADMIN_UI_PLAN.md lists manual instance termination, circuit reset, forced reconcile, DLQ redrive — only the orphaned-jobs trigger is wired.
- **Auth model has cut over:** `pkg/admin/auth.go` is fully on Keycloak headers; the `adminSecret` parameter is now a vestigial boolean toggle.
- **Audit log DynamoDB table is not yet provisioned:** audit events go to a slog-only logger; the planned `runs-fleet-audit` table doesn't exist.

## Stats

Compiled: 2026-04-30 | Topics: 19 | Concepts: 4 | Sources: 84 | Mode: codebase
