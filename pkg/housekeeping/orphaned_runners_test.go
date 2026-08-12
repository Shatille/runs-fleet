package housekeeping

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
)

type stubRunnerRegistry struct {
	byRepo  map[string][]RegisteredRunner
	deleted []int64
	listErr error
	delErr  error
}

func (s *stubRunnerRegistry) ListRunners(_ context.Context, repo string) ([]RegisteredRunner, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.byRepo[repo], nil
}

func (s *stubRunnerRegistry) DeleteRunner(_ context.Context, _ string, runnerID int64) error {
	if s.delErr != nil {
		return s.delErr
	}
	s.deleted = append(s.deleted, runnerID)
	return nil
}

// fakeSightings models the durable first-seen-offline store. firstSeen is keyed
// per repo+runner, exactly as the DynamoDB rows are.
type fakeSightings struct {
	firstSeen map[string]time.Time
	recordErr error
}

func newFakeSightings() *fakeSightings {
	return &fakeSightings{firstSeen: map[string]time.Time{}}
}

func (f *fakeSightings) key(repo string, id int64) string {
	return repo + ":" + strconv.FormatInt(id, 10)
}

func (f *fakeSightings) RecordRunnerOffline(_ context.Context, repo string, id int64, now time.Time) (time.Duration, error) {
	if f.recordErr != nil {
		return 0, f.recordErr
	}
	k := f.key(repo, id)
	if _, ok := f.firstSeen[k]; !ok {
		f.firstSeen[k] = now
	}
	return now.Sub(f.firstSeen[k]), nil
}

func (f *fakeSightings) ForgetRunnerOffline(_ context.Context, repo string, id int64) error {
	delete(f.firstSeen, f.key(repo, id))
	return nil
}

// backdate makes every recorded sighting old enough to be eligible, so a test
// can assert on which runners the sweep selects rather than on the age gate.
func (f *fakeSightings) backdate() {
	for k := range f.firstSeen {
		f.firstSeen[k] = time.Now().Add(-30 * 24 * time.Hour)
	}
}

func runnersTask(reg RunnerRegistry, repos []string) (*Tasks, *fakeSightings) {
	s := newFakeSightings()
	return &Tasks{
		runnerRegistry: reg,
		activeRepos:    func(context.Context) ([]string, error) { return repos, nil },
		sightings:      s,
		config:         &config.Config{MaxRuntimeMinutes: 360},
	}, s
}

// sweepPastWindow runs the sweep once to stamp the first sighting, ages those
// stamps past the window, then sweeps again — so a test asserts on selection
// rather than restating the age gate.
func sweepPastWindow(t *testing.T, tk *Tasks, s *fakeSightings) {
	t.Helper()
	if err := tk.ExecuteOrphanedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteOrphanedRunners() error = %v", err)
	}
	s.backdate()
	if err := tk.ExecuteOrphanedRunners(context.Background()); err != nil {
		t.Fatalf("ExecuteOrphanedRunners() error = %v", err)
	}
}

// The window must be derived from the configured runtime ceiling, not fixed:
// the validator permits up to 24h, and a deploy that raises the ceiling for a
// slower job class would otherwise leave the threshold below it — the same
// hazard deadAssignmentAge is derived to avoid.
func TestOfflineWindow_ClearsTheConfiguredRuntimeCeiling(t *testing.T) {
	tests := []struct {
		name           string
		runtimeMinutes int
	}{
		{name: "default ceiling", runtimeMinutes: 360},
		{name: "raised ceiling for a slow job class", runtimeMinutes: 1440},
		{name: "ceiling below the standby allowance", runtimeMinutes: 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tk := &Tasks{config: &config.Config{MaxRuntimeMinutes: tt.runtimeMinutes}}
			got := tk.minOfflineAge()

			ceiling := time.Duration(tt.runtimeMinutes) * time.Minute
			if got <= ceiling {
				t.Errorf("minOfflineAge %v does not clear the runtime ceiling %v", got, ceiling)
			}
			if got <= agentStandbyAllowance {
				t.Errorf("minOfflineAge %v does not clear the standby allowance %v", got, agentStandbyAllowance)
			}
		})
	}
}

