package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

const (
	defaultOrphanedJobThreshold = 2 * time.Hour
	minOrphanedJobThreshold     = 10 * time.Minute
	instanceCheckBatchSize      = 100
)

// HousekeepingEC2API defines EC2 operations for housekeeping admin actions.
type HousekeepingEC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// HousekeepingDynamoAPI defines DynamoDB operations for housekeeping admin actions.
type HousekeepingDynamoAPI interface {
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// HousekeepingHandler provides admin endpoints for housekeeping actions.
type HousekeepingHandler struct {
	ec2Client     HousekeepingEC2API
	dynamoClient  HousekeepingDynamoAPI
	jobsTableName string
	auth          *AuthMiddleware
	log           *logging.Logger
}

// NewHousekeepingHandler creates a new housekeeping admin handler.
func NewHousekeepingHandler(ec2Client HousekeepingEC2API, dynamoClient HousekeepingDynamoAPI, jobsTableName string, auth *AuthMiddleware) *HousekeepingHandler {
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

	candidates, err := h.findOrphanedJobCandidates(ctx, threshold)
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

	// Separate candidates into those with instance IDs (need EC2 check) and those without
	var candidatesWithInstance []orphanedJobInfo
	var orphanedJobs []orphanedJobInfo

	for _, c := range candidates {
		if c.InstanceID == "" {
			// Jobs without instance ID (stuck in claiming) are automatically orphaned
			orphanedJobs = append(orphanedJobs, c)
		} else {
			candidatesWithInstance = append(candidatesWithInstance, c)
		}
	}

	// Check EC2 for candidates with instance IDs
	if len(candidatesWithInstance) > 0 {
		existingInstances := h.batchCheckInstances(ctx, candidatesWithInstance)
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
		if err := h.markJobOrphaned(ctx, j.JobID); err != nil {
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

type orphanedJobInfo struct {
	JobID      int64
	InstanceID string
	Status     string
}

func (h *HousekeepingHandler) findOrphanedJobCandidates(ctx context.Context, threshold time.Duration) ([]orphanedJobInfo, error) {
	cutoffTime := time.Now().Add(-threshold).Format(time.RFC3339)

	// Check for jobs in "running" or "claiming" status that are older than threshold
	// Jobs can get stuck in "claiming" if instance creation failed or webhook retry created duplicates
	input := &dynamodb.ScanInput{
		TableName:        aws.String(h.jobsTableName),
		FilterExpression: aws.String("(#status = :running OR #status = :claiming) AND created_at < :cutoff"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":running":  &types.AttributeValueMemberS{Value: "running"},
			":claiming": &types.AttributeValueMemberS{Value: "claiming"},
			":cutoff":   &types.AttributeValueMemberS{Value: cutoffTime},
		},
		ProjectionExpression: aws.String("job_id, instance_id, #status"),
	}

	var candidates []orphanedJobInfo
	var lastEvaluatedKey map[string]types.AttributeValue

	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := h.dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, err
		}

		for _, item := range output.Items {
			var jobID int64
			var instanceID string
			var status string

			if v, ok := item["job_id"].(*types.AttributeValueMemberN); ok {
				if parsed, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
					jobID = parsed
				}
			}
			if v, ok := item["instance_id"].(*types.AttributeValueMemberS); ok {
				instanceID = v.Value
			}
			if v, ok := item["status"].(*types.AttributeValueMemberS); ok {
				status = v.Value
			}

			if jobID == 0 {
				continue
			}

			// Jobs in "claiming" status without instance_id are orphaned (instance creation failed)
			// Jobs in "running" status need instance_id to check if instance exists
			if status == "running" && instanceID == "" {
				continue
			}

			candidates = append(candidates, orphanedJobInfo{
				JobID:      jobID,
				InstanceID: instanceID,
				Status:     status,
			})
		}

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	return candidates, nil
}

func (h *HousekeepingHandler) batchCheckInstances(ctx context.Context, candidates []orphanedJobInfo) map[string]bool {
	instanceSet := make(map[string]struct{})
	for _, c := range candidates {
		instanceSet[c.InstanceID] = struct{}{}
	}

	instanceIDs := make([]string, 0, len(instanceSet))
	for id := range instanceSet {
		instanceIDs = append(instanceIDs, id)
	}

	existing := make(map[string]bool)

	for i := 0; i < len(instanceIDs); i += instanceCheckBatchSize {
		end := i + instanceCheckBatchSize
		if end > len(instanceIDs) {
			end = len(instanceIDs)
		}
		batch := instanceIDs[i:end]

		output, err := h.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: batch,
		})
		if err != nil {
			h.log.Warn("batch describe instances failed, checking individually",
				slog.String(logging.KeyError, err.Error()))
			for _, id := range batch {
				if h.instanceExists(ctx, id) {
					existing[id] = true
				}
			}
			continue
		}

		for _, reservation := range output.Reservations {
			for _, instance := range reservation.Instances {
				if instance.InstanceId == nil {
					continue
				}
				if instance.State != nil && instance.State.Name != ec2types.InstanceStateNameTerminated {
					existing[*instance.InstanceId] = true
				}
			}
		}
	}

	return existing
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

func (h *HousekeepingHandler) markJobOrphaned(ctx context.Context, jobID int64) error {
	now := time.Now().Format(time.RFC3339)

	// Allow marking as orphaned if status is "running" or "claiming"
	_, err := h.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(h.jobsTableName),
		Key: map[string]types.AttributeValue{
			"job_id": &types.AttributeValueMemberN{Value: strconv.FormatInt(jobID, 10)},
		},
		UpdateExpression: aws.String("SET #status = :orphaned, completed_at = :now"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":orphaned": &types.AttributeValueMemberS{Value: "orphaned"},
			":now":      &types.AttributeValueMemberS{Value: now},
			":running":  &types.AttributeValueMemberS{Value: "running"},
			":claiming": &types.AttributeValueMemberS{Value: "claiming"},
		},
		ConditionExpression: aws.String("#status = :running OR #status = :claiming"),
	})

	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil
		}
		return err
	}
	return nil
}

func (h *HousekeepingHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		h.log.Error("json encode failed", slog.String(logging.KeyError, err.Error()))
	}
}

func (h *HousekeepingHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}
