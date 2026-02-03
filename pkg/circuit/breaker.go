// Package circuit implements circuit breaker for spot instance interruptions.
package circuit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

var circuitLog = logging.WithComponent(logging.LogTypeCircuit, "breaker")

const (
	// InterruptionThreshold is the number of interruptions before circuit opens
	InterruptionThreshold = 3
	// TimeWindow is the time window for counting interruptions (15 minutes)
	TimeWindow = 15 * time.Minute
	// CooldownPeriod is how long to wait before auto-resetting circuit (30 minutes)
	CooldownPeriod = 30 * time.Minute
)

// State represents the state of a circuit breaker.
type State string

const (
	// StateClosed means spot instances are allowed
	StateClosed State = "closed"
	// StateOpen means spot instances are blocked, use on-demand
	StateOpen State = "open"
	// StateHalfOpen means testing if spot is stable again
	StateHalfOpen State = "half-open"
)

// DynamoDBAPI defines DynamoDB operations for circuit breaker state.
type DynamoDBAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// Record represents circuit breaker state in DynamoDB.
type Record struct {
	InstanceType        string `dynamodbav:"instance_type"`
	State               string `dynamodbav:"state"`
	InterruptionCount   int    `dynamodbav:"interruption_count"`
	FirstInterruptionAt string `dynamodbav:"first_interruption_at"`
	LastInterruptionAt  string `dynamodbav:"last_interruption_at"`
	OpenedAt            string `dynamodbav:"opened_at"`
	AutoResetAt         string `dynamodbav:"auto_reset_at"`
	TTL                 int64  `dynamodbav:"ttl"`
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
	State    State
	CachedAt time.Time
	CacheTTL time.Duration
}

// NewBreaker creates a new circuit breaker.
func NewBreaker(cfg aws.Config, tableName string) *Breaker {
	b := &Breaker{
		dynamoClient: dynamodb.NewFromConfig(cfg),
		tableName:    tableName,
		cache:        make(map[string]*CachedState),
	}
	return b
}

// StartCacheCleanup starts a background goroutine to periodically clean up expired cache entries.
// This prevents memory leaks from accumulating cache entries over time.
// Returns a channel that closes when the goroutine exits.
func (b *Breaker) StartCacheCleanup(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.cleanupExpiredCache()
			}
		}
	}()
	return done
}

// cleanupExpiredCache removes cache entries that have exceeded their TTL.
func (b *Breaker) cleanupExpiredCache() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	expiredKeys := make([]string, 0)

	for key, cached := range b.cache {
		expiresAt := cached.CachedAt.Add(cached.CacheTTL * 2)
		if now.After(expiresAt) {
			expiredKeys = append(expiredKeys, key)
		}
	}

	for _, key := range expiredKeys {
		delete(b.cache, key)
	}

	if len(expiredKeys) > 0 {
		circuitLog.Debug("cache cleanup completed", slog.Int(logging.KeyCount, len(expiredKeys)))
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
		record = &Record{
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

	record.InterruptionCount++
	record.LastInterruptionAt = now.Format(time.RFC3339)

	// Open circuit if threshold exceeded
	if record.InterruptionCount >= InterruptionThreshold && record.State == string(StateClosed) {
		record.State = string(StateOpen)
		record.OpenedAt = now.Format(time.RFC3339)
		record.AutoResetAt = now.Add(CooldownPeriod).Format(time.RFC3339)
		circuitLog.Warn("circuit breaker opened",
			slog.String(logging.KeyInstanceType, instanceType),
			slog.Int(logging.KeyCount, record.InterruptionCount))
	}

	// NOTE: Set TTL for DynamoDB cleanup (1 hour after auto-reset)
	if record.AutoResetAt != "" {
		autoResetTime, _ := time.Parse(time.RFC3339, record.AutoResetAt)
		record.TTL = autoResetTime.Add(1 * time.Hour).Unix()
	}

	if err := b.putRecord(ctx, record); err != nil {
		return fmt.Errorf("failed to save circuit record: %w", err)
	}

	b.mu.Lock()
	delete(b.cache, instanceType)
	b.mu.Unlock()

	return nil
}

// CheckCircuit checks the current circuit state for an instance type.
func (b *Breaker) CheckCircuit(ctx context.Context, instanceType string) (State, error) {
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

	state := State(record.State)

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
				circuitLog.Error("circuit reset failed",
					slog.String(logging.KeyInstanceType, instanceType),
					slog.String("error", err.Error()))
			} else {
				circuitLog.Info("circuit breaker auto-reset",
					slog.String(logging.KeyInstanceType, instanceType))
			}
		}
	}

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
	record := &Record{
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

	b.mu.Lock()
	delete(b.cache, instanceType)
	b.mu.Unlock()

	circuitLog.Info("circuit breaker manually reset",
		slog.String(logging.KeyInstanceType, instanceType))
	return nil
}

// getRecord retrieves circuit breaker state from DynamoDB.
func (b *Breaker) getRecord(ctx context.Context, instanceType string) (*Record, error) {
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

	var record Record
	if err := attributevalue.UnmarshalMap(output.Item, &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal item: %w", err)
	}

	return &record, nil
}

// putRecord saves circuit breaker state to DynamoDB.
func (b *Breaker) putRecord(ctx context.Context, record *Record) error {
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
