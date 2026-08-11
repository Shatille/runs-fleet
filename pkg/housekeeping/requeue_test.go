package housekeeping

import (
	"context"
	"errors"
	"slices"
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

func newRequeueDepsWithMetrics(ec2 *mockEC2API, dyn *mockTaskDynamoDBAPI, rq JobRequeuer, m MetricsAPI) RequeueDeps {
	deps := newRequeueDeps(ec2, dyn, rq)
	deps.Metrics = m
	return deps
}

// A launched job whose instance is still alive but whose runner never confirmed is
// terminated and requeued on-demand, and the record is flipped to requeued.
func TestRequeueHungJobs_RecoversAliveInstance(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-stuck", 7, 0, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{}
	metrics := &mockTaskMetricsAPI{}

	res, err := RequeueHungJobs(context.Background(), newRequeueDepsWithMetrics(ec2, dyn, rq, metrics), RequeueOptions{
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
	if want := []string{requeueReasonOperator}; !slices.Equal(metrics.requeuedReasons, want) {
		t.Errorf("expected requeued reasons %v, got %v", want, metrics.requeuedReasons)
	}
	if len(metrics.schedulingFailures) != 0 {
		t.Errorf("a successful requeue must not emit a scheduling failure, got %v", metrics.schedulingFailures)
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
	metrics := &mockTaskMetricsAPI{}

	res, err := RequeueHungJobs(context.Background(), newRequeueDepsWithMetrics(ec2, dyn, rq, metrics), RequeueOptions{
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
	if want := []string{requeueReasonOperator}; !slices.Equal(metrics.schedulingFailures, want) {
		t.Errorf("expected scheduling failures %v, got %v", want, metrics.schedulingFailures)
	}
	if len(metrics.requeuedReasons) != 0 {
		t.Errorf("exhausted job must not emit a requeue metric, got %v", metrics.requeuedReasons)
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
		requeueJobItem(43, "i-gone2", 8, MaxRequeueRetries, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{}
	metrics := &mockTaskMetricsAPI{}

	res, err := RequeueHungJobs(context.Background(), newRequeueDepsWithMetrics(ec2, dyn, rq, metrics), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}
	if res.Candidates != 2 || res.Requeued != 0 || res.SkippedExhausted != 1 {
		t.Errorf("dry run: expected candidates=2 requeued=0 skipped_exhausted=1, got %+v", res)
	}
	// Only the non-exhausted candidate is a would-requeue; the exhausted one is
	// reported as skipped but never as a JobID.
	if len(res.JobIDs) != 1 || res.JobIDs[0] != 42 {
		t.Errorf("dry run should report only requeue-able candidate ids, got %v", res.JobIDs)
	}
	if len(rq.sent) != 0 || dyn.updateCalls != 0 || ec2.terminateCalls != 0 {
		t.Errorf("dry run must not mutate; sent=%d updates=%d terminate=%d", len(rq.sent), dyn.updateCalls, ec2.terminateCalls)
	}
	// A dry run reports but must emit nothing — not even for the exhausted candidate.
	if len(metrics.requeuedReasons) != 0 || len(metrics.schedulingFailures) != 0 {
		t.Errorf("dry run must not emit metrics; requeued=%v failures=%v", metrics.requeuedReasons, metrics.schedulingFailures)
	}
}

// A send failure must leave the record back in launched (so a later sweep
// retries) and must not count the job as requeued.
func TestRequeueHungJobs_SendFailureLeavesRecordLaunched(t *testing.T) {
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
	// The flip is written before the send so the record is claimable; a failed
	// send rolls it back, so the record must not be left in requeued.
	if dyn.updateCalls != 2 {
		t.Errorf("expected the flip to be rolled back after a failed send, got %d updates", dyn.updateCalls)
	}
}

// Termination happens before the send. If the instance is terminated but the queue send
// then fails, the record must end up back in launched so the next sweep re-dispatches it.
func TestRequeueHungJobs_TerminateSucceedsSendFailsRollsBack(t *testing.T) {
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
	if dyn.updateCalls != 2 {
		t.Errorf("expected the flip to be rolled back after a failed send, got %d updates", dyn.updateCalls)
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

// The scan snapshot can be minutes old. A runner that confirmed since then has a
// live job on the instance, so the operator sweep must re-read the record and
// leave it alone rather than terminating a working runner mid-job.
func TestRequeueHungJobs_SkipsJobThatConfirmed(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-confirmed")}}
	dyn := &mockTaskDynamoDBAPI{
		items:          []map[string]types.AttributeValue{requeueJobItem(42, "i-confirmed", 7, 0, db.JobStatusLaunched)},
		statusOverride: map[int64]string{42: string(db.JobStatusRunning)},
	}
	rq := &mockJobRequeuer{}

	result, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}

	if ec2.terminateCalls != 0 {
		t.Errorf("a confirmed runner must not be terminated, got %d terminate calls", ec2.terminateCalls)
	}
	if len(rq.sent) != 0 {
		t.Errorf("a confirmed runner's job must not be requeued, got %d", len(rq.sent))
	}
	if result.Requeued != 0 {
		t.Errorf("expected 0 requeued, got %d", result.Requeued)
	}
}

// An inconclusive re-read means the job's true state is unknown; the sweep must
// never terminate on uncertainty.
func TestRequeueHungJobs_PreflightErrorSkips(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{
		items:      []map[string]types.AttributeValue{requeueJobItem(42, "i-stuck", 7, 0, db.JobStatusLaunched)},
		getItemErr: errors.New("dynamo unavailable"),
	}
	rq := &mockJobRequeuer{}

	result, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}

	if ec2.terminateCalls != 0 || len(rq.sent) != 0 || result.Requeued != 0 {
		t.Errorf("must not act on an unknown job state: terminates=%d sends=%d requeued=%d",
			ec2.terminateCalls, len(rq.sent), result.Requeued)
	}
}

// A dry run must report what a real run would act on, so it applies the same
// pre-flight rather than listing candidates the real sweep would skip.
func TestRequeueHungJobs_DryRunAppliesPreflight(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-confirmed")}}
	dyn := &mockTaskDynamoDBAPI{
		items:          []map[string]types.AttributeValue{requeueJobItem(42, "i-confirmed", 7, 0, db.JobStatusLaunched)},
		statusOverride: map[int64]string{42: string(db.JobStatusRunning)},
	}
	rq := &mockJobRequeuer{}

	result, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}

	if len(result.JobIDs) != 0 {
		t.Errorf("a dry run must not list a job the real sweep would skip, got %v", result.JobIDs)
	}
	if ec2.terminateCalls != 0 {
		t.Errorf("a dry run must never terminate, got %d", ec2.terminateCalls)
	}
}

// The record must be claimable before the message is visible to a worker:
// ClaimJob rejects a record still reading launched as already-claimed and drops
// the message, so a send-then-flip ordering strands the job.
func TestRequeueHungJobs_FlipsRecordBeforeSending(t *testing.T) {
	var order []string
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{
		items:        []map[string]types.AttributeValue{requeueJobItem(42, "i-stuck", 7, 0, db.JobStatusLaunched)},
		onUpdateItem: func() { order = append(order, "flip") },
	}
	rq := &mockJobRequeuer{onSend: func() { order = append(order, "send") }}

	if _, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	}); err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}

	if len(order) != 2 || order[0] != "flip" || order[1] != "send" {
		t.Errorf("expected the record flipped before the send, got order %v", order)
	}
}

// A flip whose send then fails would strand the job: the sweep scans launched
// records only, so a record left in requeued with no message is invisible to
// every future sweep. The flip must be rolled back so the next sweep retries.
func TestRequeueHungJobs_SendFailureRollsBackFlip(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	var updates []*dynamodb.UpdateItemInput
	dyn := &mockTaskDynamoDBAPI{
		items:          []map[string]types.AttributeValue{requeueJobItem(42, "i-stuck", 7, 0, db.JobStatusLaunched)},
		captureUpdates: &updates,
	}
	rq := &mockJobRequeuer{sendErr: errors.New("sqs unavailable")}

	result, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}
	if result.Requeued != 0 {
		t.Errorf("expected 0 requeued after a failed send, got %d", result.Requeued)
	}

	// Two writes: the flip to requeued, then the rollback to launched.
	if len(updates) != 2 {
		t.Fatalf("expected flip + rollback writes, got %d", len(updates))
	}
	rollback := updates[1]
	got := rollback.ExpressionAttributeValues[":from"]
	if v, ok := got.(*types.AttributeValueMemberS); !ok || v.Value != string(db.JobStatusLaunched) {
		t.Errorf("rollback must restore the launched status, got %v", got)
	}
	if rollback.ConditionExpression == nil || !strings.Contains(*rollback.ConditionExpression, "requeued") {
		t.Errorf("rollback must be conditioned on the record still being requeued, got %v", rollback.ConditionExpression)
	}
}

// Two sweeps can scan the same launched job before either writes. The one whose
// conditional flip loses must NOT send: its sibling owns the dispatch, and a
// message sent on state the sibling may still roll back would be dropped by
// ClaimJob (the record reads launched again) and the job would be stranded.
func TestRequeueHungJobs_LostFlipDoesNotSend(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{
		items: []map[string]types.AttributeValue{requeueJobItem(42, "i-stuck", 7, 0, db.JobStatusLaunched)},
		// A sibling sweep already flipped the record, so this sweep's guarded
		// write fails its condition.
		updateErr: &types.ConditionalCheckFailedException{},
	}
	rq := &mockJobRequeuer{}

	result, err := RequeueHungJobs(context.Background(), newRequeueDeps(ec2, dyn, rq), RequeueOptions{
		Threshold: 15 * time.Minute,
		Statuses:  []db.JobStatus{db.JobStatusLaunched},
	})
	if err != nil {
		t.Fatalf("RequeueHungJobs() error = %v", err)
	}

	if len(rq.sent) != 0 {
		t.Errorf("a sweep whose flip lost must not send, got %d messages", len(rq.sent))
	}
	if result.Requeued != 0 {
		t.Errorf("expected 0 requeued when the flip was lost, got %d", result.Requeued)
	}
}

// The single-job action re-dispatches exactly like the sweep: the alive
// dead-agent instance is terminated first, the record is flipped, and the
// launch message goes out on-demand with a bumped retry count.
func TestRequeueJob_RecoversAliveInstance(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-dead-agent")}}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-dead-agent", 7, 0, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{}
	metrics := &mockTaskMetricsAPI{}

	res, err := RequeueJob(context.Background(), newRequeueDepsWithMetrics(ec2, dyn, rq, metrics), 42, RequeueJobOptions{})
	if err != nil {
		t.Fatalf("RequeueJob() error = %v", err)
	}
	if res.Outcome != OutcomeRequeued {
		t.Errorf("expected outcome %q, got %q", OutcomeRequeued, res.Outcome)
	}
	if !res.InstanceTerminated || ec2.terminatedIDs == nil || ec2.terminatedIDs[0] != "i-dead-agent" {
		t.Errorf("expected i-dead-agent terminated; terminated=%v ids=%v", res.InstanceTerminated, ec2.terminatedIDs)
	}
	if len(rq.sent) != 1 || rq.sent[0].RetryCount != 1 || !rq.sent[0].ForceOnDemand {
		t.Errorf("requeue message wrong: %+v", rq.sent)
	}
	if res.RetryCount != 1 {
		t.Errorf("expected the resulting retry count 1, got %d", res.RetryCount)
	}
	if want := []string{requeueReasonOperator}; !slices.Equal(metrics.requeuedReasons, want) {
		t.Errorf("expected requeued reasons %v, got %v", want, metrics.requeuedReasons)
	}
}

// The operator picked this row deliberately, so the sweep's staleness threshold
// does not apply: a job launched seconds ago is still re-dispatchable.
func TestRequeueJob_IgnoresAgeThreshold(t *testing.T) {
	item := requeueJobItem(42, "i-gone", 7, 0, db.JobStatusLaunched)
	item["created_at"] = &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{item}}
	rq := &mockJobRequeuer{}

	res, err := RequeueJob(context.Background(), newRequeueDeps(&mockEC2API{}, dyn, rq), 42, RequeueJobOptions{})
	if err != nil {
		t.Fatalf("RequeueJob() error = %v", err)
	}
	if res.Outcome != OutcomeRequeued || len(rq.sent) != 1 {
		t.Errorf("a freshly launched job must still be requeue-able, got %+v (sent=%d)", res, len(rq.sent))
	}
	if dyn.scanCalls != 0 {
		t.Errorf("the single-job path must read the record directly, not scan; got %d scans", dyn.scanCalls)
	}
}

// A gone instance needs no termination.
func TestRequeueJob_MissingInstanceSkipsTerminate(t *testing.T) {
	ec2 := &mockEC2API{}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-gone", 7, 0, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{}

	res, err := RequeueJob(context.Background(), newRequeueDeps(ec2, dyn, rq), 42, RequeueJobOptions{})
	if err != nil {
		t.Fatalf("RequeueJob() error = %v", err)
	}
	if res.Outcome != OutcomeRequeued || res.InstanceTerminated {
		t.Errorf("expected a requeue with no termination, got %+v", res)
	}
	if ec2.terminateCalls != 0 {
		t.Errorf("a missing instance must not be terminated, got %d calls", ec2.terminateCalls)
	}
}

// The retry cap still binds: the operator button is a backstop, not an override.
func TestRequeueJob_RefusesExhausted(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-dead-agent")}}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-dead-agent", 7, MaxRequeueRetries, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{}

	res, err := RequeueJob(context.Background(), newRequeueDeps(ec2, dyn, rq), 42, RequeueJobOptions{})
	if err != nil {
		t.Fatalf("RequeueJob() error = %v", err)
	}
	if res.Outcome != OutcomeExhausted || res.RetryCount != MaxRequeueRetries {
		t.Errorf("expected an exhausted refusal, got %+v", res)
	}
	if len(rq.sent) != 0 || dyn.updateCalls != 0 || ec2.terminateCalls != 0 {
		t.Errorf("an exhausted job must not be touched; sent=%d updates=%d terminates=%d",
			len(rq.sent), dyn.updateCalls, ec2.terminateCalls)
	}
}

func TestRequeueJob_NotFound(t *testing.T) {
	dyn := &mockTaskDynamoDBAPI{}
	res, err := RequeueJob(context.Background(), newRequeueDeps(&mockEC2API{}, dyn, &mockJobRequeuer{}), 42, RequeueJobOptions{})
	if err != nil {
		t.Fatalf("RequeueJob() error = %v", err)
	}
	if res.Outcome != OutcomeNotFound {
		t.Errorf("expected a not-found outcome, got %+v", res)
	}
}

// A read failure is not a missing job: acting on an unknown state could kill a
// live runner, so it surfaces as an error rather than a refusal.
func TestRequeueJob_ReadErrorSurfaces(t *testing.T) {
	dyn := &mockTaskDynamoDBAPI{getItemErr: errors.New("dynamo unavailable")}
	_, err := RequeueJob(context.Background(), newRequeueDeps(&mockEC2API{}, dyn, &mockJobRequeuer{}), 42, RequeueJobOptions{})
	if err == nil {
		t.Fatal("expected an error when the record cannot be read")
	}
}

// Same invariant as the sweep: a flip whose send fails must be rolled back, or
// the record sits in requeued with no message and no sweep ever finds it again.
func TestRequeueJob_SendFailureRollsBackFlip(t *testing.T) {
	var updates []*dynamodb.UpdateItemInput
	dyn := &mockTaskDynamoDBAPI{
		items:          []map[string]types.AttributeValue{requeueJobItem(42, "i-gone", 7, 0, db.JobStatusLaunched)},
		captureUpdates: &updates,
	}
	rq := &mockJobRequeuer{sendErr: errors.New("sqs unavailable")}

	res, err := RequeueJob(context.Background(), newRequeueDeps(&mockEC2API{}, dyn, rq), 42, RequeueJobOptions{})
	if err == nil {
		t.Fatal("expected an error when the send fails")
	}
	if res.Outcome != OutcomeSendFailed {
		t.Errorf("expected a send-failed outcome, got %+v", res)
	}
	if len(updates) != 2 {
		t.Fatalf("expected the flip and its rollback, got %d updates", len(updates))
	}
	rollback := updates[1]
	if !strings.Contains(*rollback.UpdateExpression, string(db.JobStatusLaunched)) &&
		!strings.Contains(*rollback.ConditionExpression, "#status") {
		t.Errorf("second update does not look like a rollback: %+v", rollback)
	}
}

// A sibling sweep that already owns the dispatch must not be double-sent.
func TestRequeueJob_LostFlipDoesNotSend(t *testing.T) {
	dyn := &mockTaskDynamoDBAPI{
		items:     []map[string]types.AttributeValue{requeueJobItem(42, "i-gone", 7, 0, db.JobStatusLaunched)},
		updateErr: &types.ConditionalCheckFailedException{},
	}
	rq := &mockJobRequeuer{}

	res, err := RequeueJob(context.Background(), newRequeueDeps(&mockEC2API{}, dyn, rq), 42, RequeueJobOptions{})
	if err != nil {
		t.Fatalf("RequeueJob() error = %v", err)
	}
	if res.Outcome != OutcomeLostRace || len(rq.sent) != 0 {
		t.Errorf("expected a lost-race refusal with no send, got %+v (sent=%d)", res, len(rq.sent))
	}
}

// mockQueuedChecker stands in for GitHub's view of a job.
type mockQueuedChecker struct {
	status string
	err    error
	calls  int
}

func (m *mockQueuedChecker) GetWorkflowJobStatus(_ context.Context, _ string, _ int64) (string, error) {
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	return m.status, nil
}

func newRequeueDepsWithGitHub(ec2 *mockEC2API, dyn *mockTaskDynamoDBAPI, rq JobRequeuer, gh JobQueuedChecker) RequeueDeps {
	deps := newRequeueDeps(ec2, dyn, rq)
	deps.GitHub = gh
	return deps
}

// The production hang: our record says running because a runner confirmed, but
// GitHub never handed it this job. GitHub reporting queued is proof no work is
// in progress, which is the only thing that makes terminating the instance safe.
func TestRequeueJob_RunningIsRequeuedWhenGitHubStillHasItQueued(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-idle")}}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-idle", 7, 0, db.JobStatusRunning),
	}}
	rq := &mockJobRequeuer{}
	gh := &mockQueuedChecker{status: "queued"}

	res, err := RequeueJob(context.Background(), newRequeueDepsWithGitHub(ec2, dyn, rq, gh), 42, RequeueJobOptions{})
	if err != nil {
		t.Fatalf("RequeueJob() error = %v", err)
	}
	if res.Outcome != OutcomeRequeued {
		t.Fatalf("expected a requeue, got %+v", res)
	}
	if res.GitHubStatus != "queued" {
		t.Errorf("GitHubStatus = %q, want the observed status carried back", res.GitHubStatus)
	}
	if !res.InstanceTerminated || len(ec2.terminatedIDs) != 1 || ec2.terminatedIDs[0] != "i-idle" {
		t.Errorf("the idle instance must be terminated first; terminated=%v ids=%v", res.InstanceTerminated, ec2.terminatedIDs)
	}
	if len(rq.sent) != 1 || rq.sent[0].RetryCount != 1 {
		t.Errorf("requeue message wrong: %+v", rq.sent)
	}
}

