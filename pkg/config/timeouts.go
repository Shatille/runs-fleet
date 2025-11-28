package config

import "time"

// Common timeout durations used throughout the application.
const (
	// ShortTimeout for quick operations (message deletion, cleanup)
	ShortTimeout = 3 * time.Second

	// MessageReceiveTimeout for SQS long polling
	MessageReceiveTimeout = 25 * time.Second

	// MessageProcessTimeout for processing individual messages
	// Set to 90s to accommodate synchronous EC2 Fleet creation (FleetTypeInstant)
	MessageProcessTimeout = 90 * time.Second

	// CleanupTimeout for deferred cleanup operations
	CleanupTimeout = 5 * time.Second
)

// HTTP body size limits
const (
	// MaxBodySize is the maximum size for HTTP request bodies (1MB)
	MaxBodySize = 1 << 20 // 1 MB
)
