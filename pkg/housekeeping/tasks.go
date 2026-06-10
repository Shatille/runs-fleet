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
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/queue"
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
	PublishHousekeepingAction(ctx context.Context, action string, count int) error
	PublishPoolInstances(ctx context.Context, pool, state string, n int) error
	PublishPoolDesired(ctx context.Context, pool, kind string, n int) error
	PublishJobRequeued(ctx context.Context, reason string) error
	PublishSchedulingFailure(ctx context.Context, taskType string) error
	PublishQueueDepth(ctx context.Context, queue string, depth float64) error
}

// JobRequeuer re-enqueues a job onto the main job queue. Satisfied by the SQS
// queue client; used by the unconfirmed-runner watchdog to recover a job whose
// instance launched but whose runner never registered.
type JobRequeuer interface {
	SendMessage(ctx context.Context, job *queue.JobMessage) error
}

// Housekeeping action labels for PublishHousekeepingAction.
const (
	housekeepingActionOrphanedInstances = "orphaned_instances"
	housekeepingActionSSMParams         = "ssm_params"
	housekeepingActionJobRecords        = "job_records"
	housekeepingActionOrphanedJobs      = "orphaned_jobs"
	housekeepingActionStaleJobs         = "stale_jobs"
	housekeepingActionUnconfirmedRunner = "unconfirmed_runners"
	housekeepingActionDLQRedrive        = "dlq_redrive"
)

// dlqQueueName labels the main queue's dead-letter queue for depth metrics,
// using the same "<queue>-dlq" convention the admin queues handler reports under.
const dlqQueueName = "main-dlq"

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
	ListMessageMoveTasks(ctx context.Context, params *sqs.ListMessageMoveTasksInput, optFns ...func(*sqs.Options)) (*sqs.ListMessageMoveTasksOutput, error)
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
	ec2Client     EC2API
	secretsStore  secrets.Store
	dynamoClient  DynamoDBAPI
	sqsClient     SQSAPI
	poolDB        PoolDBAPI
	gitHubChecker GitHubJobChecker
	jobRequeuer   JobRequeuer
	metrics       MetricsAPI
	costReporter  CostReporter
	config        *config.Config
	log           *logging.Logger
}

