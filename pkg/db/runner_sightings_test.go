package db

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// The orchestrator runs multiple replicas and the housekeeping task lock only
// serializes one tick, so consecutive sweeps are usually run by different
// replicas. An in-memory streak would therefore never accumulate; the first
// sighting has to be durable and shared.
func TestRecordRunnerOffline_FirstSightingIsDurableAndReturnsZeroAge(t *testing.T) {
	var put map[string]types.AttributeValue
	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			put = params.ExpressionAttributeValues
			return &dynamodb.UpdateItemOutput{
				Attributes: map[string]types.AttributeValue{
					"first_seen_offline": put[":now"],
				},
			}, nil
		},
	}
	client := &Client{dynamoClient: mockDB, poolsTable: "pools-table"}

	// Sub-second: the stamp is stored as whole Unix seconds, so a first sighting
	// reads back as the current second rather than exactly zero.
	age, err := client.RecordRunnerOffline(context.Background(), "octo/repo", 42, time.Now())
	if err != nil {
		t.Fatalf("RecordRunnerOffline() error = %v", err)
	}
	if age >= time.Second {
		t.Errorf("age on first sighting = %v, want under a second", age)
	}
}

// A later sweep must read back the ORIGINAL first-seen stamp, not overwrite it,
// so the age keeps growing until it clears the confirmation window.
func TestRecordRunnerOffline_KeepsOriginalStampAndReportsAge(t *testing.T) {
	firstSeen := time.Now().Add(-3 * time.Hour)

	mockDB := &MockDynamoDBAPI{
		UpdateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			if params.UpdateExpression == nil {
				t.Fatal("no update expression")
			}
			return &dynamodb.UpdateItemOutput{
				Attributes: map[string]types.AttributeValue{
					"first_seen_offline": &types.AttributeValueMemberN{
						Value: itoa(firstSeen.Unix()),
					},
				},
			}, nil
		},
	}
	client := &Client{dynamoClient: mockDB, poolsTable: "pools-table"}

	age, err := client.RecordRunnerOffline(context.Background(), "octo/repo", 42, time.Now())
	if err != nil {
		t.Fatalf("RecordRunnerOffline() error = %v", err)
	}
	if age < 2*time.Hour || age > 4*time.Hour {
		t.Errorf("age = %v, want ~3h (original stamp must survive)", age)
	}
}

// A runner that comes back online, or is deleted, must not leave a stamp behind
// that would instantly condemn a future registration reusing that id.
func TestForgetRunnerOffline_DeletesTheSighting(t *testing.T) {
	var deletedKey map[string]types.AttributeValue
	mockDB := &MockDynamoDBAPI{
		DeleteItemFunc: func(_ context.Context, params *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
			deletedKey = params.Key
			return &dynamodb.DeleteItemOutput{}, nil
		},
	}
	client := &Client{dynamoClient: mockDB, poolsTable: "pools-table"}

	if err := client.ForgetRunnerOffline(context.Background(), "octo/repo", 42); err != nil {
		t.Fatalf("ForgetRunnerOffline() error = %v", err)
	}
	got, ok := deletedKey["pool_name"].(*types.AttributeValueMemberS)
	if !ok {
		t.Fatal("delete key missing pool_name")
	}
	if got.Value != runnerSightingKey("octo/repo", 42) {
		t.Errorf("deleted key = %q, want %q", got.Value, runnerSightingKey("octo/repo", 42))
	}
}

func TestRunnerSightingKey_IsScopedPerRepo(t *testing.T) {
	if runnerSightingKey("octo/a", 1) == runnerSightingKey("octo/b", 1) {
		t.Error("runner ids are per-repo; keys for different repos must not collide")
	}
}
