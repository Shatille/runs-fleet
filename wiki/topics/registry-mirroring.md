---
topic: Registry Mirroring (Docker Hub via ECR Pull-Through Cache)
last_compiled: 2026-08-21
sources_count: 14
---

# Registry Mirroring (Docker Hub via ECR Pull-Through Cache)

## Purpose [coverage: high -- 14 sources]

Docker Hub rate-limits anonymous pulls per source IP, and a fleet of ephemeral
runners behind shared NAT egress hits that limit — surfacing inside workflows as
`429 Too Many Requests` naming `registry-1.docker.io`. The GitHub-hosted ARC
runners in the same account already rode an ECR pull-through cache; runs-fleet
never did, so its runners pulled from Hub directly on every job.

This topic covers the machinery that fixes that: a small **host-local mirror
proxy** (`cmd/mirror-proxy` + `pkg/mirrorproxy`) baked into the runner AMI, which
presents itself to dockerd and BuildKit as an ordinary registry mirror and
translates each request onto an [ECR pull-through
cache](https://docs.aws.amazon.com/AmazonECR/latest/userguide/pull-through-cache.html)
endpoint, injecting instance-role credentials per request.

The whole feature is opt-in behind one Packer variable
(`ecr_pull_through_endpoint`, fed from the `ECR_PULL_THROUGH_ENDPOINT` secret or
variable). Unset, the proxy and its systemd unit are still baked but inert, and
runtime behaviour is byte-identical to an unconfigured build.

**Why a local proxy at all** — none of the obvious direct wirings work:

- dockerd sends **no credentials** to a `registry-mirrors` entry, and ECR
  refuses anonymous pulls.
- ECR namespaces cached images **under the pull-through rule prefix**
  (`<prefix>/library/mysql`), and neither dockerd's `registry-mirrors` nor
  BuildKit's mirror config can attach a path prefix to a mirror host.
- dockerd's `registry-mirrors` applies **only to Docker Hub** (all Hub images,
  official and namespaced alike — see Gotchas for the misconception this
  corrects), so it cannot cover `quay.io` or `registry.k8s.io` rules at all.

Something local has to translate. The proxy is that translator, and it leaves no
credential material on disk anywhere — no `docker login`, no config.json.

## Architecture [coverage: high -- 14 sources]

### Two components

`pkg/mirrorproxy` is the reusable library: the HTTP handler, the ECR token
source, and pull-through rule discovery. `cmd/mirror-proxy` is the on-host
binary: region resolution, listener binding, and BuildKit config generation.

### The handler ([pkg/mirrorproxy/proxy.go](../../pkg/mirrorproxy/proxy.go))

`New(endpoint string, tokens TokenSource) (*Handler, error)` parses an endpoint
of the form `https://<registry-host>/<rule-prefix>`. Both halves are required —
the path *is* the namespace every repository path is rewritten under
(proxy.go:52). It seeds `namespaces = {"docker.io": prefix}`.

`ServeHTTP` (proxy.go:71) is deliberately minimal:

1. **GET/HEAD only.** Anything else is `405 mirror is pull-only`.
2. `rewrite` maps the request path; a refusal is a `404`.
3. Fetch a token from the `TokenSource`; failure is a **`502`**, which is the
   designed degradation — dockerd/BuildKit answer a 5xx mirror by pulling from
   the real registry.
4. Build an upstream request, copy headers minus hop-by-hop, and **replace**
   `Authorization` with `Basic <token>`.
5. Relay the upstream response status, headers, and body verbatim.

`rewrite` (proxy.go:129) does four things:

- **Refuses any `..` path segment.** The port is reachable by any local process
  (including job code) and the ECR token is registry-wide, so a surviving `..`
  would escape the namespace.
- Reads the `ns` query parameter — the registry name BuildKit's containerd
  resolver names when mirroring. Absent means `docker.io`; an **unknown `ns` is
  a 404** with no upstream call, so the client falls back to the real registry.
- **Strips `ns`** before forwarding (ECR is not itself a proxy) while preserving
  every other query parameter.
