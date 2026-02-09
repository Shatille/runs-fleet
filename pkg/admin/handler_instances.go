package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

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
}

// ListInstances handles GET /api/instances.
func (h *InstancesHandler) ListInstances(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	poolFilter := q.Get("pool")
	stateFilter := q.Get("state")

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
			h.log.Error("failed to describe instances", slog.String(logging.KeyError, err.Error()))
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

	for pool := range pools {
		busyIDs, err := h.db.GetPoolBusyInstanceIDs(ctx, pool)
		if err != nil {
			h.log.Warn("failed to get busy instances for pool",
				slog.String(logging.KeyPoolName, pool),
				slog.String(logging.KeyError, err.Error()))
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

	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"instances": instances,
		"total":     len(instances),
	})
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
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(data); err != nil {
		h.log.Error("json encode failed", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		h.log.Error("write response failed", slog.String(logging.KeyError, err.Error()))
	}
}

func (h *InstancesHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}
