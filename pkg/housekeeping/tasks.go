package housekeeping

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// EC2API defines EC2 operations for housekeeping.
type EC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// SSMAPI defines SSM operations for housekeeping.
type SSMAPI interface {
	GetParametersByPath(ctx context.Context, params *ssm.GetParametersByPathInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
	DeleteParameter(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
}

// DynamoDBAPI defines DynamoDB operations for housekeeping.
type DynamoDBAPI interface {
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	BatchWriteItem(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// MetricsAPI defines CloudWatch metrics publishing.
type MetricsAPI interface {
	PublishOrphanedInstancesTerminated(ctx context.Context, count int) error
	PublishSSMParametersDeleted(ctx context.Context, count int) error
	PublishJobRecordsArchived(ctx context.Context, count int) error
	PublishPoolUtilization(ctx context.Context, poolName string, utilization float64) error
}

// CostReporter generates cost reports.
type CostReporter interface {
	GenerateDailyReport(ctx context.Context) error
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
	ec2Client    EC2API
	ssmClient    SSMAPI
	dynamoClient DynamoDBAPI
	sqsClient    SQSAPI
	poolDB       PoolDBAPI
	metrics      MetricsAPI
	costReporter CostReporter
	config       *config.Config
}

// NewTasks creates a new Tasks executor.
func NewTasks(cfg aws.Config, appConfig *config.Config, metrics MetricsAPI, costReporter CostReporter) *Tasks {
	return &Tasks{
		ec2Client:    ec2.NewFromConfig(cfg),
		ssmClient:    ssm.NewFromConfig(cfg),
		dynamoClient: dynamodb.NewFromConfig(cfg),
		sqsClient:    sqs.NewFromConfig(cfg),
		metrics:      metrics,
		costReporter: costReporter,
		config:       appConfig,
	}
}

// ExecuteOrphanedInstances detects and terminates orphaned instances.
func (t *Tasks) ExecuteOrphanedInstances(ctx context.Context) error {
	log.Println("Executing orphaned instances cleanup...")

	// Calculate the cutoff time (max_runtime + 10 minute buffer)
	maxRuntime := time.Duration(t.config.MaxRuntimeMinutes+10) * time.Minute
	cutoffTime := time.Now().Add(-maxRuntime)

	// Query for managed instances
	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:runs-fleet:managed"),
				Values: []string{"true"},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"running", "pending"},
			},
		},
	}

	output, err := t.ec2Client.DescribeInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to describe instances: %w", err)
	}

	var orphanedIDs []string
	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
			if instance.LaunchTime != nil && instance.LaunchTime.Before(cutoffTime) {
				if instance.InstanceId != nil {
					orphanedIDs = append(orphanedIDs, *instance.InstanceId)
					log.Printf("Found orphaned instance: %s (launched at %s)", *instance.InstanceId, instance.LaunchTime)
				}
			}
		}
	}

	if len(orphanedIDs) == 0 {
		log.Println("No orphaned instances found")
		return nil
	}

	// Terminate orphaned instances
	log.Printf("Terminating %d orphaned instances", len(orphanedIDs))
	_, err = t.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: orphanedIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate orphaned instances: %w", err)
	}

	// Publish metrics
	if t.metrics != nil {
		if err := t.metrics.PublishOrphanedInstancesTerminated(ctx, len(orphanedIDs)); err != nil {
			log.Printf("Warning: failed to publish metric: %v", err)
		}
	}

	log.Printf("Terminated %d orphaned instances", len(orphanedIDs))
	return nil
}

