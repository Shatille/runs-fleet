package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

func TestRecordAudit_NoAuditTable(t *testing.T) {
	t.Parallel()

	client := &Client{dynamoClient: &MockDynamoDBAPI{}}
	err := client.RecordAudit(context.Background(), AuditEntry{User: "alice", Action: "pool.create"})
	if err == nil {
		t.Fatal("RecordAudit() should return error when audit table not configured")
	}
}

func TestRecordAudit_PutsExpectedItem(t *testing.T) {
	t.Parallel()

	var putItem map[string]types.AttributeValue
	mock := &MockDynamoDBAPI{
		PutItemFunc: func(_ context.Context, input *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			if input.TableName == nil || *input.TableName != testAuditTable {
				return nil, errors.New("unexpected table name")
			}
			putItem = input.Item
			return &dynamodb.PutItemOutput{}, nil
		},
	}

	client := &Client{dynamoClient: mock, auditTable: testAuditTable}

	entry := AuditEntry{
		User:     "alice",
		Action:   "pool.create",
		Target:   "default",
		Result:   "success",
		Details:  map[string]any{"changes": "desired_running: 0 -> 2"},
		ClientIP: "203.0.113.5",
	}
	if err := client.RecordAudit(context.Background(), entry); err != nil {
		t.Fatalf("RecordAudit() error = %v", err)
	}

	if putItem == nil {
		t.Fatal("PutItem was not called")
	}

	var record auditRecord
	if err := attributevalue.UnmarshalMap(putItem, &record); err != nil {
		t.Fatalf("unmarshal put item: %v", err)
	}

	if record.ID == "" {
		t.Error("expected a generated ULID id, got empty string")
	}
	if record.User != "alice" || record.Action != "pool.create" || record.Target != "default" || record.Result != "success" {
		t.Errorf("record = %+v, want user=alice action=pool.create target=default result=success", record)
	}
	if record.ClientIP != "203.0.113.5" {
		t.Errorf("ClientIP = %q, want 203.0.113.5", record.ClientIP)
	}
	if record.Timestamp == "" {
		t.Error("expected a non-empty timestamp")
	}
	if _, err := time.Parse(time.RFC3339, record.Timestamp); err != nil {
		t.Errorf("Timestamp %q is not RFC3339: %v", record.Timestamp, err)
	}
	if record.TTL <= time.Now().Unix() {
		t.Errorf("TTL = %d, want a future epoch timestamp", record.TTL)
	}
	wantTTL := time.Now().Add(auditRetention).Unix()
	if diff := record.TTL - wantTTL; diff < -5 || diff > 5 {
		t.Errorf("TTL = %d, want ~%d (90 days out)", record.TTL, wantTTL)
	}
}

func TestRecordAudit_DefaultsUserToAnonymous(t *testing.T) {
	t.Parallel()

	var putItem map[string]types.AttributeValue
	mock := &MockDynamoDBAPI{
		PutItemFunc: func(_ context.Context, input *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			putItem = input.Item
			return &dynamodb.PutItemOutput{}, nil
		},
	}
	client := &Client{dynamoClient: mock, auditTable: testAuditTable}

	if err := client.RecordAudit(context.Background(), AuditEntry{Action: "pool.create", Result: "success"}); err != nil {
		t.Fatalf("RecordAudit() error = %v", err)
	}

	var record auditRecord
	if err := attributevalue.UnmarshalMap(putItem, &record); err != nil {
		t.Fatalf("unmarshal put item: %v", err)
	}
	if record.User != "anonymous" {
		t.Errorf("User = %q, want anonymous", record.User)
	}
}

func TestListAuditLogs_NoAuditTable(t *testing.T) {
	t.Parallel()

	client := &Client{dynamoClient: &MockDynamoDBAPI{}}
	_, err := client.ListAuditLogs(context.Background(), AuditFilter{})
	if err == nil {
		t.Fatal("ListAuditLogs() should return error when audit table not configured")
	}
}

