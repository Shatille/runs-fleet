package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/housekeeping"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const (
	// defaultReplaceStaleMax caps one call so a pool is never drained. The pool
	// reconciler replenishes what this terminates, but not instantly.
	defaultReplaceStaleMax = 5
	maxReplaceStaleMax     = 25
)

// ReplaceStaleResponse reports one replace-stale sweep.
type ReplaceStaleResponse struct {
	// Terminated are the instances whose replacements the reconciler will launch
	// on the current AMI.
	Terminated []string `json:"terminated,omitempty"`
	// Busy are stale instances left alone because a job is running on them or
	// they are already claimed for one.
	Busy []string `json:"busy,omitempty"`
	// Running are stale instances left alone because they are running. EC2 does
	// not re-image on start, so a running instance picks up the current AMI when
	// it cycles after its next job; one that never cycles is a hung instance, a
	// different problem with a different fix.
	Running []string `json:"running,omitempty"`
	// Skipped are stale instances the cap left for a later call.
	Skipped []string `json:"skipped,omitempty"`
	Stale   int      `json:"stale"`
	DryRun  bool     `json:"dry_run"`
	Message string   `json:"message"`
}

// ReplaceStaleInstances handles POST /api/instances/replace-stale.
//
// It terminates stopped instances that are not running what their architecture's
// launch template would boot today; the pool reconciler then replaces them on the
// current AMI. Running instances are never terminated here, matching the
// housekeeping sweep: EC2 does not re-image on start, so they pick up the current
// AMI when they cycle after their next job. Stopped candidates are gated on both
// the pool claim and an active job — a claim is written before the instance is
// started, so it is the only signal covering that window. Everything left alone
// is reported, never force-killed; forcing stays a deliberate per-row act.
//
// Query params:
//   - pool: restrict to one pool
//   - max: how many to terminate this call (default 5, max 25)
//   - dry_run: report the targets without touching them
func (h *InstancesHandler) ReplaceStaleInstances(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.jobsTableName == "" {
		h.writeError(w, http.StatusServiceUnavailable, "Jobs table not configured", "the active-job safety check is unavailable")
		return
	}
	if h.amis == nil {
		h.writeError(w, http.StatusServiceUnavailable, "AMI source not configured",
			"without a reference AMI nothing can be called stale")
		return
	}

	q := r.URL.Query()
	dryRun := q.Get("dry_run") == queryTrue
	limit := defaultReplaceStaleMax
	if raw := q.Get("max"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxReplaceStaleMax {
			h.writeError(w, http.StatusBadRequest, "Invalid max",
				"must be a whole number between 1 and "+strconv.Itoa(maxReplaceStaleMax))
			return
		}
		limit = n
	}

	current, err := h.amis.Current(ctx)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, "Failed to read launch templates", err.Error())
		return
	}

	stale, err := h.findStaleInstances(ctx, q.Get("pool"), current)
	if err != nil {
		h.log.Error(ctx, "failed to list instances for replace-stale", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to list instances", err.Error())
		return
	}

	resp := ReplaceStaleResponse{Stale: len(stale), DryRun: dryRun}
	for _, target := range stale {
		// Checked before the cap so instances that can never be terminated do not
		// consume the budget that exists to avoid draining a pool.
		if target.state != string(types.InstanceStateNameStopped) {
			resp.Running = append(resp.Running, target.instanceID)
			continue
		}
		if len(resp.Terminated) >= limit {
			resp.Skipped = append(resp.Skipped, target.instanceID)
			continue
		}
		// The claim precedes the job record, so it is checked first. Fail closed on
		// both: a lookup failure must not be mistaken for "nothing wants this".
		claimed, err := h.db.HasLiveInstanceClaim(ctx, target.instanceID)
		if err != nil {
			h.log.Warn(ctx, "replace-stale skipped an instance: claim check failed",
				slog.String(logging.KeyInstanceID, target.instanceID),
				slog.String(logging.KeyError, err.Error()))
			resp.Busy = append(resp.Busy, target.instanceID)
			continue
		}
		if claimed {
			resp.Busy = append(resp.Busy, target.instanceID)
			continue
		}
		job, err := h.db.GetJobByInstance(ctx, target.instanceID)
		if err != nil {
			h.log.Warn(ctx, "replace-stale skipped an instance: active-job check failed",
				slog.String(logging.KeyInstanceID, target.instanceID),
				slog.String(logging.KeyError, err.Error()))
			resp.Busy = append(resp.Busy, target.instanceID)
			continue
		}
		if job != nil {
			resp.Busy = append(resp.Busy, target.instanceID)
			continue
		}

		if dryRun {
			resp.Terminated = append(resp.Terminated, target.instanceID)
			continue
		}

		// Terminate immediately after confirming this instance rather than batching
		// at the end: batching would stretch the gap between "confirmed unclaimed"
		// and "destroyed" across every other candidate's DynamoDB round trips.
		//
		// Cancel first: a persistent spot request would otherwise replace the
		// instance we are about to kill, on the same stale AMI.
		housekeeping.CancelSpotRequestForInstance(ctx, h.ec2, target.instanceID, h.log)
		if _, err := h.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: []string{target.instanceID},
		}); err != nil {
			h.log.Error(ctx, "failed to terminate stale instance",
				slog.String(logging.KeyInstanceID, target.instanceID),
				slog.String(logging.KeyError, err.Error()))
			// Instances terminated earlier in this loop are already destroyed, so the
			// audit record and the error the operator sees must both name them; a bare
			// failure would report a destructive action as having done nothing.
			recordAdminAction(r, h.auditDB, "instance.replace_stale", target.instanceID, "error",
				slog.String(logging.KeyReason, err.Error()),
				slog.Int("terminated_before_failure", len(resp.Terminated)),
				slog.String("terminated", strings.Join(resp.Terminated, ",")))
			detail := err.Error()
			if len(resp.Terminated) > 0 {
				detail = fmt.Sprintf("%s (already replaced: %s)", detail, strings.Join(resp.Terminated, ", "))
			}
			h.writeError(w, http.StatusInternalServerError, "Failed to terminate stale instances", detail)
			return
		}
		resp.Terminated = append(resp.Terminated, target.instanceID)
	}

	if dryRun {
		resp.Message = fmt.Sprintf("Dry run: would replace %d of %d stale instance(s)", len(resp.Terminated), resp.Stale)
		h.writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.Message = fmt.Sprintf("Replacing %d of %d stale instance(s)", len(resp.Terminated), resp.Stale)
	recordAdminAction(r, h.auditDB, "instance.replace_stale", strings.Join(resp.Terminated, ","), "success",
		slog.Int("stale", resp.Stale),
		slog.Int("terminated", len(resp.Terminated)),
		slog.Int("busy", len(resp.Busy)),
		slog.Int("running", len(resp.Running)),
		slog.Int("skipped", len(resp.Skipped)))
	h.writeJSON(w, http.StatusOK, resp)
}