// SetJobRequeuer wires the main-queue requeuer used by the unconfirmed-runner
// watchdog. When unset, the watchdog is a no-op (it never destroys a job it
// cannot recover).
func (t *Tasks) SetJobRequeuer(r JobRequeuer) {
	t.jobRequeuer = r
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
		t.logger().Debug(ctx, "no orphaned instances found")
		return nil
	}

	if len(untaggedOrphans) > 0 {
		t.logger().Warn(ctx, "found untagged orphaned instances (persistent spot tag propagation failure)",
			slog.Int("untagged_count", len(untaggedOrphans)),
			slog.Any("untagged_ids", untaggedOrphans))
	}

	// Cancel persistent spot requests before termination to prevent zombie resurrection
	t.cancelSpotRequestsForInstances(ctx, orphanedIDs)

	t.logger().Info(ctx, "terminating orphaned instances", slog.Int(logging.KeyCount, len(orphanedIDs)))

	_, err := t.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: orphanedIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate orphaned instances: %w", err)
	}

	if t.metrics != nil {
		_ = t.metrics.PublishHousekeepingAction(ctx, housekeepingActionOrphanedInstances, len(orphanedIDs))
	}

	t.logger().Info(ctx, "orphaned instances terminated",
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
			t.logger().Error(ctx, "failed to describe profile-based instances", slog.String("error", err.Error()))
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
			t.logger().Error(ctx, "failed to describe spot requests for orphaned instances after retry",
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
			t.logger().Error(ctx, "failed to cancel spot requests for orphaned instances after retry",
				slog.String("error", err.Error()),
				slog.Int("batch_size", len(batch)),
				slog.Any("spot_request_ids", batch))
			continue
		}
		cancelledCount += len(batch)
	}

	if cancelledCount > 0 {
		t.logger().Info(ctx, "cancelled spot requests for orphaned instances",
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
	t.logger().Warn(ctx, "describe spot requests failed, retrying",
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
	t.logger().Warn(ctx, "cancel spot requests failed, retrying",
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
		_ = t.metrics.PublishHousekeepingAction(ctx, housekeepingActionSSMParams, deletedCount)
	}

	if deletedCount > 0 {
		t.logger().Info(ctx, "stale secrets deleted", slog.Int(logging.KeyCount, deletedCount))
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
		_ = t.metrics.PublishHousekeepingAction(ctx, housekeepingActionJobRecords, deletedCount)
	}

	if deletedCount > 0 {
		t.logger().Info(ctx, "old job records deleted", slog.Int(logging.KeyCount, deletedCount))
	}
	return nil
}

// orphanedJobThreshold is the minimum age for running jobs to be considered orphaned.
// Jobs older than this with non-existent instances are marked as orphaned.
// 2 hours exceeds max job runtime (MaxRuntimeMinutes + 10) and allows for spot
// interruption re-queue delays, ensuring legitimate jobs aren't prematurely orphaned.
const orphanedJobThreshold = 2 * time.Hour

// ExecuteOrphanedJobs cleans up jobs with status "running" whose instances no longer exist.
// This prevents stale job records from inflating pool busy counts and causing infinite scaling.
//
// Note: Uses DynamoDB Scan which is O(n) on table size. For tables with >100k items,
// consider adding GSI on (status, created_at) for more efficient queries.
func (t *Tasks) ExecuteOrphanedJobs(ctx context.Context) error {
	if t.config.JobsTableName == "" {
		return nil
	}

	candidates, err := FindOrphanedJobCandidates(ctx, t.dynamoClient, t.config.JobsTableName, orphanedJobThreshold)
	if err != nil {
		return fmt.Errorf("failed to scan jobs: %w", err)
	}

	if len(candidates) == 0 {
		return nil
	}

	candidatesWithInstance, orphanedCandidates := SeparateOrphanedJobs(candidates)

	if len(candidatesWithInstance) > 0 {
		fallback := func(ctx context.Context, instanceID string) bool {
			exists, _ := t.instanceExists(ctx, instanceID)
			return exists
		}
		existingInstances := BatchCheckInstanceExistence(ctx, t.ec2Client, candidatesWithInstance, fallback)
		for _, c := range candidatesWithInstance {
			if !existingInstances[c.InstanceID] {
				orphanedCandidates = append(orphanedCandidates, c)
			}
		}
	}

	var orphanedCount int
	for _, c := range orphanedCandidates {
		jobCtx := logging.ContextWith(ctx,
			slog.Int64(logging.KeyJobID, c.JobID),
			slog.String(logging.KeyInstanceID, c.InstanceID))
		if err := MarkJobOrphaned(ctx, t.dynamoClient, t.config.JobsTableName, c.JobID); err != nil {
			t.logger().Error(jobCtx, "mark job orphaned failed",
				slog.String("error", err.Error()))
			continue
		}
		orphanedCount++
		t.logger().Info(jobCtx, "orphaned job cleaned up")
	}

	if t.metrics != nil && orphanedCount > 0 {
		_ = t.metrics.PublishHousekeepingAction(ctx, housekeepingActionOrphanedJobs, orphanedCount)
	}

	if orphanedCount > 0 {
		t.logger().Info(ctx, "orphaned jobs cleaned up", slog.Int(logging.KeyCount, orphanedCount))
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

		// Utilization is derivable from the running and desired-running gauges,
		// so emit those states rather than a precomputed percentage.
		if t.metrics != nil {
			_ = t.metrics.PublishPoolInstances(ctx, poolName, "running", currentRunning)
			_ = t.metrics.PublishPoolDesired(ctx, poolName, "running", desiredRunning)
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

// ExecuteDLQRedrive moves messages from the DLQ back to the main queue via an
// SQS message-move task, skipping if one is already in progress.
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
	// Publish DLQ depth every tick (including 0) so the gauge tracks a draining DLQ;
	// a non-numeric attribute (never expected from SQS) just skips the emit.
	depth, depthErr := strconv.ParseFloat(msgCount, 64)
	if t.metrics != nil && depthErr == nil {
		_ = t.metrics.PublishQueueDepth(ctx, dlqQueueName, depth)
	}
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

	// SQS permits only one active move task per source queue; skip if one is
	// already running rather than erroring on every redrive tick.
	running, err := t.dlqMoveTaskRunning(ctx, dlqArn)
	if err != nil {
		return fmt.Errorf("failed to list message move tasks: %w", err)
	}
	if running {
		t.logger().Info(ctx, "dlq redrive skipped; move task already running",
			slog.String("message_count", msgCount))
		return nil
	}

	output, err := t.sqsClient.StartMessageMoveTask(ctx, &sqs.StartMessageMoveTaskInput{
		SourceArn:      aws.String(dlqArn),
		DestinationArn: aws.String(mainQueueArn),
	})
	if err != nil {
		return fmt.Errorf("failed to start message move task: %w", err)
	}

	// Count the (approximate) messages handed to the move task, reusing the same
	// parsed depth as the gauge above. A non-numeric ApproximateNumberOfMessages
	// (never expected from SQS) skips the count, like it skips the gauge.
	if t.metrics != nil && depthErr == nil {
		_ = t.metrics.PublishHousekeepingAction(ctx, housekeepingActionDLQRedrive, int(depth))
	}

	t.logger().Info(ctx, "dlq redrive started",
		slog.String("message_count", msgCount),
		slog.String("task_handle", aws.ToString(output.TaskHandle)))
	return nil
}

// dlqMoveTaskRunning reports whether an SQS message-move task is currently
// running for the given source queue ARN.
func (t *Tasks) dlqMoveTaskRunning(ctx context.Context, sourceArn string) (bool, error) {
	out, err := t.sqsClient.ListMessageMoveTasks(ctx, &sqs.ListMessageMoveTasksInput{
		SourceArn:  aws.String(sourceArn),
		MaxResults: aws.Int32(1),
	})
	if err != nil {
		return false, err
	}
	for _, task := range out.Results {
		// RUNNING and CANCELLING are both live states in which SQS rejects a new task.
		if status := aws.ToString(task.Status); status == "RUNNING" || status == "CANCELLING" {
			return true, nil
		}
	}
	return false, nil
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
	JobID      int64
	RunID      int64
	Repo       string
	InstanceID string
	Status     string
}

// ExecuteStaleJobs finalizes jobs stuck in running/claiming so their records stop
// occupying pool capacity in the p90 concurrency calculation.
//
// It uses two signals, in order of authority:
//
//  1. Instance existence (write-side root cause). A record whose instance no longer
//     exists cannot still be running a job, so it is finalized immediately via
//     MarkJobOrphaned (terminal status + completed_at). This closes records the
//     agent's termination message never reached — including records with no repo —
//     in ~10 minutes instead of waiting for the 2h orphan reaper. These are the
//     lingering "running" records that inflate GetPoolP90Concurrency.
//  2. GitHub status. For records whose instance still exists (genuinely could be
//     running), the GitHub workflow-job status is the authoritative completion
//     signal; completed jobs are marked completed with the real conclusion.
func (t *Tasks) ExecuteStaleJobs(ctx context.Context) error {
	if t.config.JobsTableName == "" {
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
			":running":  &types.AttributeValueMemberS{Value: string(db.JobStatusRunning)},
			":claiming": &types.AttributeValueMemberS{Value: string(db.JobStatusClaiming)},
			":cutoff":   &types.AttributeValueMemberS{Value: cutoffTime},
		},
		ProjectionExpression: aws.String("job_id, run_id, repo, instance_id, #status"),
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
			var jobID, runID int64
			var repo, instanceID, status string

			if v, ok := item["job_id"].(*types.AttributeValueMemberN); ok {
				parsed, err := strconv.ParseInt(v.Value, 10, 64)
				if err != nil {
					continue
				}
				jobID = parsed
			}
			if v, ok := item["run_id"].(*types.AttributeValueMemberN); ok {
				if parsed, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
					runID = parsed
				}
			}
			if v, ok := item["repo"].(*types.AttributeValueMemberS); ok {
				repo = v.Value
			}
			if v, ok := item["instance_id"].(*types.AttributeValueMemberS); ok {
				instanceID = v.Value
			}
			if v, ok := item["status"].(*types.AttributeValueMemberS); ok {
				status = v.Value
			}

			// A record we can neither verify against an instance nor against GitHub
			// (no instance_id and no repo) is unreachable by either signal; leave it
			// for the orphan reaper rather than guess.
			if jobID == 0 || (repo == "" && instanceID == "") {
				t.logger().Debug(ctx, "skipping stale job candidate with missing fields",
					slog.Int64("job_id", jobID),
					slog.String("repo", repo))
				continue
			}

			candidates = append(candidates, staleJobCandidate{
				JobID:      jobID,
				RunID:      runID,
				Repo:       repo,
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

	gone, alive := t.partitionStaleByInstance(ctx, candidates)

	finalized := t.finalizeStaleJobsWithGoneInstance(ctx, gone)
	reconciled := t.reconcileStaleJobsViaGitHub(ctx, alive)
	total := finalized + reconciled

	if t.metrics != nil && total > 0 {
		if err := t.metrics.PublishHousekeepingAction(ctx, housekeepingActionStaleJobs, total); err != nil {
			t.logger().Warn(ctx, "failed to publish stale jobs reconciled metric", slog.String("error", err.Error()))
		}
	}

	if total > 0 {
		t.logger().Info(ctx, "stale jobs reconciled",
			slog.Int("finalized", finalized),
			slog.Int("github_reconciled", reconciled),
			slog.Int(logging.KeyCount, total))
	}
	return nil
}

// partitionStaleByInstance splits candidates into those whose instance is gone
// (terminated/non-existent) and those whose instance still exists. Candidates
// without an instance_id are treated as still-existing so the GitHub path can
// verify them. A failed existence check leaves a candidate in the "alive" set so
// we never finalize a job on the strength of a transient EC2 error.
func (t *Tasks) partitionStaleByInstance(ctx context.Context, candidates []staleJobCandidate) (gone, alive []staleJobCandidate) {
	var withInstance []OrphanedJobCandidate
	for _, c := range candidates {
		if c.InstanceID == "" {
			alive = append(alive, c)
			continue
		}
		withInstance = append(withInstance, OrphanedJobCandidate{JobID: c.JobID, InstanceID: c.InstanceID})
	}

	if len(withInstance) == 0 {
		return gone, alive
	}

	fallback := func(ctx context.Context, instanceID string) bool {
		exists, _ := t.instanceExists(ctx, instanceID)
		return exists
	}
	existing := BatchCheckInstanceExistence(ctx, t.ec2Client, withInstance, fallback)

	for _, c := range candidates {
		if c.InstanceID == "" {
			continue
		}
		if existing[c.InstanceID] {
			alive = append(alive, c)
		} else {
			gone = append(gone, c)
		}
	}
	return gone, alive
}

// finalizeStaleJobsWithGoneInstance marks each candidate orphaned (terminal status +
// completed_at) via a conditional write that only fires while the record is still
// running/claiming, so a concurrent normal completion or spot-interruption requeue is
// never clobbered. Returns the number of records finalized.
func (t *Tasks) finalizeStaleJobsWithGoneInstance(ctx context.Context, gone []staleJobCandidate) int {
	finalized := 0
	for _, c := range gone {
		jobCtx := logging.ContextWith(ctx,
			slog.Int64(logging.KeyJobID, c.JobID),
			slog.Int64(logging.KeyRunID, c.RunID),
			slog.String(logging.KeyRepo, c.Repo),
			slog.String(logging.KeyInstanceID, c.InstanceID))

		if err := MarkJobOrphaned(ctx, t.dynamoClient, t.config.JobsTableName, c.JobID); err != nil {
			t.logger().Error(jobCtx, "finalize stale job with gone instance failed",
				slog.String("error", err.Error()))
			continue
		}

		finalized++
		t.logger().Info(jobCtx, "stale job finalized; instance no longer exists")
	}
	return finalized
}

// reconcileStaleJobsViaGitHub finalizes candidates whose instance still exists by
// consulting the GitHub workflow-job status, bounded by maxStaleJobChecks to stay
// within API rate limits. Returns the number of records reconciled.
func (t *Tasks) reconcileStaleJobsViaGitHub(ctx context.Context, alive []staleJobCandidate) int {
	if len(alive) == 0 {
		return 0
	}
	if t.gitHubChecker == nil {
		t.logger().Warn(ctx, "stale job github reconcile skipped: GitHub job checker not configured")
		return 0
	}

	checked := 0
	reconciled := 0
	for _, c := range alive {
		if checked >= maxStaleJobChecks {
			t.logger().Info(ctx, "stale jobs check limit reached",
				slog.Int("checked", checked),
				slog.Int("remaining", len(alive)-checked))
			break
		}

		jobCtx := logging.ContextWith(ctx,
			slog.Int64(logging.KeyJobID, c.JobID),
			slog.Int64(logging.KeyRunID, c.RunID),
			slog.String(logging.KeyRepo, c.Repo))

		owner, _, ok := splitRepo(c.Repo)
		if !ok {
			t.logger().Debug(jobCtx, "skipping stale job with invalid repo format")
			continue
		}

		checked++
		ghStatus, err := t.gitHubChecker.GetWorkflowJobStatus(ctx, owner, c.Repo, c.JobID)
		if err != nil {
			t.logger().Warn(jobCtx, "github job status check failed",
				slog.String("error", err.Error()))
			continue
		}

		if ghStatus == nil || !ghStatus.Completed {
			continue
		}

		if err := t.markJobCompleted(jobCtx, c.JobID, ghStatus.Conclusion); err != nil {
			t.logger().Error(jobCtx, "mark stale job completed failed",
				slog.String("error", err.Error()))
			continue
		}

		reconciled++
		t.logger().Info(jobCtx, "stale job reconciled",
			slog.String("conclusion", ghStatus.Conclusion))
	}
	return reconciled
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
			":completed":    &types.AttributeValueMemberS{Value: string(db.JobStatusCompleted)},
			":running":      &types.AttributeValueMemberS{Value: string(db.JobStatusRunning)},
			":claiming":     &types.AttributeValueMemberS{Value: string(db.JobStatusClaiming)},
			":completed_at": &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
			":conclusion":   &types.AttributeValueMemberS{Value: conclusion},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			t.logger().Debug(ctx, "stale job already completed concurrently")
			return nil
		}
		return fmt.Errorf("failed to update job: %w", err)
	}

	return nil
}

// splitRepo splits "owner/repo" into owner and repo components.
func splitRepo(repo string) (owner, name string, ok bool) {
	if strings.Count(repo, "/") != 1 {
		return "", "", false
	}
	parts := strings.SplitN(repo, "/", 2)
	if parts[0] == "" || parts[1] == "" {
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

		poolCtx := logging.ContextWith(ctx, slog.String(logging.KeyPoolName, poolName))

		if err := t.terminatePoolInstances(poolCtx, poolName); err != nil {
			t.logger().Error(poolCtx, "pool instances terminate failed",
				slog.String("error", err.Error()))
		}

		if err := t.poolDB.DeletePoolConfig(ctx, poolName); err != nil {
			t.logger().Error(poolCtx, "ephemeral pool delete failed",
				slog.String("error", err.Error()))
			continue
		}

		t.logger().Info(poolCtx, "ephemeral pool deleted",
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
		t.logger().Debug(ctx, "no managed spot requests found")
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
	fallback := func(ctx context.Context, instanceID string) bool {
		exists, _ := t.instanceExists(ctx, instanceID)
		return exists
	}
	existingInstances := BatchCheckInstanceExistence(ctx, t.ec2Client, instanceIDsToOrphanedCandidates(instanceIDs), fallback)

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
			t.logger().Error(ctx, "spot request cancellation failed",
				slog.Int("count", len(batch)),
				slog.String("error", err.Error()))
			continue
		}
		cancelledCount += len(batch)
	}

	if cancelledCount > 0 {
		t.logger().Info(ctx, "orphaned spot requests cancelled",
			slog.Int(logging.KeyCount, cancelledCount))
	}
	return nil
}

// instanceIDsToOrphanedCandidates converts instance IDs to OrphanedJobCandidate slice for batch checking.
func instanceIDsToOrphanedCandidates(instanceIDs []string) []OrphanedJobCandidate {
	candidates := make([]OrphanedJobCandidate, len(instanceIDs))
	for i, id := range instanceIDs {
		candidates[i] = OrphanedJobCandidate{InstanceID: id}
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

	t.logger().Info(ctx, "pool instances terminated",
		slog.Int(logging.KeyCount, len(instanceIDs)))
	return nil
}
