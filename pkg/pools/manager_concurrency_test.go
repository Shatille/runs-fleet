package pools

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// claimTracker models the DynamoDB ClaimInstanceForJob conditional write: an
// instance can be claimed by at most one job at a time. A claim by the current
// owner is idempotent; any other concurrent claim fails with
// ErrInstanceAlreadyClaimed. It is safe for concurrent use so -race can flag
// races in the manager rather than in the test double.
type claimTracker struct {
	mu     sync.Mutex
	owners map[string]int64 // instanceID -> owning jobID
}

func newClaimTracker() *claimTracker {
	return &claimTracker{owners: make(map[string]int64)}
}

func (c *claimTracker) claim(instanceID string, jobID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if owner, ok := c.owners[instanceID]; ok && owner != jobID {
		return db.ErrInstanceAlreadyClaimed
	}
	c.owners[instanceID] = jobID
	return nil
}

func (c *claimTracker) release(instanceID string, jobID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if owner, ok := c.owners[instanceID]; ok && owner == jobID {
		delete(c.owners, instanceID)
	}
}

// stoppedReservation builds a DescribeInstances reservation of stopped instances.
func stoppedReservation(ids ...string) []ec2types.Reservation {
	insts := make([]ec2types.Instance, 0, len(ids))
	for _, id := range ids {
		insts = append(insts, ec2types.Instance{
			InstanceId:   aws.String(id),
			InstanceType: ec2types.InstanceTypeC7gXlarge,
			State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
		})
	}
	return []ec2types.Reservation{{Instances: insts}}
}

// TestClaimAndStartPoolInstance_RaceNoDoubleClaim runs many goroutines claiming
// from the same pool concurrently. The DynamoDB mock enforces single-ownership.
// We assert every winner started a distinct instance, the number of winners
// never exceeds the number of instances, and every start corresponds to a
// successful claim. Run under -race, this also proves the manager has no data
// race when claims execute outside any global lock.
func TestClaimAndStartPoolInstance_RaceNoDoubleClaim(t *testing.T) {
	t.Parallel()

	const numInstances = 5
	const numGoroutines = 32

	ids := make([]string, numInstances)
	for i := range ids {
		ids[i] = "i-" + string(rune('a'+i))
	}

	claimed := newClaimTracker()
	var startCount int64

	// Track which instance each started job got, to detect double-starts.
	var startMu sync.Mutex
	startsByInstance := make(map[string]int)

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
			return &ec2.DescribeInstancesOutput{Reservations: stoppedReservation(ids...)}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			atomic.AddInt64(&startCount, 1)
			startMu.Lock()
			for _, id := range params.InstanceIds {
				startsByInstance[id]++
			}
			startMu.Unlock()
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	type outcome struct {
		instanceID string
		err        error
	}
	results := make(chan outcome, numGoroutines)
	var wg sync.WaitGroup
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(jobID int64) {
			defer wg.Done()
			inst, err := manager.ClaimAndStartPoolInstance(context.Background(), "race-pool", jobID, "owner/repo", nil)
			if inst != nil {
				results <- outcome{instanceID: inst.InstanceID, err: err}
			} else {
				results <- outcome{err: err}
			}
		}(int64(1000 + i))
	}
	wg.Wait()
	close(results)

	seen := make(map[string]int)
	var success, noInstance int
	for r := range results {
		switch {
		case r.err == nil:
			success++
			if r.instanceID == "" {
				t.Error("successful claim returned empty instance ID")
			}
			seen[r.instanceID]++
		case errors.Is(r.err, ErrNoAvailableInstance):
			noInstance++
		default:
			t.Errorf("unexpected error: %v", r.err)
		}
	}

	// No instance handed to two winners.
	for id, n := range seen {
		if n != 1 {
			t.Errorf("instance %s claimed by %d winners, want 1 (double-claim)", id, n)
		}
	}
	// Winners cannot exceed available instances.
	if success > numInstances {
		t.Errorf("got %d successful claims, want <= %d", success, numInstances)
	}
	if success+noInstance != numGoroutines {
		t.Errorf("accounted %d outcomes, want %d", success+noInstance, numGoroutines)
	}
	// Every winning instance was started exactly once.
	startMu.Lock()
	for id, n := range startsByInstance {
		if n != 1 {
			t.Errorf("instance %s started %d times, want 1", id, n)
		}
	}
	startMu.Unlock()
	if got := atomic.LoadInt64(&startCount); int(got) != success {
		t.Errorf("start count %d != successful claims %d", got, success)
	}
}

