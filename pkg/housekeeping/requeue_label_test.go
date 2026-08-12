package housekeeping

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const requestedLabel = "runs-fleet/cpu=2/pool=lingua-franca"

func labelledJobItem(jobID int64, label string) map[string]types.AttributeValue {
	item := map[string]types.AttributeValue{
		"job_id":        &types.AttributeValueMemberN{Value: strconv.FormatInt(jobID, 10)},
		"run_id":        &types.AttributeValueMemberN{Value: "31566820776"},
		"repo":          &types.AttributeValueMemberS{Value: "octo/repo"},
		"instance_type": &types.AttributeValueMemberS{Value: "c7g.large"},
		"pool":          &types.AttributeValueMemberS{Value: "lingua-franca"},
		"retry_count":   &types.AttributeValueMemberN{Value: "0"},
		"status":        &types.AttributeValueMemberS{Value: string(db.JobStatusLaunched)},
		"instance_id":   &types.AttributeValueMemberS{Value: "i-a"},
		"created_at":    &types.AttributeValueMemberS{Value: time.Now().Add(-time.Hour).Format(time.RFC3339)},
	}
	if label != "" {
		item["original_label"] = &types.AttributeValueMemberS{Value: label}
	}
	return item
}

// The scan must both request original_label from DynamoDB and map it onto the
// candidate. The projection is asserted on the emitted ScanInput because an
// unprojected attribute is simply absent from the real response — a stub that
// returns whole items cannot reproduce that, so dropping it from the projection
// would otherwise pass.
func TestFindRequeueableJobs_ProjectsOriginalLabel(t *testing.T) {
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		labelledJobItem(1, requestedLabel),
	}}

	jobs, err := FindRequeueableJobs(context.Background(), dyn, "jobs-table", time.Minute, []db.JobStatus{db.JobStatusLaunched})
	if err != nil {
		t.Fatalf("FindRequeueableJobs() error = %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].OriginalLabel != requestedLabel {
		t.Errorf("OriginalLabel = %q, want %q", jobs[0].OriginalLabel, requestedLabel)
	}
	if len(dyn.scanInputs) == 0 {
		t.Fatal("no scan input captured")
	}
	if proj := aws.ToString(dyn.scanInputs[0].ProjectionExpression); !strings.Contains(proj, "original_label") {
		t.Errorf("projection %q does not request original_label", proj)
	}
}

// A re-dispatch exists to give a starving job a runner it can actually match, so
// the message must carry the label the workflow asked for.
func TestBuildRequeueMessage_PreservesOriginalLabel(t *testing.T) {
	msg := BuildRequeueMessage(RequeueableJob{
		JobID:         42,
		RunID:         7,
		Repo:          "octo/repo",
		InstanceType:  "c7g.large",
		Pool:          "lingua-franca",
		OriginalLabel: requestedLabel,
	})
	if msg.OriginalLabel != requestedLabel {
		t.Errorf("OriginalLabel = %q, want %q", msg.OriginalLabel, requestedLabel)
	}
}
