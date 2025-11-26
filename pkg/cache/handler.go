package cache

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
)

// Metrics defines the interface for publishing cache metrics.
type Metrics interface {
	PublishCacheHit(ctx context.Context) error
	PublishCacheMiss(ctx context.Context) error
}

// Handler implements HTTP endpoints for GitHub Actions cache protocol.
type Handler struct {
	server  *Server
	metrics Metrics
	auth    *AuthMiddleware
}

// NewHandler creates a new cache handler.
func NewHandler(server *Server) *Handler {
	return &Handler{server: server}
}

// NewHandlerWithMetrics creates a new cache handler with metrics support.
func NewHandlerWithMetrics(server *Server, metrics Metrics) *Handler {
	return &Handler{server: server, metrics: metrics}
}

// NewHandlerWithAuth creates a new cache handler with authentication and metrics.
// Authentication is stateless - no token registration required on the server.
func NewHandlerWithAuth(server *Server, metrics Metrics, cacheSecret string) *Handler {
	var auth *AuthMiddleware

	if cacheSecret != "" {
		auth = NewAuthMiddleware(cacheSecret)
		log.Println("Cache authentication enabled (stateless HMAC)")
	} else {
		log.Println("WARNING: Cache authentication disabled - RUNS_FLEET_CACHE_SECRET not set")
	}

	return &Handler{
		server:  server,
		metrics: metrics,
		auth:    auth,
	}
}

// IsAuthEnabled returns whether cache authentication is enabled.
func (h *Handler) IsAuthEnabled() bool {
	return h.auth != nil && h.auth.IsEnabled()
}

type reserveCacheRequest struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

type reserveCacheResponse struct {
	CacheID  int    `json:"cacheId"`
	CacheKey string `json:"cacheKey,omitempty"`
}

type commitCacheRequest struct {
	Size int64 `json:"size"`
}

type getCacheResponse struct {
	ArchiveLocation string `json:"archiveLocation"`
	CacheKey        string `json:"cacheKey"`
}

// ReserveCacheEntry reserves a cache entry for upload via POST /_apis/artifactcache/caches.
func (h *Handler) ReserveCacheEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, config.MaxBodySize)

	var req reserveCacheRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	cacheKey, err := h.server.CreateCacheEntry(r.Context(), req.Key, req.Version)
	if err != nil {
		http.Error(w, "Invalid cache parameters", http.StatusBadRequest)
		return
	}

	uploadURL, err := h.server.GeneratePresignedURL(r.Context(), cacheKey, http.MethodPut)
	if err != nil {
		log.Printf("Failed to generate upload URL for key=%s: %v", cacheKey, err)
		http.Error(w, "Failed to generate upload URL", http.StatusInternalServerError)
		return
	}

	resp := reserveCacheResponse{
		CacheID:  int(time.Now().Unix()),
		CacheKey: cacheKey,
	}

	log.Printf("Reserved cache entry: key=%s version=%s cacheId=%d", req.Key, req.Version, resp.CacheID)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", uploadURL)
	_ = json.NewEncoder(w).Encode(resp)
}

