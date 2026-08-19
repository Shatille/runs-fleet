package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"

	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/housekeeping"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// instanceIDPattern validates EC2 instance IDs: i- followed by exactly 8
// (legacy) or 17 (current) hex chars. The in-between lengths AWS never issues
// are rejected at the boundary; EC2 remains the authoritative validator.
var instanceIDPattern = regexp.MustCompile(`^i-(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{17})$`)

// EC2API defines the EC2 operations needed for instance listing and manual
// termination. The method set matches housekeeping.EC2API so the shared
// spot-request cancellation helper takes it directly.
type EC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeSpotInstanceRequests(ctx context.Context, params *ec2.DescribeSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSpotInstanceRequestsOutput, error)
	CancelSpotInstanceRequests(ctx context.Context, params *ec2.CancelSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error)
}

// InstancesDB defines the database operations for checking instance status.
type InstancesDB interface {
	GetPoolBusyInstanceIDs(ctx context.Context, poolName string) ([]string, error)
	GetJobByInstance(ctx context.Context, instanceID string) (*events.JobInfo, error)
	// HasLiveInstanceClaim reports whether an instance is promised to a job. A
	// pool member is claimed before it is started, so for that window it reads
	// stopped while a job already waits on it; the jobs table cannot see that.
	HasLiveInstanceClaim(ctx context.Context, instanceID string) (bool, error)
	MarkInstanceTerminating(ctx context.Context, instanceID string) error
}

// InstanceResponse represents an instance in the admin API response.
type InstanceResponse struct {
	InstanceID   string `json:"instance_id"`
	InstanceType string `json:"instance_type"`
	Pool         string `json:"pool"`
	State        string `json:"state"`
	LaunchTime   string `json:"launch_time,omitempty"`
	PrivateIP    string `json:"private_ip,omitempty"`
	Spot         bool   `json:"spot"`
	Busy         bool   `json:"busy"`
	ImageID      string `json:"image_id,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	// AMIStale reports that this instance is not running what its own
	// architecture's launch template would boot today. False whenever the
	// reference AMI is unknown — see ami_current_unknown on the list response.
	AMIStale bool `json:"ami_stale,omitempty"`
}

// InstanceDetailResponse is the single-instance view: the list fields plus
// placement, image, and full tag set.
type InstanceDetailResponse struct {
	InstanceResponse
	AvailabilityZone string            `json:"availability_zone,omitempty"`
	ImageID          string            `json:"image_id,omitempty"`
	SubnetID         string            `json:"subnet_id,omitempty"`
	Architecture     string            `json:"architecture,omitempty"`
	StateReason      string            `json:"state_reason,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
}

// ActiveJobRef identifies the job occupying an instance.
type ActiveJobRef struct {
	JobID int64  `json:"job_id"`
	RunID int64  `json:"run_id"`
	Repo  string `json:"repo"`
}

// TerminateInstanceResponse reports a completed manual termination.
type TerminateInstanceResponse struct {
	InstanceID string        `json:"instance_id"`
	Pool       string        `json:"pool,omitempty"`
	State      string        `json:"state,omitempty"`
	Forced     bool          `json:"forced"`
	ActiveJob  *ActiveJobRef `json:"active_job,omitempty"`
	Message    string        `json:"message"`
}

// TerminateInstanceConflict is the 409 body when an instance is still serving a
// job. It carries the same error/details fields as ErrorResponse so generic
// error handling keeps working, plus the job the operator needs to see before
// deciding to force.
type TerminateInstanceConflict struct {
	Error     string        `json:"error"`
	Details   string        `json:"details,omitempty"`
	ActiveJob *ActiveJobRef `json:"active_job"`
}

// InstancesHandler provides HTTP endpoints for instance management.
type InstancesHandler struct {
	ec2           EC2API
	db            InstancesDB
	jobsTableName string
	auditDB       AuditDB
	amis          *fleet.AMIResolver
	auth          *AuthMiddleware
	log           *logging.Logger
}

