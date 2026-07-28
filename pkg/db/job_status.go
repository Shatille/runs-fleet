package db

// JobStatus represents the lifecycle state of a job in DynamoDB.
type JobStatus string

// Job lifecycle states stored in DynamoDB.
const (
	JobStatusLaunched    JobStatus = "launched"
	JobStatusRunning     JobStatus = "running"
	JobStatusClaiming    JobStatus = "claiming"
	JobStatusTerminating JobStatus = "terminating"
	JobStatusRequeued    JobStatus = "requeued"
	JobStatusCompleted   JobStatus = "completed"
	JobStatusSuccess     JobStatus = "success"
	JobStatusFailed      JobStatus = "failed"
	JobStatusError       JobStatus = "error"
	JobStatusOrphaned    JobStatus = "orphaned"
)

func (s JobStatus) String() string {
	return string(s)
}

// Agent-reported terminal statuses. The agent writes its own vocabulary
// (pkg/agent/telemetry.go) straight through MarkJobComplete, so the table holds
// "failure" rather than JobStatusFailed and carries "timeout"/"interrupted",
// which have no JobStatus constant.
const (
	JobStatusAgentFailure     JobStatus = "failure"
	JobStatusAgentTimeout     JobStatus = "timeout"
	JobStatusAgentInterrupted JobStatus = "interrupted"
)

// IsTerminal reports whether the job has reached an end state, meaning the
// instance that ran it has no further work and is only accruing cost.
//
// Deliberately excluded: requeued and terminating (the instance may still take
// work), and orphaned — that status is stamped by ExecuteOrphanedJobs on a
// swallowed EC2 lookup error, so a transient API fault would otherwise mark a
// live instance's job terminal and get the instance reaped.
func (s JobStatus) IsTerminal() bool {
	switch s {
	case JobStatusCompleted, JobStatusSuccess, JobStatusError,
		JobStatusAgentFailure, JobStatusAgentTimeout, JobStatusAgentInterrupted:
		return true
	default:
		return false
	}
}
