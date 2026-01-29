package cache

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

var cacheLog = logging.WithComponent(logging.LogTypeCache, "handler")

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
		cacheLog.Info("cache authentication enabled")
	} else {
		cacheLog.Warn("cache authentication disabled - RUNS_FLEET_CACHE_SECRET not set")
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

// scopedServer returns a server scoped to the repository from the request context.
// If no scope is present in the context, returns the original server.
func (h *Handler) scopedServer(r *http.Request) *Server {
	scope := ScopeFromContext(r.Context())
	if scope != "" {
		return h.server.WithScope(scope)
	}
	return h.server
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

	server := h.scopedServer(r)
	cacheKey, err := server.CreateCacheEntry(r.Context(), req.Key, req.Version)
	if err != nil {
		http.Error(w, "Invalid cache parameters", http.StatusBadRequest)
		return
	}

	uploadURL, err := server.GeneratePresignedURL(r.Context(), cacheKey, http.MethodPut)
	if err != nil {
		cacheLog.Error("upload URL generation failed",
			slog.String("cache_key", cacheKey),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to generate upload URL", http.StatusInternalServerError)
		return
	}

	resp := reserveCacheResponse{
		CacheID:  int(time.Now().Unix()),
		CacheKey: cacheKey,
	}

	scope := ScopeFromContext(r.Context())
	cacheLog.Info("cache entry reserved",
		slog.String("key", req.Key),
		slog.String("version", req.Version),
		slog.Int("cache_id", resp.CacheID),
		slog.String("scope", scope))

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
		cacheLog.Error("commit request decode failed",
			slog.String("cache_id", cacheID),
			slog.String("error", err.Error()))
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	server := h.scopedServer(r)
	if err := server.CommitCacheEntry(r.Context(), cacheID); err != nil {
		cacheLog.Error("cache entry commit failed",
			slog.String("cache_id", cacheID),
			slog.Int64("size", req.Size),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to commit cache entry", http.StatusInternalServerError)
		return
	}

	scope := ScopeFromContext(r.Context())
	cacheLog.Info("cache entry committed",
		slog.String("cache_id", cacheID),
		slog.Int64("size", req.Size),
		slog.String("scope", scope))
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

	server := h.scopedServer(r)
	scope := ScopeFromContext(r.Context())
	keys := strings.Split(keysParam, ",")
	cacheKey, found, err := server.GetCacheEntry(r.Context(), keys, version)
	if err != nil {
		cacheLog.Error("cache entry check failed",
			slog.Any("keys", keys),
			slog.String("version", version),
			slog.String("scope", scope),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to check cache entry", http.StatusInternalServerError)
		return
	}

	if !found {
		cacheLog.Info("cache miss",
			slog.Any("keys", keys),
			slog.String("version", version),
			slog.String("scope", scope))
		if h.metrics != nil {
			if mErr := h.metrics.PublishCacheMiss(r.Context()); mErr != nil {
				cacheLog.Error("cache miss metric failed", slog.String("error", mErr.Error()))
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	downloadURL, err := server.GeneratePresignedURL(r.Context(), cacheKey, http.MethodGet)
	if err != nil {
		cacheLog.Error("download URL generation failed",
			slog.String("cache_key", cacheKey),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to generate download URL", http.StatusInternalServerError)
		return
	}

	cacheLog.Info("cache hit",
		slog.String("cache_key", cacheKey),
		slog.Any("keys", keys),
		slog.String("version", version),
		slog.String("scope", scope))
	if h.metrics != nil {
		if err := h.metrics.PublishCacheHit(r.Context()); err != nil {
			cacheLog.Error("cache hit metric failed", slog.String("error", err.Error()))
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
		cacheLog.Error("invalid cache ID",
			slog.String("cache_id", cacheID),
			slog.String("error", err.Error()))
		http.Error(w, "Invalid cache parameters", http.StatusBadRequest)
		return
	}

	server := h.scopedServer(r)
	downloadURL, err := server.GeneratePresignedURL(r.Context(), cacheID, http.MethodGet)
	if err != nil {
		cacheLog.Error("download URL generation failed",
			slog.String("cache_id", cacheID),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to generate download URL", http.StatusInternalServerError)
		return
	}

	scope := ScopeFromContext(r.Context())
	cacheLog.Info("redirecting to download URL",
		slog.String("cache_id", cacheID),
		slog.String("scope", scope))
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
