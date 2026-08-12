package handler

import (
	"context"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/google/go-github/v57/github"
)

// The failure-requeue path rebuilds its launch message from the stored record,
// so it drops the requested label unless it reads it back. A re-dispatch under
// the synthesized fallback registers a runner GitHub can never hand this job to.
func TestHandleJobFailure_RequeuePreservesOriginalLabel(t *testing.T) {
	const want = "runs-fleet/cpu=2/pool=lingua-franca"

	item, err := attributevalue.MarshalMap(map[string]any{
		"job_id":         int64(94023466800),
		"run_id":         int64(31566820776),
		"repo":           "owner/repo",
		"retry_count":    0,
		"status":         "running",
		"original_label": want,
	})
	if err != nil {
		t.Fatalf("MarshalMap() unexpected error: %v", err)
	}

	fake := &fakeDynamoForFailure{
		getItem: func() (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
	}
	dbc := db.NewClientWithAPI(fake, "pools", "jobs")

	var sent *queue.JobMessage
	mockQueue := &MockQueue{
		SendMessageFunc: func(_ context.Context, m *queue.JobMessage) error {
			sent = m
			return nil
		},
	}

	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         github.Int64(94023466800),
			RunnerName: github.String("runs-fleet-i-1234567890abcdef0"),
			Labels:     []string{want},
		},
	}

	requeued, err := HandleJobFailure(context.Background(), event, mockQueue, dbc, nil)
	if err != nil {
		t.Fatalf("HandleJobFailure() unexpected error: %v", err)
	}
	if !requeued {
		t.Fatal("HandleJobFailure() should requeue the job")
	}
	if sent == nil {
		t.Fatal("no requeue message sent")
	}
	if sent.OriginalLabel != want {
		t.Errorf("requeued OriginalLabel = %q, want %q", sent.OriginalLabel, want)
	}
	if got := BuildRunnerLabel(context.Background(), sent); got != want {
		t.Errorf("registration label = %q, want %q", got, want)
	}
}
