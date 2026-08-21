package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fleetDayPrefix keys the per-UTC-day fleet cost rollups the fleet-cost sampler
// accumulates into, alongside task locks, instance claims, and runner sightings.
//
// These rows carry NO expiry and no reaper. They are the only surviving record
// of what the fleet cost: job records are hard-deleted after 7 days by the
// old-jobs housekeeping task, so a cost figure derived from them silently
// truncates. One row per day is ~365 rows a year, which the pools table absorbs
// without trouble.
const fleetDayPrefix = "__fleet_day:"

// fleetDayKey keys one UTC day's rollup. day is "2006-01-02"; the lexicographic
// ordering of that layout makes a string range scan a date range scan.
func fleetDayKey(day string) string { return fleetDayPrefix + day }

// FleetDayFormat is the layout for the day key of a fleet cost rollup.
const FleetDayFormat = "2006-01-02"

// FleetCostDelta is one sampling tick's contribution to a day's fleet cost.
// Every monetary and duration field accumulates; Partial latches.
type FleetCostDelta struct {
	TotalCost   float64
	ComputeCost float64
	EBSCost     float64

	// InstanceSeconds is instance-time observed this tick, summed across every
	// managed instance. AttributedSeconds is the part of it spent on instances
	// that were running a job, so coverage comes from one sampling pass rather
	// than from dividing two independently sourced numbers.
	InstanceSeconds   float64
	AttributedSeconds float64

	// Partial marks a tick whose elapsed window had to be clamped, meaning the
	// day is known to understate. It latches true for the whole day.
	Partial bool

	// SampledAt checkpoints this tick so the next one can price the elapsed gap
	// rather than assuming a fixed interval.
	SampledAt time.Time
}

// FleetCostDay is one UTC day's accumulated fleet cost.
type FleetCostDay struct {
	Day               string
	TotalCost         float64
	ComputeCost       float64
	EBSCost           float64
	InstanceSeconds   float64
	AttributedSeconds float64
	Partial           bool
	LastSampleAt      time.Time
}

// AddFleetCostSample folds one sampling tick into its day's rollup.
//
// The monetary and duration fields use DynamoDB's atomic ADD rather than a
// read-modify-write: several orchestrator replicas may sample concurrently, and
// a lost update would undercount the fleet with no external signal. last_sample_at
// is SET because it is a checkpoint rather than an accumulator.
func (c *Client) AddFleetCostSample(ctx context.Context, day string, d FleetCostDelta) error {
	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}
	if day == "" {
		return fmt.Errorf("day cannot be empty")
	}

	update := "ADD cost_usd :cost, compute_usd :compute, ebs_usd :ebs, " +
		"instance_seconds :inst, attributed_seconds :attr " +
		"SET last_sample_at = :at, #day = :day"
	exprValues := map[string]types.AttributeValue{
		":cost":    numAttr(d.TotalCost),
		":compute": numAttr(d.ComputeCost),
		":ebs":     numAttr(d.EBSCost),
		":inst":    numAttr(d.InstanceSeconds),
		":attr":    numAttr(d.AttributedSeconds),
		":at":      &types.AttributeValueMemberN{Value: strconv.FormatInt(d.SampledAt.Unix(), 10)},
		":day":     &types.AttributeValueMemberS{Value: day},
	}
	// Latch, never clear: one clamped tick means the day understates, and a
	// later clean tick does not undo that.
	if d.Partial {
		update += ", partial = :partial"
		exprValues[":partial"] = &types.AttributeValueMemberBOOL{Value: true}
	}

	_, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: fleetDayKey(day)},
		},
		UpdateExpression:          aws.String(update),
		ExpressionAttributeNames:  map[string]string{"#day": "day"},
		ExpressionAttributeValues: exprValues,
	})
	if err != nil {
		return fmt.Errorf("failed to add fleet cost sample for %s: %w", day, err)
	}
	return nil
}

