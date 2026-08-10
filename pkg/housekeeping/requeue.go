package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// MaxRequeueRetries bounds how many times a runner-less job may be requeued, mirroring
// the worker's on-demand retry cap so a fleet-wide registration failure cannot churn
// instances indefinitely. Shared by the in-process unconfirmed-runner watchdog and the
// operator-triggered requeue action.
const MaxRequeueRetries = 2

// requeueReasonOperator labels requeues and scheduling failures driven by the
// operator-triggered RequeueHungJobs action, kept distinct from the watchdog's
// unconfirmed_runners so a spike signals the automated watchdog isn't keeping up.
const requeueReasonOperator = "operator_requeue"

// RequeueableJob carries the fields needed to rebuild a launch message for a job whose
// runner needs to be re-dispatched.
type RequeueableJob struct {
	JobID        int64
	InstanceID   string
	RunID        int64
	Repo         string
	InstanceType string
	Pool         string
	RetryCount   int
	Status       string
}

// FindRequeueableJobs scans DynamoDB for jobs in any of the given statuses whose
// created_at is older than the threshold, projecting the fields needed to rebuild a
// launch message. It is the single source of truth for "which records describe a job
// whose runner may need re-dispatching" — shared by the watchdog and the
// operator-triggered action (both pass launched only; see RequeueOptions.Statuses).
func FindRequeueableJobs(ctx context.Context, scanAPI OrphanScanAPI, jobsTable string, threshold time.Duration, statuses []db.JobStatus) ([]RequeueableJob, error) {
	if len(statuses) == 0 {
		return nil, nil
	}

	cutoff := time.Now().Add(-threshold).Format(time.RFC3339)

	statusNames := make([]string, len(statuses))
	exprValues := map[string]types.AttributeValue{
		":cutoff": &types.AttributeValueMemberS{Value: cutoff},
	}
	for i, s := range statuses {
		ph := fmt.Sprintf(":s%d", i)
		statusNames[i] = "#status = " + ph
		exprValues[ph] = &types.AttributeValueMemberS{Value: string(s)}
	}
	filter := fmt.Sprintf("(%s) AND created_at < :cutoff", strings.Join(statusNames, " OR "))

	input := &dynamodb.ScanInput{
		TableName:                 aws.String(jobsTable),
		FilterExpression:          aws.String(filter),
		ExpressionAttributeNames:  map[string]string{"#status": "status", "#pool": "pool"},
		ExpressionAttributeValues: exprValues,
		ProjectionExpression:      aws.String("job_id, instance_id, run_id, repo, instance_type, #pool, retry_count, #status"),
	}

	var jobs []RequeueableJob
	var lastKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastKey
		output, err := scanAPI.Scan(ctx, input)
		if err != nil {
			return nil, err
		}
		for _, item := range output.Items {
			j := RequeueableJob{
				JobID:        avInt64(item, "job_id"),
				InstanceID:   avString(item, "instance_id"),
				RunID:        avInt64(item, "run_id"),
				Repo:         avString(item, "repo"),
				InstanceType: avString(item, "instance_type"),
				Pool:         avString(item, "pool"),
				RetryCount:   int(avInt64(item, "retry_count")),
				Status:       avString(item, "status"),
			}
			if j.JobID == 0 {
				continue
			}
			jobs = append(jobs, j)
		}
		lastKey = output.LastEvaluatedKey
		if lastKey == nil {
			break
		}
	}
	return jobs, nil
}

// BuildRequeueMessage builds the SQS launch message that re-dispatches a fresh runner
// for an already-queued GitHub job. It forces on-demand (reliability over the negligible
// spot saving on a recovery) and bumps the retry count so the FIFO dedup id advances and
// the worker's retry budget is honored. It never touches the GitHub job — only re-enqueues
// into runs-fleet's own queue.
func BuildRequeueMessage(job RequeueableJob) *queue.JobMessage {
	return &queue.JobMessage{
		JobID:         job.JobID,
		RunID:         job.RunID,
		Repo:          job.Repo,
		InstanceType:  job.InstanceType,
		Pool:          job.Pool,
		Spot:          false,
		RetryCount:    job.RetryCount + 1,
		ForceOnDemand: true,
	}
}

