package pools

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// assignedReservation builds a reservation whose named instances carry the
// assigned tag, so tests can exercise the durable exclusion end to end.
func assignedReservation(running, stopped []string, assigned map[string]bool) []ec2types.Reservation {
	insts := make([]ec2types.Instance, 0, len(running)+len(stopped))
	add := func(id string, state ec2types.InstanceStateName) {
		tags := []ec2types.Tag{{Key: aws.String("runs-fleet:managed"), Value: aws.String("true")}}
		if assigned[id] {
			tags = append(tags, ec2types.Tag{Key: aws.String(tagInstanceAssigned), Value: aws.String("true")})
		}
		insts = append(insts, ec2types.Instance{
			InstanceId:   aws.String(id),
			InstanceType: ec2types.InstanceTypeC7gXlarge,
			State:        &ec2types.InstanceState{Name: state},
			LaunchTime:   aws.Time(time.Now().Add(-time.Hour)),
			Tags:         tags,
		})
	}
	for _, id := range running {
		add(id, ec2types.InstanceStateNameRunning)
	}
	for _, id := range stopped {
		add(id, ec2types.InstanceStateNameStopped)
	}
	return []ec2types.Reservation{{Instances: insts}}
}

// getPoolInstances must surface the assigned tag so every downstream guard can
// see that an instance has already had a runner config written for it.
func TestGetPoolInstances_ReadsAssignedTag(t *testing.T) {
	t.Parallel()

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: assignedReservation([]string{"i-tagged", "i-clean"}, nil, map[string]bool{"i-tagged": true}),
			}, nil
		},
	}
	m := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	m.SetEC2Client(mockEC2)

	instances, err := m.getPoolInstances(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("getPoolInstances() error = %v", err)
	}
	byID := map[string]PoolInstance{}
	for _, inst := range instances {
		byID[inst.InstanceID] = inst
	}
	if !byID["i-tagged"].Assigned {
		t.Error("i-tagged must be marked Assigned")
	}
	if byID["i-clean"].Assigned {
		t.Error("i-clean must not be marked Assigned")
	}
}

// A previously-assigned running instance still hosts the agent that consumed
// that assignment's config; the agent never re-reads config, so re-claiming it
// would register a runner for the wrong job. It must not even be offered as a
// candidate (no claim taken, no secrets probe).
func TestClaim_AssignedRunningSpareNotOffered(t *testing.T) {
	t.Parallel()

	var startCalls int64
	var claimedIDs []string
	checker := &mockConfigChecker{}

	mockDB := &MockDBClient{
		GetPoolConfigFunc:          func(context.Context, string) (*db.PoolConfig, error) { return hotPoolCfg(), nil },
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) { return nil, nil },
		ClaimInstanceForJobFunc: func(_ context.Context, instanceID string, _ int64, _ time.Duration) error {
			claimedIDs = append(claimedIDs, instanceID)
			return nil
		},
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: assignedReservation([]string{iRunID}, []string{iStopID}, map[string]bool{iRunID: true}),
			}, nil
		},
		StartInstancesFunc: func(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			atomic.AddInt64(&startCalls, 1)
			return &ec2.StartInstancesOutput{}, nil
		},
		CreateTagsFunc: func(context.Context, *ec2.CreateTagsInput, ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			return &ec2.CreateTagsOutput{}, nil
		},
	}

	m := NewManager(mockDB, &MockFleetAPI{}, hotGateConfig())
	m.SetEC2Client(mockEC2)
	m.SetRunnerConfigChecker(checker)

	inst, err := m.ClaimAndStartPoolInstance(context.Background(), "hot", 1, "owner/repo", nil)
	if err != nil {
		t.Fatalf("claim err = %v", err)
	}
	if inst.InstanceID != iStopID {
		t.Errorf("claimed %s, want %s (assigned running spare must be skipped)", inst.InstanceID, iStopID)
	}
	if slices.Contains(claimedIDs, iRunID) {
		t.Errorf("an assigned spare must never be claimed, claims=%v", claimedIDs)
	}
	if checker.calls != 0 {
		t.Errorf("an assigned spare must be filtered before any config probe, got %d probes", checker.calls)
	}
	if atomic.LoadInt64(&startCalls) != 1 {
		t.Errorf("expected the stopped fallback to be started, got %d starts", startCalls)
	}
}