// The gate keys on "not launched", not on "running", so claiming has to be held
// to it too — a refactor that special-cased running would silently exempt it.
func TestRequeueJob_ClaimingIsGatedLikeRunning(t *testing.T) {
	tests := []struct {
		ghStatus    string
		wantOutcome RequeueOutcome
		wantSends   int
	}{
		{ghStatus: "queued", wantOutcome: OutcomeRequeued, wantSends: 1},
		{ghStatus: "in_progress", wantOutcome: OutcomeNotQueued},
	}

	for _, tt := range tests {
		t.Run(tt.ghStatus, func(t *testing.T) {
			ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-claiming")}}
			dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
				requeueJobItem(42, "i-claiming", 7, 0, db.JobStatusClaiming),
			}}
			rq := &mockJobRequeuer{}
			gh := &mockQueuedChecker{status: tt.ghStatus}

			res, err := RequeueJob(context.Background(), newRequeueDepsWithGitHub(ec2, dyn, rq, gh), 42, RequeueJobOptions{})
			if err != nil {
				t.Fatalf("RequeueJob() error = %v", err)
			}
			if res.Outcome != tt.wantOutcome {
				t.Errorf("outcome = %q, want %q", res.Outcome, tt.wantOutcome)
			}
			if gh.calls != 1 {
				t.Errorf("GitHub calls = %d, want 1 — claiming must be confirmed, not assumed", gh.calls)
			}
			if len(rq.sent) != tt.wantSends {
				t.Errorf("sends = %d, want %d", len(rq.sent), tt.wantSends)
			}
		})
	}
}

