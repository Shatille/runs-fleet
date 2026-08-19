package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func intPtr(n int) *int { return &n }

// A &0 override persists as an actual 0 attribute (force-cold), not dropped as an
// omitempty zero, so the three-state (unset / 0 / N) survives a save.
func TestSavePoolConfig_OverrideZeroPersists(t *testing.T) {
	t.Parallel()

	var captured *dynamodb.UpdateItemInput
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			captured = params
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	client := &Client{dynamoClient: mockDB, poolsTable: testPoolsTable}

	cfg := &PoolConfig{PoolName: "p", OverrideLingerMinutes: intPtr(0), OverrideMaxHot: intPtr(2)}
	if err := client.SavePoolConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SavePoolConfig() error = %v", err)
	}

	olm := captured.ExpressionAttributeValues[":olm"]
	n, ok := olm.(*types.AttributeValueMemberN)
	if !ok || n.Value != "0" {
		t.Errorf(":olm = %#v, want N=0 (force-cold override persisted, not dropped)", olm)
	}
	omh := captured.ExpressionAttributeValues[":omh"]
	if n2, ok := omh.(*types.AttributeValueMemberN); !ok || n2.Value != "2" {
		t.Errorf(":omh = %#v, want N=2", omh)
	}
}

// A nil override clears to NULL so an admin save removes the override.
func TestSavePoolConfig_OverrideNilClears(t *testing.T) {
	t.Parallel()

	var captured *dynamodb.UpdateItemInput
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			captured = params
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	client := &Client{dynamoClient: mockDB, poolsTable: testPoolsTable}

	if err := client.SavePoolConfig(context.Background(), &PoolConfig{PoolName: "p"}); err != nil {
		t.Fatalf("SavePoolConfig() error = %v", err)
	}

	if _, ok := captured.ExpressionAttributeValues[":olm"].(*types.AttributeValueMemberNULL); !ok {
		t.Errorf(":olm = %#v, want NULL (nil override clears)", captured.ExpressionAttributeValues[":olm"])
	}
}

// SavePoolConfig must never write auto_tune: it is tuner-owned, so an admin save
// cannot clobber a recommendation.
func TestSavePoolConfig_ExcludesAutoTune(t *testing.T) {
	t.Parallel()

	var captured *dynamodb.UpdateItemInput
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			captured = params
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	client := &Client{dynamoClient: mockDB, poolsTable: testPoolsTable}

	cfg := &PoolConfig{PoolName: "p", AutoTune: &AutoTuneRec{RecommendedLingerMinutes: 5, Reason: "tuned"}}
	if err := client.SavePoolConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SavePoolConfig() error = %v", err)
	}

	if strings.Contains(*captured.UpdateExpression, "auto_tune") {
		t.Errorf("UpdateExpression = %q, must NOT contain auto_tune", *captured.UpdateExpression)
	}
}

// UpdatePoolAutoTune writes only auto_tune (as a Map), guarded by
// attribute_exists(pool_name), and round-trips through GetPoolConfig.
func TestUpdatePoolAutoTune(t *testing.T) {
	t.Parallel()

	rec := AutoTuneRec{
		RecommendedLingerMinutes: 5,
		RecommendedMaxHot:        2,
		WindowDays:               7,
		JobCount:                 42,
		BurstCount:               3,
		P90IntraBurstGapSeconds:  240,
		PeakConcurrency:          2,
		Reason:                   "tuned",
		TunedAt:                  time.Date(2026, 7, 24, 1, 2, 3, 0, time.UTC),
	}

	var stored map[string]types.AttributeValue
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			if *params.UpdateExpression != "SET auto_tune = :m" {
				t.Errorf("UpdateExpression = %q, want exactly 'SET auto_tune = :m'", *params.UpdateExpression)
			}
			if params.ConditionExpression == nil || !strings.Contains(*params.ConditionExpression, "attribute_exists(pool_name)") {
				t.Errorf("ConditionExpression = %v, want attribute_exists(pool_name)", params.ConditionExpression)
			}
			m, ok := params.ExpressionAttributeValues[":m"].(*types.AttributeValueMemberM)
			if !ok {
				t.Fatalf(":m = %#v, want Map", params.ExpressionAttributeValues[":m"])
			}
			stored = map[string]types.AttributeValue{"pool_name": &types.AttributeValueMemberS{Value: "p"}, "auto_tune": m}
			return &dynamodb.UpdateItemOutput{}, nil
		},
		GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: stored}, nil
		},
	}
	client := &Client{dynamoClient: mockDB, poolsTable: testPoolsTable}

	if err := client.UpdatePoolAutoTune(context.Background(), "p", rec); err != nil {
		t.Fatalf("UpdatePoolAutoTune() error = %v", err)
	}

	got, err := client.GetPoolConfig(context.Background(), "p")
	if err != nil {
		t.Fatalf("GetPoolConfig() error = %v", err)
	}
	if got.AutoTune == nil {
		t.Fatal("AutoTune round-trip lost: got nil")
	}
	if *got.AutoTune != rec {
		t.Errorf("AutoTune round-trip = %+v, want %+v", *got.AutoTune, rec)
	}
}

