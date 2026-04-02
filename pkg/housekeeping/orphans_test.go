package housekeeping

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

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
}

func (m *mockOrphanDynamo) Scan(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
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
				"status": &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id": &types.AttributeValueMemberN{Value: "101"},
				"status": &types.AttributeValueMemberS{Value: "claiming"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "102"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-abc"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
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
				"status":      &types.AttributeValueMemberS{Value: "running"},
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
				"status":      &types.AttributeValueMemberS{Value: "running"},
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
		{JobID: 1, InstanceID: "i-abc", Status: "running"},
		{JobID: 2, InstanceID: "", Status: "claiming"},
		{JobID: 3, InstanceID: "i-def", Status: "claiming"},
		{JobID: 4, InstanceID: "", Status: "claiming"},
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
