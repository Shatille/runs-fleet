package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// rerunWaitBudget bounds the wait for GitHub to conclude an interrupted job.
// The events worker is blocked for the duration, so it must leave room inside
// config.MessageProcessTimeout (90s) for the rest of the handler; spending the
// whole budget here would cascade deadline errors across the interruption path.
//
// GitHub concluded a reclaimed job a median of 29s after the job started, but
// p90 was 169s, so this deliberately does not wait out the slow tail: a job
// still running when the budget expires is left to the re-queue, which is the
// mechanism that rescues a job GitHub has not yet given up on anyway.
var rerunWaitBudget = 45 * time.Second

// rerunPollInterval is how often the job's status is re-read while waiting.
var rerunPollInterval = 5 * time.Second

// WorkflowJobState is GitHub's view of a workflow job.
type WorkflowJobState struct {
	Status     string // "queued", "in_progress", "completed"
	Conclusion string // "success", "failure", "cancelled", ...
}

// JobRerunner reads a workflow job's state and asks GitHub to re-run it.
type JobRerunner interface {
	GetWorkflowJobState(ctx context.Context, repo string, jobID int64) (WorkflowJobState, error)
	RerunJob(ctx context.Context, repo string, jobID int64) error
}

// SetGitHub sets the client used to recover jobs killed by a spot reclaim.
func (h *Handler) SetGitHub(gh JobRerunner) {
	h.gitHub = gh
}

// recoverInterruptedJob re-runs a job whose runner AWS reclaimed.
//
// Re-queueing cannot recover such a job: registration binds a runner to a
// label, not to a job, so GitHub never hands the dead job to the replacement,
// and by the time one registers GitHub has already concluded the job failed.
// Re-running is the only route back.
//
// It is safe because the caller is the spot-interruption path alone: the job
// was killed mid-flight and never succeeded, so a re-run repeats an aborted
// attempt rather than duplicating accepted work.
//
// Errors are returned for logging only; the interruption path must not fail
// over a recovery attempt.
func (h *Handler) recoverInterruptedJob(ctx context.Context, job *JobInfo) error {
	if h.gitHub == nil || job == nil || job.JobID == 0 || job.Repo == "" {
		return nil
	}
	// One recovery per job: a reclaim during the re-run must not start another.
	if job.RetryCount > 0 {
		eventsLog.Info(ctx, "job already retried; not re-running after interruption",
			slog.Int("retry_count", job.RetryCount))
		return nil
	}

	state, err := h.awaitConcludedJob(ctx, job)
	if err != nil {
		return err
	}
	// Only a failure is ours to undo. A success means the job finished despite
	// the reclaim, and a cancellation is somebody's deliberate stop.
	if state.Conclusion != "failure" {
		eventsLog.Info(ctx, "interrupted job did not fail; nothing to re-run",
			slog.String("conclusion", state.Conclusion))
		return nil
	}

	if err := h.gitHub.RerunJob(ctx, job.Repo, job.JobID); err != nil {
		return fmt.Errorf("re-run job %d: %w", job.JobID, err)
	}
	eventsLog.Info(ctx, "re-ran job after spot interruption")
	return nil
}

// awaitConcludedJob polls until GitHub reports the job completed, or the
// budget runs out.
func (h *Handler) awaitConcludedJob(ctx context.Context, job *JobInfo) (WorkflowJobState, error) {
	deadline := time.Now().Add(rerunWaitBudget)
	for {
		state, err := h.gitHub.GetWorkflowJobState(ctx, job.Repo, job.JobID)
		if err != nil {
			return WorkflowJobState{}, fmt.Errorf("read job %d state: %w", job.JobID, err)
		}
		if state.Status == "completed" {
			return state, nil
		}
		if !time.Now().Before(deadline) {
			return WorkflowJobState{}, fmt.Errorf("job %d still %q after %s; not re-running",
				job.JobID, state.Status, rerunWaitBudget)
		}
		select {
		case <-ctx.Done():
			return WorkflowJobState{}, ctx.Err()
		case <-time.After(rerunPollInterval):
		}
	}
}
