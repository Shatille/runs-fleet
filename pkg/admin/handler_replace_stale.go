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
	// Busy are stale instances left alone because a job is running on them.
	Busy []string `json:"busy,omitempty"`
	// Skipped are stale instances the cap left for a later call.
	Skipped []string `json:"skipped,omitempty"`
	Stale   int      `json:"stale"`
	DryRun  bool     `json:"dry_run"`
	Message string   `json:"message"`
}

// ReplaceStaleInstances handles POST /api/instances/replace-stale.
//
// It terminates instances that are not running what their architecture's launch
// template would boot today; the pool reconciler then replaces them on the
// current AMI. Instances with a job running are reported, never force-killed —
// forcing stays a deliberate per-row act.
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
	for _, id := range stale {
		if len(resp.Terminated) >= limit {
			resp.Skipped = append(resp.Skipped, id)
			continue
		}
		// Fail closed: a lookup failure must not be mistaken for "no job running".
		job, err := h.db.GetJobByInstance(ctx, id)
		if err != nil {
			h.log.Warn(ctx, "replace-stale skipped an instance: active-job check failed",
				slog.String(logging.KeyInstanceID, id),
				slog.String(logging.KeyError, err.Error()))
			resp.Busy = append(resp.Busy, id)
			continue
		}
		if job != nil {
			resp.Busy = append(resp.Busy, id)
			continue
		}
		resp.Terminated = append(resp.Terminated, id)
	}

	if dryRun {
		resp.Message = fmt.Sprintf("Dry run: would replace %d of %d stale instance(s)", len(resp.Terminated), resp.Stale)
		h.writeJSON(w, http.StatusOK, resp)
		return
	}

	if len(resp.Terminated) > 0 {
		for _, id := range resp.Terminated {
			// Cancel first: a persistent spot request would otherwise replace the
			// instance we are about to kill, on the same stale AMI.
			housekeeping.CancelSpotRequestForInstance(ctx, h.ec2, id, h.log)
		}
		if _, err := h.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: resp.Terminated}); err != nil {
			h.log.Error(ctx, "failed to terminate stale instances", slog.String(logging.KeyError, err.Error()))
			recordAdminAction(r, h.auditDB, "instance.replace_stale", strings.Join(resp.Terminated, ","), "error",
				slog.String(logging.KeyReason, err.Error()))
			h.writeError(w, http.StatusInternalServerError, "Failed to terminate stale instances", err.Error())
			return
		}
	}

	resp.Message = fmt.Sprintf("Replacing %d of %d stale instance(s)", len(resp.Terminated), resp.Stale)
	recordAdminAction(r, h.auditDB, "instance.replace_stale", strings.Join(resp.Terminated, ","), "success",
		slog.Int("stale", resp.Stale),
		slog.Int("terminated", len(resp.Terminated)),
		slog.Int("busy", len(resp.Busy)),
		slog.Int("skipped", len(resp.Skipped)))
	h.writeJSON(w, http.StatusOK, resp)
}

// findStaleInstances lists managed instances that are not on their
// architecture's current AMI. Only pool members are returned: a cold-start
// instance is ephemeral, so replacing it buys nothing and there is no pool to
// launch the replacement.
func (h *InstancesHandler) findStaleInstances(ctx context.Context, pool string, current map[string]fleet.CurrentAMI) ([]string, error) {
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
	var stale []string
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
				stale = append(stale, aws.ToString(inst.InstanceId))
			}
		}
		if output.NextToken == nil {
			break
		}
		input.NextToken = output.NextToken
	}
	return stale, nil
}
