package config

import (
	"testing"
	"time"
)

func TestTimeoutConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		got      time.Duration
		want     time.Duration
		minValue time.Duration
	}{
		{
			name:     "ShortTimeout",
			got:      ShortTimeout,
			want:     3 * time.Second,
			minValue: time.Second,
		},
		{
			name:     "MessageReceiveTimeout",
			got:      MessageReceiveTimeout,
			want:     25 * time.Second,
			minValue: 10 * time.Second,
		},
		{
			name:     "MessageProcessTimeout",
			got:      MessageProcessTimeout,
			want:     90 * time.Second,
			minValue: 30 * time.Second,
		},
		{
			name:     "CleanupTimeout",
			got:      CleanupTimeout,
			want:     5 * time.Second,
			minValue: time.Second,
		},
		{
			name:     "AWSResponseHeaderTimeout",
			got:      AWSResponseHeaderTimeout,
			want:     10 * time.Second,
			minValue: time.Second,
		},
		{
			name:     "AWSSQSResponseHeaderTimeout",
			got:      AWSSQSResponseHeaderTimeout,
			want:     25 * time.Second,
			minValue: 21 * time.Second,
		},
		{
			name:     "AWSTCPUserTimeout",
			got:      AWSTCPUserTimeout,
			want:     20 * time.Second,
			minValue: time.Second,
		},
		{
			name:     "AWSKeepAliveIdle",
			got:      AWSKeepAliveIdle,
			want:     15 * time.Second,
			minValue: time.Second,
		},
		{
			name:     "AWSKeepAliveInterval",
			got:      AWSKeepAliveInterval,
			want:     5 * time.Second,
			minValue: time.Second,
		},
		{
			name:     "AWSSlowCallThreshold",
			got:      AWSSlowCallThreshold,
			want:     2 * time.Second,
			minValue: time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.got != tt.want {
				t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
			}

			// Also verify timeouts are above reasonable minimum
			if tt.got < tt.minValue {
				t.Errorf("%s = %v is below minimum reasonable value %v", tt.name, tt.got, tt.minValue)
			}
		})
	}
}

func TestMaxBodySize(t *testing.T) {
	t.Parallel()

	// MaxBodySize should be 1MB
	expectedSize := int64(1 << 20) // 1MB = 1048576 bytes

	if MaxBodySize != expectedSize {
		t.Errorf("MaxBodySize = %d, want %d (1MB)", MaxBodySize, expectedSize)
	}

	// Verify it's at least 1KB
	if MaxBodySize < 1024 {
		t.Errorf("MaxBodySize = %d is too small (< 1KB)", MaxBodySize)
	}

	// Verify it's not larger than 100MB (reasonable upper bound)
	if MaxBodySize > 100*1024*1024 {
		t.Errorf("MaxBodySize = %d is too large (> 100MB)", MaxBodySize)
	}
}

func TestTimeoutRelationships(t *testing.T) {
	t.Parallel()

	// MessageReceiveTimeout should be shorter than MessageProcessTimeout
	// This ensures we don't timeout waiting for messages while processing
	if MessageReceiveTimeout >= MessageProcessTimeout {
		t.Errorf("MessageReceiveTimeout (%v) should be less than MessageProcessTimeout (%v)",
			MessageReceiveTimeout, MessageProcessTimeout)
	}

	// ShortTimeout should be the smallest timeout
	if ShortTimeout >= MessageReceiveTimeout {
		t.Errorf("ShortTimeout (%v) should be less than MessageReceiveTimeout (%v)",
			ShortTimeout, MessageReceiveTimeout)
	}

	// CleanupTimeout should be reasonably short
	if CleanupTimeout > MessageReceiveTimeout {
		t.Errorf("CleanupTimeout (%v) should not exceed MessageReceiveTimeout (%v)",
			CleanupTimeout, MessageReceiveTimeout)
	}
}

