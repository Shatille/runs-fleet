package db

// JobStatus represents the lifecycle state of a job in DynamoDB.
type JobStatus string

// Job lifecycle states stored in DynamoDB.
const (
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