// Terminating on an unconfirmed guess would kill real work, so every way of
// failing to confirm must refuse, and must refuse before anything irreversible.
func TestRequeueJob_RunningRefusedWithoutAQueuedConfirmation(t *testing.T) {
	tests := []struct {
		name        string
		github      JobQueuedChecker
		wantOutcome RequeueOutcome
	}{
		{
			name:        "GitHub says the job is executing",
			github:      &mockQueuedChecker{status: "in_progress"},
			wantOutcome: OutcomeNotQueued,
		},
		{
			name:        "GitHub says the job already finished",
			github:      &mockQueuedChecker{status: "completed"},
			wantOutcome: OutcomeNotQueued,
		},
		{
			name:        "GitHub could not be reached",
			github:      &mockQueuedChecker{err: errors.New("api rate limit exceeded")},
			wantOutcome: OutcomeGitHubUnknown,
		},
		{
			name:        "no GitHub client is configured",
			github:      nil,
			wantOutcome: OutcomeGitHubUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-live")}}
			dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
				requeueJobItem(42, "i-live", 7, 0, db.JobStatusRunning),
			}}
			rq := &mockJobRequeuer{}

			res, err := RequeueJob(context.Background(), newRequeueDepsWithGitHub(ec2, dyn, rq, tt.github), 42, RequeueJobOptions{})
			if err != nil {
				t.Fatalf("RequeueJob() error = %v", err)
			}
			if res.Outcome != tt.wantOutcome {
				t.Errorf("outcome = %q, want %q", res.Outcome, tt.wantOutcome)
			}
			if ec2.terminateCalls != 0 || len(rq.sent) != 0 || dyn.updateCalls != 0 {
				t.Errorf("nothing may be touched without a queued confirmation; terminates=%d sends=%d updates=%d",
					ec2.terminateCalls, len(rq.sent), dyn.updateCalls)
			}
		})
	}
}

