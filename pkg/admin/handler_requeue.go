package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/housekeeping"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

const (
	defaultRequeueThreshold = 15 * time.Minute
	minRequeueThresholdMins = 10
)

// RequeueHandler exposes the operator-triggered requeue of runner-less / hung jobs.
// It re-dispatches a fresh runner for a still-queued GitHub job by re-enqueuing into the
// main SQS queue; it never cancels, re-runs, or otherwise touches the GitHub job/run.
type RequeueHandler struct {
	ec2Client     housekeeping.EC2API
	dynamoClient  housekeeping.OrphanScanAPI
	requeuer      housekeeping.JobRequeuer
	jobsTableName string
	auth          *AuthMiddleware
	log           *logging.Logger
}

// NewRequeueHandler creates a requeue admin handler.
func NewRequeueHandler(ec2Client housekeeping.EC2API, dynamoClient housekeeping.OrphanScanAPI, requeuer housekeeping.JobRequeuer, jobsTableName string, auth *AuthMiddleware) *RequeueHandler {
	return &RequeueHandler{
		ec2Client:     ec2Client,
		dynamoClient:  dynamoClient,
		requeuer:      requeuer,
		jobsTableName: jobsTableName,
		auth:          auth,
		log:           logging.WithComponent(logging.LogTypeAdmin, "requeue"),
	}
}

// RegisterRoutes registers requeue admin routes.
func (h *RequeueHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/housekeeping/requeue-hung-jobs", h.auth.WrapFunc(h.RequeueHungJobs))
}

// RequeueHungJobsResponse contains the result of a requeue sweep.
type RequeueHungJobsResponse struct {
	Requeued         int     `json:"requeued"`
	Candidates       int     `json:"candidates"`
	SkippedExhausted int     `json:"skipped_exhausted"`
	JobIDs           []int64 `json:"job_ids,omitempty"`
	Message          string  `json:"message"`
}

// RequeueHungJobs handles POST /api/housekeeping/requeue-hung-jobs.
//
// It re-dispatches a fresh runner for jobs whose instance launched/started but whose
// runner is dead or never registered (status launched/running past the threshold with a
// gone or unconfirmed instance), bounded by housekeeping.MaxRequeueRetries. The GitHub
// job stays queued; only the runner side is re-driven.
//
// Query params:
//   - threshold_minutes: minimum job age in minutes (default 15, clamped to a 10 minimum)
//   - dry_run: if "true", report candidates without terminating, sending, or mutating
func (h *RequeueHandler) RequeueHungJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.jobsTableName == "" {
		h.writeError(w, http.StatusServiceUnavailable, "Jobs table not configured", "")
		return
	}
	if h.requeuer == nil {
		h.writeError(w, http.StatusServiceUnavailable, "Job queue not configured", "")
		return
	}

	threshold := defaultRequeueThreshold
	if s := r.URL.Query().Get("threshold_minutes"); s != "" {
		if mins, err := strconv.Atoi(s); err == nil && mins >= minRequeueThresholdMins {
			threshold = time.Duration(mins) * time.Minute
		}
	}

	dryRun := r.URL.Query().Get("dry_run") == "true"

	result, err := housekeeping.RequeueHungJobs(ctx, housekeeping.RequeueDeps{
		Scan:         h.dynamoClient,
		EC2:          h.ec2Client,
		TerminateEC2: h.ec2Client,
		Requeuer:     h.requeuer,
		JobsTable:    h.jobsTableName,
		Log:          h.log,
	}, housekeeping.RequeueOptions{
		Threshold: threshold,
		// launched only: the runner never confirmed. A running job has a live
		// runner doing real work and must never be terminated/requeued; the
		// "runner died mid-job" case is owned by spot-interruption + the orphan
		// reaper, not this operator action.
		Statuses: []db.JobStatus{db.JobStatusLaunched},
		DryRun:   dryRun,
	})
	if err != nil {
		h.log.Error(ctx, "requeue hung jobs failed", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to requeue hung jobs", err.Error())
		return
	}

	resp := RequeueHungJobsResponse{
		Requeued:         result.Requeued,
		Candidates:       result.Candidates,
		SkippedExhausted: result.SkippedExhausted,
		JobIDs:           result.JobIDs,
	}
	switch {
	case result.Candidates == 0:
		resp.Message = "No hung jobs found"
	case dryRun:
		resp.Message = fmt.Sprintf("Dry run: would requeue %d hung job(s)", len(result.JobIDs))
	default:
		resp.Message = fmt.Sprintf("Requeued %d hung job(s)", result.Requeued)
	}

	h.auditLog(r, "housekeeping.requeue_hung_jobs", "success",
		slog.Bool("dry_run", dryRun),
		slog.Int("candidates", result.Candidates),
		slog.Int("requeued", result.Requeued),
		slog.Int("skipped_exhausted", result.SkippedExhausted))

	h.writeJSON(w, http.StatusOK, resp)
}

func (h *RequeueHandler) auditLog(r *http.Request, action, result string, extra ...any) {
	remoteAddr := r.Header.Get("X-Forwarded-For")
	if remoteAddr == "" {
		remoteAddr = r.RemoteAddr
	}
	user := GetUsername(r.Context())
	if user == "" {
		user = "anonymous"
	}
	attrs := []any{
		slog.Bool(logging.KeyAudit, true),
		slog.String(logging.KeyUser, user),
		slog.String(logging.KeyAction, action),
		slog.String(logging.KeyResult, result),
		slog.String(logging.KeyRemoteAddr, remoteAddr),
	}
	attrs = append(attrs, extra...)
	auditLog.Info(r.Context(), "admin action", attrs...)
}

func (h *RequeueHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	ctx := context.Background()
	buf, err := json.Marshal(data)
	if err != nil {
		h.log.Error(ctx, "json encode failed", slog.String(logging.KeyError, err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	buf = append(buf, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		h.log.Error(ctx, "response write failed", slog.String(logging.KeyError, err.Error()))
	}
}

func (h *RequeueHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}
