package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/housekeeping"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

const (
	defaultOrphanedJobThreshold = 2 * time.Hour
	minOrphanedJobThreshold     = 10 * time.Minute
	// defaultHousekeepingMaxItems bounds one operator-triggered sweep. The console
	// drains successive batches, so this trades a few more requests for the
	// certainty that no single one outlives the browser's fetch timeout.
	defaultHousekeepingMaxItems = 100
	maxHousekeepingMaxItems     = 500
)

// parseMaxItems reads the batch cap shared by the housekeeping sweeps, answering
// false once it has written the 400 for an out-of-range value.
func parseMaxItems(w http.ResponseWriter, r *http.Request, writeErr func(http.ResponseWriter, int, string, string)) (int, bool) {
	raw := r.URL.Query().Get("max_items")
	if raw == "" {
		return defaultHousekeepingMaxItems, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > maxHousekeepingMaxItems {
		writeErr(w, http.StatusBadRequest, "Invalid max_items",
			"must be a whole number between 1 and "+strconv.Itoa(maxHousekeepingMaxItems))
		return 0, false
	}
	return n, true
}

// OrphanInstanceSweeper runs the orphaned-instance reaper on demand. It is the
// scheduled housekeeping task's detection and termination, callable with a dry run.
type OrphanInstanceSweeper interface {
	SweepOrphanedInstances(ctx context.Context, dryRun bool) (housekeeping.OrphanInstanceSweep, error)
}

// HousekeepingHandler provides admin endpoints for housekeeping actions.
type HousekeepingHandler struct {
	ec2Client     housekeeping.OrphanEC2API
	dynamoClient  housekeeping.OrphanScanAPI
	jobsTableName string
	auditDB       AuditDB
	sweeper       OrphanInstanceSweeper
	auth          *AuthMiddleware
	log           *logging.Logger
}

// NewHousekeepingHandler creates a new housekeeping admin handler. sweeper is
// optional: housekeeping is disabled when no pools table is configured, and the
// instance-reaper endpoint then reports itself unavailable.
func NewHousekeepingHandler(ec2Client housekeeping.OrphanEC2API, dynamoClient housekeeping.OrphanScanAPI, jobsTableName string, auditDB AuditDB, sweeper OrphanInstanceSweeper, auth *AuthMiddleware) *HousekeepingHandler {
	return &HousekeepingHandler{
		ec2Client:     ec2Client,
		dynamoClient:  dynamoClient,
		jobsTableName: jobsTableName,
		auditDB:       auditDB,
		sweeper:       sweeper,
		auth:          auth,
		log:           logging.WithComponent(logging.LogTypeAdmin, "housekeeping"),
	}
}

// RegisterRoutes registers housekeeping admin routes.
func (h *HousekeepingHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/housekeeping/orphaned-jobs", h.auth.WrapFunc(h.CleanupOrphanedJobs))
	mux.Handle("POST /api/housekeeping/orphaned-instances", h.auth.WrapFunc(h.ReapOrphanedInstances))
}

// OrphanedInstancesResponse reports one orphaned-instance sweep.
type OrphanedInstancesResponse struct {
	InstanceIDs []string `json:"instance_ids,omitempty"`
	Candidates  int      `json:"candidates"`
	Terminated  int      `json:"terminated"`
	DryRun      bool     `json:"dry_run"`
	Message     string   `json:"message"`
}

// ReapOrphanedInstances handles POST /api/housekeeping/orphaned-instances.
//
// It runs the same five-phase detection the scheduled reaper uses (over-runtime
// tagged instances, untagged profile zombies, never-claimed cold starts, finished
// instances that failed to self-terminate, and abandoned stopped instances) and
// terminates what it finds. It deliberately does not take the housekeeping task
// lock: the scheduled sweep converges on the same idempotent TerminateInstances
// call, so the worst case of an overlap is a duplicate terminate, and waiting on a
// 60s lock would make the button feel broken.
//
// Query params:
//   - dry_run: if "true", report the instances that would be reaped without touching them
func (h *HousekeepingHandler) ReapOrphanedInstances(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.sweeper == nil {
		h.writeError(w, http.StatusServiceUnavailable, "Housekeeping not configured",
			"the orphaned-instance reaper is unavailable without a pools table")
		return
	}

	dryRun := r.URL.Query().Get("dry_run") == queryTrue

	sweep, err := h.sweeper.SweepOrphanedInstances(ctx, dryRun)
	if err != nil {
		h.log.Error(ctx, "orphaned instance sweep failed", slog.String(logging.KeyError, err.Error()))
		recordAdminAction(r, h.auditDB, "housekeeping.orphaned-instances", "", "error",
			slog.Bool("dry_run", dryRun), slog.String(logging.KeyReason, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to sweep orphaned instances", err.Error())
		return
	}

	resp := OrphanedInstancesResponse{
		InstanceIDs: sweep.InstanceIDs,
		Candidates:  len(sweep.InstanceIDs),
		Terminated:  sweep.Terminated,
		DryRun:      sweep.DryRun,
	}
	switch {
	case len(sweep.InstanceIDs) == 0:
		resp.Message = "No orphaned instances found"
	case dryRun:
		resp.Message = "Dry run: would terminate " + strconv.Itoa(len(sweep.InstanceIDs)) + " orphaned instance(s)"
	default:
		resp.Message = "Terminated " + strconv.Itoa(sweep.Terminated) + " orphaned instance(s)"
	}

	recordAdminAction(r, h.auditDB, "housekeeping.orphaned-instances", strings.Join(sweep.InstanceIDs, ","), "success",
		slog.Bool("dry_run", dryRun),
		slog.Int("candidates", len(sweep.InstanceIDs)),
		slog.Int("terminated", sweep.Terminated))

	h.writeJSON(w, http.StatusOK, resp)
}

// CleanupOrphanedJobsResponse contains the result of orphaned job cleanup.
type CleanupOrphanedJobsResponse struct {
	Cleaned    int     `json:"cleaned"`
	Candidates int     `json:"candidates"`
	JobIDs     []int64 `json:"job_ids,omitempty"`
	// Truncated reports that the batch cap left candidates unread, so another
	// call has more to do.
	Truncated bool   `json:"truncated"`
	Message   string `json:"message"`
}

// CleanupOrphanedJobs handles POST /api/housekeeping/orphaned-jobs.
//
// It reports Truncated when the batch cap left candidates unread, which is how the
// console knows to send another batch. Bounding one call is what keeps this endpoint
// inside the browser's fetch timeout on a large jobs table.
//
// Query params:
//   - threshold_minutes: minimum age in minutes for jobs to be considered orphaned (default: 120, min: 10)
//   - max_items: how many candidates to take on this call (default 100, max 500)
//   - dry_run: if "true", only report what would be cleaned without actually cleaning
func (h *HousekeepingHandler) CleanupOrphanedJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.jobsTableName == "" {
		h.writeError(w, http.StatusServiceUnavailable, "Jobs table not configured", "")
		return
	}

	threshold := defaultOrphanedJobThreshold
	if thresholdStr := r.URL.Query().Get("threshold_minutes"); thresholdStr != "" {
		if mins, err := strconv.Atoi(thresholdStr); err == nil && mins >= 10 {
			threshold = time.Duration(mins) * time.Minute
		}
	}

	maxItems, ok := parseMaxItems(w, r, h.writeError)
	if !ok {
		return
	}

	dryRun := r.URL.Query().Get("dry_run") == queryTrue

	candidates, truncated, err := housekeeping.FindOrphanedJobCandidates(ctx, h.dynamoClient, h.jobsTableName, threshold,
		housekeeping.WithMaxItems(maxItems))
	if err != nil {
		h.log.Error(ctx, "failed to find orphaned job candidates", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to scan for orphaned jobs", err.Error())
		return
	}

	if len(candidates) == 0 {
		recordAdminAction(r, h.auditDB, "housekeeping.orphaned-jobs", "", "success",
			slog.Bool("dry_run", dryRun), slog.Int("cleaned", 0), slog.Int("candidates", 0))
		h.writeJSON(w, http.StatusOK, CleanupOrphanedJobsResponse{
			Cleaned:    0,
			Candidates: 0,
			Truncated:  truncated,
			Message:    "No orphaned jobs found",
		})
		return
	}

	candidatesWithInstance, orphanedJobs := housekeeping.SeparateOrphanedJobs(candidates)

	if len(candidatesWithInstance) > 0 {
		existingInstances := housekeeping.BatchCheckInstanceExistence(ctx, h.ec2Client, candidatesWithInstance, h.instanceExists)
		for _, c := range candidatesWithInstance {
			if !existingInstances[c.InstanceID] {
				orphanedJobs = append(orphanedJobs, c)
			}
		}
	}

	if len(orphanedJobs) == 0 {
		recordAdminAction(r, h.auditDB, "housekeeping.orphaned-jobs", "", "success",
			slog.Bool("dry_run", dryRun), slog.Int("cleaned", 0), slog.Int("candidates", len(candidates)))
		h.writeJSON(w, http.StatusOK, CleanupOrphanedJobsResponse{
			Cleaned:    0,
			Candidates: len(candidates),
			Truncated:  truncated,
			Message:    "All candidate jobs have running instances",
		})
		return
	}

	if dryRun {
		jobIDs := make([]int64, len(orphanedJobs))
		for i, j := range orphanedJobs {
			jobIDs[i] = j.JobID
		}
		recordAdminAction(r, h.auditDB, "housekeeping.orphaned-jobs", joinJobIDs(jobIDs), "success",
			slog.Bool("dry_run", true), slog.Int("cleaned", 0), slog.Int("candidates", len(candidates)),
			slog.Bool("truncated", truncated))
		h.writeJSON(w, http.StatusOK, CleanupOrphanedJobsResponse{
			Cleaned:    0,
			Candidates: len(candidates),
			JobIDs:     jobIDs,
			Truncated:  truncated,
			Message:    "Dry run: would clean " + strconv.Itoa(len(orphanedJobs)) + " orphaned jobs",
		})
		return
	}

	var cleanedCount int
	var cleanedJobIDs []int64
	for _, j := range orphanedJobs {
		jobCtx := logging.ContextWith(ctx,
			slog.Int64(logging.KeyJobID, j.JobID),
			slog.String(logging.KeyInstanceID, j.InstanceID))
		marked, err := housekeeping.MarkJobOrphaned(ctx, h.dynamoClient, h.jobsTableName, j.JobID, j.Status)
		if err != nil {
			h.log.Error(jobCtx, "failed to mark job as orphaned",
				slog.String(logging.KeyError, err.Error()))
			continue
		}
		if !marked {
			h.log.Info(jobCtx, "job left its scanned status before it could be orphaned")
			continue
		}
		cleanedCount++
		cleanedJobIDs = append(cleanedJobIDs, j.JobID)
		h.log.Info(jobCtx, "marked job as orphaned")
	}

	recordAdminAction(r, h.auditDB, "housekeeping.orphaned-jobs", joinJobIDs(cleanedJobIDs), "success",
		slog.Bool("dry_run", false), slog.Int("cleaned", cleanedCount), slog.Int("candidates", len(candidates)),
		slog.Bool("truncated", truncated))
	h.writeJSON(w, http.StatusOK, CleanupOrphanedJobsResponse{
		Cleaned:    cleanedCount,
		Candidates: len(candidates),
		JobIDs:     cleanedJobIDs,
		Truncated:  truncated,
		Message:    "Cleaned " + strconv.Itoa(cleanedCount) + " orphaned jobs",
	})
}

// joinJobIDs renders job IDs as a comma-separated audit target string.
func joinJobIDs(jobIDs []int64) string {
	if len(jobIDs) == 0 {
		return ""
	}
	parts := make([]string, len(jobIDs))
	for i, id := range jobIDs {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func (h *HousekeepingHandler) instanceExists(ctx context.Context, instanceID string) bool {
	output, err := h.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		// InvalidInstanceID.NotFound means instance definitively doesn't exist
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidInstanceID.NotFound" {
			return false
		}
		// For API errors (throttling, AWS outages), assume exists (safe default)
		h.log.Warn(ctx, "failed to describe instance, assuming exists",
			slog.String("instance_id", instanceID),
			slog.String(logging.KeyError, err.Error()))
		return true
	}

	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
			if instance.State != nil && instance.State.Name != ec2types.InstanceStateNameTerminated {
				return true
			}
		}
	}
	return false
}

func (h *HousekeepingHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	// Response-writer helper with no request/context in scope.
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

func (h *HousekeepingHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}