// A running record whose repo was never persisted cannot be checked against
// GitHub at all, and an unverifiable record must never reach the terminate.
func TestRequeueJob_RunningWithoutRepoRefused(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-live")}}
	item := requeueJobItem(42, "i-live", 7, 0, db.JobStatusRunning)
	item["repo"] = &types.AttributeValueMemberS{Value: ""}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{item}}
	rq := &mockJobRequeuer{}
	gh := &mockQueuedChecker{status: "queued"}

	res, err := RequeueJob(context.Background(), newRequeueDepsWithGitHub(ec2, dyn, rq, gh), 42, RequeueJobOptions{})
	if err != nil {
		t.Fatalf("RequeueJob() error = %v", err)
	}
	if res.Outcome != OutcomeGitHubUnknown {
		t.Errorf("outcome = %q, want %q", res.Outcome, OutcomeGitHubUnknown)
	}
	if gh.calls != 0 {
		t.Errorf("GitHub calls = %d, want 0: there is no repo to ask about", gh.calls)
	}
	if ec2.terminateCalls != 0 || len(rq.sent) != 0 || dyn.updateCalls != 0 {
		t.Errorf("nothing may be touched for an unverifiable record; terminates=%d sends=%d updates=%d",
			ec2.terminateCalls, len(rq.sent), dyn.updateCalls)
	}
}

