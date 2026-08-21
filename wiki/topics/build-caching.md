---
topic: Build Caching (transparent buildx layer cache)
last_compiled: 2026-08-21
sources_count: 19
---

# Build Caching (transparent buildx layer cache)

## Purpose [coverage: high -- 19 sources]

S3-backed Docker layer caching for `docker buildx build` with **zero workflow
changes** — no `cache-from`, no `cache-to`, no registry, no extra steps
([docs/USAGE.md](../../docs/USAGE.md) "Automatic Docker layer caching"). The
mechanism is a small Go binary, `runs-fleet-buildx-shim`, installed on the
runner AMI *in place of* the `docker-buildx` CLI plugin. Docker invokes it as
the plugin; it inspects argv, optionally appends cache flags, and then always
`syscall.Exec`s the real buildx plugin. Everything else about the build is
unchanged.

Two independent injections ride the same shim:

1. **`buildx build`** — append `--cache-from`/`--cache-to type=s3,...` pointing
   at the deployment's cache bucket under a `buildkit/<org>/<repo>/` prefix
   (PR #394).
2. **`buildx create`** — append `--buildkitd-config` so a newly-created builder
   picks up the baked BuildKit registry-mirror config, or, when the workflow
   brought its own config, rewrite the mirror address inside it. That half
   belongs to the Docker Hub pull-through path — see
   [registry-mirroring](registry-mirroring.md).

The consumers are the [agent-runtime](agent-runtime.md) (writes the env vars,
reads the outcome back), [observability](observability.md) (the
`RunnerBuildCacheInterception` metric), and
[infrastructure](infrastructure.md) (the AMI provisioner that installs the
shim and the buildx pin it depends on).

## Architecture [coverage: high -- 9 sources]

```
orchestrator (pkg/runner/manager.go)
  BuildkitCacheBucket/Region set  →  buildkit/<org>/<repo>/  prefix
        │  RunnerConfig (SSM/Vault)
        ▼
agent on the instance (pkg/agent/buildkitcache.go)
  appends to the runner .env:
    RUNS_FLEET_BUILDKIT_CACHE_BUCKET / _REGION / _PREFIX / _OUTCOME
        │  (job process env)
        ▼
docker buildx build  →  /usr/local/lib/docker/cli-plugins/docker-buildx
                        == runs-fleet-buildx-shim (cmd/buildx-shim)
        │
        ├─ IsCreate?  → DecideCreate / redirectUserConfig  (mirror config)
        ├─ Decide(argv, env, creds, loadState)             (cache flags)
        │     ├─ LoadBuildxState  (buildx store: current + instances/)
        │     └─ IMDSClient.FetchCredentials (only if otherwise eligible)
        ├─ WriteOutcome → <runner>/_rf_buildkit_cache_outcome
        └─ syscall.Exec(real docker-buildx, argv [+ extra])
        │
        ▼
agent at termination: ReadBuildCacheOutcome → telemetry
  BuildCacheInterception → RunnerBuildCacheInterception{Status}
```

Files, all pure-decision except where noted:

- [pkg/buildxshim/decide.go](../../pkg/buildxshim/decide.go) — the whole gate.
  `Decide` is deliberately I/O-free (`loadState` is a lazy callback) so the
  full passthrough matrix is unit-testable. Also holds the argv walk
  (`splitSubcommand`, `isBuild`, `hasCacheFlag`, `builderFromArgv`,
  `platformSlug`) and the cache-name derivation (`cacheName`,
  `repoFromPrefix`, `sanitizeSlug`).
- [pkg/buildxshim/create.go](../../pkg/buildxshim/create.go) — `IsCreate` /
  `DecideCreate` for the `buildx create` path; recognizes
  `--buildkitd-config`, `--buildkitd-config-inline`, and the deprecated
  `--config` / `--config-inline` aliases as "user brought their own".
- [pkg/buildxshim/builderstate.go](../../pkg/buildxshim/builderstate.go) —
  reads the buildx store (`BUILDX_CONFIG` > `DOCKER_CONFIG/buildx` >
  `$HOME/.docker/buildx`): the `current` file's builder name and
  `instances/<name>`'s `Driver`. Every failure yields zero values, which
  `Decide` reads as "no eligible builder".
- [pkg/buildxshim/imds.go](../../pkg/buildxshim/imds.go) — IMDSv2 handshake
  (PUT token → GET role name → GET role credentials) behind a `CredsFetcher`
  interface, bounded by a 2-second *total* timeout.
- [pkg/buildxshim/mirrorconfig.go](../../pkg/buildxshim/mirrorconfig.go) —
  `MirrorAddrFromConfig`, `RewriteMirrorHosts`, `UserConfigFlag`,
  `ReplaceFlagValue` for the user-config redirect.
- [pkg/buildxshim/outcome.go](../../pkg/buildxshim/outcome.go) —
  `WriteOutcome` appends one line to the agent-named outcome file; every error
  is swallowed.
- [pkg/buildxshim/plugin.go](../../pkg/buildxshim/plugin.go) —
  `DiscoverRealPlugin`: prefer the path recorded at provision time
  (`/opt/runs-fleet/buildx-real-path`), else the first existing entry of
  `DefaultPluginSearch`.
- [cmd/buildx-shim/main.go](../../cmd/buildx-shim/main.go) — the only I/O
  orchestration: `safePlan` wraps the whole decision path in a `recover()` so a
  shim panic degrades to unmodified argv, then `syscall.Exec`.
- [pkg/agent/buildkitcache.go](../../pkg/agent/buildkitcache.go) —
  `WriteBuildkitCacheEnv` (agent side, writes the four env vars) and
  `ReadBuildCacheOutcome` (reads the last non-empty line, keeps only the
  pre-colon prefix).

### Install path

The shim is built as a third binary in the runner container image
([docker/runner/Dockerfile:29-31](../../docker/runner/Dockerfile)) alongside
the agent and the mirror proxy, purely so the AMI provisioner can extract it —
it is never used inside the container. `provision-runs-fleet.sh` `docker cp`s
it out and installs it to
`/usr/local/lib/docker/cli-plugins/docker-buildx`, which **precedes** the OS
plugin dir in docker's search order, so the packaged buildx is shadowed rather
than replaced; the discovered real path is written to
`/opt/runs-fleet/buildx-real-path`
([packer/provision-runs-fleet.sh:53-82](../../packer/provision-runs-fleet.sh)).
`nix build .#buildx-shim-{amd64,arm64}` builds the same binary for dev
([flake.nix:116-141](../../flake.nix)).

## Talks To [coverage: high -- 8 sources]

- **The real `docker-buildx` plugin** — always exec'd, with the original argv
  plus at most the appended flags. Discovery order:
  `RUNS_FLEET_BUILDKIT_REAL_PLUGIN` env → the recorded-path file →
  `DefaultPluginSearch` (`/usr/libexec/...`, `/usr/lib/...`,
  `/usr/local/libexec/...`, `/root/.docker/cli-plugins/...`). Nothing found
  still execs `DefaultPluginSearch[0]` so the error text is docker's own.
- **EC2 IMDSv2** at `169.254.169.254` — instance-profile session credentials,
  embedded inline in the S3 cache attribute string so the buildkit container
  never needs its own IMDS access (hop-limit 1 is fine: the shim runs on the
  host).
- **S3** — BuildKit's own `type=s3` cache backend does the reads/writes; the
  shim only hands it bucket/region/prefix/name plus credentials.
- **The buildx store on disk** — `current` and `instances/*` JSON, read-only.
- **The orchestrator** via `RunnerConfig`:
  [pkg/runner/manager.go:124-130](../../pkg/runner/manager.go) sets
  `BuildkitCacheBucket`/`Region` and derives
  `buildkit/<org>/<repo>/`; [cmd/server/main.go:361-362](../../cmd/server/main.go)
  wires both from `cfg.CacheBucketName` / `cfg.AWSRegion` — the same bucket as
  the Actions cache, under a different prefix. No dedicated env var exists.
- **The agent** — writes the env vars into the runner `.env`, then reads the
  outcome file back at termination and ships it as
  `BuildCacheInterception` in the telemetry message
  ([pkg/agent/telemetry.go:80-84](../../pkg/agent/telemetry.go)).
- **`pkg/termination` → `pkg/metrics`** —
  [pkg/termination/handler.go:684-685](../../pkg/termination/handler.go)
  publishes `RunnerBuildCacheInterception{Status}`.
- **[registry-mirroring](registry-mirroring.md)** — the `buildx create` path
  and the baked `/opt/runs-fleet/buildkitd.toml`.

## API Surface [coverage: high -- 6 sources]

### Exported decision surface (`pkg/buildxshim`)

| Symbol | Role |
| --- | --- |
| `Decide(argv, env, creds, loadState) ([]string, string)` | The build-path gate. Returns flags to append (nil = passthrough) plus an outcome string. I/O-free. |
| `IsCreate(argv) bool` / `DecideCreate(argv, builderConfigPath)` | The create-path gate; `builderConfigPath` is resolved by the caller (one `os.Stat`). |
| `LoadBuildxState(env) BuildxState` | Best-effort read of the buildx store. |
| `Credentials{AccessKeyID, SecretAccessKey, SessionToken}` | All three must be non-empty (`complete()`), else injection is suppressed. |
| `CredsFetcher` / `NewIMDSClient()` | Credential seam; the fake is how the passthrough matrix is tested. |
| `DiscoverRealPlugin(recordFile, searchList) string` | Real-plugin resolution. |
| `WriteOutcome(path, outcome)` | Append-only telemetry write, never fatal. |
| `MirrorAddrFromConfig`, `RewriteMirrorHosts`, `UserConfigFlag`, `ReplaceFlagValue` | User-config redirect helpers. |
| `OutcomeNeedsCreds`, `OutcomeSkippedUserConfig`, `OutcomeEngagedUserConfig` | The three outcomes `cmd/buildx-shim` branches on. |

### Env vars

| Variable | Set by | Meaning |
| --- | --- | --- |
| `RUNS_FLEET_BUILDKIT_CACHE_BUCKET` | agent (from RunnerConfig) | S3 bucket; **all three of bucket/region/prefix must be present or the outcome is `disabled`** |
| `RUNS_FLEET_BUILDKIT_CACHE_REGION` | agent | S3 region |
| `RUNS_FLEET_BUILDKIT_CACHE_PREFIX` | agent | `buildkit/<org>/<repo>/` |
| `RUNS_FLEET_BUILDKIT_CACHE_OUTCOME` | agent | path of the outcome file the shim appends to |
| `RUNS_FLEET_BUILD_CACHE=off` | **workflow author** | per-job/step opt-out (case-insensitive `off`) |
| `RUNS_FLEET_BUILDKIT_REAL_PLUGIN` | escape hatch | overrides the recorded-path file |
| `RUNS_FLEET_BUILDKIT_BUILDER_CONFIG` | escape hatch | overrides `/opt/runs-fleet/buildkitd.toml` |

### Injection conditions (all must hold)

From the `Decide` doc comment and body
([pkg/buildxshim/decide.go:61-127](../../pkg/buildxshim/decide.go)):

1. argv is a buildx `build` (or its `b` alias) invocation — not
   `docker-cli-plugin-metadata`, not another subcommand.
2. bucket + region + prefix all present.
3. no `--cache-from`/`--cache-to` already in argv, in either the
   separate-arg or `=`-inline form. Explicit user config always wins.
4. `RUNS_FLEET_BUILD_CACHE` is not `off`.
5. The effective builder — resolved with buildx's own precedence, `--builder`
   argv > `BUILDX_BUILDER` env > the store's `current` file — is a
   *user-created instance* whose driver is `docker-container`, `kubernetes`,
   or `remote`. The `default` name and any name with no instance record are
   rejected.
6. Complete instance-profile session credentials.

### Outcomes

`engaged`, `skipped:<reason>`, `failed:<reason>`, `disabled`. Reasons seen in
source: `skipped:not-build`, `skipped:opt-out`, `skipped:user-cache-flags`,
`skipped:no-cache-builder`, `skipped:no-builder-config`,
`skipped:user-config`, `skipped:driver`, `failed:no-creds` (internal signal
only — `cmd/buildx-shim` turns it into an IMDS fetch), `failed:imds`,
`engaged:create`, `engaged:user-config-redirect`. `ReadBuildCacheOutcome`
truncates at the first colon, so the metric stays four-valued
([docs/METRICS.md:200-212](../../docs/METRICS.md)).

## Data [coverage: medium -- 4 sources]

### The injected cache attribute strings

Both flags share a base of `type=s3,region=…,bucket=…,prefix=…,name=…` plus
the three inline credential fields. `--cache-to` adds `mode=max` and
`ignore-error=true`; the latter demotes export failures (missing IAM grant, S3
outage) to warnings so an injected build can never fail on a cache write
([pkg/buildxshim/decide.go:119-121](../../pkg/buildxshim/decide.go)).

### S3 layout

Prefix `buildkit/<org>/<repo>/` inside the deployment's Actions-cache bucket
(`RUNS_FLEET_CACHE_BUCKET`). The manifest `name` is
`<sanitized-repo>-<platform-slug>`, platform taken from the first `--platform`
value with slashes→dashes, else the shim's own `GOOS-GOARCH`. Parallel
amd64/arm64 jobs of one repo therefore keep separate manifests; blobs are
content-addressed and shared regardless
([pkg/buildxshim/decide.go:295-305](../../pkg/buildxshim/decide.go)).
Scoping is conventional (shared bucket IAM), not cryptographically enforced
like the Actions cache — appropriate for a same-org fleet
([docs/USAGE.md](../../docs/USAGE.md)).

### On-instance files

| Path | Written by | Read by |
| --- | --- | --- |
| `/usr/local/lib/docker/cli-plugins/docker-buildx` | AMI provisioner | docker (as the plugin) |
| `/opt/runs-fleet/buildx-real-path` | AMI provisioner | `DiscoverRealPlugin` |
| `<runner>/_rf_buildkit_cache_outcome` | shim (append) | agent at termination |
| `/opt/runs-fleet/buildkitd.toml` | mirror proxy at boot | `DecideCreate` (presence = activation) |
| `/tmp/runs-fleet-buildkitd-*.toml` | shim (redirect path) | buildx, after the exec |

## Key Decisions [coverage: high -- 7 sources]

- **A CLI-plugin shim, not a wrapper script or a workflow action.** Docker
  resolves `docker buildx` (and `docker build`, which aliases to it) through
  the cli-plugins search path, so shadowing that one file intercepts every
  invocation — including the ones inside `docker/build-push-action` — with no
  workflow, image, or action change. `/usr/local/lib/docker/cli-plugins`
  precedes the OS plugin dir, so nothing is overwritten and removing the file
  fully reverts the feature.
- **Never break a build, at three layers.** (a) The shim *always* execs the
  real plugin; the only variable is whether flags were appended. (b)
  `safePlan` wraps the entire decision path in `recover()`, so a shim panic
  passes the original argv through. (c) `ignore-error=true` on `--cache-to`
  keeps a cache-export failure a warning. Telemetry writes swallow every
  error for the same reason.
- **Under-inject on ambiguity.** `splitSubcommand` walks only docker's stable
  global value/bool flags plus buildx's root `--builder`; an unrecognized dash
  token makes the subcommand position ambiguous (it might consume the next
  token), so parsing returns "not a build" rather than guessing.
  `IsCreate`/`DecideCreate` follow the same rule.
- **The builder's *driver* is checked, never just its name.** The default
  docker driver cannot export a cache, so injecting `--cache-to` there would
  *fail* the build. `cacheCapableBuilder` requires an instance record in the
  buildx store with a `docker-container`/`kubernetes`/`remote` driver;
  context-backed builders have no such record and correctly fail the lookup.
  This is also why the documented usage pairs the feature with
  `docker/setup-buildx-action` — that step is what provides a container
  builder.
- **The `current` file is distrusted across endpoints/contexts.**
  `loadCurrent` returns "" when `DOCKER_CONTEXT` names a non-default context,
  when `config.json`'s `currentContext` does, or when the record's `Key` is
  not the effective `DOCKER_HOST` — buildx itself ignores the file in those
  cases, and trusting it could inject into a default-driver build.
- **Credentials are fetched only when they can matter, and inlined.** `plan`
  calls `Decide` once with empty creds; only the exact
  `failed:no-creds` sentinel triggers the IMDS round trip, so the metadata
  handshake and every non-build invocation never touch IMDS (and `Decide`
  only touches the filesystem past its cheap gates). Credentials are then
  embedded in the attribute string rather than granting the buildkit
  container IMDS access.
- **The shim lives in the runner *AMI* layer, the pin it needs lives in
  *base*.** Per [packer/README.md](../../packer/README.md) the shim is a
  runs-fleet orchestration artifact, so it goes in `provision-runs-fleet.sh`;
  the buildx binary it execs is a CI-workload package and stays in
  `provision-base.sh`.
- **Outcome telemetry is a file, not a call.** The shim is a short-lived
  exec'd process with no AWS client and no orchestrator credentials; appending
  a line to a path the agent named is the cheapest join. The agent takes the
  *last* non-empty line, so the reported outcome is the job's most recent
  build.

## Gotchas [coverage: high -- 6 sources]

- **The AMI's buildx had to be pinned before any of this worked.** AL2023's
  docker package bundles buildx 0.12.1, which only speaks the GitHub Actions
  cache **v1** protocol, decommissioned 2025-04-15 — so any job using
  `--cache-to type=gha` failed on a dead endpoint. Cache service v2 needs
  buildx ≥ 0.21. `provision-base.sh` now downloads a SHA-256-verified
  buildx **0.35.0** to `/usr/libexec/docker/cli-plugins/docker-buildx` *and
  deletes the distro copy from every higher-precedence system dir* so the
  stale 0.12.1 can never shadow it, then asserts `docker buildx version`
  resolves to the pin
  ([packer/provision-base.sh:150-182](../../packer/provision-base.sh)).
  That first packaged location is also the first entry of the shim's
  `DefaultPluginSearch`, which is not a coincidence.
- **The container image and the AMI pin the same buildx, enforced in CI.**
  `docker/runner/Dockerfile`'s `BUILDX_VERSION`/`BUILDX_SHA256_*` ARGs and
  `provision-base.sh`'s `BUILDX_VERSION`/`BUILDX_SHA256` must match;
  [.github/scripts/check-pin-sync.sh](../../.github/scripts/check-pin-sync.sh)
  fails the `pin-sync` CI job otherwise. Bumping one and not the other is a
  red build, not a silent drift.
- **`docker compose build` is not covered.** It does not route through the
  buildx CLI plugin, so nothing intercepts it
  ([docs/USAGE.md](../../docs/USAGE.md)).
- **`disabled` conflates two very different states.** No cache bucket
  configured for the deployment, *and* "the shim never ran because the job
  contained no docker build", both report `disabled` — `ReadBuildCacheOutcome`
  returns `BuildCacheDisabled` for an empty path, a missing file, and an empty
  file alike. A fleet-wide misconfiguration and a fleet of Go-only jobs look
  identical on the metric.
- **Effectiveness depends on an IAM grant the chart cannot give.** The
  instance profile needs S3 read/write on `buildkit/*`; until then builds
  succeed with cache-miss warnings and the outcome still reads `engaged`
  ([docs/CONFIGURATION.md:235-238](../../docs/CONFIGURATION.md), and see
  `AGENTS.md`'s "Cache & Admin" note). `engaged` therefore means *injected*,
  not *hit*.
- **The redirect temp file is deliberately never cleaned up.** `writeTempConfig`
  leaves `/tmp/runs-fleet-buildkitd-*.toml` behind because buildx reads it
  only *after* this process has exec'd away; the instance is ephemeral
  ([cmd/buildx-shim/main.go:151-164](../../cmd/buildx-shim/main.go)).
- **A `--cache-to` failure is invisible in the exit code by design.** With
  `ignore-error=true`, a missing grant, a bucket-policy denial, or an S3
  outage produces warnings in the build log and a green job. The alarm is the
  metric, not the build: sustained `failed` means the IMDS/creds path broke
  ([docs/METRICS.md:212-220](../../docs/METRICS.md)).
- **A workflow that sets its own cache flags gets nothing from this feature,
  silently and correctly.** `skipped:user-cache-flags` is benign, but it means
  a repo that migrated to `type=gha` earlier is still on GitHub's cache rather
  than S3 until those flags are removed.

## Sources [coverage: high]

- [pkg/buildxshim/decide.go](../../pkg/buildxshim/decide.go)
- [pkg/buildxshim/create.go](../../pkg/buildxshim/create.go)
- [pkg/buildxshim/builderstate.go](../../pkg/buildxshim/builderstate.go)
- [pkg/buildxshim/imds.go](../../pkg/buildxshim/imds.go)
- [pkg/buildxshim/mirrorconfig.go](../../pkg/buildxshim/mirrorconfig.go)
- [pkg/buildxshim/outcome.go](../../pkg/buildxshim/outcome.go)
- [pkg/buildxshim/plugin.go](../../pkg/buildxshim/plugin.go)
- [cmd/buildx-shim/main.go](../../cmd/buildx-shim/main.go)
- [pkg/agent/buildkitcache.go](../../pkg/agent/buildkitcache.go)
- [pkg/runner/manager.go](../../pkg/runner/manager.go)
- [packer/provision-runs-fleet.sh](../../packer/provision-runs-fleet.sh)
- [packer/provision-base.sh](../../packer/provision-base.sh)
- [docker/runner/Dockerfile](../../docker/runner/Dockerfile)
- [flake.nix](../../flake.nix)
- [.github/scripts/check-pin-sync.sh](../../.github/scripts/check-pin-sync.sh)
- [docs/USAGE.md](../../docs/USAGE.md)
- [docs/METRICS.md](../../docs/METRICS.md)
- [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md)
- [pkg/termination/handler.go](../../pkg/termination/handler.go)