// A STOPPED instance that was assigned before is safe to reuse: it reboots and
// reads whatever config the new assignment writes, so the tag must not exclude
// it (that would leak the whole stopped reserve after one use each).
func TestClaim_AssignedStoppedInstanceStillClaimable(t *testing.T) {
	t.Parallel()

	mockDB := &MockDBClient{
		GetPoolConfigFunc:          func(context.Context, string) (*db.PoolConfig, error) { return hotPoolCfg(), nil },
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) { return nil, nil },
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: assignedReservation(nil, []string{iStopID}, map[string]bool{iStopID: true}),
			}, nil
		},
		StartInstancesFunc: func(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			return &ec2.StartInstancesOutput{}, nil
		},
		CreateTagsFunc: func(context.Context, *ec2.CreateTagsInput, ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			return &ec2.CreateTagsOutput{}, nil
		},
	}

	m := NewManager(mockDB, &MockFleetAPI{}, hotGateConfig())
	m.SetEC2Client(mockEC2)

	inst, err := m.ClaimAndStartPoolInstance(context.Background(), "hot", 1, "owner/repo", nil)
	if err != nil {
		t.Fatalf("claim err = %v", err)
	}
	if inst.InstanceID != iStopID {
		t.Errorf("claimed %s, want %s (a stopped instance stays reusable)", inst.InstanceID, iStopID)
	}
}

// Assignment stamps the tag so the exclusion survives a replica restart and is
// visible to every replica, unlike the in-memory idle tracking it complements.
func TestMarkInstanceAssigned_TagsInstance(t *testing.T) {
	t.Parallel()

	var tagged []ec2types.Tag
	mockEC2 := &MockEC2API{
		CreateTagsFunc: func(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			tagged = append(tagged, in.Tags...)
			return &ec2.CreateTagsOutput{}, nil
		},
	}
	m := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	m.SetEC2Client(mockEC2)

	m.markInstanceAssigned(context.Background(), "i-1", "owner/repo")

	var hasAssigned, hasRole bool
	for _, tg := range tagged {
		switch aws.ToString(tg.Key) {
		case tagInstanceAssigned:
			hasAssigned = aws.ToString(tg.Value) == "true"
		case "Role":
			hasRole = true
		}
	}
	if !hasAssigned {
		t.Errorf("assignment must stamp %s=true, got %v", tagInstanceAssigned, tagged)
	}
	if !hasRole {
		t.Errorf("the Role tag must still be applied, got %v", tagged)
	}
}

// The assigned tag must be written even when the repo is unknown, since the
// exclusion it drives is what keeps the instance out of later claims.
func TestMarkInstanceAssigned_TagsWithoutRepo(t *testing.T) {
	t.Parallel()

	var tagged []ec2types.Tag
	mockEC2 := &MockEC2API{
		CreateTagsFunc: func(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			tagged = append(tagged, in.Tags...)
			return &ec2.CreateTagsOutput{}, nil
		},
	}
	m := NewManager(&MockDBClient{}, &MockFleetAPI{}, &config.Config{})
	m.SetEC2Client(mockEC2)

	m.markInstanceAssigned(context.Background(), "i-1", "")

	if len(tagged) != 1 || aws.ToString(tagged[0].Key) != tagInstanceAssigned {
		t.Errorf("expected only the assigned tag when repo is empty, got %v", tagged)
	}
}