// A launched record has no confirmed runner, so nothing can be executing and
// there is nothing for GitHub to settle. Asking anyway would spend an API call
// per click to learn what the status already says.
func TestRequeueJob_LaunchedDoesNotConsultGitHub(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-dead-agent")}}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
		requeueJobItem(42, "i-dead-agent", 7, 0, db.JobStatusLaunched),
	}}
	rq := &mockJobRequeuer{}
	gh := &mockQueuedChecker{status: "in_progress"}

	res, err := RequeueJob(context.Background(), newRequeueDepsWithGitHub(ec2, dyn, rq, gh), 42, RequeueJobOptions{})
	if err != nil {
		t.Fatalf("RequeueJob() error = %v", err)
	}
	if res.Outcome != OutcomeRequeued {
		t.Errorf("a launched job must requeue exactly as before, got %+v", res)
	}
	if gh.calls != 0 {
		t.Errorf("GitHub calls = %d, want 0 for a launched record", gh.calls)
	}
}

// The cap exists to stop automated churn. An operator acting on a job GitHub
// confirms is still queued is the case it should not block — but force buys
// nothing else.
func TestRequeueJob_ForceBypassesOnlyTheRetryCap(t *testing.T) {
	newDeps := func(status db.JobStatus, ghStatus string) (*mockEC2API, *mockTaskDynamoDBAPI, *mockJobRequeuer, RequeueDeps) {
		ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-idle")}}
		dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{
			requeueJobItem(42, "i-idle", 7, MaxRequeueRetries, status),
		}}
		rq := &mockJobRequeuer{}
		return ec2, dyn, rq, newRequeueDepsWithGitHub(ec2, dyn, rq, &mockQueuedChecker{status: ghStatus})
	}

	t.Run("force re-dispatches an exhausted job GitHub still has queued", func(t *testing.T) {
		ec2, _, rq, deps := newDeps(db.JobStatusRunning, "queued")
		res, err := RequeueJob(context.Background(), deps, 42, RequeueJobOptions{Force: true})
		if err != nil {
			t.Fatalf("RequeueJob() error = %v", err)
		}
		if res.Outcome != OutcomeRequeued || len(rq.sent) != 1 {
			t.Fatalf("expected a forced requeue, got %+v (sent=%d)", res, len(rq.sent))
		}
		if res.RetryCount != MaxRequeueRetries+1 || ec2.terminateCalls != 1 {
			t.Errorf("retry=%d terminates=%d, want %d and 1", res.RetryCount, ec2.terminateCalls, MaxRequeueRetries+1)
		}
	})

	t.Run("force does not bypass the queued confirmation", func(t *testing.T) {
		ec2, _, rq, deps := newDeps(db.JobStatusRunning, "in_progress")
		res, err := RequeueJob(context.Background(), deps, 42, RequeueJobOptions{Force: true})
		if err != nil {
			t.Fatalf("RequeueJob() error = %v", err)
		}
		if res.Outcome != OutcomeNotQueued {
			t.Errorf("outcome = %q, want %q — force must never override the safety gate", res.Outcome, OutcomeNotQueued)
		}
		if ec2.terminateCalls != 0 || len(rq.sent) != 0 {
			t.Errorf("nothing may be touched; terminates=%d sends=%d", ec2.terminateCalls, len(rq.sent))
		}
	})

	t.Run("without force the cap still binds", func(t *testing.T) {
		_, _, rq, deps := newDeps(db.JobStatusRunning, "queued")
		res, err := RequeueJob(context.Background(), deps, 42, RequeueJobOptions{})
		if err != nil {
			t.Fatalf("RequeueJob() error = %v", err)
		}
		if res.Outcome != OutcomeExhausted || len(rq.sent) != 0 {
			t.Errorf("expected an exhausted refusal, got %+v (sent=%d)", res, len(rq.sent))
		}
	})
}

