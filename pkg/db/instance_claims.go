package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ErrInstanceAlreadyClaimed is returned when attempting to claim an instance that is already assigned to a job.
var ErrInstanceAlreadyClaimed = errors.New("instance already claimed by another job")

// instanceClaimPrefix is the key prefix for instance claims stored in the pools table.
const instanceClaimPrefix = "__instance_claim:"

// instanceClaimKey returns the DynamoDB key for an instance claim.
func instanceClaimKey(instanceID string) string {
	return instanceClaimPrefix + instanceID
}

// ClaimInstanceForJob atomically claims an instance for a specific job.
// Returns nil if the claim was acquired successfully.
// Returns ErrInstanceAlreadyClaimed if another job has already claimed this instance.
// The claim expires after the specified TTL to handle failed assignments.
//
// This provides distributed locking across multiple orchestrator instances to prevent
// race conditions where multiple orchestrators try to assign the same warm pool instance.
func (c *Client) ClaimInstanceForJob(ctx context.Context, instanceID string, jobID int64, ttl time.Duration) error {
	if instanceID == "" {
		return fmt.Errorf("instance ID cannot be empty")
	}
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}
	if ttl <= 0 {
		return fmt.Errorf("TTL must be positive")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	now := time.Now()
	expiresAt := now.Add(ttl).Unix()
	claimKey := instanceClaimKey(instanceID)

	_, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: claimKey},
		},
		UpdateExpression: aws.String("SET job_id = :job_id, claimed_at = :claimed_at, claim_expiry = :expiry"),
		ConditionExpression: aws.String(
			"attribute_not_exists(pool_name) OR " +
				"attribute_not_exists(claim_expiry) OR " +
				"claim_expiry < :now OR " +
				"job_id = :job_id",
		),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":job_id":     &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", jobID)},
			":claimed_at": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Unix())},
			":expiry":     &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", expiresAt)},
			":now":        &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Unix())},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrInstanceAlreadyClaimed
		}
		return fmt.Errorf("failed to claim instance: %w", err)
	}

	return nil
}

// ReleaseInstanceClaim releases an instance claim for a specific job.
// Only releases if the current job is the claim owner.
// Returns nil even if the claim was already released or owned by another job.
func (c *Client) ReleaseInstanceClaim(ctx context.Context, instanceID string, jobID int64) error {
	if instanceID == "" {
		return fmt.Errorf("instance ID cannot be empty")
	}
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	claimKey := instanceClaimKey(instanceID)

	_, err := c.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: claimKey},
		},
		ConditionExpression: aws.String("attribute_exists(pool_name) AND job_id = :job_id"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":job_id": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", jobID)},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			dbLog.Debug("instance claim not held",
				slog.String(logging.KeyInstanceID, instanceID),
				slog.Int64(logging.KeyJobID, jobID))
			return nil
		}
		return fmt.Errorf("failed to release instance claim: %w", err)
	}

	return nil
}
