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

// Multipart upload routes. An orchestrator without them answers 404, which the
// runner treats as "stage on disk instead".
const (
	MultipartCreatePath   = "/_apis/artifactcache/multipart/create"
	MultipartPartPath     = "/_apis/artifactcache/multipart/part"
	MultipartCompletePath = "/_apis/artifactcache/multipart/complete"
	MultipartAbortPath    = "/_apis/artifactcache/multipart/abort"
)

// MultipartRequest is the request body of every multipart route. Key and
// Version name the cache entry exactly as ReserveCacheRequest does; UploadID is
// unset on create, PartNumber is set only on part, Parts and Size only on
// complete.
type MultipartRequest struct {
	Key        string          `json:"key"`
	Version    string          `json:"version"`
	UploadID   string          `json:"uploadId,omitempty"`
	PartNumber int32           `json:"partNumber,omitempty"`
	Parts      []MultipartPart `json:"parts,omitempty"`
	Size       int64           `json:"size,omitempty"`
}

// MultipartPart is one uploaded part of a multipart upload, as S3 returned it.
type MultipartPart struct {
	PartNumber int32  `json:"partNumber"`
	ETag       string `json:"etag"`
}

// MultipartCreateResponse is the multipart create response body.
type MultipartCreateResponse struct {
	UploadID string `json:"uploadId"`
}

// MultipartPartResponse is the multipart part response body: a presigned
// UploadPart URL for the requested part.
type MultipartPartResponse struct {
	URL string `json:"url"`
}
