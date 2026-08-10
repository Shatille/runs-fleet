package housekeeping

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type mockOrphanDynamo struct {
	items       []map[string]types.AttributeValue
	scanErr     error
	updateErr   error
	updateCalls int
	lastUpdate  *dynamodb.UpdateItemInput
	lastScan    *dynamodb.ScanInput
}

func (m *mockOrphanDynamo) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	key, ok := in.Key["job_id"].(*types.AttributeValueMemberN)
	if !ok {
		return &dynamodb.GetItemOutput{}, nil
	}
	for _, item := range m.items {
		if n, ok := item["job_id"].(*types.AttributeValueMemberN); ok && n.Value == key.Value {
			return &dynamodb.GetItemOutput{Item: item}, nil
		}
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (m *mockOrphanDynamo) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	m.lastScan = in
	if m.scanErr != nil {
		return nil, m.scanErr
	}
	return &dynamodb.ScanOutput{Items: m.items}, nil
}

func (m *mockOrphanDynamo) UpdateItem(_ context.Context, input *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	m.updateCalls++
	m.lastUpdate = input
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

type mockOrphanEC2 struct {
	instances     map[string]ec2types.InstanceStateName
	err           error
	failBatchOnly bool
}

func (m *mockOrphanEC2) DescribeInstances(_ context.Context, params *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if m.failBatchOnly && len(params.InstanceIds) > 1 {
		return nil, m.err
	}
	if m.err != nil && !m.failBatchOnly {
		return nil, m.err
	}

	var instances []ec2types.Instance
	for _, id := range params.InstanceIds {
		if state, ok := m.instances[id]; ok {
			instances = append(instances, ec2types.Instance{
				InstanceId: aws.String(id),
				State:      &ec2types.InstanceState{Name: state},
			})
		}
	}

	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{
			{Instances: instances},
		},
	}, nil
}

func TestFindOrphanedJobCandidates_ScanError(t *testing.T) {
	t.Parallel()
	client := &mockOrphanDynamo{scanErr: errors.New("scan failed")}
	_, err := FindOrphanedJobCandidates(context.Background(), client, "jobs", 2*time.Hour)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFindOrphanedJobCandidates_NoCandidates(t *testing.T) {
	t.Parallel()
	client := &mockOrphanDynamo{items: nil}
	candidates, err := FindOrphanedJobCandidates(context.Background(), client, "jobs", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates, got %d", len(candidates))
	}
}

func TestFindOrphanedJobCandidates_FiltersRunningWithoutInstance(t *testing.T) {
	t.Parallel()
	client := &mockOrphanDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id": &types.AttributeValueMemberN{Value: "100"},
				"status": &types.AttributeValueMemberS{Value: string(db.JobStatusRunning)},
			},
			{
				"job_id": &types.AttributeValueMemberN{Value: "101"},
				"status": &types.AttributeValueMemberS{Value: string(db.JobStatusClaiming)},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "102"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-abc"},
				"status":      &types.AttributeValueMemberS{Value: string(db.JobStatusRunning)},
			},
		},
	}
	candidates, err := FindOrphanedJobCandidates(context.Background(), client, "jobs", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Job 100 (running, no instance) should be filtered out
	// Job 101 (claiming, no instance) should be included
	// Job 102 (running, with instance) should be included
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	ids := map[int64]bool{}
	for _, c := range candidates {
		ids[c.JobID] = true
	}
	if ids[100] {
		t.Error("job 100 (running without instance) should have been filtered")
	}
	if !ids[101] || !ids[102] {
		t.Errorf("expected jobs 101 and 102, got %v", ids)
	}
}

func TestFindOrphanedJobCandidates_SkipsZeroJobID(t *testing.T) {
	t.Parallel()
	client := &mockOrphanDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "0"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-abc"},
				"status":      &types.AttributeValueMemberS{Value: string(db.JobStatusRunning)},
			},
		},
	}
	candidates, err := FindOrphanedJobCandidates(context.Background(), client, "jobs", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates (zero job_id skipped), got %d", len(candidates))
	}
}

