package metrics

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestNewDatadogPublisher(t *testing.T) {
	// Start a UDP listener to accept DogStatsD packets
	addr, cleanup := startUDPServer(t)
	defer cleanup()

	tests := []struct {
		name    string
		cfg     DatadogConfig
		wantErr bool
	}{
		{
			name: "default config",
			cfg: DatadogConfig{
				Address: addr,
			},
			wantErr: false,
		},
		{
			name: "custom namespace",
			cfg: DatadogConfig{
				Address:   addr,
				Namespace: "custom_namespace",
			},
			wantErr: false,
		},
		{
			name: "with tags",
			cfg: DatadogConfig{
				Address: addr,
				Tags:    []string{"env:test", "service:runs-fleet"},
			},
			wantErr: false,
		},
		{
			name:    "empty address uses default",
			cfg:     DatadogConfig{},
			wantErr: false, // UDP is connectionless, client creation succeeds even without listener
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub, err := NewDatadogPublisher(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewDatadogPublisher() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil {
				if pub == nil {
					t.Error("NewDatadogPublisher() returned nil publisher")
				}
				if pub != nil && pub.client == nil {
					t.Error("NewDatadogPublisher() returned publisher with nil client")
				}
				if pub != nil {
					_ = pub.Close()
				}
			}
		})
	}
}

