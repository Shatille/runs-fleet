package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// mockRequeueEC2 implements housekeeping.EC2API for the requeue handler tests.
type mockRequeueEC2 struct {
	instances      map[string]ec2types.InstanceStateName
	terminateCalls int
	terminatedIDs  []string
}

func (m *mockRequeueEC2) DescribeInstances(_ context.Context, params *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
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
		Reservations: []ec2types.Reservation{{Instances: instances}},
	}, nil
}

func (m *mockRequeueEC2) TerminateInstances(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	m.terminateCalls++
	m.terminatedIDs = append(m.terminatedIDs, params.InstanceIds...)
	return &ec2.TerminateInstancesOutput{}, nil
}

func (m *mockRequeueEC2) DescribeSpotInstanceRequests(_ context.Context, _ *ec2.DescribeSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	return &ec2.DescribeSpotInstanceRequestsOutput{}, nil
}

func (m *mockRequeueEC2) CancelSpotInstanceRequests(_ context.Context, _ *ec2.CancelSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

type mockRequeueDynamo struct {
	items        []map[string]types.AttributeValue
	scanErr      error
	updateCalls  int
	capturedScan *dynamodb.ScanInput
}

func (m *mockRequeueDynamo) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	m.capturedScan = in
	if m.scanErr != nil {
		return nil, m.scanErr
	}
	return &dynamodb.ScanOutput{Items: m.items}, nil
}

func (m *mockRequeueDynamo) UpdateItem(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	m.updateCalls++
	return &dynamodb.UpdateItemOutput{}, nil
}

type mockRequeuer struct {
	sent []*queue.JobMessage
}

func (m *mockRequeuer) SendMessage(_ context.Context, job *queue.JobMessage) error {
	m.sent = append(m.sent, job)
	return nil
}

// mockRequeueMetrics captures the operator_requeue counters so the handler→
// housekeeping metrics wiring can be asserted end-to-end. It implements
// housekeeping.MetricsAPI.
type mockRequeueMetrics struct {
	requeuedReasons    []string
	schedulingFailures []string
}

func (m *mockRequeueMetrics) PublishJobRequeued(_ context.Context, reason string) error {
	m.requeuedReasons = append(m.requeuedReasons, reason)
	return nil
}

func (m *mockRequeueMetrics) PublishSchedulingFailure(_ context.Context, taskType string) error {
	m.schedulingFailures = append(m.schedulingFailures, taskType)
	return nil
}

func (m *mockRequeueMetrics) PublishHousekeepingAction(_ context.Context, _ string, _ int) error {
	return nil
}

func (m *mockRequeueMetrics) PublishPoolInstances(_ context.Context, _, _ string, _ int) error {
	return nil
}

func (m *mockRequeueMetrics) PublishPoolDesired(_ context.Context, _, _ string, _ int) error {
	return nil
}

func requeueAdminItem(jobID int64, instanceID string, runID int64, retry int, status string) map[string]types.AttributeValue {
	item := map[string]types.AttributeValue{
		"job_id":        &types.AttributeValueMemberN{Value: strconv.FormatInt(jobID, 10)},
		"run_id":        &types.AttributeValueMemberN{Value: strconv.FormatInt(runID, 10)},
		"repo":          &types.AttributeValueMemberS{Value: "octo/repo"},
		"instance_type": &types.AttributeValueMemberS{Value: "c7g.large"},
		"pool":          &types.AttributeValueMemberS{Value: "default"},
		"retry_count":   &types.AttributeValueMemberN{Value: strconv.Itoa(retry)},
		"status":        &types.AttributeValueMemberS{Value: status},
		"created_at":    &types.AttributeValueMemberS{Value: time.Now().Add(-time.Hour).Format(time.RFC3339)},
	}
	if instanceID != "" {
		item["instance_id"] = &types.AttributeValueMemberS{Value: instanceID}
	}
	return item
}

func TestRequeueHandler_NoJobsTable(t *testing.T) {
	t.Parallel()

	handler := NewRequeueHandler(nil, nil, nil, nil, "", NewAuthMiddleware(""))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/requeue-hung-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

// The operator action terminates the candidate's instance, so it must scan launched
// only — a running job has a live runner doing real work and must never be selected.
func TestRequeueHandler_ScansLaunchedOnly(t *testing.T) {
	t.Parallel()

	dyn := &mockRequeueDynamo{}
	handler := NewRequeueHandler(&mockRequeueEC2{}, dyn, &mockRequeuer{}, nil, "jobs-table", NewAuthMiddleware(""))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/requeue-hung-jobs", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if dyn.capturedScan == nil {
		t.Fatal("handler issued no scan")
	}
	var statusVals []string
	for _, v := range dyn.capturedScan.ExpressionAttributeValues {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			statusVals = append(statusVals, s.Value)
		}
	}
	if !slices.Contains(statusVals, string(db.JobStatusLaunched)) {
		t.Errorf("scan must filter on launched; status values = %v", statusVals)
	}
	if slices.Contains(statusVals, string(db.JobStatusRunning)) {
		t.Errorf("scan must NOT include running (it would terminate live jobs); status values = %v", statusVals)
	}
}

