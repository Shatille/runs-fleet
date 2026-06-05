package housekeeping

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
)

// mockTaskExecutor implements TaskExecutor for testing.
type mockTaskExecutor struct {
	orphanedErr        error
	ssmErr             error
	jobsErr            error
	orphanedJobsErr    error
	staleJobsErr       error
	unconfirmedRunErr  error
	poolErr            error
	costErr            error
	dlqErr             error
	ephemeralPoolErr   error
	packerErr          error
	orphanedCall       int
	ssmCall            int
	jobsCall           int
	orphanedJobsCall   int
	staleJobsCall      int
	unconfirmedRunCall int
	poolCall           int
	costCall           int
	dlqCall            int
	ephemeralPoolCall  int
	packerCall         int
}

func (m *mockTaskExecutor) ExecuteOrphanedInstances(_ context.Context) error {
	m.orphanedCall++
	return m.orphanedErr
}

func (m *mockTaskExecutor) ExecuteStaleSecrets(_ context.Context) error {
	m.ssmCall++
	return m.ssmErr
}

func (m *mockTaskExecutor) ExecuteOldJobs(_ context.Context) error {
	m.jobsCall++
	return m.jobsErr
}

func (m *mockTaskExecutor) ExecuteOrphanedJobs(_ context.Context) error {
	m.orphanedJobsCall++
	return m.orphanedJobsErr
}

func (m *mockTaskExecutor) ExecuteStaleJobs(_ context.Context) error {
	m.staleJobsCall++
	return m.staleJobsErr
}

func (m *mockTaskExecutor) ExecuteUnconfirmedRunners(_ context.Context) error {
	m.unconfirmedRunCall++
	return m.unconfirmedRunErr
}

func (m *mockTaskExecutor) ExecutePoolAudit(_ context.Context) error {
	m.poolCall++
	return m.poolErr
}

func (m *mockTaskExecutor) ExecuteCostReport(_ context.Context) error {
	m.costCall++
	return m.costErr
}

func (m *mockTaskExecutor) ExecuteDLQRedrive(_ context.Context) error {
	m.dlqCall++
	return m.dlqErr
}

func (m *mockTaskExecutor) ExecuteEphemeralPoolCleanup(_ context.Context) error {
	m.ephemeralPoolCall++
	return m.ephemeralPoolErr
}

func (m *mockTaskExecutor) ExecuteOrphanedPackerInstances(_ context.Context) error {
	m.packerCall++
	return m.packerErr
}

// mockTaskLocker implements TaskLocker for testing.
type mockTaskLocker struct {
	acquireErr   error
	releaseErr   error
	acquireCalls int
	releaseCalls int
	lastTaskType string
	lastOwner    string
}

func (m *mockTaskLocker) AcquireTaskLock(_ context.Context, taskType, owner string, _ time.Duration) error {
	m.acquireCalls++
	m.lastTaskType = taskType
	m.lastOwner = owner
	return m.acquireErr
}

func (m *mockTaskLocker) ReleaseTaskLock(_ context.Context, taskType, owner string) error {
	m.releaseCalls++
	m.lastTaskType = taskType
	m.lastOwner = owner
	return m.releaseErr
}

// mockRunnerMetrics records the runner's per-task latency emissions. Safe for
// concurrent use so a Run loop can be raced.
type mockRunnerMetrics struct {
	mu      sync.Mutex
	calls   int
	queue   string
	result  string
	seconds float64
}

func (m *mockRunnerMetrics) PublishMessageProcessingSeconds(_ context.Context, queue, result string, seconds float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.queue = queue
	m.result = result
	m.seconds = seconds
	return nil
}

func (m *mockRunnerMetrics) snapshot() (int, string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, m.queue, m.result
}

// longIntervals returns a config where every task interval is far longer than
// any test runs, so only initial-tick tasks fire.
func longIntervals() SchedulerConfig {
	d := time.Hour
	return SchedulerConfig{
		OrphanedInstancesInterval:       d,
		StaleSSMInterval:                d,
		OldJobsInterval:                 d,
		OrphanedJobsInterval:            d,
		PoolAuditInterval:               d,
		CostReportInterval:              d,
		DLQRedriveInterval:              d,
		EphemeralPoolCleanupInterval:    d,
		StaleJobsInterval:               d,
		UnconfirmedRunnersInterval:      d,
		OrphanedPackerInstancesInterval: d,
	}
}

