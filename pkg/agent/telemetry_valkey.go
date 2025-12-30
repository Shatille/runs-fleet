package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ValkeyTelemetry handles sending job status to Valkey Streams.
type ValkeyTelemetry struct {
	client *redis.Client
	stream string
	logger Logger
}

// Verify ValkeyTelemetry implements TelemetryClient.
var _ TelemetryClient = (*ValkeyTelemetry)(nil)

// NewValkeyTelemetry creates a new Valkey telemetry client.
func NewValkeyTelemetry(addr, password string, db int, stream string, logger Logger) (*ValkeyTelemetry, error) {
	return newValkeyTelemetryWithOptions(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	}, 5*time.Second, stream, logger)
}

// newValkeyTelemetryWithOptions creates a Valkey telemetry client with custom options.
// This is an internal function primarily for testing with custom timeouts.
func newValkeyTelemetryWithOptions(opts *redis.Options, pingTimeout time.Duration, stream string, logger Logger) (*ValkeyTelemetry, error) {
	client := redis.NewClient(opts)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Valkey: %w", err)
	}

	return &ValkeyTelemetry{
		client: client,
		stream: stream,
		logger: logger,
	}, nil
}

// SendJobStarted sends a job started notification.
func (t *ValkeyTelemetry) SendJobStarted(ctx context.Context, status JobStatus) error {
	status.Status = StatusStarted
	return t.sendMessage(ctx, status)
}

// SendJobCompleted sends a job completion notification.
func (t *ValkeyTelemetry) SendJobCompleted(ctx context.Context, status JobStatus) error {
	status.Status = DetermineCompletionStatus(status.InterruptedBy, status.ExitCode)
	return t.sendMessage(ctx, status)
}

// sendMessage sends a job status message to Valkey Stream.
func (t *ValkeyTelemetry) sendMessage(ctx context.Context, status JobStatus) error {
	body, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("failed to marshal status: %w", err)
	}

	// Retry up to 3 times
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		err = t.client.XAdd(ctx, &redis.XAddArgs{
			Stream: t.stream,
			Values: map[string]interface{}{
				"data":        string(body),
				"instance_id": status.InstanceID,
				"job_id":      status.JobID,
				"status":      status.Status,
			},
		}).Err()
		if err != nil {
			lastErr = err
			t.logger.Printf("Failed to send telemetry to Valkey (attempt %d/3): %v", attempt+1, err)
			continue
		}

		t.logger.Printf("Sent telemetry to Valkey: status=%s, job_id=%s", status.Status, status.JobID)
		return nil
	}

	return fmt.Errorf("failed to send telemetry after 3 attempts: %w", lastErr)
}

// Close closes the Valkey client connection.
func (t *ValkeyTelemetry) Close() error {
	return t.client.Close()
}
