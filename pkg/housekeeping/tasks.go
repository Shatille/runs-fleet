package housekeeping

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
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

// Tasks implements housekeeping task execution.
type Tasks struct {
	ec2Client    EC2API
	ssmClient    SSMAPI
	dynamoClient DynamoDBAPI
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

	// Get all runner config parameters
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

			// Check if instance still exists
			exists, err := t.instanceExists(ctx, instanceID)
			if err != nil {
				log.Printf("Warning: failed to check instance %s: %v", instanceID, err)
				continue
			}

			if !exists {
				// Delete the parameter
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

// instanceExists checks if an EC2 instance exists.
func (t *Tasks) instanceExists(ctx context.Context, instanceID string) (bool, error) {
	output, err := t.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		// Check if it's a "not found" error
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
			fmt.Sscanf(v.Value, "%d", &desiredRunning)
		}
		if v, ok := item["current_running"].(*types.AttributeValueMemberN); ok {
			fmt.Sscanf(v.Value, "%d", &currentRunning)
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

	if t.costReporter == nil {
		log.Println("Cost reporter not configured, skipping")
		return nil
	}

	return t.costReporter.GenerateDailyReport(ctx)
}
