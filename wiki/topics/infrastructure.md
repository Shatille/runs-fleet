---
topic: Infrastructure (Docker, Packer, Helm, Nix)
last_compiled: 2026-07-21
sources_count: 19
---

# Infrastructure (Docker, Packer, Helm, Nix)

## Purpose [coverage: high -- 19 sources]

Build and packaging artifacts for runs-fleet. Three target artifacts ship to
production: a server image (the orchestrator, run on Fargate or a Kubernetes
cluster), a runner container image (carries the `runs-fleet-agent` binary and
doubles as a runnable runner), and runner AMIs for both architectures (what EC2
job instances actually boot). A Helm chart packages the orchestrator for
Kubernetes control planes; runners are EC2-only since the 2026-06 K8s-backend
removal. A Nix flake provides a reproducible dev shell plus pinned Go toolchain
for agent binaries, and `deploy/terraform/` holds illustrative Terraform
samples (IAM, queues, DynamoDB) — the production IaC lives in a separate
repository.

The orchestrator runs as `runs-fleet-server` (cmd/server) and the agent runs
as `runs-fleet-agent` (cmd/agent) — these are the only Go binaries. All
artifacts derive from one repo and one Go module.

## Architecture [coverage: high -- 10 sources]

```
                        runs-fleet repo (Go module)
                                  |
        +-------------------------+--------------------------+
        |                         |                          |
   Server image              Runner image               Runner AMIs
   (Fargate / K8s)      (agent carrier, multi-arch)   (EC2 instances, amd64+arm64)
        |                         |                          |
   Dockerfile           docker/runner/Dockerfile    packer/runner-base-{arch}.pkr.hcl
   alpine:3.19          ghcr.io/actions/            (provision-base.sh)
        |               actions-runner base                  |
   ECR: runs-fleet      ECR: runs-fleet-runner     packer/runs-fleet-runner-{arch}.pkr.hcl
        |                         |                 (provision-runs-fleet.sh,
        +-- Helm chart            +---- agent binary  extracts agent from ECR image)
            (orchestrator only)         extracted into AMI    |
                                                     Launch templates
                                                     (new version per build)
```

Two-layer AMI build, per architecture (`amd64` and `arm64`):

- `runner-base-{arch}-*` is built first (`provision-base.sh`, ~16 min). It is
  the shared OS layer and the default home for any new package
  ([packer/README.md](../../packer/README.md)): full AL2023 packages, git +
  git-lfs, GitHub CLI, Docker + Compose + buildx multiarch builder, QEMU
  binfmt (pinned `qemu-v9.2.0-51` via a systemd unit), **pre-baked Docker
  images** (the `PREBAKE_IMAGES` array: mysql 8.0/8.4, postgres 16/17,
  redis 7, `moby/buildkit:buildx-stable-1`, `tonistiigi/binfmt`), Node.js +
  yarn + pnpm, Vault CLI, yq, CloudWatch + SSM agents, Java 21 + sbt,
  Python 3.11–3.13 + pipx, Ruby 3.2/3.4, pre-populated Actions tool caches
  (Python, Ruby, Node 20/22, Go 1.24/1.25, Temurin JDK 17/21), the
  `actions/runner` binary itself (v2.334.0) and its OS deps, and on ARM64
  only a from-source gold linker for Go race-detector compatibility. A
  downstream hook (`provision-base-hook.sh`, empty upstream) runs just before
  cleanup, and a Trivy filesystem scan gates the snapshot.
- `runs-fleet-runner-{arch}-*` (`provision-runs-fleet.sh`, ~3 min) layers only
  the runs-fleet orchestration bits: the agent binary extracted from the
  `runs-fleet-runner` ECR image (`docker create` + `docker cp`), a
  `CAP_NET_BIND_SERVICE` grant plus a root-owned cache-engage helper and
  scoped sudoers drop-in for the transparent cache interceptor, the
  `runs-fleet-agent.service` systemd unit, the boot helper library,
  `agent-bootstrap.sh`, the cloud-init per-boot script, and a CloudWatch
  config override for `/opt/actions-runner/_diag/*.log`.

Both layers are built by one workflow,
[.github/workflows/build-amis.yml](../../.github/workflows/build-amis.yml):
`build-runner-ami` declares `needs: build-base` so a push touching both
provisioners bakes the runner AMI on the fresh base instead of racing it. Each
successful runner-AMI build writes a new launch-template version
(`runs-fleet-runner-{arch}`) and keeps the latest 2 AMIs per arch.

