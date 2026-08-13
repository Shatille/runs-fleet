package db

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// runnerSightingPrefix keys the first-offline-sighting rows the orphaned-runner
// sweep writes into the pools table, alongside instance claims.
const runnerSightingPrefix = "__runner_offline:"

// runnerSightingTTL bounds how long an unrevisited sighting survives. The row is
// deleted the moment a runner comes back online or is deregistered; this only
// reclaims rows for registrations that vanished without either.
//
// DynamoDB TTL cannot do this: the pools table already spends its single TTL
// attribute on claim_expiry, so the ttl attribute written below is inert and the
// rows are reaped by DeleteStaleRunnerSightings instead.
const runnerSightingTTL = 7 * 24 * time.Hour

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// runnerSightingKey scopes a sighting to its repo: runner ids are allocated per
// repo, so the same id means different runners in different repos.
func runnerSightingKey(repo string, runnerID int64) string {
	return runnerSightingPrefix + repo + ":" + itoa(runnerID)
}

// RecordRunnerOffline stamps the first time a runner registration was seen
// offline and returns how long it has been offline since.
//
// The stamp is durable because the orchestrator runs multiple replicas and the
// housekeeping task lock only serializes a single tick — consecutive sweeps are
// usually run by different replicas, so a per-process counter would never
// accumulate. Returns 0 on the first sighting.
func (c *Client) RecordRunnerOffline(ctx context.Context, repo string, runnerID int64, now time.Time) (time.Duration, error) {
	if c.poolsTable == "" {
		return 0, fmt.Errorf("pools table not configured")
	}

	out, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: runnerSightingKey(repo, runnerID)},
		},
		// if_not_exists keeps the ORIGINAL stamp: overwriting it every sweep
		// would reset the age and the registration could never age out.
		UpdateExpression: aws.String("SET first_seen_offline = if_not_exists(first_seen_offline, :now), #ttl = :ttl"),
		ExpressionAttributeNames: map[string]string{
			"#ttl": "ttl",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now": &types.AttributeValueMemberN{Value: itoa(now.Unix())},
			":ttl": &types.AttributeValueMemberN{Value: itoa(now.Add(runnerSightingTTL).Unix())},
		},
		ReturnValues: types.ReturnValueAllNew,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to record runner sighting: %w", err)
	}

	stamp, ok := out.Attributes["first_seen_offline"].(*types.AttributeValueMemberN)
	if !ok {
		return 0, nil
	}
	secs, err := strconv.ParseInt(stamp.Value, 10, 64)
	if err != nil {
		return 0, nil
	}
	age := now.Sub(time.Unix(secs, 0))
	if age < 0 {
		return 0, nil
	}
	return age, nil
}

// ForgetRunnerOffline drops a runner's offline sighting, so a registration that
// came back online (or was removed) starts over if it is ever seen offline again.
func (c *Client) ForgetRunnerOffline(ctx context.Context, repo string, runnerID int64) error {
	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	_, err := c.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: runnerSightingKey(repo, runnerID)},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to forget runner sighting: %w", err)
	}
	return nil
}

// DeleteStaleRunnerSightings removes sighting rows older than runnerSightingTTL
// and returns how many were deleted.
//
// The sweep that writes these rows only revisits repos ListActiveRepos still
// reports, so a repo that stops using runs-fleet strands its sightings, and the
// table's TTL slot is taken by claim_expiry. Without this reaper those rows
// accumulate in the pools table forever.
//
// A runner seen offline again between the scan and the delete keeps its row: the
// conditional delete in reapReservedRows rejects the rewritten stamp, so the
// reaper cannot restart an offline clock and delay a deregistration.
func (c *Client) DeleteStaleRunnerSightings(ctx context.Context, now time.Time) (int, error) {
	cutoff := now.Add(-runnerSightingTTL).Unix()
	return c.reapReservedRows(ctx, runnerSightingPrefix, "first_seen_offline", cutoff, "stale runner sightings")
}
