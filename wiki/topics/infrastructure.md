---
topic: Infrastructure (Docker, Packer, Helm, Nix)
last_compiled: 2026-04-30
sources_count: 10
---

# Infrastructure (Docker, Packer, Helm, Nix)

## Purpose [coverage: high -- 10 sources]

Build and packaging artifacts for runs-fleet. Three target artifacts ship to
production: a server image (Fargate orchestrator), a runner image (K8s mode),
and a runner AMI (EC2 mode). A Helm chart wraps the K8s deployment, and a Nix
flake provides a reproducible dev shell plus pinned Go toolchain for agent
binaries.

The orchestrator runs as `runs-fleet-server` (cmd/server) and the agent runs
as `runs-fleet-agent` (cmd/agent) — these are the only Go binaries. All
artifacts derive from one repo and one Go module.

## Architecture [coverage: high -- 8 sources]

```
                        runs-fleet repo (Go module)
                                  |
        +-------------------------+--------------------------+
        |                         |                          |
   Server image              Runner image              Runner AMI
   (Fargate)                 (K8s pods)               (EC2 instances)
        |                         |                          |
   Dockerfile           docker/runner/Dockerfile   packer/runs-fleet-runner-arm64.pkr.hcl
   alpine:3.19              ubuntu:24.04             AL2023 (runner-base-arm64)
        |                         |                          |
   ECR: runs-fleet      ECR: runs-fleet-runner       AMI: runs-fleet-runner-arm64-*
        |                                                    |
        +-- Helm chart deploys server image to K8s           +-- Launch templates
            (orchestrator + optional Karpenter NodePool)         (provisioned at boot)
```

Two-layer AMI build:
- `runner-base-arm64-*` is built first (provision-base.sh). It contains the
  shared OS layer: Docker, Docker Compose, Node.js + yarn + pnpm, Vault CLI,
  CloudWatch agent, SSM agent, QEMU binfmt, buildx multiarch builder, and on
  ARM64 only a from-source gold linker for Go race-detector compatibility.
- `runs-fleet-runner-arm64-*` (provision-runs-fleet.sh) layers on top: it
  adds the GitHub Actions runner v2.330.0, AL2023 dev tools group, Java 21
  (Corretto), sbt 1.10.7, the agent binary (extracted from the
  `runs-fleet-runner` ECR image), the runs-fleet-agent.service systemd unit,
  the agent-bootstrap.sh script, and the cloud-init per-boot script.

The Nix flake exposes packages (`server`, `agent-amd64`, `agent-arm64`,
`docker`, `admin-ui`) and a dev shell with pinned Go 1.25, golangci-lint,
delve, awscli2, ssm-session-manager-plugin, packer, gnumake, nodejs_20, jq,
yq, and actionlint.

## Talks To [coverage: high -- 5 sources]

- **AWS ECR** — push targets for both server and runner images
  (`$AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com`). The Makefile logs in
  with `aws ecr get-login-password`. The Packer build `docker pull`s the
  runner image from ECR to extract the agent binary into the AMI.
- **Packer** — `runs-fleet-runner-arm64.pkr.hcl` builds AMIs in
  `ap-northeast-1` using SSH-over-SSM (`ssh_interface = "session_manager"`)
  on a `c7g.xlarge` builder.
- **Helm / Kubernetes** — chart at `deploy/helm/runs-fleet/` renders the
  orchestrator Deployment, ServiceAccount, optional Valkey, optional
  Karpenter NodePool/EC2NodeClass, optional Istio Gateway/VirtualService,
  optional warm-pool placeholder pods.
- **Nix** — `flake.nix` consumes nixpkgs unstable and flake-utils, builds
  Go binaries via `buildGoModule` and the admin UI via `buildNpmPackage`.
- **GitHub Actions runner releases** — both Dockerfile and
  `provision-runs-fleet.sh` download `actions-runner-linux-${ARCH}-${VER}.tar.gz`
  directly from the upstream `actions/runner` GitHub release.

## API Surface [coverage: high -- 4 sources]

### Make targets (Makefile)

