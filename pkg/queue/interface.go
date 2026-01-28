// Package queue provides message queue abstractions for job orchestration.
// Supports SQS (EC2 mode) and Valkey Streams (K8s mode).
package queue

import (
	"context"
)

// Pinger is an optional interface for health checking queue connectivity.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Queue defines the interface for message queue operations.
// Implementations must provide FIFO semantics per message group.
type Queue interface {
	// SendMessage publishes a job message to the queue.
	// Returns error if the message cannot be sent.
	SendMessage(ctx context.Context, job *JobMessage) error

	// ReceiveMessages retrieves up to maxMessages with long polling.
	// waitTimeSeconds controls how long to wait for messages (0 = no wait).
	// Returns empty slice if no messages available.
	ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]Message, error)

	// DeleteMessage acknowledges successful message processing.
	// handle is the receipt handle (SQS) or message ID (Valkey).
	DeleteMessage(ctx context.Context, handle string) error
}

// Message represents a queue message in a provider-agnostic format.
type Message struct {
	// ID is the unique message identifier.
	ID string

	// Body contains the JSON-encoded JobMessage.
	Body string

	// Handle is used to delete/acknowledge the message.
	// For SQS: ReceiptHandle
	// For Valkey: stream message ID
	Handle string

	// Attributes contains message metadata (trace context, etc).
	Attributes map[string]string
}

// JobMessage represents workflow job configuration for queue transport.
type JobMessage struct {
	JobID         int64    `json:"job_id,omitempty"`
	RunID         int64    `json:"run_id"`
	Repo          string   `json:"repo,omitempty"`
	InstanceType  string   `json:"instance_type"`
	Pool          string   `json:"pool,omitempty"`
	Spot          bool     `json:"spot"`
	OriginalLabel string   `json:"original_label,omitempty"`
	RetryCount    int      `json:"retry_count,omitempty"`
	ForceOnDemand bool     `json:"force_on_demand,omitempty"`
	Region        string   `json:"region,omitempty"`
	Environment   string   `json:"environment,omitempty"`
	OS            string   `json:"os,omitempty"`   // linux, windows
	Arch          string   `json:"arch,omitempty"` // amd64, arm64
	InstanceTypes []string `json:"instance_types,omitempty"`
	StorageGiB    int      `json:"storage_gib,omitempty"` // Disk storage in GiB
	PublicIP      bool     `json:"public_ip,omitempty"`   // Request public IP (uses public subnet)
	TraceID       string   `json:"trace_id,omitempty"`
	SpanID        string   `json:"span_id,omitempty"`
	ParentID      string   `json:"parent_id,omitempty"`
}

// ExtractTraceContext extracts OpenTelemetry trace context from message attributes.
func ExtractTraceContext(msg Message) (traceID, spanID, parentID string) {
	if msg.Attributes == nil {
		return "", "", ""
	}
	return msg.Attributes["TraceID"], msg.Attributes["SpanID"], msg.Attributes["ParentID"]
}
