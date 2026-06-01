package pools

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// mockMetrics records the pool latency/gauge emissions for assertions. It is
// safe for concurrent use so claim tests can run under -race.
type mockMetrics struct {
	mu sync.Mutex

	lockWaits        []lockWaitCall
	reconcileSeconds []float64
	instances        []instancesCall
}

type lockWaitCall struct {
	lock    string
	seconds float64
}

type instancesCall struct {
	state    string
	capacity string
	pool     string
	n        int
}

func (m *mockMetrics) PublishPoolAction(_ context.Context, _, _, _ string) error      { return nil }
func (m *mockMetrics) PublishPoolDesired(_ context.Context, _, _ string, _ int) error { return nil }
func (m *mockMetrics) PublishPoolInstances(_ context.Context, _, _ string, _ int) error {
	return nil
}

func (m *mockMetrics) PublishPoolReconcileSeconds(_ context.Context, seconds float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconcileSeconds = append(m.reconcileSeconds, seconds)
	return nil
}

func (m *mockMetrics) PublishLockWaitSeconds(_ context.Context, lock string, seconds float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lockWaits = append(m.lockWaits, lockWaitCall{lock: lock, seconds: seconds})
	return nil
}

func (m *mockMetrics) PublishInstances(_ context.Context, state, capacity, pool string, n int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances = append(m.instances, instancesCall{state: state, capacity: capacity, pool: pool, n: n})
	return nil
}

func (m *mockMetrics) lockWaitCount(lock string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, w := range m.lockWaits {
		if w.lock == lock {
			n++
		}
	}
	return n
}

// TestReconcilePoolEmitsLockWaitAndReconcileSeconds proves a successful pool
// reconcile pass emits the pool_reconcile lock-wait histogram and the reconcile
// latency histogram.
func TestReconcilePoolEmitsLockWaitAndReconcileSeconds(t *testing.T) {
	mockDB := &MockDBClient{
		ListPoolsFunc:                func(_ context.Context) ([]string, error) { return []string{"p"}, nil },
		AcquirePoolReconcileLockFunc: func(_ context.Context, _, _ string, _ time.Duration) error { return nil },
		ReleasePoolReconcileLockFunc: func(_ context.Context, _, _ string) error { return nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{DesiredRunning: 1, DesiredStopped: 1, InstanceType: "t3.medium"}, nil
		},
		UpdatePoolStateFunc:        func(_ context.Context, _ string, _, _ int) error { return nil },
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) { return nil, nil },
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{Instances: []ec2types.Instance{
						{InstanceId: aws.String("i-running1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}, LaunchTime: aws.Time(time.Now())},
						{InstanceId: aws.String("i-stopped1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
					}},
				},
			}, nil
		},
	}

	m := &mockMetrics{}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)
	manager.SetMetrics(m)

	manager.reconcile(context.Background())

	if got := m.lockWaitCount(lockPoolReconcile); got != 1 {
		t.Errorf("pool_reconcile lock-wait emissions = %d, want 1", got)
	}
	if len(m.reconcileSeconds) != 1 {
		t.Fatalf("pool reconcile seconds emissions = %d, want 1", len(m.reconcileSeconds))
	}
	if m.reconcileSeconds[0] < 0 {
		t.Errorf("reconcile seconds = %v, want >= 0", m.reconcileSeconds[0])
	}
}

// TestReconcilePoolEmitsInstancesGaugeWithRealCounts proves the global instances
// gauge is fed from the authoritative running/stopped counts (not per-event
// 0/1 deltas) with the on_demand capacity label.
func TestReconcilePoolEmitsInstancesGaugeWithRealCounts(t *testing.T) {
	mockDB := &MockDBClient{
		ListPoolsFunc:                func(_ context.Context) ([]string, error) { return []string{"p"}, nil },
		AcquirePoolReconcileLockFunc: func(_ context.Context, _, _ string, _ time.Duration) error { return nil },
		ReleasePoolReconcileLockFunc: func(_ context.Context, _, _ string) error { return nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{DesiredRunning: 2, DesiredStopped: 3, InstanceType: "t3.medium"}, nil
		},
		UpdatePoolStateFunc:        func(_ context.Context, _ string, _, _ int) error { return nil },
		GetPoolBusyInstanceIDsFunc: func(_ context.Context, _ string) ([]string, error) { return nil, nil },
	}
	// 2 running, 3 stopped.
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{Instances: []ec2types.Instance{
						{InstanceId: aws.String("i-r1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}, LaunchTime: aws.Time(time.Now())},
						{InstanceId: aws.String("i-r2"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}, LaunchTime: aws.Time(time.Now())},
						{InstanceId: aws.String("i-s1"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
						{InstanceId: aws.String("i-s2"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
						{InstanceId: aws.String("i-s3"), InstanceType: ec2types.InstanceTypeT3Medium, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
					}},
				},
			}, nil
		},
	}

	m := &mockMetrics{}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)
	manager.SetMetrics(m)

	manager.reconcile(context.Background())

	var running, stopped *instancesCall
	for i := range m.instances {
		c := &m.instances[i]
		if c.pool != "p" || c.capacity != capacityOnDemand {
			t.Errorf("instances gauge has unexpected labels: %+v", *c)
		}
		switch c.state {
		case stateRunning:
			running = c
		case stateStopped:
			stopped = c
		}
	}
	if running == nil || running.n != 2 {
		t.Errorf("instances running = %v, want n=2", running)
	}
	if stopped == nil || stopped.n != 3 {
		t.Errorf("instances stopped = %v, want n=3", stopped)
	}
}

// TestClaimAndStartPoolInstanceEmitsClaimLockWait proves the claim path emits a
// single claim lock-wait measurement when an instance is successfully claimed.
func TestClaimAndStartPoolInstanceEmitsClaimLockWait(t *testing.T) {
	claimed := newClaimTracker()
	mockDB := &MockDBClient{
		ClaimInstanceForJobFunc: func(_ context.Context, instanceID string, jobID int64, _ time.Duration) error {
			return claimed.claim(instanceID, jobID)
		},
		ReleaseInstanceClaimFunc: func(_ context.Context, instanceID string, jobID int64) error {
			claimed.release(instanceID, jobID)
			return nil
		},
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: stoppedReservation("i-a")}, nil
		},
		StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	m := &mockMetrics{}
	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)
	manager.SetMetrics(m)

	inst, err := manager.ClaimAndStartPoolInstance(context.Background(), "p", 1, "owner/repo", nil)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	if inst == nil {
		t.Fatal("expected an instance to be claimed")
	}
	if got := m.lockWaitCount(lockClaim); got != 1 {
		t.Errorf("claim lock-wait emissions = %d, want 1", got)
	}
}

// TestClaimAndStartPoolInstanceEmitsClaimLockWaitOnExhaustion proves the claim
// lock-wait is emitted even when no instance is available (the wait is still a
// contention signal).
func TestClaimAndStartPoolInstanceEmitsClaimLockWaitOnExhaustion(t *testing.T) {
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{}, nil
		},
	}
	m := &mockMetrics{}
	manager := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)
	manager.SetMetrics(m)

	_, err := manager.ClaimAndStartPoolInstance(context.Background(), "p", 1, "owner/repo", nil)
	if !errors.Is(err, ErrNoAvailableInstance) {
		t.Fatalf("err = %v, want ErrNoAvailableInstance", err)
	}
	if got := m.lockWaitCount(lockClaim); got != 1 {
		t.Errorf("claim lock-wait emissions = %d, want 1", got)
	}
}