func TestFindOrphanedJobCandidates_SkipsInvalidJobID(t *testing.T) {
	t.Parallel()
	client := &mockOrphanDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "not-a-number"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-abc"},
				"status":      &types.AttributeValueMemberS{Value: string(db.JobStatusRunning)},
			},
		},
	}
	candidates, err := FindOrphanedJobCandidates(context.Background(), client, "jobs", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates (invalid job_id skipped), got %d", len(candidates))
	}
}

func TestSeparateOrphanedJobs(t *testing.T) {
	t.Parallel()
	candidates := []OrphanedJobCandidate{
		{JobID: 1, InstanceID: "i-abc", Status: string(db.JobStatusRunning)},
		{JobID: 2, InstanceID: "", Status: string(db.JobStatusClaiming)},
		{JobID: 3, InstanceID: "i-def", Status: string(db.JobStatusClaiming)},
		{JobID: 4, InstanceID: "", Status: string(db.JobStatusClaiming)},
	}

	withInstance, withoutInstance := SeparateOrphanedJobs(candidates)

	if len(withInstance) != 2 {
		t.Errorf("expected 2 with instance, got %d", len(withInstance))
	}
	if len(withoutInstance) != 2 {
		t.Errorf("expected 2 without instance, got %d", len(withoutInstance))
	}
}

func TestSeparateOrphanedJobs_Empty(t *testing.T) {
	t.Parallel()
	withInstance, withoutInstance := SeparateOrphanedJobs(nil)
	if withInstance != nil || withoutInstance != nil {
		t.Error("expected nil slices for nil input")
	}
}

func TestBatchCheckInstanceExistence_AllExist(t *testing.T) {
	t.Parallel()
	ec2Client := &mockOrphanEC2{
		instances: map[string]ec2types.InstanceStateName{
			"i-abc": ec2types.InstanceStateNameRunning,
			"i-def": ec2types.InstanceStateNamePending,
		},
	}
	candidates := []OrphanedJobCandidate{
		{InstanceID: "i-abc"},
		{InstanceID: "i-def"},
	}
	fallback := func(_ context.Context, _ string) bool { return false }

	existing := BatchCheckInstanceExistence(context.Background(), ec2Client, candidates, fallback)
	if !existing["i-abc"] || !existing["i-def"] {
		t.Errorf("expected both instances to exist, got %v", existing)
	}
}

func TestBatchCheckInstanceExistence_TerminatedNotExist(t *testing.T) {
	t.Parallel()
	ec2Client := &mockOrphanEC2{
		instances: map[string]ec2types.InstanceStateName{
			"i-abc": ec2types.InstanceStateNameTerminated,
		},
	}
	candidates := []OrphanedJobCandidate{{InstanceID: "i-abc"}}
	fallback := func(_ context.Context, _ string) bool { return false }

	existing := BatchCheckInstanceExistence(context.Background(), ec2Client, candidates, fallback)
	if existing["i-abc"] {
		t.Error("terminated instance should not be marked as existing")
	}
}

func TestBatchCheckInstanceExistence_ErrorFallback(t *testing.T) {
	t.Parallel()
	ec2Client := &mockOrphanEC2{err: errors.New("API error")}
	candidates := []OrphanedJobCandidate{
		{InstanceID: "i-abc"},
		{InstanceID: "i-def"},
	}
	fallbackCalls := map[string]bool{}
	fallback := func(_ context.Context, id string) bool {
		fallbackCalls[id] = true
		return id == "i-abc"
	}

	existing := BatchCheckInstanceExistence(context.Background(), ec2Client, candidates, fallback)
	if !existing["i-abc"] {
		t.Error("expected i-abc to exist via fallback")
	}
	if existing["i-def"] {
		t.Error("expected i-def to not exist via fallback")
	}
	if len(fallbackCalls) != 2 {
		t.Errorf("expected 2 fallback calls, got %d", len(fallbackCalls))
	}
}