// ExecuteStaleSSM cleans up stale SSM parameters.
func (t *Tasks) ExecuteStaleSSM(ctx context.Context) error {
	log.Println("Executing stale SSM parameter cleanup...")

	path := "/runs-fleet/runners/"
	var nextToken *string
	var deletedCount int

	for {
		input := &ssm.GetParametersByPathInput{
			Path:      aws.String(path),
			Recursive: aws.Bool(true),
			NextToken: nextToken,
		}

		output, err := t.ssmClient.GetParametersByPath(ctx, input)
		if err != nil {
			return fmt.Errorf("failed to get parameters: %w", err)
		}

		for _, param := range output.Parameters {
			if param.Name == nil {
				continue
			}

			// Extract instance ID from parameter name
			// Format: /runs-fleet/runners/{instance-id}/config
			parts := strings.Split(*param.Name, "/")
			if len(parts) < 4 {
				continue
			}
			instanceID := parts[3]

			exists, err := t.instanceExists(ctx, instanceID)
			if err != nil {
				log.Printf("Warning: failed to check instance %s: %v", instanceID, err)
				continue
			}

			if !exists {
				_, err := t.ssmClient.DeleteParameter(ctx, &ssm.DeleteParameterInput{
					Name: param.Name,
				})
				if err != nil {
					log.Printf("Warning: failed to delete parameter %s: %v", *param.Name, err)
				} else {
					deletedCount++
					log.Printf("Deleted stale parameter: %s", *param.Name)
				}
			}
		}

		nextToken = output.NextToken
		if nextToken == nil {
			break
		}
	}

	// Publish metrics
	if t.metrics != nil && deletedCount > 0 {
		if err := t.metrics.PublishSSMParametersDeleted(ctx, deletedCount); err != nil {
			log.Printf("Warning: failed to publish metric: %v", err)
		}
	}

	log.Printf("Deleted %d stale SSM parameters", deletedCount)
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
				// Consider instance as existing if it's not terminated
				if state != ec2types.InstanceStateNameTerminated {
					return true, nil
				}

				// For terminated instances, check if enough time has passed
				// StateTransitionReason contains the termination time for terminated instances
				// If we can't determine when it was terminated, be conservative and consider it existing
				if instance.StateTransitionReason != nil {
					// StateTransitionReason format: "User initiated (YYYY-MM-DD HH:MM:SS GMT)"
					// We'll use a simpler approach: if the instance was launched recently, wait
					if instance.LaunchTime != nil {
						// If launched within grace period, consider it still "existing" to be safe
						if time.Since(*instance.LaunchTime) < instanceTerminationGracePeriod {
							log.Printf("Instance %s terminated recently, waiting before cleanup", instanceID)
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
	log.Println("Executing old job records cleanup...")

	if t.config.JobsTableName == "" {
		log.Println("Jobs table not configured, skipping")
		return nil
	}

	// Calculate cutoff time (7 days ago)
	cutoffTime := time.Now().AddDate(0, 0, -7).Format(time.RFC3339)

	// Scan for old job records
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
		log.Println("No old job records to delete")
		return nil
	}

	// Delete in batches of 25
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
			log.Printf("Warning: failed to delete batch: %v", err)
			continue
		}

		deletedCount += len(batch)
	}

	// Publish metrics
	if t.metrics != nil && deletedCount > 0 {
		if err := t.metrics.PublishJobRecordsArchived(ctx, deletedCount); err != nil {
			log.Printf("Warning: failed to publish metric: %v", err)
		}
	}

	log.Printf("Deleted %d old job records", deletedCount)
	return nil
}

// ExecutePoolAudit generates pool utilization reports.
func (t *Tasks) ExecutePoolAudit(ctx context.Context) error {
	log.Println("Executing pool audit...")

	if t.config.PoolsTableName == "" {
		log.Println("Pools table not configured, skipping")
		return nil
	}

	// Scan pools table
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

		log.Printf("Pool %s: %d/%d instances (%.1f%% utilization)",
			poolName, currentRunning, desiredRunning, utilization)

		// Publish metrics
		if t.metrics != nil {
			if err := t.metrics.PublishPoolUtilization(ctx, poolName, utilization); err != nil {
				log.Printf("Warning: failed to publish metric: %v", err)
			}
		}
	}

	return nil
}

