// Package cachev2 is the runner-side GitHub Actions v2 cache: the Twirp
// CacheService the on-host interceptor serves, bridged to the orchestrator's v1
// cache API (pkg/cache) for presigning via HTTPPresigner. Runner-only.
package cachev2

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/cache/blobshim"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// cacheServiceV2Prefix is the Twirp route prefix for the GitHub Actions v2 cache
// service. actions/runner 2.3+ and actions/cache@v4.2+/v5 speak this protocol
// over ACTIONS_RESULTS_URL; the on-host interceptor routes it here.
const cacheServiceV2Prefix = "/twirp/github.actions.results.api.v1.CacheService/"

const contentTypeJSON = "application/json"

var cacheLog = logging.WithComponent(logging.LogTypeCache, "cachev2")

// Metrics is the minimal cache-metrics surface the v2 service uses; the
// orchestrator's full publisher (pkg/metrics) satisfies it structurally. The
// runner currently passes nil (no metrics on the agent).
type Metrics interface {
	PublishCacheRequest(ctx context.Context, result string) error
}

// Presigner obtains orchestrator-minted presigned S3 URLs. The runner holds no
// S3 credentials — all presigning (and cache scoping, via the orchestrator's
// HMAC-token auth) happens on the orchestrator, which is the only entity with
// bucket access. This preserves the v1 isolation property.
type Presigner interface {
	// PresignUpload returns a presigned PUT URL for the given cache key+version.
	PresignUpload(ctx context.Context, key, version string) (url string, err error)
	// PresignDownload looks up the best match for keys (primary + restore keys)
	// at version and returns a presigned GET URL plus the matched key. found is
	// false on a cache miss.
	PresignDownload(ctx context.Context, keys []string, version string) (url, matchedKey string, found bool, err error)
}

// ServiceV2 answers the GitHub Actions v2 cache Twirp service on the runner. It
// turns CreateCacheEntry/GetCacheEntryDownloadURL into orchestrator presign
// calls and hands the client a local blob-shim URL that embeds the presigned S3
// URL; the shim then translates Azure block-blob bytes onto it.
//
// "v2" is GitHub's cache protocol version (the Twirp CacheService that
// superseded the v1 artifactcache REST API in handler.go), not runs-fleet
// versioning — see the Handler doc for the v1/v2 relationship.
type ServiceV2 struct {
	presigner Presigner
	blobBase  string
	metrics   Metrics
}

// NewServiceV2 returns a v2 cache service that presigns via presigner and issues
// blob URLs under blobBaseURL (the on-host shim's base). metrics may be nil.
func NewServiceV2(presigner Presigner, blobBaseURL string, metrics Metrics) *ServiceV2 {
	return &ServiceV2{presigner: presigner, blobBase: blobBaseURL, metrics: metrics}
}

// RegisterRoutes mounts the three v2 cache RPCs on mux.
func (c *ServiceV2) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc(cacheServiceV2Prefix+"CreateCacheEntry", c.handleCreateCacheEntry)
	mux.HandleFunc(cacheServiceV2Prefix+"GetCacheEntryDownloadURL", c.handleGetDownloadURL)
	mux.HandleFunc(cacheServiceV2Prefix+"FinalizeCacheEntryUpload", c.handleFinalize)
}

func (c *ServiceV2) blobURL(presignedURL string) string {
	return c.blobBase + blobshim.PathPrefix + blobshim.EncodeTarget(presignedURL)
}

type createCacheEntryRequest struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

type createCacheEntryResponse struct {
	OK              bool   `json:"ok"`
	SignedUploadURL string `json:"signedUploadUrl"`
}

type getCacheEntryDownloadURLRequest struct {
	Key string `json:"key"`
	// The cache@v5 Twirp client serializes the proto field restore_keys in
	// snake_case on the wire; bind both forms because encoding/json is
	// case-insensitive but not underscore-insensitive, so one tag can't cover
	// both. restoreKeys() merges them.
	RestoreKeysCamel []string `json:"restoreKeys"`
	RestoreKeysSnake []string `json:"restore_keys"`
	Version          string   `json:"version"`
}

