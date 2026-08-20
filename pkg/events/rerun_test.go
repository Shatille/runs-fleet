package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/queue"
)

type MockJobRerunner struct {
	mu       sync.Mutex
	statuses []WorkflowJobState
	calls    int
	reruns   []int64
	rerunErr error
	statusFn func(ctx context.Context, repo string, jobID int64) (WorkflowJobState, error)
}

func (m *MockJobRerunner) GetWorkflowJobState(ctx context.Context, repo string, jobID int64) (WorkflowJobState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.statusFn != nil {
		return m.statusFn(ctx, repo, jobID)
	}
	i := m.calls
	m.calls++
	if i >= len(m.statuses) {
		i = len(m.statuses) - 1
	}
	if i < 0 {
		return WorkflowJobState{}, errors.New("no status configured")
	}
	return m.statuses[i], nil
}

func (m *MockJobRerunner) RerunJob(_ context.Context, _ string, jobID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reruns = append(m.reruns, jobID)
	return m.rerunErr
}

func (m *MockJobRerunner) rerunCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reruns)
}

func testJob() *JobInfo {
	return &JobInfo{JobID: 4242, RunID: 99, Repo: "myorg/myrepo", RetryCount: 0}
}

// GitHub does not conclude the job the moment the runner dies, so a re-run
// issued at interruption time is rejected. The handler has to wait for the
// failure to land before it can recover it.
func TestRecoverInterruptedJobWaitsForGitHubToConcludeTheJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gh := &MockJobRerunner{statuses: []WorkflowJobState{
			{Status: "in_progress"},
			{Status: "in_progress"},
			{Status: "completed", Conclusion: "failure"},
		}}
		h := &Handler{gitHub: gh}

		if err := h.recoverInterruptedJob(context.Background(), testJob()); err != nil {
			t.Fatalf("recoverInterruptedJob() error = %v", err)
		}
		if got := gh.rerunCount(); got != 1 {
			t.Fatalf("rerun calls = %d, want 1", got)
		}
	})
}

// A job that somehow finished despite the reclaim must never be re-run: that
// would duplicate work GitHub already accepted.
func TestRecoverInterruptedJobSkipsASucceededJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gh := &MockJobRerunner{statuses: []WorkflowJobState{{Status: "completed", Conclusion: "success"}}}
		h := &Handler{gitHub: gh}

		if err := h.recoverInterruptedJob(context.Background(), testJob()); err != nil {
			t.Fatalf("recoverInterruptedJob() error = %v", err)
		}
		if got := gh.rerunCount(); got != 0 {
			t.Errorf("rerun calls = %d, want 0 for a job that succeeded", got)
		}
	})
}

// A reclaim during the re-run must not start another one; one recovery attempt
// per job is the whole budget.
func TestRecoverInterruptedJobRefusesToRerunARetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gh := &MockJobRerunner{statuses: []WorkflowJobState{{Status: "completed", Conclusion: "failure"}}}
		h := &Handler{gitHub: gh}

		job := testJob()
		job.RetryCount = 1
		if err := h.recoverInterruptedJob(context.Background(), job); err != nil {
			t.Fatalf("recoverInterruptedJob() error = %v", err)
		}
		if got := gh.rerunCount(); got != 0 {
			t.Errorf("rerun calls = %d, want 0 for an already-retried job", got)
		}
	})
}

// The wait is bounded: a job GitHub never concludes must not pin the events
// worker until the SQS visibility timeout expires and the event is redelivered.
func TestRecoverInterruptedJobGivesUpWhenTheJobNeverConcludes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gh := &MockJobRerunner{statuses: []WorkflowJobState{{Status: "in_progress"}}}
		h := &Handler{gitHub: gh}

		start := time.Now()
		err := h.recoverInterruptedJob(context.Background(), testJob())
		if err == nil {
			t.Fatal("recoverInterruptedJob() error = nil, want a timeout error")
		}
		if got := gh.rerunCount(); got != 0 {
			t.Errorf("rerun calls = %d, want 0", got)
		}
		if waited := time.Since(start); waited > rerunWaitBudget {
			t.Errorf("waited %v, want no more than %v", waited, rerunWaitBudget)
		}
	})
}