// staleTarget is a stale pool member paired with the EC2 state that decides
// whether it can be replaced at all.
type staleTarget struct {
	instanceID string
	state      string
}

// findStaleInstances lists managed instances that are not on their
// architecture's current AMI. Only pool members are returned: a cold-start
// instance is ephemeral, so replacing it buys nothing and there is no pool to
// launch the replacement.
//
// Running instances are included so the caller can report them, even though it
// will not terminate them; excluding them here would leave the stale count
// unexplained.
func (h *InstancesHandler) findStaleInstances(ctx context.Context, pool string, current map[string]fleet.CurrentAMI) ([]staleTarget, error) {
	filters := []types.Filter{
		{Name: aws.String("tag:runs-fleet:managed"), Values: []string{"true"}},
		{Name: aws.String("instance-state-name"), Values: []string{
			string(types.InstanceStateNameRunning),
			string(types.InstanceStateNameStopped),
		}},
	}
	if pool != "" {
		filters = append(filters, types.Filter{Name: aws.String("tag:runs-fleet:pool"), Values: []string{pool}})
	}

	input := &ec2.DescribeInstancesInput{Filters: filters}
	var stale []staleTarget
	for {
		output, err := h.ec2.DescribeInstances(ctx, input)
		if err != nil {
			return nil, err
		}
		for _, res := range output.Reservations {
			for _, inst := range res.Instances {
				if getEC2Tag(inst.Tags, "runs-fleet:pool") == "" {
					continue
				}
				image := aws.ToString(inst.ImageId)
				ref, ok := current[string(inst.Architecture)]
				if !ok || image == "" || image == ref.ImageID {
					continue
				}
				// An unreadable state must not read as stopped: leaving it empty
				// sorts the instance into the never-terminated path.
				var state string
				if inst.State != nil {
					state = string(inst.State.Name)
				}
				stale = append(stale, staleTarget{instanceID: aws.ToString(inst.InstanceId), state: state})
			}
		}
		if output.NextToken == nil {
			break
		}
		input.NextToken = output.NextToken
	}
	return stale, nil
}