// NewInstancesHandler creates a new instances handler.
func NewInstancesHandler(ec2Client EC2API, db InstancesDB, jobsTableName string, auditDB AuditDB, auth *AuthMiddleware) *InstancesHandler {
	return &InstancesHandler{
		ec2:           ec2Client,
		db:            db,
		jobsTableName: jobsTableName,
		auditDB:       auditDB,
		auth:          auth,
		log:           logging.WithComponent(logging.LogTypeAdmin, "instances"),
	}
}

// RegisterRoutes registers instance API routes on the given mux.
func (h *InstancesHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/instances", h.auth.WrapFunc(h.ListInstances))
	mux.Handle("GET /api/instances/amis", h.auth.WrapFunc(h.CurrentAMIs))
	mux.Handle("GET /api/instances/{instance_id}", h.auth.WrapFunc(h.GetInstance))
	mux.Handle("POST /api/instances/replace-stale", h.auth.WrapFunc(h.ReplaceStaleInstances))
	mux.Handle("DELETE /api/instances/{instance_id}", h.auth.WrapFunc(h.TerminateInstance))
}

// errInstanceNotFound signals that an ID resolved to nothing runs-fleet manages.
var errInstanceNotFound = errors.New("instance not found")

// describeManagedInstance resolves a single runs-fleet-managed instance. The
// managed-tag filter is part of the query rather than a post-hoc check, so an
// unmanaged ID and an unknown ID both come back as errInstanceNotFound.
func (h *InstancesHandler) describeManagedInstance(ctx context.Context, id string) (*types.Instance, error) {
	output, err := h.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{id},
		Filters: []types.Filter{
			{Name: aws.String("tag:runs-fleet:managed"), Values: []string{"true"}},
		},
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidInstanceID.NotFound" {
			return nil, errInstanceNotFound
		}
		return nil, err
	}

	// A DescribeInstances query by unique instance ID returns at most one
	// instance; take the first match and stop.
	for i := range output.Reservations {
		if len(output.Reservations[i].Instances) > 0 {
			return &output.Reservations[i].Instances[0], nil
		}
	}
	return nil, errInstanceNotFound
}

// GetInstance handles GET /api/instances/{instance_id}. Only runs-fleet-managed
// instances are visible: the managed-tag filter means an unmanaged or unknown ID
// both resolve to 404.
func (h *InstancesHandler) GetInstance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("instance_id")
	if !instanceIDPattern.MatchString(id) {
		h.writeError(w, http.StatusBadRequest, "Invalid instance ID", "must match i-<hex>")
		return
	}

	inst, err := h.describeManagedInstance(ctx, id)
	if err != nil {
		if errors.Is(err, errInstanceNotFound) {
			h.writeError(w, http.StatusNotFound, "Instance not found", "")
			return
		}
		h.log.Error(ctx, "failed to describe instance",
			slog.String(logging.KeyInstanceID, id),
			slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to get instance", err.Error())
		return
	}

	pool := getEC2Tag(inst.Tags, "runs-fleet:pool")
	busy := false
	if pool != "" {
		if busyIDs, err := h.db.GetPoolBusyInstanceIDs(ctx, pool); err != nil {
			h.log.Warn(ctx, "failed to get busy instances for pool",
				slog.String(logging.KeyPoolName, pool),
				slog.String(logging.KeyError, err.Error()))
		} else {
			busy = slices.Contains(busyIDs, aws.ToString(inst.InstanceId))
		}
	}

	h.writeJSON(w, http.StatusOK, instanceDetail(inst, pool, busy))
}

