---
topic: Cache Service (S3-backed Actions Cache)
last_compiled: 2026-04-30
sources_count: 3
---

# Cache Service (S3-backed Actions Cache)

## Purpose [coverage: high -- 3 sources]
Implement the GitHub Actions cache protocol over S3, so workflows running on
runs-fleet runners can use `actions/cache` against an internal cache instead
of GitHub-hosted cache. This avoids per-repo cache size limits and keeps
cache traffic inside AWS. The handler package self-describes as
"GitHub Actions cache protocol backed by S3" (`pkg/cache/s3.go`).

The service exposes a small HTTP surface that runners reach via
`ACTIONS_CACHE_URL`, authenticated with an HMAC token passed as
`ACTIONS_CACHE_TOKEN` (mapped onto the `X-Cache-Token` or `Authorization:
Bearer` header).

## Architecture [coverage: high -- 3 sources]
Three layers, all in `pkg/cache/`:

1. `auth.go` — stateless HMAC token validation middleware
   (`AuthMiddleware`). Tokens are self-contained — no server-side token
   registry. Optional repository scope is carried inside the token and
   propagated through `context.Context` via the `scopeContextKey`.
2. `handler.go` — HTTP handler implementing the cache protocol routes,
   wired onto an `http.ServeMux`. Wraps each route through the auth
   middleware when a cache secret is configured.
3. `s3.go` — `Server` struct that talks to S3 for metadata lookups
   (`HeadObject`, `ListObjectsV2`) and produces pre-signed URLs for direct
   client-to-S3 transfer (`PresignPutObject`, `PresignGetObject`).

The handler never proxies cache bytes itself; it only issues HTTP
redirects/`Location` headers to pre-signed S3 URLs. The orchestrator hosts
this handler; runners hit it during `actions/cache` save/restore.

## Talks To [coverage: high -- 3 sources]
- **S3** via the AWS SDK v2 — `HeadObject`, `ListObjectsV2`, and the
  pre-sign client for `PutObject`/`GetObject`. Bucket name is injected via
  `NewServer(cfg, bucketName)`; per CLAUDE.md this is `runs-fleet-cache`.
- **HMAC validator** in `auth.go` (no external service — shared secret).
- **Runners** acting as cache clients (`actions/cache` save/restore).
- **Metrics backend** through the `Metrics` interface
  (`PublishCacheHit` / `PublishCacheMiss`) when the handler is constructed
  via `NewHandlerWithMetrics` / `NewHandlerWithAuth`.
- **Logging** via `pkg/logging` (`logging.LogTypeCache` component
  `handler` and `auth`).

## API Surface [coverage: high -- 3 sources]
Routes are registered by `Handler.RegisterRoutes(mux)` in `handler.go`:

| Method  | Path                                  | Handler                  | Behavior                                                                 |
|---------|---------------------------------------|--------------------------|--------------------------------------------------------------------------|
| `POST`  | `/_apis/artifactcache/caches`         | `ReserveCacheEntry`      | Reserve a cache entry; returns `cacheId` + `cacheKey` and a `Location` header containing a pre-signed PUT URL. |
| `GET`   | `/_apis/artifactcache/cache`          | `GetCacheEntry`          | Lookup by `keys` (comma-separated) + `version`. Returns `{archiveLocation, cacheKey}` on hit, `204 No Content` on miss. |
| `PATCH` | `/_apis/artifactcache/caches/{id}`    | `CommitCacheEntry`       | Finalize an upload. No-op for S3 (atomic via pre-signed PUT). Returns `200 OK`. |
| `GET`   | `/_artifacts/{id}`                    | `DownloadCacheArtifact`  | `307 Temporary Redirect` to a pre-signed S3 GET URL.                     |

Notes on the `caches/` and `_artifacts/` prefix routes: both are mounted as
subtree handlers; the cache ID is parsed by stripping the prefix from
`r.URL.Path`. The `caches/` subtree only accepts `PATCH`; everything else
returns `405 Method Not Allowed`.

Auth header (when `RUNS_FLEET_CACHE_SECRET` is set): either
`X-Cache-Token: <token>` or `Authorization: Bearer <token>`. Missing or
invalid tokens return `401 Unauthorized`.

## Data [coverage: high -- 3 sources]
**S3 key layout** (`buildCacheKey` in `s3.go`):

- Without scope: `caches/<version>/<key>`
- With scope:   `caches/<scope>/<version>/<key>` where `scope` is `org` or
  `org/repo`.

**Cache entry lookup** (`GetCacheEntry`):

