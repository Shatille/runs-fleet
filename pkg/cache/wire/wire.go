// Package wire holds the GitHub Actions v1 cache HTTP contract (the
// /_apis/artifactcache/* request/response shapes and auth header) shared by the
// orchestrator's cache server (pkg/cache) and the runner's v2->v1 bridge
// (pkg/cache/cachev2). It imports nothing else from pkg/cache, keeping the
// orchestrator/runner split cycle-free.
package wire

// CacheTokenHeader is the HTTP header carrying the runner's HMAC cache token.
const CacheTokenHeader = "X-Cache-Token"

// ReserveCacheRequest is the POST /_apis/artifactcache/caches request body.
type ReserveCacheRequest struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

// GetCacheResponse is the GET /_apis/artifactcache/cache 200 response body.
type GetCacheResponse struct {
	ArchiveLocation string `json:"archiveLocation"`
	CacheKey        string `json:"cacheKey"`
}
