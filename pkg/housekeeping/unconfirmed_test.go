package housekeeping

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type mockJobRequeuer struct {
	sent    []*queue.JobMessage
	sendErr error
	onSend  func()
}

func (m *mockJobRequeuer) SendMessage(_ context.Context, job *queue.JobMessage) error {
	if m.onSend != nil {
		m.onSend()
	}
	if m.sendErr != nil {
		return m.sendErr
	}
	m.sent = append(m.sent, job)
	return nil
}

func launchedJobItem(jobID int64, instanceID string, runID int64, retry int) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"job_id":        &types.AttributeValueMemberN{Value: strconv.FormatInt(jobID, 10)},
		"instance_id":   &types.AttributeValueMemberS{Value: instanceID},
		"run_id":        &types.AttributeValueMemberN{Value: strconv.FormatInt(runID, 10)},
		"repo":          &types.AttributeValueMemberS{Value: "octo/repo"},
		"instance_type": &types.AttributeValueMemberS{Value: "c7g.large"},
		"pool":          &types.AttributeValueMemberS{Value: "default"},
		"retry_count":   &types.AttributeValueMemberN{Value: strconv.Itoa(retry)},
		"status":        &types.AttributeValueMemberS{Value: "launched"},
	}
}

func runningReservation(instanceID string) ec2types.Reservation {
	return ec2types.Reservation{Instances: []ec2types.Instance{{
		InstanceId: aws.String(instanceID),
		State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
	}}}
}

func newUnconfirmedTasks(ec2 *mockEC2API, dyn *mockTaskDynamoDBAPI, metrics *mockTaskMetricsAPI, rq JobRequeuer) *Tasks {
	return &Tasks{
		ec2Client:    ec2,
		dynamoClient: dyn,
		metrics:      metrics,
		jobRequeuer:  rq,
		config:       &config.Config{JobsTableName: "jobs-table"},
	}
}

// A launched job whose instance is still alive but whose runner never confirmed
// is terminated and requeued on-demand, bounded by the retry cap.
func TestExecuteUnconfirmedRunners_RecoversAliveInstance(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{launchedJobItem(42, "i-stuck", 7, 0)}}
	metrics := &mockTaskMetricsAPI{}
	rq := &mockJobRequeuer{}
	tasks := newUnconfirmedTasks(ec2, dyn, metrics, rq)

	if err := tasks.ExecuteUnconfirmedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteUnconfirmedRunners() error = %v", err)
	}

	if ec2.terminateCalls != 1 || len(ec2.terminatedIDs) != 1 || ec2.terminatedIDs[0] != "i-stuck" {
		t.Errorf("expected the alive dead-agent instance to be terminated; calls=%d ids=%v", ec2.terminateCalls, ec2.terminatedIDs)
	}
	if len(rq.sent) != 1 {
		t.Fatalf("expected 1 requeue, got %d", len(rq.sent))
	}
	if rq.sent[0].RetryCount != 1 || !rq.sent[0].ForceOnDemand || rq.sent[0].RunID != 7 {
		t.Errorf("requeue message wrong: %+v", rq.sent[0])
	}
	if dyn.updateCalls != 1 {
		t.Errorf("expected the record flipped to requeued (1 update), got %d", dyn.updateCalls)
	}
	if len(metrics.requeuedReasons) != 1 || metrics.requeuedReasons[0] != housekeepingActionUnconfirmedRunner {
		t.Errorf("expected a requeued metric tagged unconfirmed_runners, got %v", metrics.requeuedReasons)
	}
	if metrics.unconfirmedCount != 1 {
		t.Errorf("expected housekeeping action count 1, got %d", metrics.unconfirmedCount)
	}
}

// Once a job has exhausted its retries, the watchdog marks it terminal and emits
// a scheduling-failure alert instead of requeuing again (no infinite churn).
func TestExecuteUnconfirmedRunners_ExhaustedMarksFailed(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{launchedJobItem(42, "i-stuck", 7, maxLaunchRecoveryRetries)}}
	metrics := &mockTaskMetricsAPI{}
	rq := &mockJobRequeuer{}
	tasks := newUnconfirmedTasks(ec2, dyn, metrics, rq)

	if err := tasks.ExecuteUnconfirmedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteUnconfirmedRunners() error = %v", err)
	}

	if len(rq.sent) != 0 {
		t.Errorf("exhausted job must not be requeued, got %d requeues", len(rq.sent))
	}
	if dyn.updateCalls != 1 {
		t.Errorf("expected the record marked terminal (1 update), got %d", dyn.updateCalls)
	}
	if len(metrics.schedulingFailures) != 1 || metrics.schedulingFailures[0] != housekeepingActionUnconfirmedRunner {
		t.Errorf("expected a scheduling-failure alert tagged unconfirmed_runners, got %v", metrics.schedulingFailures)
	}
}