func TestBatchCheckInstanceExistence_DeduplicatesInstances(t *testing.T) {
	t.Parallel()
	callCount := 0
	ec2Client := &mockOrphanEC2{
		instances: map[string]ec2types.InstanceStateName{
			"i-abc": ec2types.InstanceStateNameRunning,
		},
	}
	_ = callCount
	candidates := []OrphanedJobCandidate{
		{InstanceID: "i-abc"},
		{InstanceID: "i-abc"},
		{InstanceID: "i-abc"},
	}
	fallback := func(_ context.Context, _ string) bool { return false }

	existing := BatchCheckInstanceExistence(context.Background(), ec2Client, candidates, fallback)
	if !existing["i-abc"] {
		t.Error("expected i-abc to exist")
	}
}

func TestMarkJobOrphaned_Success(t *testing.T) {
	t.Parallel()
	client := &mockOrphanDynamo{}
	err := MarkJobOrphaned(context.Background(), client, "jobs", 123)
	if err != nil {
		t.Fatal(err)
	}
	if client.updateCalls != 1 {
		t.Errorf("expected 1 update call, got %d", client.updateCalls)
	}

	key := client.lastUpdate.Key["job_id"].(*types.AttributeValueMemberN)
	if key.Value != strconv.FormatInt(123, 10) {
		t.Errorf("expected job_id 123, got %s", key.Value)
	}
}

func TestMarkJobOrphaned_ConditionalCheckFailed(t *testing.T) {
	t.Parallel()
	client := &mockOrphanDynamo{
		updateErr: &types.ConditionalCheckFailedException{Message: aws.String("condition failed")},
	}
	err := MarkJobOrphaned(context.Background(), client, "jobs", 123)
	if err != nil {
		t.Errorf("expected nil error for ConditionalCheckFailedException, got %v", err)
	}
}

func TestMarkJobOrphaned_OtherError(t *testing.T) {
	t.Parallel()
	client := &mockOrphanDynamo{updateErr: errors.New("dynamo error")}
	err := MarkJobOrphaned(context.Background(), client, "jobs", 123)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBatchCheckInstanceExistence_NonexistentNotInMap(t *testing.T) {
	t.Parallel()
	ec2Client := &mockOrphanEC2{
		instances: map[string]ec2types.InstanceStateName{
			"i-abc": ec2types.InstanceStateNameRunning,
		},
	}
	candidates := []OrphanedJobCandidate{
		{InstanceID: "i-abc"},
		{InstanceID: "i-nonexistent"},
	}
	fallback := func(_ context.Context, _ string) bool { return false }

	existing := BatchCheckInstanceExistence(context.Background(), ec2Client, candidates, fallback)
	if !existing["i-abc"] {
		t.Error("expected i-abc to exist")
	}
	if existing["i-nonexistent"] {
		t.Error("expected i-nonexistent to not exist")
	}
}

func reconcileItem(jobID int64, instanceID string, status db.JobStatus) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"job_id":      &types.AttributeValueMemberN{Value: strconv.FormatInt(jobID, 10)},
		"instance_id": &types.AttributeValueMemberS{Value: instanceID},
		"status":      &types.AttributeValueMemberS{Value: string(status)},
	}
}

