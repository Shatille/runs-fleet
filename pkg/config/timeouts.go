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

	// AWSSlowCallThreshold is the elapsed-time bound above which an AWS SDK
	// operation is logged as slow by the awsobs middleware. Calls slower than
	// this are surfaced so a wedged connection can be localized to a specific
	// service and operation before it escalates into a context-deadline cascade.
	AWSSlowCallThreshold = 2 * time.Second

	// AWSPerOpTimeout bounds a single AWS SDK operation so one wedged call cannot
	// consume the whole MessageProcessTimeout budget and cascade deadline errors
	// across the rest of a job; it must stay below MessageProcessTimeout. It sits
	// below the 20s SQS long-poll wait, so it must NOT be applied to SQS
	// ReceiveMessage (long-poll WaitTimeSeconds=20) or it would abort every empty
	// poll; the per-operation timeout middleware exempts ReceiveMessage.
	AWSPerOpTimeout = 15 * time.Second

	// MaxShutdownDrainDelay caps RUNS_FLEET_SHUTDOWN_DRAIN_DELAY_SECONDS. On
	// SIGTERM the server waits out the drain delay, then drains workers (up to
	// MessageProcessTimeout + 10s ≈ 100s), then flushes telemetry — the sum must
	// stay under the deploy's stopTimeout/terminationGracePeriodSeconds (120s in
	// deploy/) or the task is SIGKILLed mid-drain. This cap keeps the drain small
	// enough to hold that budget with margin; raising it requires raising the
	// deploy grace period to match. It also stays well below MessageProcessTimeout
	// so the drain can never consume the graceful-shutdown context.
	MaxShutdownDrainDelay = 15 * time.Second
)

// HTTP body size limits
const (
	// MaxBodySize is the maximum size for HTTP request bodies (1MB)
	MaxBodySize = 1 << 20 // 1 MB
)