// RequeueDeps bundles the clients the operator-triggered requeue needs.
type RequeueDeps struct {
	// Scan reads and conditionally flips job records.
	Scan OrphanScanAPI
	// EC2 checks instance existence (batched DescribeInstances).
	EC2 OrphanEC2API
	// TerminateEC2 terminates alive-but-dead-agent instances and cancels their spot
	// requests before requeue.
	TerminateEC2 EC2API
	// Requeuer re-enqueues the launch message onto the main queue.
	Requeuer JobRequeuer
	// JobsTable is the DynamoDB jobs table name.
	JobsTable string
	// Metrics is optional; when set, emits operator_requeue counters mirroring the
	// unconfirmed-runner watchdog. Nil-safe (no emit when unset). Never emits on a dry run.
	Metrics MetricsAPI
	// Log is optional; a default is used when nil.
	Log *logging.Logger
}

// RequeueOptions controls a single operator-triggered requeue sweep.
type RequeueOptions struct {
	// Threshold is the minimum job age to be considered hung.
	Threshold time.Duration
	// Statuses selects which job states are eligible. Pass launched only: this
	// terminates a live instance before requeue, which for a running (runner-
	// confirmed) job would kill real work in progress. The watchdog scans launched
	// only for the same reason; a runner that died mid-job is recovered by the
	// spot-interruption path and the orphan reaper.
	Statuses []db.JobStatus
	// DryRun reports candidates without terminating, sending, or mutating anything.
	DryRun bool
}

// RequeueResult summarizes a requeue sweep.
type RequeueResult struct {
	Candidates       int
	Requeued         int
	SkippedExhausted int
	JobIDs           []int64
}

// RequeueOutcome names what happened to one job's re-dispatch attempt. Refusals
// (exhausted, wrong status, lost race) are normal operation; the *Failed ones
// accompany a non-nil error.
type RequeueOutcome string

// Outcomes of a single job's re-dispatch attempt.
const (
	OutcomeRequeued        RequeueOutcome = "requeued"
	OutcomeWouldRequeue    RequeueOutcome = "would_requeue"
	OutcomeNotFound        RequeueOutcome = "not_found"
	OutcomeExhausted       RequeueOutcome = "exhausted"
	OutcomeNoRunID         RequeueOutcome = "no_run_id"
	OutcomeWrongStatus     RequeueOutcome = "wrong_status"
	OutcomeStatusUnknown   RequeueOutcome = "status_unknown"
	OutcomeLostRace        RequeueOutcome = "lost_race"
	OutcomeTerminateFailed RequeueOutcome = "terminate_failed"
	OutcomeMarkFailed      RequeueOutcome = "mark_failed"
	OutcomeSendFailed      RequeueOutcome = "send_failed"
)

// SingleRequeueResult reports one job's re-dispatch in enough detail for an
// operator to see what was done to it, not just whether it worked.
type SingleRequeueResult struct {
	JobID              int64
	Outcome            RequeueOutcome
	InstanceID         string
	InstanceTerminated bool
	// RetryCount is the count after a successful requeue, or the observed one
	// for any other outcome.
	RetryCount int
	// Status is the record's status as last read.
	Status string
}

