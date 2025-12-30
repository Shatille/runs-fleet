package worker

import (
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
)

func TestK8sWorkerDeps_ZeroValues(t *testing.T) {
	// Test that zero-value K8sWorkerDeps struct compiles and has expected defaults
	deps := K8sWorkerDeps{}

	if deps.Queue != nil {
		t.Error("Expected Queue to be nil for zero value")
	}
	if deps.Provider != nil {
		t.Error("Expected Provider to be nil for zero value")
	}
	if deps.PoolProvider != nil {
		t.Error("Expected PoolProvider to be nil for zero value")
	}
	if deps.Metrics != nil {
		t.Error("Expected Metrics to be nil for zero value")
	}
	if deps.Runner != nil {
		t.Error("Expected Runner to be nil for zero value")
	}
	if deps.DB != nil {
		t.Error("Expected DB to be nil for zero value")
	}
	if deps.Config != nil {
		t.Error("Expected Config to be nil for zero value")
	}
}

func TestK8sWorkerDeps_WithMetrics(t *testing.T) {
	// Test that NoopPublisher can be assigned to Metrics field
	deps := K8sWorkerDeps{
		Metrics: metrics.NoopPublisher{},
		Config:  &config.Config{},
	}

	if deps.Metrics == nil {
		t.Error("Expected Metrics to not be nil after assignment")
	}
	if deps.Config == nil {
		t.Error("Expected Config to not be nil after assignment")
	}
}

func TestK8sWorkerDeps_WithQueue(t *testing.T) {
	// Test that MockQueue (which implements queue.Queue interface) can be assigned
	mockQueue := &MockQueue{}

	deps := K8sWorkerDeps{
		Queue: mockQueue,
	}

	if deps.Queue == nil {
		t.Error("Expected Queue to not be nil after assignment")
	}
}

func TestK8sWorkerDeps_AllInterfaceFields(t *testing.T) {
	// Test that interface fields can be assigned
	deps := K8sWorkerDeps{
		Queue:   &MockQueue{},
		Metrics: metrics.NoopPublisher{},
		Config:  &config.Config{},
	}

	// Verify interface fields are set
	if deps.Queue == nil {
		t.Error("Queue should not be nil")
	}
	if deps.Metrics == nil {
		t.Error("Metrics should not be nil")
	}

	// Verify concrete type fields are still nil
	if deps.Provider != nil {
		t.Error("Provider should be nil when not set")
	}
	if deps.PoolProvider != nil {
		t.Error("PoolProvider should be nil when not set")
	}
	if deps.Runner != nil {
		t.Error("Runner should be nil when not set")
	}
	if deps.DB != nil {
		t.Error("DB should be nil when not set")
	}
}
