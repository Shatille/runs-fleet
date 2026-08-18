package admin

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/Shavakan/runs-fleet/pkg/agent/logship"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// presignExpiry matches the Actions cache's presign window (pkg/cache/s3.go).
const presignExpiry = 15 * time.Minute

const auditActionJobLogsView = "job_logs.view"

// LogsS3API lists the runner-log objects for a job.
type LogsS3API interface {
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// LogsPresignAPI mints the short-lived download URLs the console hands out.
type LogsPresignAPI interface {
	PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

type jobLog struct {
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
	URL          string    `json:"url"`
}

type jobLogsResponse struct {
	Logs             []jobLog `json:"logs"`
	ExpiresInSeconds int      `json:"expires_in_seconds"`
}

// SetLogSource wires the runner-log reader. Without it the endpoint reports
// that this deployment keeps no runner logs rather than 404ing every job.
func (h *JobsHandler) SetLogSource(api LogsS3API, presigner LogsPresignAPI, bucket, prefix string) {
	h.logsS3 = api
	h.logsPresigner = presigner
	h.logsBucket = bucket
	h.logsPrefix = prefix
}

// SetAuditDB wires the audit trail so runner-log reads are attributable.
func (h *JobsHandler) SetAuditDB(auditDB AuditDB) {
	h.auditDB = auditDB
}

// GetJobLogs handles GET /api/jobs/{id}/logs.
func (h *JobsHandler) GetJobLogs(w http.ResponseWriter, r *http.Request) {
	if h.logsS3 == nil || h.logsPresigner == nil || h.logsBucket == "" {
		h.writeError(w, http.StatusServiceUnavailable, "Runner log source not configured",
			"This deployment does not keep runner logs; set the runner-logs bucket to enable it.")
		return
	}

	idStr := r.PathValue("id")
	jobID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid job ID", err.Error())
		return
	}

	entry, err := h.db.GetJobForAdmin(r.Context(), jobID)
	if err != nil {
		h.log.Error(r.Context(), "failed to get job for logs",
			slog.Int64(logging.KeyJobID, jobID),
			slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to get job", err.Error())
		return
	}
	if entry == nil {
		h.writeError(w, http.StatusNotFound, "Job not found", "")
		return
	}

	runID := strconv.FormatInt(entry.RunID, 10)
	objects, err := h.listJobLogs(r.Context(), runID, idStr, entry.InstanceID)
	if err != nil {
		h.log.Error(r.Context(), "failed to list runner logs",
			slog.Int64(logging.KeyJobID, jobID),
			slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusBadGateway, "Failed to list runner logs", err.Error())
		return
	}

	logs := make([]jobLog, 0, len(objects))
	for _, obj := range objects {
		url, presignErr := h.presign(r.Context(), *obj.Key)
		if presignErr != nil {
			h.log.Error(r.Context(), "failed to presign runner log",
				slog.String(logging.KeyError, presignErr.Error()))
			h.writeError(w, http.StatusBadGateway, "Failed to sign runner log URL", presignErr.Error())
			return
		}
		entryLog := jobLog{Name: keyBasename(*obj.Key), URL: url}
		if obj.Size != nil {
			entryLog.Size = *obj.Size
		}
		if obj.LastModified != nil {
			entryLog.LastModified = *obj.LastModified
		}
		logs = append(logs, entryLog)
	}

	recordAdminAction(r, h.auditDB, auditActionJobLogsView, idStr, "success",
		slog.Int("objects", len(logs)))

	h.writeJSON(w, http.StatusOK, jobLogsResponse{Logs: logs, ExpiresInSeconds: int(presignExpiry.Seconds())})
}

// listJobLogs prefers the per-job prefix, falling back to the run prefix
// filtered by instance so a job whose agent had no job_id (keyed under
// unknown-job) is still reachable.
func (h *JobsHandler) listJobLogs(ctx context.Context, runID, jobID, instanceID string) ([]types.Object, error) {
	byJob, err := h.list(ctx, logship.BuildPrefix(h.logsPrefix, runID, jobID))
	if err != nil {
		return nil, err
	}
	if len(byJob) > 0 {
		return byJob, nil
	}

	byRun, err := h.list(ctx, logship.BuildPrefix(h.logsPrefix, runID, ""))
	if err != nil {
		return nil, err
	}
	if instanceID == "" {
		return byRun, nil
	}
	var mine []types.Object
	for _, obj := range byRun {
		if strings.Contains(*obj.Key, "/"+instanceID+"/") {
			mine = append(mine, obj)
		}
	}
	return mine, nil
}

func (h *JobsHandler) list(ctx context.Context, prefix string) ([]types.Object, error) {
	out, err := h.logsS3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(h.logsBucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil, err
	}
	return out.Contents, nil
}

func (h *JobsHandler) presign(ctx context.Context, key string) (string, error) {
	req, err := h.logsPresigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(h.logsBucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(presignExpiry))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

func keyBasename(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return key
}