func TestDatadogPublisher_Close(t *testing.T) {
	addr, cleanup := startUDPServer(t)
	defer cleanup()

	pub, err := NewDatadogPublisher(DatadogConfig{Address: addr})
	if err != nil {
		t.Fatalf("NewDatadogPublisher() error = %v", err)
	}

	err = pub.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

//nolint:dupl // Test tables are intentionally similar - testing different publishers
func TestDatadogPublisher_PublishMethods(t *testing.T) {
	addr, cleanup := startUDPServer(t)
	defer cleanup()

	pub, err := NewDatadogPublisher(DatadogConfig{
		Address:   addr,
		Namespace: "test",
	})
	if err != nil {
		t.Fatalf("NewDatadogPublisher() error = %v", err)
	}
	defer func() { _ = pub.Close() }()

	ctx := context.Background()

	tests := []struct {
		name    string
		publish func() error
	}{
		{"PublishQueueDepth", func() error { return pub.PublishQueueDepth(ctx, 10.0) }},
		{"PublishFleetSizeIncrement", func() error { return pub.PublishFleetSizeIncrement(ctx) }},
		{"PublishFleetSizeDecrement", func() error { return pub.PublishFleetSizeDecrement(ctx) }},
		{"PublishJobDuration", func() error { return pub.PublishJobDuration(ctx, 120) }},
		{"PublishJobSuccess", func() error { return pub.PublishJobSuccess(ctx) }},
		{"PublishJobFailure", func() error { return pub.PublishJobFailure(ctx) }},
		{"PublishJobQueued", func() error { return pub.PublishJobQueued(ctx) }},
		{"PublishSpotInterruption", func() error { return pub.PublishSpotInterruption(ctx) }},
		{"PublishMessageDeletionFailure", func() error { return pub.PublishMessageDeletionFailure(ctx) }},
		{"PublishCacheHit", func() error { return pub.PublishCacheHit(ctx) }},
		{"PublishCacheMiss", func() error { return pub.PublishCacheMiss(ctx) }},
		{"PublishOrphanedInstancesTerminated", func() error { return pub.PublishOrphanedInstancesTerminated(ctx, 5) }},
		{"PublishSSMParametersDeleted", func() error { return pub.PublishSSMParametersDeleted(ctx, 3) }},
		{"PublishJobRecordsArchived", func() error { return pub.PublishJobRecordsArchived(ctx, 10) }},
		{"PublishPoolUtilization", func() error { return pub.PublishPoolUtilization(ctx, "default", 75.5) }},
		{"PublishSchedulingFailure", func() error { return pub.PublishSchedulingFailure(ctx, "runner-provision") }},
		{"PublishCircuitBreakerTriggered", func() error { return pub.PublishCircuitBreakerTriggered(ctx, "t4g.medium") }},
		{"PublishJobClaimFailure", func() error { return pub.PublishJobClaimFailure(ctx) }},
		{"PublishWarmPoolHit", func() error { return pub.PublishWarmPoolHit(ctx) }},
		{"PublishFleetSize", func() error { return pub.PublishFleetSize(ctx, 5) }},
		{"PublishServiceCheck_OK", func() error { return pub.PublishServiceCheck(ctx, "health", ServiceCheckOK, "all good") }},
		{"PublishServiceCheck_Warning", func() error { return pub.PublishServiceCheck(ctx, "health", ServiceCheckWarning, "degraded") }},
		{"PublishServiceCheck_Critical", func() error { return pub.PublishServiceCheck(ctx, "health", ServiceCheckCritical, "down") }},
		{"PublishServiceCheck_Unknown", func() error { return pub.PublishServiceCheck(ctx, "health", ServiceCheckUnknown, "unknown") }},
		{"PublishEvent_Info", func() error { return pub.PublishEvent(ctx, "Test Event", "Event body", "info", nil) }},
		{"PublishEvent_Warning", func() error { return pub.PublishEvent(ctx, "Warning Event", "Body", "warning", []string{"key:value"}) }},
		{"PublishEvent_Error", func() error { return pub.PublishEvent(ctx, "Error Event", "Body", "error", nil) }},
		{"PublishEvent_Success", func() error { return pub.PublishEvent(ctx, "Success Event", "Body", "success", nil) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.publish()
			if err != nil {
				t.Errorf("%s() error = %v", tt.name, err)
			}
		})
	}
}

func TestDatadogPublisher_DefaultNamespace(t *testing.T) {
	addr, cleanup := startUDPServer(t)
	defer cleanup()

	pub, err := NewDatadogPublisher(DatadogConfig{
		Address:   addr,
		Namespace: "",
	})
	if err != nil {
		t.Fatalf("NewDatadogPublisher() error = %v", err)
	}
	defer func() { _ = pub.Close() }()

	if pub.namespace != defaultDatadogNamespace {
		t.Errorf("namespace = %s, want %s", pub.namespace, defaultDatadogNamespace)
	}
}

func TestDatadogPublisher_SampleRate(t *testing.T) {
	addr, cleanup := startUDPServer(t)
	defer cleanup()

	tests := []struct {
		name           string
		sampleRate     float64
		wantSampleRate float64
	}{
		{"default", 0, 1.0},
		{"negative", -0.5, 1.0},
		{"greater than 1", 1.5, 1.0},
		{"valid 0.5", 0.5, 0.5},
		{"valid 0.1", 0.1, 0.1},
		{"valid 1.0", 1.0, 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub, err := NewDatadogPublisher(DatadogConfig{
				Address:    addr,
				SampleRate: tt.sampleRate,
			})
			if err != nil {
				t.Fatalf("NewDatadogPublisher() error = %v", err)
			}
			defer func() { _ = pub.Close() }()

			if pub.sampleRate != tt.wantSampleRate {
				t.Errorf("sampleRate = %v, want %v", pub.sampleRate, tt.wantSampleRate)
			}
		})
	}
}

func TestDatadogPublisher_WithBufferOptions(t *testing.T) {
	addr, cleanup := startUDPServer(t)
	defer cleanup()

	pub, err := NewDatadogPublisher(DatadogConfig{
		Address:               addr,
		BufferPoolSize:        4096,
		WorkersCount:          4,
		MaxMessagesPerPayload: 100,
	})
	if err != nil {
		t.Fatalf("NewDatadogPublisher() error = %v", err)
	}
	defer func() { _ = pub.Close() }()

	if pub.client == nil {
		t.Error("expected non-nil client with buffer options")
	}
}

// startUDPServer starts a UDP server and returns the address and cleanup function.
func startUDPServer(t *testing.T) (string, func()) {
	t.Helper()

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("failed to start UDP server: %v", err)
	}

	// Set a read deadline to prevent blocking forever
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		_ = conn.Close()
		t.Fatalf("failed to set read deadline: %v", err)
	}

	addr := conn.LocalAddr().String()
	return addr, func() { _ = conn.Close() }
}