The runner container image extends the official
`ghcr.io/actions/actions-runner` base (tag via `RUNNER_BASE_TAG`, default
2.335.1): stage 1 cross-compiles the agent, stage 2 strips the upstream-bundled
Docker binaries and reinstalls Docker from its apt repo, deletes the deprecated
Node 20 tree (`FORCE_JAVASCRIPT_ACTIONS_TO_NODE24=true`), self-updates the
bundled npm, restricts sudo to apt, and copies in the agent
([docker/runner/Dockerfile](../../docker/runner/Dockerfile)).

The Nix flake exposes packages (`server`, `agent-amd64`, `agent-arm64`,
`docker`, `admin-ui`) and a dev shell with pinned Go 1.25, golangci-lint,
delve, awscli2, ssm-session-manager-plugin, packer, gnumake, nodejs_20, jq,
yq, and actionlint.

## Talks To [coverage: high -- 8 sources]

- **AWS ECR** — push targets for both server and runner images
  (`$AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com`). The Makefile logs in
  with `aws ecr get-login-password`. The Packer runner-AMI build `docker pull`s
  the runner image from ECR (repo overridable via the `ecr_repository`
  variable / `ECR_REPOSITORY_RUNNER` secret) to extract the agent binary.
- **Packer / AWS EC2** — four templates build AMIs in `ap-northeast-1` using
  SSH-over-SSM (`ssh_interface = "session_manager"`) on `c7g.xlarge` (arm64)
  and `c6i.xlarge`/`c7i.xlarge` (amd64) builders inside `PACKER_VPC_ID`; the
  `runs-fleet-runner` security group and instance profile are discovered by
  name.
- **Docker Hub / GHCR** — `provision-base.sh` pulls the `PREBAKE_IMAGES` set
  from Docker Hub into the AMI image store; the runner image is FROM
  `ghcr.io/actions/actions-runner`.
- **GitHub Actions runner releases** — `provision-base.sh` downloads
  `actions-runner-linux-${ARCH}-${VER}.tar.gz` from the upstream
  `actions/runner` release into `/opt/actions-runner` (base layer, not runner
  layer).
- **EC2 launch templates** — `build-amis.yml` writes a new version per build
  and flips the default; `pkg/fleet/fleet.go` resolves `"$Latest"` (highest
  version number) at CreateFleet time.
- **Helm / Kubernetes** — chart at `deploy/helm/runs-fleet/` renders the
  orchestrator Deployment, ServiceAccount (IRSA via annotations), and optional
  Istio Gateway/VirtualService. No runner pods, Valkey, or Karpenter — those
  left with the K8s backend removal.
- **Nix** — `flake.nix` consumes nixpkgs unstable and flake-utils, builds Go
  binaries via `buildGoModule` and the admin UI via `buildNpmPackage`.
- **Trivy** — `make scan-runner`/`sbom-runner` run `aquasec/trivy:0.70.0` in a
  container; `provision-trivy-scan.sh` installs the same pinned version on the
  builder. All paths share `.trivy/trivy.yaml`, `.trivy/vex.json`, and
  `.trivy/gate.sh`.

## API Surface [coverage: high -- 6 sources]

### Make targets (Makefile)

| Target | Purpose |
| --- | --- |
| `init` | Download Go modules, create `bin/` |
| `build-admin-ui` | `npm ci && npm run build` for `pkg/admin/ui` |
| `build-server` / `build` | Static Linux build of `cmd/server` (depends on UI) |
| `test` / `coverage` | `go test -race -parallel=$(CPUS) ./...` |
| `lint` | `golangci-lint run --concurrency=$(CPUS) --timeout=$(LINT_TIMEOUT)` |
| `docker-build` / `docker-push` | Orchestrator image build / ECR push |
| `docker-build-runner` | Runner image, local arch only (`RUNNER_BASE_TAG?=2.335.1`) |
| `docker-push-runner` | Multi-arch buildx push: amd64+arm64 in parallel via `make -j2`, then `buildx imagetools create` manifest |
| `scan-runner` | Build + Trivy-scan the runner image, apply `.trivy/gate.sh` (matches CI) |
| `sbom-runner` | CycloneDX SBOM at `bin/runs-fleet-runner.sbom.json` |
| `run-server`, `deps`, `mocks`, `ci` | Local dev loop |