// findSpec returns the taskSpec for a task type, failing if absent.
func findSpec(t *testing.T, r *Runner, taskType TaskType) taskSpec {
	t.Helper()
	for _, s := range r.taskSpecs() {
		if s.taskType == taskType {
			return s
		}
	}
	t.Fatalf("no taskSpec for %s", taskType)
	return taskSpec{}
}

func TestNewRunner(t *testing.T) {
	t.Parallel()

	executor := &mockTaskExecutor{}
	cfg := DefaultSchedulerConfig()
	r := NewRunner(executor, cfg)

	if r.executor != executor {
		t.Error("expected executor to be set")
	}
	if r.config != cfg {
		t.Error("expected config to be set")
	}
	if r.logger() == nil {
		t.Error("expected logger to be available")
	}
}

func TestRunner_SetTaskLocker(t *testing.T) {
	t.Parallel()

	r := NewRunner(&mockTaskExecutor{}, DefaultSchedulerConfig())
	locker := &mockTaskLocker{}
	r.SetTaskLocker(locker, "instance-1")

	if r.taskLocker != locker {
		t.Error("expected taskLocker to be set")
	}
	if r.instanceID != "instance-1" {
		t.Error("expected instanceID to be set")
	}
}

// TestRunner_TaskSpecs_Covers proves taskSpecs lists every task type exactly
// once, so no executor method is left unscheduled.
func TestRunner_TaskSpecs_Covers(t *testing.T) {
	t.Parallel()

	want := []TaskType{
		TaskOrphanedInstances, TaskStaleSecrets, TaskOldJobs, TaskOrphanedJobs,
		TaskStaleJobs, TaskUnconfirmedRunners, TaskPoolAudit, TaskCostReport, TaskDLQRedrive,
		TaskEphemeralPoolCleanup, TaskOrphanedPackerInstances,
	}
	r := NewRunner(&mockTaskExecutor{}, DefaultSchedulerConfig())
	specs := r.taskSpecs()
	if len(specs) != len(want) {
		t.Fatalf("taskSpecs len = %d, want %d", len(specs), len(want))
	}
	seen := map[TaskType]bool{}
	for _, s := range specs {
		if seen[s.taskType] {
			t.Errorf("duplicate taskSpec for %s", s.taskType)
		}
		seen[s.taskType] = true
		if s.interval <= 0 {
			t.Errorf("%s has non-positive interval %v", s.taskType, s.interval)
		}
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("missing taskSpec for %s", w)
		}
	}
}

// TestRunner_TryRunTask_DispatchesToExecutor proves each spec routes to its
// matching executor method.
func TestRunner_TryRunTask_DispatchesToExecutor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		taskType TaskType
		count    func(*mockTaskExecutor) int
	}{
		{TaskOrphanedInstances, func(m *mockTaskExecutor) int { return m.orphanedCall }},
		{TaskStaleSecrets, func(m *mockTaskExecutor) int { return m.ssmCall }},
		{TaskOldJobs, func(m *mockTaskExecutor) int { return m.jobsCall }},
		{TaskOrphanedJobs, func(m *mockTaskExecutor) int { return m.orphanedJobsCall }},
		{TaskStaleJobs, func(m *mockTaskExecutor) int { return m.staleJobsCall }},
		{TaskUnconfirmedRunners, func(m *mockTaskExecutor) int { return m.unconfirmedRunCall }},
		{TaskPoolAudit, func(m *mockTaskExecutor) int { return m.poolCall }},
		{TaskCostReport, func(m *mockTaskExecutor) int { return m.costCall }},
		{TaskDLQRedrive, func(m *mockTaskExecutor) int { return m.dlqCall }},
		{TaskEphemeralPoolCleanup, func(m *mockTaskExecutor) int { return m.ephemeralPoolCall }},
		{TaskOrphanedPackerInstances, func(m *mockTaskExecutor) int { return m.packerCall }},
	}

	for _, c := range cases {
		t.Run(string(c.taskType), func(t *testing.T) {
			executor := &mockTaskExecutor{}
			r := NewRunner(executor, DefaultSchedulerConfig())
			r.tryRunTask(context.Background(), findSpec(t, r, c.taskType))
			if got := c.count(executor); got != 1 {
				t.Errorf("%s: executor call count = %d, want 1", c.taskType, got)
			}
		})
	}
}