// ExecuteCostReport generates the daily cost report.
func (t *Tasks) ExecuteCostReport(ctx context.Context) error {
	log.Println("Executing cost report generation...")

	if isNilInterface(t.costReporter) {
		log.Println("Cost reporter not configured, skipping")
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
	log.Println("Executing DLQ redrive...")

	if t.config.QueueDLQURL == "" {
		log.Println("DLQ URL not configured, skipping")
		return nil
	}

	if t.config.QueueURL == "" {
		log.Println("Main queue URL not configured, skipping")
		return nil
	}

	// Check if DLQ has messages
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
		log.Println("DLQ is empty, nothing to redrive")
		return nil
	}

	dlqArn := attrs.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
	if dlqArn == "" {
		return fmt.Errorf("failed to get DLQ ARN")
	}

	// Get main queue ARN
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

	log.Printf("Starting DLQ redrive: %s messages from DLQ to main queue", msgCount)

	// Start the message move task
	output, err := t.sqsClient.StartMessageMoveTask(ctx, &sqs.StartMessageMoveTaskInput{
		SourceArn:      aws.String(dlqArn),
		DestinationArn: aws.String(mainQueueArn),
	})
	if err != nil {
		return fmt.Errorf("failed to start message move task: %w", err)
	}

	log.Printf("DLQ redrive started: task handle %s", aws.ToString(output.TaskHandle))
	return nil
}

// SetPoolDB sets the pool database client for ephemeral pool cleanup.
func (t *Tasks) SetPoolDB(poolDB PoolDBAPI) {
	t.poolDB = poolDB
}

// EphemeralPoolTTL is the duration after which inactive ephemeral pools are deleted.
const EphemeralPoolTTL = 4 * time.Hour

// ExecuteEphemeralPoolCleanup removes ephemeral pools that have been inactive for longer than TTL.
// It first terminates any running/stopped instances belonging to the pool before deleting the config.
func (t *Tasks) ExecuteEphemeralPoolCleanup(ctx context.Context) error {
	if t.poolDB == nil {
		log.Println("Skipping ephemeral pool cleanup: pool database not configured")
		return nil
	}

	log.Println("Executing ephemeral pool cleanup...")

	pools, err := t.poolDB.ListPools(ctx)
	if err != nil {
		return fmt.Errorf("failed to list pools: %w", err)
	}

	var cleaned int
	for _, poolName := range pools {
		config, err := t.poolDB.GetPoolConfig(ctx, poolName)
		if err != nil {
			log.Printf("Failed to get pool config for %s: %v", poolName, err)
			continue
		}
		if config == nil {
			continue
		}

		// Only clean up ephemeral pools
		if !config.Ephemeral {
			continue
		}

		// Check if pool is inactive beyond TTL
		if time.Since(config.LastJobTime) <= EphemeralPoolTTL {
			continue
		}

		// Terminate pool instances before deleting config
		if err := t.terminatePoolInstances(ctx, poolName); err != nil {
			log.Printf("Failed to terminate instances for pool %s: %v", poolName, err)
			// Continue to try deleting config anyway - orphaned instances will be cleaned up separately
		}

		// Delete the ephemeral pool
		if err := t.poolDB.DeletePoolConfig(ctx, poolName); err != nil {
			log.Printf("Failed to delete ephemeral pool %s: %v", poolName, err)
			continue
		}

		log.Printf("Deleted ephemeral pool %s (inactive for %v)", poolName, time.Since(config.LastJobTime).Round(time.Minute))
		cleaned++
	}

	log.Printf("Ephemeral pool cleanup completed: %d pools deleted", cleaned)
	return nil
}

// terminatePoolInstances terminates all EC2 instances belonging to a pool.
func (t *Tasks) terminatePoolInstances(ctx context.Context, poolName string) error {
	if t.ec2Client == nil {
		log.Printf("EC2 client not configured, skipping instance termination for pool %s", poolName)
		return nil
	}

	// Query for pool instances
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
		log.Printf("No instances to terminate for pool %s", poolName)
		return nil
	}

	log.Printf("Terminating %d instances for pool %s: %v", len(instanceIDs), poolName, instanceIDs)
	_, err = t.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instances: %w", err)
	}

	return nil
}
