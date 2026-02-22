package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// EC2API defines EC2 operations for housekeeping.
type EC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeSpotInstanceRequests(ctx context.Context, params *ec2.DescribeSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSpotInstanceRequestsOutput, error)
	CancelSpotInstanceRequests(ctx context.Context, params *ec2.CancelSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error)
}

// DynamoDBAPI defines DynamoDB operations for housekeeping.
type DynamoDBAPI interface {
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	BatchWriteItem(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// MetricsAPI defines CloudWatch metrics publishing.
type MetricsAPI interface {
	PublishOrphanedInstancesTerminated(ctx context.Context, count int) error
	PublishSSMParametersDeleted(ctx context.Context, count int) error
	PublishJobRecordsArchived(ctx context.Context, count int) error
	PublishOrphanedJobsCleanedUp(ctx context.Context, count int) error
	PublishStaleJobsReconciled(ctx context.Context, count int) error
	PublishPoolUtilization(ctx context.Context, poolName string, utilization float64) error
}

// CostReporter generates cost reports.
type CostReporter interface {
	GenerateDailyReport(ctx context.Context) error
}

// GitHubJobStatus represents the status of a job as reported by GitHub.
type GitHubJobStatus struct {
	Completed  bool
	Conclusion string // "success", "failure", "cancelled", "timed_out", etc.
}

// GitHubJobChecker queries GitHub for workflow job status.
type GitHubJobChecker interface {
	GetWorkflowJobStatus(ctx context.Context, owner, repo string, jobID int64) (*GitHubJobStatus, error)
}

// SQSAPI defines SQS operations for housekeeping.
type SQSAPI interface {
	GetQueueAttributes(ctx context.Context, params *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	StartMessageMoveTask(ctx context.Context, params *sqs.StartMessageMoveTaskInput, optFns ...func(*sqs.Options)) (*sqs.StartMessageMoveTaskOutput, error)
}

// PoolConfig represents pool configuration for housekeeping.
type PoolConfig struct {
	PoolName    string
	Ephemeral   bool
	LastJobTime time.Time
}

// PoolDBAPI defines pool database operations for housekeeping.
type PoolDBAPI interface {
	ListPools(ctx context.Context) ([]string, error)
	GetPoolConfig(ctx context.Context, poolName string) (*PoolConfig, error)
	DeletePoolConfig(ctx context.Context, poolName string) error
}

// Tasks implements housekeeping task execution.
type Tasks struct {
	ec2Client      EC2API
	secretsStore   secrets.Store
	dynamoClient   DynamoDBAPI
	sqsClient      SQSAPI
	poolDB         PoolDBAPI
	gitHubChecker  GitHubJobChecker
	metrics        MetricsAPI
	costReporter   CostReporter
	config         *config.Config
	log            *logging.Logger
}

// logger returns the logger, using a default if not initialized.
func (t *Tasks) logger() *logging.Logger {
	if t.log == nil {
		return logging.WithComponent(logging.LogTypeHousekeep, "tasks")
	}
	return t.log
}

// NewTasks creates a new Tasks executor.
func NewTasks(cfg aws.Config, appConfig *config.Config, secretsStore secrets.Store, metrics MetricsAPI, costReporter CostReporter) *Tasks {
	return &Tasks{
		ec2Client:    ec2.NewFromConfig(cfg),
		secretsStore: secretsStore,
		dynamoClient: dynamodb.NewFromConfig(cfg),
		sqsClient:    sqs.NewFromConfig(cfg),
		metrics:      metrics,
		costReporter: costReporter,
		config:       appConfig,
		log:          logging.WithComponent(logging.LogTypeHousekeep, "tasks"),
	}
}

// ExecuteOrphanedInstances detects and terminates orphaned instances.
// Uses two detection strategies:
//  1. Tag-based: finds tagged instances (runs-fleet:managed=true) that exceeded max runtime
//  2. Profile-based: finds ANY instance using the runner IAM profile that exceeded max runtime,
//     catching untagged zombies from persistent spot tag propagation failures
func (t *Tasks) ExecuteOrphanedInstances(ctx context.Context) error {
	maxRuntime := time.Duration(t.config.MaxRuntimeMinutes+10) * time.Minute
	cutoffTime := time.Now().Add(-maxRuntime)

	// Phase 1: Tag-based detection (fast, targeted)
	taggedOrphans, tagErr := t.findOrphansByTag(ctx, cutoffTime)

	// Phase 2: Profile-based detection (catches untagged zombies from persistent spot tag failures)
	untaggedOrphans := t.findOrphansByProfile(ctx, cutoffTime)

	orphanedIDs := mergeUniqueIDs(taggedOrphans, untaggedOrphans)

	// If tag-based detection failed and profile-based found nothing, propagate the error
	if tagErr != nil && len(orphanedIDs) == 0 {
		return tagErr
	}
	if len(orphanedIDs) == 0 {
		t.logger().Debug("no orphaned instances found")
		return nil
	}

	if len(untaggedOrphans) > 0 {
		t.logger().Warn("found untagged orphaned instances (persistent spot tag propagation failure)",
			slog.Int("untagged_count", len(untaggedOrphans)),
			slog.Any("untagged_ids", untaggedOrphans))
	}

	// Cancel persistent spot requests before termination to prevent zombie resurrection
	t.cancelSpotRequestsForInstances(ctx, orphanedIDs)

	t.logger().Info("terminating orphaned instances", slog.Int(logging.KeyCount, len(orphanedIDs)))

	_, err := t.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: orphanedIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate orphaned instances: %w", err)
	}

	if t.metrics != nil {
		_ = t.metrics.PublishOrphanedInstancesTerminated(ctx, len(orphanedIDs))
	}

	t.logger().Info("orphaned instances terminated",
		slog.Int(logging.KeyCount, len(orphanedIDs)),
		slog.Any("instance_ids", orphanedIDs))
	return nil
}

func (t *Tasks) findOrphansByTag(ctx context.Context, cutoffTime time.Time) ([]string, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:runs-fleet:managed"), Values: []string{"true"}},
			{Name: aws.String("instance-state-name"), Values: []string{"running", "pending"}},
		},
	}

	var allOrphans []string
	for {
		output, err := t.ec2Client.DescribeInstances(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to describe instances: %w", err)
		}
		allOrphans = append(allOrphans, extractOrphanIDs(output, cutoffTime)...)
		if output.NextToken == nil {
			break
		}
		input.NextToken = output.NextToken
	}
	return allOrphans, nil
}

