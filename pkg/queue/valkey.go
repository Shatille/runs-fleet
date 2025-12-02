package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ValkeyClient implements Queue interface using Valkey/Redis Streams.
type ValkeyClient struct {
	client     *redis.Client
	stream     string
	group      string
	consumerID string
}

// Verify ValkeyClient implements Queue interface.
var _ Queue = (*ValkeyClient)(nil)

// ValkeyConfig holds Valkey connection settings.
type ValkeyConfig struct {
	Addr       string
	Password   string
	DB         int
	Stream     string
	Group      string
	ConsumerID string
}

// NewValkeyClient creates a Valkey-backed queue client.
func NewValkeyClient(cfg ValkeyConfig) (*ValkeyClient, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	consumerID := cfg.ConsumerID
	if consumerID == "" {
		consumerID = uuid.New().String()
	}

	vc := &ValkeyClient{
		client:     client,
		stream:     cfg.Stream,
		group:      cfg.Group,
		consumerID: consumerID,
	}

	if err := vc.ensureConsumerGroup(context.Background()); err != nil {
		_ = client.Close()
		return nil, err
	}

	return vc, nil
}

// NewValkeyClientWithRedis creates a Valkey client with an existing redis.Client (for testing).
func NewValkeyClientWithRedis(client *redis.Client, stream, group, consumerID string) *ValkeyClient {
	return &ValkeyClient{
		client:     client,
		stream:     stream,
		group:      group,
		consumerID: consumerID,
	}
}

func (c *ValkeyClient) ensureConsumerGroup(ctx context.Context) error {
	err := c.client.XGroupCreateMkStream(ctx, c.stream, c.group, "0").Err()
	if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
		return fmt.Errorf("failed to create consumer group: %w", err)
	}
	return nil
}

// SendMessage publishes job message to Valkey stream.
func (c *ValkeyClient) SendMessage(ctx context.Context, job *JobMessage) error {
	if job.JobID == "" {
		return fmt.Errorf("job ID is required")
	}
	if job.RunID == "" {
		return fmt.Errorf("run ID is required")
	}

	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("failed to marshal job: %w", err)
	}

	values := map[string]interface{}{
		"body":     string(body),
		"run_id":   job.RunID,
		"job_id":   job.JobID,
		"trace_id": job.TraceID,
		"span_id":  job.SpanID,
	}

	_, err = c.client.XAdd(ctx, &redis.XAddArgs{
		Stream: c.stream,
		Values: values,
	}).Result()
	if err != nil {
		return fmt.Errorf("failed to add message to stream: %w", err)
	}

	return nil
}

// ReceiveMessages retrieves messages from Valkey stream using consumer group.
func (c *ValkeyClient) ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]Message, error) {
	blockDuration := time.Duration(waitTimeSeconds) * time.Second

	streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.group,
		Consumer: c.consumerID,
		Streams:  []string{c.stream, ">"},
		Count:    int64(maxMessages),
		Block:    blockDuration,
	}).Result()

	if errors.Is(err, redis.Nil) {
		return []Message{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read from stream: %w", err)
	}

	var messages []Message
	for _, stream := range streams {
		for _, xmsg := range stream.Messages {
			msg := Message{
				ID:         xmsg.ID,
				Handle:     xmsg.ID,
				Attributes: make(map[string]string),
			}

			if body, ok := xmsg.Values["body"].(string); ok {
				msg.Body = body
			}
			if traceID, ok := xmsg.Values["trace_id"].(string); ok && traceID != "" {
				msg.Attributes["TraceID"] = traceID
			}
			if spanID, ok := xmsg.Values["span_id"].(string); ok && spanID != "" {
				msg.Attributes["SpanID"] = spanID
			}

			messages = append(messages, msg)
		}
	}

	return messages, nil
}

// DeleteMessage acknowledges message processing via XACK.
func (c *ValkeyClient) DeleteMessage(ctx context.Context, handle string) error {
	_, err := c.client.XAck(ctx, c.stream, c.group, handle).Result()
	if err != nil {
		return fmt.Errorf("failed to acknowledge message: %w", err)
	}
	return nil
}

// Close closes the Valkey client connection.
func (c *ValkeyClient) Close() error {
	return c.client.Close()
}

// Ping checks connectivity to Valkey.
func (c *ValkeyClient) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}
