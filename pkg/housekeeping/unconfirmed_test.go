package housekeeping

import (
	"context"
	"strconv"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type mockJobRequeuer struct {
	sent    []*queue.JobMessage
	sendErr error
}

func (m *mockJobRequeuer) SendMessage(_ context.Context, job *queue.JobMessage) error {
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