// findOrphansByProfile detects untagged zombie instances via IAM instance profile.
// Includes "stopped" state because untagged stopped instances are invisible to pool
// reconciliation and will never be started or cleaned up — they leak EBS costs.
// Tagged stopped instances are excluded (they're legitimate warm pool members).
func (t *Tasks) findOrphansByProfile(ctx context.Context, cutoffTime time.Time) []string {
	if t.config.InstanceProfileARN == "" {
		return nil
	}

	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("iam-instance-profile.arn"), Values: []string{t.config.InstanceProfileARN}},
			{Name: aws.String("instance-state-name"), Values: []string{"running", "pending", "stopped"}},
		},
	}

	var orphanIDs []string
	for {
		output, err := t.ec2Client.DescribeInstances(ctx, input)
		if err != nil {
			t.logger().Error("failed to describe profile-based instances", slog.String("error", err.Error()))
			return orphanIDs
		}

		for _, reservation := range output.Reservations {
			for _, instance := range reservation.Instances {
				if instance.LaunchTime == nil || !instance.LaunchTime.Before(cutoffTime) || instance.InstanceId == nil {
					continue
				}
				hasManaged := false
				for _, tag := range instance.Tags {
					if aws.ToString(tag.Key) == "runs-fleet:managed" {
						hasManaged = true
						break
					}
				}
				if !hasManaged {
					orphanIDs = append(orphanIDs, *instance.InstanceId)
				}
			}
		}

		if output.NextToken == nil {
			break
		}
		input.NextToken = output.NextToken
	}
	return orphanIDs
}

const maxInstanceIDsPerSpotQuery = 200

func (t *Tasks) cancelSpotRequestsForInstances(ctx context.Context, instanceIDs []string) {
	if len(instanceIDs) == 0 {
		return
	}

	var spotRequestIDs []string
	for i := 0; i < len(instanceIDs); i += maxInstanceIDsPerSpotQuery {
		end := min(i+maxInstanceIDsPerSpotQuery, len(instanceIDs))
		batch := instanceIDs[i:end]

		output, err := t.describeSpotRequestsWithRetry(ctx, batch)
		if err != nil {
			t.logger().Error("failed to describe spot requests for orphaned instances after retry",
				slog.String("error", err.Error()),
				slog.Int("batch_size", len(batch)))
			continue
		}

		for _, req := range output.SpotInstanceRequests {
			if req.SpotInstanceRequestId != nil {
				spotRequestIDs = append(spotRequestIDs, *req.SpotInstanceRequestId)
			}
		}
	}

	if len(spotRequestIDs) == 0 {
		return
	}

	const cancelBatchSize = 100
	var cancelledCount int
	for i := 0; i < len(spotRequestIDs); i += cancelBatchSize {
		end := min(i+cancelBatchSize, len(spotRequestIDs))
		batch := spotRequestIDs[i:end]

		if err := t.cancelSpotRequestsWithRetry(ctx, batch); err != nil {
			t.logger().Error("failed to cancel spot requests for orphaned instances after retry",
				slog.String("error", err.Error()),
				slog.Int("batch_size", len(batch)),
				slog.Any("spot_request_ids", batch))
			continue
		}
		cancelledCount += len(batch)
	}

	if cancelledCount > 0 {
		t.logger().Info("cancelled spot requests for orphaned instances",
			slog.Int("count", cancelledCount))
	}
}

