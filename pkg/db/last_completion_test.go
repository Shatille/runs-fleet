package db

import (
	"context"
	"strconv"
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

			got, _, err := client.LastJobCompletionForInstance(context.Background(), "i-test")
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
	if _, _, err := client.LastJobCompletionForInstance(context.Background(), "i-test"); err == nil {
		t.Error("got nil error without an instance-id GSI configured")
	}
}

func TestLastJobCompletionForInstance_ReturnsNewestJobRef(t *testing.T) {
	t.Parallel()

	older := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	newer := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)

	withJob := func(jobID int64, repo, completedAt string) map[string]types.AttributeValue {
		item := completionItem("success", completedAt)
		item["job_id"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(jobID, 10)}
		item["repo"] = &types.AttributeValueMemberS{Value: repo}
		return item
	}

	mockDB := &MockDynamoDBAPI{
		QueryFunc: func(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
			return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{
				withJob(111, "octo/old", older.Format(time.RFC3339)),
				withJob(222, "octo/new", newer.Format(time.RFC3339)),
			}}, nil
		},
	}
	client := &Client{
		dynamoClient:      mockDB,
		jobsTable:         "jobs-table",
		jobsInstanceIDGSI: "instance-id-index",
	}

	got, ref, err := client.LastJobCompletionForInstance(context.Background(), "i-test")
	if err != nil {
		t.Fatalf("LastJobCompletionForInstance() error = %v", err)
	}
	if !got.Equal(newer) {
		t.Errorf("completion = %v, want the newest %v", got, newer)
	}
	if ref.JobID != 222 || ref.Repo != "octo/new" {
		t.Errorf("ref = %+v, want the newest completion's job (222, octo/new)", ref)
	}
}

func TestLastJobCompletionForInstance_UndecodableNewestRowErrors(t *testing.T) {
	t.Parallel()

	older := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	newer := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)

	oldRow := completionItem("success", older.Format(time.RFC3339))
	oldRow["job_id"] = &types.AttributeValueMemberN{Value: "111"}
	oldRow["repo"] = &types.AttributeValueMemberS{Value: "octo/old"}

	newRow := completionItem("success", newer.Format(time.RFC3339))
	newRow["job_id"] = &types.AttributeValueMemberS{Value: "not-a-number"}

	mockDB := &MockDynamoDBAPI{
		QueryFunc: func(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
			return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{oldRow, newRow}}, nil
		},
	}
	client := &Client{
		dynamoClient:      mockDB,
		jobsTable:         "jobs-table",
		jobsInstanceIDGSI: "instance-id-index",
	}

	got, ref, err := client.LastJobCompletionForInstance(context.Background(), "i-test")
	if err == nil {
		t.Fatal("LastJobCompletionForInstance() error = nil, want an error: an undecodable record must not be reported as a clean completion")
	}
	if !got.IsZero() {
		t.Errorf("completion = %v, want zero alongside the error", got)
	}
	if ref.JobID != 0 || ref.Repo != "" {
		t.Errorf("ref = %+v, want zero alongside the error", ref)
	}
}

func TestLastJobCompletionForInstance_UndecodableOlderRowIsNotDecoded(t *testing.T) {
	t.Parallel()

	older := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	newer := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)

	oldRow := completionItem("success", older.Format(time.RFC3339))
	oldRow["job_id"] = &types.AttributeValueMemberS{Value: "not-a-number"}

	newRow := completionItem("success", newer.Format(time.RFC3339))
	newRow["job_id"] = &types.AttributeValueMemberN{Value: "222"}
	newRow["repo"] = &types.AttributeValueMemberS{Value: "octo/new"}

	mockDB := &MockDynamoDBAPI{
		QueryFunc: func(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
			return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{oldRow, newRow}}, nil
		},
	}
	client := &Client{
		dynamoClient:      mockDB,
		jobsTable:         "jobs-table",
		jobsInstanceIDGSI: "instance-id-index",
	}

	got, ref, err := client.LastJobCompletionForInstance(context.Background(), "i-test")
	if err != nil {
		t.Fatalf("LastJobCompletionForInstance() error = %v, want nil: only the winning row's identity is read", err)
	}
	if !got.Equal(newer) {
		t.Errorf("completion = %v, want the newest %v", got, newer)
	}
	if ref.JobID != 222 || ref.Repo != "octo/new" {
		t.Errorf("ref = %+v, want the newest completion's job (222, octo/new)", ref)
	}
}

func TestLastJobCompletionForInstance_NewestRowWithoutJobRefClearsStaleRef(t *testing.T) {
	t.Parallel()

	older := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	newer := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)

	oldRow := completionItem("success", older.Format(time.RFC3339))
	oldRow["job_id"] = &types.AttributeValueMemberN{Value: "111"}
	oldRow["repo"] = &types.AttributeValueMemberS{Value: "octo/old"}

	newRow := completionItem("success", newer.Format(time.RFC3339))

	mockDB := &MockDynamoDBAPI{
		QueryFunc: func(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
			return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{oldRow, newRow}}, nil
		},
	}
	client := &Client{
		dynamoClient:      mockDB,
		jobsTable:         "jobs-table",
		jobsInstanceIDGSI: "instance-id-index",
	}

	got, ref, err := client.LastJobCompletionForInstance(context.Background(), "i-test")
	if err != nil {
		t.Fatalf("LastJobCompletionForInstance() error = %v", err)
	}
	if !got.Equal(newer) {
		t.Errorf("completion = %v, want the newest %v", got, newer)
	}
	if ref.JobID != 0 || ref.Repo != "" {
		t.Errorf("ref = %+v, want zero: it must never describe a job other than the newest completion", ref)
	}
}
