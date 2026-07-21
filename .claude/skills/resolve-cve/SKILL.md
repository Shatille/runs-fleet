---
name: resolve-cve
description: Investigate and remediate a reported CVE in runs-fleet (runner image, AMI, or Go module). Use when a CVE is reported, a Trivy gate fails, a security scan flags something, or the user asks to "fix/clear CVEs". Covers finding the actual scan source, per-finding remediation choice, VEX evidence standards, and verification.
---

# Resolve a CVE report

Goal: identify what actually flagged, remediate what upstream allows, VEX only
with evidence, and verify the report is clean — not just that the gate passes.

## 1. Locate the finding — never assume the source

"A CVE was detected" can mean four different scanners. Check all that apply:

- **Runner image**: `make scan-runner` locally — same Trivy version, config, and
  VEX as CI. Do this even if CI is green: CI ran with yesterday's DB; new CVEs
  appear with today's. The report JSON lands at `bin/trivy-report.json`.
- **Go module (our own binaries)**: `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
  — catches stdlib/dependency vulns with call-path reachability. The Trivy image
  scan only sees the agent binary as shipped; govulncheck sees what the *pinned
  toolchain* (`go.mod`) builds everywhere else.
- **AMI**: the `Build AMIs` workflow logs contain the gate output
  (`gh run view <id> --log | grep -E "CVE-|GATE"`). Note: it runs
  `trivy filesystem` (no gobinary analyzer), so host Go binaries never flag
  there — only package DBs and go.mod-style metadata.
- **Downstream scanners** (ECR/Inspector, org pipelines): they ignore our
  `.trivy/vex.json`. Only real remediations help there — ask which scanner the
  user needs clean before reaching for VEX.

## 2. For stdlib CVEs: map toolchain exposure per artifact

A Go stdlib CVE has a different fix per build lane. Enumerate:

| Lane | Toolchain source | Fix |
|---|---|---|
| CI test/build (`setup-go`) | `go-version-file: go.mod` → exact `go` directive | bump `go.mod` `go` directive |
| Docker images | `golang:X.Y-alpine` floating tag + `pull: true` | rebuild picks it up |
| release.yml | `go-version: stable` | nothing |
| Local dev / nix | devshell pin; `GOTOOLCHAIN=auto` downloads what go.mod demands | bump `go.mod` |

Bumping the `go.mod` `go` directive to the fixed patch is the single change
that propagates to every `go-version-file` consumer and to local dev.

## 3. Classify each image/AMI finding (order of preference)

Follow `docker/runner/CLAUDE.md`. In order:

1. **apt-fixable** → bump/rebuild (blocking bucket in `.trivy/gate.sh`; class
   `os-pkgs`).
2. **Self-updating tool** (npm/pip/gem) → its own update path. Then verify the
   layer actually ran (see §5 — cache staleness).
3. **Upstream published a fixed build?** Don't trust release notes — download
   the release binary and scan it:
   `trivy rootfs` the binary dir with the repo config, compare findings against
   the apt/current version. Docker's apt repo lags buildx/compose releases by
   months; the upstream release often already cleared most findings. If it
   scans clean(er), pin it (`BUILDX_VERSION`/`COMPOSE_VERSION` ARGs in
   `docker/runner/Dockerfile`) together with its per-arch SHA256 digests —
   pin digests in-repo, never verify against a checksums file fetched from
   the same release page (same-origin trust; a compromised release matches
   its own checksums).
4. **VEX** — only with evidence (§4).
5. **Leave visible non-blocking** — the gate is target-scoped; third-party
   prebuilt binaries never block. Findings clear when upstream republishes.

## 4. VEX evidence standards

- **Symbol proof**: `go run golang.org/x/vuln/cmd/govulncheck@latest -mode=binary <binary>`
  (works cross-platform on ELF; extract binaries from the image via
  `podman create` + `podman cp`). Symbols absent → `vulnerable_code_not_present`
  and the statement is airtight. Symbols present → do NOT claim not-present.
- **Trivy VEX matches per package PURL, not per binary.** One
  `stdlib@vX.Y.Z` statement suppresses that finding in *every* binary vendoring
  that version. If some binaries genuinely contain the code, the statement must
  argue the deployment context (e.g. workflows already hold docker-socket root
  on single-tenant ephemeral hosts → no privilege boundary crossed;
  justification `inline_mitigations_already_exist`) — and say both halves in
  the impact statement.
- **Pin the PURL to the detected version** so an upstream rebuild invalidates
  the statement and forces re-review. Prune statements whose PURLs no longer
  match anything (the `_note` in `.trivy/vex.json` is the contract).
- Never `.trivyignore`.

## 5. Cache staleness — verify the shipped image, not the Dockerfile

Byte-identical RUN layers (`npm install -g npm@latest`, `apt-get upgrade`) are
served stale from registry build cache forever. `no-cache-filters` in
build-push-action only matches *declared* stage names — the final stage must be
`AS runtime` (synthetic names like `stage-1` silently match nothing).

Always confirm inside the built image:

```bash
podman run --rm --entrypoint bash <img> -c 'dpkg -l | grep -E "docker|containerd"'
podman run --rm --entrypoint bash <img> -c '<node> <npm-cli.js> --version'
```

and locate lang-pkg findings precisely via
`jq '.Results[].Vulnerabilities[].PkgPath' bin/trivy-report.json` — e.g. undici
inside npm's own vendored tree means "update npm", not "patch undici".

## 6. Verify end-to-end before commit

1. `go build ./...` && `make lint` && `make test`
2. `govulncheck ./...` → 0 affecting findings
3. `make scan-runner` → **both** gate sections empty (goal is a clean report,
   not merely GATE: PASS), and inspect the image per §5
4. Keep `packer/provision-base.sh` pins (e.g. `DOCKER_COMPOSE_VERSION`) in sync
   with the Dockerfile ARGs

## Session-learned facts worth rechecking, not assuming

- nixpkgs freezes old Go minor branches (`go_1_25` stuck at 1.25.8); flake
  updates won't fix them — `GOTOOLCHAIN=auto` + go.mod directive does.
- The AMI scan skips `/var/lib/docker` and `/opt/hostedtoolcache` by design;
  don't "fix" findings in pre-baked caches by other means.
- GitHub security-alert APIs may 403 on the available token — go straight to
  local scans instead.
