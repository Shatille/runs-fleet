---
topic: Infrastructure (Docker, Packer, Helm, Nix)
last_compiled: 2026-08-21
sources_count: 30
---

# Infrastructure (Docker, Packer, Helm, Nix)

## Purpose [coverage: high -- 30 sources]

Build, packaging, and CI/CD artifacts for runs-fleet. Three artifacts ship:

- **Server image** — the orchestrator ([Dockerfile](../../Dockerfile)), run on
  Fargate or a Kubernetes control plane.
- **Runner container image** — [docker/runner/Dockerfile](../../docker/runner/Dockerfile);
  a runnable containerized runner *and* the carrier for three host binaries the
  AMI extracts (`runs-fleet-agent`, `runs-fleet-buildx-shim`,
  `runs-fleet-mirror-proxy`).
- **Runner AMIs** — a **two-layer, dual-arch** Packer pipeline (base → runner,
  each for `arm64` and `amd64`) producing what EC2 job instances actually boot.

Around them: a Helm chart that packages the orchestrator only (runners are
EC2-only since the 2026-06 K8s-backend removal), a Nix flake for a reproducible
dev shell plus dev builds of every Go binary, a Makefile, five GitHub Actions
workflows, and a shared Trivy CVE gate that both the image and the AMI paths run.
`deploy/terraform/` is **illustrative sample IaC, not the deployed policy** —
every file says so in its header comment
([deploy/terraform/iam.tf:11-14](../../deploy/terraform/iam.tf)); the production
IaC lives in a separate repository (see `AGENTS.md` "Stack").

The only Go binaries are `runs-fleet-server` (cmd/server), `runs-fleet-agent`
(cmd/agent), `runs-fleet-buildx-shim` (cmd/buildx-shim) and
`runs-fleet-mirror-proxy` (cmd/mirror-proxy). All artifacts derive from one repo
and one Go module.

## Architecture [coverage: high -- 12 sources]

```
                        runs-fleet repo (one Go module)
                                   |
        +--------------------------+---------------------------+
        |                          |                           |
   Server image              Runner image                 Runner AMIs
   (Fargate / K8s)      (3 host binaries + runner)     (EC2, amd64 + arm64)
        |                          |                           |
   Dockerfile           docker/runner/Dockerfile   packer/runner-base-{arch}.pkr.hcl
   alpine:3.19          ghcr.io/actions/                (provision-base.sh)
        |               actions-runner base                    |
   ECR: runs-fleet      ECR: runs-fleet-runner    packer/runs-fleet-runner-{arch}.pkr.hcl
        |                          |               (provision-runs-fleet.sh —
        +-- Helm chart             +-- agent/shim/proxy  extracts from the ECR image,
            (orchestrator only)        extracted into AMI  then provision-validate-agent.sh,
                                                           then provision-trivy-scan.sh)
                                                                  |
                                                          Launch templates
                                                       (new version per build)
```

### Two-layer AMI build, per architecture

Four templates: `runner-base-{amd64,arm64}.pkr.hcl` and
`runs-fleet-runner-{amd64,arm64}.pkr.hcl`. The layer split and the
"where does a new package go?" rule are documented in
[packer/README.md](../../packer/README.md).

**Base (`provision-base.sh`, ~16 min)** — the shared OS layer and the default
home for any new package. In order of appearance:

- `dnf upgrade --releasever=latest`, then `dnf remove --oldinstallonly` — see
  Key Decisions; this is what moves the OS past published CVEs.
- `*-minimal` → full package variants; base packages; git-lfs system hooks;
  GitHub CLI repo + `gh`.
- On ARM64 only: a **from-source gold linker** (binutils) for Go race-detector
  compatibility ([packer/provision-base.sh:78-98](../../packer/provision-base.sh)).
- SSM agent + Session Manager plugin; Docker enabled; Docker Compose (standalone
  + cli-plugin); a SHA-256-pinned **buildx 0.35.0** cli-plugin installed to
  `/usr/libexec/docker/cli-plugins` with the distro copy removed from every
  higher-precedence dir (see [build-caching](build-caching.md)).
- Node.js 22.13.1 + yarn/pnpm; QEMU binfmt (`BINFMT_VERSION=qemu-v9.2.0-51`
  via a systemd unit); a `buildx-setup.service` that creates the `multiarch`
  `docker-container` builder at boot, attaching
  `/opt/runs-fleet/buildkitd.toml` when it exists.
- **Pre-baked Docker images** (`PREBAKE_IMAGES`): mysql 8.0/8.4, postgres
  16/17, redis 7, `moby/buildkit:buildx-stable-1`,
  `tonistiigi/binfmt:${BINFMT_VERSION}`, and
  `mcr.microsoft.com/playwright:v1.57.0-noble` (the one exact pin).
- Vault CLI, yq, CloudWatch agent + config, `actions/runner` OS deps, CI dev
  tools, Java 21 + sbt, Python 3.11–3.13 + pipx, Ruby 3.2/3.4 + bundler.
- **Actions tool cache** (`/opt/hostedtoolcache`, chowned to `ec2-user`):
  Python 3.11/3.12/3.13, Ruby 3.2/3.4, Node lines `20 22 24 20.12 22.15 22.18`,
  Go 1.24/1.25/1.26 (newest `GO_PATCHES_PER_LINE=2` patches each **plus 8
  explicitly pinned 1.25 patches**), Temurin JDK 17/21, and — new in #448 —
  Helm 4.2, kubectl 1.35, protoc v25.6, uv 0.8, GraalVM CE for JDK 25
  (both `25.0.2` and `25.1`).
- The `actions/runner` tarball itself, version + SHA-256 injected by the
  workflow (no template defaults).
- `provision-base-hook.sh` (empty upstream) runs just before cleanup, then the
  Trivy filesystem scan gates the snapshot.