// A cancelled job is somebody's deliberate stop, not our reclaim's doing.
func TestRecoverInterruptedJobSkipsCancelledJobs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gh := &MockJobRerunner{statuses: []WorkflowJobState{{Status: "completed", Conclusion: "cancelled"}}}
		h := &Handler{gitHub: gh}

		if err := h.recoverInterruptedJob(context.Background(), testJob()); err != nil {
			t.Fatalf("recoverInterruptedJob() error = %v", err)
		}
		if got := gh.rerunCount(); got != 0 {
			t.Errorf("rerun calls = %d, want 0 for a cancelled job", got)
		}
	})
}

// Without a GitHub client wired the handler must degrade quietly, exactly as
// the rest of the interruption path does.
func TestRecoverInterruptedJobIsANoOpWithoutAGitHubClient(t *testing.T) {
	h := &Handler{}
	if err := h.recoverInterruptedJob(context.Background(), testJob()); err != nil {
		t.Errorf("recoverInterruptedJob() error = %v, want nil when no client is configured", err)
	}
}

func TestRecoverInterruptedJobIgnoresJobsWithoutIdentity(t *testing.T) {
	gh := &MockJobRerunner{statuses: []WorkflowJobState{{Status: "completed", Conclusion: "failure"}}}
	h := &Handler{gitHub: gh}

	if err := h.recoverInterruptedJob(context.Background(), &JobInfo{}); err != nil {
		t.Errorf("recoverInterruptedJob() error = %v, want nil", err)
	}
	if got := gh.rerunCount(); got != 0 {
		t.Errorf("rerun calls = %d, want 0 for a job with no ID", got)
	}
}

// The re-queue and the re-run are complementary, not alternatives. A reclaim
// that lands before GitHub gives up is rescued by the replacement runner
// taking the still-dispatchable job; only once GitHub concludes the job failed
// does no runner exist that can take it. Dropping the re-queue in favour of
// the re-run would lose the rescues that already work.
func TestSpotInterruptionRequeuesAndThenRerunsWhenGitHubGivesUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requeued bool
		mockDB := &MockDBAPI{GetJobByInstanceFunc: func(_ context.Context, _ string) (*JobInfo, error) {
			return testJob(), nil
		}}
		mockQueue := &MockQueueAPI{SendMessageFunc: func(_ context.Context, _ *queue.JobMessage) error {
			requeued = true
			return nil
		}}
		gh := &MockJobRerunner{statuses: []WorkflowJobState{{Status: "completed", Conclusion: "failure"}}}

		h := &Handler{queueClient: mockQueue, dbClient: mockDB, metrics: &MockMetricsAPI{}, config: &config.Config{}}
		h.SetJobQueue(mockQueue)
		h.SetGitHub(gh)

		h.processEvent(context.Background(), queue.Message{
			Body:   `{"detail-type":"EC2 Spot Instance Interruption Warning","detail":{"instance-id":"i-x","instance-action":"terminate"}}`,
			Handle: "h",
		})

		if !requeued {
			t.Error("job was not re-queued; the re-run must not replace the re-queue")
		}
		if got := gh.rerunCount(); got != 1 {
			t.Errorf("rerun calls = %d, want 1 once GitHub concluded the job failed", got)
		}
	})
}

// When the re-queue rescues the job, GitHub never concludes it failed and no
// re-run must fire — otherwise the work runs twice.
func TestSpotInterruptionDoesNotRerunWhenTheRequeueRescuedTheJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mockDB := &MockDBAPI{GetJobByInstanceFunc: func(_ context.Context, _ string) (*JobInfo, error) {
			return testJob(), nil
		}}
		mockQueue := &MockQueueAPI{}
		gh := &MockJobRerunner{statuses: []WorkflowJobState{{Status: "completed", Conclusion: "success"}}}

		h := &Handler{queueClient: mockQueue, dbClient: mockDB, metrics: &MockMetricsAPI{}, config: &config.Config{}}
		h.SetJobQueue(mockQueue)
		h.SetGitHub(gh)

		h.processEvent(context.Background(), queue.Message{
			Body:   `{"detail-type":"EC2 Spot Instance Interruption Warning","detail":{"instance-id":"i-x","instance-action":"terminate"}}`,
			Handle: "h",
		})

		if got := gh.rerunCount(); got != 0 {
			t.Errorf("rerun calls = %d, want 0 when the job ended up succeeding", got)
		}
	})
}
