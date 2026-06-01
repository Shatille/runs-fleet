package fleet

import (
	"context"
	"errors"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type mockFleetMetrics struct {
	createCalls  []capacityResult
	secondsCalls []capacitySeconds
}

type capacityResult struct {
	capacity string
	result   string
}

type capacitySeconds struct {
	capacity string
	seconds  float64
}

func (m *mockFleetMetrics) PublishFleetCreate(_ context.Context, capacity, result string) error {
	m.createCalls = append(m.createCalls, capacityResult{capacity: capacity, result: result})
	return nil
}

func (m *mockFleetMetrics) PublishFleetCreateSeconds(_ context.Context, capacity string, seconds float64) error {
	m.secondsCalls = append(m.secondsCalls, capacitySeconds{capacity: capacity, seconds: seconds})
	return nil
}

// TestCreateFleet_EmitsCreateSecondsSpot proves a spot CreateFleet emits the
// latency histogram and the counter with the spot capacity label.
func TestCreateFleet_EmitsCreateSecondsSpot(t *testing.T) {
	mock := &mockEC2Client{
		CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
			return &ec2.CreateFleetOutput{Instances: []types.CreateFleetInstance{{InstanceIds: []string{"i-1"}}}}, nil
		},
	}
	m := &mockFleetMetrics{}
	manager := &Manager{ec2Client: mock, config: &config.Config{SpotEnabled: true}, metrics: m}

	if _, err := manager.CreateFleet(context.Background(), &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", Spot: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(m.secondsCalls) != 1 {
		t.Fatalf("fleet_create_seconds emissions = %d, want 1", len(m.secondsCalls))
	}
	if m.secondsCalls[0].capacity != capacitySpot {
		t.Errorf("seconds capacity = %q, want spot", m.secondsCalls[0].capacity)
	}
	if m.secondsCalls[0].seconds < 0 {
		t.Errorf("seconds = %v, want >= 0", m.secondsCalls[0].seconds)
	}
	if len(m.createCalls) != 1 || m.createCalls[0].capacity != capacitySpot || m.createCalls[0].result != "success" {
		t.Errorf("fleet_create counter = %+v, want {spot success}", m.createCalls)
	}
}

// TestCreateFleet_EmitsCreateSecondsOnDemand proves a forced on-demand
// CreateFleet labels both metrics with on_demand.
func TestCreateFleet_EmitsCreateSecondsOnDemand(t *testing.T) {
	mock := &mockEC2Client{
		CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
			return &ec2.CreateFleetOutput{Instances: []types.CreateFleetInstance{{InstanceIds: []string{"i-1"}}}}, nil
		},
	}
	m := &mockFleetMetrics{}
	manager := &Manager{ec2Client: mock, config: &config.Config{SpotEnabled: true}, metrics: m}

	// ForceOnDemand makes shouldUseSpot return false even though Spot is set.
	if _, err := manager.CreateFleet(context.Background(), &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", Spot: true, ForceOnDemand: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(m.secondsCalls) != 1 || m.secondsCalls[0].capacity != capacityOnDemand {
		t.Errorf("fleet_create_seconds = %+v, want one on_demand", m.secondsCalls)
	}
	if len(m.createCalls) != 1 || m.createCalls[0].capacity != capacityOnDemand {
		t.Errorf("fleet_create counter capacity = %+v, want on_demand", m.createCalls)
	}
}

// TestCreateFleet_EmitsCreateSecondsOnError proves the latency histogram is
// emitted even when the AWS call fails, and the counter records failure.
func TestCreateFleet_EmitsCreateSecondsOnError(t *testing.T) {
	mock := &mockEC2Client{
		CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
			return nil, errors.New("api error")
		},
	}
	m := &mockFleetMetrics{}
	manager := &Manager{ec2Client: mock, config: &config.Config{SpotEnabled: true}, metrics: m}

	if _, err := manager.CreateFleet(context.Background(), &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", Spot: true}); err == nil {
		t.Fatal("expected error from CreateFleet")
	}

	if len(m.secondsCalls) != 1 {
		t.Errorf("fleet_create_seconds emissions = %d, want 1 (emitted even on error)", len(m.secondsCalls))
	}
	if len(m.createCalls) != 1 || m.createCalls[0].result != "failure" {
		t.Errorf("fleet_create counter = %+v, want failure", m.createCalls)
	}
}