// The guarded flip and its rollback must pin the status that was actually read.
// Hard-coding launched would make the write silently no-op on a running record
// and strand the job in requeued if the send then failed.
func TestRequeueJob_FlipPinsTheObservedStatus(t *testing.T) {
	var updates []*dynamodb.UpdateItemInput
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-idle")}}
	dyn := &mockTaskDynamoDBAPI{
		items:          []map[string]types.AttributeValue{requeueJobItem(42, "i-idle", 7, 0, db.JobStatusRunning)},
		captureUpdates: &updates,
	}
	rq := &mockJobRequeuer{sendErr: errors.New("sqs unavailable")}

	res, _ := RequeueJob(context.Background(),
		newRequeueDepsWithGitHub(ec2, dyn, rq, &mockQueuedChecker{status: "queued"}), 42, RequeueJobOptions{})
	if res.Outcome != OutcomeSendFailed {
		t.Fatalf("expected a send failure, got %+v", res)
	}

	if len(updates) != 2 {
		t.Fatalf("expected flip + rollback writes, got %d", len(updates))
	}
	flip := updates[0]
	if flip.ConditionExpression == nil || !strings.Contains(*flip.ConditionExpression, ":from") {
		t.Fatalf("flip condition = %v, want it pinned to the observed status", flip.ConditionExpression)
	}
	if v, ok := flip.ExpressionAttributeValues[":from"].(*types.AttributeValueMemberS); !ok || v.Value != string(db.JobStatusRunning) {
		t.Errorf("flip pinned %v, want running", flip.ExpressionAttributeValues[":from"])
	}
	rollback := updates[1]
	if v, ok := rollback.ExpressionAttributeValues[":from"].(*types.AttributeValueMemberS); !ok || v.Value != string(db.JobStatusRunning) {
		t.Errorf("rollback restored %v, want running", rollback.ExpressionAttributeValues[":from"])
	}
}
