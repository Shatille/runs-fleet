package housekeeping

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func requeueJobItem(jobID int64, instanceID string, runID int64, retry int, status db.JobStatus) map[string]types.AttributeValue {
	item := map[string]types.AttributeValue{
		"job_id":        &types.AttributeValueMemberN{Value: strconv.FormatInt(jobID, 10)},
		"run_id":        &types.AttributeValueMemberN{Value: strconv.FormatInt(runID, 10)},
		"repo":          &types.AttributeValueMemberS{Value: "octo/repo"},
		"instance_type": &types.AttributeValueMemberS{Value: "c7g.large"},
		"pool":          &types.AttributeValueMemberS{Value: "default"},
		"retry_count":   &types.AttributeValueMemberN{Value: strconv.Itoa(retry)},
		"status":        &types.AttributeValueMemberS{Value: string(status)},
		"created_at":    &types.AttributeValueMemberS{Value: time.Now().Add(-time.Hour).Format(time.RFC3339)},
	}
	if instanceID != "" {
		item["instance_id"] = &types.AttributeValueMemberS{Value: instanceID}
	}
	return item
}

func TestFindRequeueableJobs_ScansRequestedStatuses(t *testing.T) {
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(1, "i-a", 10, 0, db.JobStatusLaunched),
		requeueJobItem(2, "i-b", 11, 1, db.JobStatusRunning),
	}}

	jobs, err := FindRequeueableJobs(context.Background(), dyn, "jobs-table", 15*time.Minute,
		[]db.JobStatus{db.JobStatusLaunched, db.JobStatusRunning})
	if err != nil {
		t.Fatalf("FindRequeueableJobs() error = %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(jobs))
	}
	// Verify all fields needed to rebuild a launch message are projected.
	got := jobs[0]
	if got.JobID != 1 || got.InstanceID != "i-a" || got.RunID != 10 || got.Repo != "octo/repo" ||
		got.InstanceType != "c7g.large" || got.Pool != "default" || got.RetryCount != 0 ||
		got.Status != string(db.JobStatusLaunched) {
		t.Errorf("projected fields wrong: %+v", got)
	}
}

func TestFindRequeueableJobs_SkipsRowsMissingJobID(t *testing.T) {
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		{"run_id": &types.AttributeValueMemberN{Value: "10"}}, // no job_id
		requeueJobItem(2, "i-b", 11, 0, db.JobStatusLaunched),
	}}

	jobs, err := FindRequeueableJobs(context.Background(), dyn, "jobs-table", 15*time.Minute,
		[]db.JobStatus{db.JobStatusLaunched})
	if err != nil {
		t.Fatalf("FindRequeueableJobs() error = %v", err)
	}
	if len(jobs) != 1 || jobs[0].JobID != 2 {
		t.Fatalf("expected only job 2, got %+v", jobs)
	}
}

func TestBuildRequeueMessage_ForcesOnDemandAndBumpsRetry(t *testing.T) {
	job := RequeueableJob{
		JobID:        42,
		RunID:        7,
		Repo:         "octo/repo",
		InstanceType: "c7g.large",
		Pool:         "default",
		RetryCount:   1,
	}
	msg := BuildRequeueMessage(job)
	if msg.JobID != 42 || msg.RunID != 7 || msg.Repo != "octo/repo" ||
		msg.InstanceType != "c7g.large" || msg.Pool != "default" {
		t.Errorf("message identity fields wrong: %+v", msg)
	}
	if msg.RetryCount != 2 {
		t.Errorf("expected RetryCount bumped to 2, got %d", msg.RetryCount)
	}
	if !msg.ForceOnDemand {
		t.Error("expected ForceOnDemand=true")
	}
	if msg.Spot {
		t.Error("expected Spot=false for reliability")
	}
}

func newRequeueDeps(ec2 *mockEC2API, dyn *mockTaskDynamoDBAPI, rq JobRequeuer) RequeueDeps {
	return RequeueDeps{
		EC2:          ec2,
		Scan:         dyn,
		Requeuer:     rq,
		JobsTable:    "jobs-table",
		TerminateEC2: ec2,
	}
}