**Runner (`provision-runs-fleet.sh`, ~3 min)** — only the runs-fleet
orchestration bits, and it *repeats* the `--releasever=latest` upgrade because
the Trivy gate also runs in this layer and runner AMIs rebuild far more often
than base:

- `docker pull` the `runs-fleet-runner` ECR image, `docker create` + `docker cp`
  three binaries out of it: the agent, the buildx shim (installed as the
  `docker-buildx` cli-plugin, real path recorded to
  `/opt/runs-fleet/buildx-real-path`), and the mirror proxy (installed to
  `/usr/local/sbin`).
- `CAP_NET_BIND_SERVICE` on the agent, plus a root-owned
  `/usr/local/sbin/runs-fleet-cache-engage` helper and a scoped
  `/etc/sudoers.d/10-runs-fleet-cache` drop-in for the transparent cache
  interceptor.
- `runs-fleet-mirror-proxy.service` + a `runs-fleet-mirror-ready` readiness
  poll, both baked unconditionally and gated at runtime on
  `/opt/runs-fleet/mirror-env`, which only `ECR_PULL_THROUGH_ENDPOINT` writes
  (see [registry-mirroring](registry-mirroring.md)).
- `runs-fleet-agent.service` (carrying `AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache`,
  `PIPX_HOME`/`PIPX_BIN_DIR`, and a `PATH` prepending `/opt/pipx/bin`),
  `boot-lib.sh`, `agent-bootstrap.sh`, the cloud-init **per-boot** script, and a
  CloudWatch agent config override (`RunsFleet/Runner` namespace; the `logs`
  section was removed in #446 — the runner role never had permission for those
  paths).

**Then, before the snapshot:**
[packer/provision-validate-agent.sh](../../packer/provision-validate-agent.sh)
(new) — a post-bake smoke test on the runner layer only, and
[packer/provision-trivy-scan.sh](../../packer/provision-trivy-scan.sh) on both
layers.

### provision-validate-agent.sh — what it asserts

Cheapest checks only; anything needing AWS/Vault/GitHub credentials is out of
scope. It fails the build *before* snapshot on:

- The agent binary exists, is ELF, matches the **expected arch** (catching the
  common silent failure: `docker pull --platform` falling back to the host arch
  when `:latest` lacks the requested platform), and can actually be `exec`'d.
- `boot-lib.sh`, `agent-bootstrap.sh` and the per-boot cloud-init script exist,
  are non-empty/executable, and pass `bash -n`.
- `runs-fleet-agent.service` passes `systemd-analyze verify` **and** contains
  `AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache`, `PIPX_BIN_DIR=/opt/pipx/bin`,
  and a `PATH` prepending `/opt/pipx/bin`.
- `/opt/pipx/bin` exists and is `ec2-user`-writable.
- Unversioned `python`/`pip`/`pipx` and `ruby`/`gem`/`bundle` resolve, defaulting
  to 3.12 and 3.4; every baked Python/Ruby has a tool-cache entry with a
  `.complete` marker and a runnable interpreter; per-version Python headers and
  the default Ruby's `ruby.h` are present.
- Every pre-baked tool-cache entry is present, `.complete`, and runnable —
  including each of the 8 pinned Go patches individually, and the *exact
  platform segment and layout each `setup-*` action expects*: `helm` under
  `bin/`, `kubectl` as a bare binary (`tc.cacheFile`, no `bin/` subtree),
  `protoc` keyed with the leading `v`, and `uv` keyed by the **GNU arch**
  (`x86_64`/`aarch64`) rather than `x64`/`arm64`.
- The cache-engage helper is `root:root 755`, syntactically valid, **refuses a
  host other than the one it pins**, and its sudoers drop-in is `root:root 440`,
  passes `visudo -cf`, and drives a real engage→disengage round trip.

### Workflows

| Workflow | Trigger | Does |
| --- | --- | --- |
| [ci.yml](../../.github/workflows/ci.yml) | `pull_request`, dispatch | paths-filtered `changes` → `pin-sync`, then build (admin UI + `go build`) → lint → test |
| [build-runner.yml](../../.github/workflows/build-runner.yml) | push to main / `v*.*.*` tags on runner paths, dispatch, `workflow_call` | per-arch native build+push → `-unverified` manifest → Trivy scan → promote to `:$VERSION`/`:latest` (deleting the images on scan failure) |
| [build-amis.yml](../../.github/workflows/build-amis.yml) | push to main on `packer/**`/`scripts/**`, `workflow_run` after "Build Runner Image", dispatch | `changes` → `build-base` → `build-runner-ami` (`needs: build-base`) → launch-template version + `cleanup-old-amis` + `cleanup-orphan-builders` |
| [deploy.yml](../../.github/workflows/deploy.yml) | push to main on server paths, dispatch | version calc from ECR tags → per-arch build/push → multi-arch manifest → coverage badge → ECS rolling deploy with rollout polling |
| [runner-staleness.yml](../../.github/workflows/runner-staleness.yml) | dispatch, PR touching the min-version file | read-only: compares the newest base AMI's `RunnerVersion` tag per arch against upstream; warns at 21d, errors at 30d, **builds nothing** |

### Runner container image

Extends `ghcr.io/actions/actions-runner` (`RUNNER_BASE_TAG`, default 2.335.1).
Stage 1 (`golang:1.26-alpine`, `--platform=$BUILDPLATFORM`) cross-compiles all
three host binaries. Stage 2 (named `runtime` — load-bearing, see Gotchas)
strips the upstream-bundled Docker binaries and reinstalls `docker-ce`,
`docker-ce-cli`, `containerd.io` from Docker's apt repo; installs SHA-256-pinned
buildx 0.35.0 and compose 5.3.1 cli-plugins from their upstream GitHub releases;
deletes `externals/node20` with `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24=true`;
self-updates the bundled npm; narrows sudo to apt; copies the three binaries in.

### Nix flake