`CONTAINER_CLI` autodetects podman, falls back to docker.

### Packer Make targets (packer/Makefile)

`base` / `runs-fleet` build both arches (parallel via `MAKEFLAGS += -j2`);
`base-{arm64,amd64}` and `runs-fleet-{arm64,amd64}` chain init → validate →
build per template. `ami-list` tabulates all four AMI families; `clean` keeps
the latest 2 runner AMIs and 1 base AMI per arch.

### Nix outputs (flake.nix)

| Package | Build |
| --- | --- |
| `.#server` (default) | `runs-fleet-server` via `buildGoModule`, subPackage `cmd/server`, CGO off, static |
| `.#agent-amd64` / `.#agent-arm64` | Agent binary per GOARCH |
| `.#docker` | OCI image via `dockerTools.buildImage`, exposes 8080, env `AWS_REGION=ap-northeast-1`, `RUNS_FLEET_LOG_LEVEL=info` |
| `.#admin-ui` | Next.js static export via `buildNpmPackage` |

`packer` is allowlisted as unfree in `config.allowUnfreePredicate`. Dev shell
sets `AWS_REGION=ap-northeast-1`.

### Packer variables (all four templates)

| Variable | Default | Description |
| --- | --- | --- |
| `region` | `ap-northeast-1` | Build region |
| `ami_version` | `1.0.0` | Tag value |
| `vpc_id` | (required) | Builder VPC; subnet/SG discovered by filter |
| `extra_tags` | `{}` | Downstream fork tag merge (`vars.AMI_EXTRA_TAGS`) |
| `ecr_repository` | `runs-fleet-runner` | Runner templates only; agent-source image repo |

Base templates filter the Amazon-owned AL2023 source AMI; runner templates
filter `runner-base-{arch}-*`, `owners = ["self"]`, `most_recent = true`.

### Workflow triggers (build-amis.yml)

- **push to main** touching `packer/**`, `scripts/**`, or the workflow —
  paths-filtered per layer (`base` vs `runner` vs `image` outputs).
- **workflow_run** after a successful "Build Runner Image" — bakes the fresh
  `:latest` agent into a runner AMI (base skipped).
- **schedule** — weekly cron, Sunday 06:00 UTC: rebuilds base (picking up OS
  updates and re-pulling `PREBAKE_IMAGES`), cascades to the runner AMI.
- **workflow_dispatch** — manual, with `use_fallback_runners` to bootstrap on
  GitHub-hosted runners when no working fleet AMI exists (chicken/egg).

## Data [coverage: high -- 9 sources]

### Image labels and tags

- Server image: `runs-fleet:latest` locally,
  `$ECR_REGISTRY/runs-fleet:$DOCKER_TAG` in ECR.
- Runner image: `runs-fleet-runner:latest` locally; ECR push uses per-arch
  tags `$TAG-amd64` / `$TAG-arm64`, then a multi-arch manifest at the bare
  `$TAG`. Build args: `RUNNER_BASE_TAG` (official base image tag, default
  `2.335.1` in both Makefile and Dockerfile; CI override via
  `vars.RUNNER_BASE_TAG`), `VERSION` (becomes `-X main.version`), `TARGETARCH`.

### AMI naming and tags (Packer)

AMIs register as `runner-base-{arch}-{timestamp}` and
`runs-fleet-runner-{arch}-{timestamp}` with tags `Name`, `Version`, `OS =
Amazon Linux 2023`, `Architecture`, `ManagedBy = Packer`, `BuildTimestamp`,
`Stage` (`base` / `application`), `BaseAMI`, plus any `extra_tags`.
`run_tags`/`run_volume_tags` mark builders `created-by = runs-fleet-packer`;
the workflow's `cleanup-orphan-builders` job sweeps any older than 60 min.

### Pre-baked Docker image store

`/var/lib/docker` inside the base AMI carries the pulled `PREBAKE_IMAGES`
layers (~2.5 GB of the 30 GiB root volume). Because jobs use the host dockerd,
whose image store lives on the AMI root volume, these images persist through
the snapshot into every instance booted from the AMI.

### Reference Terraform (deploy/terraform/)

Illustrative, not production modules: `dynamodb.tf` declares the four tables —
`runs-fleet-jobs` (with `pool-created-at-index` required plus optional
`instance-id-index` / `pool-status-index` GSIs), `runs-fleet-pools`,
`runs-fleet-circuit-state`, and `runs-fleet-audit` (added 2026-07 for admin
audit persistence: ULID hash key, required `user-index` GSI, first table to
use DynamoDB TTL — ~90-day expiry). All PAY_PER_REQUEST.