func TestRequeueHandler_RequeuesHungJob(t *testing.T) {
	t.Parallel()

	ec2Client := &mockRequeueEC2{instances: map[string]ec2types.InstanceStateName{
		"i-stuck": ec2types.InstanceStateNameRunning,
	}}
	dyn := &mockRequeueDynamo{items: []map[string]types.AttributeValue{
		requeueAdminItem(42, "i-stuck", 7, 0, "launched"),
	}}
	rq := &mockRequeuer{}
	metrics := &mockRequeueMetrics{}

	handler := NewRequeueHandler(ec2Client, dyn, rq, metrics, "jobs-table", NewAuthMiddleware(""))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/requeue-hung-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	var resp RequeueHungJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Requeued != 1 || resp.Candidates != 1 {
		t.Errorf("expected requeued=1 candidates=1, got %+v", resp)
	}
	if len(rq.sent) != 1 || rq.sent[0].JobID != 42 || !rq.sent[0].ForceOnDemand {
		t.Errorf("expected one on-demand requeue for job 42, got %+v", rq.sent)
	}
	if ec2Client.terminateCalls != 1 {
		t.Errorf("expected dead-agent instance terminated, got %d", ec2Client.terminateCalls)
	}
	if dyn.updateCalls != 1 {
		t.Errorf("expected record flipped to requeued, got %d updates", dyn.updateCalls)
	}
	// The handler must forward the operator_requeue counter through to metrics.
	if !slices.Equal(metrics.requeuedReasons, []string{"operator_requeue"}) {
		t.Errorf("expected one operator_requeue requeue metric, got %v", metrics.requeuedReasons)
	}
}

// A candidate whose retries are exhausted is reported as skipped and emits an
// operator_requeue scheduling-failure (never a requeue), so an operator sweep that
// can no longer recover a job is observable.
func TestRequeueHandler_ExhaustedEmitsSchedulingFailure(t *testing.T) {
	t.Parallel()

	ec2Client := &mockRequeueEC2{} // instance gone
	dyn := &mockRequeueDynamo{items: []map[string]types.AttributeValue{
		requeueAdminItem(42, "i-gone", 7, 2, "launched"), // retry_count == MaxRequeueRetries
	}}
	rq := &mockRequeuer{}
	metrics := &mockRequeueMetrics{}

	handler := NewRequeueHandler(ec2Client, dyn, rq, metrics, "jobs-table", NewAuthMiddleware(""))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/requeue-hung-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	var resp RequeueHungJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Requeued != 0 || resp.SkippedExhausted != 1 {
		t.Errorf("expected requeued=0 skipped_exhausted=1, got %+v", resp)
	}
	if len(metrics.requeuedReasons) != 0 {
		t.Errorf("exhausted job must not emit a requeue metric, got %v", metrics.requeuedReasons)
	}
	if !slices.Equal(metrics.schedulingFailures, []string{"operator_requeue"}) {
		t.Errorf("expected one operator_requeue scheduling-failure metric, got %v", metrics.schedulingFailures)
	}
}

func TestRequeueHandler_DryRunDoesNotMutate(t *testing.T) {
	t.Parallel()

	ec2Client := &mockRequeueEC2{} // instance gone
	dyn := &mockRequeueDynamo{items: []map[string]types.AttributeValue{
		requeueAdminItem(42, "i-gone", 7, 0, "launched"),
	}}
	rq := &mockRequeuer{}

	handler := NewRequeueHandler(ec2Client, dyn, rq, nil, "jobs-table", NewAuthMiddleware(""))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/requeue-hung-jobs?dry_run=true", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp RequeueHungJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Candidates != 1 || resp.Requeued != 0 {
		t.Errorf("dry run expected candidates=1 requeued=0, got %+v", resp)
	}
	if len(resp.JobIDs) != 1 || resp.JobIDs[0] != 42 {
		t.Errorf("dry run should report candidate job ids, got %v", resp.JobIDs)
	}
	if len(rq.sent) != 0 || dyn.updateCalls != 0 {
		t.Errorf("dry run must not mutate; sent=%d updates=%d", len(rq.sent), dyn.updateCalls)
	}
}

func TestRequeueHandler_ScanError(t *testing.T) {
	t.Parallel()

	ec2Client := &mockRequeueEC2{}
	dyn := &mockRequeueDynamo{scanErr: context.DeadlineExceeded}
	rq := &mockRequeuer{}

	handler := NewRequeueHandler(ec2Client, dyn, rq, nil, "jobs-table", NewAuthMiddleware(""))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/requeue-hung-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on scan error, got %d", rec.Code)
	}
}

func TestRequeueHandler_ThresholdBelowMinimumClampsToDefault(t *testing.T) {
	t.Parallel()

	// A 1-minute threshold is below the 10-minute floor; the hour-old candidate must
	// still be selected (i.e. the handler does not honor a sub-minimum threshold by
	// silently excluding everything — it clamps to the default).
	ec2Client := &mockRequeueEC2{} // gone
	dyn := &mockRequeueDynamo{items: []map[string]types.AttributeValue{
		requeueAdminItem(42, "i-gone", 7, 0, "launched"),
	}}
	rq := &mockRequeuer{}

	handler := NewRequeueHandler(ec2Client, dyn, rq, nil, "jobs-table", NewAuthMiddleware(""))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/requeue-hung-jobs?threshold_minutes=1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp RequeueHungJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Requeued != 1 {
		t.Errorf("expected requeued=1 with clamped threshold, got %+v", resp)
	}
}