func (t *Tasks) describeSpotRequestsWithRetry(ctx context.Context, instanceIDs []string) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	input := &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("instance-id"), Values: instanceIDs},
			{Name: aws.String("state"), Values: []string{"open", "active", "disabled"}},
		},
	}
	output, err := t.ec2Client.DescribeSpotInstanceRequests(ctx, input)
	if err == nil {
		return output, nil
	}
	t.logger().Warn("describe spot requests failed, retrying",
		slog.String("error", err.Error()),
		slog.Int("batch_size", len(instanceIDs)))
	time.Sleep(2 * time.Second)
	return t.ec2Client.DescribeSpotInstanceRequests(ctx, input)
}

func (t *Tasks) cancelSpotRequestsWithRetry(ctx context.Context, spotRequestIDs []string) error {
	input := &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: spotRequestIDs,
	}
	_, err := t.ec2Client.CancelSpotInstanceRequests(ctx, input)
	if err == nil {
		return nil
	}
	t.logger().Warn("cancel spot requests failed, retrying",
		slog.String("error", err.Error()),
		slog.Int("batch_size", len(spotRequestIDs)))
	time.Sleep(2 * time.Second)
	_, err = t.ec2Client.CancelSpotInstanceRequests(ctx, input)
	return err
}

func extractOrphanIDs(output *ec2.DescribeInstancesOutput, cutoffTime time.Time) []string {
	var ids []string
	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
			if instance.LaunchTime != nil && instance.LaunchTime.Before(cutoffTime) && instance.InstanceId != nil {
				ids = append(ids, *instance.InstanceId)
			}
		}
	}
	return ids
}

func mergeUniqueIDs(lists ...[]string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, list := range lists {
		for _, id := range list {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				result = append(result, id)
			}
		}
	}
	return result
}

// ExecuteStaleSecrets cleans up stale runner secrets.
func (t *Tasks) ExecuteStaleSecrets(ctx context.Context) error {
	if t.secretsStore == nil {
		return nil
	}

	runnerIDs, err := t.secretsStore.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list runner configs: %w", err)
	}

	var deletedCount int
	for _, runnerID := range runnerIDs {
		exists, err := t.instanceExists(ctx, runnerID)
		if err != nil {
			continue
		}

		if !exists {
			if err := t.secretsStore.Delete(ctx, runnerID); err == nil {
				deletedCount++
			}
		}
	}

	if t.metrics != nil && deletedCount > 0 {
		_ = t.metrics.PublishSSMParametersDeleted(ctx, deletedCount)
	}

	if deletedCount > 0 {
		t.logger().Info("stale secrets deleted", slog.Int(logging.KeyCount, deletedCount))
	}
	return nil
}

// instanceTerminationGracePeriod is the minimum time an instance must be terminated
// before we consider it safe to delete its associated SSM parameters.
// This prevents race conditions where parameters are deleted for recently terminated
// instances that may still be processing cleanup operations.
const instanceTerminationGracePeriod = 10 * time.Minute

// instanceExists checks if an EC2 instance exists and is not in a terminated state.
// For terminated instances, it requires them to have been terminated for at least
// instanceTerminationGracePeriod before considering them non-existent.
func (t *Tasks) instanceExists(ctx context.Context, instanceID string) (bool, error) {
	output, err := t.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		if strings.Contains(err.Error(), "InvalidInstanceID") {
			return false, nil
		}
		return false, err
	}

	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
			if instance.State != nil {
				state := instance.State.Name
				if state != ec2types.InstanceStateNameTerminated {
					return true, nil
				}

				if instance.StateTransitionReason != nil {
					if instance.LaunchTime != nil {
						if time.Since(*instance.LaunchTime) < instanceTerminationGracePeriod {
							return true, nil
						}
					}
				}
			}
		}
	}

	return false, nil
}