func TestRunner_TryRunTask_WithLocking_Success(t *testing.T) {
	t.Parallel()

	executor := &mockTaskExecutor{}
	locker := &mockTaskLocker{}
	r := NewRunner(executor, DefaultSchedulerConfig())
	r.SetTaskLocker(locker, "instance-1")

	r.tryRunTask(context.Background(), findSpec(t, r, TaskOrphanedInstances))

	if locker.acquireCalls != 1 {
		t.Errorf("expected 1 acquire call, got %d", locker.acquireCalls)
	}
	if locker.releaseCalls != 1 {
		t.Errorf("expected 1 release call, got %d", locker.releaseCalls)
	}
	if executor.orphanedCall != 1 {
		t.Errorf("expected 1 orphaned call, got %d", executor.orphanedCall)
	}
	if locker.lastTaskType != string(TaskOrphanedInstances) {
		t.Errorf("expected task type '%s', got '%s'", TaskOrphanedInstances, locker.lastTaskType)
	}
	if locker.lastOwner != "instance-1" {
		t.Errorf("expected owner 'instance-1', got '%s'", locker.lastOwner)
	}
}

func TestRunner_TryRunTask_LockHeldSkips(t *testing.T) {
	t.Parallel()

	executor := &mockTaskExecutor{}
	locker := &mockTaskLocker{acquireErr: db.ErrTaskLockHeld}
	r := NewRunner(executor, DefaultSchedulerConfig())
	r.SetTaskLocker(locker, "instance-1")

	r.tryRunTask(context.Background(), findSpec(t, r, TaskOrphanedInstances))

	if executor.orphanedCall != 0 {
		t.Errorf("expected 0 orphaned calls when lock held, got %d", executor.orphanedCall)
	}
	if locker.releaseCalls != 0 {
		t.Errorf("expected 0 release calls when lock held, got %d", locker.releaseCalls)
	}
}

func TestRunner_TryRunTask_AcquireErrorSkips(t *testing.T) {
	t.Parallel()

	executor := &mockTaskExecutor{}
	locker := &mockTaskLocker{acquireErr: errors.New("dynamodb error")}
	r := NewRunner(executor, DefaultSchedulerConfig())
	r.SetTaskLocker(locker, "instance-1")

	r.tryRunTask(context.Background(), findSpec(t, r, TaskOrphanedInstances))

	if executor.orphanedCall != 0 {
		t.Errorf("expected 0 orphaned calls on acquire error, got %d", executor.orphanedCall)
	}
	if locker.releaseCalls != 0 {
		t.Errorf("expected 0 release calls on acquire error, got %d", locker.releaseCalls)
	}
}

func TestRunner_TryRunTask_ReleasesOnTaskError(t *testing.T) {
	t.Parallel()

	executor := &mockTaskExecutor{orphanedErr: errors.New("task failed")}
	locker := &mockTaskLocker{}
	r := NewRunner(executor, DefaultSchedulerConfig())
	r.SetTaskLocker(locker, "instance-1")

	r.tryRunTask(context.Background(), findSpec(t, r, TaskOrphanedInstances))

	if executor.orphanedCall != 1 {
		t.Errorf("expected 1 orphaned call, got %d", executor.orphanedCall)
	}
	if locker.releaseCalls != 1 {
		t.Errorf("expected 1 release call on task failure, got %d", locker.releaseCalls)
	}
}

func TestRunner_TryRunTask_WithoutLocking(t *testing.T) {
	t.Parallel()

	executor := &mockTaskExecutor{}
	r := NewRunner(executor, DefaultSchedulerConfig())

	r.tryRunTask(context.Background(), findSpec(t, r, TaskOrphanedInstances))

	if executor.orphanedCall != 1 {
		t.Errorf("expected 1 orphaned call without locking, got %d", executor.orphanedCall)
	}
}

func TestRunner_TryRunTask_EmitsMetrics(t *testing.T) {
	t.Parallel()

	t.Run("ok", func(t *testing.T) {
		m := &mockRunnerMetrics{}
		r := NewRunner(&mockTaskExecutor{}, DefaultSchedulerConfig())
		r.SetMetrics(m)
		r.tryRunTask(context.Background(), findSpec(t, r, TaskOrphanedInstances))
		calls, queueLabel, result := m.snapshot()
		if calls != 1 {
			t.Fatalf("metric calls = %d, want 1", calls)
		}
		if queueLabel != queueHousekeeping {
			t.Errorf("queue label = %q, want %q", queueLabel, queueHousekeeping)
		}
		if result != "ok" {
			t.Errorf("result = %q, want ok", result)
		}
	})

	t.Run("error", func(t *testing.T) {
		m := &mockRunnerMetrics{}
		r := NewRunner(&mockTaskExecutor{orphanedErr: errors.New("boom")}, DefaultSchedulerConfig())
		r.SetMetrics(m)
		r.tryRunTask(context.Background(), findSpec(t, r, TaskOrphanedInstances))
		calls, _, result := m.snapshot()
		if calls != 1 {
			t.Fatalf("metric calls = %d, want 1", calls)
		}
		if result != "error" {
			t.Errorf("result = %q, want error", result)
		}
	})
}

