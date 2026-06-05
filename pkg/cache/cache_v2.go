package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cache/blobshim"
	"github.com/Shavakan/runs-fleet/pkg/config"
)

// cacheServiceV2Prefix is the Twirp route prefix for the GitHub Actions v2 cache
// service. actions/runner 2.3+ and actions/cache@v4.2+/v5 speak this protocol
// over ACTIONS_RESULTS_URL; the on-host interceptor routes it here.
const cacheServiceV2Prefix = "/twirp/github.actions.results.api.v1.CacheService/"

// blobURLTTL bounds how long an issued blob capability URL stays valid. Cache
// archives can be large and a save/restore step can run for many minutes, so
// the window is generous; the instance is ephemeral and single-tenant anyway.
const blobURLTTL = 6 * time.Hour

const contentTypeJSON = "application/json"

// blobURLBuilder mints signed, expiring blob-shim URLs for cache archives.
type blobURLBuilder struct {
	baseURL string
	signer  *blobshim.Signer
	ttl     time.Duration
	now     func() time.Time
}

func (b *blobURLBuilder) build(op blobshim.Operation, s3Key string) string {
	return b.baseURL + blobshim.PathPrefix + b.signer.Sign(op, s3Key, b.now().Add(b.ttl))
}

// ServiceV2 implements the GitHub Actions v2 cache Twirp service backed by
// the S3 Server. CreateCacheEntry/GetCacheEntryDownloadURL hand the client URLs
// that point at the on-host blob shim; FinalizeCacheEntryUpload confirms the
// archive landed in S3.
type ServiceV2 struct {
	server  *Server
	blobs   *blobURLBuilder
	metrics Metrics
}

// NewServiceV2 returns a v2 cache service backed by server, scoped to the given
// org/repo. A non-empty scope is mandatory: the S3 bucket is shared fleet-wide,
// so an unscoped service would let every repo read and overwrite every other
// repo's cache. Blob URLs are issued under blobBaseURL and signed by signer;
// metrics may be nil.
func NewServiceV2(server *Server, scope, blobBaseURL string, signer *blobshim.Signer, metrics Metrics) (*ServiceV2, error) {
	if scope == "" {
		return nil, fmt.Errorf("cache v2 service requires a non-empty scope")
	}
	scoped := server.WithScope(scope)
	if scoped.defaultScope != scope {
		return nil, fmt.Errorf("invalid cache scope %q", scope)
	}
	return &ServiceV2{
		server:  scoped,
		blobs:   &blobURLBuilder{baseURL: blobBaseURL, signer: signer, ttl: blobURLTTL, now: time.Now},
		metrics: metrics,
	}, nil
}

// RegisterRoutes mounts the three v2 cache RPCs on mux.
func (c *ServiceV2) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc(cacheServiceV2Prefix+"CreateCacheEntry", c.handleCreateCacheEntry)
	mux.HandleFunc(cacheServiceV2Prefix+"GetCacheEntryDownloadURL", c.handleGetDownloadURL)
	mux.HandleFunc(cacheServiceV2Prefix+"FinalizeCacheEntryUpload", c.handleFinalize)
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
	Key         string   `json:"key"`
	RestoreKeys []string `json:"restoreKeys"`
	Version     string   `json:"version"`
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
	s3Key, err := c.server.CreateCacheEntry(r.Context(), req.Key, req.Version)
	if err != nil {
		writeTwirpError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	cacheLog.Info(r.Context(), "v2 cache entry created",
		slog.String("key", req.Key), slog.String("version", req.Version))
	writeJSON(w, createCacheEntryResponse{OK: true, SignedUploadURL: c.blobs.build(blobshim.OpWrite, s3Key)})
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
	if req.Key == "" {
		writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "key is required")
		return
	}

	keys := append([]string{req.Key}, req.RestoreKeys...)
	s3Key, found, err := c.server.GetCacheEntry(r.Context(), keys, req.Version)
	if err != nil {
		// Fail open: a lookup error is reported to the client as a miss so the
		// job proceeds without our cache rather than failing.
		cacheLog.Error(r.Context(), "v2 cache lookup failed", slog.String("error", err.Error()))
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
		SignedDownloadURL: c.blobs.build(blobshim.OpRead, s3Key),
		MatchedKey:        c.server.bareKeyFromS3Key(req.Version, s3Key),
	})
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
	// The shim commits the archive to S3 on block-list commit, so the object is
	// already present; confirm it landed and report the result.
	_, found, err := c.server.GetCacheEntry(r.Context(), []string{req.Key}, req.Version)
	writeJSON(w, finalizeCacheEntryUploadResponse{OK: err == nil && found, EntryID: "1"})
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

// bareKeyFromS3Key recovers the cache key the client sent from a full S3 key,
// stripping the caches/<scope>/<version>/ prefix.
func (s *Server) bareKeyFromS3Key(version, s3Key string) string {
	return strings.TrimPrefix(s3Key, s.buildCachePrefix(version, ""))
}