// ExecuteOldJobs archives or deletes old job records.
func (t *Tasks) ExecuteOldJobs(ctx context.Context) error {
	if t.config.JobsTableName == "" {
		return nil
	}

	cutoffTime := time.Now().AddDate(0, 0, -7).Format(time.RFC3339)

	input := &dynamodb.ScanInput{
		TableName:        aws.String(t.config.JobsTableName),
		FilterExpression: aws.String("completed_at < :cutoff"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":cutoff": &types.AttributeValueMemberS{Value: cutoffTime},
		},
		ProjectionExpression: aws.String("job_id"),
	}

	var jobsToDelete []map[string]types.AttributeValue
	var lastEvaluatedKey map[string]types.AttributeValue

	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := t.dynamoClient.Scan(ctx, input)
		if err != nil {
			return fmt.Errorf("failed to scan jobs: %w", err)
		}

		jobsToDelete = append(jobsToDelete, output.Items...)

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	if len(jobsToDelete) == 0 {
		return nil
	}

	deletedCount := 0
	for i := 0; i < len(jobsToDelete); i += 25 {
		end := i + 25
		if end > len(jobsToDelete) {
			end = len(jobsToDelete)
		}

		batch := jobsToDelete[i:end]
		writeRequests := make([]types.WriteRequest, len(batch))

		for j, item := range batch {
			writeRequests[j] = types.WriteRequest{
				DeleteRequest: &types.DeleteRequest{
					Key: item,
				},
			}
		}

		_, err := t.dynamoClient.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{
				t.config.JobsTableName: writeRequests,
			},
		})
		if err != nil {
			continue
		}

		deletedCount += len(batch)
	}

	if t.metrics != nil && deletedCount > 0 {
		_ = t.metrics.PublishJobRecordsArchived(ctx, deletedCount)
	}

	if deletedCount > 0 {
		t.logger().Info("old job records deleted", slog.Int(logging.KeyCount, deletedCount))
	}
	return nil
}

// orphanedJobThreshold is the minimum age for running jobs to be considered orphaned.
// Jobs older than this with non-existent instances are marked as orphaned.
// 2 hours exceeds max job runtime (MaxRuntimeMinutes + 10) and allows for spot
// interruption re-queue delays, ensuring legitimate jobs aren't prematurely orphaned.
const orphanedJobThreshold = 2 * time.Hour

// orphanedJobCandidate holds job info for orphaned job cleanup.
type orphanedJobCandidate struct {
	JobID      int64
	InstanceID string
	Status     string
}

