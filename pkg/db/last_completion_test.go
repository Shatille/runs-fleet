package db

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func completionItem(status, completedAt string) map[string]types.AttributeValue {
	item := map[string]types.AttributeValue{
		"instance_id": &types.AttributeValueMemberS{Value: "i-test"},
		"status":      &types.AttributeValueMemberS{Value: status},
	}
	if completedAt != "" {
		item["completed_at"] = &types.AttributeValueMemberS{Value: completedAt}
	}
	return item
}

func TestLastJobCompletionForInstance(t *testing.T) {
	t.Parallel()

	older := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	newer := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)

	tests := []struct {
		name  string
		items []map[string]types.AttributeValue
		want  time.Time
	}{
		{
			name:  "no rows",
			items: nil,
		},
		{
			// The agent's vocabulary is what lands in the table, so "failure" must
			// count as finished even though JobStatusFailed spells it "failed".
			name:  "agent failure counts as terminal",
			items: []map[string]types.AttributeValue{completionItem("failure", newer.Format(time.RFC3339))},
			want:  newer,
		},
		{
			name:  "interrupted counts as terminal",
			items: []map[string]types.AttributeValue{completionItem("interrupted", newer.Format(time.RFC3339))},
			want:  newer,
		},
		{
			name:  "running job is not finished",
			items: []map[string]types.AttributeValue{completionItem("running", "")},
		},
		{
			// orphaned is stamped on a swallowed EC2 lookup error, so trusting it
			// would let a transient API fault reap a live instance.
			name:  "orphaned is not trusted",
			items: []map[string]types.AttributeValue{completionItem("orphaned", newer.Format(time.RFC3339))},
		},
		{
			name: "any live row disqualifies the instance",
			items: []map[string]types.AttributeValue{
				completionItem("success", older.Format(time.RFC3339)),
				completionItem("running", ""),
			},
		},
		{
			name: "reports the most recent completion",
			items: []map[string]types.AttributeValue{
				completionItem("success", older.Format(time.RFC3339)),
				completionItem("success", newer.Format(time.RFC3339)),
			},
			want: newer,
		},
		{
			name:  "terminal row without completed_at is unusable",
			items: []map[string]types.AttributeValue{completionItem("success", "")},
		},
		{
			name:  "unparseable timestamp is unusable",
			items: []map[string]types.AttributeValue{completionItem("success", "not-a-time")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockDB := &MockDynamoDBAPI{
				QueryFunc: func(_ context.Context, params *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
					if params.IndexName == nil {
						t.Error("query omitted IndexName; instance_id is not the base table key")
					}
					return &dynamodb.QueryOutput{Items: tt.items}, nil
				},
			}
			client := &Client{
				dynamoClient:      mockDB,
				jobsTable:         "jobs-table",
				jobsInstanceIDGSI: "instance-id-index",
			}

			got, err := client.LastJobCompletionForInstance(context.Background(), "i-test")
			if err != nil {
				t.Fatalf("LastJobCompletionForInstance() error = %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("LastJobCompletionForInstance() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLastJobCompletionForInstance_NoGSI(t *testing.T) {
	t.Parallel()

	client := &Client{dynamoClient: &MockDynamoDBAPI{}, jobsTable: "jobs-table"}

	// Reaping destroys instances, so it declines rather than falling back to an
	// unindexed scan that could answer off a partial page.
	if _, err := client.LastJobCompletionForInstance(context.Background(), "i-test"); err == nil {
		t.Error("got nil error without an instance-id GSI configured")
	}
}
