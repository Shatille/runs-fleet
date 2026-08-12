package housekeeping

import (
	"context"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/internal/handler"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Every layer here was individually correct while the label still evaporated
// between them: the webhook set it, the record dropped it, and the rebuilt
// message fell back to a form the job could never match. This walks the whole
// recovery path — stored record, sweep scan, launch message, registration label
// — and asserts the label the workflow asked for comes out the far end.
//
// Mutating any single link fails it: unset original_label on the record, drop it
// from the scan projection, or omit it from BuildRequeueMessage.
func TestRequeue_PreservesRequestedLabelEndToEnd(t *testing.T) {
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		labelledJobItem(94023466800, requestedLabel),
	}}

	jobs, err := FindRequeueableJobs(context.Background(), dyn, "jobs-table", time.Minute, []db.JobStatus{db.JobStatusLaunched})
	if err != nil {
		t.Fatalf("FindRequeueableJobs() error = %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d requeue candidates, want 1", len(jobs))
	}

	msg := BuildRequeueMessage(jobs[0])
	if msg.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", msg.RetryCount)
	}

	// The registration label is what GitHub matches against runs-on. A recovery
	// that reaches the synthesized fallback produces a runner for a job that can
	// never be handed to it.
	got := handler.BuildRunnerLabel(context.Background(), msg)
	if got != requestedLabel {
		t.Errorf("registration label = %q, want %q", got, requestedLabel)
	}
}