// A registration exists from the moment its JIT config is minted, before the
// instance has booted, so a runner that is alive and about to take a job reads
// offline for its whole startup. Only a registration offline for longer than
// any boot or standby path could explain is genuinely dead.
func TestExecuteOrphanedRunners_LeavesRecentlyOfflineRunners(t *testing.T) {
	reg := &stubRunnerRegistry{byRepo: map[string][]RegisteredRunner{
		"octo/repo": {{ID: 1, Name: "runs-fleet-runner-fresh", Status: "offline"}},
	}}
	tk, _ := runnersTask(reg, []string{"octo/repo"})

	for range 5 {
		if err := tk.ExecuteOrphanedRunners(context.Background()); err != nil {
			t.Fatalf("sweep error = %v", err)
		}
	}
	if len(reg.deleted) != 0 {
		t.Errorf("deleted %v, want none (still inside the boot/standby window)", reg.deleted)
	}
}

// Coming back online proves the runner was alive, so its sighting is dropped and
// a later offline reading must serve the full window on its own.
func TestExecuteOrphanedRunners_OnlineClearsTheSighting(t *testing.T) {
	reg := &stubRunnerRegistry{byRepo: map[string][]RegisteredRunner{
		"octo/repo": {{ID: 2, Name: "runs-fleet-runner-flap", Status: "offline"}},
	}}
	tk, sight := runnersTask(reg, []string{"octo/repo"})

	if err := tk.ExecuteOrphanedRunners(context.Background()); err != nil {
		t.Fatalf("sweep 1 error = %v", err)
	}
	sight.backdate()

	reg.byRepo["octo/repo"] = []RegisteredRunner{{ID: 2, Name: "runs-fleet-runner-flap", Status: "online"}}
	if err := tk.ExecuteOrphanedRunners(context.Background()); err != nil {
		t.Fatalf("sweep 2 error = %v", err)
	}

	reg.byRepo["octo/repo"] = []RegisteredRunner{{ID: 2, Name: "runs-fleet-runner-flap", Status: "offline"}}
	if err := tk.ExecuteOrphanedRunners(context.Background()); err != nil {
		t.Fatalf("sweep 3 error = %v", err)
	}
	if len(reg.deleted) != 0 {
		t.Errorf("deleted %v after coming online in between, want none", reg.deleted)
	}
}

// An unreadable sighting means an unknown age, and deleting on an unknown age is
// the one mistake that destroys a live runner.
func TestExecuteOrphanedRunners_LeavesRegistrationWhenSightingUnavailable(t *testing.T) {
	reg := &stubRunnerRegistry{byRepo: map[string][]RegisteredRunner{
		"octo/repo": {{ID: 3, Name: "runs-fleet-runner-x", Status: "offline"}},
	}}
	tk, sight := runnersTask(reg, []string{"octo/repo"})
	sight.recordErr = errors.New("dynamo down")

	if err := tk.ExecuteOrphanedRunners(context.Background()); err != nil {
		t.Fatalf("sweep error = %v", err)
	}
	if len(reg.deleted) != 0 {
		t.Errorf("deleted %v despite an unknown offline age, want none", reg.deleted)
	}
}

// The sweep exists because GitHub only auto-deletes an ephemeral runner after it
// completes a job; one that never got work stays registered forever. Only those
// leftovers may be deleted.
func TestExecuteOrphanedRunners_DeletesOnlyOfflineRunsFleetRunners(t *testing.T) {
	reg := &stubRunnerRegistry{byRepo: map[string][]RegisteredRunner{
		"octo/repo": {
			{ID: 1, Name: "runs-fleet-runner-lingua-franca-466800-1233a", Status: "offline"},
			{ID: 2, Name: "runs-fleet-runner-lingua-franca-534581-7328e", Status: "online"},
			{ID: 3, Name: "runs-fleet-runner-lingua-franca-863999-6027a", Status: "offline", Busy: true},
			{ID: 4, Name: "some-other-teams-runner", Status: "offline"},
			{ID: 5, Name: "runs-fleet-runner-lingua-franca-985980-15d87", Status: "offline"},
		},
	}}
	tk, sight := runnersTask(reg, []string{"octo/repo"})

	sweepPastWindow(t, tk, sight)

	want := []int64{1, 5}
	if len(reg.deleted) != len(want) {
		t.Fatalf("deleted = %v, want %v", reg.deleted, want)
	}
	for i, id := range want {
		if reg.deleted[i] != id {
			t.Errorf("deleted[%d] = %d, want %d", i, reg.deleted[i], id)
		}
	}
}

