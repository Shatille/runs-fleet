package db

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// A fleet-day row lives in the pools table behind a sentinel prefix. Every such
// prefix MUST be reserved: a row the pool enumerators mistake for a real pool is
// reconciled as a phantom and inflates per-pool metric cardinality. This has
// already happened twice with earlier sentinel kinds.
func TestFleetDayKeysAreReservedPoolKeys(t *testing.T) {
	t.Parallel()

	if !IsReservedPoolKey(fleetDayKey("2026-08-20")) {
		t.Errorf("IsReservedPoolKey(%q) = false, want true", fleetDayKey("2026-08-20"))
	}
	// A real pool that merely starts with similar text must stay a real pool.
	for _, real := range []string{"fleet", "fleet-day", "default"} {
		if IsReservedPoolKey(real) {
			t.Errorf("IsReservedPoolKey(%q) = true, want false for a real pool", real)
		}
	}
}

func TestFleetDayKeyUsesTheUTCDate(t *testing.T) {
	t.Parallel()

	if got, want := fleetDayKey("2026-08-20"), fleetDayPrefix+"2026-08-20"; got != want {
		t.Errorf("fleetDayKey() = %q, want %q", got, want)
	}
}

// The sampler accumulates into a running per-day total. It must use DynamoDB's
// atomic ADD, not read-modify-write: several orchestrator replicas can sample
// concurrently and a lost update would silently undercount the fleet.
func TestAddFleetCostSampleAccumulatesAtomically(t *testing.T) {
	t.Parallel()

	var captured *dynamodb.UpdateItemInput
	mock := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			captured = params
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	c := &Client{dynamoClient: mock, poolsTable: "pools"}

	err := c.AddFleetCostSample(context.Background(), "2026-08-20", FleetCostDelta{
		TotalCost:         0.5,
		ComputeCost:       0.4,
		EBSCost:           0.1,
		InstanceSeconds:   120,
		AttributedSeconds: 60,
		SampledAt:         time.Unix(1_800_000_000, 0),
	})
	if err != nil {
		t.Fatalf("AddFleetCostSample() error = %v", err)
	}
	if captured == nil {
		t.Fatal("no UpdateItem issued")
	}

	expr := aws.ToString(captured.UpdateExpression)
	if !strings.Contains(expr, "ADD ") {
		t.Errorf("UpdateExpression = %q, want an atomic ADD so concurrent replicas cannot lose an increment", expr)
	}
	for _, attr := range []string{"cost_usd", "compute_usd", "ebs_usd", "instance_seconds", "attributed_seconds"} {
		if !strings.Contains(expr, attr) {
			t.Errorf("UpdateExpression = %q, missing accumulator %q", expr, attr)
		}
	}
	// last_sample_at is a checkpoint, not an accumulator: it must be SET.
	if !strings.Contains(expr, "SET") || !strings.Contains(expr, "last_sample_at") {
		t.Errorf("UpdateExpression = %q, want last_sample_at SET as a checkpoint", expr)
	}

	key, ok := captured.Key["pool_name"].(*types.AttributeValueMemberS)
	if !ok || key.Value != fleetDayKey("2026-08-20") {
		t.Errorf("key = %#v, want %q", captured.Key["pool_name"], fleetDayKey("2026-08-20"))
	}
}

// A tick that had to clamp its elapsed window marks the day partial, so the API
// can say the number understates rather than presenting it as complete.
func TestAddFleetCostSampleFlagsAPartialDay(t *testing.T) {
	t.Parallel()

	var expr string
	mock := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			expr = aws.ToString(params.UpdateExpression)
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	c := &Client{dynamoClient: mock, poolsTable: "pools"}

	if err := c.AddFleetCostSample(context.Background(), "2026-08-20", FleetCostDelta{
		TotalCost: 0.1, Partial: true, SampledAt: time.Unix(1_800_000_000, 0),
	}); err != nil {
		t.Fatalf("AddFleetCostSample() error = %v", err)
	}
	if !strings.Contains(expr, "partial") {
		t.Errorf("UpdateExpression = %q, want the partial flag recorded", expr)
	}
}

func TestAddFleetCostSampleRequiresAPoolsTable(t *testing.T) {
	t.Parallel()

	c := &Client{dynamoClient: &MockDynamoDBAPI{}}
	if err := c.AddFleetCostSample(context.Background(), "2026-08-20", FleetCostDelta{}); err == nil {
		t.Fatal("AddFleetCostSample() error = nil, want an error when the pools table is unconfigured")
	}
}