// A launched job whose instance is still alive but whose runner never confirmed is
// terminated and requeued on-demand, and the record is flipped to requeued.
func TestRequeueHungJobs_RecoversAliveInstance(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-stuck", 7, 0, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{}

	res, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}
	if res.Requeued != 1 || res.Candidates != 1 {
		t.Errorf("expected requeued=1 candidates=1, got %+v", res)
	}
	if ec2.terminateCalls != 1 || len(ec2.terminatedIDs) != 1 || ec2.terminatedIDs[0] != "i-stuck" {
		t.Errorf("expected the alive dead-agent instance terminated; calls=%d ids=%v", ec2.terminateCalls, ec2.terminatedIDs)
	}
	if len(rq.sent) != 1 || rq.sent[0].RetryCount != 1 || !rq.sent[0].ForceOnDemand || rq.sent[0].RunID != 7 {
		t.Errorf("requeue message wrong: %+v", rq.sent)
	}
	if dyn.updateCalls != 1 {
		t.Errorf("expected record flipped to requeued (1 update), got %d", dyn.updateCalls)
	}
}

// A missing instance needs no termination but the job is still requeued.
func TestRequeueHungJobs_MissingInstanceRequeues(t *testing.T) {
	ec2 := &mockEC2API{instances: nil} // DescribeInstances returns no reservations => gone
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-gone", 7, 0, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{}

	res, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}
	if res.Requeued != 1 {
		t.Errorf("expected requeued=1, got %+v", res)
	}
	if ec2.terminateCalls != 0 {
		t.Errorf("missing instance must not be terminated, got %d terminate calls", ec2.terminateCalls)
	}
	if len(rq.sent) != 1 {
		t.Errorf("expected 1 requeue, got %d", len(rq.sent))
	}
}

// capturingScanDynamo records the ScanInput so the staleness/status guard expressed in
// the DynamoDB filter (which the broad shared mock does not evaluate) can be asserted.
type capturingScanDynamo struct {
	mockTaskDynamoDBAPI
	captured *dynamodb.ScanInput
}

func (m *capturingScanDynamo) Scan(ctx context.Context, in *dynamodb.ScanInput, opts ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	m.captured = in
	return m.mockTaskDynamoDBAPI.Scan(ctx, in, opts...)
}

// Healthy in-flight jobs are protected by the staleness threshold pushed into the
// DynamoDB filter (created_at < cutoff) and by restricting the scan to the requested
// statuses, so a fresh or wrong-status job is never even returned as a candidate.
func TestFindRequeueableJobs_FiltersByStalenessAndStatus(t *testing.T) {
	dyn := &capturingScanDynamo{}
	_, err := FindRequeueableJobs(context.Background(), dyn, "jobs-table", 15*time.Minute,
		[]db.JobStatus{db.JobStatusLaunched, db.JobStatusRunning})
	if err != nil {
		t.Fatalf("FindRequeueableJobs() error = %v", err)
	}
	if dyn.captured == nil || dyn.captured.FilterExpression == nil {
		t.Fatal("expected a filter expression to be set")
	}
	filter := *dyn.captured.FilterExpression
	if !strings.Contains(filter, "created_at < :cutoff") {
		t.Errorf("filter must exclude fresh jobs via created_at cutoff, got %q", filter)
	}
	cutoff, ok := dyn.captured.ExpressionAttributeValues[":cutoff"].(*types.AttributeValueMemberS)
	if !ok {
		t.Fatal("expected :cutoff string value")
	}
	parsed, perr := time.Parse(time.RFC3339, cutoff.Value)
	if perr != nil {
		t.Fatalf("cutoff not RFC3339: %v", perr)
	}
	if time.Since(parsed) < 14*time.Minute {
		t.Errorf("cutoff %v should be ~15m in the past", parsed)
	}
	// Both requested statuses must be present as filter values.
	var statusVals []string
	for k, v := range dyn.captured.ExpressionAttributeValues {
		if strings.HasPrefix(k, ":s") {
			if s, ok := v.(*types.AttributeValueMemberS); ok {
				statusVals = append(statusVals, s.Value)
			}
		}
	}
	if len(statusVals) != 2 {
		t.Errorf("expected 2 status filter values, got %v", statusVals)
	}
}

