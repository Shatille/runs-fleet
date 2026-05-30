package config

import "time"

// Common timeout durations used throughout the application.
const (
	// ShortTimeout for quick operations (message deletion, cleanup)
	ShortTimeout = 3 * time.Second

	// MessageReceiveTimeout for SQS long polling
	MessageReceiveTimeout = 25 * time.Second

	// MessageProcessTimeout for processing individual messages
	// Set to 90s to accommodate synchronous EC2 Fleet creation
	MessageProcessTimeout = 90 * time.Second

	// CleanupTimeout for deferred cleanup operations
	CleanupTimeout = 5 * time.Second

	// AWSResponseHeaderTimeout bounds how long an AWS SDK request waits for
	// response headers before failing
	AWSResponseHeaderTimeout = 10 * time.Second

	// AWSSQSResponseHeaderTimeout bounds the response-header wait for SQS clients
	// that long-poll. SQS withholds response headers for up to the 20s long-poll
	// wait on an empty queue, so this must exceed 20s; it stays below
	// MessageProcessTimeout so an empty poll never outlives the message budget.
	AWSSQSResponseHeaderTimeout = 25 * time.Second

	// AWSTCPUserTimeout bounds how long transmitted data may stay unacknowledged
	// before the kernel tears the connection down (Linux TCP_USER_TIMEOUT). It
	// covers the write/ACK phase that ResponseHeaderTimeout does not: a peer that
	// stops ACKing leaves bytes wedged in Send-Q with no RST, and the kernel would
	// otherwise retry for ~15 minutes (tcp_retries2). Must stay below
	// MessageProcessTimeout so the dead socket is dropped and the SDK retries on a
	// fresh connection well within the per-message budget.
	AWSTCPUserTimeout = 20 * time.Second

	// AWSKeepAliveIdle is the idle period before TCP keepalive probing begins on
	// AWS connections.
	AWSKeepAliveIdle = 15 * time.Second

	// AWSKeepAliveInterval is the gap between TCP keepalive probes.
	AWSKeepAliveInterval = 5 * time.Second

	// AWSKeepAliveCount is the number of unanswered TCP keepalive probes that
	// marks an idle connection dead.
	AWSKeepAliveCount = 4
)

// HTTP body size limits
const (
	// MaxBodySize is the maximum size for HTTP request bodies (1MB)
	MaxBodySize = 1 << 20 // 1 MB
)
