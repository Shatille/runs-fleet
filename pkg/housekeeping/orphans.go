package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// OrphanScanAPI defines the DynamoDB operations needed for orphaned job scanning.
type OrphanScanAPI interface {
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// OrphanEC2API defines the EC2 operations needed for orphaned job instance checking.
type OrphanEC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// OrphanedJobCandidate holds job info for orphaned job detection.
type OrphanedJobCandidate struct {
	JobID      int64
	InstanceID string
	Status     string
}

const (
	orphanInstanceCheckBatchSize = 100
)

// FindOrphanedJobCandidates scans DynamoDB for jobs in "running" or "claiming" status
// older than the given threshold. Jobs in "running" status without an instance_id are
// excluded (they need an instance to verify against). Jobs in "claiming" status without
// an instance_id are included (instance creation failed).
func FindOrphanedJobCandidates(ctx context.Context, dynamoClient OrphanScanAPI, jobsTableName string, threshold time.Duration) ([]OrphanedJobCandidate, error) {
	cutoffTime := time.Now().Add(-threshold).Format(time.RFC3339)

	input := &dynamodb.ScanInput{
		TableName:        aws.String(jobsTableName),
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

	var candidates []OrphanedJobCandidate
	var lastEvaluatedKey map[string]types.AttributeValue

	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, err
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

			if status == "running" && instanceID == "" {
				continue
			}

			candidates = append(candidates, OrphanedJobCandidate{
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

// InstanceExistsChecker determines whether a single EC2 instance exists.
// Implementations may differ in error handling behavior (e.g., smithy error
// type assertion vs string matching, termination grace periods).
type InstanceExistsChecker func(ctx context.Context, instanceID string) bool

// BatchCheckInstanceExistence checks multiple instances in batches via DescribeInstances
// and returns a map of instance IDs that still exist (non-terminated).
// When a batch call fails, it falls back to checking each instance individually
// using the provided fallback function.
func BatchCheckInstanceExistence(ctx context.Context, ec2Client OrphanEC2API, candidates []OrphanedJobCandidate, fallback InstanceExistsChecker) map[string]bool {
	instanceSet := make(map[string]struct{})
	for _, c := range candidates {
		instanceSet[c.InstanceID] = struct{}{}
	}

	instanceIDs := make([]string, 0, len(instanceSet))
	for id := range instanceSet {
		instanceIDs = append(instanceIDs, id)
	}

	existing := make(map[string]bool)

	for i := 0; i < len(instanceIDs); i += orphanInstanceCheckBatchSize {
		end := i + orphanInstanceCheckBatchSize
		if end > len(instanceIDs) {
			end = len(instanceIDs)
		}
		batch := instanceIDs[i:end]

		output, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: batch,
		})
		if err != nil {
			for _, id := range batch {
				if fallback(ctx, id) {
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

// SeparateOrphanedJobs splits candidates into confirmed orphans (no instance ID)
// and those requiring EC2 verification (have instance ID).
func SeparateOrphanedJobs(candidates []OrphanedJobCandidate) (withInstance, withoutInstance []OrphanedJobCandidate) {
	for _, c := range candidates {
		if c.InstanceID == "" {
			withoutInstance = append(withoutInstance, c)
		} else {
			withInstance = append(withInstance, c)
		}
	}
	return withInstance, withoutInstance
}

// MarkJobOrphaned updates a job's status to "orphaned" with a completed_at timestamp.
// Uses conditional write to prevent race conditions with concurrent job completions.
// Returns nil if the job's status has already changed (ConditionalCheckFailedException).
func MarkJobOrphaned(ctx context.Context, dynamoClient OrphanScanAPI, jobsTableName string, jobID int64) error {
	now := time.Now().Format(time.RFC3339)

	_, err := dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(jobsTableName),
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
		return fmt.Errorf("failed to update job: %w", err)
	}
	return nil
}
