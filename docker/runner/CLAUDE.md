# Runner image — agent guide

This image runs untrusted workflow code via the GitHub Actions runner. CI has a Trivy gate that blocks merge on HIGH/CRITICAL findings **we can remediate** (OS/apt packages, our own agent binary); CVEs vendored only in third-party prebuilt binaries we install but don't build are reported but non-blocking — see "The gate is scoped to what we can remediate" below. Follow these rules before changing anything in this directory.

## Base image policy

Always extend `ghcr.io/actions/actions-runner:<tag>` (overridable via `RUNNER_BASE_TAG` build arg, default lives in the Dockerfile). Do not switch to a generic OS base + manual install. Do not download the actions-runner from a release tarball. Do not build the docker-cli from source.

Rationale: GitHub publishes the official base image and is responsible for refreshing the actions-runner artifacts (`runner.Listener.dll`, the Node.js externals, hashFiles JS). Rebuilding any of that ourselves freezes vendored dependencies and creates the very CVE drift we're trying to avoid.

## Adding a package

In strict order of preference:

1. **Docker's apt repo or Ubuntu's apt** (default). Add to the existing `apt-get install` step. Apt-managed packages get security updates via `apt-get upgrade` on every image rebuild.
2. **The package's own apt repo** (e.g., NodeSource, deadsnakes, hashicorp). Add its GPG key and `sources.list.d` entry alongside Docker's. Still apt-managed.
3. **An official upstream binary release** (e.g., a GitHub release tarball) only if no apt path exists *or the apt packaging demonstrably lags upstream security fixes* (see "Docker tooling split" below). Pin the version. Run `make scan-runner`; document or fix any new findings.
4. **NEVER**: build from source in the Dockerfile, vendor SHA512-pinned tarballs from npm/PyPI/crates.io, add per-CVE patch RUN steps that overwrite files in place. These created the patch treadmill the previous Dockerfile suffered from — every new CVE became another bump-and-hash commit.

When in doubt, ask the user before reaching for option 3.

## Docker tooling split

- **`docker-ce`, `docker-ce-cli`, `containerd.io`**: Docker's apt repo (option 2). Docker packages the engine promptly; `apt-get upgrade` on rebuild keeps them current.
- **`docker-buildx` / `docker-compose` cli-plugins**: pinned upstream GitHub releases (option 3, user-sanctioned 2026-07). Docker's apt plugin packages lag the plugin projects by months and ship builds on long-fixed `golang.org/x/*` and Go stdlib versions (25+ HIGH findings at apt versions vs 1 at upstream releases). Expected SHA256 digests are pinned in the Dockerfile per-arch — the trust anchor is this repo, not the release page (a compromised release could serve a matching same-origin checksums.txt; upstream signs nothing).

**Plugin bump procedure**: update `BUILDX_VERSION` / `COMPOSE_VERSION` **and all four `*_SHA256_*` ARGs** in the Dockerfile (take the digests from each release's `checksums.txt` at bump time), run `make scan-runner`, and review `.trivy/vex.json` — the PURL pins there reference the versions these plugins vendor, so a bump may let statements be removed (or require re-pinning). The AMI host has its own buildx and compose pins + digests (`BUILDX_VERSION` / `BUILDX_SHA256` and `DOCKER_COMPOSE_VERSION` / `DOCKER_COMPOSE_SHA256` in `packer/provision-base.sh`) — keep them in sync.

## Refreshing bundled `node_modules`

The runner ships a Node.js distribution under `/home/runner/externals/node24/`. Its npm has transitive dependencies (`tar`, `minimatch`, `glob`, `picomatch`, etc.) that lag npm's actual latest.

Do not patch those packages individually. Use the package manager's own update path:

```dockerfile
RUN /home/runner/externals/node24/bin/node \
    /home/runner/externals/node24/lib/node_modules/npm/bin/npm-cli.js \
    install -g npm@latest
```

npm verifies integrity itself; we don't pin SHAs.

## Node 20 is gone

The `node20/` directory is deleted in the Dockerfile, and `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24=true` is set. GitHub's June 2 deprecation forces every `runs.using: node20` action to node24; we just anticipate it. Do not re-add `node20/` unless the user explicitly asks.

## Handling Trivy findings

`make scan-runner` shows the current state of HIGH/CRITICAL findings using the same Trivy version and config as CI. For each finding:

1. **Can the package be bumped via apt?** Bump it.
2. **Is the package self-updating via its own tool** (npm, pip, gem)? Use that path.
3. **Is the upstream library fixed but the binary that vendors it hasn't been rebuilt?** That's the OpenVEX case. Add a statement to `../../.trivy/vex.json` with:
   - `status: "not_affected"`
   - A `justification` that matches the actual situation (`vulnerable_code_not_present`, `inline_mitigations_already_exist`, etc.)
   - A specific `impact_statement` explaining WHY the vulnerable code path is unreachable in the affected binary
   - A PURL pinned to the exact vulnerable version, so a future bump invalidates the statement and forces re-review
4. **Genuinely affecting us with no upstream fix?** Don't suppress. File an upstream issue and bring it back to the user.

**Never add `.trivyignore` entries.** We removed the previous one because it was hiding future CVEs in the same packages without any audit trail. VEX is per-CVE and per-PURL; new CVEs in the same package surface immediately.

## The gate is scoped to what we can remediate

The scan does not fail on every HIGH/CRITICAL. `.trivy/gate.sh` post-processes the Trivy JSON and **fails only on findings we can act on**:

- **OS / apt packages** (`Class == os-pkgs`) — fixed by `apt-get upgrade` on rebuild.
- **our own `runs-fleet-agent` binary** (`Target ~ runs-fleet-agent`) — fixed by a `go.mod` bump.

Findings that live **only** in third-party prebuilt binaries we install but do not build — Docker/containerd's `dockerd` and the `buildx`/`compose` cli-plugins — or in upstream-bundled `node_modules` (npm's vendored libs, e.g. `undici`) are **reported but non-blocking**. We cannot rebuild those binaries (see the base-image policy above), so failing on them would perpetually block image promotion and the AMI rebuild until the upstream project republishes on patched libraries. They clear on their own when upstream ships a fixed build, and `--ignore-unfixed` already drops anything with no fix.

