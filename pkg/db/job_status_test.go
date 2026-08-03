package db

import "testing"

func TestJobStatusConstants(t *testing.T) {
	tests := []struct {
		status   JobStatus
		expected string
	}{
		{JobStatusRunning, "running"},
		{JobStatusClaiming, "claiming"},
		{JobStatusTerminating, "terminating"},
		{JobStatusRequeued, "requeued"},
		{JobStatusCompleted, "completed"},
		{JobStatusSuccess, "success"},
		{JobStatusFailed, "failed"},
		{JobStatusError, "error"},
		{JobStatusOrphaned, "orphaned"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if string(tt.status) != tt.expected {
				t.Errorf("JobStatus constant = %q, want %q", string(tt.status), tt.expected)
			}
		})
	}
}

func TestJobStatusString(t *testing.T) {
	tests := []struct {
		status   JobStatus
		expected string
	}{
		{JobStatusRunning, "running"},
		{JobStatusClaiming, "claiming"},
		{JobStatusTerminating, "terminating"},
		{JobStatusRequeued, "requeued"},
		{JobStatusCompleted, "completed"},
		{JobStatusSuccess, "success"},
		{JobStatusFailed, "failed"},
		{JobStatusError, "error"},
		{JobStatusOrphaned, "orphaned"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if tt.status.String() != tt.expected {
				t.Errorf("String() = %q, want %q", tt.status.String(), tt.expected)
			}
		})
	}
}

func TestJobStatusIsTerminal(t *testing.T) {
	// The agent's vocabulary is what actually lands in the table: a live scan of
	// the production jobs table shows "success", "interrupted" and "failure",
	// never "failed". Matching only the JobStatus* spellings would make the
	// completed-instance sweep a near no-op.
	terminal := []JobStatus{
		JobStatusCompleted, JobStatusSuccess, JobStatusError,
		JobStatusAgentFailure, JobStatusAgentTimeout, JobStatusAgentInterrupted,
	}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s.IsTerminal() = false, want true: the instance has finished and is "+
				"only accruing cost", s)
		}
	}

	// requeued and terminating may still take work; orphaned is stamped by
	// ExecuteOrphanedJobs on a swallowed EC2 error, so trusting it would let a
	// transient API fault reap a live instance.
	live := []JobStatus{
		JobStatusLaunched, JobStatusRunning, JobStatusClaiming,
		JobStatusTerminating, JobStatusRequeued, JobStatusOrphaned,
	}
	for _, s := range live {
		if s.IsTerminal() {
			t.Errorf("%s.IsTerminal() = true, want false: reaping it would kill an instance "+
				"that may still do work", s)
		}
	}
}

// A job cancelled while still queued at GitHub ends in a terminal state: the
// instance that was provisioned for it has no work coming, so reapers must treat
// it like any other finished job.
func TestJobStatusCancelled_IsTerminal(t *testing.T) {
	t.Parallel()

	if !JobStatusCancelled.IsTerminal() {
		t.Error("cancelled must be terminal")
	}
	if JobStatusCancelled.String() != "cancelled" {
		t.Errorf("JobStatusCancelled = %q, want cancelled", JobStatusCancelled)
	}
}
