package circuit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// mockDynamoDB implements DynamoDBAPI for testing.
type mockDynamoDB struct {
	mu       sync.Mutex
	items    map[string]map[string]types.AttributeValue
	getErr   error
	putErr   error
	getCalls int
	putCalls int
}

func newMockDynamoDB() *mockDynamoDB {
	return &mockDynamoDB{
		items: make(map[string]map[string]types.AttributeValue),
	}
}

func (m *mockDynamoDB) GetItem(_ context.Context, params *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++

	if m.getErr != nil {
		return nil, m.getErr
	}

	var instanceType string
	if v, ok := params.Key["instance_type"]; ok {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			instanceType = s.Value
		}
	}

	item, exists := m.items[instanceType]
	if !exists {
		return &dynamodb.GetItemOutput{Item: nil}, nil
	}

	return &dynamodb.GetItemOutput{Item: item}, nil
}

func (m *mockDynamoDB) PutItem(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putCalls++

	if m.putErr != nil {
		return nil, m.putErr
	}

	var instanceType string
	if v, ok := params.Item["instance_type"]; ok {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			instanceType = s.Value
		}
	}

	m.items[instanceType] = params.Item
	return &dynamodb.PutItemOutput{}, nil
}

func (m *mockDynamoDB) UpdateItem(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	return &dynamodb.UpdateItemOutput{}, nil
}