func TestReconcileJob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		item        map[string]types.AttributeValue
		instances   map[string]ec2types.InstanceStateName
		updateErr   error
		wantOutcome ReconcileOutcome
		wantUpdates int
	}{
		{
			name:        "instance gone retires the record",
			item:        reconcileItem(42, "i-gone", db.JobStatusRunning),
			wantOutcome: ReconcileOrphaned,
			wantUpdates: 1,
		},
		{
			name:        "terminated instance counts as gone",
			item:        reconcileItem(42, "i-dead", db.JobStatusLaunched),
			instances:   map[string]ec2types.InstanceStateName{"i-dead": ec2types.InstanceStateNameTerminated},
			wantOutcome: ReconcileOrphaned,
			wantUpdates: 1,
		},
		{
			// The record is the only thing tying a live runner to its work.
			name:        "live instance is refused",
			item:        reconcileItem(42, "i-live", db.JobStatusRunning),
			instances:   map[string]ec2types.InstanceStateName{"i-live": ec2types.InstanceStateNameRunning},
			wantOutcome: ReconcileInstanceAlive,
		},
		{
			name:        "settled job has nothing to reconcile",
			item:        reconcileItem(42, "i-gone", db.JobStatusSuccess),
			wantOutcome: ReconcileWrongStatus,
		},
		{
			// The sweep retires these; the per-job button has to agree, or it
			// refuses exactly the records the sweep was taught to find.
			name:        "record stranded in requeued is retired",
			item:        reconcileItem(42, "i-gone", db.JobStatusRequeued),
			wantOutcome: ReconcileOrphaned,
			wantUpdates: 1,
		},
		{
			// Unverifiable: there is nothing to check against EC2, and the bulk
			// sweep drops this shape for the same reason.
			name:        "running job with no instance is refused",
			item:        reconcileItem(42, "", db.JobStatusRunning),
			wantOutcome: ReconcileNoInstance,
		},
		{
			name:        "launched job with no instance is refused",
			item:        reconcileItem(42, "", db.JobStatusLaunched),
			wantOutcome: ReconcileNoInstance,
		},
		{
			// Instance creation itself failed, so there is no instance to verify
			// against and the record is safe to retire — what the bulk sweep does.
			name:        "claiming job with no instance is retired",
			item:        reconcileItem(42, "", db.JobStatusClaiming),
			wantOutcome: ReconcileOrphaned,
			wantUpdates: 1,
		},
		{
			// The job completed between the read and the guarded write, so the
			// write never landed: reporting it as orphaned would be a lie in the
			// response and in the audit trail.
			name:        "job that settled mid-reconcile is not claimed as orphaned",
			item:        reconcileItem(42, "i-gone", db.JobStatusRunning),
			updateErr:   &types.ConditionalCheckFailedException{},
			wantOutcome: ReconcileLostRace,
			wantUpdates: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dyn := &mockOrphanDynamo{
				items:     []map[string]types.AttributeValue{tt.item},
				updateErr: tt.updateErr,
			}
			ec2Client := &mockOrphanEC2{instances: tt.instances}

			res, err := ReconcileJob(context.Background(), dyn, ec2Client, "jobs-table", 42)
			if err != nil {
				t.Fatalf("ReconcileJob() error = %v", err)
			}
			if res.Outcome != tt.wantOutcome {
				t.Errorf("outcome = %q, want %q", res.Outcome, tt.wantOutcome)
			}
			if dyn.updateCalls != tt.wantUpdates {
				t.Errorf("update calls = %d, want %d", dyn.updateCalls, tt.wantUpdates)
			}
		})
	}
}

func TestReconcileJob_NotFound(t *testing.T) {
	t.Parallel()

	dyn := &mockOrphanDynamo{}
	res, err := ReconcileJob(context.Background(), dyn, &mockOrphanEC2{}, "jobs-table", 42)
	if err != nil {
		t.Fatalf("ReconcileJob() error = %v", err)
	}
	if res.Outcome != ReconcileNotFound || dyn.updateCalls != 0 {
		t.Errorf("res = %+v with %d updates, want a not-found outcome and no write", res, dyn.updateCalls)
	}
}