// ExecuteOrphanedJobs cleans up jobs with status "running" whose instances no longer exist.
// This prevents stale job records from inflating pool busy counts and causing infinite scaling.
//
// Note: Uses DynamoDB Scan which is O(n) on table size. For tables with >100k items,
// consider adding GSI on (status, created_at) for more efficient queries.
func (t *Tasks) ExecuteOrphanedJobs(ctx context.Context) error {
	if t.config.JobsTableName == "" {
		return nil
	}

	cutoffTime := time.Now().Add(-orphanedJobThreshold).Format(time.RFC3339)

	// Check for jobs in "running" or "claiming" status that are older than threshold
	// Jobs can get stuck in "claiming" if instance creation failed or webhook retry created duplicates
	input := &dynamodb.ScanInput{
		TableName:        aws.String(t.config.JobsTableName),
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

	// Collect all candidate jobs first
	var candidates []orphanedJobCandidate
	var lastEvaluatedKey map[string]types.AttributeValue

	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := t.dynamoClient.Scan(ctx, input)
		if err != nil {
			return fmt.Errorf("failed to scan jobs: %w", err)
		}

		for _, item := range output.Items {
			var jobID int64
			var instanceID string
			var status string

			if v, ok := item["job_id"].(*types.AttributeValueMemberN); ok {
				parsed, err := strconv.ParseInt(v.Value, 10, 64)
				if err != nil {
					continue
				}
				jobID = parsed
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

			// Jobs in "running" status need instance_id to check if instance exists
			// Jobs in "claiming" status without instance_id are orphaned (instance creation failed)
			if status == "running" && instanceID == "" {
				continue
			}

			candidates = append(candidates, orphanedJobCandidate{
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

	if len(candidates) == 0 {
		return nil
	}

	// Separate candidates into those with instance IDs (need EC2 check) and those without
	var candidatesWithInstance []orphanedJobCandidate
	var orphanedCandidates []orphanedJobCandidate

	for _, c := range candidates {
		if c.InstanceID == "" {
			// Jobs without instance ID (stuck in claiming) are automatically orphaned
			orphanedCandidates = append(orphanedCandidates, c)
		} else {
			candidatesWithInstance = append(candidatesWithInstance, c)
		}
	}

	// Batch check instance existence for candidates with instance IDs
	if len(candidatesWithInstance) > 0 {
		existingInstances := t.batchCheckInstances(ctx, candidatesWithInstance)
		for _, c := range candidatesWithInstance {
			if !existingInstances[c.InstanceID] {
				orphanedCandidates = append(orphanedCandidates, c)
			}
		}
	}

	// Mark orphaned jobs
	var orphanedCount int
	for _, c := range orphanedCandidates {

		if err := t.markJobOrphaned(ctx, c.JobID); err != nil {
			t.logger().Error("mark job orphaned failed",
				slog.Int64("job_id", c.JobID),
				slog.String("error", err.Error()))
			continue
		}
		orphanedCount++
		t.logger().Info("orphaned job cleaned up",
			slog.Int64("job_id", c.JobID),
			slog.String("instance_id", c.InstanceID))
	}

	if t.metrics != nil && orphanedCount > 0 {
		_ = t.metrics.PublishOrphanedJobsCleanedUp(ctx, orphanedCount)
	}

	if orphanedCount > 0 {
		t.logger().Info("orphaned jobs cleaned up", slog.Int(logging.KeyCount, orphanedCount))
	}
	return nil
}

// batchCheckInstances checks multiple instances in batches and returns a map of existing instance IDs.
// Uses batched DescribeInstances calls to reduce API overhead.
func (t *Tasks) batchCheckInstances(ctx context.Context, candidates []orphanedJobCandidate) map[string]bool {
	// Collect unique instance IDs
	instanceSet := make(map[string]struct{})
	for _, c := range candidates {
		instanceSet[c.InstanceID] = struct{}{}
	}

	instanceIDs := make([]string, 0, len(instanceSet))
	for id := range instanceSet {
		instanceIDs = append(instanceIDs, id)
	}

	existing := make(map[string]bool)
	const batchSize = 100 // EC2 recommends batches of 100

	for i := 0; i < len(instanceIDs); i += batchSize {
		end := i + batchSize
		if end > len(instanceIDs) {
			end = len(instanceIDs)
		}
		batch := instanceIDs[i:end]

		output, err := t.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: batch,
		})
		if err != nil {
			// On error, check individually to handle InvalidInstanceID errors
			for _, id := range batch {
				exists, _ := t.instanceExists(ctx, id)
				existing[id] = exists
			}
			continue
		}

		for _, reservation := range output.Reservations {
			for _, instance := range reservation.Instances {
				if instance.InstanceId == nil || instance.State == nil {
					continue
				}
				state := instance.State.Name
				if state != ec2types.InstanceStateNameTerminated {
					existing[*instance.InstanceId] = true
				} else if instance.LaunchTime != nil && time.Since(*instance.LaunchTime) < instanceTerminationGracePeriod {
					existing[*instance.InstanceId] = true
				}
			}
		}
	}

	return existing
}

// markJobOrphaned updates a job's status to "orphaned" if it's still running or claiming.
// Uses conditional write to prevent race conditions with concurrent job completions.
func (t *Tasks) markJobOrphaned(ctx context.Context, jobID int64) error {
	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": jobID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	// Allow marking as orphaned if status is "running" or "claiming"
	_, err = t.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(t.config.JobsTableName),
		Key:                 key,
		UpdateExpression:    aws.String("SET #status = :orphaned, completed_at = :completed_at"),
		ConditionExpression: aws.String("#status = :running OR #status = :claiming"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":orphaned":     &types.AttributeValueMemberS{Value: "orphaned"},
			":running":      &types.AttributeValueMemberS{Value: "running"},
			":claiming":     &types.AttributeValueMemberS{Value: "claiming"},
			":completed_at": &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
		},
	})
	if err != nil {
		// ConditionalCheckFailedException means job status changed (completed normally)
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil
		}
		return fmt.Errorf("failed to update job: %w", err)
	}

	return nil
}

// ExecutePoolAudit generates pool utilization reports.
func (t *Tasks) ExecutePoolAudit(ctx context.Context) error {
	if t.config.PoolsTableName == "" {
		return nil
	}

	output, err := t.dynamoClient.Scan(ctx, &dynamodb.ScanInput{
		TableName: aws.String(t.config.PoolsTableName),
	})
	if err != nil {
		return fmt.Errorf("failed to scan pools: %w", err)
	}

	for _, item := range output.Items {
		poolName := ""
		desiredRunning := 0
		currentRunning := 0

		if v, ok := item["pool_name"].(*types.AttributeValueMemberS); ok {
			poolName = v.Value
		}
		if v, ok := item["desired_running"].(*types.AttributeValueMemberN); ok {
			_, _ = fmt.Sscanf(v.Value, "%d", &desiredRunning)
		}
		if v, ok := item["current_running"].(*types.AttributeValueMemberN); ok {
			_, _ = fmt.Sscanf(v.Value, "%d", &currentRunning)
		}

		if poolName == "" || desiredRunning == 0 {
			continue
		}

		utilization := float64(currentRunning) / float64(desiredRunning) * 100

		if t.metrics != nil {
			_ = t.metrics.PublishPoolUtilization(ctx, poolName, utilization)
		}
	}

	return nil
}

