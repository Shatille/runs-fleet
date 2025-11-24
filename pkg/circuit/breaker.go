// Package circuit implements circuit breaker for spot instance interruptions.
package circuit

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

const (
	// InterruptionThreshold is the number of interruptions before circuit opens
	InterruptionThreshold = 3
	// TimeWindow is the time window for counting interruptions (15 minutes)
	TimeWindow = 15 * time.Minute
	// CooldownPeriod is how long to wait before auto-resetting circuit (30 minutes)
	CooldownPeriod = 30 * time.Minute
)

// CircuitState represents the state of a circuit breaker.
type CircuitState string

const (
	// StateClosed means spot instances are allowed
	StateClosed CircuitState = "closed"
	// StateOpen means spot instances are blocked, use on-demand
	StateOpen CircuitState = "open"
	// StateHalfOpen means testing if spot is stable again
	StateHalfOpen CircuitState = "half-open"
)

// DynamoDBAPI defines DynamoDB operations for circuit breaker state.
type DynamoDBAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// CircuitRecord represents circuit breaker state in DynamoDB.
type CircuitRecord struct {
	InstanceType         string    `dynamodbav:"instance_type"`
	State                string    `dynamodbav:"state"`
	InterruptionCount    int       `dynamodbav:"interruption_count"`
	FirstInterruptionAt  string    `dynamodbav:"first_interruption_at"`
	LastInterruptionAt   string    `dynamodbav:"last_interruption_at"`
	OpenedAt             string    `dynamodbav:"opened_at"`
	AutoResetAt          string    `dynamodbav:"auto_reset_at"`
	TTL                  int64     `dynamodbav:"ttl"`
}

// Breaker implements circuit breaker pattern for spot interruptions.
type Breaker struct {
	dynamoClient DynamoDBAPI
	tableName    string
	mu           sync.RWMutex
	cache        map[string]*CachedState
}

// CachedState represents cached circuit state.
type CachedState struct {
	State     CircuitState
	CachedAt  time.Time
	CacheTTL  time.Duration
}

// NewBreaker creates a new circuit breaker.
func NewBreaker(cfg aws.Config, tableName string) *Breaker {
	return &Breaker{
		dynamoClient: dynamodb.NewFromConfig(cfg),
		tableName:    tableName,
		cache:        make(map[string]*CachedState),
	}
}

// RecordInterruption records a spot interruption and updates circuit state.
func (b *Breaker) RecordInterruption(ctx context.Context, instanceType string) error {
	now := time.Now()

	// Get current state
	record, err := b.getRecord(ctx, instanceType)
	if err != nil {
		return fmt.Errorf("failed to get circuit record: %w", err)
	}

	// Initialize if new
	if record == nil {
		record = &CircuitRecord{
			InstanceType:        instanceType,
			State:               string(StateClosed),
			InterruptionCount:   0,
			FirstInterruptionAt: now.Format(time.RFC3339),
		}
	}

	// Parse first interruption time
	var firstInterruptionTime time.Time
	if record.FirstInterruptionAt != "" {
		firstInterruptionTime, _ = time.Parse(time.RFC3339, record.FirstInterruptionAt)
	}

	// Reset count if outside time window
	if !firstInterruptionTime.IsZero() && now.Sub(firstInterruptionTime) > TimeWindow {
		record.InterruptionCount = 0
		record.FirstInterruptionAt = now.Format(time.RFC3339)
	}

	// Increment count
	record.InterruptionCount++
	record.LastInterruptionAt = now.Format(time.RFC3339)

	// Open circuit if threshold exceeded
	if record.InterruptionCount >= InterruptionThreshold && record.State == string(StateClosed) {
		record.State = string(StateOpen)
		record.OpenedAt = now.Format(time.RFC3339)
		record.AutoResetAt = now.Add(CooldownPeriod).Format(time.RFC3339)
		log.Printf("Circuit breaker OPENED for instance type %s after %d interruptions", instanceType, record.InterruptionCount)
	}

	// Set TTL for DynamoDB cleanup (1 hour after auto-reset)
	if record.AutoResetAt != "" {
		autoResetTime, _ := time.Parse(time.RFC3339, record.AutoResetAt)
		record.TTL = autoResetTime.Add(1 * time.Hour).Unix()
	}

	// Save record
	if err := b.putRecord(ctx, record); err != nil {
		return fmt.Errorf("failed to save circuit record: %w", err)
	}

	// Invalidate cache
	b.mu.Lock()
	delete(b.cache, instanceType)
	b.mu.Unlock()

	return nil
}