| Target | Purpose |
| --- | --- |
| `init` | Download Go modules, create `bin/` |
| `build-admin-ui` | `npm ci && npm run build` for `pkg/admin/ui` |
| `build-server` | Static Linux build of `cmd/server` (depends on UI) |
| `build` | Alias for `build-server` |
| `test` | `go test -race -parallel=$(CPUS) ./...` |
| `coverage` | Tests with coverage profile |
| `lint` | `golangci-lint run --concurrency=$(CPUS) --timeout=$(LINT_TIMEOUT)` |
| `clean` | Remove `bin/`, coverage, UI build artifacts |
| `docker-build` | Build orchestrator image (`runs-fleet:latest`) |
| `docker-push` | Login to ECR, tag, push orchestrator |
| `docker-build-runner` | Build runner image, local arch only |
| `docker-push-runner` | Multi-arch buildx push: amd64+arm64 in parallel via `make -j2`, then `buildx imagetools create` manifest |
| `_build-runner-amd64` / `_build-runner-arm64` | Per-arch buildx targets (private) |
| `run-server` | `go run cmd/server/main.go` |
| `deps` | `go mod tidy && go mod verify` |
| `mocks` | `go generate ./...` |
| `ci` | `deps lint test build` |

`CONTAINER_CLI` autodetects podman, falls back to docker.

### Nix outputs (flake.nix)

| Package | Build |
| --- | --- |
| `.#server` (default) | `runs-fleet-server` via `buildGoModule`, subPackage `cmd/server`, CGO off, static |
| `.#agent-amd64` | Agent binary, `GOARCH=amd64` |
| `.#agent-arm64` | Agent binary, `GOARCH=arm64` |
| `.#docker` | OCI image via `dockerTools.buildImage`, exposes 8080, env `AWS_REGION=ap-northeast-1`, `RUNS_FLEET_LOG_LEVEL=info` |
| `.#admin-ui` | Next.js static export via `buildNpmPackage` |

`packer` is allowlisted as unfree in `config.allowUnfreePredicate`. Dev shell
sets `AWS_REGION=ap-northeast-1`.

### Packer variables (runs-fleet-runner-arm64.pkr.hcl)

| Variable | Default | Description |
| --- | --- | --- |
| `region` | `ap-northeast-1` | Build region |
| `ami_version` | `1.0.0` | Tag value |
| `security_group_id` | (required) | Builder SG |
| `subnet_id` | (required) | Builder subnet |

Source AMI is filtered by `name = runner-base-arm64-*`, `owners = ["self"]`,
`most_recent = true`. The build provisions
`/tmp/agent-bootstrap.sh` and `/tmp/cloud-init-boot.sh` (file provisioner),
then runs `provision-runs-fleet.sh` with `RUNNER_ARCH=arm64`.

### Helm values (deploy/helm/runs-fleet/values.yaml)

Top-level keys: `commonLabels`, `mode` (`ec2` | `k8s`), `aws.*`,
`orchestrator.*`, `runner.*`, `docker.*`, `github.*`, `secrets.*`,
`valkey.*`, `ingress.*`, `logging.*`, `metrics.*`, `warmPool.*`,
`istio.*`, `karpenter.*`.

Highlights:
- `mode: ec2` (default) routes jobs through the SQS/EC2 fleet stack.
- `mode: k8s` enables runner pods, requires `valkey.enabled: true` for the
  queue backend, and optionally `karpenter.enabled: true` to provision nodes.
- `secrets.backend` toggles `ssm` vs `vault`, with `vault.authMethod` of
  `aws | kubernetes | jwt | approle | token`.
- `metrics.{cloudwatch,prometheus,datadog}.enabled` are independent toggles.
- `karpenter.requirements` exposes `architectures`, `capacityTypes`,
  `instanceCategories`, `instanceFamilies`, `instanceSizes`, `minCpu=1`,
  `maxCpu=17` (Karpenter Gt/Lt are exclusive — `>1` means `>=2`).

## Data [coverage: high -- 5 sources]

### Image labels and tags

- Server image: `runs-fleet:latest` locally,
  `$ECR_REGISTRY/runs-fleet:$DOCKER_TAG` in ECR.
- Runner image: `runs-fleet-runner:latest` locally; ECR push uses per-arch
  tags `$TAG-amd64` / `$TAG-arm64`, then a multi-arch manifest at the bare
  `$TAG`. Build args: `RUNNER_VERSION` (default `2.330.0` in Makefile,
  `2.331.0` in the Dockerfile ARG), `VERSION` (becomes `-X main.version`),
  `TARGETARCH`, `DOCKER_CLI_VERSION=v27.5.1`.

### AMI tags (Packer)

```
Name           = runs-fleet-runner-arm64
Version        = 1.0.0
OS             = Amazon Linux 2023
Architecture   = arm64
Runner         = latest
ManagedBy      = Packer
BuildTimestamp = {{timestamp}}
Stage          = application
BaseAMI        = runner-base-arm64
```