// Exhausted jobs (retry cap reached) are skipped, never requeued and never mutated —
// the operator action never destroys a job.
func TestRequeueHungJobs_ExhaustedSkipped(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-stuck", 7, MaxRequeueRetries, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{}

	res, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}
	if res.Requeued != 0 || res.SkippedExhausted != 1 {
		t.Errorf("expected requeued=0 skipped_exhausted=1, got %+v", res)
	}
	if len(rq.sent) != 0 {
		t.Errorf("exhausted job must not be requeued, got %d", len(rq.sent))
	}
	if dyn.updateCalls != 0 {
		t.Errorf("exhausted job must not be mutated, got %d updates", dyn.updateCalls)
	}
	if ec2.terminateCalls != 0 {
		t.Errorf("exhausted job instance must not be terminated, got %d", ec2.terminateCalls)
	}
}

// A job with no run_id cannot be rebuilt into a launch message and is skipped without
// sending or mutating.
func TestRequeueHungJobs_NoRunIDSkipped(t *testing.T) {
	item := requeueJobItem(42, "i-stuck", 0, 0, db.JobStatusLaunched)
	delete(item, "run_id")
	ec2 := &mockEC2API{instances: nil}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{item}}
	rq := &mockJobRequeuer{}

	res, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}
	if res.Requeued != 0 {
		t.Errorf("no-run_id job must not be requeued, got %+v", res)
	}
	if len(rq.sent) != 0 || dyn.updateCalls != 0 {
		t.Errorf("no-run_id job must not send or mutate; sent=%d updates=%d", len(rq.sent), dyn.updateCalls)
	}
}

// Dry run reports candidates without terminating, sending, or mutating anything.
func TestRequeueHungJobs_DryRun(t *testing.T) {
	ec2 := &mockEC2API{instances: nil}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-gone", 7, 0, db.JobStatusLaunched),
		requeueJobItem(43, "i-gone2", 8, 0, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{}

	res, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}
	if res.Candidates != 2 || res.Requeued != 0 {
		t.Errorf("dry run: expected candidates=2 requeued=0, got %+v", res)
	}
	if len(res.JobIDs) != 2 {
		t.Errorf("dry run should report candidate job IDs, got %v", res.JobIDs)
	}
	if len(rq.sent) != 0 || dyn.updateCalls != 0 || ec2.terminateCalls != 0 {
		t.Errorf("dry run must not mutate; sent=%d updates=%d terminate=%d", len(rq.sent), dyn.updateCalls, ec2.terminateCalls)
	}
}

// A send failure leaves the record unflipped (so a later attempt retries) and does not
// count the job as requeued.
func TestRequeueHungJobs_SendFailureDoesNotFlip(t *testing.T) {
	ec2 := &mockEC2API{instances: nil}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-gone", 7, 0, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{sendErr: errors.New("sqs down")}

	res, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}
	if res.Requeued != 0 {
		t.Errorf("send failure must not count as requeued, got %+v", res)
	}
	if dyn.updateCalls != 0 {
		t.Errorf("send failure must not flip the record, got %d updates", dyn.updateCalls)
	}
}

// Termination happens before the send. If the instance is terminated but the queue send
// then fails, the record must NOT be flipped — the next sweep re-dispatches (FIFO dedup
// keeps it idempotent), so the job is never lost.
func TestRequeueHungJobs_TerminateSucceedsSendFailsNoFlip(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-stuck", 7, 0, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{sendErr: errors.New("sqs down")}

	res, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}
	if ec2.terminateCalls != 1 {
		t.Errorf("alive dead-agent instance must be terminated before the send, got %d", ec2.terminateCalls)
	}
	if res.Requeued != 0 {
		t.Errorf("send failure must not count as requeued, got %+v", res)
	}
	if dyn.updateCalls != 0 {
		t.Errorf("send failure must not flip the record, got %d updates", dyn.updateCalls)
	}
}

func TestRequeueHungJobs_ScanError(t *testing.T) {
	ec2 := &mockEC2API{}
	dyn := &mockTaskDynamoDBAPI{scanErr: errors.New("dynamo down")}
	rq := &mockJobRequeuer{}

	_, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	})
	if err == nil {
		t.Fatal("expected error when scan fails")
	}
}