// An assigned-but-not-busy running instance can never serve a pool claim, so it
// must not be counted as ready capacity. Counting it lets one such instance
// satisfy desiredRunning forever, silently degrading every job in the pool to a
// stopped-instance start for the life of the episode.
func TestReconcile_AssignedSpareDoesNotSatisfyDesiredRunning(t *testing.T) {
	t.Parallel()

	var started, created int64
	mockDB := &MockDBClient{
		ListPoolsFunc: func(context.Context) ([]string, error) { return []string{"test-pool"}, nil },
		GetPoolConfigFunc: func(context.Context, string) (*db.PoolConfig, error) {
			return &db.PoolConfig{DesiredRunning: 1, DesiredStopped: 1, InstanceType: "c7g.xlarge"}, nil
		},
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) { return nil, nil },
	}
	mockFleet := &MockFleetAPI{
		CreateOnDemandInstanceFunc: func(context.Context, *fleet.LaunchSpec) (string, error) {
			atomic.AddInt64(&created, 1)
			return "i-new", nil
		},
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// The only running instance is a previously-assigned one; the pool has
			// no usable ready spare even though `running` is 1.
			return &ec2.DescribeInstancesOutput{
				Reservations: assignedReservation([]string{"i-assigned"}, []string{"i-stopped"}, map[string]bool{"i-assigned": true}),
			}, nil
		},
		StartInstancesFunc: func(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			atomic.AddInt64(&started, 1)
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	m := NewManager(mockDB, mockFleet, &config.Config{SubnetIDs: []string{"subnet-1"}})
	m.SetEC2Client(mockEC2)

	m.reconcile(context.Background())

	if atomic.LoadInt64(&started)+atomic.LoadInt64(&created) == 0 {
		t.Error("expected the reconciler to provision a usable spare to replace the assigned one")
	}
}

// An assigned instance must never be a scale-down candidate: its agent is
// serving GitHub as surplus and terminates itself when done.
func TestReconcile_AssignedSpareNotScaledDown(t *testing.T) {
	t.Parallel()

	manager, capture := newHotPoolManager(t, nil, nil)
	manager.readyDwellPeriod = 0
	// Rebuild the EC2 mock so the fixture carries the assigned tag.
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: assignedReservation([]string{"i-assigned", "i-idle1", "i-idle2"}, nil,
					map[string]bool{"i-assigned": true}),
			}, nil
		},
		StopInstancesFunc: func(_ context.Context, params *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			capture.stopped = append(capture.stopped, params.InstanceIds...)
			return &ec2.StopInstancesOutput{}, nil
		},
		TerminateInstancesFunc: func(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			capture.terminated = append(capture.terminated, params.InstanceIds...)
			return &ec2.TerminateInstancesOutput{}, nil
		},
	}
	manager.SetEC2Client(mockEC2)
	markIdle(manager, "i-assigned", "i-idle1", "i-idle2")

	manager.reconcile(context.Background())

	if slices.Contains(capture.all(), "i-assigned") {
		t.Errorf("an assigned instance must not be scaled down, got stopped=%v terminated=%v",
			capture.stopped, capture.terminated)
	}
}

// The stuck-assigned condition must be visible on a dashboard, not only in a log
// line: the production incident this guards against went unnoticed for days
// because the only evidence was buried in log volume.
func TestReconcile_PublishesAssignedIdleGauge(t *testing.T) {
	t.Parallel()

	mockDB := &MockDBClient{
		ListPoolsFunc: func(context.Context) ([]string, error) { return []string{"test-pool"}, nil },
		GetPoolConfigFunc: func(context.Context, string) (*db.PoolConfig, error) {
			return &db.PoolConfig{DesiredRunning: 1, InstanceType: "c7g.xlarge"}, nil
		},
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) { return nil, nil },
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: assignedReservation([]string{"i-assigned", "i-clean"}, nil, map[string]bool{"i-assigned": true}),
			}, nil
		},
		StartInstancesFunc: func(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			return &ec2.StartInstancesOutput{}, nil
		},
	}
	metrics := &mockMetrics{}

	m := NewManager(mockDB, &MockFleetAPI{}, &config.Config{SubnetIDs: []string{"subnet-1"}})
	m.SetEC2Client(mockEC2)
	m.SetMetrics(metrics)

	m.reconcile(context.Background())

	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	var found bool
	for _, c := range metrics.poolInstances {
		if c.state == "assigned_idle" {
			found = true
			if c.n != 1 {
				t.Errorf("assigned_idle gauge = %d, want 1", c.n)
			}
		}
	}
	if !found {
		t.Errorf("expected an assigned_idle gauge, got %+v", metrics.poolInstances)
	}
}