// CheckCircuit checks the current circuit state for an instance type.
func (b *Breaker) CheckCircuit(ctx context.Context, instanceType string) (CircuitState, error) {
	// Check cache first
	b.mu.RLock()
	cached, exists := b.cache[instanceType]
	b.mu.RUnlock()

	if exists && time.Since(cached.CachedAt) < cached.CacheTTL {
		return cached.State, nil
	}

	// Get from DynamoDB
	record, err := b.getRecord(ctx, instanceType)
	if err != nil {
		return StateClosed, fmt.Errorf("failed to get circuit record: %w", err)
	}

	// Default to closed if no record
	if record == nil {
		return StateClosed, nil
	}

	state := CircuitState(record.State)

	// Check for auto-reset
	if state == StateOpen && record.AutoResetAt != "" {
		autoResetTime, err := time.Parse(time.RFC3339, record.AutoResetAt)
		if err == nil && time.Now().After(autoResetTime) {
			state = StateClosed
			record.State = string(StateClosed)
			record.InterruptionCount = 0
			record.FirstInterruptionAt = ""
			record.LastInterruptionAt = ""
			record.OpenedAt = ""
			record.AutoResetAt = ""

			if err := b.putRecord(ctx, record); err != nil {
				log.Printf("Failed to reset circuit: %v", err)
			} else {
				log.Printf("Circuit breaker auto-RESET for instance type %s", instanceType)
			}
		}
	}

	// Cache the result
	b.mu.Lock()
	b.cache[instanceType] = &CachedState{
		State:    state,
		CachedAt: time.Now(),
		CacheTTL: 1 * time.Minute,
	}
	b.mu.Unlock()

	return state, nil
}

// ResetCircuit manually resets the circuit breaker for an instance type.
func (b *Breaker) ResetCircuit(ctx context.Context, instanceType string) error {
	record := &CircuitRecord{
		InstanceType:        instanceType,
		State:               string(StateClosed),
		InterruptionCount:   0,
		FirstInterruptionAt: "",
		LastInterruptionAt:  "",
		OpenedAt:            "",
		AutoResetAt:         "",
	}

	if err := b.putRecord(ctx, record); err != nil {
		return fmt.Errorf("failed to reset circuit: %w", err)
	}

	// Invalidate cache
	b.mu.Lock()
	delete(b.cache, instanceType)
	b.mu.Unlock()

	log.Printf("Circuit breaker manually RESET for instance type %s", instanceType)
	return nil
}

// getRecord retrieves circuit breaker state from DynamoDB.
func (b *Breaker) getRecord(ctx context.Context, instanceType string) (*CircuitRecord, error) {
	key, err := attributevalue.MarshalMap(map[string]string{
		"instance_type": instanceType,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal key: %w", err)
	}

	output, err := b.dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(b.tableName),
		Key:       key,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get item: %w", err)
	}

	if output.Item == nil {
		return nil, nil
	}

	var record CircuitRecord
	if err := attributevalue.UnmarshalMap(output.Item, &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal item: %w", err)
	}

	return &record, nil
}

// putRecord saves circuit breaker state to DynamoDB.
func (b *Breaker) putRecord(ctx context.Context, record *CircuitRecord) error {
	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("failed to marshal record: %w", err)
	}

	_, err = b.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(b.tableName),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("failed to put item: %w", err)
	}

	return nil
}
