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

// EC2API defines the EC2 operations needed for instance listing.
type EC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// InstancesDB defines the database operations for checking instance status.
type InstancesDB interface {
	GetPoolBusyInstanceIDs(ctx context.Context, poolName string) ([]string, error)
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

// InstancesHandler provides HTTP endpoints for instance management.
type InstancesHandler struct {
	ec2  EC2API
	db   InstancesDB
	auth *AuthMiddleware
	log  *logging.Logger
}

// NewInstancesHandler creates a new instances handler.
func NewInstancesHandler(ec2Client EC2API, db InstancesDB, auth *AuthMiddleware) *InstancesHandler {
	return &InstancesHandler{
		ec2:  ec2Client,
		db:   db,
		auth: auth,
		log:  logging.WithComponent(logging.LogTypeAdmin, "instances"),
	}
}

// RegisterRoutes registers instance API routes on the given mux.
func (h *InstancesHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/instances", h.auth.WrapFunc(h.ListInstances))
	mux.Handle("GET /api/instances/{instance_id}", h.auth.WrapFunc(h.GetInstance))
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

	output, err := h.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{id},
		Filters: []types.Filter{
			{Name: aws.String("tag:runs-fleet:managed"), Values: []string{"true"}},
		},
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidInstanceID.NotFound" {
			h.writeError(w, http.StatusNotFound, "Instance not found", "")
			return
		}
		h.log.Error(ctx, "failed to describe instance",
			slog.String(logging.KeyInstanceID, id),
			slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to get instance", err.Error())
		return
	}

	// A DescribeInstances query by unique instance ID returns at most one
	// instance; take the first match and stop.
	var inst *types.Instance
	for i := range output.Reservations {
		if len(output.Reservations[i].Instances) > 0 {
			inst = &output.Reservations[i].Instances[0]
			break
		}
	}
	if inst == nil {
		h.writeError(w, http.StatusNotFound, "Instance not found", "")
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

	var instances []InstanceResponse
	for _, res := range allReservations {
		for _, inst := range res.Instances {
			resp := InstanceResponse{
				InstanceID:   aws.ToString(inst.InstanceId),
				InstanceType: string(inst.InstanceType),
				State:        string(inst.State.Name),
				Pool:         getEC2Tag(inst.Tags, "runs-fleet:pool"),
				Spot:         inst.InstanceLifecycle == types.InstanceLifecycleTypeSpot,
				Busy:         busySet[aws.ToString(inst.InstanceId)],
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
	if len(warnings) > 0 {
		response["warnings"] = warnings
	}
	h.writeJSON(w, http.StatusOK, response)
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