// GetRequeueableJob reads a single job record with the same projection the sweep's
// scan uses, so the single-job action and the sweep act on identical inputs. A
// consistent read is required: the operator may click seconds after the state
// changed. Returns nil when no such record exists.
func GetRequeueableJob(ctx context.Context, scanAPI OrphanScanAPI, jobsTable string, jobID int64) (*RequeueableJob, error) {
	out, err := scanAPI.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:                aws.String(jobsTable),
		Key:                      map[string]types.AttributeValue{"job_id": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", jobID)}},
		ProjectionExpression:     aws.String("job_id, instance_id, run_id, repo, instance_type, #pool, retry_count, #status"),
		ExpressionAttributeNames: map[string]string{"#status": "status", "#pool": "pool"},
		ConsistentRead:           aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	return &RequeueableJob{
		JobID:        jobID,
		InstanceID:   avString(out.Item, "instance_id"),
		RunID:        avInt64(out.Item, "run_id"),
		Repo:         avString(out.Item, "repo"),
		InstanceType: avString(out.Item, "instance_type"),
		Pool:         avString(out.Item, "pool"),
		RetryCount:   int(avInt64(out.Item, "retry_count")),
		Status:       avString(out.Item, "status"),
	}, nil
}

// RequeueJob re-dispatches a fresh runner for one operator-chosen job. It runs the
// same steps as the sweep with the same guardrails — launched only, bounded by
// MaxRequeueRetries — minus the staleness threshold, which exists to stop a sweep
// acting on jobs that may still be starting up and has no meaning for a row an
// operator picked deliberately.
func RequeueJob(ctx context.Context, deps RequeueDeps, jobID int64) (SingleRequeueResult, error) {
	log := deps.Log
	if log == nil {
		log = logging.WithComponent(logging.LogTypeHousekeep, "requeue")
	}

	res := SingleRequeueResult{JobID: jobID}
	if deps.JobsTable == "" {
		return res, fmt.Errorf("jobs table not configured")
	}

	job, err := GetRequeueableJob(ctx, deps.Scan, deps.JobsTable, jobID)
	if err != nil {
		return res, fmt.Errorf("read job %d: %w", jobID, err)
	}
	if job == nil {
		res.Outcome = OutcomeNotFound
		return res, nil
	}

	jobCtx := logging.ContextWith(ctx,
		slog.Int64(logging.KeyJobID, job.JobID),
		slog.String(logging.KeyInstanceID, job.InstanceID))

	alive := job.InstanceID != "" && instanceStillExists(jobCtx, deps.EC2, job.InstanceID)
	return requeueCandidate(jobCtx, deps, *job, alive, []db.JobStatus{db.JobStatusLaunched}, false, log)
}

// RequeueHungJobs is the operator-triggered backstop to the unconfirmed-runner watchdog.
// It scans for hung jobs (launched past the threshold — see RequeueOptions.Statuses), and
// for each requeue-able one re-dispatches a fresh runner: terminating an alive-but-dead-agent
// instance first
// (so two runners never serve one job), sending a fresh launch message via the queue, then
// flipping the record to requeued under a conditional write. It is bounded by
// MaxRequeueRetries and never cancels, re-runs, or otherwise touches the GitHub job — the
// GitHub job stays queued and a new healthy instance picks it up.
func RequeueHungJobs(ctx context.Context, deps RequeueDeps, opts RequeueOptions) (RequeueResult, error) {
	log := deps.Log
	if log == nil {
		log = logging.WithComponent(logging.LogTypeHousekeep, "requeue")
	}

	var result RequeueResult
	if deps.JobsTable == "" {
		return result, fmt.Errorf("jobs table not configured")
	}

	candidates, err := FindRequeueableJobs(ctx, deps.Scan, deps.JobsTable, opts.Threshold, opts.Statuses)
	if err != nil {
		return result, fmt.Errorf("scan requeueable jobs: %w", err)
	}
	if len(candidates) == 0 {
		return result, nil
	}

	// Determine which recorded instances still exist. A candidate is requeue-able when
	// its instance is gone OR alive-but-dead-agent (still in a pre-work state past the
	// threshold). A live instance is terminated before requeue; a missing one needs no
	// termination. An EC2 error makes an instance "assumed alive" (safe default) so a
	// transient outage never causes a spurious requeue.
	checkList := make([]OrphanedJobCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.InstanceID != "" {
			checkList = append(checkList, OrphanedJobCandidate{JobID: c.JobID, InstanceID: c.InstanceID})
		}
	}
	alive := map[string]bool{}
	if len(checkList) > 0 {
		fallback := func(ctx context.Context, instanceID string) bool {
			return instanceStillExists(ctx, deps.EC2, instanceID)
		}
		alive = BatchCheckInstanceExistence(ctx, deps.EC2, checkList, fallback)
	}

	result.Candidates = len(candidates)
	for _, c := range candidates {
		jobCtx := logging.ContextWith(ctx,
			slog.Int64(logging.KeyJobID, c.JobID),
			slog.String(logging.KeyInstanceID, c.InstanceID))

		// Errors are already logged inside; one job's failure never aborts the sweep.
		outcome, _ := requeueCandidate(jobCtx, deps, c, alive[c.InstanceID], opts.Statuses, opts.DryRun, log)
		switch outcome.Outcome {
		case OutcomeRequeued:
			result.Requeued++
			result.JobIDs = append(result.JobIDs, c.JobID)
		case OutcomeWouldRequeue:
			result.JobIDs = append(result.JobIDs, c.JobID)
		case OutcomeExhausted:
			result.SkippedExhausted++
		}
	}

	return result, nil
}

// requeueCandidate re-dispatches one job. It is the single implementation shared by
// the sweep and the operator's per-job action, so the two can never drift apart on
// the ordering that makes a requeue safe: re-read the status, terminate an
// alive-but-dead-agent instance, flip the record under a condition, then send.
func requeueCandidate(ctx context.Context, deps RequeueDeps, c RequeueableJob, instanceAlive bool, statuses []db.JobStatus, dryRun bool, log *logging.Logger) (SingleRequeueResult, error) {
	res := SingleRequeueResult{
		JobID:      c.JobID,
		InstanceID: c.InstanceID,
		RetryCount: c.RetryCount,
		Status:     c.Status,
	}

	if c.RetryCount >= MaxRequeueRetries {
		res.Outcome = OutcomeExhausted
		log.Warn(ctx, "requeue skipped: retries exhausted", slog.Int("retry_count", c.RetryCount))
		if !dryRun && deps.Metrics != nil {
			_ = deps.Metrics.PublishSchedulingFailure(ctx, requeueReasonOperator)
		}
		return res, nil
	}
	if c.RunID == 0 {
		res.Outcome = OutcomeNoRunID
		log.Warn(ctx, "requeue skipped: job has no run_id")
		return res, nil
	}

	// The scan snapshot can be minutes old; a runner that confirmed since then is
	// executing a job on this instance. Re-read before the irreversible terminate
	// below — the guarded status flip protects the record but cannot undo a kill.
	// Applied on dry runs too so the preview reports what a real sweep would act on.
	current, err := currentJobStatus(ctx, deps.Scan, deps.JobsTable, c.JobID)
	if err != nil {
		res.Outcome = OutcomeStatusUnknown
		log.Warn(ctx, "requeue skipped: pre-flight status read failed", slog.String("error", err.Error()))
		return res, err
	}
	if current == "" {
		res.Outcome = OutcomeNotFound
		return res, nil
	}
	res.Status = current
	if !statusIn(current, statuses) {
		res.Outcome = OutcomeWrongStatus
		log.Info(ctx, "requeue skipped: job advanced since the scan", slog.String("status", current))
		return res, nil
	}

	if dryRun {
		res.Outcome = OutcomeWouldRequeue
		return res, nil
	}

	if c.InstanceID != "" && instanceAlive {
		if termErr := terminateDeadAgentInstance(ctx, deps, c.InstanceID, log); termErr != nil {
			res.Outcome = OutcomeTerminateFailed
			log.Error(ctx, "requeue terminate failed", slog.String("error", termErr.Error()))
			return res, termErr
		}
		res.InstanceTerminated = true
	}

	// Flip before sending: ClaimJob rejects a record still reading launched as
	// already-claimed, so a worker that receives the message before this write
	// lands drops it and the job is never re-dispatched. The send is gated on OUR
	// write landing — a concurrent sweep whose flip won owns the dispatch, and
	// sending on its state would let its rollback strand a message we put in flight.
	flipped, err := markRequeued(ctx, deps.Scan, deps.JobsTable, c.JobID)
	if err != nil {
		res.Outcome = OutcomeMarkFailed
		log.Error(ctx, "requeue mark failed", slog.String("error", err.Error()))
		return res, err
	}
	if !flipped {
		res.Outcome = OutcomeLostRace
		log.Info(ctx, "requeue skipped: another sweep owns this job's re-dispatch")
		return res, nil
	}

	if err := deps.Requeuer.SendMessage(ctx, BuildRequeueMessage(c)); err != nil {
		log.Error(ctx, "requeue send failed", slog.String("error", err.Error()))
		// Only launched records are scanned, so a record left in requeued with no
		// delivered message would be invisible to every future sweep. Undo the
		// flip so the job is a candidate again.
		rollbackRequeueFlip(ctx, deps.Scan, deps.JobsTable, c.JobID, log)
		res.Outcome = OutcomeSendFailed
		return res, err
	}

	res.Outcome = OutcomeRequeued
	res.RetryCount = c.RetryCount + 1
	if deps.Metrics != nil {
		_ = deps.Metrics.PublishJobRequeued(ctx, requeueReasonOperator)
	}
	log.Info(ctx, "hung job requeued", slog.Int("retry_count", res.RetryCount))
	return res, nil
}

// jobHasStatus reports whether a job is still in one of the given statuses.
// Unknown state (read error or missing row) counts as no, so a caller that only
// needs a yes/no never acts on a job whose state it could not confirm.
func jobHasStatus(ctx context.Context, scanAPI OrphanScanAPI, jobsTable string, jobID int64, statuses []db.JobStatus, log *logging.Logger) bool {
	current, err := currentJobStatus(ctx, scanAPI, jobsTable, jobID)
	if err != nil {
		log.Warn(ctx, "requeue skipped: pre-flight status read failed", slog.String("error", err.Error()))
		return false
	}
	if current == "" {
		return false
	}
	if !statusIn(current, statuses) {
		log.Info(ctx, "requeue skipped: job advanced since the scan", slog.String("status", current))
		return false
	}
	return true
}

func statusIn(status string, statuses []db.JobStatus) bool {
	for _, s := range statuses {
		if status == string(s) {
			return true
		}
	}
	return false
}

// terminateDeadAgentInstance cancels any persistent spot request and terminates an
// instance whose runner never came up, so a fresh runner does not contend with it.
// Spot-request cancellation is best-effort: a failure there does not block termination.
func terminateDeadAgentInstance(ctx context.Context, deps RequeueDeps, instanceID string, log *logging.Logger) error {
	CancelSpotRequestForInstance(ctx, deps.TerminateEC2, instanceID, log)
	_, err := deps.TerminateEC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	return err
}

// CancelSpotRequestForInstance cancels the persistent spot request backing an instance
// (if any) so a terminated instance is not resurrected. Best-effort: errors are logged
// and swallowed. Exported so every terminate path — including the admin API's manual
// termination — cancels the request before killing the instance.
func CancelSpotRequestForInstance(ctx context.Context, ec2Client EC2API, instanceID string, log *logging.Logger) {
	out, err := ec2Client.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("instance-id"), Values: []string{instanceID}},
			{Name: aws.String("state"), Values: []string{"open", "active", "disabled"}},
		},
	})
	if err != nil {
		log.Warn(ctx, "describe spot request for requeue failed", slog.String("error", err.Error()))
		return
	}
	var ids []string
	for _, req := range out.SpotInstanceRequests {
		if req.SpotInstanceRequestId != nil {
			ids = append(ids, *req.SpotInstanceRequestId)
		}
	}
	if len(ids) == 0 {
		return
	}
	if _, err := ec2Client.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: ids,
	}); err != nil {
		log.Warn(ctx, "cancel spot request for requeue failed", slog.String("error", err.Error()))
	}
}