func instanceDetail(inst *types.Instance, pool string, busy bool) InstanceDetailResponse {
	resp := InstanceDetailResponse{
		InstanceResponse: InstanceResponse{
			InstanceID:   aws.ToString(inst.InstanceId),
			InstanceType: string(inst.InstanceType),
			Pool:         pool,
			Spot:         inst.InstanceLifecycle == types.InstanceLifecycleTypeSpot,
			Busy:         busy,
		},
		ImageID:      aws.ToString(inst.ImageId),
		SubnetID:     aws.ToString(inst.SubnetId),
		Architecture: string(inst.Architecture),
	}
	if inst.State != nil {
		resp.State = string(inst.State.Name)
	}
	if inst.LaunchTime != nil {
		resp.LaunchTime = inst.LaunchTime.Format("2006-01-02T15:04:05Z")
	}
	if inst.PrivateIpAddress != nil {
		resp.PrivateIP = *inst.PrivateIpAddress
	}
	if inst.Placement != nil {
		resp.AvailabilityZone = aws.ToString(inst.Placement.AvailabilityZone)
	}
	if inst.StateReason != nil {
		resp.StateReason = aws.ToString(inst.StateReason.Message)
	}
	if len(inst.Tags) > 0 {
		resp.Tags = make(map[string]string, len(inst.Tags))
		for _, tag := range inst.Tags {
			resp.Tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
		}
	}
	return resp
}

// TerminateInstance handles DELETE /api/instances/{instance_id}[?force=true].
//
// An instance still serving a job is refused with 409 unless force is set, so a
// mis-click cannot kill a running build. Forcing flips that job to terminating
// (what the spot-interruption path does) but does not re-dispatch it: the GitHub
// job fails when its runner disappears, and re-running is the operator's call.
// Every outcome lands in the persisted audit trail.
func (h *InstancesHandler) TerminateInstance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Without the jobs table the active-job check cannot run, and terminating
	// without it would silently drop the only safeguard this endpoint has.
	if h.jobsTableName == "" {
		h.writeError(w, http.StatusServiceUnavailable, "Jobs table not configured", "the active-job safety check is unavailable")
		return
	}

	id := r.PathValue("instance_id")
	if !instanceIDPattern.MatchString(id) {
		h.writeError(w, http.StatusBadRequest, "Invalid instance ID", "must match i-<hex>")
		return
	}
	force := r.URL.Query().Get("force") == queryTrue

	inst, err := h.describeManagedInstance(ctx, id)
	if err != nil {
		if errors.Is(err, errInstanceNotFound) {
			h.recordTerminateAudit(r, id, "", auditDenied, force, nil, "instance not found or not runs-fleet-managed")
			h.writeError(w, http.StatusNotFound, "Instance not found", "")
			return
		}
		h.log.Error(ctx, "failed to describe instance for termination",
			slog.String(logging.KeyInstanceID, id),
			slog.String(logging.KeyError, err.Error()))
		h.recordTerminateAudit(r, id, "", "error", force, nil, err.Error())
		h.writeError(w, http.StatusInternalServerError, "Failed to look up instance", err.Error())
		return
	}

	pool := getEC2Tag(inst.Tags, "runs-fleet:pool")
	state := ""
	if inst.State != nil {
		state = string(inst.State.Name)
	}

	// Fail closed: a lookup failure must not be mistaken for "no job running".
	job, err := h.db.GetJobByInstance(ctx, id)
	if err != nil {
		h.log.Error(ctx, "failed to check for an active job before termination",
			slog.String(logging.KeyInstanceID, id),
			slog.String(logging.KeyError, err.Error()))
		h.recordTerminateAudit(r, id, pool, "error", force, nil, err.Error())
		h.writeError(w, http.StatusInternalServerError, "Failed to check for an active job", err.Error())
		return
	}

	var activeJob *ActiveJobRef
	if job != nil {
		activeJob = &ActiveJobRef{JobID: job.JobID, RunID: job.RunID, Repo: job.Repo}
	}

	if activeJob != nil && !force {
		details := fmt.Sprintf("job %d (run %d) in %s is still running; retry with force=true to terminate anyway",
			activeJob.JobID, activeJob.RunID, activeJob.Repo)
		h.recordTerminateAudit(r, id, pool, auditDenied, force, activeJob, "instance has an active job")
		h.writeJSON(w, http.StatusConflict, TerminateInstanceConflict{
			Error:     "Instance has an active job",
			Details:   details,
			ActiveJob: activeJob,
		})
		return
	}

	// Cancel first: a persistent spot request would otherwise replace the
	// instance we are about to kill.
	housekeeping.CancelSpotRequestForInstance(ctx, h.ec2, id, h.log)

	if _, err := h.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		h.log.Error(ctx, "failed to terminate instance",
			slog.String(logging.KeyInstanceID, id),
			slog.String(logging.KeyError, err.Error()))
		h.recordTerminateAudit(r, id, pool, "error", force, activeJob, err.Error())
		h.writeError(w, http.StatusInternalServerError, "Failed to terminate instance", err.Error())
		return
	}

	// Mark only after EC2 accepts the terminate. Marking first would strand the job
	// at "terminating" if the terminate then failed, and nothing sweeps that state:
	// FindOrphanedJobCandidates scans running/claiming/launched only, while
	// occupiesInstance counts terminating as busy for maxConcurrencyRuntime, so
	// reconciliation would hold the still-live instance for two hours. In this order
	// a failure leaves the job at running/launched with a dead instance, which the
	// orphaned-jobs sweep is built to reconcile -- so it reports success rather than
	// asking the operator to retry a termination that already happened.
	var markErr error
	if activeJob != nil {
		if markErr = h.db.MarkInstanceTerminating(ctx, id); markErr != nil {
			h.log.Error(ctx, "terminated the instance but failed to mark its job terminating",
				slog.String(logging.KeyInstanceID, id),
				slog.Int64(logging.KeyJobID, activeJob.JobID),
				slog.String(logging.KeyError, markErr.Error()))
		}
	}

	message := "Termination requested"
	reason := ""
	switch {
	case markErr != nil:
		message = fmt.Sprintf("Termination requested; job %d could not be marked terminating and will be reconciled by the orphaned-jobs sweep", activeJob.JobID)
		reason = "job record not updated: " + markErr.Error()
	case activeJob != nil:
		message = fmt.Sprintf("Termination requested; job %d marked terminating", activeJob.JobID)
	}

	h.recordTerminateAudit(r, id, pool, "success", force, activeJob, reason)
	h.writeJSON(w, http.StatusOK, TerminateInstanceResponse{
		InstanceID: id,
		Pool:       pool,
		State:      state,
		Forced:     force,
		ActiveJob:  activeJob,
		Message:    message,
	})
}