func TestListAuditLogs_GSIPath(t *testing.T) {
	t.Parallel()

	now := time.Now()
	since := now.Add(-1 * time.Hour)

	queryCalled := false
	mock := &MockDynamoDBAPI{
		QueryFunc: func(_ context.Context, input *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
			queryCalled = true
			if input.IndexName == nil || *input.IndexName != userTimestampIndexName {
				return nil, errors.New("expected GSI name " + userTimestampIndexName)
			}
			if input.KeyConditionExpression == nil || !strings.Contains(*input.KeyConditionExpression, "#user = :user") {
				return nil, errors.New("expected key condition on user")
			}
			item, _ := attributevalue.MarshalMap(auditRecord{
				ID:        "01HXYZ",
				User:      "alice",
				Action:    "pool.create",
				Timestamp: now.Format(time.RFC3339),
			})
			return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{item}}, nil
		},
		ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			t.Error("Scan should not be called when GSI query succeeds")
			return &dynamodb.ScanOutput{}, nil
		},
	}

	client := &Client{dynamoClient: mock, auditTable: testAuditTable}

	entries, err := client.ListAuditLogs(context.Background(), AuditFilter{User: "alice", Since: since})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !queryCalled {
		t.Fatal("Query was not called")
	}
	if len(entries) != 1 || entries[0].User != "alice" {
		t.Errorf("entries = %+v, want one entry for alice", entries)
	}
}

func TestListAuditLogs_GSIFallbackToScan(t *testing.T) {
	t.Parallel()

	scanCalled := false
	mock := &MockDynamoDBAPI{
		QueryFunc: func(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
			return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "index not found"}
		},
		ScanFunc: func(_ context.Context, input *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			scanCalled = true
			if input.FilterExpression == nil || !strings.Contains(*input.FilterExpression, "#user = :user") {
				return nil, errors.New("scan fallback must filter on user")
			}
			item, _ := attributevalue.MarshalMap(auditRecord{
				ID:        "01HXYZ",
				User:      "alice",
				Action:    "pool.delete",
				Timestamp: time.Now().Format(time.RFC3339),
			})
			return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{item}}, nil
		},
	}

	client := &Client{dynamoClient: mock, auditTable: testAuditTable}

	entries, err := client.ListAuditLogs(context.Background(), AuditFilter{User: "alice"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !scanCalled {
		t.Fatal("Scan was not called as fallback")
	}
	if len(entries) != 1 || entries[0].Action != "pool.delete" {
		t.Errorf("entries = %+v, want one entry with action pool.delete", entries)
	}
}

func TestListAuditLogs_GSINonValidationError(t *testing.T) {
	t.Parallel()

	mock := &MockDynamoDBAPI{
		QueryFunc: func(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
			return nil, errors.New("throttling: rate exceeded")
		},
		ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			t.Error("Scan should not be called for non-validation GSI errors")
			return &dynamodb.ScanOutput{}, nil
		},
	}

	client := &Client{dynamoClient: mock, auditTable: testAuditTable}

	_, err := client.ListAuditLogs(context.Background(), AuditFilter{User: "alice"})
	if err == nil {
		t.Fatal("expected error for non-validation GSI failure")
	}
	if !strings.Contains(err.Error(), "throttling") {
		t.Errorf("expected throttling error, got: %v", err)
	}
}

func TestListAuditLogs_NoUserFilterUsesScan(t *testing.T) {
	t.Parallel()

	scanCalled := false
	mock := &MockDynamoDBAPI{
		QueryFunc: func(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
			t.Error("Query should not be called without a user filter")
			return nil, errors.New("unexpected query")
		},
		ScanFunc: func(_ context.Context, input *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			scanCalled = true
			if input.FilterExpression != nil && strings.Contains(*input.FilterExpression, "#user = :user") {
				return nil, errors.New("scan should not filter on user when none given")
			}
			item, _ := attributevalue.MarshalMap(auditRecord{
				ID:        "01HXYZ",
				User:      "bob",
				Action:    "housekeeping.orphaned-jobs",
				Timestamp: time.Now().Format(time.RFC3339),
			})
			return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{item}}, nil
		},
	}

	client := &Client{dynamoClient: mock, auditTable: testAuditTable}

	entries, err := client.ListAuditLogs(context.Background(), AuditFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !scanCalled {
		t.Fatal("Scan was not called")
	}
	if len(entries) != 1 || entries[0].User != "bob" {
		t.Errorf("entries = %+v, want one entry for bob", entries)
	}
}

func TestListAuditLogs_FiltersByActionAndTimeRange(t *testing.T) {
	t.Parallel()

	now := time.Now()
	since := now.Add(-2 * time.Hour)
	until := now.Add(-1 * time.Hour)

	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, input *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			if input.FilterExpression == nil ||
				!strings.Contains(*input.FilterExpression, "#action = :action") ||
				!strings.Contains(*input.FilterExpression, "#timestamp >= :since") ||
				!strings.Contains(*input.FilterExpression, "#timestamp <= :until") {
				return nil, errors.New("expected action and time range filters")
			}
			return &dynamodb.ScanOutput{}, nil
		},
	}

	client := &Client{dynamoClient: mock, auditTable: testAuditTable}

	_, err := client.ListAuditLogs(context.Background(), AuditFilter{Action: "pool.create", Since: since, Until: until})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