- `keys[0]` is the primary cache key, looked up via `HeadObject` for an
  exact match.
- `keys[1:]` are restore-keys, treated as prefixes via `ListObjectsV2`
  (`buildCachePrefix`), capped at `MaxKeys: 100`. Among matches, the most
  recently modified object wins (`findCacheByPrefix`).

**Token format** (`GenerateCacheToken` / `ParseCacheToken`):
`base64url(jobID:instanceID:scope) + "." + hex(HMAC-SHA256(secret, payload))`.
Stateless — server validates by recomputing the HMAC. Scope is optional
for backwards compatibility with older two-field tokens.

**Validation rules** (`validateKey`, `validateVersion`, `validateScope`):
keys ≤ 504 bytes, versions ≤ 512 bytes, no `..`, no `\`, no NUL, no leading
`/`. Scope must be `org` or `org/repo` with non-empty parts.

**Pre-signed URL TTL**: `15 * time.Minute` for both PUT and GET
(`GeneratePresignedURL`). Matches the 15-min expiry called out in
CLAUDE.md.

**Lifecycle**: 30-day object expiry on the bucket (per CLAUDE.md; configured
in the separate Terraform repo, not in this code).

**Request body limit**: `config.MaxBodySize` (`http.MaxBytesReader`) on
both reserve and commit endpoints.

## Key Decisions [coverage: high -- 3 sources]
- **Stateless HMAC tokens, not Bearer/JWT.** Tokens are self-contained
  payload+signature so the orchestrator does not have to maintain a token
  registry. `ValidateCacheToken` recomputes the HMAC on every request and
  uses `hmac.Equal` for constant-time comparison.
- **Pre-signed URLs over proxying.** The orchestrator never streams cache
  bytes — it returns S3 pre-signed PUT/GET URLs (15 min TTL). This keeps
  the Fargate container small/cheap and lets runners transfer at S3
  bandwidth.
- **Optional repo scoping in the key.** Tokens can encode `org/repo`,
  which the auth middleware lifts into `context.Context`; the handler
  derives a scoped `Server` per request via `scopedServer` /
  `Server.WithScope`. Scoping prevents cache key collisions across repos
  and is enforced at the S3 key level.
- **Restore-key prefix matching.** Mirrors `actions/cache` semantics:
  exact match for the primary key, prefix-match-most-recent for fallbacks.
- **Auth optional for backwards compatibility.** If `cacheSecret` is empty
  (`NewAuthMiddleware("")`), middleware short-circuits and lets all
  requests through. `NewHandlerWithAuth` logs a warning when this happens.
- **Commit is a no-op on S3.** Atomic uploads via pre-signed PUT mean the
  protocol's `PATCH /caches/{id}` step has nothing to do server-side; it
  just returns 200 to keep clients happy.

## Gotchas [coverage: medium -- 3 sources]
- **Pre-signed URL expiry.** 15 minutes. Slow uploads of large caches over
  weak runner networking can fail near the boundary; clients must restart
  the reserve flow.
- **Restore-key result ranking is best-effort.** `findCacheByPrefix`
  caps `ListObjectsV2` at 100 keys; popular prefixes with more than 100
  entries may not surface the truly newest object if the newer ones fall
  outside the first page.
- **`HeadObject` error sniffing is string-based as a fallback.** The code
  first checks `errors.As` for `types.NoSuchKey` / `types.NotFound`, but
  falls back to substring matches on `"NotFound"` / `"404"` in the error
  text. SDK error wrapping changes could quietly turn miss-handling into a
  500.
- **`reserveCacheResponse.CacheID` is `time.Now().Unix()`.** Two reserve
  requests in the same second produce identical IDs. Clients usually rely
  on `cacheKey` rather than the numeric ID, so this is mostly cosmetic, but
  worth knowing.
- **`WithScope` swallows invalid scope silently** — returns the original
  unscoped server instead of erroring. Callers relying on scoping for
  isolation should validate scopes before they reach the cache layer.
- **Auth disabled state is silent at request time.** When the secret is
  unset the middleware passes everything through. Logged once at startup;
  easy to miss in environments that copy from staging.
- **Header dual-read.** The middleware accepts both `X-Cache-Token` and
  `Authorization: Bearer`. If both are present, `X-Cache-Token` wins.

## Sources [coverage: medium]
- [pkg/cache/auth.go](../../pkg/cache/auth.go)
- [pkg/cache/handler.go](../../pkg/cache/handler.go)
- [pkg/cache/s3.go](../../pkg/cache/s3.go)
