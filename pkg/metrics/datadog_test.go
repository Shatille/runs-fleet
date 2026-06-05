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

	pub, err := NewDatadogPublisher(DatadogConfig{Address: addr})
	if err != nil {
		t.Fatalf("NewDatadogPublisher() error = %v", err)
	}
	defer func() { _ = pub.Close() }()

	ctx := context.Background()

	tests := []struct {
		name    string
		publish func() error
	}{
		{"PublishJobEnqueued", func() error { return pub.PublishJobEnqueued(ctx, "default", "arm64", "4", "o/r") }},
		{"PublishJobAssigned", func() error { return pub.PublishJobAssigned(ctx, "default", "warm_pool", "o/r") }},
		{"PublishRunnerConfirmed", func() error { return pub.PublishRunnerConfirmed(ctx, "default") }},
		{"PublishJobCompleted", func() error { return pub.PublishJobCompleted(ctx, "default", "success", "o/r") }},
		{"PublishJobRequeued", func() error { return pub.PublishJobRequeued(ctx, "spot_interruption") }},
		{"PublishJobWaitSeconds", func() error { return pub.PublishJobWaitSeconds(ctx, "default", "cold_start", 12) }},
		{"PublishJobExecutionSeconds", func() error { return pub.PublishJobExecutionSeconds(ctx, "default", "success", 90) }},
		{"PublishInstanceProvisionSeconds", func() error { return pub.PublishInstanceProvisionSeconds(ctx, "cold_start", "c7g", 30) }},
		{"PublishFleetCreate", func() error { return pub.PublishFleetCreate(ctx, "1", "success") }},
		{"PublishFleetCreateSeconds", func() error { return pub.PublishFleetCreateSeconds(ctx, "1", 5) }},
		{"PublishInstances", func() error { return pub.PublishInstances(ctx, "running", "4", "default", 3) }},
		{"PublishSpotInterruption", func() error { return pub.PublishSpotInterruption(ctx, "c7g") }},
		{"PublishCircuitBreakerTrip", func() error { return pub.PublishCircuitBreakerTrip(ctx, "c7g.xlarge") }},
		{"PublishCircuitBreakerOpen", func() error { return pub.PublishCircuitBreakerOpen(ctx, "c7g.xlarge", true) }},
		{"PublishPoolInstances", func() error { return pub.PublishPoolInstances(ctx, "default", "ready", 5) }},
		{"PublishPoolDesired", func() error { return pub.PublishPoolDesired(ctx, "default", "running", 2) }},
		{"PublishPoolAction", func() error { return pub.PublishPoolAction(ctx, "default", "create", "ready_deficit") }},
		{"PublishPoolReconcileSeconds", func() error { return pub.PublishPoolReconcileSeconds(ctx, 2) }},
		{"PublishMessageProcessingSeconds", func() error { return pub.PublishMessageProcessingSeconds(ctx, "main", "success", 0.2) }},
		{"PublishLockWaitSeconds", func() error { return pub.PublishLockWaitSeconds(ctx, "pool_reconcile", 0.05) }},
		{"PublishWorkerInflight", func() error { return pub.PublishWorkerInflight(ctx, "main", 4) }},
		{"PublishQueueDepth", func() error { return pub.PublishQueueDepth(ctx, "main", 10.0) }},
		{"PublishQueueReceive", func() error { return pub.PublishQueueReceive(ctx, "main", "messages") }},
		{"PublishAWSCallDuration", func() error { return pub.PublishAWSCallDuration(ctx, "SQS", "ReceiveMessage", 1.5) }},
		{"PublishAWSCallFailure", func() error { return pub.PublishAWSCallFailure(ctx, "SQS", "ReceiveMessage", "timeout") }},
		{"PublishCacheRequest", func() error { return pub.PublishCacheRequest(ctx, "hit") }},
		{"PublishCacheOperation", func() error { return pub.PublishCacheOperation(ctx, "commit") }},
		{"PublishCacheBytesStored", func() error { return pub.PublishCacheBytesStored(ctx, 1024) }},
		{"PublishCacheError", func() error { return pub.PublishCacheError(ctx, "commit") }},
		{"PublishCacheAuthRejected", func() error { return pub.PublishCacheAuthRejected(ctx, "invalid") }},
		{"PublishHousekeepingAction", func() error { return pub.PublishHousekeepingAction(ctx, "ssm_params", 3) }},
		{"PublishSchedulingFailure", func() error { return pub.PublishSchedulingFailure(ctx, "job_claim") }},
		{"PublishMessageDeletionFailure", func() error { return pub.PublishMessageDeletionFailure(ctx, "events") }},
		{"PublishInstanceHours", func() error { return pub.PublishInstanceHours(ctx, "4", "c7g", 2) }},
		{"PublishEstimatedCost", func() error { return pub.PublishEstimatedCost(ctx, 12.5) }},
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

// TestDatadogPublisher_FixedNamespace asserts the namespace is fixed at
// datadogNamespace and is not influenced by DatadogConfig (the override knob is
// gone), guarding against metric-name collisions across deployments.
func TestDatadogPublisher_FixedNamespace(t *testing.T) {
	addr, cleanup := startUDPServer(t)
	defer cleanup()

	pub, err := NewDatadogPublisher(DatadogConfig{Address: addr})
	if err != nil {
		t.Fatalf("NewDatadogPublisher() error = %v", err)
	}
	defer func() { _ = pub.Close() }()

	if pub.namespace != datadogNamespace {
		t.Errorf("namespace = %s, want %s", pub.namespace, datadogNamespace)
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
