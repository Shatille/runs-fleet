package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
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
	updateErr    error
	updateCalls  int
	capturedScan *dynamodb.ScanInput
}

// GetItem serves the requeue sweep's pre-flight status re-read from the same
// fixture rows the scan returns.
func (m *mockRequeueDynamo) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
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

func (m *mockRequeueDynamo) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	m.capturedScan = in
	if m.scanErr != nil {
		return nil, m.scanErr
	}
	return &dynamodb.ScanOutput{Items: m.items}, nil
}

func (m *mockRequeueDynamo) UpdateItem(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	m.updateCalls++
	if m.updateErr != nil {
		return nil, m.updateErr
	}
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

func (m *mockRequeueMetrics) PublishQueueDepth(_ context.Context, _ string, _ float64) error {
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

	handler := NewRequeueHandler(nil, nil, nil, nil, "", nil, NewAuthMiddleware(""))
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
	handler := NewRequeueHandler(&mockRequeueEC2{}, dyn, &mockRequeuer{}, nil, "jobs-table", nil, NewAuthMiddleware(""))
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

	handler := NewRequeueHandler(ec2Client, dyn, rq, metrics, "jobs-table", nil, NewAuthMiddleware(""))
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

	handler := NewRequeueHandler(ec2Client, dyn, rq, metrics, "jobs-table", nil, NewAuthMiddleware(""))
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

	handler := NewRequeueHandler(ec2Client, dyn, rq, nil, "jobs-table", nil, NewAuthMiddleware(""))
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

	handler := NewRequeueHandler(ec2Client, dyn, rq, nil, "jobs-table", nil, NewAuthMiddleware(""))
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

	handler := NewRequeueHandler(ec2Client, dyn, rq, nil, "jobs-table", nil, NewAuthMiddleware(""))
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

func newSingleJobHandler(ec2Client *mockRequeueEC2, dyn *mockRequeueDynamo, rq *mockRequeuer, auditDB AuditDB) *http.ServeMux {
	handler := NewRequeueHandler(ec2Client, dyn, rq, nil, "jobs-table", auditDB, NewAuthMiddleware(""))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return mux
}

func postJSON(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
	return rec
}

// The per-job button re-dispatches the row the operator picked, terminating its
// dead-agent instance on the way.
func TestRequeueHandler_RequeueJob(t *testing.T) {
	t.Parallel()

	ec2Client := &mockRequeueEC2{instances: map[string]ec2types.InstanceStateName{
		"i-stuck": ec2types.InstanceStateNameRunning,
	}}
	dyn := &mockRequeueDynamo{items: []map[string]types.AttributeValue{
		requeueAdminItem(42, "i-stuck", 7, 0, "launched"),
	}}
	rq := &mockRequeuer{}
	auditDB := &mockAuditDB{}

	rec := postJSON(t, newSingleJobHandler(ec2Client, dyn, rq, auditDB), "/api/jobs/42/requeue")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", rec.Code, rec.Body.String())
	}

	var resp RequeueJobResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.JobID != 42 || resp.Outcome != "requeued" || !resp.InstanceTerminated {
		t.Errorf("response = %+v, want job 42 requeued with its instance terminated", resp)
	}
	if len(rq.sent) != 1 || rq.sent[0].JobID != 42 || !rq.sent[0].ForceOnDemand {
		t.Errorf("expected one on-demand requeue for job 42, got %+v", rq.sent)
	}
	if len(auditDB.entries) != 1 || auditDB.entries[0].Action != "job.requeue" ||
		auditDB.entries[0].Result != resultSuccess || auditDB.entries[0].Target != "42" {
		t.Errorf("audit entries = %+v, want one job.requeue success for target 42", auditDB.entries)
	}
}

// A refusal has to say why, or the operator cannot tell "already recovered" from
// "the button is broken".
func TestRequeueHandler_RequeueJobRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		item        map[string]types.AttributeValue
		wantCode    int
		wantOutcome string
		wantDetails string
	}{
		{
			name:        "running job keeps its live runner",
			item:        requeueAdminItem(42, "i-live", 7, 0, "running"),
			wantCode:    http.StatusConflict,
			wantOutcome: "wrong_status",
			wantDetails: "running",
		},
		{
			name:        "retry budget spent",
			item:        requeueAdminItem(42, "i-stuck", 7, 2, "launched"),
			wantCode:    http.StatusConflict,
			wantOutcome: "exhausted",
			wantDetails: "retries",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dyn := &mockRequeueDynamo{items: []map[string]types.AttributeValue{tt.item}}
			rq := &mockRequeuer{}
			ec2Client := &mockRequeueEC2{instances: map[string]ec2types.InstanceStateName{
				"i-live":  ec2types.InstanceStateNameRunning,
				"i-stuck": ec2types.InstanceStateNameRunning,
			}}
			auditDB := &mockAuditDB{}

			rec := postJSON(t, newSingleJobHandler(ec2Client, dyn, rq, auditDB), "/api/jobs/42/requeue")
			if rec.Code != tt.wantCode {
				t.Fatalf("expected %d, got %d (body %s)", tt.wantCode, rec.Code, rec.Body.String())
			}

			var resp RequeueJobResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Outcome != tt.wantOutcome {
				t.Errorf("outcome = %q, want %q", resp.Outcome, tt.wantOutcome)
			}
			if !strings.Contains(resp.Details, tt.wantDetails) {
				t.Errorf("details = %q, want it to mention %q", resp.Details, tt.wantDetails)
			}
			if len(rq.sent) != 0 || ec2Client.terminateCalls != 0 {
				t.Errorf("a refused requeue must not send or terminate; sent=%d terminates=%d",
					len(rq.sent), ec2Client.terminateCalls)
			}
			if len(auditDB.entries) != 1 || auditDB.entries[0].Result != "denied" {
				t.Errorf("audit entries = %+v, want one denied entry", auditDB.entries)
			}
		})
	}
}