- Maps `/v2/<name>/...` → `/v2/<prefix>/<name>/...`, and the bare `/v2` or
  `/v2/` ping to the registry root `/v2/` (not the namespace), so the mirror's
  API-version probe succeeds.

Two client-side details matter. The HTTP client sets
`CheckRedirect: http.ErrUseLastResponse` so blob `307`s are **relayed to the
caller rather than followed** — image bytes stream from object storage straight
to dockerd instead of through the proxy (proxy.go:62). And `Authorization` is
listed in `hopByHopHeaders` alongside the RFC 9110 set (proxy.go:163): the
client's credential is for the mirror, not ECR, and is replaced wholesale.

### Credentials ([pkg/mirrorproxy/token.go](../../pkg/mirrorproxy/token.go))

`NewECRTokenSource(client ecrAPI) TokenSource` exchanges the ambient AWS
credentials (the instance role, in production) for `ecr:GetAuthorizationToken`
output. That output is **already basic-auth formatted**, so it is sent as
`Basic <token>` verbatim. A `cachedTokenSource` holds it under a mutex until
`tokenExpiryMargin = 5m` before expiry, so a token is never handed out with
seconds of validity left mid-pull. A missing `ExpiresAt` defaults to one hour.
A fetch error is **not** cached.

### Rule discovery ([pkg/mirrorproxy/discover.go](../../pkg/mirrorproxy/discover.go))

`DiscoverRules` pages `ecr:DescribePullThroughCacheRules` and builds an
upstream-host → rule-prefix table. Two normalizations:

- `registry-1.docker.io` (what ECR records as the Docker Hub upstream) is
  normalized to `docker.io` (what the `ns` parameter carries).
- Duplicate rules for one upstream dedup **deterministically** — shortest
  prefix, then lexicographic (`prefixWins`, discover.go:47) — so the handler's
  routing table and the generated `buildkitd.toml` can never disagree.

`Handler.AddRules` merges the discovered table but lets the endpoint's explicit
`docker.io` declaration win on conflict (proxy.go:116).

### The binary ([cmd/mirror-proxy/main.go](../../cmd/mirror-proxy/main.go))

Startup order, all fatal-on-failure:

1. `ECR_PULL_THROUGH_ENDPOINT` must be set (from
   `/opt/runs-fleet/mirror-env`).
2. `config.LoadDefaultConfig`, then `resolveRegion` (main.go:45) — see below.
3. `mirrorproxy.New` + `NewECRTokenSource`.
4. `DiscoverRules`; **a discovery failure exits** rather than serving a mirror
   that would 502 every pull (main.go:291).
5. `parseListenAddrs` splits the comma-separated `-listen` flag, dedups (a
   repeat would make the second bind fail as "address already in use"), and
   rejects empty entries.
6. `listenAll` binds **every required address or none** (main.go:151): a
   half-bound set must not look healthy, because it would answer one namespace
   and silently shed the rest to Docker Hub.
7. `addBridge` adds the best-effort bridge listener and reconciles the BuildKit
   config against whatever actually bound.
8. One `server.Serve` goroutine per listener. A **required** listener failing
   sends to the `failed` channel and takes the process down so systemd
   restarts it; the **bridge** listener failing only logs, because a bridge
   problem must not also take away the loopback mirror dockerd is using.

#### Region resolution (PR #444)

