package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// JobsDB defines the database operations for jobs.
type JobsDB interface {
	ListJobsForAdmin(ctx context.Context, filter db.AdminJobFilter) ([]db.AdminJobEntry, int, error)
	GetJobForAdmin(ctx context.Context, jobID int64) (*db.AdminJobEntry, error)
	GetJobStatsForAdmin(ctx context.Context, since time.Time) (*db.AdminJobStats, error)
}

// JobResponse represents a job in the admin API response.
type JobResponse struct {
	JobID           int64      `json:"job_id"`
	RunID           int64      `json:"run_id,omitempty"`
	Repo            string     `json:"repo,omitempty"`
	InstanceID      string     `json:"instance_id,omitempty"`
	InstanceType    string     `json:"instance_type,omitempty"`
	Pool            string     `json:"pool,omitempty"`
	Spot            bool       `json:"spot"`
	WarmPoolHit     bool       `json:"warm_pool_hit"`
	RetryCount      int        `json:"retry_count"`
	Status          string     `json:"status"`
	ExitCode        int        `json:"exit_code,omitempty"`
	DurationSeconds int        `json:"duration_seconds,omitempty"`
	CreatedAt       *time.Time `json:"created_at,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

// JobStatsResponse contains aggregate job statistics.
type JobStatsResponse struct {
	Total       int     `json:"total"`
	Completed   int     `json:"completed"`
	Failed      int     `json:"failed"`
	Running     int     `json:"running"`
	Requeued    int     `json:"requeued"`
	WarmPoolHit int     `json:"warm_pool_hit"`
	HitRate     float64 `json:"hit_rate"`
}

// JobsHandler provides HTTP endpoints for job management.
type JobsHandler struct {
	db   JobsDB
	auth *AuthMiddleware
	log  *logging.Logger
}

// NewJobsHandler creates a new jobs handler.
func NewJobsHandler(db JobsDB, auth *AuthMiddleware) *JobsHandler {
	return &JobsHandler{
		db:   db,
		auth: auth,
		log:  logging.WithComponent(logging.LogTypeAdmin, "jobs"),
	}
}

// RegisterRoutes registers jobs API routes on the given mux.
func (h *JobsHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/jobs", h.auth.WrapFunc(h.ListJobs))
	mux.Handle("GET /api/jobs/stats", h.auth.WrapFunc(h.GetJobStats))
	mux.Handle("GET /api/jobs/{id}", h.auth.WrapFunc(h.GetJob))
}

// ListJobs handles GET /api/jobs.
func (h *JobsHandler) ListJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	filter := db.AdminJobFilter{
		Status: q.Get("status"),
		Pool:   q.Get("pool"),
		Limit:  50,
		Offset: 0,
	}

	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = t
		}
	}

	if limit := q.Get("limit"); limit != "" {
		if n, err := strconv.Atoi(limit); err == nil && n > 0 && n <= 100 {
			filter.Limit = n
		}
	}

	if offset := q.Get("offset"); offset != "" {
		if n, err := strconv.Atoi(offset); err == nil && n >= 0 {
			filter.Offset = n
		}
	}

	entries, total, err := h.db.ListJobsForAdmin(ctx, filter)
	if err != nil {
		h.log.Error("failed to list jobs", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to list jobs", err.Error())
		return
	}

	jobs := make([]JobResponse, len(entries))
	for i, e := range entries {
		jobs[i] = jobEntryToResponse(e)
	}

	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"jobs":   jobs,
		"total":  total,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

// GetJob handles GET /api/jobs/{id}.
func (h *JobsHandler) GetJob(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	if idStr == "" {
		h.writeError(w, http.StatusBadRequest, "Job ID is required", "")
		return
	}

	jobID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid job ID", err.Error())
		return
	}

	entry, err := h.db.GetJobForAdmin(r.Context(), jobID)
	if err != nil {
		h.log.Error("failed to get job",
			slog.Int64(logging.KeyJobID, jobID),
			slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to get job", err.Error())
		return
	}

	if entry == nil {
		h.writeError(w, http.StatusNotFound, "Job not found", "")
		return
	}

	h.writeJSON(w, http.StatusOK, jobEntryToResponse(*entry))
}

// GetJobStats handles GET /api/jobs/stats.
func (h *JobsHandler) GetJobStats(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Add(-24 * time.Hour)

	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if t, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			since = t
		}
	}

	stats, err := h.db.GetJobStatsForAdmin(r.Context(), since)
	if err != nil {
		h.log.Error("failed to get job stats", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to get job stats", err.Error())
		return
	}

	resp := JobStatsResponse{
		Total:       stats.Total,
		Completed:   stats.Completed,
		Failed:      stats.Failed,
		Running:     stats.Running,
		Requeued:    stats.Requeued,
		WarmPoolHit: stats.WarmPoolHit,
	}
	if stats.Completed > 0 {
		resp.HitRate = float64(stats.WarmPoolHit) / float64(stats.Completed)
	}

	h.writeJSON(w, http.StatusOK, resp)
}

func jobEntryToResponse(e db.AdminJobEntry) JobResponse {
	resp := JobResponse{
		JobID:           e.JobID,
		RunID:           e.RunID,
		Repo:            e.Repo,
		InstanceID:      e.InstanceID,
		InstanceType:    e.InstanceType,
		Pool:            e.Pool,
		Spot:            e.Spot,
		WarmPoolHit:     e.WarmPoolHit,
		RetryCount:      e.RetryCount,
		Status:          e.Status,
		ExitCode:        e.ExitCode,
		DurationSeconds: e.DurationSeconds,
	}
	if !e.CreatedAt.IsZero() {
		resp.CreatedAt = &e.CreatedAt
	}
	if !e.StartedAt.IsZero() {
		resp.StartedAt = &e.StartedAt
	}
	if !e.CompletedAt.IsZero() {
		resp.CompletedAt = &e.CompletedAt
	}
	return resp
}

func (h *JobsHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		h.log.Error("json encode failed", slog.String(logging.KeyError, err.Error()))
	}
}

func (h *JobsHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}