func TestAWSSQSResponseHeaderTimeoutClearsLongPoll(t *testing.T) {
	t.Parallel()

	// Every SQS consumer long-polls with WaitTimeSeconds=20, during which SQS
	// withholds response headers on an empty queue. The SQS client's header
	// timeout must exceed that wait, or empty polls fail instead of returning
	// no-message responses.
	const longPollWait = 20 * time.Second

	if AWSSQSResponseHeaderTimeout <= longPollWait {
		t.Errorf("AWSSQSResponseHeaderTimeout (%v) must exceed the 20s long-poll wait (%v)",
			AWSSQSResponseHeaderTimeout, longPollWait)
	}

	// It must outlast the fast client so SQS gets the longer budget, but stay
	// below MessageProcessTimeout so an empty poll never outlives the per-message
	// processing budget.
	if AWSSQSResponseHeaderTimeout <= AWSResponseHeaderTimeout {
		t.Errorf("AWSSQSResponseHeaderTimeout (%v) must exceed AWSResponseHeaderTimeout (%v)",
			AWSSQSResponseHeaderTimeout, AWSResponseHeaderTimeout)
	}
	if AWSSQSResponseHeaderTimeout >= MessageProcessTimeout {
		t.Errorf("AWSSQSResponseHeaderTimeout (%v) must stay below MessageProcessTimeout (%v)",
			AWSSQSResponseHeaderTimeout, MessageProcessTimeout)
	}
}
func TestAWSTCPUserTimeoutBoundsWritePhase(t *testing.T) {
	t.Parallel()

	// TCP_USER_TIMEOUT tears down a connection whose transmitted data is never
	// ACKed. It must be positive and stay below MessageProcessTimeout so the dead
	// socket is dropped and the SDK retries on a fresh connection inside the
	// per-message budget, instead of the request hanging until the job context
	// expires.
	if AWSTCPUserTimeout <= 0 {
		t.Errorf("AWSTCPUserTimeout must be positive, got %v", AWSTCPUserTimeout)
	}
	if AWSTCPUserTimeout >= MessageProcessTimeout {
		t.Errorf("AWSTCPUserTimeout (%v) must stay below MessageProcessTimeout (%v) so a retry fits the job budget",
			AWSTCPUserTimeout, MessageProcessTimeout)
	}
}

func TestAWSKeepAliveConstantsAreSane(t *testing.T) {
	t.Parallel()

	if AWSKeepAliveIdle <= 0 {
		t.Errorf("AWSKeepAliveIdle must be positive, got %v", AWSKeepAliveIdle)
	}
	if AWSKeepAliveInterval <= 0 {
		t.Errorf("AWSKeepAliveInterval must be positive, got %v", AWSKeepAliveInterval)
	}
	if AWSKeepAliveCount <= 0 {
		t.Errorf("AWSKeepAliveCount must be positive, got %d", AWSKeepAliveCount)
	}

	// The full keepalive detection window (idle + interval*count) must clear an
	// idle connection well within MessageProcessTimeout.
	window := AWSKeepAliveIdle + AWSKeepAliveInterval*time.Duration(AWSKeepAliveCount)
	if window >= MessageProcessTimeout {
		t.Errorf("keepalive detection window (%v) must stay below MessageProcessTimeout (%v)",
			window, MessageProcessTimeout)
	}
}

func TestAWSSlowCallThreshold(t *testing.T) {
	t.Parallel()

	// The threshold must be positive so the middleware can ever flag a slow
	// call, and well below the per-message processing budget so a single slow
	// AWS operation is surfaced long before it consumes the whole budget and
	// surfaces only as an opaque context-deadline error.
	if AWSSlowCallThreshold <= 0 {
		t.Errorf("AWSSlowCallThreshold (%v) must be positive", AWSSlowCallThreshold)
	}
	if AWSSlowCallThreshold >= MessageProcessTimeout {
		t.Errorf("AWSSlowCallThreshold (%v) must stay below MessageProcessTimeout (%v)",
			AWSSlowCallThreshold, MessageProcessTimeout)
	}
}
