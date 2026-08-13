package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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

// DeleteExpiredInstanceClaims sweeps the pools table for instance-claim rows
// whose claim_expiry has already passed and deletes them, returning the number
// actually removed. Claims are written by ClaimInstanceForJob but only
// overwritten on the next claim of the same instance, so without this reaper an
// ephemeral fleet accumulates one dead claim row per instance forever — the
// backlog that bloated the pools table past the 1 MB Scan page ListPools reads.
//
// Each row is deleted with a conditional DeleteItem (claim_expiry < :now) rather
// than an unconditional or batch delete: a claim renewed between the scan and the
// delete would otherwise be dropped while still live, causing double-assignment.
// A failed condition means the claim was renewed (or is a non-claim row) and is
// skipped, not counted, not an error.
func (c *Client) DeleteExpiredInstanceClaims(ctx context.Context, now time.Time) (int, error) {
	if c.poolsTable == "" {
		return 0, fmt.Errorf("pools table not configured")
	}

	return c.reapReservedRows(ctx, instanceClaimPrefix, "claim_expiry", now.Unix(), "expired instance claims")
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
			dbLog.Debug(ctx, "instance claim not held",
				slog.String(logging.KeyInstanceID, instanceID))
			return nil
		}
		return fmt.Errorf("failed to release instance claim: %w", err)
	}

	return nil
}

// HasLiveInstanceClaim reports whether a job currently holds a claim on an
// instance. A claim is written before the instance is started, so during that
// window EC2 still reports the instance stopped while it is already promised to
// a job — this is the only signal that says so.
//
// An unreadable claim counts as held: the caller uses this to decide whether
// destroying the instance is safe, and an unanswered question is not a yes.
func (c *Client) HasLiveInstanceClaim(ctx context.Context, instanceID string) (bool, error) {
	if instanceID == "" {
		return false, fmt.Errorf("instance ID cannot be empty")
	}
	if c.poolsTable == "" {
		return false, fmt.Errorf("pools table not configured")
	}

	out, err := c.dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: instanceClaimKey(instanceID)},
		},
		ProjectionExpression: aws.String("claim_expiry"),
		ConsistentRead:       aws.Bool(true),
	})
	if err != nil {
		return false, fmt.Errorf("failed to read instance claim for %s: %w", instanceID, err)
	}
	if len(out.Item) == 0 {
		return false, nil
	}

	expiry, ok := out.Item["claim_expiry"].(*types.AttributeValueMemberN)
	if !ok {
		// A claim row with no readable expiry cannot be judged expired.
		return true, nil
	}
	seconds, err := strconv.ParseInt(expiry.Value, 10, 64)
	if err != nil {
		return true, nil
	}
	return time.Now().Unix() < seconds, nil
}