Packages `server` (default), `agent-{amd64,arm64}`, `buildx-shim-{amd64,arm64}`,
`docker`, `admin-ui`, and `golangci-lint`. Dev shell pins **Go 1.26** and a
`golangci-lint` overridden to **2.9.0** — the version CI installs — built with
`buildGo126Module`. `packer` is allowlisted as unfree. There is **no
`mirror-proxy` Nix package** (the shim has one, the proxy does not).

## Talks To [coverage: high -- 10 sources]

- **AWS ECR** — push targets for both images. The Makefile logs in with
  `aws ecr get-login-password`; the Packer runner layer `docker pull`s the
  runner image (repo overridable via the `ecr_repository` variable /
  `ECR_REPOSITORY_RUNNER` secret) to extract the three binaries. When
  `ECR_PULL_THROUGH_ENDPOINT` is set, runner instances also pull *through* ECR
  (#439) — but only from the runner layer onward; base-layer pre-bake pulls
  still go to Docker Hub directly.
- **Packer / AWS EC2** — four templates build in `ap-northeast-1` over
  SSH-via-SSM (`ssh_interface = "session_manager"`, no public IP or bastion) on
  `c7g.xlarge` (arm64 base), `c6i.xlarge` (amd64 base) and `c7i.xlarge` (amd64
  runner) builders inside `PACKER_VPC_ID`; the `runs-fleet-runner` security
  group and instance profile are discovered by name. 30 GiB gp3 root volume.
- **Docker Hub / GHCR / upstream release CDNs** — `PREBAKE_IMAGES`; the runner
  image's base; and the pinned/checksum-verified downloads in
  `provision-base.sh` (nodejs.org, go.dev, api.adoptium.net, get.helm.sh,
  dl.k8s.io, GitHub releases for buildx/compose/protoc/uv/GraalVM/gh).
- **GitHub Actions runner releases** — `build-amis.yml`'s "Resolve
  actions/runner version" step reads `releases/latest`, extracts the per-arch
  SHA-256 from the release body's HTML-comment markers, enforces the
  `.github/RUNNER_MIN_VERSION` floor with `sort -V`, and passes both to Packer.
  `packer/Makefile` reimplements the same resolution for local builds.
- **EC2 launch templates** — each successful runner-AMI build creates a new
  version from `$Latest` and flips the default;
  [pkg/fleet/fleet.go](../../pkg/fleet/fleet.go) resolves `"$Latest"` at
  CreateFleet time (see Gotchas).
- **Helm / Kubernetes** — [deploy/helm/runs-fleet/](../../deploy/helm/runs-fleet/)
  renders the orchestrator Deployment, ServiceAccount (IRSA annotations),
  Service, RBAC, Secrets, and optional Ingress / Istio Gateway+VirtualService.
  No runner pods, Valkey, or Karpenter.
- **ECS** — `deploy.yml` re-registers the task definition with the new image and
  polls the created deployment's `rolloutState` until terminal, treating a
  vanished deployment (circuit-breaker rollback) as failure.
- **Trivy** — `make scan-runner`/`sbom-runner` run `aquasec/trivy:0.70.0` in a
  container; `provision-trivy-scan.sh` installs the same pinned version on the
  builder. All three paths share
  [.trivy/trivy.yaml](../../.trivy/trivy.yaml),
  [.trivy/vex.json](../../.trivy/vex.json), and
  [.trivy/gate.sh](../../.trivy/gate.sh).
- **Nix** — nixpkgs unstable + flake-utils; `buildGoModule` for Go binaries,
  `buildNpmPackage` for the admin UI.

## API Surface [coverage: high -- 8 sources]

### Make targets ([Makefile](../../Makefile))

| Target | Purpose |
| --- | --- |
| `init` | `go mod download` + `verify`, create `bin/` |
| `build-admin-ui` | `npm ci && npm run build` in `pkg/admin/ui` |
| `build-server` / `build` | Static Linux build of `cmd/server` (depends on the UI) |
| `test` / `coverage` | `go test -race -parallel=$(CPUS) ./...` |
| `lint` | `CGO_ENABLED=0 golangci-lint run --concurrency=$(CPUS) --timeout=$(LINT_TIMEOUT)` |
| `docker-build` / `docker-push` | Orchestrator image build / ECR push |
| `docker-build-runner` | Runner image, local arch only (`RUNNER_BASE_TAG?=2.335.1`) |
| `docker-push-runner` | Multi-arch: `make -j2 --output-sync=target` both arches, then `buildx imagetools create` |
| `scan-runner` | Build + Trivy scan, then `.trivy/gate.sh` (identical to CI) |
| `sbom-runner` | CycloneDX SBOM at `bin/runs-fleet-runner.sbom.json` |
| `run-server`, `deps`, `mocks`, `ci`, `clean`, `help` | Local dev loop |

`CONTAINER_CLI` autodetects podman, falling back to docker.

### Packer make targets ([packer/Makefile](../../packer/Makefile))

`base` / `runs-fleet` build both arches (`MAKEFLAGS += -j2`);
`base-{arm64,amd64}` and `runs-fleet-{arm64,amd64}` chain init → validate →
build. The base targets resolve `runner_version`/`runner_sha256` themselves via
`gh api` (with the same `RUNNER_MIN_VERSION` floor check) so `make base` works
without hand-copying digests. `ami-list` tabulates all four AMI families;
`clean` keeps the latest 2 runner AMIs and 1 base AMI per arch.

### Nix outputs ([flake.nix](../../flake.nix))

| Package | Build |
| --- | --- |
| `.#server` (default) | `buildGoModule`, subPackage `cmd/server`, CGO off, static |
| `.#agent-{amd64,arm64}` | Agent binary per GOARCH |
| `.#buildx-shim-{amd64,arm64}` | Buildx cache shim per GOARCH |
| `.#docker` | OCI image via `dockerTools.buildImage`; exposes 8080, env `AWS_REGION=ap-northeast-1`, `RUNS_FLEET_LOG_LEVEL=info` |
| `.#admin-ui` | Next.js static export via `buildNpmPackage` |
| `.#golangci-lint` | The 2.9.0-pinned linter, also in the dev shell |

### Packer variables

| Variable | Templates | Default | Description |
| --- | --- | --- | --- |
| `region` | all 4 | `ap-northeast-1` | Build region |
| `ami_version` | all 4 | `1.0.0` | `Version` tag value |
| `vpc_id` | all 4 | (required) | Builder VPC; subnet/SG discovered by filter |
| `extra_tags` | all 4 | `{}` | Downstream fork tag merge (`vars.AMI_EXTRA_TAGS`); keys override the built-in set |
| `runner_version` | base only | **no default** | `actions/runner` version, resolved at bake time |
| `runner_sha256` | base only | **no default** | Its per-arch tarball digest |
| `ecr_repository` | runner only | `runs-fleet-runner` | Source image for the extracted binaries |
| `ecr_pull_through_endpoint` | runner only | `""` | ECR pull-through cache endpoint; empty bakes the mirror inert |

Base templates filter the Amazon-owned `al2023-ami-2023.*-kernel-6.1-{arch}`
source AMI (deliberately narrow: the broader glob also matches
`al2023-ami-minimal-*`, which ships without `amazon-ssm-agent`, and Packer's
`session_manager` SSH then times out). Runner templates filter
`runner-base-{arch}-*`, `owners = ["self"]`, `most_recent = true`.

### Trivy gate ([.trivy/gate.sh](../../.trivy/gate.sh))

Reads a Trivy JSON report (arg or stdin), already severity-filtered by
`trivy.yaml` and VEX-filtered by `vex.json`, and prints two sections. It fails
**only** on findings matching `Class == "os-pkgs"` or a `Target` matching
`runs-fleet-agent|runs-fleet-buildx-shim|runs-fleet-mirror-proxy`. Everything
else — Docker/containerd binaries, the buildx/compose cli-plugins,
npm-bundled `node_modules` — is reported non-blocking. It is
**target-scoped, not CVE-id-scoped**: a *new* CVE in an OS package or in one of
our Go binaries still fails.

### Pin-sync check ([.github/scripts/check-pin-sync.sh](../../.github/scripts/check-pin-sync.sh))

Asserts, and fails CI on mismatch: buildx version + both arch digests, and
compose version + both arch digests, between
`docker/runner/Dockerfile` ARGs and `packer/provision-base.sh` variables; plus
`golangci-lint` version between `flake.nix`'s `golangciLintVersion` and
`ci.yml`'s install step, **and** between `ci.yml`'s install step and its own
tool-cache key (a stale key restores the previous binary and silently defeats a
bump).

## Data [coverage: high -- 9 sources]

### Image tags

- Server: `runs-fleet:latest` locally; `$ECR_REGISTRY/runs-fleet:$VERSION` plus
  `:latest` and per-arch `-amd64`/`-arm64` staging tags in ECR. Cache via
  `type=registry,ref=...:cache-$arch,mode=max,image-manifest=true`.
- Runner: `runs-fleet-runner:latest` locally. CI publishes per-arch tags, joins
  them into `$VERSION-unverified`, scans that, then promotes the *manifest* to
  `$VERSION` (+ `latest` on push) via `ecr put-image` and deletes the staging
  tags. Build args: `RUNNER_BASE_TAG` (default 2.335.1 in both Makefile and
  Dockerfile, CI override `vars.RUNNER_BASE_TAG`), `VERSION` (→
  `-X main.version`), `TARGETARCH`.

### AMI naming and tags

`runner-base-{arch}-{timestamp}` and `runs-fleet-runner-{arch}-{timestamp}`.
Tags: `Name`, `Version`, `OS = Amazon Linux 2023`, `Architecture`, `ManagedBy =
Packer`, `BuildTimestamp`, `Stage` (`base` / `application`), plus
`RunnerVersion` on base only and `Runner = latest` + `BaseAMI` on runner only,
merged with `extra_tags`. `run_tags`/`run_volume_tags`/`snapshot_tags` mark
builders `created-by = runs-fleet-packer`; `cleanup-orphan-builders` sweeps any
older than 60 min (well clear of the ~16 min base build).

### Pre-baked Docker image store and tool cache

`/var/lib/docker` inside the base AMI carries the pulled `PREBAKE_IMAGES`
layers; `/opt/hostedtoolcache` carries the Actions tool cache. Both ride the
AMI root volume (30 GiB), so both persist through the snapshot into every
instance booted from that AMI. Both are `--skip-dirs`-excluded from the Trivy
scan (see Key Decisions).

### Reference Terraform ([deploy/terraform/](../../deploy/terraform/))

Illustrative samples with placeholder variables, not production modules:

- `dynamodb.tf` — four PAY_PER_REQUEST tables: `runs-fleet-jobs`
  (`pool-created-at-index` **required**, `instance-id-index` and
  `pool-status-index` optional), `runs-fleet-pools` (one table, four logical row
  kinds — pool config, reconcile locks, instance claims, runner-offline
  sightings — with TTL on `claim_expiry` only, the table's one TTL slot),
  `runs-fleet-circuit-state`, `runs-fleet-audit` (ULID key, `user-index` GSI,
  ~90-day TTL).
- `queues.tf` — the FIFO main queue and standard pool/termination/events queues,
  each paired with a type-matched DLQ.
- `iam.tf` — three roles: runner instance role + profile, orchestrator task role
  (Fargate trust shown; IRSA variant described in a comment), CI/Packer OIDC
  role.

### Helm values structure

`aws.*` carries the EC2 wiring: `baseUrl` (required, served to runners as
`ACTIONS_CACHE_URL`), VPC/subnets/SG/instance profile, queue URLs, DynamoDB
tables and jobs-GSI names, S3 buckets, runner image, launch template name, spot
toggle, `maxRuntimeMinutes` (360), a free-form `tags` map, and the
cost-attribution overrides `tagKeyApplication`/`tagValueApplication`/
`tagKeyService`/`tagValueService` (empty = the orchestrator defaults
`Application=runs-fleet`, `Service=runner`). Other top-level keys:
`commonLabels`, `orchestrator.*` (incl. `serviceAccount.annotations`, the IRSA
hook), `github.*`, `admin.*`, `labelAliases`, `hotPools`, `secrets.*`
(ssm/vault), `ingress`, `logging.*`, `metrics.*`, `istio.*`.

`admin.*` plumbs the native-OIDC admin auth and audit persistence: `rateLimit`
(60, always rendered), `auditTable` (empty = admin actions logged via slog but
not persisted), `oidc.*` (`issuerUrl`, `clientId`, `clientSecret`,
`redirectUrl` — defaults to `{aws.baseUrl}/api/auth/callback` — `scopes`
default `openid,profile,email`, `groupsClaim` default `groups`),
`sessionSecret`, `sessionTtlMinutes` (480, always rendered), `existingSecret`.
All auth fields default to `""`, matching the orchestrator's
auth-disabled-when-unset design. Secrets take one of two paths: plain values
land in a chart-created `<fullname>-admin-secrets` Secret, or
`admin.existingSecret` names a pre-existing one carrying
`RUNS_FLEET_ADMIN_OIDC_CLIENT_SECRET` and `RUNS_FLEET_ADMIN_SESSION_SECRET`.

`hotPools` is a single master toggle plus fleet-wide `caps`; there is
deliberately no per-pool config in the chart (per-repo tuning lives in the
admin UI).

`metrics.cloudwatch.enabled` is now **`false`** — see Key Decisions.

## Key Decisions [coverage: high -- 14 sources]

- **Runner image extends the official `ghcr.io/actions/actions-runner` base.**
  Replaced an earlier `ubuntu:24.04` + from-source docker-cli + SHA-pinned
  npm-patch approach, which froze vendored dependencies and made every new CVE a
  bump-and-hash commit (71 stdlib CVEs from the custom docker-cli alone). Policy
  and the concrete anti-patterns are codified in
  [docker/runner/CLAUDE.md](../../docker/runner/CLAUDE.md).
- **Package-install order of preference for the runner image**, strictly:
  (1) Docker's apt repo or Ubuntu apt — apt-managed packages get security
  updates via `apt-get upgrade` on every rebuild; (2) the package's own apt
  repo (NodeSource, hashicorp, …), still apt-managed; (3) an official upstream
  binary release, *only* if no apt path exists or apt demonstrably lags security
  fixes — pin the version and run `make scan-runner`; (4) **never** build from
  source in the Dockerfile, vendor SHA512-pinned npm/PyPI tarballs, or add
  per-CVE in-place patch RUN steps. Ask the user before reaching for option 3.
- **The buildx/compose cli-plugins are the sanctioned option-3 exception.**
  Docker's apt plugin packages lag the plugin projects by months and shipped
  25+ HIGH findings versus 1 at the upstream releases. Digests are pinned
  **in this repo**, not fetched from the release's `checksums.txt`, because a
  compromised release could serve a matching same-origin checksum file and
  upstream signs nothing. Bumping requires updating the version *and all four*
  digest ARGs, re-running `make scan-runner`, and reviewing `.trivy/vex.json`
  (its PURLs pin the versions these plugins vendor, so a bump may invalidate
  statements). The AMI pins the same versions — `check-pin-sync.sh` enforces it.
- **VEX, never `.trivyignore`.** A previous `.trivyignore` was removed because it
  hid *future* CVEs in the same packages with no audit trail. A suppression must
  be an OpenVEX statement with `status: not_affected`, a justification matching
  the actual situation, an impact statement explaining why the path is
  unreachable, and a **PURL pinned to the exact vulnerable version** so a bump
  invalidates the statement and forces re-review. A genuinely-affecting CVE with
  no upstream fix is not suppressed — it goes upstream and back to the user.
- **The gate is scoped to what we can remediate, and the same script runs in all
  three places.** Failing on CVEs inside third-party prebuilt binaries we
  install but cannot rebuild would perpetually block image promotion *and* the
  AMI rebuild until upstream republishes. `make scan-runner`,
  `build-runner.yml`'s scan, and `provision-trivy-scan.sh` all invoke
  `.trivy/gate.sh`, so local and CI cannot disagree. The AMI variant scans the
  provisioned filesystem *before* the snapshot, so no vulnerable AMI is ever
  registered.
- **Two-layer AMI with a "default: base" placement rule.** Anything stable
  across agent-binary revisions — OS packages, toolchains, `actions/runner`
  itself — goes in `provision-base.sh`. `provision-runs-fleet.sh` keeps a small,
  exhaustive runs-fleet orchestration list, which is what keeps the frequent
  runner rebuild at ~3 min against base's ~16 min.
- **`dnf upgrade --releasever=latest`, in both layers (#450, #442).** AL2023
  pins dnf to the release snapshot its source AMI shipped, so a bare
  `dnf upgrade` reports "Nothing to do" even when fixed packages exist — the
  fixes live in a *newer* snapshot. Without this the Trivy gate fails on
  kernel/containerd/docker/wget CVEs that nothing in the build could advance.
  The kernel is `installonly`, so the upgrade *adds* the new one and leaves the
  old package in the RPM DB where Trivy still reads it; hence the
  `--oldinstallonly` removal with `protect_running_kernel=false` — safe only
  because this image is snapshotted and never rebooted in place. The repeat in
  the runner layer exists because that layer runs the gate and rebuilds far more
  often. Separately, Go-toolchain CVEs (e.g. CVE-2026-46600) are cleared by
  bumping the `toolchain` directive — `go.mod` now carries `go 1.25.12` with
  `toolchain go1.26.6`.
- **Tool-cache entries must match the key each `setup-*` action computes
  (#452, #448).** This is the single most error-prone thing in the base layer,
  because a wrong directory name is invisible at runtime — the action silently
  re-downloads. Three distinct failure shapes are handled: `setup-go` passes a
  full `go 1.x.y` / `toolchain` line through as an **exact-patch** spec that
  bypasses range matching, so the newest-N-patches window can never serve it —
  hence 8 explicitly pinned 1.25 patches alongside the window; `setup-node`'s
  `tc.find` range-matches, but a spec naming a *minor line with no baked patch
  at all* can never match — hence entries per requested **line**
  (`20.12`, `22.15`, `22.18`), not per major; and the common tools each key
  differently (`kubectl` a bare binary, `protoc` keyed with its leading `v`,
  `uv` keyed by GNU arch, GraalVM's entry named `ce` while its artifact says
  `community`, and `25.1` coerced to `25.1.0` by `toSemVer`).
  `provision-validate-agent.sh` asserts each of these shapes rather than merely
  "something runnable exists".
- **`actions/runner` is resolved at bake time (#421), not pinned (#420 raised
  the floor to 2.336.0).** GitHub expires runners on a rolling clock:
  registration needs at least `.github/RUNNER_MIN_VERSION`, but *executing jobs*
  requires each release be installed within 30 days of publication, so any
  static literal expires ~30 days after the release it names. The workflow
  resolves `releases/latest`, takes the per-arch SHA-256 out of the release
  notes, and passes both to Packer; the templates declare them with **no
  defaults** so a missing value fails the build. Resolution happens on the
  workflow runner, not inside the builder, because the builder has no GitHub
  credential (60 req/hr anonymous) and `tags` is evaluated at build-plan time so
  an in-instance value could never reach the `RunnerVersion` tag. The floor is
  the guard against floating: `sort -V` rejects a yanked or rolled-back upstream
  release. The accepted tradeoff is that AMI contents are no longer reproducible
  from a git SHA alone.
- **Pre-baked Docker images in the base layer (PR #386).** Ephemeral runners boot
  with an empty `/var/lib/docker`, so every job re-pulled service/build images
  from Docker Hub (~25s for `mysql:8.0` alone). Pulling during base provisioning
  persists the layers on the root volume. Pulls get 3-attempt retry and an
  explicit `systemctl start docker` (the daemon is only *enabled* during
  provisioning). `tonistiigi/binfmt` reuses the same `BINFMT_VERSION` as the
  `binfmt-qemu.service` unit so the two refs cannot diverge. Prefer floating
  tags so a rebuild picks up updates; the Playwright pin is the deliberate
  exception, because only an exact match with the consumer's
  `@playwright/test` version is a cache hit.
- **`--skip-dirs /var/lib/docker` and `--skip-dirs /opt/hostedtoolcache`.** The
  image store's nested dpkg/rpm DBs would be scanned as host packages and could
  hard-block the os-pkgs gate, and buildkit's Go binaries would flood the report
  — upstream publishers scan those images, not us. The tool cache is excluded
  for a sharper reason: its only hits are `go.mod` files *inside the Go
  toolchains' own source trees* (x/crypto pins for internal codegen tools, the
  stdlib's vendored x/net), which even the newest patched Go release flags — so
  **no baked version could ever scan clean**.
- **One AMI workflow with enforced layer ordering.** Base and runner builds used
  to be separate workflows racing on the same push — the ~3 min runner build
  would resolve a stale base via `source_ami_filter` before the ~16 min base
  finished. `needs: build-base` plus paths-filtering fixed it; a push that also
  rebuilds the runner *image* defers the AMI to the `workflow_run` cascade so it
  never bakes an about-to-be-replaced `:latest`. `cancel-in-progress` is
  disabled specifically for `workflow_run` so a cascade queues behind an
  in-flight base build instead of killing it.
- **The runner image rebuild decision is derived from the import graph, not a
  glob.** `build-runner.yml` runs `go list -deps ./cmd/agent ./cmd/buildx-shim
  ./cmd/mirror-proxy` and diffs only those first-party package dirs plus the
  non-Go inputs, so orchestrator-only changes (e.g. the `pkg/cache` v1 server)
  don't rebuild the image — while a `go list` failure falls back to building.
  Its *trigger* paths stay a broad `pkg/**` on purpose: over-triggering is safe,
  hand-maintaining the agent's import list is not.
- **CloudWatch metrics now ship disabled, and it took two changes (this
  branch).** Nothing queries the `RunsFleet` namespace — no alarms, no
  dashboards, and the admin console reads DynamoDB — while every metric costs
  one un-batched `PutMetricData` call plus a per-series monthly charge (pool
  reconciliation alone emits 9 gauges per pool per 60s pass, and again on every
  queued-job webhook). The Go default in
  [pkg/config/config.go:257](../../pkg/config/config.go) flipped to `false`
  **and** `values.yaml`'s `metrics.cloudwatch.enabled` flipped `true`→`false`,
  because `deployment.yaml` renders
  `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` **unconditionally** from values
  ([templates/deployment.yaml:268-269](../../deploy/helm/runs-fleet/templates/deployment.yaml))
  — there is no `{{- if }}` guard, so the Go default alone would never reach a
  deployment. Every `Publish*` call site and the backend itself are untouched:
  re-enabling is one env var. **Caveat for operators:** the real deploy is
  EKS+ArgoCD and this chart flows downstream to a fork, so a downstream values
  file may still pin it `true`; and `AGENTS.md`'s "Metrics" section still
  documents the old `default: true`.
- **Images bake through an ECR pull-through cache when configured (#439, #440,
  #441, #444, #453).** Declaring `ECR_PULL_THROUGH_ENDPOINT` is the entire
  opt-in; unset, the mirror units are baked but inert and behaviour is
  byte-identical. Details in [registry-mirroring](registry-mirroring.md).
- **`golangci-lint` is pinned to the version CI installs (#447).** A local
  linter that disagrees with CI is worse than none — 2.12 reports ~1100 goconst
  findings on this tree that 2.9 does not. `flake.nix` overrides the nixpkgs
  derivation to 2.9.0 and forces its builder to Go 1.26, because golangci-lint
  refuses to load a config whose `go.mod` targets a newer Go language version
  than the one it was built with. `check-pin-sync.sh` keeps flake, CI install
  step, and CI cache key in lockstep.
- **Multi-stage builds with `--platform=$BUILDPLATFORM`.** Go and Node
  toolchains run natively on the build host and cross-compile for
  `$TARGETARCH`, so one buildx invocation produces both arches with no QEMU
  compilation penalty. CI additionally builds each arch on a *native* runner.
- **SSH over SSM in Packer.** No public IP or bastion; only `vpc_id` is passed
  in, the SG and instance profile are discovered by the `runs-fleet-runner` name.
- **Nix for reproducible dev shell + dev binaries.** `buildGoModule` with
  `vendorHash = null` and CGO off produces deterministic static binaries;
  production AMIs instead extract the Docker-built ones from ECR.
- **Helm chart packages the orchestrator only.** Since the K8s runner backend
  was removed (2026-06) the chart carries no runner pods, Valkey, or Karpenter;
  it exists for deployments self-hosting the control plane on Kubernetes while
  all runners remain EC2. Pre-render checks fail loudly on missing required keys.
- **Admin secret is its own independently-gated Secret object (PR #389).** Every
  env var is hardcoded per-field in `deployment.yaml` with no generic
  passthrough, and #389's first cut nested the admin OIDC/session secrets inside
  the `github.existingSecret` gate: a deployer bringing their own GitHub Secret
  but supplying admin secrets as plain values got neither a rendered Secret nor
  an `envFrom` entry — no template error, then a startup crash-loop on
  `cfg.Validate()`'s partial-OIDC check, several layers from the cause.
  `secrets.yaml` now renders two Secrets gated separately, with a
  `runs-fleet.adminSecretName` helper mirroring `runs-fleet.secretName` and the
  `envFrom` secretRef deduplicated when both resolve to the same name.
- **Static binaries everywhere.** All Go builds are `CGO_ENABLED=0`, `-s -w`,
  static. Server runtime is `alpine:3.19` with a non-root user and a `/health`
  HEALTHCHECK.
- **Restricted sudo for the runner user.** The base image's `NOPASSWD:ALL` is
  narrowed to `/usr/bin/apt-get,/usr/bin/apt`. On the AMI the agent gets
  `CAP_NET_BIND_SERVICE` plus a single root-owned engage helper behind a scoped
  sudoers rule instead of running as root — and the helper lives in
  `/usr/local/sbin` (root-owned), **not** under the `ec2-user`-owned
  `/opt/runs-fleet`, so it cannot be swapped out to escalate through the grant.
- **`deploy/terraform/` is a sample, not the deployed policy.** Every file says
  so; resource ARNs are placeholder variables, and bucket/table/queue resources
  are explicitly out of scope. Changing IAM here changes nothing in production
  — the real policy lives in a separate repository.

## Gotchas [coverage: high -- 11 sources]

- **Existing warm-pool instances keep old pre-baked images and tool caches until
  churned.** Both ride the AMI root volume, so editing `PREBAKE_IMAGES` or the
  tool-cache lists only affects instances launched from the *new* AMI.
  Stopped/running warm-pool instances built from an older AMI keep the stale
  store until pool reconciliation replaces them — a fleet is heterogeneous until
  natural churn completes.
- **The two-layer rebuild cascade has three distinct paths.** A base-path push
  rebuilds base *and then* the runner AMI on top of it; a runner-path push
  rebuilds only the runner AMI against the existing latest base; a push that
  also touches `image` paths (`pkg/**`, `cmd/agent/**`, `cmd/buildx-shim/**`,
  `cmd/mirror-proxy/**`, `go.mod`, `.trivy/**`, …) builds *no* AMI directly —
  the runner AMI arrives later via the `workflow_run` cascade after "Build
  Runner Image" promotes `:latest`. When debugging "my packer change didn't
  produce an AMI", check which filter the push matched. Note that
  `provision-validate-agent.sh`, `provision-trivy-scan.sh` and `.trivy/**` are
  listed in the *runner* filter precisely because the broad `packer/**` trigger
  would otherwise fire the workflow with every build job skipping — a silent
  no-op.
- **Pre-baked image CVEs are invisible to the AMI gate.** `--skip-dirs
  /var/lib/docker` is deliberate, but it means a vulnerable `mysql:8.0` layer
  ships silently; the trust boundary is the upstream publisher's own scanning
  plus whenever base is next rebuilt.
- **`$Latest` means highest version number, not the default version.**
  `pkg/fleet/fleet.go` pins `LaunchTemplateSpecification.Version` to
  `"$Latest"`, so `aws ec2 modify-launch-template --default-version N` does
  *not* roll back fleet launches — even though `build-amis.yml` also sets the
  default version. Rolling back a bad AMI requires creating a **new**
  launch-template version cloning the last good one, for *both* arch templates
  ([packer/README.md](../../packer/README.md) has the exact command).
- **Cloud-init per-boot script must exist in the AMI.**
  `provision-runs-fleet.sh` installs the bootstrap trigger under
  `/var/lib/cloud/scripts/per-boot/`. Warm-pool instances stop and restart, so
  `per-instance/` or `once/` would break every resume (root cause of commit
  `490249b`).
- **Nothing is scheduled, so nothing advances on its own.** There is
  deliberately no cron trigger on `build-amis.yml`: OS packages, prebaked
  images, tool-cache entries, and `actions/runner` are all as old as the last
  triggered build. Bake-time resolution only helps *when a build runs*. The
  coverage for that is reporting, not automation: `runner-staleness.yml` warns
  at 21 days past the baked release's publication and errors at 30, never builds
  anything, and is itself `workflow_dispatch` — so even checking is a deliberate
  act. Enforcement for github.com began 2026-09-25 with brownouts from
  2026-08-24; Enterprise Cloud with Data Residency was enforced from 2026-07-31.
- **The container image is a separate, static supply chain on the same 30-day
  clock.** `RUNNER_BASE_TAG` 2.335.1 in the Makefile and Dockerfile
  (`vars.RUNNER_BASE_TAG` to override) gets none of the AMI's bake-time
  resolution, so EC2 jobs and container-based runs can report different runner
  versions.
- **The `runtime` stage name is load-bearing in *both* Dockerfiles.**
  `no-cache-filters: runtime` in `build-runner.yml` and `deploy.yml` matches
  declared stage names only; the earlier `stage-1` (BuildKit's synthetic name
  for the then-unnamed final stage) matched nothing, so byte-identical layers
  like `npm install -g npm@latest` and `apt-get upgrade` were served stale from
  the registry cache instead of running fresh. Renaming the stage silently
  reintroduces that.
- **The server Dockerfile and the runner Dockerfile build on different Go
  images.** `Dockerfile` uses `golang:1.25-alpine`, `docker/runner/Dockerfile`
  uses `golang:1.26-alpine`, and `go.mod` declares `go 1.25.12` with
  `toolchain go1.26.6`. The server build therefore relies on the toolchain
  directive being honoured (a download) rather than the base image already
  carrying it — worth knowing before assuming a CVE fix reached both images.
- **Nix Go binaries and the AMI's diverge.** `nix build .#agent-arm64` builds
  from the Nix store; the AMI's binaries are extracted from the
  `runs-fleet-runner` ECR image at Packer build time. Different toolchain,
  flags, timestamps. Production uses the Docker path; Nix is dev iteration. And
  there is no `.#mirror-proxy` output at all — that binary is Docker-only.
- **`npmDepsHash` placeholder in flake.nix.** The admin-ui hash is the
  `sha256-AAAA...` sentinel, so `.#admin-ui` fails with a hash mismatch until
  it's replaced after a first build attempt.
- **`buildx imagetools create` requires both arch tags pushed first.** The
  manifest step in `docker-push-runner` runs after `make -j2` builds both
  arches; `--output-sync=target` keeps a failure visible and blocks the
  manifest.
- **Trivy download failures are retried, not fatal-on-first-touch.**
  `provision-trivy-scan.sh` uses `--retry-all-errors` because GitHub's release
  CDN has 504'd mid-build; a genuine failure still aborts the build before
  registration. The script also removes Trivy and its caches afterwards to keep
  the AMI lean.
- **`ECR_PULL_THROUGH_ENDPOINT` feeds the runner-AMI build only.** Changing the
  secret or variable triggers nothing — it needs a `Build AMIs` dispatch — and
  base-layer pre-bake pulls still go to Docker Hub directly regardless.
- **`AGENTS.md`'s env-var reference drifts by construction.** It inlines an
  abridged copy of the config reference that has to be updated by hand on every
  change, and it lagged the PR #456 CloudWatch default flip until that PR caught
  it. `docs/CONFIGURATION.md` is the maintained reference; treat `AGENTS.md`'s
  list as advisory.

## Sources [coverage: high]

- [Dockerfile](../../Dockerfile)
- [docker/runner/Dockerfile](../../docker/runner/Dockerfile)
- [docker/runner/CLAUDE.md](../../docker/runner/CLAUDE.md)
- [Makefile](../../Makefile)
- [flake.nix](../../flake.nix)
- [.golangci.yml](../../.golangci.yml)
- [AGENTS.md](../../AGENTS.md)
- [packer/README.md](../../packer/README.md)
- [packer/Makefile](../../packer/Makefile)
- [packer/provision-base.sh](../../packer/provision-base.sh)
- [packer/provision-runs-fleet.sh](../../packer/provision-runs-fleet.sh)
- [packer/provision-validate-agent.sh](../../packer/provision-validate-agent.sh)
- [packer/provision-trivy-scan.sh](../../packer/provision-trivy-scan.sh)
- [packer/runner-base-amd64.pkr.hcl](../../packer/runner-base-amd64.pkr.hcl)
- [packer/runner-base-arm64.pkr.hcl](../../packer/runner-base-arm64.pkr.hcl)
- [packer/runs-fleet-runner-amd64.pkr.hcl](../../packer/runs-fleet-runner-amd64.pkr.hcl)
- [packer/runs-fleet-runner-arm64.pkr.hcl](../../packer/runs-fleet-runner-arm64.pkr.hcl)
- [.trivy/gate.sh](../../.trivy/gate.sh)
- [.github/scripts/check-pin-sync.sh](../../.github/scripts/check-pin-sync.sh)
- [.github/workflows/ci.yml](../../.github/workflows/ci.yml)
- [.github/workflows/build-runner.yml](../../.github/workflows/build-runner.yml)
- [.github/workflows/build-amis.yml](../../.github/workflows/build-amis.yml)
- [.github/workflows/deploy.yml](../../.github/workflows/deploy.yml)
- [.github/workflows/runner-staleness.yml](../../.github/workflows/runner-staleness.yml)
- [deploy/helm/runs-fleet/values.yaml](../../deploy/helm/runs-fleet/values.yaml)
- [deploy/helm/runs-fleet/templates/deployment.yaml](../../deploy/helm/runs-fleet/templates/deployment.yaml)
- [deploy/helm/runs-fleet/templates/secrets.yaml](../../deploy/helm/runs-fleet/templates/secrets.yaml)
- [deploy/terraform/iam.tf](../../deploy/terraform/iam.tf)
- [deploy/terraform/dynamodb.tf](../../deploy/terraform/dynamodb.tf)
- [deploy/terraform/queues.tf](../../deploy/terraform/queues.tf)
