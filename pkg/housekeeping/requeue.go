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
	"github.com/aws/smithy-go"
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

		if c.RetryCount >= MaxRequeueRetries {
			result.SkippedExhausted++
			log.Warn(jobCtx, "requeue skipped: retries exhausted", slog.Int("retry_count", c.RetryCount))
			if !opts.DryRun && deps.Metrics != nil {
				_ = deps.Metrics.PublishSchedulingFailure(jobCtx, requeueReasonOperator)
			}
			continue
		}
		if c.RunID == 0 {
			log.Warn(jobCtx, "requeue skipped: job has no run_id")
			continue
		}

		if opts.DryRun {
			result.JobIDs = append(result.JobIDs, c.JobID)
			continue
		}

		if c.InstanceID != "" && alive[c.InstanceID] {
			if err := terminateDeadAgentInstance(jobCtx, deps, c.InstanceID, log); err != nil {
				log.Error(jobCtx, "requeue terminate failed", slog.String("error", err.Error()))
				continue
			}
		}

		if err := deps.Requeuer.SendMessage(jobCtx, BuildRequeueMessage(c)); err != nil {
			log.Error(jobCtx, "requeue send failed", slog.String("error", err.Error()))
			continue
		}

		if err := markRequeued(jobCtx, deps.Scan, deps.JobsTable, c.JobID); err != nil {
			log.Error(jobCtx, "requeue mark failed", slog.String("error", err.Error()))
			// Message already sent; the next sweep re-sends (SQS dedupes on job_id+retry).
			continue
		}

		result.Requeued++
		result.JobIDs = append(result.JobIDs, c.JobID)
		if deps.Metrics != nil {
			_ = deps.Metrics.PublishJobRequeued(jobCtx, requeueReasonOperator)
		}
		log.Info(jobCtx, "hung job requeued", slog.Int("retry_count", c.RetryCount+1))
	}

	return result, nil
}

// terminateDeadAgentInstance cancels any persistent spot request and terminates an
// instance whose runner never came up, so a fresh runner does not contend with it.
// Spot-request cancellation is best-effort: a failure there does not block termination.
func terminateDeadAgentInstance(ctx context.Context, deps RequeueDeps, instanceID string, log *logging.Logger) error {
	cancelSpotRequestForInstance(ctx, deps.TerminateEC2, instanceID, log)
	_, err := deps.TerminateEC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	return err
}

// cancelSpotRequestForInstance cancels the persistent spot request backing an instance
// (if any) so a terminated instance is not resurrected. Best-effort: errors are logged
// and swallowed.
func cancelSpotRequestForInstance(ctx context.Context, ec2Client EC2API, instanceID string, log *logging.Logger) {
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
		// A definitive "instance ID invalid/not found" means gone; any other
		// (transient) error assumes the instance exists so we never requeue on the
		// strength of an EC2 outage. Prefer the typed smithy code, falling back to a
		// message match for errors that don't surface a code.
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			code := apiErr.ErrorCode()
			return code != "InvalidInstanceID.NotFound" && code != "InvalidInstanceID.Malformed"
		}
		return !strings.Contains(err.Error(), "InvalidInstanceID")
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

// markRequeued flips a job record to requeued under a conditional write guarded on the
// record still being launched, so a job that advanced (completed, or confirmed its runner
// into running between the scan and now) is never clobbered. A ConditionalCheckFailedException
// means the job moved on and is treated as success.
func markRequeued(ctx context.Context, scanAPI OrphanScanAPI, jobsTable string, jobID int64) error {
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
			return nil
		}
		return fmt.Errorf("mark requeued: %w", err)
	}
	return nil
}