// TestClaimAndStartPoolInstance_DeadContextNoAWS verifies that a claim issued
// with an already-cancelled context returns promptly with a retryable error and
// makes no AWS or DynamoDB calls (no DOA cascade).
func TestClaimAndStartPoolInstance_DeadContextNoAWS(t *testing.T) {
	t.Parallel()

	var describeCalls, claimCalls, startCalls int64

	mockDB := &MockDBClient{
		ClaimInstanceForJobFunc: func(_ context.Context, _ string, _ int64, _ time.Duration) error {
			atomic.AddInt64(&claimCalls, 1)
			return nil
		},
	}
	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			atomic.AddInt64(&describeCalls, 1)
			return &ec2.DescribeInstancesOutput{Reservations: stoppedReservation("i-x")}, nil
		},
		StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			atomic.AddInt64(&startCalls, 1)
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // budget already spent

	start := time.Now()
	inst, err := manager.ClaimAndStartPoolInstance(ctx, "test-pool", 12345, "owner/repo", nil)
	elapsed := time.Since(start)

	if inst != nil {
		t.Errorf("expected nil instance, got %v", inst)
	}
	if !errors.Is(err, ErrClaimContextDone) {
		t.Errorf("expected ErrClaimContextDone, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("claim with dead context took %v, expected immediate return", elapsed)
	}
	if n := atomic.LoadInt64(&describeCalls); n != 0 {
		t.Errorf("DescribeInstances called %d times, want 0", n)
	}
	if n := atomic.LoadInt64(&claimCalls); n != 0 {
		t.Errorf("ClaimInstanceForJob called %d times, want 0", n)
	}
	if n := atomic.LoadInt64(&startCalls); n != 0 {
		t.Errorf("StartInstances called %d times, want 0", n)
	}
}

// TestClaimAndStartPoolInstance_DifferentPoolsDoNotSerialize verifies that a
// slow operation on one pool does not block a claim on a different pool. Pool A's
// DescribeInstances blocks until pool B's claim has completed; if the two pools
// shared a lock, this would deadlock and the test would time out.
func TestClaimAndStartPoolInstance_DifferentPoolsDoNotSerialize(t *testing.T) {
	t.Parallel()

	bDone := make(chan struct{})
	aProceed := make(chan struct{})

	claimed := newClaimTracker()

	mockDB := &MockDBClient{
		ClaimInstanceForJobFunc: func(_ context.Context, instanceID string, jobID int64, _ time.Duration) error {
			return claimed.claim(instanceID, jobID)
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, params *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			// Identify the pool from the tag filter.
			pool := poolFromFilters(params)
			if pool == "pool-a" {
				// Pool A must wait until pool B finishes; if pools serialized on a
				// shared lock, B could never run and this would block forever.
				select {
				case <-bDone:
				case <-time.After(5 * time.Second):
					return nil, errors.New("pool A timed out waiting for pool B (pools serialized)")
				}
				return &ec2.DescribeInstancesOutput{Reservations: stoppedReservation("i-a")}, nil
			}
			return &ec2.DescribeInstancesOutput{Reservations: stoppedReservation("i-b")}, nil
		},
		StartInstancesFunc: func(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{}, &config.Config{})
	manager.SetEC2Client(mockEC2)

	var aErr, bErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-aProceed
		_, aErr = manager.ClaimAndStartPoolInstance(context.Background(), "pool-a", 1, "owner/repo", nil)
	}()
	go func() {
		defer wg.Done()
		close(aProceed) // let A start (and block in DescribeInstances)
		_, bErr = manager.ClaimAndStartPoolInstance(context.Background(), "pool-b", 2, "owner/repo", nil)
		close(bDone) // unblock A
	}()
	wg.Wait()

	if aErr != nil {
		t.Errorf("pool A claim failed: %v", aErr)
	}
	if bErr != nil {
		t.Errorf("pool B claim failed: %v", bErr)
	}
}

// poolFromFilters extracts the runs-fleet:pool tag value from a DescribeInstances
// filter set.
func poolFromFilters(params *ec2.DescribeInstancesInput) string {
	for _, f := range params.Filters {
		if aws.ToString(f.Name) == "tag:runs-fleet:pool" && len(f.Values) > 0 {
			return f.Values[0]
		}
	}
	return ""
}