// signalingExecutor reports each executed task type on a channel so a test can
// wait for specific tasks to run rather than sleeping.
type signalingExecutor struct {
	mockTaskExecutor
	ran chan TaskType
}

func (e *signalingExecutor) ExecuteOrphanedInstances(ctx context.Context) error {
	err := e.mockTaskExecutor.ExecuteOrphanedInstances(ctx)
	e.ran <- TaskOrphanedInstances
	return err
}

func (e *signalingExecutor) ExecuteDLQRedrive(ctx context.Context) error {
	err := e.mockTaskExecutor.ExecuteDLQRedrive(ctx)
	e.ran <- TaskDLQRedrive
	return err
}

// TestRunner_Run_RunsInitialTasksOnStart proves the initial-tick tasks
// (orphaned instances, dlq redrive) fire once on start while the rest wait for
// their interval.
func TestRunner_Run_RunsInitialTasksOnStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &signalingExecutor{ran: make(chan TaskType, 10)}
		r := NewRunner(executor, longIntervals())

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			r.Run(ctx)
			close(done)
		}()

		// Let the initial-tick tasks run and every task loop settle into its
		// ticker select. After Wait returns no bubble goroutine is runnable, so
		// the signals buffered on executor.ran are exactly the initial set.
		synctest.Wait()

		got := map[TaskType]bool{}
		for len(executor.ran) > 0 {
			got[<-executor.ran] = true
		}
		if !got[TaskOrphanedInstances] {
			t.Error("orphaned instances (initial) did not run on start")
		}
		if !got[TaskDLQRedrive] {
			t.Error("dlq redrive (initial) did not run on start")
		}

		// Non-initial tasks must not have fired: their 1h tickers have not
		// elapsed, so these counters were never written.
		if executor.ssmCall != 0 || executor.jobsCall != 0 || executor.poolCall != 0 {
			t.Errorf("non-initial tasks ran before their interval: ssm=%d jobs=%d pool=%d",
				executor.ssmCall, executor.jobsCall, executor.poolCall)
		}

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Run did not return after cancel")
		}
	})
}

// blockingTaskExecutor pauses orphaned-instances mid-execution so a test can
// cancel the runner's parent context while the task is in flight, then inspect
// whether the task's context observed that cancellation.
type blockingTaskExecutor struct {
	mockTaskExecutor
	started     chan struct{}
	release     chan struct{}
	ctxErr      error
	startedOnce sync.Once
}

func (b *blockingTaskExecutor) ExecuteOrphanedInstances(ctx context.Context) error {
	b.startedOnce.Do(func() { close(b.started) })
	<-b.release
	b.ctxErr = ctx.Err()
	return nil
}

// TestRunner_Run_InflightTaskRunsToCompletionAfterCancel simulates a SIGTERM
// arriving while a task is executing: the parent context is cancelled mid-task.
// The in-flight task must run to completion observing a context that is NOT
// cancelled, and Run must return only after it finishes.
func TestRunner_Run_InflightTaskRunsToCompletionAfterCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &blockingTaskExecutor{started: make(chan struct{}), release: make(chan struct{})}
		r := NewRunner(executor, longIntervals())

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			r.Run(ctx)
			close(done)
		}()

		// Wait until the orphaned-instances task is in flight (parked on
		// <-release) and every other loop is blocked in its ticker select.
		<-executor.started
		synctest.Wait()

		// SIGTERM arrives mid-task.
		cancel()
		synctest.Wait()

		// Run must NOT have returned: the in-flight task is detached from the
		// signal and still blocked on release. This replaces a 50ms sleep that
		// guessed at "long enough to prove Run is still running".
		select {
		case <-done:
			t.Fatal("Run returned before in-flight task completed")
		default:
		}

		// Let the in-flight task finish.
		close(executor.release)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Run did not return after in-flight task completed")
		}

		// done is closed, so this read happens-after the task goroutine wrote
		// ctxErr (ordered through the WaitGroup and the close).
		if executor.ctxErr != nil {
			t.Errorf("task context was cancelled by SIGTERM (%v); in-flight work must be detached from the signal", executor.ctxErr)
		}
	})
}