func TestRequeueHandler_RequeueJobNotFound(t *testing.T) {
	t.Parallel()

	rec := postJSON(t, newSingleJobHandler(&mockRequeueEC2{}, &mockRequeueDynamo{}, &mockRequeuer{}, &mockAuditDB{}), "/api/jobs/42/requeue")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d (body %s)", rec.Code, rec.Body.String())
	}
}

func TestRequeueHandler_RequeueJobInvalidID(t *testing.T) {
	t.Parallel()

	rec := postJSON(t, newSingleJobHandler(&mockRequeueEC2{}, &mockRequeueDynamo{}, &mockRequeuer{}, &mockAuditDB{}), "/api/jobs/not-a-number/requeue")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d (body %s)", rec.Code, rec.Body.String())
	}
}

// Reconcile is for the job whose instance is definitively gone: it retires the
// record so the row stops looking live.
func TestRequeueHandler_ReconcileJobMarksOrphaned(t *testing.T) {
	t.Parallel()

	dyn := &mockRequeueDynamo{items: []map[string]types.AttributeValue{
		requeueAdminItem(42, "i-gone", 7, 0, "running"),
	}}
	auditDB := &mockAuditDB{}

	rec := postJSON(t, newSingleJobHandler(&mockRequeueEC2{}, dyn, &mockRequeuer{}, auditDB), "/api/jobs/42/reconcile")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", rec.Code, rec.Body.String())
	}

	var resp ReconcileJobResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.JobID != 42 || !resp.Orphaned {
		t.Errorf("response = %+v, want job 42 marked orphaned", resp)
	}
	if dyn.updateCalls != 1 {
		t.Errorf("expected exactly one record update, got %d", dyn.updateCalls)
	}
	if len(auditDB.entries) != 1 || auditDB.entries[0].Action != "job.reconcile" {
		t.Errorf("audit entries = %+v, want one job.reconcile entry", auditDB.entries)
	}
}

// Marking a job orphaned while its instance is alive would hide work in flight.
func TestRequeueHandler_ReconcileRefusesLiveInstance(t *testing.T) {
	t.Parallel()

	ec2Client := &mockRequeueEC2{instances: map[string]ec2types.InstanceStateName{
		"i-live": ec2types.InstanceStateNameRunning,
	}}
	dyn := &mockRequeueDynamo{items: []map[string]types.AttributeValue{
		requeueAdminItem(42, "i-live", 7, 0, "running"),
	}}

	rec := postJSON(t, newSingleJobHandler(ec2Client, dyn, &mockRequeuer{}, &mockAuditDB{}), "/api/jobs/42/reconcile")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if dyn.updateCalls != 0 {
		t.Errorf("a live instance's job must not be mutated, got %d updates", dyn.updateCalls)
	}
}

