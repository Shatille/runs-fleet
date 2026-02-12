package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type mockHousekeepingEC2 struct {
	instances       map[string]ec2types.InstanceStateName
	err             error
	failBatchOnly   bool // If true, fail only when multiple instances requested
	callCount       int
}

func (m *mockHousekeepingEC2) DescribeInstances(_ context.Context, params *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	m.callCount++

	// Fail batch calls but allow individual lookups
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

type mockHousekeepingDynamo struct {
	items      []map[string]types.AttributeValue
	updateErr  error
	scanErr    error
	updateCalls int
}

func (m *mockHousekeepingDynamo) Scan(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	if m.scanErr != nil {
		return nil, m.scanErr
	}
	return &dynamodb.ScanOutput{
		Items: m.items,
	}, nil
}

func (m *mockHousekeepingDynamo) UpdateItem(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	m.updateCalls++
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func TestHousekeepingHandler_CleanupOrphanedJobs_NoJobsTable(t *testing.T) {
	handler := NewHousekeepingHandler(nil, nil, "", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", rec.Code)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_NoCandidates(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Cleaned != 0 {
		t.Errorf("expected 0 cleaned, got %d", resp.Cleaned)
	}
	if resp.Message != "No orphaned jobs found" {
		t.Errorf("unexpected message: %s", resp.Message)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_InstancesExist(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{
			"i-running": ec2types.InstanceStateNameRunning,
		},
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "123"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-running"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
		},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Cleaned != 0 {
		t.Errorf("expected 0 cleaned, got %d", resp.Cleaned)
	}
	if resp.Candidates != 1 {
		t.Errorf("expected 1 candidate, got %d", resp.Candidates)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_OrphanedFound(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{
			"i-terminated": ec2types.InstanceStateNameTerminated,
		},
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "123"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-terminated"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "124"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-nonexistent"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
		},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Cleaned != 2 {
		t.Errorf("expected 2 cleaned, got %d", resp.Cleaned)
	}
	if resp.Candidates != 2 {
		t.Errorf("expected 2 candidates, got %d", resp.Candidates)
	}
	if dynamoClient.updateCalls != 2 {
		t.Errorf("expected 2 update calls, got %d", dynamoClient.updateCalls)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_DryRun(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{},
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "123"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-gone"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
		},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs?dry_run=true", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Cleaned != 0 {
		t.Errorf("expected 0 cleaned (dry run), got %d", resp.Cleaned)
	}
	if len(resp.JobIDs) != 1 || resp.JobIDs[0] != 123 {
		t.Errorf("expected job ID 123 in dry run response, got %v", resp.JobIDs)
	}
	if dynamoClient.updateCalls != 0 {
		t.Errorf("expected 0 update calls in dry run, got %d", dynamoClient.updateCalls)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_ClaimingStatusNoInstance(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{},
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id": &types.AttributeValueMemberN{Value: "123"},
				// No instance_id - job stuck in claiming status
				"status": &types.AttributeValueMemberS{Value: "claiming"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "124"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-exists"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
		},
	}

	// Add running instance for job 124
	ec2Client.instances = map[string]ec2types.InstanceStateName{
		"i-exists": ec2types.InstanceStateNameRunning,
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Only job 123 (claiming without instance) should be cleaned
	// Job 124 has a running instance so should not be cleaned
	if resp.Cleaned != 1 {
		t.Errorf("expected 1 cleaned (claiming job without instance), got %d", resp.Cleaned)
	}
	if resp.Candidates != 2 {
		t.Errorf("expected 2 candidates, got %d", resp.Candidates)
	}
	if dynamoClient.updateCalls != 1 {
		t.Errorf("expected 1 update call, got %d", dynamoClient.updateCalls)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_ScanError(t *testing.T) {
	dynamoClient := &mockHousekeepingDynamo{
		scanErr: errors.New("DynamoDB scan failed"),
	}

	handler := NewHousekeepingHandler(nil, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}

	var resp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error != "Failed to scan for orphaned jobs" {
		t.Errorf("unexpected error message: %s", resp.Error)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_UpdateError(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{},
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "123"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-gone"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "124"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-also-gone"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
		},
		updateErr: errors.New("DynamoDB update failed"),
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// Should still return 200 - update errors are logged but don't fail the request
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Both updates failed, so cleaned should be 0
	if resp.Cleaned != 0 {
		t.Errorf("expected 0 cleaned (update errors), got %d", resp.Cleaned)
	}
	if resp.Candidates != 2 {
		t.Errorf("expected 2 candidates, got %d", resp.Candidates)
	}
	// Both updates were attempted
	if dynamoClient.updateCalls != 2 {
		t.Errorf("expected 2 update calls, got %d", dynamoClient.updateCalls)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_ConditionalCheckFailed(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{},
	}
	// Simulate ConditionalCheckFailedException (job status changed)
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "123"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-gone"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
		},
		updateErr: &types.ConditionalCheckFailedException{Message: aws.String("condition failed")},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// ConditionalCheckFailedException is treated as success (job already completed)
	if resp.Cleaned != 1 {
		t.Errorf("expected 1 cleaned (conditional check = already done), got %d", resp.Cleaned)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_CustomThreshold(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{},
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "123"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-gone"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
		},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Use custom threshold of 30 minutes
	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs?threshold_minutes=30", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_ThresholdBelowMinimum(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{},
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Threshold below minimum (10 minutes) should use default (120 minutes)
	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs?threshold_minutes=5", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// Should still work with default threshold
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_RunningStatusNoInstance(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{},
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id": &types.AttributeValueMemberN{Value: "123"},
				// Running status but no instance_id - should be skipped
				"status": &types.AttributeValueMemberS{Value: "running"},
			},
		},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Running without instance_id is skipped (not a candidate)
	if resp.Candidates != 0 {
		t.Errorf("expected 0 candidates (running without instance skipped), got %d", resp.Candidates)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_VariousInstanceStates(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{
			"i-running":      ec2types.InstanceStateNameRunning,
			"i-pending":      ec2types.InstanceStateNamePending,
			"i-stopping":     ec2types.InstanceStateNameStopping,
			"i-stopped":      ec2types.InstanceStateNameStopped,
			"i-shutting":     ec2types.InstanceStateNameShuttingDown,
			"i-terminated":   ec2types.InstanceStateNameTerminated,
		},
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "1"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-running"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "2"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-pending"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "3"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-stopping"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "4"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-stopped"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "5"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-shutting"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "6"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-terminated"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "7"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-nonexistent"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
		},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Only terminated and nonexistent instances should be orphaned
	// running, pending, stopping, stopped, shutting-down are all considered "existing"
	if resp.Cleaned != 2 {
		t.Errorf("expected 2 cleaned (terminated + nonexistent), got %d", resp.Cleaned)
	}
	if resp.Candidates != 7 {
		t.Errorf("expected 7 candidates, got %d", resp.Candidates)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_EC2Error(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		err: errors.New("EC2 API error"),
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "123"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-unknown"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
		},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// EC2 error causes instance to be considered non-existent (safe to orphan)
	if resp.Cleaned != 1 {
		t.Errorf("expected 1 cleaned (EC2 error = instance gone), got %d", resp.Cleaned)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_BatchFailFallbackSuccess(t *testing.T) {
	// Batch call fails, but individual lookups succeed
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{
			"i-running":    ec2types.InstanceStateNameRunning,
			"i-terminated": ec2types.InstanceStateNameTerminated,
		},
		err:           errors.New("batch request failed"),
		failBatchOnly: true,
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "1"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-running"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "2"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-terminated"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "3"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-nonexistent"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
		},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Batch failed, fell back to individual checks
	// i-running exists, i-terminated and i-nonexistent don't
	if resp.Cleaned != 2 {
		t.Errorf("expected 2 cleaned, got %d", resp.Cleaned)
	}
	if resp.Candidates != 3 {
		t.Errorf("expected 3 candidates, got %d", resp.Candidates)
	}
	// 1 batch call + 3 individual calls = 4 total
	if ec2Client.callCount != 4 {
		t.Errorf("expected 4 EC2 calls (1 batch + 3 individual), got %d", ec2Client.callCount)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_InvalidJobID(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			{
				"job_id":      &types.AttributeValueMemberN{Value: "invalid"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-test"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			{
				"job_id":      &types.AttributeValueMemberN{Value: "0"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-test2"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
		},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Invalid job IDs should be skipped
	if resp.Candidates != 0 {
		t.Errorf("expected 0 candidates (invalid job IDs skipped), got %d", resp.Candidates)
	}
}

func TestHousekeepingHandler_CleanupOrphanedJobs_MixedScenario(t *testing.T) {
	ec2Client := &mockHousekeepingEC2{
		instances: map[string]ec2types.InstanceStateName{
			"i-running":    ec2types.InstanceStateNameRunning,
			"i-terminated": ec2types.InstanceStateNameTerminated,
		},
	}
	dynamoClient := &mockHousekeepingDynamo{
		items: []map[string]types.AttributeValue{
			// Running job with running instance - not orphaned
			{
				"job_id":      &types.AttributeValueMemberN{Value: "1"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-running"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			// Running job with terminated instance - orphaned
			{
				"job_id":      &types.AttributeValueMemberN{Value: "2"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-terminated"},
				"status":      &types.AttributeValueMemberS{Value: "running"},
			},
			// Claiming job without instance - orphaned
			{
				"job_id": &types.AttributeValueMemberN{Value: "3"},
				"status": &types.AttributeValueMemberS{Value: "claiming"},
			},
			// Claiming job with instance (rare but possible) - check instance
			{
				"job_id":      &types.AttributeValueMemberN{Value: "4"},
				"instance_id": &types.AttributeValueMemberS{Value: "i-running"},
				"status":      &types.AttributeValueMemberS{Value: "claiming"},
			},
			// Running job without instance - skipped
			{
				"job_id": &types.AttributeValueMemberN{Value: "5"},
				"status": &types.AttributeValueMemberS{Value: "running"},
			},
		},
	}

	handler := NewHousekeepingHandler(ec2Client, dynamoClient, "test-jobs", NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/api/housekeeping/orphaned-jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp CleanupOrphanedJobsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Candidates: 1 (running+running), 2 (running+terminated), 3 (claiming), 4 (claiming+running)
	// Job 5 skipped (running without instance)
	if resp.Candidates != 4 {
		t.Errorf("expected 4 candidates, got %d", resp.Candidates)
	}
	// Orphaned: 2 (terminated), 3 (claiming without instance)
	// Job 1 and 4 have running instances
	if resp.Cleaned != 2 {
		t.Errorf("expected 2 cleaned, got %d", resp.Cleaned)
	}
}