`run_tags` and `run_volume_tags` add `created-by = packer` for cleanup of
abandoned builder instances/volumes.

### Helm values structure

`aws.*` carries the EC2-mode wiring: VPC, public/private subnets, SG,
instance profile ARN, all five queue URLs (main, dlq, pool, events,
termination, housekeeping), DynamoDB tables (`runs-fleet-jobs`,
`runs-fleet-pools`, `runs-fleet-circuit-state`), S3 buckets (cache, config),
runner image URL, launch template name (`runs-fleet-runner`), spot toggle,
max runtime minutes (default 360 = 6 hours), key name, and a free-form
`tags` map (PascalCase recommended for AWS Cost Allocation Tags).

`orchestrator.serviceAccount.annotations` is the IRSA hook
(`eks.amazonaws.com/role-arn`).

## Key Decisions [coverage: high -- 5 sources]

- **Multi-stage Docker builds with `--platform=$BUILDPLATFORM`.** The Go and
  Node toolchains run natively on the build host, then cross-compile for
  `$TARGETARCH`. This avoids QEMU emulation for compilation, so a single
  buildx invocation produces both arm64 and amd64 layers without 5x build
  time penalty.
- **Docker CLI built from source (runner image).** A separate
  `docker-builder` stage clones `docker/cli` at `v27.5.1`, copies
  `vendor.mod`/`vendor.sum` into `go.mod`/`go.sum`, bumps
  `golang.org/x/crypto` to `v0.35.0` to patch CVE-2025-68121, then builds
  with `-mod=vendor`. This pins the Go toolchain (not the upstream binary
  release) for the CVE fix.
- **In-image npm CVE patches.** The runner Dockerfile fetches
  `tar@7.5.7` and `@isaacs/brace-expansion@5.0.1` from the npm registry,
  verifies SHA512 against `dist.integrity`, and rewrites every matching
  package directory inside `/home/runner/externals` (the runner's bundled
  Node modules). Build fails if zero `tar` packages were patched.
- **Two-layer AMI (base + application).** `runner-base-arm64` carries
  language toolchains and Docker; `runs-fleet-runner-arm64` adds the
  GitHub runner and the agent. Reduces per-rev build time and makes the
  base reusable across runner variants.
- **ARM-first AMI in `ap-northeast-1`.** Only `runs-fleet-runner-arm64.pkr.hcl`
  exists; AMD64 fallback is handled by EC2 Fleet at request time. The
  builder uses `c7g.xlarge` (Graviton). On ARM64, the base build compiles
  binutils 2.43 from source for the gold linker (Go race detector
  prerequisite) — skipped on amd64.
- **SSH over SSM in Packer.** `ssh_interface = "session_manager"` removes
  the need for a public IP or bastion on the builder; the IAM instance
  profile `runs-fleet-runner` provides SSM access.
- **Nix for reproducible dev shell + agent binaries.** `buildGoModule` with
  `vendorHash = null` (no vendoring) and CGO off produces deterministic
  static agents. Dev shell pins Go 1.25, golangci-lint, delve, awscli2,
  ssm-session-manager-plugin, packer.
- **Helm chart for K8s mode only.** EC2 mode does not deploy via Helm —
  the orchestrator runs on Fargate (Terraform in `shavakan-terraform`).
  Helm exists to package the same orchestrator image for K8s clusters that
  want to self-host the control plane and use Karpenter-provisioned runner
  pods (`mode: k8s`).