// GetFleetCostDays returns the rollups for [fromDay, toDay], both inclusive and
// both formatted as FleetDayFormat. Days with no rollup are simply absent —
// a gap is data (the sampler was not running), not an error.
func (c *Client) GetFleetCostDays(ctx context.Context, fromDay, toDay string) ([]FleetCostDay, error) {
	if c.poolsTable == "" {
		return nil, fmt.Errorf("pools table not configured")
	}

	input := &dynamodb.ScanInput{
		TableName: aws.String(c.poolsTable),
		// The sampler reads last_sample_at from here and then writes the window
		// since it, so a stale read would re-price a window the previous tick
		// already counted. The housekeeping task lock serializes executions but
		// does not make an eventually-consistent read fresh.
		ConsistentRead:   aws.Bool(true),
		FilterExpression: aws.String("begins_with(pool_name, :p) AND #day BETWEEN :from AND :to"),
		ExpressionAttributeNames: map[string]string{
			"#day": "day",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":p":    &types.AttributeValueMemberS{Value: fleetDayPrefix},
			":from": &types.AttributeValueMemberS{Value: fromDay},
			":to":   &types.AttributeValueMemberS{Value: toDay},
		},
	}

	var days []FleetCostDay
	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := c.dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to scan fleet cost days: %w", err)
		}

		for _, item := range output.Items {
			days = append(days, fleetCostDayFromItem(item))
		}

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			return days, nil
		}
	}
}

func fleetCostDayFromItem(item map[string]types.AttributeValue) FleetCostDay {
	d := FleetCostDay{
		Day:               getStringAttr(item, "day"),
		TotalCost:         getFloatAttr(item, "cost_usd"),
		ComputeCost:       getFloatAttr(item, "compute_usd"),
		EBSCost:           getFloatAttr(item, "ebs_usd"),
		InstanceSeconds:   getFloatAttr(item, "instance_seconds"),
		AttributedSeconds: getFloatAttr(item, "attributed_seconds"),
	}
	d.Partial = getBoolAttr(item, "partial")
	if v, ok := item["last_sample_at"].(*types.AttributeValueMemberN); ok {
		if secs, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
			d.LastSampleAt = time.Unix(secs, 0).UTC()
		}
	}
	return d
}

// ListBusyInstanceIDs returns every instance fleet-wide whose job occupies it,
// across all pools and cold-start instances alike.
//
// GetPoolBusyInstanceIDs cannot answer this: the pool-status GSI is keyed on
// pool, and a cold-start instance has none. Coverage has to see those instances
// or it would report the whole cold-start fleet as unattributed.
//
// An unconfigured jobs table yields no IDs and no error: fleet cost is still
// worth recording when the attributed share cannot be determined.
func (c *Client) ListBusyInstanceIDs(ctx context.Context) ([]string, error) {
	if c.jobsTable == "" {
		return nil, nil
	}

	// Bind the same statuses the pool reconciler treats as occupying an
	// instance, so "busy" means one thing across the codebase.
	placeholders := make([]string, len(busyJobStatuses))
	exprValues := make(map[string]types.AttributeValue, len(busyJobStatuses))
	for i, status := range busyJobStatuses {
		ph := ":s" + strconv.Itoa(i)
		placeholders[i] = ph
		exprValues[ph] = &types.AttributeValueMemberS{Value: string(status)}
	}

	// Bound by age as well as status, matching occupiesInstance: an active record
	// older than maxConcurrencyRuntime no longer occupies its instance, and a
	// leaked stale row would otherwise mark an instance busy forever and inflate
	// the attributed share.
	//
	// This is a correctness bound, not a cost one — DynamoDB applies a filter
	// after the read, so it trims the response, not the RCU.
	exprValues[":since"] = &types.AttributeValueMemberS{
		Value: time.Now().UTC().Add(-maxConcurrencyRuntime).Format(time.RFC3339),
	}

	input := &dynamodb.ScanInput{
		TableName: aws.String(c.jobsTable),
		FilterExpression: aws.String(
			"#status IN (" + strings.Join(placeholders, ", ") + ") AND created_at >= :since"),
		ExpressionAttributeNames:  map[string]string{"#status": "status"},
		ExpressionAttributeValues: exprValues,
		ProjectionExpression:      aws.String("instance_id"),
	}

	var ids []string
	seen := make(map[string]struct{})
	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := c.dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to scan busy instances: %w", err)
		}
		for _, id := range extractInstanceIDs(output.Items) {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			return ids, nil
		}
	}
}

func numAttr(v float64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatFloat(v, 'f', -1, 64)}
}

func getFloatAttr(item map[string]types.AttributeValue, key string) float64 {
	if v, ok := item[key]; ok {
		if n, ok := v.(*types.AttributeValueMemberN); ok {
			val, _ := strconv.ParseFloat(n.Value, 64)
			return val
		}
	}
	return 0
}
