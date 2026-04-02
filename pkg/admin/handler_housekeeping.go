package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
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
)

// HousekeepingHandler provides admin endpoints for housekeeping actions.
type HousekeepingHandler struct {
	ec2Client     housekeeping.OrphanEC2API
	dynamoClient  housekeeping.OrphanScanAPI
	jobsTableName string
	auth          *AuthMiddleware
	log           *logging.Logger
}

// NewHousekeepingHandler creates a new housekeeping admin handler.
func NewHousekeepingHandler(ec2Client housekeeping.OrphanEC2API, dynamoClient housekeeping.OrphanScanAPI, jobsTableName string, auth *AuthMiddleware) *HousekeepingHandler {
	return &HousekeepingHandler{
		ec2Client:     ec2Client,
		dynamoClient:  dynamoClient,
		jobsTableName: jobsTableName,
		auth:          auth,
		log:           logging.WithComponent(logging.LogTypeAdmin, "housekeeping"),
	}
}

// RegisterRoutes registers housekeeping admin routes.
func (h *HousekeepingHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/housekeeping/orphaned-jobs", h.auth.WrapFunc(h.CleanupOrphanedJobs))
}

// CleanupOrphanedJobsResponse contains the result of orphaned job cleanup.
type CleanupOrphanedJobsResponse struct {
	Cleaned    int      `json:"cleaned"`
	Candidates int      `json:"candidates"`
	JobIDs     []int64  `json:"job_ids,omitempty"`
	Message    string   `json:"message"`
}

// CleanupOrphanedJobs handles POST /api/housekeeping/orphaned-jobs.
// Query params:
//   - threshold_minutes: minimum age in minutes for jobs to be considered orphaned (default: 120, min: 10)
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

	dryRun := r.URL.Query().Get("dry_run") == "true"

	candidates, err := housekeeping.FindOrphanedJobCandidates(ctx, h.dynamoClient, h.jobsTableName, threshold)
	if err != nil {
		h.log.Error("failed to find orphaned job candidates", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to scan for orphaned jobs", err.Error())
		return
	}

	if len(candidates) == 0 {
		h.writeJSON(w, http.StatusOK, CleanupOrphanedJobsResponse{
			Cleaned:    0,
			Candidates: 0,
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
		h.writeJSON(w, http.StatusOK, CleanupOrphanedJobsResponse{
			Cleaned:    0,
			Candidates: len(candidates),
			Message:    "All candidate jobs have running instances",
		})
		return
	}

	if dryRun {
		jobIDs := make([]int64, len(orphanedJobs))
		for i, j := range orphanedJobs {
			jobIDs[i] = j.JobID
		}
		h.writeJSON(w, http.StatusOK, CleanupOrphanedJobsResponse{
			Cleaned:    0,
			Candidates: len(candidates),
			JobIDs:     jobIDs,
			Message:    "Dry run: would clean " + strconv.Itoa(len(orphanedJobs)) + " orphaned jobs",
		})
		return
	}

	var cleanedCount int
	var cleanedJobIDs []int64
	for _, j := range orphanedJobs {
		if err := housekeeping.MarkJobOrphaned(ctx, h.dynamoClient, h.jobsTableName, j.JobID); err != nil {
			h.log.Error("failed to mark job as orphaned",
				slog.Int64(logging.KeyJobID, j.JobID),
				slog.String(logging.KeyError, err.Error()))
			continue
		}
		cleanedCount++
		cleanedJobIDs = append(cleanedJobIDs, j.JobID)
		h.log.Info("marked job as orphaned",
			slog.Int64(logging.KeyJobID, j.JobID),
			slog.String("instance_id", j.InstanceID))
	}

	h.writeJSON(w, http.StatusOK, CleanupOrphanedJobsResponse{
		Cleaned:    cleanedCount,
		Candidates: len(candidates),
		JobIDs:     cleanedJobIDs,
		Message:    "Cleaned " + strconv.Itoa(cleanedCount) + " orphaned jobs",
	})
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
		h.log.Warn("failed to describe instance, assuming exists",
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
	buf, err := json.Marshal(data)
	if err != nil {
		h.log.Error("json encode failed", slog.String(logging.KeyError, err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	buf = append(buf, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		h.log.Error("response write failed", slog.String(logging.KeyError, err.Error()))
	}
}

func (h *HousekeepingHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}