// A job that already reached a terminal state has nothing to reconcile.
func TestRequeueHandler_ReconcileRefusesTerminalJob(t *testing.T) {
	t.Parallel()

	dyn := &mockRequeueDynamo{items: []map[string]types.AttributeValue{
		requeueAdminItem(42, "i-gone", 7, 0, "success"),
	}}

	rec := postJSON(t, newSingleJobHandler(&mockRequeueEC2{}, dyn, &mockRequeuer{}, &mockAuditDB{}), "/api/jobs/42/reconcile")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if dyn.updateCalls != 0 {
		t.Errorf("a finished job must not be mutated, got %d updates", dyn.updateCalls)
	}
}

// The bulk sweep's outcome belongs in the persisted trail too, not only in logs.
func TestRequeueHandler_BulkSweepPersistsAudit(t *testing.T) {
	t.Parallel()

	dyn := &mockRequeueDynamo{items: []map[string]types.AttributeValue{
		requeueAdminItem(42, "i-gone", 7, 0, "launched"),
	}}
	auditDB := &mockAuditDB{}

	rec := postJSON(t, newSingleJobHandler(&mockRequeueEC2{}, dyn, &mockRequeuer{}, auditDB), "/api/housekeeping/requeue-hung-jobs")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if len(auditDB.entries) != 1 || auditDB.entries[0].Action != "housekeeping.requeue_hung_jobs" {
		t.Errorf("audit entries = %+v, want one housekeeping.requeue_hung_jobs entry", auditDB.entries)
	}
}

// A job that settles between the read and the guarded write was not reconciled by
// this call, and the operator must not be told otherwise.
func TestRequeueHandler_ReconcileLostRace(t *testing.T) {
	t.Parallel()

	dyn := &mockRequeueDynamo{
		items:     []map[string]types.AttributeValue{requeueAdminItem(42, "i-gone", 7, 0, "running")},
		updateErr: &types.ConditionalCheckFailedException{},
	}
	auditDB := &mockAuditDB{}

	rec := postJSON(t, newSingleJobHandler(&mockRequeueEC2{}, dyn, &mockRequeuer{}, auditDB), "/api/jobs/42/reconcile")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (body %s)", rec.Code, rec.Body.String())
	}

	var resp ReconcileJobResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Orphaned || resp.Outcome != "lost_race" {
		t.Errorf("response = %+v, want a lost-race refusal that does not claim the job was orphaned", resp)
	}
	if len(auditDB.entries) != 1 || auditDB.entries[0].Result != "denied" {
		t.Errorf("audit entries = %+v, want one denied entry", auditDB.entries)
	}
}

// Nothing to verify the record against, so the button refuses rather than
// guessing the job is dead.
func TestRequeueHandler_ReconcileRefusesJobWithNoInstance(t *testing.T) {
	t.Parallel()

	dyn := &mockRequeueDynamo{items: []map[string]types.AttributeValue{
		requeueAdminItem(42, "", 7, 0, "running"),
	}}
	auditDB := &mockAuditDB{}

	rec := postJSON(t, newSingleJobHandler(&mockRequeueEC2{}, dyn, &mockRequeuer{}, auditDB), "/api/jobs/42/reconcile")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (body %s)", rec.Code, rec.Body.String())
	}

	var resp ReconcileJobResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Orphaned || resp.Outcome != "no_instance" {
		t.Errorf("response = %+v, want a no-instance refusal", resp)
	}
	if dyn.updateCalls != 0 {
		t.Errorf("an unverifiable record must not be mutated, got %d updates", dyn.updateCalls)
	}
	if len(auditDB.entries) != 1 || auditDB.entries[0].Result != auditDenied {
		t.Errorf("audit entries = %+v, want one denied entry", auditDB.entries)
	}
}