// ExecuteCostReport generates the daily cost report.
func (t *Tasks) ExecuteCostReport(ctx context.Context) error {
	if isNilInterface(t.costReporter) {
		return nil
	}

	return t.costReporter.GenerateDailyReport(ctx)
}

func isNilInterface(i interface{}) bool {
	if i == nil {
		return true
	}
	v := reflect.ValueOf(i)
	return v.Kind() == reflect.Ptr && v.IsNil()
}

// ExecuteDLQRedrive moves messages from the DLQ back to the main queue.
func (t *Tasks) ExecuteDLQRedrive(ctx context.Context) error {
	if t.config.QueueDLQURL == "" || t.config.QueueURL == "" {
		return nil
	}

	attrs, err := t.sqsClient.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(t.config.QueueDLQURL),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameQueueArn,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get DLQ attributes: %w", err)
	}

	msgCount := attrs.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)]
	if msgCount == "0" {
		return nil
	}

	dlqArn := attrs.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
	if dlqArn == "" {
		return fmt.Errorf("failed to get DLQ ARN")
	}

	mainAttrs, err := t.sqsClient.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(t.config.QueueURL),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameQueueArn,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get main queue attributes: %w", err)
	}

	mainQueueArn := mainAttrs.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
	if mainQueueArn == "" {
		return fmt.Errorf("failed to get main queue ARN")
	}

	output, err := t.sqsClient.StartMessageMoveTask(ctx, &sqs.StartMessageMoveTaskInput{
		SourceArn:      aws.String(dlqArn),
		DestinationArn: aws.String(mainQueueArn),
	})
	if err != nil {
		return fmt.Errorf("failed to start message move task: %w", err)
	}

	t.logger().Info("dlq redrive started",
		slog.String("message_count", msgCount),
		slog.String("task_handle", aws.ToString(output.TaskHandle)))
	return nil
}

// SetPoolDB sets the pool database client for ephemeral pool cleanup.
func (t *Tasks) SetPoolDB(poolDB PoolDBAPI) {
	t.poolDB = poolDB
}

// SetGitHubJobChecker sets the GitHub job checker for stale job detection.
func (t *Tasks) SetGitHubJobChecker(checker GitHubJobChecker) {
	t.gitHubChecker = checker
}

// staleJobThreshold is the minimum age for running/claiming jobs to be checked against GitHub.
// Jobs younger than this are likely still legitimately starting up.
const staleJobThreshold = 10 * time.Minute

// maxStaleJobChecks limits GitHub API calls per cycle to stay within rate limits.
// At 5-min intervals: 30 * 12 = 360 calls/hour, well within 5000/hour.
const maxStaleJobChecks = 30

// staleJobCandidate holds job info for stale job detection.
type staleJobCandidate struct {
	JobID  int64
	Repo   string
	Status string
}