func (h *InstancesHandler) recordTerminateAudit(r *http.Request, id, pool, result string, force bool, job *ActiveJobRef, reason string) {
	attrs := []any{
		slog.Bool("forced", force),
		slog.String("pool", pool),
	}
	if job != nil {
		attrs = append(attrs, slog.Int64(logging.KeyJobID, job.JobID), slog.Int64(logging.KeyRunID, job.RunID))
	}
	if reason != "" {
		attrs = append(attrs, slog.String(logging.KeyReason, reason))
	}
	recordAdminAction(r, h.auditDB, "instance.terminate", id, result, attrs...)
}

// ListInstances handles GET /api/instances.
func (h *InstancesHandler) ListInstances(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	poolFilter := q.Get("pool")
	stateFilter := q.Get("state")

	if stateFilter != "" {
		validStates := map[string]bool{
			"pending": true, "running": true, "shutting-down": true,
			"terminated": true, "stopping": true, "stopped": true,
		}
		if !validStates[stateFilter] {
			h.writeError(w, http.StatusBadRequest, "Invalid state filter", fmt.Sprintf("allowed values: pending, running, shutting-down, terminated, stopping, stopped; got %q", stateFilter))
			return
		}
	}

	filters := []types.Filter{
		{
			Name:   aws.String("tag:runs-fleet:managed"),
			Values: []string{"true"},
		},
	}

	if poolFilter != "" {
		filters = append(filters, types.Filter{
			Name:   aws.String("tag:runs-fleet:pool"),
			Values: []string{poolFilter},
		})
	}

	stateValues := []string{"pending", "running", "stopping", "stopped"}
	if stateFilter != "" {
		stateValues = []string{stateFilter}
	}
	filters = append(filters, types.Filter{
		Name:   aws.String("instance-state-name"),
		Values: stateValues,
	})

	var allReservations []types.Reservation
	var nextToken *string
	for {
		output, err := h.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters:   filters,
			NextToken: nextToken,
		})
		if err != nil {
			h.log.Error(ctx, "failed to describe instances", slog.String(logging.KeyError, err.Error()))
			h.writeError(w, http.StatusInternalServerError, "Failed to list instances", err.Error())
			return
		}
		allReservations = append(allReservations, output.Reservations...)
		if output.NextToken == nil {
			break
		}
		nextToken = output.NextToken
	}

	busySet := make(map[string]bool)
	pools := make(map[string]bool)
	for _, res := range allReservations {
		for _, inst := range res.Instances {
			pool := getEC2Tag(inst.Tags, "runs-fleet:pool")
			if pool != "" {
				pools[pool] = true
			}
		}
	}

	var warnings []string
	for pool := range pools {
		busyIDs, err := h.db.GetPoolBusyInstanceIDs(ctx, pool)
		if err != nil {
			h.log.Warn(ctx, "failed to get busy instances for pool",
				slog.String(logging.KeyPoolName, pool),
				slog.String(logging.KeyError, err.Error()))
			warnings = append(warnings, "busy status unavailable for pool: "+pool)
			continue
		}
		for _, id := range busyIDs {
			busySet[id] = true
		}
	}

	// A reference AMI we could not read leaves staleness unanswered rather than
	// answered wrongly: marking the fleet stale on a transient error would
	// invite an operator to replace all of it.
	currentAMIs, amiUnknown := h.referenceAMIs(ctx)

	var instances []InstanceResponse
	for _, res := range allReservations {
		for _, inst := range res.Instances {
			arch := string(inst.Architecture)
			resp := InstanceResponse{
				InstanceID:   aws.ToString(inst.InstanceId),
				InstanceType: string(inst.InstanceType),
				State:        string(inst.State.Name),
				Pool:         getEC2Tag(inst.Tags, "runs-fleet:pool"),
				Spot:         inst.InstanceLifecycle == types.InstanceLifecycleTypeSpot,
				Busy:         busySet[aws.ToString(inst.InstanceId)],
				ImageID:      aws.ToString(inst.ImageId),
				Architecture: arch,
			}
			if ref, ok := currentAMIs[arch]; ok && resp.ImageID != "" {
				resp.AMIStale = resp.ImageID != ref.ImageID
			}
			if inst.LaunchTime != nil {
				resp.LaunchTime = inst.LaunchTime.Format("2006-01-02T15:04:05Z")
			}
			if inst.PrivateIpAddress != nil {
				resp.PrivateIP = *inst.PrivateIpAddress
			}
			instances = append(instances, resp)
		}
	}

	response := map[string]interface{}{
		"instances": instances,
		"total":     len(instances),
	}
	if amiUnknown {
		response["ami_current_unknown"] = true
	}
	if len(warnings) > 0 {
		response["warnings"] = warnings
	}
	h.writeJSON(w, http.StatusOK, response)
}

// referenceAMIs resolves what each architecture would launch today. The second
// return reports that at least one architecture is unknown, so the caller can
// say so instead of implying every instance is current.
func (h *InstancesHandler) referenceAMIs(ctx context.Context) (map[string]fleet.CurrentAMI, bool) {
	if h.amis == nil {
		return nil, true
	}
	current, err := h.amis.Current(ctx)
	if err != nil {
		h.log.Warn(ctx, "failed to resolve launch template AMIs", slog.String(logging.KeyError, err.Error()))
		return nil, true
	}
	return current, len(h.amis.UnresolvedArchs()) > 0
}

func getEC2Tag(tags []types.Tag, key string) string {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == key {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

func (h *InstancesHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	// Response-writer helper with no request/context in scope.
	ctx := context.Background()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(data); err != nil {
		h.log.Error(ctx, "json encode failed", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		h.log.Error(ctx, "write response failed", slog.String(logging.KeyError, err.Error()))
	}
}

func (h *InstancesHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}