`resolveRegion(ctx, cfgRegion, fromIMDS)` returns the ambient config's region
when it has one, else queries IMDS via `imds.NewFromConfig(...).GetRegion`. A
systemd unit starts with a near-empty environment, so `AWS_REGION` is usually
absent, and the SDK's own IMDS region lookup sits in the default resolver chain
but **only fires for a config that opts into it** — which this binary did not.
Without a region every ECR call fails and the mirror can only answer 502, so an
unresolvable region is now a **hard exit**. The systemd unit also sets
`Environment=AWS_REGION=${REGION}` (baked from the Packer build's region), so
IMDS is the second line of defence rather than the primary path.

#### Two listen addresses (PR #453)

The proxy's two clients sit in **different network namespaces**:

| Client | Namespace | Reaches the mirror at |
| ------ | --------- | --------------------- |
| dockerd | the host's | `127.0.0.1:8989` (loopback) |
| buildkitd (buildx `docker-container` driver) | a bridge-network container | the **docker bridge gateway**, e.g. `172.17.0.1:8989` |

buildkitd's `127.0.0.1` is the container's own, and the host's primary ENI is
not bound — so before #453 BuildKit could reach the mirror at neither address.
Binding the primary ENI instead would put a credential-injecting proxy on the
VPC; the bridge gateway is routable from every container on the host and from
nowhere off it.

`DefaultListen = "127.0.0.1:8989"` is required (main.go:73);
`DefaultBridgeInterface = "docker0"` is best-effort (main.go:79). The gateway is
**discovered, never assumed**: `discoverBridgeAddr` (main.go:122) takes the
first IPv4 address of the named interface, because dockerd picks the bridge
subnet from an address pool and steps around collisions with the host's own
networks — `172.17.0.1` is its usual first choice, not a guarantee. The bridge
listener's **port comes from `-listen`** (`portOf`, main.go:111) rather than a
second flag, so the two cannot drift.

Pinning the gateway with dockerd's `bip` was rejected: it would trade a lookup
for a dockerd that refuses to start when the host network overlaps the pin, and
`runs-fleet-agent.service` has `Requires=docker.service`, so that would take the
whole runner down rather than just the mirror.

#### BuildKit configuration ([cmd/mirror-proxy/config.go](../../cmd/mirror-proxy/config.go))

BuildKit with the `docker-container` driver resolves registries itself and
**does not read `/etc/docker/daemon.json`** — this is the whole reason PR #453
exists. It needs its own `buildkitd.toml`:

```toml
[registry."docker.io"]
  mirrors = ["172.17.0.1:8989"]
[registry."quay.io"]
  mirrors = ["172.17.0.1:8989"]
[registry."172.17.0.1:8989"]
  http = true
```

`buildkitdConfig(mirrorAddr, rules)` emits one `mirrors` block **per discovered
upstream**, in sorted order so the file is byte-stable across restarts, then a
trailing `http = true` block for the mirror itself (the mirror speaks plaintext;
without it BuildKit would try HTTPS).

The file is **not baked into the AMI** — it names the bridge gateway, an address
dockerd only picks at runtime, and a second independent lookup could resolve a
different one. `reconcileBuildkitdConfig` (main.go:209) writes it from the
address `bindBridge` actually bound, and **removes it when nothing bound**: a
config naming an unbound address reads as a working mirror and silently sheds
builds to Hub, while absence makes the buildx shim skip cleanly. Writes go
through `writeBuildkitdConfig`, a create-temp-chmod-rename so a reader never
observes a half-written config. The empty-address return from `bindBridge` is
therefore load-bearing, not just a sentinel.

Reconciliation happens **before serving starts**, so the readiness probe on the
listening port also implies the config is in place for `buildx-setup.service`
and the buildx shim.

### AMI integration ([packer/provision-runs-fleet.sh](../../packer/provision-runs-fleet.sh))

Everything lives in the **runner** layer, not the base layer:

- The binary is built into the runner container image
  ([docker/runner/Dockerfile](../../docker/runner/Dockerfile):38, :154) and
  `docker cp`'d out during provisioning alongside the agent and the buildx
  shim, then installed root-owned to `/usr/local/sbin/runs-fleet-mirror-proxy`
  (root-owned so workflow code, running as `ec2-user`, cannot swap it).
- `MIRROR_PORT=8989` is defined **once** in the provisioner and interpolated
  into the unit, the readiness poll, and `daemon.json`.
- `runs-fleet-mirror-proxy.service` is baked **unconditionally** but gated on
  `ConditionPathExists=/opt/runs-fleet/mirror-env`, which only the opt-in
  writes. It has `After=`/`Requires=docker.service` (dockerd owns the bridge),
  `Before=binfmt-qemu.service buildx-setup.service runs-fleet-agent.service`,
  `Restart=always`, `RestartSec=2`, and **`StartLimitIntervalSec=0`** — the
  proxy exits rather than serve without its required address, so the default
  burst limit of 5-in-10s would latch the unit failed and cost the runner the
  mirror for its whole life.
- `/usr/local/sbin/runs-fleet-mirror-ready` polls `/dev/tcp/127.0.0.1/8989`
  for up to 30s. `Type=simple` marks the unit started the instant the process
  forks, but the proxy resolves a region and calls ECR before it binds — so
  without this poll the `Before=` ordering would not actually protect the boot
  units that pull images. It runs as `ExecStartPost=-` (leading dash): an
  ordering barrier, not a health check, so a proxy that is merely slow
  (throttled ECR discovery on a fleet-wide boot) binds late rather than never.
- The opt-in writes exactly two files: `/opt/runs-fleet/mirror-env` (the
  endpoint) and `/etc/docker/daemon.json` with
  `registry-mirrors: ["http://127.0.0.1:8989"]` +
  `insecure-registries: ["127.0.0.1:8989"]`. The endpoint format is
  **validated at bake time** (`https://*/*`) so a malformed value fails the
  build rather than the boot.

The base layer ([packer/provision-base.sh](../../packer/provision-base.sh))
contributes only the consumer side: `buildx-setup.service` attaches
`/opt/runs-fleet/buildkitd.toml` to the baked `multiarch` builder when the file
exists, resolved at boot via backticks (systemd `$`-expands `ExecStart` but
leaves backticks for the shell) — provision-base.sh:256. It also pins buildx to
0.35.0, which matters because AL2023's packaged 0.12.1 predates the
`--buildkitd-config` flag name.

### Coverage map

| Pull path | Covered by | Notes |
| --------- | ---------- | ----- |
| Job `docker pull` / `docker build` (Hub) | `daemon.json` `registry-mirrors` | official and namespaced Hub images alike |
| Service containers (Hub) | `daemon.json` | |
| `binfmt-qemu` / `buildkit` boot-unit pulls | `daemon.json` + the unit's `Before=` + readiness poll | |
| `docker buildx build` (`docker-container` driver), any upstream with a rule | `buildkitd.toml` (bridge gateway) | includes `FROM quay.io/...` |
| `docker/setup-buildx-action`'s fresh builders | the buildx shim injecting `--buildkitd-config` | see [infrastructure](infrastructure.md) |
| Job `docker pull quay.io/...` | **nothing** | `registry-mirrors` is Hub-only by design |
| `mcr.microsoft.com`, `ghcr.io` (no rule) | **nothing** | unknown `ns` → 404 → client falls back |
| Base-layer bake-time pre-bake pulls | **nothing** | the proxy arrives in the runner layer |

## Talks To [coverage: high -- 14 sources]

| Component | Direction | Interface |
| --------- | --------- | --------- |
| dockerd | in | HTTP `GET/HEAD /v2/...` on `127.0.0.1:8989`, via `registry-mirrors` |
| buildkitd (in a bridge container) | in | HTTP `GET/HEAD /v2/...?ns=<registry>` on `<bridge-gw>:8989`, via `buildkitd.toml` |
| ECR registry | out | `GET /v2/<prefix>/<name>/...` with `Authorization: Basic` |
| ECR API | out | `ecr:GetAuthorizationToken` (cached ~12h), `ecr:DescribePullThroughCacheRules` (once at startup) |
| IMDS | out | `GetRegion`, only when the environment carries no region |
| Docker Hub / upstream registries | out (indirect) | the fallback path every failure degrades to — the client's, not the proxy's |
| `buildx-setup.service` | out (file) | `/opt/runs-fleet/buildkitd.toml` |
| runs-fleet buildx shim | out (file) | same file; injects or redirects `--buildkitd-config` |
| systemd | both | `ConditionPathExists`, `Before=`, `ExecStartPost` readiness poll |

The instance role needs `ecr:GetAuthorizationToken`,
`ecr:DescribePullThroughCacheRules`, plus `ecr:BatchGetImage`,
`ecr:GetDownloadUrlForLayer`, `ecr:BatchImportUpstreamImage`, and
`ecr:CreateRepository` on the cache-prefix repositories so a first-ever pull can
populate the cache (packer/README.md). The real policy lives outside this
repository — see [infrastructure](infrastructure.md).

## API Surface [coverage: high -- 14 sources]

### `pkg/mirrorproxy`

- `New(endpoint string, tokens TokenSource) (*Handler, error)` — endpoint must
  be `http(s)://<host>/<prefix>`; both host and non-empty path are required
- `(*Handler).ServeHTTP(w, r)` — `http.Handler`; GET/HEAD under `/v2/` only
- `(*Handler).AddRules(rules map[string]string)` — merge discovered ns→prefix
  mappings; existing entries (the endpoint seed) win
- `TokenSource` interface — `Token(ctx) (string, error)`, returning a value
  sent verbatim as `Basic <token>`
- `NewECRTokenSource(client ecrAPI) TokenSource` — instance-role exchange,
  cached to 5 minutes before expiry
- `DiscoverRules(ctx, client ecrAPI) (map[string]string, error)` — paged
  upstream-host → rule-prefix table, `registry-1.docker.io` normalized to
  `docker.io`
- `ecrAPI` (unexported) — `GetAuthorizationToken` + `DescribePullThroughCacheRules`

### `cmd/mirror-proxy`

Flags:

| Flag | Default | Meaning |
| ---- | ------- | ------- |
| `-listen` | `127.0.0.1:8989` (`DefaultListen`) | comma-separated addresses that **must** all bind |
| `-bridge-interface` | `docker0` (`DefaultBridgeInterface`) | bridge whose gateway is additionally bound, best-effort; empty disables |
| `-buildkitd-config` | `/opt/runs-fleet/buildkitd.toml` (`DefaultBuildkitdConfig`) | path to write the BuildKit mirror config to; empty disables |

Environment: `ECR_PULL_THROUGH_ENDPOINT` (required),
`AWS_REGION` (optional — IMDS fallback).

Internal helpers, all unit-tested through injected seams:
`resolveRegion`, `imdsRegion`, `parseListenAddrs`, `portOf`,
`discoverBridgeAddr`, `interfaceAddrs`, `listenAll`, `bindBridge`,
`reconcileBuildkitdConfig`, `addBridge`/`bridgeParams`, `buildkitdConfig`,
`writeBuildkitdConfig`, `removeBuildkitdConfig`.

### Build-time knobs

- Packer variable `ecr_pull_through_endpoint`
  ([packer/runs-fleet-runner-arm64.pkr.hcl](../../packer/runs-fleet-runner-arm64.pkr.hcl):37
  and the amd64 twin), passed to the provisioner as
  `ECR_PULL_THROUGH_ENDPOINT` (:148). Empty (the default) bakes the proxy
  inert.
- `.github/workflows/build-amis.yml` sets
  `PKR_VAR_ecr_pull_through_endpoint` from
  `secrets.ECR_PULL_THROUGH_ENDPOINT || vars.ECR_PULL_THROUGH_ENDPOINT` —
  secret first, because the value names an account-specific registry and
  variables are readable on public repositories.
- `cmd/mirror-proxy/**` is in the runner-image and AMI rebuild trigger sets
  (`build-runner.yml`:13, `build-amis.yml`:97), and `go list -deps
  ./cmd/agent ./cmd/buildx-shim ./cmd/mirror-proxy` derives the first-party
  packages that also trigger a rebuild.

## Data [coverage: medium -- 4 sources]

The proxy owns no persistent state. Everything it holds is process-local:

| State | Where | Lifetime |
| ----- | ----- | -------- |
| ECR basic-auth token | `cachedTokenSource` (in memory, mutex-guarded) | until 5 min before ECR's ~12h expiry |
| ns → rule-prefix table | `Handler.namespaces` (in memory) | process lifetime; discovered once at startup, never refreshed |

Files it reads or writes on the instance:

| Path | Written by | Role |
| ---- | ---------- | ---- |
| `/opt/runs-fleet/mirror-env` | Packer (opt-in only) | `ECR_PULL_THROUGH_ENDPOINT`; its existence is the unit's `ConditionPathExists` gate |
| `/etc/docker/daemon.json` | Packer (opt-in only) | dockerd `registry-mirrors` + `insecure-registries` |
| `/opt/runs-fleet/buildkitd.toml` | **the proxy, at boot** | BuildKit mirror config naming the bound bridge address; removed when no bridge bound |
| `/usr/local/sbin/runs-fleet-mirror-proxy` | Packer | the binary, root-owned |
| `/usr/local/sbin/runs-fleet-mirror-ready` | Packer | TCP readiness poll |
| `/etc/systemd/system/runs-fleet-mirror-proxy.service` | Packer (unconditional) | the unit |

No credential material is ever persisted — no `docker login`, no
`~/.docker/config.json` entry. The token exists only in the proxy's memory and
on the wire to ECR.

## Key Decisions [coverage: high -- 14 sources]

- **A local translating proxy, not bake-time reference rewriting (PR #440,
  9f38af8).** PR #439 (d1c9ddb) first tried rewriting the pre-baked image
  references onto the cache and running `docker login` at bake and boot. Three
  of its premises did not survive contact: dockerd's `registry-mirrors` covers
  *all* Docker Hub images (not just `library/*`, so the manual `library/` infix
  work was unnecessary), every `docker login` lands in **root's** config while
  `buildx-setup` and job-time docker run as `ec2-user`, and nothing in that
  design could reach pulls **inside workflow jobs** — the main 429 source. The
  proxy covers job-time pulls, needs no `docker login` anywhere, and works for
  both users.
- **Fail open, always.** Every failure mode — proxy dead, token fetch failed,
  upstream unreachable, unknown `ns`, cache rule deleted, IAM missing —
  degrades to the client pulling from the real registry, i.e. exactly the
  unconfigured behaviour. A mirror is allowed to be a performance
  optimization; it is never allowed to break a build.
- **Bind the bridge gateway, not the primary ENI (PR #453).** BuildKit needs a
  host address reachable from inside a bridge container. The gateway is
  routable from every container on the host and from nowhere off it; the ENI
  would put a credential-injecting proxy on the VPC.
- **Loopback required, bridge best-effort.** Exiting on a bridge failure would
  also cost dockerd the loopback mirror that *was* working — strictly worse
  than the problem being reported. Exiting on a loopback failure is correct
  because a half-bound set would look healthy while shedding one namespace.
- **Discover the gateway; don't pin it with `bip`.** dockerd picks the bridge
  subnet from an address pool and avoids collisions with host networks. Pinning
  it would make dockerd refuse to start on an overlap, and
  `runs-fleet-agent.service` `Requires=docker.service` — so a pin turns a
  mirror problem into a dead runner.
- **The proxy writes `buildkitd.toml`, the AMI does not bake it (PR #453).**
  The config must name the address that actually bound. A second independent
  lookup could resolve differently, and BuildKit answers a mirror address
  nothing listens on by pulling from Hub *without saying so*. Same reason the
  file is **removed** when no bridge bound: absence is honest, a stale address
  is not.
- **Exit rather than serve a 502-only mirror (PR #444, 0134c9d).** Both region
  failures were originally non-fatal — discovery logged a WARN and served
  `docker.io` only; each token fetch logged a WARN and let the client fall
  back. The proxy therefore bound its port and reported *active* while
  answering 502 to every pull, and BuildKit's silent fallback meant builds
  still passed with the reported error naming the upstream. The pull-through
  cache had been bypassed since it was introduced. Discovery failure and
  region failure are now both fatal.
- **Route by `ns`, covering every rule (PR #441, 1f554bb).** BuildKit's
  containerd resolver names the registry it is mirroring in an `ns` query
  parameter, so one proxy can serve every pull-through rule the account has
  (`quay`, `registry.k8s.io`, …) with no per-registry configuration. Absent
  `ns` means `docker.io`; unknown `ns` falls back. Duplicate rules for one
  upstream resolve identically in the handler and in the generated config
  (shortest prefix, then lexicographic) so the two cannot disagree.
- **Pull-only, and dot-segments refused.** The listening port is reachable by
  any local process including job code, and the ECR token is registry-wide.
  `405` on writes and `404` on `..` are a containment boundary, not politeness.
- **Relay blob redirects instead of following them.** `ErrUseLastResponse`
  keeps image bytes streaming from object storage directly to the client — the
  proxy never becomes a data-plane bottleneck.
- **Baked unconditionally, activated by a file.** The unit and binary always
  ship; `ConditionPathExists=/opt/runs-fleet/mirror-env` decides whether they
  do anything. One Packer variable is the entire opt-in, and its absence is
  byte-identical to an unconfigured build.
- **`StartLimitIntervalSec=0`.** The proxy exits rather than serve without its
  required address, so a slow or throttled start can cost several restarts.
  systemd's default burst of 5-in-10s would latch the unit `failed` and the
  runner would silently lose the mirror for its whole life — the exact silent
  fallback the unit exists to prevent.
- **Readiness poll, non-fatal.** `Type=simple` reports "started" at fork, but
  the proxy calls ECR before binding — so `Before=` alone would not stop the
  boot units from racing the listener. The `ExecStartPost=-` poll makes the
  ordering real on the happy path while letting a slow start bind late rather
  than never.

## Gotchas [coverage: high -- 14 sources]

- **The failure mode is invisible until Hub throttles, and then it blames
  Hub.** Every degradation is a silent fallback to the real registry, so a
  broken mirror looks like nothing at all until a `429 Too Many Requests`
  naming `registry-1.docker.io` kills a build. When a 429 appears, check the
  mirror before believing the blame. From the instance:
  `ss -lntp | grep 8989`; from inside the builder container,
  `docker exec buildx_buildkit_<builder>0 wget -qO- http://172.17.0.1:8989/v2/`.
  In the #453 incident the real cause sat one line above the 429:
  `dial tcp 172.21.138.36:8989: connect: connection refused`.
- **`registry-mirrors` covers all of Docker Hub but *only* Docker Hub.** The
  common misreading in both directions: PR #439 assumed it covered only
  `library/*` official images (it does not — namespaced Hub images are mirrored
  too), and it is equally wrong to assume it extends to other registries. A
  bare `docker pull quay.io/...` in a job **always** goes direct, even with a
  `quay` pull-through rule configured; only BuildKit builds follow those rules.
- **BuildKit does not read `daemon.json`.** The `docker-container` driver runs
  its own buildkitd, which resolves registries itself. Configuring
  `registry-mirrors` and expecting `docker buildx build` to use it is the bug
  PR #453 fixed. `docker build` (the classic builder in dockerd) *is* covered
  by `daemon.json`; `docker buildx build` needs `buildkitd.toml`.
- **A `buildkitd.toml` naming an unbound address is worse than no config.**
  BuildKit treats an unreachable mirror as a cue to pull from Hub, silently.
  This is why `reconcileBuildkitdConfig` deletes the file when the bridge did
  not bind, and why `bindBridge` returning `("", nil)` on every failure path is
  load-bearing rather than lazy error handling.
- **A missing region is not a startup error you will notice — it *was* a
  fleet-wide 502.** Pre-#444, `LoadDefaultConfig` in a systemd unit resolved no
  region (near-empty environment; the SDK's IMDS region lookup only fires for a
  config that opts in), every ECR call failed with "Missing Region", and the
  mirror bound its port and answered 502 to everything. It is now fatal, but if
  you run the binary by hand outside EC2 with no `AWS_REGION`, it exits 1 by
  design.
- **Rules are discovered once, at startup.** Adding a pull-through rule to the
  registry does not reach a running proxy; the instance must restart the unit
  (or, in practice, a new ephemeral runner must boot). Conversely a rule
  *deleted* under a running proxy makes those pulls 502 → fall back to the real
  registry.
- **The token is registry-wide.** `ecr:GetAuthorizationToken` returns a
  credential for the whole registry, not the cache prefix. That, plus a port
  any local process can reach, is why `..` segments are refused and why the
  binary and its unit are root-owned in `/usr/local/sbin` — workflow code runs
  as `ec2-user` and must not be able to swap either.
- **Base-layer pre-bake pulls still go to Docker Hub.** `PREBAKE_IMAGES` in
  `provision-base.sh` is pulled during the *base* build; the proxy arrives in
  the *runner* layer. The opt-in reduces job-time and boot-time pulls, not
  bake-time ones.
- **Changing the endpoint needs an AMI rebuild.** The value feeds the
  runner-AMI build, and changing a secret or variable triggers nothing on its
  own — a `Build AMIs` dispatch is required.
- **`packer/README.md` at commit 61e280c is stale on the BuildKit config
  mechanism.** It still describes `buildkitd.toml` being "rendered at boot from
  `/opt/runs-fleet/buildkitd.toml.tmpl`" by a
  `runs-fleet-mirror-buildkitd-config` unit — the pre-#453 design. Neither the
  template nor that unit exists anywhere in the tree; PR #453 replaced them
  with the proxy writing the file directly from the address it bound, and
  didn't update those two README paragraphs. Trust
  `cmd/mirror-proxy/config.go` and `provision-runs-fleet.sh` over the README
  here.
- **Endpoint validation is bake-time only, and shape-only.** The provisioner
  checks the value matches `https://*/*`; a syntactically valid endpoint
  naming a registry or prefix that does not exist bakes fine and fails at
  runtime as a 404/502 fallback.
- **A workflow bringing its own `--buildkitd-config` is redirected, not
  covered by our file.** buildx accepts one config, so the shim rewrites the
  mirror address *inside* the workflow's config to the address the proxy bound
  (outcome `engaged:user-config-redirect`), touching only addresses on the
  mirror's own port whose host is local. That logic lives in
  `cmd/buildx-shim` — see [infrastructure](infrastructure.md).
- **Observability is a single counter.** `RunnerBuildCacheInterception`
  (`engaged`/`skipped`/`failed`/`disabled`) reports the buildx shim's outcome;
  there is **no** metric for mirror hit rate, 502 rate, or fallback volume.
  The proxy's own signal is its `slog` JSON on stdout, in the instance journal
  — which the runner ships to S3 rather than CloudWatch Logs (see
  [observability](observability.md)).

## Sources [coverage: high]

- [pkg/mirrorproxy/proxy.go](../../pkg/mirrorproxy/proxy.go)
- [pkg/mirrorproxy/discover.go](../../pkg/mirrorproxy/discover.go)
- [pkg/mirrorproxy/token.go](../../pkg/mirrorproxy/token.go)
- [cmd/mirror-proxy/main.go](../../cmd/mirror-proxy/main.go)
- [cmd/mirror-proxy/config.go](../../cmd/mirror-proxy/config.go)
- [packer/provision-runs-fleet.sh](../../packer/provision-runs-fleet.sh)
- [packer/provision-base.sh](../../packer/provision-base.sh)
- [packer/runs-fleet-runner-arm64.pkr.hcl](../../packer/runs-fleet-runner-arm64.pkr.hcl)
- [packer/runs-fleet-runner-amd64.pkr.hcl](../../packer/runs-fleet-runner-amd64.pkr.hcl)
- [packer/README.md](../../packer/README.md)
- [docker/runner/Dockerfile](../../docker/runner/Dockerfile)
- [cmd/buildx-shim/main.go](../../cmd/buildx-shim/main.go)
- [.github/workflows/build-amis.yml](../../.github/workflows/build-amis.yml)
- [.github/workflows/build-runner.yml](../../.github/workflows/build-runner.yml)
