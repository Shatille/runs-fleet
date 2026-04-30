# Compile Log

## 2026-04-30

**Topics updated:** project-overview, cmd-server, cmd-agent, agent-runtime, fleet-orchestration, warm-pools, compute-providers, queue-processing, job-state-machine, state-storage, github-integration, cache-service, events-and-termination, observability, housekeeping, admin-ui, config-bootstrap, internal-services, infrastructure

**New topics:** all 19 (initial compilation)

**Concepts updated:** per-resource-locking, two-track-reliability, asymmetric-backend-abstraction, idempotent-retry-over-rollback

**New concepts:** all 4 (initial compilation)

**Sources scanned:** 84

**Sources changed:** 84 (first run)

**Notes:** Codebase-mode first compile. Topics consolidated from 25 packages into 19 surfaces (e.g., db+circuit+secrets merged into state-storage, metrics+logging+cost merged into observability). Compilation discovered several drift items worth tracking: runner version pinned in 3 places (Docker, Makefile, Packer); npm sentinel hash in flake.nix; cost reporter prices are us-east-1 but default region is ap-northeast-1; Vault secrets backend partially implemented with a planned audit-log table not yet built.