func TestUpdatePoolAutoTune_PoolDeleted(t *testing.T) {
	t.Parallel()

	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, &types.ConditionalCheckFailedException{Message: nil}
		},
	}
	client := &Client{dynamoClient: mockDB, poolsTable: testPoolsTable}

	err := client.UpdatePoolAutoTune(context.Background(), "gone", AutoTuneRec{})
	if !errors.Is(err, ErrPoolNotFound) {
		t.Errorf("UpdatePoolAutoTune() error = %v, want ErrPoolNotFound", err)
	}
}

func TestUpdatePoolAutoTune_Validation(t *testing.T) {
	t.Parallel()

	client := &Client{dynamoClient: &MockDynamoDBAPI{}, poolsTable: testPoolsTable}
	if err := client.UpdatePoolAutoTune(context.Background(), "", AutoTuneRec{}); err == nil {
		t.Error("empty pool name: want error")
	}

	noTable := &Client{dynamoClient: &MockDynamoDBAPI{}, poolsTable: ""}
	if err := noTable.UpdatePoolAutoTune(context.Background(), "p", AutoTuneRec{}); err == nil {
		t.Error("no table: want error")
	}
}

// SavePoolConfig must never write the effective desired counts: they are
// reconciler-owned, so an admin save cannot clobber the resolved targets.
func TestSavePoolConfig_ExcludesEffectiveDesired(t *testing.T) {
	t.Parallel()

	var captured *dynamodb.UpdateItemInput
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			captured = params
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	client := &Client{dynamoClient: mockDB, poolsTable: testPoolsTable}

	running, stopped := 4, 2
	cfg := &PoolConfig{
		PoolName:                "p",
		EffectiveDesiredRunning: &running,
		EffectiveDesiredStopped: &stopped,
	}
	if err := client.SavePoolConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SavePoolConfig() error = %v", err)
	}

	if strings.Contains(*captured.UpdateExpression, "effective_desired") {
		t.Errorf("UpdateExpression = %q, must NOT contain effective_desired_*", *captured.UpdateExpression)
	}
}

// The nil/zero distinction must survive DynamoDB: a pool that never reconciled
// reads back nil, while a target that genuinely resolved to zero reads back &0.
// The UI picks its fallback on exactly that difference.
func TestGetPoolConfig_EffectiveDesiredNilVersusZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item map[string]types.AttributeValue
		want *int
	}{
		{
			name: "attribute absent reads back nil",
			item: map[string]types.AttributeValue{
				"pool_name": &types.AttributeValueMemberS{Value: "p"},
			},
			want: nil,
		},
		{
			name: "zero reads back a pointer to zero",
			item: map[string]types.AttributeValue{
				"pool_name":                 &types.AttributeValueMemberS{Value: "p"},
				"effective_desired_running": &types.AttributeValueMemberN{Value: "0"},
			},
			want: intPtr(0),
		},
		{
			name: "nonzero reads back its value",
			item: map[string]types.AttributeValue{
				"pool_name":                 &types.AttributeValueMemberS{Value: "p"},
				"effective_desired_running": &types.AttributeValueMemberN{Value: "4"},
			},
			want: intPtr(4),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockDB := &MockDynamoDBAPI{
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{Item: tt.item}, nil
				},
			}
			client := &Client{dynamoClient: mockDB, poolsTable: testPoolsTable}

			cfg, err := client.GetPoolConfig(context.Background(), "p")
			if err != nil {
				t.Fatalf("GetPoolConfig() error = %v", err)
			}
			got := cfg.EffectiveDesiredRunning
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("EffectiveDesiredRunning = %d, want nil", *got)
			case tt.want != nil && got == nil:
				t.Errorf("EffectiveDesiredRunning = nil, want %d", *tt.want)
			case tt.want != nil && *got != *tt.want:
				t.Errorf("EffectiveDesiredRunning = %d, want %d", *got, *tt.want)
			}
		})
	}
}
