package pools

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func hasLogAtLevel(t *testing.T, buf *bytes.Buffer, level, msg string) bool {
	t.Helper()
	for _, rec := range decodeLogRecords(t, buf) {
		if rec["msg"] == msg && rec["level"] == level {
			return true
		}
	}
	return false
}

// TestCreatePoolFleetInstances_PassesReason proves the deficit-driven create
// path stamps each LaunchSpec with the caller-supplied reason.
func TestCreatePoolFleetInstances_PassesReason(t *testing.T) {
	t.Parallel()

	var reasons []string
	mockFleet := &MockFleetAPI{
		CreateOnDemandInstanceFunc: func(_ context.Context, spec *fleet.LaunchSpec) (string, error) {
			reasons = append(reasons, spec.Reason)
			return testInstanceNewID, nil
		},
	}
	manager := NewManager(&MockDBClient{}, mockFleet, &config.Config{SubnetIDs: []string{"subnet-1"}})
	manager.SetEC2Client(&MockEC2API{})

	poolConfig := &db.PoolConfig{PoolName: "test-pool", InstanceType: "t4g.medium", DesiredRunning: 2}
	manager.createPoolFleetInstances(context.Background(), "test-pool", "stopped_replenish", 2, poolConfig)

	if len(reasons) != 2 {
		t.Fatalf("expected 2 creates, got %d", len(reasons))
	}
	for _, r := range reasons {
		if r != "stopped_replenish" {
			t.Errorf("LaunchSpec.Reason = %q, want stopped_replenish", r)
		}
	}
}

// TestReconcileStopLogCarriesReason proves the warm-pool stop path emits an
// "instances stopped" log tagged with reason excess_ready.
func TestReconcileStopLogCarriesReason(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) { return []string{"warm-pool"}, nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{DesiredRunning: 0, DesiredStopped: 1, IdleTimeoutMinutes: 30, InstanceType: "t3.medium"}, nil
		},
		UpdatePoolStateFunc:        func(_ context.Context, _ string, _, _ int) error { return nil },
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) { return []string{}, nil },
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{Instances: []ec2types.Instance{{
						InstanceId:   aws.String("i-running1"),
						InstanceType: ec2types.InstanceTypeT3Medium,
						State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
						LaunchTime:   aws.Time(time.Now().Add(-5 * time.Minute)),
					}}},
				},
			}, nil
		},
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{SubnetIDs: []string{"subnet-1"}})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	rec := findLog(t, &buf, "instances stopped")
	if got := rec["reason"]; got != "excess_ready" {
		t.Errorf("instances stopped reason = %v, want excess_ready", got)
	}
}

// TestReconcileTerminateLogCarriesReason proves the excess-stopped terminate
// path emits an "instances terminated" log tagged with reason
// over_desired_stopped.
func TestReconcileTerminateLogCarriesReason(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) { return []string{"test-pool"}, nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{DesiredRunning: 1, DesiredStopped: 1, InstanceType: "t3.medium"}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error { return nil },
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{Instances: []ec2types.Instance{
						{InstanceId: aws.String("i-running1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
						{InstanceId: aws.String("i-stopped1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
						{InstanceId: aws.String("i-stopped2"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
					}},
				},
			}, nil
		},
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	rec := findLog(t, &buf, "instances terminated")
	if got := rec["reason"]; got != "over_desired_stopped" {
		t.Errorf("instances terminated reason = %v, want over_desired_stopped", got)
	}
}

// TestReconcileBailsCleanlyOnCancelledContext proves that when the context is
// already cancelled, a reconcile pass emits NO error-level "pool reconciliation
// failed" line: the loop bails before iterating every pool-table row.
func TestReconcileBailsCleanlyOnCancelledContext(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"pool-a", "pool-b", "pool-c"}, nil
		},
		AcquirePoolReconcileLockFunc: func(_ context.Context, _, _ string, _ time.Duration) error {
			return fmt.Errorf("acquire lock: %w", context.Canceled)
		},
		ReleasePoolReconcileLockFunc: func(_ context.Context, _, _ string) error { return nil },
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(&MockEC2API{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	manager.reconcile(ctx)

	if hasLogAtLevel(t, &buf, "ERROR", "pool reconciliation failed") {
		t.Errorf("reconcile must not emit ERROR-level 'pool reconciliation failed' on shutdown; got: %s", buf.String())
	}
}

// TestReconcilePoolDowngradesShutdownError proves that a reconcilePool failure
// caused by context cancellation is logged at Debug, not Error.
func TestReconcilePoolDowngradesShutdownError(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) { return []string{"pool-a"}, nil },
		AcquirePoolReconcileLockFunc: func(_ context.Context, _, _ string, _ time.Duration) error {
			return fmt.Errorf("acquire lock: %w", context.DeadlineExceeded)
		},
		ReleasePoolReconcileLockFunc: func(_ context.Context, _, _ string) error { return nil },
	}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(&MockEC2API{})

	manager.reconcile(context.Background())

	if hasLogAtLevel(t, &buf, "ERROR", "pool reconciliation failed") {
		t.Errorf("shutdown error must not be logged at ERROR; got: %s", buf.String())
	}
	if !hasLogAtLevel(t, &buf, "DEBUG", "reconciliation aborted: shutting down") {
		t.Errorf("expected DEBUG 'reconciliation aborted: shutting down'; got: %s", buf.String())
	}
}