// The rollups are the only surviving record of fleet spend, so the reader must
// return every day in the window and skip nothing silently.
func TestGetFleetCostDaysReturnsEachDayInTheWindow(t *testing.T) {
	t.Parallel()

	row := func(day string, cost float64) map[string]types.AttributeValue {
		return map[string]types.AttributeValue{
			"pool_name":          &types.AttributeValueMemberS{Value: fleetDayKey(day)},
			"day":                &types.AttributeValueMemberS{Value: day},
			"cost_usd":           &types.AttributeValueMemberN{Value: strconv.FormatFloat(cost, 'f', -1, 64)},
			"compute_usd":        &types.AttributeValueMemberN{Value: strconv.FormatFloat(cost, 'f', -1, 64)},
			"ebs_usd":            &types.AttributeValueMemberN{Value: "0"},
			"instance_seconds":   &types.AttributeValueMemberN{Value: "600"},
			"attributed_seconds": &types.AttributeValueMemberN{Value: "300"},
		}
	}

	var scans int
	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			scans++
			if scans == 1 {
				if params.FilterExpression == nil || !strings.Contains(*params.FilterExpression, "begins_with(pool_name, :p)") {
					t.Errorf("Scan FilterExpression = %v, want it scoped to the fleet-day prefix", params.FilterExpression)
				}
				return &dynamodb.ScanOutput{
					Items:            []map[string]types.AttributeValue{row("2026-08-19", 1.5)},
					LastEvaluatedKey: map[string]types.AttributeValue{"pool_name": &types.AttributeValueMemberS{Value: "x"}},
				}, nil
			}
			if params.ExclusiveStartKey == nil {
				t.Error("second page must carry ExclusiveStartKey")
			}
			return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{row("2026-08-20", 2.5)}}, nil
		},
	}
	c := &Client{dynamoClient: mock, poolsTable: "pools"}

	days, err := c.GetFleetCostDays(context.Background(), "2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("GetFleetCostDays() error = %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("got %d days, want 2 (pagination must be followed)", len(days))
	}

	total := 0.0
	for _, d := range days {
		total += d.TotalCost
	}
	if total != 4.0 {
		t.Errorf("summed cost = %v, want 4.0", total)
	}
}

// No rollups yet (fresh deploy, or sampler disabled) is not an error: the API
// omits the fleet figure entirely rather than reporting a false zero.
func TestGetFleetCostDaysIsEmptyNotAnErrorWhenNothingIsRecorded(t *testing.T) {
	t.Parallel()

	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			return &dynamodb.ScanOutput{}, nil
		},
	}
	c := &Client{dynamoClient: mock, poolsTable: "pools"}

	days, err := c.GetFleetCostDays(context.Background(), "2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("GetFleetCostDays() error = %v", err)
	}
	if len(days) != 0 {
		t.Errorf("got %d days, want none", len(days))
	}
}

// Coverage needs to know which instances were actually running a job, fleet-wide.
// The pool-status GSI is keyed on pool, so it cannot answer this for cold-start
// instances, which carry no pool — hence a dedicated scan over the same busy
// statuses pool reconciliation treats as occupied.
func TestListBusyInstanceIDsCoversEveryBusyStatusFleetWide(t *testing.T) {
	t.Parallel()

	var filter string
	var values map[string]types.AttributeValue
	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			filter = aws.ToString(params.FilterExpression)
			values = params.ExpressionAttributeValues
			return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{
				{"instance_id": &types.AttributeValueMemberS{Value: "i-aaa"}},
				{"instance_id": &types.AttributeValueMemberS{Value: "i-bbb"}},
			}}, nil
		},
	}
	c := &Client{dynamoClient: mock, jobsTable: "jobs"}

	ids, err := c.ListBusyInstanceIDs(context.Background())
	if err != nil {
		t.Fatalf("ListBusyInstanceIDs() error = %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d instance IDs, want 2", len(ids))
	}
	// Every status the rest of the system treats as occupying an instance must
	// be bound here too, or coverage undercounts busy time.
	for _, status := range busyJobStatuses {
		found := false
		for _, v := range values {
			if s, ok := v.(*types.AttributeValueMemberS); ok && s.Value == string(status) {
				found = true
			}
		}
		if !found {
			t.Errorf("status %q not bound into the filter %q", status, filter)
		}
	}
}

// An unconfigured jobs table must not fail the sampler: the fleet cost is still
// worth recording even when the attributed share cannot be determined.
func TestListBusyInstanceIDsIsEmptyWithoutAJobsTable(t *testing.T) {
	t.Parallel()

	c := &Client{dynamoClient: &MockDynamoDBAPI{}}
	ids, err := c.ListBusyInstanceIDs(context.Background())
	if err != nil {
		t.Fatalf("ListBusyInstanceIDs() error = %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("got %d IDs, want none", len(ids))
	}
}