// instanceStillExists reports whether a single instance exists and is non-terminated.
// On an API error it assumes the instance exists (safe default — we never requeue on the
// strength of a transient EC2 failure).
func instanceStillExists(ctx context.Context, ec2Client OrphanEC2API, instanceID string) bool {
	output, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		// Any error short of a definitive "instance ID invalid/not found" assumes
		// the instance exists, so we never requeue on the strength of an EC2 outage.
		return !instanceIDGone(err)
	}
	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
			if instance.State != nil && instance.State.Name != ec2types.InstanceStateNameTerminated {
				return true
			}
		}
	}
	return false
}

// isConditionalCheckFailed reports whether err is a DynamoDB conditional-check failure.
func isConditionalCheckFailed(err error) bool {
	var condErr *types.ConditionalCheckFailedException
	return errors.As(err, &condErr)
}

// rollbackRequeueFlip restores a record to launched after its requeue message
// could not be sent, conditioned on the record still being the requeued one this
// sweep wrote (a concurrent claim that advanced it owns the state instead).
//
// A failure here is not recoverable automatically: no sweep scans requeued
// records, so the job stays invisible until someone repairs it by hand. It takes
// two consecutive DynamoDB failures to get there, and the error log is the only
// signal, hence the severity.
func rollbackRequeueFlip(ctx context.Context, scanAPI OrphanScanAPI, jobsTable string, jobID int64, log *logging.Logger) {
	_, err := scanAPI.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(jobsTable),
		Key: map[string]types.AttributeValue{
			"job_id": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", jobID)},
		},
		UpdateExpression:         aws.String("SET #status = :launched REMOVE requeued_at"),
		ConditionExpression:      aws.String("#status = :requeued"),
		ExpressionAttributeNames: map[string]string{"#status": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":launched": &types.AttributeValueMemberS{Value: string(db.JobStatusLaunched)},
			":requeued": &types.AttributeValueMemberS{Value: string(db.JobStatusRequeued)},
		},
	})
	if err != nil && !isConditionalCheckFailed(err) {
		log.Error(ctx, "requeue flip rollback failed; job left for the orphan sweep",
			slog.String("error", err.Error()))
	}
}