This is **target-scoped, not CVE-id-scoped**: a new CVE in our agent or an OS package still fails the gate. It is not a `.trivyignore`. The same `gate.sh` runs in `make scan-runner`, the `build-runner.yml` image scan, and the Packer AMI scan (`packer/provision-trivy-scan.sh`), so all three agree.

For a genuinely-unreachable CVE in one of those third-party binaries, still prefer a precise OpenVEX statement (it documents *why* it can't affect us and keeps the report clean); the gate is the backstop for reachable-but-unfixable-by-us findings, not a substitute for that analysis.

## Verification workflow

Before pushing any change to this directory:

```bash
make scan-runner   # must exit 0 (gate PASS); third-party non-blocking findings may be listed
```

CI uses the same Trivy version (`aquasec/trivy:0.70.0`), the same `.trivy/trivy.yaml` config, and the same `--vex .trivy/vex.json` flag. If local passes, CI passes. Iterating via CI burns time and runner-cost.

For supply-chain transparency, `make sbom-runner` generates a CycloneDX SBOM at `bin/runs-fleet-runner.sbom.json`. CI emits the same SBOM as a BuildKit attestation attached to the image manifest (retrievable via `docker buildx imagetools inspect <image> --format '{{json .SBOM}}'`).

## Auto-rebuild and AMI cascade

Pushes to `main` touching `docker/runner/**`, `cmd/agent/**`, `pkg/agent/**`, `.trivy/**`, or `build-runner.yml` itself trigger the runner image build automatically. On successful completion, `build-ami.yml` runs via `workflow_run` and rebuilds the AMI off the freshly-promoted `:latest`. Edits here ship to both ECS Fargate (container) and EC2 (AMI) without manual intervention.

## What lives where

- `Dockerfile` — image build. Stage 1 builds the agent binary; stage 2 (`runtime` — the name is load-bearing: `build-runner.yml`'s `no-cache-filters: runtime` forces its layers fresh each CI build so `npm@latest` / `apt-get upgrade` can't be served stale from registry cache) extends the official base, installs apt packages, installs pinned buildx/compose plugins, refreshes npm, copies the agent.
- `entrypoint.sh` — execs the agent binary. Keep it trivial.
- `../../.trivy/trivy.yaml` — shared Trivy config for local + CI (severity, pkg types).
- `../../.trivy/vex.json` — OpenVEX statements with per-CVE rationale.

## Anti-patterns from prior sessions

These are real mistakes that happened and got reverted; do not repeat them:

- Using `ubuntu:24.04` as base and manually installing the actions-runner from a release tarball. Result: 71 stdlib CVEs from a custom-built docker-cli, none of which were fixable without further patching.
- Patching individual `node_modules` packages with SHA512-pinned tarballs in RUN steps. Result: every new CVE in those packages required a new Dockerfile commit. Replaced by the npm self-update path.
- Adding a `.trivyignore` for the npm-bundled package CVEs. Result: future CVEs in those packages silently disappeared from the scan. Replaced by structurally fixing the source (deleting `node20`, updating `npm`).
- Using `aquasecurity/trivy-action@<branch>` with no version pin. Result: a Trivy upgrade could break the build at any time. Pinned and replaced with direct `docker run`.

## Operational notes

- `RUNNER_BASE_TAG` is overridable in CI via the repo variable `vars.RUNNER_BASE_TAG` and locally via `make RUNNER_BASE_TAG=x.y.z scan-runner`.
- Removing upstream-bundled binaries (`rm -rf /usr/local/lib/docker /usr/libexec/docker`) before apt-installing replacements is necessary; otherwise both versions coexist and the older binaries still show up in scans.
- The sudo tightening sed targets the base image's main `/etc/sudoers` line. Verify with `sudo -l` inside the built image after any sudo-related change.
- `aquasecurity/trivy-action` does not expose a `--vex` input. The workflow runs Trivy via `docker run` directly so the CLI flag is available.
