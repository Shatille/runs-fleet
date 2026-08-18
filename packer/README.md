# Packer AMIs

Two-layer AMI build:

| Layer | Template | Provisioner | What it contains |
|---|---|---|---|
| **Base** | `runner-base-{amd64,arm64}.pkr.hcl` | `provision-base.sh` | OS + every package that's stable across runner-image revs: docker, git/git-lfs, node, vault, yq, pinned buildx cli-plugin (AL2023's bundled 0.12.1 predates the GHA cache v2 protocol), gh, runner OS deps, dev tools, language toolchains, the actions/runner binary, CloudWatch agent. |
| **Runner** | `runs-fleet-runner-{amd64,arm64}.pkr.hcl` | `provision-runs-fleet.sh` | The diff over base: the `runs-fleet-agent` binary extracted from the ECR runner image, the `runs-fleet-buildx-shim` installed as the `docker-buildx` CLI plugin, its systemd unit, the bootstrap shim, the per-boot cloud-init script, and the runs-fleet-specific CloudWatch metrics override. |

Both layers are built by a single workflow at `.github/workflows/build-amis.yml`. The runner-AMI job declares `needs: build-base`, so when a single push touches both `provision-base.sh` and `provision-runs-fleet.sh`, the runner AMI waits for the new base to register before starting Packer (its `source_ami_filter` then resolves to the fresh base). When a push only touches one layer, the other layer's job is skipped.

## Where does a new package go?

**Default: base AMI.** Anything that doesn't change with every agent-binary revision belongs there. Concrete signals:

- It's an OS package, a language toolchain, or a build tool → `provision-base.sh`.
- Its version is pinned and bumped on its own cadence (sbt, Node, Vault, gh) → `provision-base.sh`.
- It's a runtime dependency of `actions/runner` itself → `provision-base.sh`.
- It needs to be present before the runner agent starts a job → `provision-base.sh`.

**Runner AMI only when:** the thing is genuinely part of the `runs-fleet` orchestration layer and not a CI-workload concern. The current list is small and exhaustive:

- Extracting the `runs-fleet-agent` binary from the ECR runner image (this is the project's actual artifact).
- Extracting the `runs-fleet-buildx-shim` from the same image and installing it as the `docker-buildx` CLI plugin at `/usr/local/lib/docker/cli-plugins/docker-buildx` (this dir precedes the OS plugin dir in docker's search order, so it shadows the packaged buildx without replacing it). The real plugin path is recorded to `/opt/runs-fleet/buildx-real-path`. Like the agent, this is a runs-fleet orchestration artifact, not a CI-workload package — the shim is inert until the orchestrator injects the cache env vars.
- Installing the `runs-fleet-agent.service` systemd unit.
- Installing `agent-bootstrap.sh` and the per-boot cloud-init script.
- The CloudWatch agent JSON override carrying the runner metrics namespace.

If you're tempted to add anything else to `provision-runs-fleet.sh`, that's a signal it probably belongs in base instead.

## Adding to the base AMI

1. Edit `packer/provision-base.sh`. Add a new `echo "==> ..."` block, follow the existing pattern (explicit error handling for downloads + checksum verify for tarballs).
2. If the package adds a binary that downstream consumers should be able to inspect, append a line to the trailing summary (`echo "    - your-tool: $(your-tool --version)"`).
3. Open a PR. Merging to `main` triggers the unified `Build AMIs` workflow (`.github/workflows/build-amis.yml`); the `build-base` job runs, and `build-runner-ami` waits for it. The base build also runs on workflow_dispatch. There is no scheduled rebuild — nothing advances the prebaked images or `actions/runner` unless a build is triggered. OS packages are the exception: both provisioners open with `dnf upgrade --releasever=latest`, so every build moves the OS to the newest AL2023 release snapshot (a plain `dnf upgrade` cannot — AL2023 pins dnf to the snapshot its source AMI shipped, and the CVE fixes live in a newer one). The `Runner Staleness Check` workflow reports when the baked runner is nearing GitHub's 30-day expiry so you know a rebuild is due.

## Pre-baked Docker images

`provision-base.sh` pulls a small set of common CI images (databases, Redis, buildkit, binfmt, Playwright) into the AMI's Docker image store so ephemeral runners don't re-pull them from Docker Hub on every job. The list lives in the `PREBAKE_IMAGES` array in `provision-base.sh` — edit it there to add or drop an image. This belongs in the base layer (not the runner layer) because the images are a CI-workload concern that's stable across agent-binary revisions, and because the host dockerd's image store lives on the AMI root volume, so images pulled during base provisioning persist into the snapshot. Nothing bounds staleness on a timer — base rebuilds are triggered, not scheduled — but a moved tag at job time re-pulls only changed layers.

### Routing Docker Hub pulls through an ECR pull-through cache

Docker Hub rate-limits anonymous pulls per source IP, and a fleet of ephemeral runners behind shared egress hits that limit. Declaring `ECR_PULL_THROUGH_ENDPOINT` — the [ECR pull-through cache](https://docs.aws.amazon.com/AmazonECR/latest/userguide/pull-through-cache.html) endpoint for Docker Hub, e.g. `https://<account>.dkr.ecr.<region>.amazonaws.com/docker-hub` — is the entire opt-in. Set it as a repository **secret** (preferred: the value names an account-specific registry, and variables are readable on public repositories — same rationale as `ECR_REPOSITORY_RUNNER`); a repository variable also works. Unset (the default), nothing below exists at runtime and behaviour is byte-identical to an unconfigured build.

When set, runner instances route **every** Docker Hub pull through the cache:

- A local mirror proxy (`cmd/mirror-proxy`, baked from the runner image like the agent and buildx shim) serves `127.0.0.1:8989`, rewriting each request onto the endpoint's namespace and injecting registry credentials from the instance role. Its systemd unit is baked unconditionally but gated on `ConditionPathExists=/opt/runs-fleet/mirror-env`, which only the opt-in writes.
- `/etc/docker/daemon.json` points dockerd's `registry-mirrors` at it — covering job-time `docker pull` / `docker build`, service containers, and the `binfmt`/`buildkit` boot units. Mirrors apply to all Docker Hub images, official and namespaced alike, but only to Docker Hub — `mcr.microsoft.com`, `ghcr.io`, etc. are untouched.
- `/opt/runs-fleet/buildkitd.toml` covers `docker buildx build` with the `docker-container` driver, whose BuildKit resolves registries itself and ignores the daemon config. `buildx-setup.service` attaches it to the baked `multiarch` builder, and the buildx shim injects `--buildkitd-config` into any `docker buildx create` that doesn't bring its own config or a non-container driver — so `docker/setup-buildx-action`'s fresh builders are covered without workflow changes.

Why a local proxy at all: dockerd sends no credentials to a mirror and BuildKit cannot attach the cache's path prefix, while ECR refuses anonymous pulls and namespaces cached images under the rule prefix. Something local has to translate; the proxy is that translator, and no credential material lands on disk anywhere.

**Other pull-through rules ride along automatically.** The proxy discovers every rule in the registry (`ecr:DescribePullThroughCacheRules`) and routes by the `ns` query parameter the containerd resolver sends, and the bake writes a `buildkitd.toml` mirror block per discovered upstream. So a `FROM quay.io/...` inside a buildx build follows the account's `quay` rule with zero extra configuration. This only reaches BuildKit builds: dockerd's `registry-mirrors` is Hub-only by design, so a bare `docker pull quay.io/...` in a job always goes direct. Duplicate rules for one upstream resolve to the shortest prefix (then lexicographic), and discovery failure degrades to Hub-only with a warning.

**Everything fails open.** Proxy dead, cache rule deleted, IAM missing — dockerd and BuildKit fall back to Docker Hub directly, degrading to exactly the unconfigured behaviour (expect a fallback warning line per pull in the daemon journal).

The instance role needs `ecr:GetAuthorizationToken`, `ecr:DescribePullThroughCacheRules`, plus `ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer`, `ecr:BatchImportUpstreamImage`, and `ecr:CreateRepository` on the cache-prefix repositories so a first-ever pull can populate the cache. Note the value feeds the **runner-AMI** build: changing it requires a `Build AMIs` dispatch (a secret or variable change alone triggers nothing), and bake-time pre-bake pulls on the *base* layer still go to Hub directly — the proxy arrives in the runner layer.

Prefer floating tags so the next rebuild picks up updates on its own. The Playwright image is the exception: its tag is coupled to the `@playwright/test` version a consumer repo pins in its workflow (`container: mcr.microsoft.com/playwright:vX.Y.Z-noble`), and only an exact match is a cache hit. When a consumer upgrades Playwright, bump the pin here too — a stale pin degrades to a miss (the job re-pulls, as it did before the prebake), never a break.

## Go tool cache and exact-patch matching

`actions/setup-go` reading a `go.mod` with a full `go 1.x.y` line resolves it as an *exact patch* spec, so a tool-cache entry for a different patch of the same minor line is not a hit and the job downloads the toolchain (~18s) despite the pre-bake. `provision-base.sh` therefore bakes the newest `GO_PATCHES_PER_LINE` patches of each supported line, so a Go patch released mid-AMI-cycle still hits. Raise that constant if upstream patch cadence starts outrunning your rebuild cadence; each extra patch costs one more toolchain download at bake time and its unpacked size on the AMI.

## Verifying

`docker/runner/CLAUDE.md` describes the container-image security workflow. The AMI side runs a Trivy scan on the provisioned filesystem before the snapshot is taken (`packer/provision-trivy-scan.sh`), then applies the shared `.trivy/gate.sh`: the build fails only on HIGH/CRITICAL findings we can remediate (OS packages, our `runs-fleet-agent` binary). Findings that live only in third-party prebuilt binaries (Docker/containerd, npm-bundled libs) are reported but non-blocking — same policy as the container image (see `docker/runner/CLAUDE.md`). If your new package introduces a *remediable* finding, fix it (bump the package) or, for a genuinely-unreachable one, suppress it in `.trivy/vex.json` with a documented justification rather than `.trivyignore`.

## Downstream extension point

`packer/provision-base-hook.sh` is an empty stub in upstream. Downstream forks that need to layer in account-specific tooling without modifying upstream can rewrite it (e.g. from a CI secret) — the base build uploads it unconditionally and executes it just before cleanup if non-empty. **Prefer upstreaming new packages over relying on this hook.** The hook is for things that genuinely cannot live in upstream (account-specific credentials, internal-mirror configs).

## Rolling back a bad runner AMI

`pkg/fleet/fleet.go` pins `LaunchTemplateSpecification.Version` to `"$Latest"`, which means **highest version number** — not the version flagged as default. So `aws ec2 modify-launch-template --default-version N` does **not** affect new EC2 fleet launches. To roll back, create a new launch-template version that clones the last known-good one (the new version's number becomes `$Latest`):

```bash
aws ec2 create-launch-template-version \
  --launch-template-name runs-fleet-runner-<arch> \
  --source-version <good-version-number> \
  --launch-template-data '{}' \
  --version-description "rollback of <bad-version-number>"
```

Both `runs-fleet-runner-amd64` and `runs-fleet-runner-arm64` need the same treatment. The next `CreateFleet` call from the orchestrator will resolve `$Latest` to the new version and launch the rolled-back AMI. The next successful `Build AMIs` run then naturally advances `$Latest` again with a fresh AMI.