// When the instance is already gone there is nothing to terminate, but the job
// is still recovered (requeued).
func TestExecuteUnconfirmedRunners_InstanceGoneRequeues(t *testing.T) {
	ec2 := &mockEC2API{instances: nil} // DescribeInstances returns no reservations => gone
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{launchedJobItem(42, "i-gone", 7, 0)}}
	metrics := &mockTaskMetricsAPI{}
	rq := &mockJobRequeuer{}
	tasks := newUnconfirmedTasks(ec2, dyn, metrics, rq)

	if err := tasks.ExecuteUnconfirmedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteUnconfirmedRunners() error = %v", err)
	}

	if ec2.terminateCalls != 0 {
		t.Errorf("a gone instance must not be terminated, got %d terminate calls", ec2.terminateCalls)
	}
	if len(rq.sent) != 1 {
		t.Errorf("expected the job requeued, got %d requeues", len(rq.sent))
	}
}

// Regression: `pool` is a DynamoDB reserved keyword. It must never appear bare in
// the projection, only via the #pool alias mapped in ExpressionAttributeNames —
// otherwise every Scan throws ValidationException ("reserved keyword: pool") and the
// watchdog silently fails on every run (the defect that kept it from ever working).
func TestExecuteUnconfirmedRunners_ScanAliasesReservedPool(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{launchedJobItem(42, "i-stuck", 7, 0)}}
	metrics := &mockTaskMetricsAPI{}
	rq := &mockJobRequeuer{}
	tasks := newUnconfirmedTasks(ec2, dyn, metrics, rq)

	if err := tasks.ExecuteUnconfirmedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteUnconfirmedRunners() error = %v", err)
	}

	if len(dyn.scanInputs) == 0 {
		t.Fatal("expected the watchdog to scan; no ScanInput captured")
	}
	in := dyn.scanInputs[0]

	if in.ProjectionExpression == nil {
		t.Fatal("ScanInput.ProjectionExpression is nil")
	}
	proj := *in.ProjectionExpression

	// `pool` must appear only as the aliased token #pool, never bare. A word-boundary
	// match for a bare `pool` not preceded by `#` would catch the regressed form.
	barePool := regexp.MustCompile(`(^|[^#\w])pool\b`)
	if barePool.MatchString(proj) {
		t.Errorf("ProjectionExpression contains bare reserved keyword 'pool' (must be '#pool'): %q", proj)
	}

	if got := in.ExpressionAttributeNames["#pool"]; got != "pool" {
		t.Errorf("ExpressionAttributeNames must map #pool -> pool, got %q (names=%v)", got, in.ExpressionAttributeNames)
	}
	// The pre-existing #status alias must remain intact alongside the new one.
	if got := in.ExpressionAttributeNames["#status"]; got != "status" {
		t.Errorf("ExpressionAttributeNames must still map #status -> status, got %q", got)
	}
}

// A runner that confirms between the scan and the sweep flips the job to
// running; the watchdog must re-read the record and leave the live runner alone
// (the old behavior terminated it mid-job).
func TestExecuteUnconfirmedRunners_SkipsJobThatConfirmed(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-confirmed")}}
	dyn := &mockTaskDynamoDBAPI{
		items:          []map[string]types.AttributeValue{launchedJobItem(42, "i-confirmed", 7, 0)},
		statusOverride: map[int64]string{42: "running"},
	}
	metrics := &mockTaskMetricsAPI{}
	rq := &mockJobRequeuer{}
	tasks := newUnconfirmedTasks(ec2, dyn, metrics, rq)

	if err := tasks.ExecuteUnconfirmedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteUnconfirmedRunners() error = %v", err)
	}

	if ec2.terminateCalls != 0 {
		t.Errorf("a confirmed runner must not be terminated, got %d terminate calls", ec2.terminateCalls)
	}
	if len(rq.sent) != 0 {
		t.Errorf("a confirmed runner's job must not be requeued, got %d", len(rq.sent))
	}
	if dyn.updateCalls != 0 {
		t.Errorf("a confirmed runner's record must not be flipped, got %d updates", dyn.updateCalls)
	}
}

// A re-read failure means the job's true state is unknown; the watchdog must
// not terminate on uncertainty and must leave the candidate for the next cycle.
func TestExecuteUnconfirmedRunners_GetItemErrorSkips(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{
		items:      []map[string]types.AttributeValue{launchedJobItem(42, "i-stuck", 7, 0)},
		getItemErr: errors.New("dynamo unavailable"),
	}
	metrics := &mockTaskMetricsAPI{}
	rq := &mockJobRequeuer{}
	tasks := newUnconfirmedTasks(ec2, dyn, metrics, rq)

	if err := tasks.ExecuteUnconfirmedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteUnconfirmedRunners() error = %v", err)
	}

	if ec2.terminateCalls != 0 {
		t.Errorf("must not terminate when the job state is unknown, got %d terminate calls", ec2.terminateCalls)
	}
	if len(rq.sent) != 0 {
		t.Errorf("must not requeue when the job state is unknown, got %d", len(rq.sent))
	}
}