// ExecuteStaleJobs detects jobs stuck in running/claiming by querying GitHub API for actual status.
// Resolves stale jobs in ~10 minutes instead of waiting for the 6h orphan detection window.
func (t *Tasks) ExecuteStaleJobs(ctx context.Context) error {
	if t.config.JobsTableName == "" || t.gitHubChecker == nil {
		return nil
	}

	cutoffTime := time.Now().Add(-staleJobThreshold).Format(time.RFC3339)

	input := &dynamodb.ScanInput{
		TableName:        aws.String(t.config.JobsTableName),
		FilterExpression: aws.String("(#status = :running OR #status = :claiming) AND created_at < :cutoff"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":running":  &types.AttributeValueMemberS{Value: "running"},
			":claiming": &types.AttributeValueMemberS{Value: "claiming"},
			":cutoff":   &types.AttributeValueMemberS{Value: cutoffTime},
		},
		ProjectionExpression: aws.String("job_id, repo, #status"),
	}

	var candidates []staleJobCandidate
	var lastEvaluatedKey map[string]types.AttributeValue

	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := t.dynamoClient.Scan(ctx, input)
		if err != nil {
			return fmt.Errorf("failed to scan jobs: %w", err)
		}

		for _, item := range output.Items {
			var jobID int64
			var repo, status string

			if v, ok := item["job_id"].(*types.AttributeValueMemberN); ok {
				parsed, err := strconv.ParseInt(v.Value, 10, 64)
				if err != nil {
					continue
				}
				jobID = parsed
			}
			if v, ok := item["repo"].(*types.AttributeValueMemberS); ok {
				repo = v.Value
			}
			if v, ok := item["status"].(*types.AttributeValueMemberS); ok {
				status = v.Value
			}

			if jobID == 0 || repo == "" {
				continue
			}

			candidates = append(candidates, staleJobCandidate{
				JobID:  jobID,
				Repo:   repo,
				Status: status,
			})
		}

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	checked := 0
	reconciled := 0
	for _, c := range candidates {
		if checked >= maxStaleJobChecks {
			t.logger().Info("stale jobs check limit reached",
				slog.Int("checked", checked),
				slog.Int("remaining", len(candidates)-checked))
			break
		}

		owner, _, ok := splitRepo(c.Repo)
		if !ok {
			continue
		}

		checked++
		ghStatus, err := t.gitHubChecker.GetWorkflowJobStatus(ctx, owner, c.Repo, c.JobID)
		if err != nil {
			t.logger().Warn("github job status check failed",
				slog.Int64("job_id", c.JobID),
				slog.String("error", err.Error()))
			continue
		}

		if !ghStatus.Completed {
			continue
		}

		if err := t.markJobCompleted(ctx, c.JobID, ghStatus.Conclusion); err != nil {
			t.logger().Error("mark stale job completed failed",
				slog.Int64("job_id", c.JobID),
				slog.String("error", err.Error()))
			continue
		}

		reconciled++
		t.logger().Info("stale job reconciled",
			slog.Int64("job_id", c.JobID),
			slog.String("conclusion", ghStatus.Conclusion))
	}

	if t.metrics != nil && reconciled > 0 {
		_ = t.metrics.PublishStaleJobsReconciled(ctx, reconciled)
	}

	if reconciled > 0 {
		t.logger().Info("stale jobs reconciled", slog.Int(logging.KeyCount, reconciled))
	}
	return nil
}

// markJobCompleted updates a job's status to "completed" with the real GitHub conclusion.
// Uses conditional write to prevent race with concurrent normal job completion.
func (t *Tasks) markJobCompleted(ctx context.Context, jobID int64, conclusion string) error {
	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": jobID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	_, err = t.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(t.config.JobsTableName),
		Key:                 key,
		UpdateExpression:    aws.String("SET #status = :completed, completed_at = :completed_at, conclusion = :conclusion"),
		ConditionExpression: aws.String("#status = :running OR #status = :claiming"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":completed":    &types.AttributeValueMemberS{Value: "completed"},
			":running":      &types.AttributeValueMemberS{Value: "running"},
			":claiming":     &types.AttributeValueMemberS{Value: "claiming"},
			":completed_at": &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
			":conclusion":   &types.AttributeValueMemberS{Value: conclusion},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil
		}
		return fmt.Errorf("failed to update job: %w", err)
	}

	return nil
}

// splitRepo splits "owner/repo" into owner and repo components.
func splitRepo(repo string) (owner, name string, ok bool) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// EphemeralPoolTTL is the duration after which inactive ephemeral pools are deleted.
const EphemeralPoolTTL = 4 * time.Hour

// ExecuteEphemeralPoolCleanup removes ephemeral pools that have been inactive for longer than TTL.
// It first terminates any running/stopped instances belonging to the pool before deleting the config.
func (t *Tasks) ExecuteEphemeralPoolCleanup(ctx context.Context) error {
	if t.poolDB == nil {
		return nil
	}

	pools, err := t.poolDB.ListPools(ctx)
	if err != nil {
		return fmt.Errorf("failed to list pools: %w", err)
	}

	var cleaned int
	for _, poolName := range pools {
		poolCfg, err := t.poolDB.GetPoolConfig(ctx, poolName)
		if err != nil || poolCfg == nil {
			continue
		}

		if !poolCfg.Ephemeral {
			continue
		}

		if time.Since(poolCfg.LastJobTime) <= EphemeralPoolTTL {
			continue
		}

		if err := t.terminatePoolInstances(ctx, poolName); err != nil {
			t.logger().Error("pool instances terminate failed",
				slog.String(logging.KeyPoolName, poolName),
				slog.String("error", err.Error()))
		}

		if err := t.poolDB.DeletePoolConfig(ctx, poolName); err != nil {
			t.logger().Error("ephemeral pool delete failed",
				slog.String(logging.KeyPoolName, poolName),
				slog.String("error", err.Error()))
			continue
		}

		t.logger().Info("ephemeral pool deleted",
			slog.String(logging.KeyPoolName, poolName),
			slog.Duration("inactive_duration", time.Since(poolCfg.LastJobTime).Round(time.Minute)))
		cleaned++
	}

	return nil
}

