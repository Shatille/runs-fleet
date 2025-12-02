package config

import (
	"testing"
	"time"
)

func TestTimeoutConstants(t *testing.T) {
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