// A job that reached a terminal state after the scan is not a recovery
// candidate anymore.
func TestExecuteUnconfirmedRunners_SkipsCompletedJob(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-done")}}
	dyn := &mockTaskDynamoDBAPI{
		items:          []map[string]types.AttributeValue{launchedJobItem(42, "i-done", 7, 0)},
		statusOverride: map[int64]string{42: "success"},
	}
	metrics := &mockTaskMetricsAPI{}
	rq := &mockJobRequeuer{}
	tasks := newUnconfirmedTasks(ec2, dyn, metrics, rq)

	if err := tasks.ExecuteUnconfirmedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteUnconfirmedRunners() error = %v", err)
	}

	if ec2.terminateCalls != 0 || len(rq.sent) != 0 || dyn.updateCalls != 0 {
		t.Errorf("completed job must be untouched: terminates=%d requeues=%d updates=%d",
			ec2.terminateCalls, len(rq.sent), dyn.updateCalls)
	}
}

// Without a requeuer the watchdog is a no-op: it must never destroy a job it
// cannot recover, and must not even scan.
func TestExecuteUnconfirmedRunners_NoRequeuerNoOp(t *testing.T) {
	ec2 := &mockEC2API{}
	dyn := &mockTaskDynamoDBAPI{items: []map[string]types.AttributeValue{launchedJobItem(42, "i-stuck", 7, 0)}}
	metrics := &mockTaskMetricsAPI{}
	tasks := newUnconfirmedTasks(ec2, dyn, metrics, nil)

	if err := tasks.ExecuteUnconfirmedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteUnconfirmedRunners() error = %v", err)
	}
	if dyn.scanCalls != 0 {
		t.Errorf("watchdog must not scan when no requeuer is configured, got %d scans", dyn.scanCalls)
	}
	if ec2.terminateCalls != 0 {
		t.Errorf("watchdog must not terminate anything when disabled, got %d", ec2.terminateCalls)
	}
}

// Same claimability ordering as the operator sweep: a message that reaches a
// worker before the record is flipped is rejected by ClaimJob and dropped.
func TestExecuteUnconfirmedRunners_FlipsRecordBeforeSending(t *testing.T) {
	var order []string
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{
		items:        []map[string]types.AttributeValue{launchedJobItem(42, "i-stuck", 7, 0)},
		onUpdateItem: func() { order = append(order, "flip") },
	}
	rq := &mockJobRequeuer{onSend: func() { order = append(order, "send") }}
	tasks := newUnconfirmedTasks(ec2, dyn, &mockTaskMetricsAPI{}, rq)

	if err := tasks.ExecuteUnconfirmedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteUnconfirmedRunners() error = %v", err)
	}

	if len(order) != 2 || order[0] != "flip" || order[1] != "send" {
		t.Errorf("expected the record flipped before the send, got order %v", order)
	}
}

// Same rollback requirement as the operator sweep: the watchdog also scans
// launched records only, so a flip with no delivered message must be undone.
func TestExecuteUnconfirmedRunners_SendFailureRollsBackFlip(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	var updates []*dynamodb.UpdateItemInput
	dyn := &mockTaskDynamoDBAPI{
		items:          []map[string]types.AttributeValue{launchedJobItem(42, "i-stuck", 7, 0)},
		captureUpdates: &updates,
	}
	rq := &mockJobRequeuer{sendErr: errors.New("sqs unavailable")}
	tasks := newUnconfirmedTasks(ec2, dyn, &mockTaskMetricsAPI{}, rq)

	if err := tasks.ExecuteUnconfirmedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteUnconfirmedRunners() error = %v", err)
	}

	if len(updates) != 2 {
		t.Fatalf("expected flip + rollback writes, got %d", len(updates))
	}
	if v, ok := updates[1].ExpressionAttributeValues[":launched"].(*types.AttributeValueMemberS); !ok || v.Value != "launched" {
		t.Errorf("rollback must restore the launched status, got %v", updates[1].ExpressionAttributeValues[":launched"])
	}
}

// Same ownership rule for the watchdog: losing the guarded flip means another
// actor owns the dispatch, so this cycle must not send a message of its own.
func TestExecuteUnconfirmedRunners_LostFlipDoesNotSend(t *testing.T) {
	ec2 := &mockEC2API{instances: []ec2types.Reservation{runningReservation("i-stuck")}}
	dyn := &mockTaskDynamoDBAPI{
		items:     []map[string]types.AttributeValue{launchedJobItem(42, "i-stuck", 7, 0)},
		updateErr: &types.ConditionalCheckFailedException{},
	}
	rq := &mockJobRequeuer{}
	tasks := newUnconfirmedTasks(ec2, dyn, &mockTaskMetricsAPI{}, rq)

	if err := tasks.ExecuteUnconfirmedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteUnconfirmedRunners() error = %v", err)
	}

	if len(rq.sent) != 0 {
		t.Errorf("a cycle whose flip lost must not send, got %d messages", len(rq.sent))
	}
}