// restoreKeys returns the restore-key list regardless of the wire casing,
// unioning both forms (order-preserving, de-duplicated) so neither is dropped.
func (r getCacheEntryDownloadURLRequest) restoreKeys() []string {
	seen := make(map[string]struct{}, len(r.RestoreKeysCamel)+len(r.RestoreKeysSnake))
	out := make([]string, 0, len(r.RestoreKeysCamel)+len(r.RestoreKeysSnake))
	for _, src := range [][]string{r.RestoreKeysCamel, r.RestoreKeysSnake} {
		for _, k := range src {
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	return out
}

type getCacheEntryDownloadURLResponse struct {
	OK                bool   `json:"ok"`
	SignedDownloadURL string `json:"signedDownloadUrl"`
	MatchedKey        string `json:"matchedKey"`
}

type finalizeCacheEntryUploadRequest struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

type finalizeCacheEntryUploadResponse struct {
	OK      bool   `json:"ok"`
	EntryID string `json:"entryId"`
}

func (c *ServiceV2) handleCreateCacheEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeTwirpError(w, http.StatusMethodNotAllowed, "bad_route", "POST required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, config.MaxBodySize)
	var req createCacheEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeTwirpError(w, http.StatusBadRequest, "malformed", "invalid request body")
		return
	}
	if req.Key == "" || req.Version == "" {
		writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "key and version are required")
		return
	}
	url, err := c.presigner.PresignUpload(r.Context(), req.Key, req.Version)
	if err != nil {
		cacheLog.Error(r.Context(), "v2 presign upload failed", slog.String("error", err.Error()))
		writeTwirpError(w, http.StatusBadGateway, "unavailable", "presign failed")
		return
	}
	cacheLog.Info(r.Context(), "v2 cache entry created",
		slog.String("key", req.Key), slog.String("version", req.Version))
	writeJSON(w, createCacheEntryResponse{OK: true, SignedUploadURL: c.blobURL(url)})
}

func (c *ServiceV2) handleGetDownloadURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeTwirpError(w, http.StatusMethodNotAllowed, "bad_route", "POST required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, config.MaxBodySize)
	var req getCacheEntryDownloadURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeTwirpError(w, http.StatusBadRequest, "malformed", "invalid request body")
		return
	}
	if req.Key == "" || req.Version == "" {
		writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "key and version are required")
		return
	}

	keys := append([]string{req.Key}, req.restoreKeys()...)
	url, matchedKey, found, err := c.presigner.PresignDownload(r.Context(), keys, req.Version)
	if err != nil {
		// Fail open: a lookup error is reported to the client as a miss so the
		// job proceeds without our cache rather than failing.
		cacheLog.Error(r.Context(), "v2 presign download failed", slog.String("error", err.Error()))
		c.publish(r.Context(), "miss")
		writeJSON(w, getCacheEntryDownloadURLResponse{OK: false})
		return
	}
	if !found {
		c.publish(r.Context(), "miss")
		writeJSON(w, getCacheEntryDownloadURLResponse{OK: false})
		return
	}

	c.publish(r.Context(), "hit")
	cacheLog.Info(r.Context(), "v2 cache hit",
		slog.String("key", req.Key), slog.String("version", req.Version))
	writeJSON(w, getCacheEntryDownloadURLResponse{
		OK:                true,
		SignedDownloadURL: c.blobURL(url),
		MatchedKey:        bareKeyForVersion(matchedKey, req.Version),
	})
}

// bareKeyForVersion recovers the cache key the client sent from the
// orchestrator's full S3 key (caches/<scope>/<version>/<key>). The client
// compares matchedKey against its requested keys, so it must be the bare key.
func bareKeyForVersion(fullKey, version string) string {
	marker := "/" + version + "/"
	if i := strings.Index(fullKey, marker); i >= 0 {
		return fullKey[i+len(marker):]
	}
	return fullKey
}

func (c *ServiceV2) handleFinalize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeTwirpError(w, http.StatusMethodNotAllowed, "bad_route", "POST required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, config.MaxBodySize)
	var req finalizeCacheEntryUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeTwirpError(w, http.StatusBadRequest, "malformed", "invalid request body")
		return
	}
	if req.Key == "" {
		writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "key is required")
		return
	}
	// The shim already PUT the archive to S3 on block-list commit (the client
	// only reaches Finalize after a successful upload), so this is an ack.
	writeJSON(w, finalizeCacheEntryUploadResponse{OK: true, EntryID: "1"})
}

func (c *ServiceV2) publish(ctx context.Context, result string) {
	if c.metrics == nil {
		return
	}
	if err := c.metrics.PublishCacheRequest(ctx, result); err != nil {
		cacheLog.Error(ctx, "v2 cache metric failed", slog.String("error", err.Error()))
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	_ = json.NewEncoder(w).Encode(v)
}

func writeTwirpError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "msg": msg})
}