// CommitCacheEntry commits a cache entry after upload via PATCH /_apis/artifactcache/caches/{id}.
func (h *Handler) CommitCacheEntry(w http.ResponseWriter, r *http.Request, cacheID string) {
	if cacheID == "" {
		http.Error(w, "Cache ID is required", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, config.MaxBodySize)

	var req commitCacheRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("Failed to decode commit request for cacheId=%s: %v", cacheID, err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.server.CommitCacheEntry(r.Context(), cacheID); err != nil {
		log.Printf("Failed to commit cache entry cacheId=%s size=%d: %v", cacheID, req.Size, err)
		http.Error(w, "Failed to commit cache entry", http.StatusInternalServerError)
		return
	}

	log.Printf("Committed cache entry: cacheId=%s size=%d", cacheID, req.Size)
	w.WriteHeader(http.StatusOK)
}

// GetCacheEntry retrieves cache metadata via GET /_apis/artifactcache/cache.
func (h *Handler) GetCacheEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	keysParam := r.URL.Query().Get("keys")
	version := r.URL.Query().Get("version")

	if keysParam == "" {
		http.Error(w, "keys parameter is required", http.StatusBadRequest)
		return
	}

	if version == "" {
		http.Error(w, "version parameter is required", http.StatusBadRequest)
		return
	}

	keys := strings.Split(keysParam, ",")
	cacheKey, found, err := h.server.GetCacheEntry(r.Context(), keys, version)
	if err != nil {
		log.Printf("Failed to check cache entry keys=%v version=%s: %v", keys, version, err)
		http.Error(w, "Failed to check cache entry", http.StatusInternalServerError)
		return
	}

	if !found {
		log.Printf("Cache miss: keys=%v version=%s", keys, version)
		// Publish cache miss metric
		if h.metrics != nil {
			if metricErr := h.metrics.PublishCacheMiss(r.Context()); metricErr != nil {
				log.Printf("Failed to publish cache miss metric: %v", metricErr)
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	downloadURL, err := h.server.GeneratePresignedURL(r.Context(), cacheKey, http.MethodGet)
	if err != nil {
		log.Printf("Failed to generate download URL for cacheKey=%s: %v", cacheKey, err)
		http.Error(w, "Failed to generate download URL", http.StatusInternalServerError)
		return
	}

	log.Printf("Cache hit: cacheKey=%s keys=%v version=%s", cacheKey, keys, version)
	// Publish cache hit metric
	if h.metrics != nil {
		if err := h.metrics.PublishCacheHit(r.Context()); err != nil {
			log.Printf("Failed to publish cache hit metric: %v", err)
		}
	}

	resp := getCacheResponse{
		ArchiveLocation: downloadURL,
		CacheKey:        cacheKey,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// DownloadCacheArtifact redirects to pre-signed S3 URL via GET /_artifacts/{id}.
func (h *Handler) DownloadCacheArtifact(w http.ResponseWriter, r *http.Request, cacheID string) {
	if cacheID == "" {
		http.Error(w, "Cache ID is required", http.StatusBadRequest)
		return
	}

	if err := validateKey(cacheID); err != nil {
		log.Printf("Invalid cache ID in download request: cacheId=%s err=%v", cacheID, err)
		http.Error(w, "Invalid cache parameters", http.StatusBadRequest)
		return
	}

	downloadURL, err := h.server.GeneratePresignedURL(r.Context(), cacheID, http.MethodGet)
	if err != nil {
		log.Printf("Failed to generate download URL for cacheId=%s: %v", cacheID, err)
		http.Error(w, "Failed to generate download URL", http.StatusInternalServerError)
		return
	}

	log.Printf("Redirecting to download URL: cacheId=%s", cacheID)
	http.Redirect(w, r, downloadURL, http.StatusTemporaryRedirect)
}

// RegisterRoutes registers all cache API routes with the HTTP mux.
// If authentication is enabled, all cache endpoints require a valid token.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Helper to wrap handlers with auth middleware if enabled
	wrap := func(handler http.HandlerFunc) http.Handler {
		if h.auth != nil {
			return h.auth.WrapFunc(handler)
		}
		return handler
	}

	mux.Handle("POST /_apis/artifactcache/caches", wrap(h.ReserveCacheEntry))
	mux.Handle("GET /_apis/artifactcache/cache", wrap(h.GetCacheEntry))

	mux.Handle("/_apis/artifactcache/caches/", wrap(func(w http.ResponseWriter, r *http.Request) {
		cacheID := strings.TrimPrefix(r.URL.Path, "/_apis/artifactcache/caches/")
		if r.Method == http.MethodPatch {
			h.CommitCacheEntry(w, r, cacheID)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	mux.Handle("/_artifacts/", wrap(func(w http.ResponseWriter, r *http.Request) {
		cacheID := strings.TrimPrefix(r.URL.Path, "/_artifacts/")
		h.DownloadCacheArtifact(w, r, cacheID)
	}))
}
