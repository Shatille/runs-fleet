# Packer AMIs

Two-layer AMI build:

| Layer | Template | Provisioner | What it contains |
|---|---|---|---|
| **Base** | `runner-base-{amd64,arm64}.pkr.hcl` | `provision-base.sh` | OS + every package that's stable across runner-image revs: docker, git/git-lfs, node, vault, yq, buildx, gh, runner OS deps, dev tools, language toolchains, the actions/runner binary, CloudWatch agent. |
| **Runner** | `runs-fleet-runner-{amd64,arm64}.pkr.hcl` | `provision-runs-fleet.sh` | The diff over base: the `runs-fleet-agent` binary extracted from the ECR runner image, its systemd unit, the bootstrap shim, the per-boot cloud-init script, and the runs-fleet-specific CloudWatch override. |

Both layers are built by a single workflow at `.github/workflows/build-amis.yml`. The runner-AMI job declares `needs: build-base`, so when a single push touches both `provision-base.sh` and `provision-runs-fleet.sh`, the runner AMI waits for the new base to register before starting Packer (its `source_ami_filter` then resolves to the fresh base). When a push only touches one layer, the other layer's job is skipped.

## Where does a new package go?

**Default: base AMI.** Anything that doesn't change with every agent-binary revision belongs there. Concrete signals:

- It's an OS package, a language toolchain, or a build tool → `provision-base.sh`.
- Its version is pinned and bumped on its own cadence (sbt, Node, Vault, gh) → `provision-base.sh`.
- It's a runtime dependency of `actions/runner` itself → `provision-base.sh`.
- It needs to be present before the runner agent starts a job → `provision-base.sh`.

**Runner AMI only when:** the thing is genuinely part of the `runs-fleet` orchestration layer and not a CI-workload concern. The current list is small and exhaustive:

- Extracting the `runs-fleet-agent` binary from the ECR runner image (this is the project's actual artifact).
- Installing the `runs-fleet-agent.service` systemd unit.
- Installing `agent-bootstrap.sh` and the per-boot cloud-init script.
- The CloudWatch agent JSON override that points at `/opt/actions-runner/_diag/*.log`.

If you're tempted to add anything else to `provision-runs-fleet.sh`, that's a signal it probably belongs in base instead.

## Adding to the base AMI

1. Edit `packer/provision-base.sh`. Add a new `echo "==> ..."` block, follow the existing pattern (explicit error handling for downloads + checksum verify for tarballs).
2. If the package adds a binary that downstream consumers should be able to inspect, append a line to the trailing summary (`echo "    - your-tool: $(your-tool --version)"`).
3. Open a PR. Merging to `main` triggers the unified `Build AMIs` workflow (`.github/workflows/build-amis.yml`); the `build-base` job runs, and `build-runner-ami` waits for it. The base build also runs weekly (Sunday 06:00 UTC) and on workflow_dispatch.

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