// An online runner is a live instance waiting for or running work, and a busy
// runner is mid-job. Deleting either destroys running work.
func TestExecuteOrphanedRunners_NeverDeletesOnlineOrBusy(t *testing.T) {
	reg := &stubRunnerRegistry{byRepo: map[string][]RegisteredRunner{
		"octo/repo": {
			{ID: 10, Name: "runs-fleet-runner-a", Status: "online"},
			{ID: 11, Name: "runs-fleet-runner-b", Status: "online", Busy: true},
			{ID: 12, Name: "runs-fleet-runner-c", Status: "offline", Busy: true},
		},
	}}
	tk, sight := runnersTask(reg, []string{"octo/repo"})

	sweepPastWindow(t, tk, sight)
	if len(reg.deleted) != 0 {
		t.Errorf("deleted %v, want none", reg.deleted)
	}
}

// Runners this fleet did not create belong to someone else; the name prefix is
// the only thing that establishes ownership.
func TestExecuteOrphanedRunners_IgnoresForeignRunners(t *testing.T) {
	reg := &stubRunnerRegistry{byRepo: map[string][]RegisteredRunner{
		"octo/repo": {
			{ID: 20, Name: "ubuntu-latest-self-hosted", Status: "offline"},
			{ID: 21, Name: "arc-runner-set-xyz", Status: "offline"},
			{ID: 22, Name: "runs-fleet-runner-mine", Status: "offline"},
		},
	}}
	tk, sight := runnersTask(reg, []string{"octo/repo"})

	sweepPastWindow(t, tk, sight)
	if len(reg.deleted) != 1 || reg.deleted[0] != 22 {
		t.Errorf("deleted = %v, want [22]", reg.deleted)
	}
}

// One unreachable repo must not stop the others: the sweep is best-effort
// cleanup, and a partial pass still drains the backlog.
func TestExecuteOrphanedRunners_ContinuesPastPerRepoFailure(t *testing.T) {
	reg := &stubRunnerRegistry{
		byRepo: map[string][]RegisteredRunner{
			"octo/good": {{ID: 30, Name: "runs-fleet-runner-x", Status: "offline"}},
		},
		listErr: nil,
	}
	failing := &failingListRegistry{inner: reg, failFor: "octo/bad"}
	tk, sight := runnersTask(failing, []string{"octo/bad", "octo/good"})

	sweepPastWindow(t, tk, sight)
	if len(reg.deleted) != 1 || reg.deleted[0] != 30 {
		t.Errorf("deleted = %v, want [30] (good repo still swept)", reg.deleted)
	}
}

type failingListRegistry struct {
	inner   *stubRunnerRegistry
	failFor string
}

func (f *failingListRegistry) ListRunners(ctx context.Context, repo string) ([]RegisteredRunner, error) {
	if repo == f.failFor {
		return nil, errors.New("boom")
	}
	return f.inner.ListRunners(ctx, repo)
}

func (f *failingListRegistry) DeleteRunner(ctx context.Context, repo string, id int64) error {
	return f.inner.DeleteRunner(ctx, repo, id)
}

// Without the registry wired the sweep must do nothing rather than error, so an
// operator who has not enabled it sees no noise.
func TestExecuteOrphanedRunners_NoopWithoutRegistry(t *testing.T) {
	tk := &Tasks{}
	if err := tk.ExecuteOrphanedRunners(context.Background()); err != nil {
		t.Errorf("ExecuteOrphanedRunners() error = %v, want nil", err)
	}
}

// The per-cycle cap keeps one sweep from spending an unbounded API budget when a
// large backlog exists; the rest drains on later cycles.
func TestExecuteOrphanedRunners_BoundsDeletesPerCycle(t *testing.T) {
	var many []RegisteredRunner
	for i := range maxRunnerDeregistrations + 25 {
		many = append(many, RegisteredRunner{
			ID:     int64(i + 1),
			Name:   "runs-fleet-runner-bulk",
			Status: "offline",
		})
	}
	reg := &stubRunnerRegistry{byRepo: map[string][]RegisteredRunner{"octo/repo": many}}
	tk, sight := runnersTask(reg, []string{"octo/repo"})

	sweepPastWindow(t, tk, sight)
	if len(reg.deleted) != maxRunnerDeregistrations {
		t.Errorf("deleted %d runners, want the cap %d", len(reg.deleted), maxRunnerDeregistrations)
	}
}

func TestRunnerSweepInterval_IsSane(t *testing.T) {
	c := DefaultSchedulerConfig()
	if c.OrphanedRunnersInterval <= 0 {
		t.Fatal("OrphanedRunnersInterval must be positive")
	}
	if c.OrphanedRunnersInterval < time.Minute {
		t.Errorf("interval %v is too aggressive for an API-bound sweep", c.OrphanedRunnersInterval)
	}
}