// ExecuteOrphanedSpotRequests finds and cancels spot instance requests whose instances no longer exist.
// This cleans up persistent spot requests that were not properly cancelled during instance termination.
func (t *Tasks) ExecuteOrphanedSpotRequests(ctx context.Context) error {
	// Find all active spot requests with runs-fleet:managed tag
	input := &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:runs-fleet:managed"),
				Values: []string{"true"},
			},
			{
				Name:   aws.String("state"),
				Values: []string{"open", "active", "disabled"},
			},
		},
	}

	output, err := t.ec2Client.DescribeSpotInstanceRequests(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to describe spot instance requests: %w", err)
	}

	if len(output.SpotInstanceRequests) == 0 {
		t.logger().Debug("no managed spot requests found")
		return nil
	}

	// Collect instance IDs to check for existence
	var instanceIDs []string
	requestToInstance := make(map[string]string) // spot request ID -> instance ID
	for _, req := range output.SpotInstanceRequests {
		if req.SpotInstanceRequestId == nil {
			continue
		}
		// Only check fulfilled requests (have an instance ID)
		if req.InstanceId != nil && *req.InstanceId != "" {
			instanceIDs = append(instanceIDs, *req.InstanceId)
			requestToInstance[*req.SpotInstanceRequestId] = *req.InstanceId
		}
	}

	if len(instanceIDs) == 0 {
		return nil
	}

	// Check which instances still exist
	existingInstances := t.batchCheckInstances(ctx, instanceIDsToOrphanedCandidates(instanceIDs))

	// Cancel spot requests for non-existent instances
	var orphanedRequestIDs []string
	for reqID, instanceID := range requestToInstance {
		if !existingInstances[instanceID] {
			orphanedRequestIDs = append(orphanedRequestIDs, reqID)
		}
	}

	if len(orphanedRequestIDs) == 0 {
		return nil
	}

	// Cancel orphaned spot requests in batches of 100
	const batchSize = 100
	var cancelledCount int
	for i := 0; i < len(orphanedRequestIDs); i += batchSize {
		end := i + batchSize
		if end > len(orphanedRequestIDs) {
			end = len(orphanedRequestIDs)
		}
		batch := orphanedRequestIDs[i:end]

		_, err := t.ec2Client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
			SpotInstanceRequestIds: batch,
		})
		if err != nil {
			t.logger().Error("spot request cancellation failed",
				slog.Int("count", len(batch)),
				slog.String("error", err.Error()))
			continue
		}
		cancelledCount += len(batch)
	}

	if cancelledCount > 0 {
		t.logger().Info("orphaned spot requests cancelled",
			slog.Int(logging.KeyCount, cancelledCount))
	}
	return nil
}

// instanceIDsToOrphanedCandidates converts instance IDs to orphanedJobCandidate slice for batch checking.
func instanceIDsToOrphanedCandidates(instanceIDs []string) []orphanedJobCandidate {
	candidates := make([]orphanedJobCandidate, len(instanceIDs))
	for i, id := range instanceIDs {
		candidates[i] = orphanedJobCandidate{InstanceID: id}
	}
	return candidates
}

// terminatePoolInstances terminates all EC2 instances belonging to a pool.
func (t *Tasks) terminatePoolInstances(ctx context.Context, poolName string) error {
	if t.ec2Client == nil {
		return nil
	}

	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:runs-fleet:pool"),
				Values: []string{poolName},
			},
			{
				Name:   aws.String("tag:runs-fleet:managed"),
				Values: []string{"true"},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"pending", "running", "stopping", "stopped"},
			},
		},
	}

	output, err := t.ec2Client.DescribeInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to describe pool instances: %w", err)
	}

	var instanceIDs []string
	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
			if instance.InstanceId != nil {
				instanceIDs = append(instanceIDs, *instance.InstanceId)
			}
		}
	}

	if len(instanceIDs) == 0 {
		return nil
	}

	_, err = t.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instances: %w", err)
	}

	t.logger().Info("pool instances terminated",
		slog.String(logging.KeyPoolName, poolName),
		slog.Int(logging.KeyCount, len(instanceIDs)))
	return nil
}
