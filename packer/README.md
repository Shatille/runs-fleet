# Packer AMIs

Two-layer AMI build:

| Layer | Template | Provisioner | What it contains |
|---|---|---|---|
| **Base** | `runner-base-{amd64,arm64}.pkr.hcl` | `provision-base.sh` | OS + every package that's stable across runner-image revs: docker, git/git-lfs, node, vault, yq, buildx, gh, runner OS deps, dev tools, language toolchains, the actions/runner binary, CloudWatch agent. |
| **Runner** | `runs-fleet-runner-{amd64,arm64}.pkr.hcl` | `provision-runs-fleet.sh` | The diff over base: the `runs-fleet-agent` binary extracted from the ECR runner image, its systemd unit, the bootstrap shim, the per-boot cloud-init script, and the runs-fleet-specific CloudWatch override. |

The runner AMI uses `source_ami_filter` to pick the most recent `runner-base-{arch}-*` AMI in the account at build time, so changes to the base only take effect after `Build Runner Base AMI` runs.

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
3. Open a PR. Merging to `main` triggers `Build Runner Base AMI` automatically (`.github/workflows/build-base-ami.yml`).
4. **Wait for the base AMI build to complete before relying on the new package.** Until the next `runner-base-*-{timestamp}` AMI is registered, `Build Runner AMI` will still pick up the old base via `source_ami_filter`. The base build also runs weekly (Sunday 06:00 UTC) and on workflow_dispatch.

## Verifying

`docker/runner/CLAUDE.md` describes the container-image security workflow. The AMI side runs a Trivy scan on the provisioned filesystem before the snapshot is taken (`packer/provision-trivy-scan.sh`); HIGH/CRITICAL unfixed findings fail the build. If your new package introduces a finding, suppress it in `.trivy/vex.json` with a documented justification rather than `.trivyignore`.

## Downstream extension point

`packer/provision-base-hook.sh` is an empty stub in upstream. Downstream forks that need to layer in account-specific tooling without modifying upstream can rewrite it (e.g. from a CI secret) — the base build uploads it unconditionally and executes it just before cleanup if non-empty. **Prefer upstreaming new packages over relying on this hook.** The hook is for things that genuinely cannot live in upstream (account-specific credentials, internal-mirror configs).