func TestNewBreaker(t *testing.T) {
	// Create a breaker with mock (this tests the constructor structure)
	b := &Breaker{
		dynamoClient: newMockDynamoDB(),
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	if b.tableName != "test-table" {
		t.Errorf("expected tableName 'test-table', got '%s'", b.tableName)
	}

	if b.cache == nil {
		t.Error("expected cache to be initialized")
	}
}

func TestCheckCircuit_NoRecord(t *testing.T) {
	mock := newMockDynamoDB()
	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	state, err := b.CheckCircuit(context.Background(), "m5.large")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if state != StateClosed {
		t.Errorf("expected StateClosed for no record, got %s", state)
	}
}

func TestCheckCircuit_WithCachedState(t *testing.T) {
	mock := newMockDynamoDB()
	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	// Pre-populate cache
	b.cache["m5.large"] = &CachedState{
		State:    StateOpen,
		CachedAt: time.Now(),
		CacheTTL: 5 * time.Minute,
	}

	state, err := b.CheckCircuit(context.Background(), "m5.large")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if state != StateOpen {
		t.Errorf("expected StateOpen from cache, got %s", state)
	}

	// Should not have called DynamoDB
	if mock.getCalls > 0 {
		t.Errorf("expected no DynamoDB calls when cache is valid, got %d", mock.getCalls)
	}
}

func TestCheckCircuit_ExpiredCache(t *testing.T) {
	mock := newMockDynamoDB()
	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	// Pre-populate cache with expired entry
	b.cache["m5.large"] = &CachedState{
		State:    StateOpen,
		CachedAt: time.Now().Add(-10 * time.Minute), // Expired
		CacheTTL: 1 * time.Minute,
	}

	state, err := b.CheckCircuit(context.Background(), "m5.large")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return closed (no record in DynamoDB)
	if state != StateClosed {
		t.Errorf("expected StateClosed after cache expiry, got %s", state)
	}

	// Should have called DynamoDB
	if mock.getCalls == 0 {
		t.Error("expected DynamoDB call when cache is expired")
	}
}

func TestCheckCircuit_OpenState(t *testing.T) {
	mock := newMockDynamoDB()

	// Create a record with open state
	record := &Record{
		InstanceType:      "m5.large",
		State:             string(StateOpen),
		InterruptionCount: 5,
		AutoResetAt:       time.Now().Add(30 * time.Minute).Format(time.RFC3339),
	}
	item, _ := attributevalue.MarshalMap(record)
	mock.items["m5.large"] = item

	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	state, err := b.CheckCircuit(context.Background(), "m5.large")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if state != StateOpen {
		t.Errorf("expected StateOpen, got %s", state)
	}
}

func TestCheckCircuit_AutoReset(t *testing.T) {
	mock := newMockDynamoDB()

	// Create a record with open state that should auto-reset
	record := &Record{
		InstanceType:      "m5.large",
		State:             string(StateOpen),
		InterruptionCount: 5,
		AutoResetAt:       time.Now().Add(-10 * time.Minute).Format(time.RFC3339), // Past
	}
	item, _ := attributevalue.MarshalMap(record)
	mock.items["m5.large"] = item

	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	state, err := b.CheckCircuit(context.Background(), "m5.large")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should auto-reset to closed
	if state != StateClosed {
		t.Errorf("expected StateClosed after auto-reset, got %s", state)
	}

	// Should have saved the reset state
	if mock.putCalls == 0 {
		t.Error("expected PutItem call to save reset state")
	}
}

func TestCheckCircuit_DynamoDBError(t *testing.T) {
	mock := newMockDynamoDB()
	mock.getErr = errors.New("dynamodb error")

	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	state, err := b.CheckCircuit(context.Background(), "m5.large")
	if err == nil {
		t.Fatal("expected error from DynamoDB")
	}

	// Should return closed state on error (fail-open)
	if state != StateClosed {
		t.Errorf("expected StateClosed on error, got %s", state)
	}
}

func TestRecordInterruption_FirstInterruption(t *testing.T) {
	mock := newMockDynamoDB()
	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	err := b.RecordInterruption(context.Background(), "m5.large")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mock.putCalls != 1 {
		t.Errorf("expected 1 PutItem call, got %d", mock.putCalls)
	}

	// Check the stored record
	item := mock.items["m5.large"]
	if item == nil {
		t.Fatal("expected item to be stored")
	}

	var record Record
	if err := attributevalue.UnmarshalMap(item, &record); err != nil {
		t.Fatalf("failed to unmarshal record: %v", err)
	}

	if record.InterruptionCount != 1 {
		t.Errorf("expected InterruptionCount 1, got %d", record.InterruptionCount)
	}

	if record.State != string(StateClosed) {
		t.Errorf("expected StateClosed after first interruption, got %s", record.State)
	}
}

func TestRecordInterruption_OpensCircuit(t *testing.T) {
	mock := newMockDynamoDB()

	// Pre-populate with 2 interruptions
	record := &Record{
		InstanceType:        "m5.large",
		State:               string(StateClosed),
		InterruptionCount:   2,
		FirstInterruptionAt: time.Now().Format(time.RFC3339),
	}
	item, _ := attributevalue.MarshalMap(record)
	mock.items["m5.large"] = item

	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	// Third interruption should open circuit
	err := b.RecordInterruption(context.Background(), "m5.large")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check the stored record
	updatedItem := mock.items["m5.large"]
	var updatedRecord Record
	if err := attributevalue.UnmarshalMap(updatedItem, &updatedRecord); err != nil {
		t.Fatalf("failed to unmarshal record: %v", err)
	}

	if updatedRecord.State != string(StateOpen) {
		t.Errorf("expected StateOpen after threshold, got %s", updatedRecord.State)
	}

	if updatedRecord.InterruptionCount != 3 {
		t.Errorf("expected InterruptionCount 3, got %d", updatedRecord.InterruptionCount)
	}

	if updatedRecord.OpenedAt == "" {
		t.Error("expected OpenedAt to be set")
	}

	if updatedRecord.AutoResetAt == "" {
		t.Error("expected AutoResetAt to be set")
	}
}

func TestRecordInterruption_ResetsCountOutsideWindow(t *testing.T) {
	mock := newMockDynamoDB()

	// Pre-populate with old interruption (outside time window)
	record := &Record{
		InstanceType:        "m5.large",
		State:               string(StateClosed),
		InterruptionCount:   2,
		FirstInterruptionAt: time.Now().Add(-20 * time.Minute).Format(time.RFC3339), // 20 min ago
	}
	item, _ := attributevalue.MarshalMap(record)
	mock.items["m5.large"] = item

	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	err := b.RecordInterruption(context.Background(), "m5.large")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check the stored record - count should be reset to 1
	updatedItem := mock.items["m5.large"]
	var updatedRecord Record
	if err := attributevalue.UnmarshalMap(updatedItem, &updatedRecord); err != nil {
		t.Fatalf("failed to unmarshal record: %v", err)
	}

	if updatedRecord.InterruptionCount != 1 {
		t.Errorf("expected InterruptionCount 1 after window reset, got %d", updatedRecord.InterruptionCount)
	}
}

func TestRecordInterruption_InvalidatesCache(t *testing.T) {
	mock := newMockDynamoDB()
	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	// Pre-populate cache
	b.cache["m5.large"] = &CachedState{
		State:    StateClosed,
		CachedAt: time.Now(),
		CacheTTL: 5 * time.Minute,
	}

	err := b.RecordInterruption(context.Background(), "m5.large")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Cache should be invalidated
	if _, exists := b.cache["m5.large"]; exists {
		t.Error("expected cache to be invalidated after recording interruption")
	}
}

func TestRecordInterruption_DynamoDBError(t *testing.T) {
	mock := newMockDynamoDB()
	mock.getErr = errors.New("dynamodb get error")

	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	err := b.RecordInterruption(context.Background(), "m5.large")
	if err == nil {
		t.Fatal("expected error from DynamoDB")
	}
}

func TestRecordInterruption_PutError(t *testing.T) {
	mock := newMockDynamoDB()
	mock.putErr = errors.New("dynamodb put error")

	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	err := b.RecordInterruption(context.Background(), "m5.large")
	if err == nil {
		t.Fatal("expected error from DynamoDB put")
	}
}

func TestResetCircuit(t *testing.T) {
	mock := newMockDynamoDB()

	// Pre-populate with open circuit
	record := &Record{
		InstanceType:      "m5.large",
		State:             string(StateOpen),
		InterruptionCount: 5,
		OpenedAt:          time.Now().Format(time.RFC3339),
	}
	item, _ := attributevalue.MarshalMap(record)
	mock.items["m5.large"] = item

	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	// Pre-populate cache
	b.cache["m5.large"] = &CachedState{
		State:    StateOpen,
		CachedAt: time.Now(),
		CacheTTL: 5 * time.Minute,
	}

	err := b.ResetCircuit(context.Background(), "m5.large")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check the stored record
	updatedItem := mock.items["m5.large"]
	var updatedRecord Record
	if err := attributevalue.UnmarshalMap(updatedItem, &updatedRecord); err != nil {
		t.Fatalf("failed to unmarshal record: %v", err)
	}

	if updatedRecord.State != string(StateClosed) {
		t.Errorf("expected StateClosed after reset, got %s", updatedRecord.State)
	}

	if updatedRecord.InterruptionCount != 0 {
		t.Errorf("expected InterruptionCount 0 after reset, got %d", updatedRecord.InterruptionCount)
	}

	// Cache should be invalidated
	if _, exists := b.cache["m5.large"]; exists {
		t.Error("expected cache to be invalidated after reset")
	}
}

func TestResetCircuit_PutError(t *testing.T) {
	mock := newMockDynamoDB()
	mock.putErr = errors.New("dynamodb put error")

	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	err := b.ResetCircuit(context.Background(), "m5.large")
	if err == nil {
		t.Fatal("expected error from DynamoDB put")
	}
}

func TestCleanupExpiredCache(t *testing.T) {
	b := &Breaker{
		dynamoClient: newMockDynamoDB(),
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	// Add some cache entries
	b.cache["valid"] = &CachedState{
		State:    StateClosed,
		CachedAt: time.Now(),
		CacheTTL: 5 * time.Minute,
	}
	b.cache["expired"] = &CachedState{
		State:    StateOpen,
		CachedAt: time.Now().Add(-20 * time.Minute), // Expired (TTL * 2 = 10 min)
		CacheTTL: 5 * time.Minute,
	}

	b.cleanupExpiredCache()

	// Valid entry should remain
	if _, exists := b.cache["valid"]; !exists {
		t.Error("expected valid cache entry to remain")
	}

	// Expired entry should be removed
	if _, exists := b.cache["expired"]; exists {
		t.Error("expected expired cache entry to be removed")
	}
}

func TestStartCacheCleanup(_ *testing.T) {
	b := &Breaker{
		dynamoClient: newMockDynamoDB(),
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start cleanup routine
	b.StartCacheCleanup(ctx)

	// Cancel context to stop the goroutine
	cancel()

	// Give goroutine time to stop
	time.Sleep(10 * time.Millisecond)
}

func TestStateConstants(t *testing.T) {
	tests := []struct {
		state    State
		expected string
	}{
		{StateClosed, "closed"},
		{StateOpen, "open"},
		{StateHalfOpen, "half-open"},
	}

	for _, tt := range tests {
		if string(tt.state) != tt.expected {
			t.Errorf("expected state %s, got %s", tt.expected, tt.state)
		}
	}
}

func TestConstants(t *testing.T) {
	if InterruptionThreshold != 3 {
		t.Errorf("expected InterruptionThreshold 3, got %d", InterruptionThreshold)
	}

	if TimeWindow != 15*time.Minute {
		t.Errorf("expected TimeWindow 15m, got %v", TimeWindow)
	}

	if CooldownPeriod != 30*time.Minute {
		t.Errorf("expected CooldownPeriod 30m, got %v", CooldownPeriod)
	}
}

func TestConcurrentAccess(t *testing.T) {
	mock := newMockDynamoDB()
	b := &Breaker{
		dynamoClient: mock,
		tableName:    "test-table",
		cache:        make(map[string]*CachedState),
	}

	// Test concurrent access to CheckCircuit and RecordInterruption
	var wg sync.WaitGroup
	errors := make(chan error, 20)

	for i := 0; i < 10; i++ {
		wg.Add(2)

		go func() {
			defer wg.Done()
			_, err := b.CheckCircuit(context.Background(), "m5.large")
			if err != nil {
				errors <- err
			}
		}()

		go func() {
			defer wg.Done()
			err := b.RecordInterruption(context.Background(), "m5.large")
			if err != nil {
				errors <- err
			}
		}()
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("concurrent access error: %v", err)
	}
}
