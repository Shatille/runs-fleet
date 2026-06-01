package db

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func pageKey(value string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"job_id": &types.AttributeValueMemberN{Value: value},
	}
}

func TestGetJobByInstanceViaScan_Paginates(t *testing.T) {
	t.Parallel()

	match, err := attributevalue.MarshalMap(jobRecord{
		InstanceID: "i-target",
		JobID:      42,
		Status:     string(JobStatusRunning),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var calls int
	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			calls++
			if calls == 1 {
				if params.ExclusiveStartKey != nil {
					t.Error("first page should not set ExclusiveStartKey")
				}
				return &dynamodb.ScanOutput{
					Items:            []map[string]types.AttributeValue{},
					LastEvaluatedKey: pageKey("1"),
				}, nil
			}
			if params.ExclusiveStartKey == nil {
				t.Error("second page must carry ExclusiveStartKey")
			}
			return &dynamodb.ScanOutput{
				Items: []map[string]types.AttributeValue{match},
			}, nil
		},
	}

	client := &Client{dynamoClient: mock, jobsTable: "jobs-table"}

	info, err := client.GetJobByInstance(context.Background(), "i-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 scan pages, got %d", calls)
	}
	if info == nil {
		t.Fatal("expected job from second page, got nil")
	}
	if info.JobID != 42 {
		t.Errorf("got job_id %d, want 42", info.JobID)
	}
}

func TestQueryPoolJobHistoryViaScan_Paginates(t *testing.T) {
	t.Parallel()

	now := time.Now()
	since := now.Add(-time.Hour)

	page1, _ := attributevalue.MarshalMap(map[string]interface{}{
		"job_id":     int64(1),
		"pool":       "default",
		"created_at": now.Add(-30 * time.Minute).Format(time.RFC3339),
	})
	page2, _ := attributevalue.MarshalMap(map[string]interface{}{
		"job_id":     int64(2),
		"pool":       "default",
		"created_at": now.Add(-20 * time.Minute).Format(time.RFC3339),
	})

	var calls int
	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			calls++
			if calls == 1 {
				return &dynamodb.ScanOutput{
					Items:            []map[string]types.AttributeValue{page1},
					LastEvaluatedKey: pageKey("1"),
				}, nil
			}
			if params.ExclusiveStartKey == nil {
				t.Error("second page must carry ExclusiveStartKey")
			}
			return &dynamodb.ScanOutput{
				Items: []map[string]types.AttributeValue{page2},
			}, nil
		},
	}

	client := &Client{dynamoClient: mock, jobsTable: "jobs-table"}

	entries, err := client.queryPoolJobHistoryViaScan(context.Background(), "default", since)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 scan pages, got %d", calls)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (both pages)", len(entries))
	}
}

func TestGetPoolBusyInstanceIDsViaScan_Paginates(t *testing.T) {
	t.Parallel()

	var calls int
	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			calls++
			if calls == 1 {
				return &dynamodb.ScanOutput{
					Items:            []map[string]types.AttributeValue{{"instance_id": &types.AttributeValueMemberS{Value: "i-aaa"}}},
					LastEvaluatedKey: pageKey("1"),
				}, nil
			}
			if params.ExclusiveStartKey == nil {
				t.Error("second page must carry ExclusiveStartKey")
			}
			return &dynamodb.ScanOutput{
				Items: []map[string]types.AttributeValue{{"instance_id": &types.AttributeValueMemberS{Value: "i-bbb"}}},
			}, nil
		},
	}

	client := &Client{dynamoClient: mock, jobsTable: "jobs-table"}

	ids, err := client.GetPoolBusyInstanceIDs(context.Background(), "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 scan pages, got %d", calls)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d instance IDs, want 2 (both pages)", len(ids))
	}
}

func TestListJobsForAdmin_Paginates(t *testing.T) {
	t.Parallel()

	page1, _ := attributevalue.MarshalMap(map[string]interface{}{
		"job_id":     int64(1),
		"created_at": time.Now().Add(-30 * time.Minute).Format(time.RFC3339),
	})
	page2, _ := attributevalue.MarshalMap(map[string]interface{}{
		"job_id":     int64(2),
		"created_at": time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
	})

	var calls int
	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			calls++
			if calls == 1 {
				return &dynamodb.ScanOutput{
					Items:            []map[string]types.AttributeValue{page1},
					LastEvaluatedKey: pageKey("1"),
				}, nil
			}
			if params.ExclusiveStartKey == nil {
				t.Error("second page must carry ExclusiveStartKey")
			}
			return &dynamodb.ScanOutput{
				Items: []map[string]types.AttributeValue{page2},
			}, nil
		},
	}

	client := &Client{dynamoClient: mock, jobsTable: "jobs-table"}

	entries, total, err := client.ListJobsForAdmin(context.Background(), AdminJobFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 scan pages, got %d", calls)
	}
	if total != 2 || len(entries) != 2 {
		t.Fatalf("got total=%d entries=%d, want 2 (both pages)", total, len(entries))
	}
}

func TestGetJobStatsForAdmin_Paginates(t *testing.T) {
	t.Parallel()

	page1, _ := attributevalue.MarshalMap(map[string]interface{}{
		"job_id":     int64(1),
		"status":     string(JobStatusSuccess),
		"created_at": time.Now().Add(-30 * time.Minute).Format(time.RFC3339),
	})
	page2, _ := attributevalue.MarshalMap(map[string]interface{}{
		"job_id":     int64(2),
		"status":     string(JobStatusFailed),
		"created_at": time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
	})

	var calls int
	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			calls++
			if calls == 1 {
				return &dynamodb.ScanOutput{
					Items:            []map[string]types.AttributeValue{page1},
					LastEvaluatedKey: pageKey("1"),
				}, nil
			}
			if params.ExclusiveStartKey == nil {
				t.Error("second page must carry ExclusiveStartKey")
			}
			return &dynamodb.ScanOutput{
				Items: []map[string]types.AttributeValue{page2},
			}, nil
		},
	}

	client := &Client{dynamoClient: mock, jobsTable: "jobs-table"}

	stats, err := client.GetJobStatsForAdmin(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 scan pages, got %d", calls)
	}
	if stats.Total != 2 {
		t.Fatalf("got total=%d, want 2 (both pages)", stats.Total)
	}
	if stats.Completed != 1 || stats.Failed != 1 {
		t.Errorf("got completed=%d failed=%d, want 1 each", stats.Completed, stats.Failed)
	}
}