func TestRunner_Run_StopsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := NewRunner(&mockTaskExecutor{}, longIntervals())
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			r.Run(ctx)
			close(done)
		}()

		synctest.Wait()
		cancel()
		synctest.Wait()

		select {
		case <-done:
		default:
			t.Fatal("Run did not stop after cancellation")
		}
	})
}

// countingExecutor records how many times the stale-secrets task ran via an
// atomic, so a test goroutine can read the count while the task loop is still
// alive (merely blocked on its ticker) without a data race.
type countingExecutor struct {
	mockTaskExecutor
	staleSecrets atomic.Int64
}

func (c *countingExecutor) ExecuteStaleSecrets(ctx context.Context) error {
	err := c.mockTaskExecutor.ExecuteStaleSecrets(ctx)
	c.staleSecrets.Add(1)
	return err
}

// TestRunner_Run_FiresTasksOnInterval proves a non-initial task fires once per
// elapsed interval. It advances the synctest fake clock instead of waiting on
// real time, so the scheduler's core periodic behavior is exercised in zero
// wall-clock time.
func TestRunner_Run_FiresTasksOnInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = time.Minute
		const n = 3

		// Only stale-secrets gets a short interval; the rest stay at 1h so the
		// count reflects exactly the stale-secrets ticker.
		cfg := longIntervals()
		cfg.StaleSSMInterval = interval

		exec := &countingExecutor{}
		r := NewRunner(exec, cfg)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			r.Run(ctx)
			close(done)
		}()

		// Settle so every ticker is armed from a common fake-time baseline.
		synctest.Wait()

		// Advance fake time across n intervals (+half to avoid landing exactly
		// on a tick boundary). time.NewTicker fires at d, 2d, 3d, ..., so a
		// non-initial task fires exactly n times over n intervals.
		time.Sleep(n*interval + interval/2)
		synctest.Wait()

		if got := exec.staleSecrets.Load(); got != int64(n) {
			t.Errorf("StaleSecrets fired %d times over %d intervals, want %d", got, n, n)
		}

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Run did not return after cancel")
		}
	})
}

func TestTaskTypeConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		taskType TaskType
		expected string
	}{
		{TaskOrphanedInstances, "orphaned_instances"},
		{TaskStaleSecrets, "stale_secrets"},
		{TaskOldJobs, "old_jobs"},
		{TaskPoolAudit, "pool_audit"},
		{TaskCostReport, "cost_report"},
		{TaskDLQRedrive, "dlq_redrive"},
		{TaskEphemeralPoolCleanup, "ephemeral_pool_cleanup"},
		{TaskOrphanedJobs, "orphaned_jobs"},
		{TaskStaleJobs, "stale_jobs"},
		{TaskOrphanedPackerInstances, "orphaned_packer_instances"},
	}

	for _, tt := range tests {
		if string(tt.taskType) != tt.expected {
			t.Errorf("expected TaskType '%s', got '%s'", tt.expected, tt.taskType)
		}
	}
}

func TestDefaultSchedulerConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultSchedulerConfig()
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"OrphanedInstances", cfg.OrphanedInstancesInterval, 5 * time.Minute},
		{"StaleSSM", cfg.StaleSSMInterval, 15 * time.Minute},
		{"OldJobs", cfg.OldJobsInterval, 1 * time.Hour},
		{"OrphanedJobs", cfg.OrphanedJobsInterval, 15 * time.Minute},
		{"PoolAudit", cfg.PoolAuditInterval, 10 * time.Minute},
		{"CostReport", cfg.CostReportInterval, 24 * time.Hour},
		{"DLQRedrive", cfg.DLQRedriveInterval, 1 * time.Minute},
		{"EphemeralPoolCleanup", cfg.EphemeralPoolCleanupInterval, 1 * time.Hour},
		{"StaleJobs", cfg.StaleJobsInterval, 5 * time.Minute},
		{"OrphanedPackerInstances", cfg.OrphanedPackerInstancesInterval, 15 * time.Minute},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s interval = %v, want %v", c.name, c.got, c.want)
		}
	}
}
