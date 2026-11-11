package cache

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/Shavakan/runs-fleet/pkg/cache/wire"
	"github.com/Shavakan/runs-fleet/pkg/config"
)

// registerMultipartRoutes mounts the streamed-upload routes. Each resolves the
// S3 key from the request's key and version under the token's scope, exactly
// like ReserveCacheEntry, so a runner can only touch its own repository's entries.
func (h *Handler) registerMultipartRoutes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST "+wire.MultipartCreatePath, wrap(h.multipartHandler("multipart_create", h.createMultipart)))
	mux.Handle("POST "+wire.MultipartPartPath, wrap(h.multipartHandler("multipart_part", h.presignPart)))
	mux.Handle("POST "+wire.MultipartCompletePath, wrap(h.multipartHandler("multipart_complete", h.completeMultipart)))
	mux.Handle("POST "+wire.MultipartAbortPath, wrap(h.multipartHandler("multipart_abort", h.abortMultipart)))
}

type multipartOp func(w http.ResponseWriter, r *http.Request, server *Server, req wire.MultipartRequest) error

func (h *Handler) multipartHandler(operation string, op multipartOp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		server := h.scopedServer(r)
		if !server.MultipartEnabled() {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, config.MaxBodySize)
		var req wire.MultipartRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		req.Key = unmarkRefresh(req.Key)
		err := op(w, r, server, req)
		switch {
		case err == nil:
			if operation != "multipart_part" {
				h.emitOperation(r.Context(), operation)
			}
		case errors.Is(err, ErrMultipartS3):
			cacheLog.Error(r.Context(), "multipart request failed",
				slog.String("operation", operation),
				slog.String("key", req.Key),
				slog.String("scope", ScopeFromContext(r.Context())),
				slog.String("error", err.Error()))
			h.emitError(r.Context(), operation)
			http.Error(w, "Upstream storage error", http.StatusBadGateway)
		default:
			http.Error(w, "Invalid multipart parameters", http.StatusBadRequest)
		}
	}
}

func (h *Handler) createMultipart(w http.ResponseWriter, r *http.Request, server *Server, req wire.MultipartRequest) error {
	id, err := server.CreateMultipartUpload(r.Context(), req.Key, req.Version)
	if err != nil {
		return err
	}
	writeJSON(w, wire.MultipartCreateResponse{UploadID: id})
	return nil
}

func (h *Handler) presignPart(w http.ResponseWriter, r *http.Request, server *Server, req wire.MultipartRequest) error {
	url, err := server.PresignUploadPart(r.Context(), req.Key, req.Version, req.UploadID, req.PartNumber)
	if err != nil {
		return err
	}
	writeJSON(w, wire.MultipartPartResponse{URL: url})
	return nil
}

func (h *Handler) completeMultipart(w http.ResponseWriter, r *http.Request, server *Server, req wire.MultipartRequest) error {
	if err := server.CompleteMultipartUpload(r.Context(), req.Key, req.Version, req.UploadID, req.Parts); err != nil {
		return err
	}
	cacheLog.Info(r.Context(), "multipart cache entry committed",
		slog.String("key", req.Key),
		slog.Int("parts", len(req.Parts)),
		slog.Int64("size", req.Size),
		slog.String("scope", ScopeFromContext(r.Context())))
	w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) abortMultipart(w http.ResponseWriter, r *http.Request, server *Server, req wire.MultipartRequest) error {
	if err := server.AbortMultipartUpload(r.Context(), req.Key, req.Version, req.UploadID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