### Helm values structure

`aws.*` carries the EC2 wiring: `baseUrl` (required, served to runners as
`ACTIONS_CACHE_URL`), VPC/subnets/SG/instance profile, queue URLs, DynamoDB
tables and jobs-GSI names, S3 buckets, runner image URL, launch template name,
spot toggle, max runtime (default 360 min), a free-form `tags` map, and (PR
#389) the cost-attribution overrides `tagKeyApplication` /
`tagValueApplication` / `tagKeyService` / `tagValueService` (empty = the
orchestrator defaults `Application`=`runs-fleet`, `Service`=`runner`).
`orchestrator.serviceAccount.annotations` is the IRSA hook. Other top-level
keys: `commonLabels`, `github.*`, `labelAliases`, `secrets.*` (ssm/vault),
`logging.*`, `metrics.*`, `istio.*`, and (PR #389) `admin.*`.

`admin.*` plumbs the native-OIDC admin auth and audit persistence through the
chart: `rateLimit` (default 60, always rendered), `auditTable` (empty = admin
actions logged via slog but not persisted/queryable), `oidc.*` (`issuerUrl`,
`clientId`, `clientSecret`, `redirectUrl` — defaults to
`{aws.baseUrl}/api/auth/callback` — `scopes` default `openid,profile,email`,
`groupsClaim` default `groups`), `sessionSecret`, `sessionTtlMinutes`
(default 480, always rendered), and `existingSecret`. All auth fields default
to `""`, matching the orchestrator's auth-disabled-when-unset design — a
fully-unset block renders no admin OIDC env vars. Secrets take one of two
paths: plain `oidc.clientSecret`/`sessionSecret` values land in a
chart-created `<fullname>-admin-secrets` Secret, or `admin.existingSecret`
names a pre-existing Secret carrying `RUNS_FLEET_ADMIN_OIDC_CLIENT_SECRET`
and `RUNS_FLEET_ADMIN_SESSION_SECRET`. The Deployment mounts the admin secret
as a second `envFrom` secretRef whenever one exists and its name differs from
the GitHub secret's.

## Key Decisions [coverage: high -- 11 sources]

- **Runner image extends the official `ghcr.io/actions/actions-runner` base.**
  Replaced the earlier `ubuntu:24.04` + from-source docker-cli + SHA-pinned
  npm-patch approach, which froze vendored dependencies and made every new CVE
  a bump-and-hash commit. Upstream-bundled Docker binaries are deleted and
  reinstalled from Docker's apt repo (security updates via `apt-get upgrade`),
  npm self-updates, and the Node 20 tree is removed outright. Policy and
  anti-patterns are codified in
  [docker/runner/CLAUDE.md](../../docker/runner/CLAUDE.md).
- **Two-layer AMI with a "default: base" placement rule.** Anything stable
  across agent-binary revisions — OS packages, toolchains, `actions/runner`
  itself — goes in `provision-base.sh`; `provision-runs-fleet.sh` is kept to
  the small, exhaustive runs-fleet orchestration list. This keeps the frequent
  runner-AMI rebuild at ~3 min vs ~16 min for base
  ([packer/README.md](../../packer/README.md)).
- **Pre-baked Docker images in the base layer (PR #386).** Ephemeral runners
  boot with an empty `/var/lib/docker`, so every job re-pulled service/build
  images from Docker Hub (~25s for `mysql:8.0` alone, vs a ~2s digest check on
  Blacksmith, which pre-caches on its hosts). Pulling during base provisioning
  persists the layers on the AMI root volume. Placement follows the README
  rule — the images are a CI-workload concern stable across agent revisions.
  Pulls get 3-attempt retry and an explicit `systemctl start docker` (the
  daemon is only *enabled* during provisioning). `tonistiigi/binfmt` reuses
  the same `BINFMT_VERSION` variable as the `binfmt-qemu.service` unit so the
  two refs cannot diverge. The weekly base rebuild bounds image staleness at
  ≤7 days, and a moved tag at job time re-pulls only changed layers.
- **One AMI workflow with enforced layer ordering.** Base and runner builds
  used to be separate workflows racing on the same push — the ~3 min runner
  build would resolve a stale base via `source_ami_filter` before the ~16 min
  base build finished. `needs: build-base` plus paths-filtering fixed the
  race; a push that also rebuilds the runner *image* defers the AMI to the
  `workflow_run` cascade so it never bakes an about-to-be-replaced `:latest`.
- **Shared, remediation-scoped Trivy gate for image and AMI.**
  `provision-trivy-scan.sh` scans the provisioned filesystem *before* the
  snapshot, so no vulnerable AMI is ever registered. The same `.trivy/gate.sh`
  as the container path fails only on findings we can fix (OS packages, the
  `runs-fleet-agent` binary); third-party prebuilt binaries are reported but
  non-blocking. Suppressions are per-CVE PURL-pinned OpenVEX, never
  `.trivyignore`. The scan adds `--skip-dirs /var/lib/docker` for the
  pre-baked layers: their nested dpkg/rpm DBs would be scanned as host
  packages and could hard-block the os-pkgs gate, and buildkit's Go binaries
  would flood the report — upstream publishers scan those images, not us.
- **Multi-stage Docker builds with `--platform=$BUILDPLATFORM`.** Go and Node
  toolchains run natively on the build host and cross-compile for
  `$TARGETARCH`, so one buildx invocation produces both arches without QEMU
  compilation penalty.
- **SSH over SSM in Packer.** `ssh_interface = "session_manager"` removes the
  need for a public IP or bastion; only `vpc_id` is passed in, the SG and
  instance profile are discovered by the `runs-fleet-runner` name.
- **Nix for reproducible dev shell + agent binaries.** `buildGoModule` with
  `vendorHash = null` and CGO off produces deterministic static agents;
  production AMIs instead extract the Docker-built agent from ECR.
- **Helm chart packages the orchestrator only.** Since the K8s runner backend
  was removed (2026-06), the chart no longer carries runner pods, Valkey, or
  Karpenter — it exists for deployments that self-host the control plane on a
  Kubernetes cluster while all runners remain EC2. The chart's pre-render
  checks fail loudly on missing required keys.
- **Admin secret is its own independently-gated Secret object (PR #389).**
  Before #389 the chart had no way at all to configure the already-shipped
  admin OIDC auth or audit persistence — every env var is hardcoded per-field
  in `deployment.yaml` with no generic passthrough. The PR's first cut nested
  the admin OIDC/session secrets inside the `github.existingSecret` gate on
  the chart's single Secret: a deployer bringing their own GitHub Secret but
  supplying admin secrets as plain values got neither a rendered Secret nor
  an `envFrom` entry — no template error, then a startup crash-loop on
  `cfg.Validate()`'s partial-OIDC check, several layers from the cause.
  `secrets.yaml` now renders two Secret resources gated separately by
  `github.existingSecret` / `admin.existingSecret`, with a
  `runs-fleet.adminSecretName` helper mirroring `runs-fleet.secretName` and
  the `envFrom` secretRef deduplicated when both resolve to the same name.
- **Static binaries everywhere.** All Go builds are `CGO_ENABLED=0`, `-s -w`,
  static. Server runtime is `alpine:3.19` with a non-root user and `/health`
  HEALTHCHECK.
- **Restricted sudo for the runner user.** The base image's `NOPASSWD:ALL` is
  narrowed to `/usr/bin/apt-get,/usr/bin/apt`; on the AMI side the agent gets
  `CAP_NET_BIND_SERVICE` plus a single root-owned engage helper behind a
  scoped sudoers rule instead of running as root.

## Gotchas [coverage: high -- 8 sources]

- **Existing warm-pool instances keep old pre-baked images until churned.**
  The image store rides the AMI root volume, so editing `PREBAKE_IMAGES` (or
  the weekly refresh itself) only affects instances launched from the *new*
  AMI. Stopped/running warm-pool instances built from an older AMI keep the
  stale store until pool reconciliation replaces them — a fleet is
  heterogeneous until natural churn completes.
- **The two-layer rebuild cascade has three distinct paths.** A base-path push
  rebuilds base *and then* the runner AMI on top of it; a runner-path push
  rebuilds only the runner AMI against the existing latest base; a push that
  also touches image paths (`pkg/**`, `cmd/agent/**`, `go.mod`, ...) builds
  *no* AMI directly — the runner AMI arrives later via the `workflow_run`
  cascade after "Build Runner Image" promotes `:latest`. When debugging "my
  packer change didn't produce an AMI", check which filter the push matched;
  a push matching only `image` paths intentionally skips both AMI jobs.
- **Pre-baked image CVEs are invisible to the AMI gate.** `--skip-dirs
  /var/lib/docker` is deliberate (see Key Decisions), but it means a
  vulnerable `mysql:8.0` layer ships silently — the trust boundary is the
  upstream publisher's own scanning plus the weekly re-pull.
- **`$Latest` means highest version number, not the default version.**
  `pkg/fleet/fleet.go` pins `LaunchTemplateSpecification.Version` to
  `"$Latest"`, so `aws ec2 modify-launch-template --default-version N` does
  *not* roll back fleet launches. Rolling back a bad AMI requires creating a
  *new* launch-template version cloning the last good one, for both arch
  templates ([packer/README.md](../../packer/README.md)).
- **Cloud-init per-boot script must exist in the AMI.**
  `provision-runs-fleet.sh` installs the bootstrap trigger under
  `/var/lib/cloud/scripts/per-boot/`. Warm-pool instances stop and restart, so
  `per-instance/` or `once/` would break every resume (root cause of commit
  `490249b`).
- **Runner version drift between AMI and container image.** The AMI bakes
  `actions/runner` v2.334.0 (`RUNNER_VERSION` in `provision-base.sh`); the
  container tracks the official base image at `RUNNER_BASE_TAG` 2.335.1
  (Makefile and Dockerfile now agree, CI can override via
  `vars.RUNNER_BASE_TAG`). Two supply chains, two bump sites — EC2 jobs and
  container-based runs can run different runner versions between bumps.
- **Nix Go agent vs Docker runner agent diverge.** `nix build .#agent-arm64`
  builds from the Nix store; the AMI's agent is extracted from the
  `runs-fleet-runner` ECR image at Packer build time. Different toolchain,
  flags, timestamps. Production AMIs use the Docker path; Nix is dev
  iteration.
- **`npmDepsHash` placeholder in flake.nix.** The admin-ui hash is the
  `sha256-AAAA...` sentinel; building `.#admin-ui` fails with a hash mismatch
  until it's replaced after the first build attempt.
- **`buildx imagetools create` requires both arch tags pushed first.** The
  manifest step in `docker-push-runner` runs after `make -j2` builds both
  arches; `--output-sync=target` keeps a failure visible and blocks the
  manifest.
- **Trivy download failures are retried, not fatal-on-first-touch.**
  `provision-trivy-scan.sh` uses `--retry-all-errors` because GitHub's release
  CDN has 504'd mid-build before; a genuine failure still aborts the AMI
  build before registration.

## Sources [coverage: high]

- [Dockerfile](../../Dockerfile)
- [docker/runner/Dockerfile](../../docker/runner/Dockerfile)
- [docker/runner/entrypoint.sh](../../docker/runner/entrypoint.sh)
- [docker/runner/CLAUDE.md](../../docker/runner/CLAUDE.md)
- [Makefile](../../Makefile)
- [flake.nix](../../flake.nix)
- [packer/README.md](../../packer/README.md)
- [packer/Makefile](../../packer/Makefile)
- [packer/provision-base.sh](../../packer/provision-base.sh)
- [packer/provision-runs-fleet.sh](../../packer/provision-runs-fleet.sh)
- [packer/provision-trivy-scan.sh](../../packer/provision-trivy-scan.sh)
- [packer/runner-base-arm64.pkr.hcl](../../packer/runner-base-arm64.pkr.hcl) (+ amd64 and runner-layer twins)
- [.github/workflows/build-amis.yml](../../.github/workflows/build-amis.yml)
- [deploy/helm/runs-fleet/Chart.yaml](../../deploy/helm/runs-fleet/Chart.yaml)
- [deploy/helm/runs-fleet/values.yaml](../../deploy/helm/runs-fleet/values.yaml)
- [deploy/helm/runs-fleet/templates/deployment.yaml](../../deploy/helm/runs-fleet/templates/deployment.yaml)
- [deploy/helm/runs-fleet/templates/secrets.yaml](../../deploy/helm/runs-fleet/templates/secrets.yaml)
- [deploy/helm/runs-fleet/templates/_helpers.tpl](../../deploy/helm/runs-fleet/templates/_helpers.tpl)
- [deploy/terraform/dynamodb.tf](../../deploy/terraform/dynamodb.tf)