- **Static binaries everywhere.** All Go builds are `CGO_ENABLED=0` with
  `-extldflags "-static"`, `-s -w`, and `-X main.version=$VERSION`. The
  server image FROM is `alpine:3.19`; the runner image FROM is
  `ubuntu:24.04` (needed for the GitHub Actions runner's installdependencies).
- **Restricted sudo for the runner user.** Instead of GitHub's default
  `NOPASSWD: ALL`, the runner Dockerfile grants only
  `/usr/bin/apt-get,/usr/bin/apt`. Sudo is kept (not removed) because
  workflows need build deps, but ephemeral pods limit blast radius.

## Gotchas [coverage: high -- 4 sources]

- **Cloud-init per-boot script must exist in the AMI.** `provision-runs-fleet.sh`
  copies `cloud-init-boot.sh` to
  `/var/lib/cloud/scripts/per-boot/runs-fleet-bootstrap.sh`. Without it,
  `agent-bootstrap.sh` never runs at instance start (this was the root cause
  of recent commit `490249b`). Per-boot vs once-only matters: warm-pool
  instances stop and restart, so the bootstrap script must be in
  `per-boot/`, not `per-instance/` or `once/`.
- **Runner version drift between Dockerfile and Makefile.**
  `Dockerfile.runner` has `ARG RUNNER_VERSION=2.331.0`; the Makefile sets
  `RUNNER_VERSION?=2.330.0`; `provision-runs-fleet.sh` hardcodes `2.330.0`.
  The Makefile build arg wins for runner image builds, but the AMI ships
  `2.330.0`. Bumping requires updating all three.
- **`vendorHash` placeholder in flake.nix.** The admin-ui `npmDepsHash` is
  `"sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="` — a sentinel
  meant to be replaced after the first `nix build` failure prints the real
  hash. Building `.#admin-ui` without fixing this fails with a hash
  mismatch error.
- **Nix Go agent vs Docker runner agent diverge.** `nix build .#agent-arm64`
  produces a static binary directly from the Nix store. The AMI's agent is
  pulled out of the `runs-fleet-runner` ECR image at Packer build time
  (`docker create` + `docker cp`). Different toolchain, different timestamps,
  different ld flags. CI typically uses the Docker path for production AMIs;
  Nix is for dev iteration.
- **Helm release naming and `commonLabels` precedence.** The chart applies
  `commonLabels` to all resources, but for runner pods the comment notes
  "System labels (`app`, `runs-fleet.io/*`) take precedence over custom
  labels." Setting `app:` in `runner.labels` is silently overridden.
- **K8s mode requires Valkey.** `mode: k8s` without `valkey.enabled: true`
  (or an external `RUNS_FLEET_VALKEY_ADDR`) leaves the queue backend
  unconfigured. The chart does not enforce this — the orchestrator fails at
  runtime.
- **Karpenter `minCpu`/`maxCpu` are exclusive bounds.** Comment: "Gt/Lt are
  exclusive, so >1 means >=2, <17 means <=16". Setting `maxCpu: 16` would
  exclude 16-vCPU instances entirely.
- **Final apt upgrade ordering matters.** The runner Dockerfile runs
  `apt-get upgrade` AFTER installing packages, then a SECOND
  `apt-get update && apt-get upgrade -y` after installing the GitHub runner
  binary. The second pass picks up CVEs in dependencies pulled in by
  `installdependencies.sh` (e.g., CVE-2025-15467 OpenSSL stack overflow).
- **`buildx imagetools create` requires both arch tags pushed first.** The
  multi-arch manifest step in `docker-push-runner` runs after
  `make -j2 _build-runner-amd64 _build-runner-arm64`. If either arch fails,
  `--output-sync=target` ensures the failure is visible and the manifest
  step won't run.

## Sources [coverage: high]

- [/Users/shavakan/workspace/runs-fleet/Dockerfile](/Users/shavakan/workspace/runs-fleet/Dockerfile)
- [/Users/shavakan/workspace/runs-fleet/docker/runner/Dockerfile](/Users/shavakan/workspace/runs-fleet/docker/runner/Dockerfile)
- [/Users/shavakan/workspace/runs-fleet/docker/runner/entrypoint.sh](/Users/shavakan/workspace/runs-fleet/docker/runner/entrypoint.sh)
- [/Users/shavakan/workspace/runs-fleet/Makefile](/Users/shavakan/workspace/runs-fleet/Makefile)
- [/Users/shavakan/workspace/runs-fleet/flake.nix](/Users/shavakan/workspace/runs-fleet/flake.nix)
- [/Users/shavakan/workspace/runs-fleet/packer/provision-base.sh](/Users/shavakan/workspace/runs-fleet/packer/provision-base.sh)
- [/Users/shavakan/workspace/runs-fleet/packer/provision-runs-fleet.sh](/Users/shavakan/workspace/runs-fleet/packer/provision-runs-fleet.sh)
- [/Users/shavakan/workspace/runs-fleet/packer/runs-fleet-runner-arm64.pkr.hcl](/Users/shavakan/workspace/runs-fleet/packer/runs-fleet-runner-arm64.pkr.hcl)
- [/Users/shavakan/workspace/runs-fleet/deploy/helm/runs-fleet/Chart.yaml](/Users/shavakan/workspace/runs-fleet/deploy/helm/runs-fleet/Chart.yaml)
- [/Users/shavakan/workspace/runs-fleet/deploy/helm/runs-fleet/values.yaml](/Users/shavakan/workspace/runs-fleet/deploy/helm/runs-fleet/values.yaml)