// An EC2 outage must not be read as "the instance is gone", or a transient failure
// retires jobs that are running fine.
func TestReconcileJob_DescribeErrorTreatsInstanceAsAlive(t *testing.T) {
	t.Parallel()

	dyn := &mockOrphanDynamo{items: []map[string]types.AttributeValue{
		reconcileItem(42, "i-unknown", db.JobStatusRunning),
	}}
	ec2Client := &mockOrphanEC2{err: errors.New("ec2 unavailable")}

	res, err := ReconcileJob(context.Background(), dyn, ec2Client, "jobs-table", 42)
	if err != nil {
		t.Fatalf("ReconcileJob() error = %v", err)
	}
	if res.Outcome != ReconcileInstanceAlive || dyn.updateCalls != 0 {
		t.Errorf("res = %+v with %d updates, want the job left alone", res, dyn.updateCalls)
	}
}

// A record flipped to requeued whose re-dispatch never landed is invisible to
// every other sweep -- stale jobs scans running/claiming, the requeue sweep scans
// launched, and the old-jobs GC keys off completed_at, which a requeued record
// never gets. The orphan sweep is the only thing that can retire it.
func TestFindOrphanedJobCandidates_IncludesStrandedRequeued(t *testing.T) {
	t.Parallel()

	client := &mockOrphanDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "200"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-gone"},
				"status":      &types.AttributeValueMemberS{Value: string(db.JobStatusRequeued)},
			},
		},
	}

	candidates, err := FindOrphanedJobCandidates(context.Background(), client, "jobs", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].JobID != 200 {
		t.Fatalf("candidates = %+v, want the stranded requeued record", candidates)
	}
}

// A requeue is transient by design: the record sits in requeued only until a
// worker claims it. Ageing it on created_at would retire a job re-dispatched
// seconds ago just because its first attempt was hours earlier, so the scan has
// to anchor on requeued_at.
func TestFindOrphanedJobCandidates_AgesRequeuedByRequeuedAt(t *testing.T) {
	t.Parallel()

	client := &mockOrphanDynamo{}
	if _, err := FindOrphanedJobCandidates(context.Background(), client, "jobs", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if client.lastScan == nil || client.lastScan.FilterExpression == nil {
		t.Fatal("no scan filter captured")
	}

	filter := *client.lastScan.FilterExpression
	if !strings.Contains(filter, "requeued_at < :cutoff") {
		t.Errorf("filter must age requeued records on requeued_at, got %q", filter)
	}
	if !strings.Contains(filter, "created_at < :cutoff") {
		t.Errorf("filter must still age the other statuses on created_at, got %q", filter)
	}
	v, ok := client.lastScan.ExpressionAttributeValues[":requeued"].(*types.AttributeValueMemberS)
	if !ok || v.Value != string(db.JobStatusRequeued) {
		t.Errorf("scan must bind :requeued to %q, got %v", db.JobStatusRequeued, client.lastScan.ExpressionAttributeValues[":requeued"])
	}
}

// Without requeued in the condition the write silently no-ops: DynamoDB rejects
// it, MarkJobOrphaned swallows the conditional failure, and the sweep counts a
// record it never actually retired.
func TestMarkJobOrphaned_AcceptsRequeuedRecord(t *testing.T) {
	t.Parallel()

	client := &mockOrphanDynamo{}
	if err := MarkJobOrphaned(context.Background(), client, "jobs", 200); err != nil {
		t.Fatal(err)
	}
	if client.lastUpdate == nil || client.lastUpdate.ConditionExpression == nil {
		t.Fatal("no conditional write captured")
	}
	if !strings.Contains(*client.lastUpdate.ConditionExpression, ":requeued") {
		t.Errorf("condition must permit a requeued record, got %q", *client.lastUpdate.ConditionExpression)
	}
	v, ok := client.lastUpdate.ExpressionAttributeValues[":requeued"].(*types.AttributeValueMemberS)
	if !ok || v.Value != string(db.JobStatusRequeued) {
		t.Errorf("update must bind :requeued to %q", db.JobStatusRequeued)
	}
}
