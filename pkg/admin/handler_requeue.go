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
	metrics       housekeeping.MetricsAPI
	jobsTableName string
	auditDB       AuditDB
	auth          *AuthMiddleware
	log           *logging.Logger
}

// NewRequeueHandler creates a requeue admin handler. metrics is optional (nil-safe)
// and, when set, emits operator_requeue counters for the sweep's outcomes.
func NewRequeueHandler(ec2Client housekeeping.EC2API, dynamoClient housekeeping.OrphanScanAPI, requeuer housekeeping.JobRequeuer, metrics housekeeping.MetricsAPI, jobsTableName string, auditDB AuditDB, auth *AuthMiddleware) *RequeueHandler {
	return &RequeueHandler{
		ec2Client:     ec2Client,
		dynamoClient:  dynamoClient,
		requeuer:      requeuer,
		metrics:       metrics,
		jobsTableName: jobsTableName,
		auditDB:       auditDB,
		auth:          auth,
		log:           logging.WithComponent(logging.LogTypeAdmin, "requeue"),
	}
}

// RegisterRoutes registers requeue admin routes.
func (h *RequeueHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/housekeeping/requeue-hung-jobs", h.auth.WrapFunc(h.RequeueHungJobs))
	mux.Handle("POST /api/jobs/{id}/requeue", h.auth.WrapFunc(h.RequeueJob))
	mux.Handle("POST /api/jobs/{id}/reconcile", h.auth.WrapFunc(h.ReconcileJob))
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

	dryRun := r.URL.Query().Get("dry_run") == queryTrue

	result, err := housekeeping.RequeueHungJobs(ctx, housekeeping.RequeueDeps{
		Scan:         h.dynamoClient,
		EC2:          h.ec2Client,
		TerminateEC2: h.ec2Client,
		Requeuer:     h.requeuer,
		Metrics:      h.metrics,
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

	recordAdminAction(r, h.auditDB, "housekeeping.requeue_hung_jobs", joinJobIDs(result.JobIDs), "success",
		slog.Bool("dry_run", dryRun),
		slog.Int("candidates", result.Candidates),
		slog.Int("requeued", result.Requeued),
		slog.Int("skipped_exhausted", result.SkippedExhausted))

	h.writeJSON(w, http.StatusOK, resp)
}

// RequeueJobResponse reports a single job's re-dispatch. Outcome is the machine-
// readable verdict; Details is the sentence an operator reads when it was refused.
type RequeueJobResponse struct {
	JobID              int64  `json:"job_id"`
	Outcome            string `json:"outcome"`
	InstanceID         string `json:"instance_id,omitempty"`
	InstanceTerminated bool   `json:"instance_terminated"`
	RetryCount         int    `json:"retry_count"`
	Status             string `json:"status,omitempty"`
	Message            string `json:"message"`
	Details            string `json:"details,omitempty"`
}

// RequeueJob handles POST /api/jobs/{id}/requeue.
//
// Same guardrails as the sweep — launched only, bounded by MaxRequeueRetries — minus
// the staleness threshold, which exists to stop a sweep acting on jobs that may still
// be starting and has no meaning for a row an operator picked. A refusal is a 409
// carrying the reason, so the UI can say why rather than "failed".
func (h *RequeueHandler) RequeueJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	jobID, ok := h.parseJobID(w, r)
	if !ok {
		return
	}
	if !h.requeueConfigured(w) {
		return
	}

	result, err := housekeeping.RequeueJob(ctx, h.requeueDeps(), jobID)
	if err != nil {
		h.log.Error(ctx, "requeue job failed",
			slog.Int64(logging.KeyJobID, jobID),
			slog.String(logging.KeyError, err.Error()))
		h.recordJobAction(r, "job.requeue", jobID, "error", slog.String(logging.KeyReason, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to requeue job", err.Error())
		return
	}

	resp := RequeueJobResponse{
		JobID:              result.JobID,
		Outcome:            string(result.Outcome),
		InstanceID:         result.InstanceID,
		InstanceTerminated: result.InstanceTerminated,
		RetryCount:         result.RetryCount,
		Status:             result.Status,
	}

	if result.Outcome == housekeeping.OutcomeRequeued {
		resp.Message = fmt.Sprintf("Job %d requeued (attempt %d of %d)", jobID, result.RetryCount, housekeeping.MaxRequeueRetries)
		if result.InstanceTerminated {
			resp.Message += fmt.Sprintf("; instance %s terminated", result.InstanceID)
		}
		h.recordJobAction(r, "job.requeue", jobID, "success",
			slog.String("instance_id", result.InstanceID),
			slog.Bool("instance_terminated", result.InstanceTerminated),
			slog.Int("retry_count", result.RetryCount))
		h.writeJSON(w, http.StatusOK, resp)
		return
	}

	status, details := requeueRefusal(result, jobID)
	resp.Message = "Job was not requeued"
	resp.Details = details
	if status == http.StatusNotFound {
		h.recordJobAction(r, "job.requeue", jobID, auditDenied, slog.String(logging.KeyReason, details))
		h.writeError(w, http.StatusNotFound, "Job not found", details)
		return
	}
	h.recordJobAction(r, "job.requeue", jobID, auditDenied,
		slog.String("outcome", string(result.Outcome)),
		slog.String(logging.KeyReason, details))
	h.writeJSON(w, status, resp)
}

// requeueRefusal maps a non-requeued outcome to its HTTP status and the sentence an
// operator needs to decide what to do next.
func requeueRefusal(result housekeeping.SingleRequeueResult, jobID int64) (int, string) {
	switch result.Outcome {
	case housekeeping.OutcomeNotFound:
		return http.StatusNotFound, fmt.Sprintf("job %d has no record", jobID)
	case housekeeping.OutcomeExhausted:
		return http.StatusConflict, fmt.Sprintf("job %d has spent its %d requeue retries; a fresh runner has already failed to register twice",
			jobID, housekeeping.MaxRequeueRetries)
	case housekeeping.OutcomeWrongStatus:
		return http.StatusConflict, fmt.Sprintf("job %d is %s, not launched — only a job whose runner never confirmed can be re-dispatched",
			jobID, result.Status)
	case housekeeping.OutcomeNoRunID:
		return http.StatusConflict, fmt.Sprintf("job %d has no run_id, so no launch message can be rebuilt for it", jobID)
	case housekeeping.OutcomeLostRace:
		return http.StatusConflict, fmt.Sprintf("another sweep already owns job %d's re-dispatch", jobID)
	default:
		return http.StatusConflict, fmt.Sprintf("job %d was not requeued (%s)", jobID, result.Outcome)
	}
}

// notReconciledMessage heads every reconcile refusal; the reason lives in Details.
const notReconciledMessage = "Job was not reconciled"

// ReconcileJobResponse reports a single job's reconcile.
type ReconcileJobResponse struct {
	JobID      int64  `json:"job_id"`
	Outcome    string `json:"outcome"`
	Orphaned   bool   `json:"orphaned"`
	InstanceID string `json:"instance_id,omitempty"`
	Status     string `json:"status,omitempty"`
	Message    string `json:"message"`
	Details    string `json:"details,omitempty"`
}

// ReconcileJob handles POST /api/jobs/{id}/reconcile: the targeted form of the
// orphaned-jobs sweep. It retires a job whose instance is gone and refuses while the
// instance is alive, so a live runner's record is never hidden.
func (h *RequeueHandler) ReconcileJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	jobID, ok := h.parseJobID(w, r)
	if !ok {
		return
	}
	if h.jobsTableName == "" {
		h.writeError(w, http.StatusServiceUnavailable, "Jobs table not configured", "")
		return
	}

	result, err := housekeeping.ReconcileJob(ctx, h.dynamoClient, h.ec2Client, h.jobsTableName, jobID)
	if err != nil {
		h.log.Error(ctx, "reconcile job failed",
			slog.Int64(logging.KeyJobID, jobID),
			slog.String(logging.KeyError, err.Error()))
		h.recordJobAction(r, "job.reconcile", jobID, "error", slog.String(logging.KeyReason, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to reconcile job", err.Error())
		return
	}

	resp := ReconcileJobResponse{
		JobID:      result.JobID,
		Outcome:    string(result.Outcome),
		InstanceID: result.InstanceID,
		Status:     result.Status,
	}

	switch result.Outcome {
	case housekeeping.ReconcileOrphaned:
		resp.Orphaned = true
		resp.Message = fmt.Sprintf("Job %d marked orphaned; its instance no longer exists", jobID)
		h.recordJobAction(r, "job.reconcile", jobID, "success",
			slog.String("instance_id", result.InstanceID),
			slog.String("previous_status", result.Status))
		h.writeJSON(w, http.StatusOK, resp)
	case housekeeping.ReconcileNotFound:
		resp.Details = fmt.Sprintf("job %d has no record", jobID)
		h.recordJobAction(r, "job.reconcile", jobID, auditDenied, slog.String(logging.KeyReason, resp.Details))
		h.writeError(w, http.StatusNotFound, "Job not found", resp.Details)
	case housekeeping.ReconcileInstanceAlive:
		resp.Message = notReconciledMessage
		resp.Details = fmt.Sprintf("instance %s is still running; terminate it first if its runner is dead", result.InstanceID)
		h.recordJobAction(r, "job.reconcile", jobID, auditDenied, slog.String(logging.KeyReason, resp.Details))
		h.writeJSON(w, http.StatusConflict, resp)
	case housekeeping.ReconcileNoInstance:
		resp.Message = notReconciledMessage
		resp.Details = fmt.Sprintf("job %d is %s but records no instance, so there is nothing to verify it against", jobID, result.Status)
		h.recordJobAction(r, "job.reconcile", jobID, auditDenied, slog.String(logging.KeyReason, resp.Details))
		h.writeJSON(w, http.StatusConflict, resp)
	case housekeeping.ReconcileLostRace:
		resp.Message = notReconciledMessage
		resp.Details = fmt.Sprintf("job %d reached a terminal state while it was being reconciled", jobID)
		h.recordJobAction(r, "job.reconcile", jobID, auditDenied, slog.String(logging.KeyReason, resp.Details))
		h.writeJSON(w, http.StatusConflict, resp)
	default:
		resp.Message = notReconciledMessage
		resp.Details = fmt.Sprintf("job %d is %s, which is already a settled state", jobID, result.Status)
		h.recordJobAction(r, "job.reconcile", jobID, auditDenied, slog.String(logging.KeyReason, resp.Details))
		h.writeJSON(w, http.StatusConflict, resp)
	}
}

func (h *RequeueHandler) parseJobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || jobID <= 0 {
		h.writeError(w, http.StatusBadRequest, "Invalid job ID", "must be a positive integer")
		return 0, false
	}
	return jobID, true
}

// requeueConfigured reports whether the dependencies a re-dispatch needs are wired,
// answering 503 when they are not.
func (h *RequeueHandler) requeueConfigured(w http.ResponseWriter) bool {
	if h.jobsTableName == "" {
		h.writeError(w, http.StatusServiceUnavailable, "Jobs table not configured", "")
		return false
	}
	if h.requeuer == nil {
		h.writeError(w, http.StatusServiceUnavailable, "Job queue not configured", "")
		return false
	}
	return true
}

func (h *RequeueHandler) requeueDeps() housekeeping.RequeueDeps {
	return housekeeping.RequeueDeps{
		Scan:         h.dynamoClient,
		EC2:          h.ec2Client,
		TerminateEC2: h.ec2Client,
		Requeuer:     h.requeuer,
		Metrics:      h.metrics,
		JobsTable:    h.jobsTableName,
		Log:          h.log,
	}
}

func (h *RequeueHandler) recordJobAction(r *http.Request, action string, jobID int64, result string, extra ...any) {
	recordAdminAction(r, h.auditDB, action, strconv.FormatInt(jobID, 10), result, extra...)
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