// currentJobStatus re-reads a candidate's status with a consistent read. An empty
// status with no error means the record is gone; callers must treat a read error
// as unknown state and never act on it.
func currentJobStatus(ctx context.Context, scanAPI OrphanScanAPI, jobsTable string, jobID int64) (string, error) {
	out, err := scanAPI.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(jobsTable),
		Key: map[string]types.AttributeValue{
			"job_id": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", jobID)},
		},
		ProjectionExpression:     aws.String("#status"),
		ExpressionAttributeNames: map[string]string{"#status": "status"},
		ConsistentRead:           aws.Bool(true),
	})
	if err != nil {
		return "", err
	}
	if out.Item == nil {
		return "", nil
	}
	return avString(out.Item, "status"), nil
}

// markRequeued flips a job record to requeued under a conditional write guarded on the
// record still being launched, so a job that advanced (completed, or confirmed its runner
// into running between the scan and now) is never clobbered. It reports whether THIS call
// performed the write: a ConditionalCheckFailedException means another actor owns the
// record's state, which the caller must not mistake for its own success.
func markRequeued(ctx context.Context, scanAPI OrphanScanAPI, jobsTable string, jobID int64) (bool, error) {
	_, err := scanAPI.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(jobsTable),
		Key: map[string]types.AttributeValue{
			"job_id": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", jobID)},
		},
		UpdateExpression:         aws.String("SET #status = :requeued, requeued_at = :now"),
		ConditionExpression:      aws.String("#status = :launched"),
		ExpressionAttributeNames: map[string]string{"#status": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":requeued": &types.AttributeValueMemberS{Value: string(db.JobStatusRequeued)},
			":launched": &types.AttributeValueMemberS{Value: string(db.JobStatusLaunched)},
			":now":      &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
		},
	})
	if err != nil {
		if isConditionalCheckFailed(err) {
			return false, nil
		}
		return false, fmt.Errorf("mark requeued: %w", err)
	}
	return true, nil
}
