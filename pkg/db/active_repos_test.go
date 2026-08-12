package db

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// The runner sweep needs the repos this fleet actually serves. Duplicates across
// job records collapse so a busy repo is not swept once per job.
func TestListActiveRepos_DedupesAndPaginates(t *testing.T) {
	page1 := []map[string]types.AttributeValue{
		{"repo": &types.AttributeValueMemberS{Value: "octo/a"}},
		{"repo": &types.AttributeValueMemberS{Value: "octo/a"}},
		{"repo": &types.AttributeValueMemberS{Value: "octo/b"}},
	}
	page2 := []map[string]types.AttributeValue{
		{"repo": &types.AttributeValueMemberS{Value: "octo/b"}},
		{"repo": &types.AttributeValueMemberS{Value: "octo/c"}},
		{"repo": &types.AttributeValueMemberS{Value: ""}},
	}

	calls := 0
	mockDB := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			calls++
			if params.ExclusiveStartKey == nil {
				return &dynamodb.ScanOutput{
					Items:            page1,
					LastEvaluatedKey: map[string]types.AttributeValue{"job_id": &types.AttributeValueMemberN{Value: "1"}},
				}, nil
			}
			return &dynamodb.ScanOutput{Items: page2}, nil
		},
	}
	client := &Client{dynamoClient: mockDB, jobsTable: "jobs-table"}

	got, err := client.ListActiveRepos(context.Background())
	if err != nil {
		t.Fatalf("ListActiveRepos() error = %v", err)
	}
	if calls != 2 {
		t.Errorf("scan calls = %d, want 2 (pagination not followed)", calls)
	}

	want := []string{"octo/a", "octo/b", "octo/c"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("repos = %v, want %v", got, want)
	}
}

// The jobs table keeps ~90 days; scanning all of it would grow the sweep's read
// cost without widening coverage, since a repo idle that long has nothing left
// to sweep.
func TestListActiveRepos_BoundsScanToRecentJobs(t *testing.T) {
	var gotFilter string
	mockDB := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			if params.FilterExpression != nil {
				gotFilter = *params.FilterExpression
			}
			return &dynamodb.ScanOutput{}, nil
		},
	}
	client := &Client{dynamoClient: mockDB, jobsTable: "jobs-table"}

	if _, err := client.ListActiveRepos(context.Background()); err != nil {
		t.Fatalf("ListActiveRepos() error = %v", err)
	}
	if gotFilter == "" {
		t.Fatal("scan has no filter expression; it reads the whole table")
	}
	if !strings.Contains(gotFilter, "created_at") {
		t.Errorf("filter %q does not bound by created_at", gotFilter)
	}
}

func TestListActiveRepos_NoTableConfigured(t *testing.T) {
	client := &Client{dynamoClient: &MockDynamoDBAPI{}}
	got, err := client.ListActiveRepos(context.Background())
	if err != nil {
		t.Errorf("ListActiveRepos() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("repos = %v, want empty", got)
	}
}
