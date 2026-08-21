package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// OrphanScanAPI defines the DynamoDB operations needed for orphaned job scanning.
type OrphanScanAPI interface {
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

// OrphanEC2API defines the EC2 operations needed for orphaned job instance checking.
type OrphanEC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// OrphanedJobCandidate holds job info for orphaned job detection.
type OrphanedJobCandidate struct {
	JobID      int64
	InstanceID string
	Status     string
}

const (
	orphanInstanceCheckBatchSize = 100
)

// ScanOption bounds one sweep's scan. Options exist so the scheduled sweeps,
// which must keep draining the whole table, can go on calling these functions
// unchanged while the admin endpoints ask for a bounded batch.
type ScanOption func(*scanOptions)

type scanOptions struct {
	// maxItems caps how many candidates one call collects. Zero means no cap.
	maxItems int
}

// WithMaxItems caps a scan at n candidates and reports whether more remain, so an
// operator-triggered sweep does not grow with the table. A non-positive n is no cap.
//
// The cap is applied to matched candidates rather than through ScanInput.Limit,
// which bounds items *read* and so cannot express "stop after n matches" behind a
// filter expression.
func WithMaxItems(n int) ScanOption {
	return func(o *scanOptions) {
		if n > 0 {
			o.maxItems = n
		}
	}
}

func newScanOptions(opts []ScanOption) scanOptions {
	var o scanOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// FindOrphanedJobCandidates scans DynamoDB for jobs in "running", "claiming",
// "launched", or "requeued" status older than the given threshold. Jobs in "running" or
// "launched" status without an instance_id are excluded (they need an instance to verify
// against). Jobs in "claiming" status without an instance_id are included (instance
// creation failed).
//
// A requeued record is aged on requeued_at rather than created_at. Requeued is meant to
// be transient — the record sits there only until a worker claims the re-dispatch — so
// ageing it on created_at would retire a job re-dispatched seconds ago merely because its
// first attempt was hours earlier. Its recorded instance is always gone (the requeue
// terminates it), which makes requeued_at the only thing separating a live re-dispatch
// from one that never landed.
//
// Requeued has to be here because nothing else can see it: the stale-jobs sweep scans
// running/claiming, the requeue sweep scans launched, and the old-jobs GC keys off
// completed_at, which a requeued record never gets. A re-dispatch that is never claimed
// strands its record permanently otherwise.
// It returns truncated=true when a cap from WithMaxItems stopped it with candidates
// still unread, which is the signal an operator-triggered sweep uses to ask for
// another batch.
func FindOrphanedJobCandidates(ctx context.Context, dynamoClient OrphanScanAPI, jobsTableName string, threshold time.Duration, opts ...ScanOption) ([]OrphanedJobCandidate, bool, error) {
	scanOpts := newScanOptions(opts)
	cutoffTime := time.Now().Add(-threshold).Format(time.RFC3339)

	input := &dynamodb.ScanInput{
		TableName: aws.String(jobsTableName),
		FilterExpression: aws.String("((#status = :running OR #status = :claiming OR #status = :launched) AND created_at < :cutoff)" +
			" OR (#status = :requeued AND requeued_at < :cutoff)"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":running":  &types.AttributeValueMemberS{Value: string(db.JobStatusRunning)},
			":claiming": &types.AttributeValueMemberS{Value: string(db.JobStatusClaiming)},
			":launched": &types.AttributeValueMemberS{Value: string(db.JobStatusLaunched)},
			":requeued": &types.AttributeValueMemberS{Value: string(db.JobStatusRequeued)},
			":cutoff":   &types.AttributeValueMemberS{Value: cutoffTime},
		},
		ProjectionExpression: aws.String("job_id, instance_id, #status"),
	}

	var candidates []OrphanedJobCandidate
	var lastEvaluatedKey map[string]types.AttributeValue

	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, false, err
		}

		for i, item := range output.Items {
			var jobID int64
			var instanceID string
			var status string

			if v, ok := item["job_id"].(*types.AttributeValueMemberN); ok {
				parsed, err := strconv.ParseInt(v.Value, 10, 64)
				if err != nil {
					continue
				}
				jobID = parsed
			}
			if v, ok := item["instance_id"].(*types.AttributeValueMemberS); ok {
				instanceID = v.Value
			}
			if v, ok := item["status"].(*types.AttributeValueMemberS); ok {
				status = v.Value
			}

			if jobID == 0 {
				continue
			}

			if (status == string(db.JobStatusRunning) || status == string(db.JobStatusLaunched)) && instanceID == "" {
				continue
			}

			candidates = append(candidates, OrphanedJobCandidate{
				JobID:      jobID,
				InstanceID: instanceID,
				Status:     status,
			})

			if scanOpts.maxItems > 0 && len(candidates) >= scanOpts.maxItems {
				// More candidates remain if this page has unread items left or
				// another page follows. Anything less would end the caller's
				// drain with work still stranded.
				more := i+1 < len(output.Items) || output.LastEvaluatedKey != nil
				return candidates, more, nil
			}
		}

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	return candidates, false, nil
}

// InstanceExistsChecker determines whether a single EC2 instance exists.
// Implementations may differ in error handling behavior (e.g., smithy error
// type assertion vs string matching, termination grace periods).
type InstanceExistsChecker func(ctx context.Context, instanceID string) bool

// BatchCheckInstanceExistence checks multiple instances in batches via DescribeInstances
// and returns a map of instance IDs that still exist (non-terminated).
// When a batch call fails, it falls back to checking each instance individually
// using the provided fallback function.
func BatchCheckInstanceExistence(ctx context.Context, ec2Client OrphanEC2API, candidates []OrphanedJobCandidate, fallback InstanceExistsChecker) map[string]bool {
	instanceSet := make(map[string]struct{})
	for _, c := range candidates {
		instanceSet[c.InstanceID] = struct{}{}
	}

	instanceIDs := make([]string, 0, len(instanceSet))
	for id := range instanceSet {
		instanceIDs = append(instanceIDs, id)
	}

	existing := make(map[string]bool)

	for i := 0; i < len(instanceIDs); i += orphanInstanceCheckBatchSize {
		end := i + orphanInstanceCheckBatchSize
		if end > len(instanceIDs) {
			end = len(instanceIDs)
		}
		batch := instanceIDs[i:end]

		output, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: batch,
		})
		if err != nil {
			for _, id := range batch {
				if fallback(ctx, id) {
					existing[id] = true
				}
			}
			continue
		}

		for _, reservation := range output.Reservations {
			for _, instance := range reservation.Instances {
				if instance.InstanceId == nil {
					continue
				}
				if instance.State != nil && instance.State.Name != ec2types.InstanceStateNameTerminated {
					existing[*instance.InstanceId] = true
				}
			}
		}
	}

	return existing
}

// SeparateOrphanedJobs splits candidates into confirmed orphans (no instance ID)
// and those requiring EC2 verification (have instance ID).
func SeparateOrphanedJobs(candidates []OrphanedJobCandidate) (withInstance, withoutInstance []OrphanedJobCandidate) {
	for _, c := range candidates {
		if c.InstanceID == "" {
			withoutInstance = append(withoutInstance, c)
		} else {
			withInstance = append(withInstance, c)
		}
	}
	return withInstance, withoutInstance
}

// ReconcileOutcome names what happened to one job's reconcile attempt.
type ReconcileOutcome string

// Outcomes of a single job's reconcile attempt.
const (
	ReconcileOrphaned      ReconcileOutcome = "orphaned"
	ReconcileNotFound      ReconcileOutcome = "not_found"
	ReconcileWrongStatus   ReconcileOutcome = "wrong_status"
	ReconcileInstanceAlive ReconcileOutcome = "instance_alive"
	ReconcileNoInstance    ReconcileOutcome = "no_instance"
	ReconcileLostRace      ReconcileOutcome = "lost_race"
)

// ReconcileResult reports one job's reconcile.
type ReconcileResult struct {
	JobID      int64
	Outcome    ReconcileOutcome
	InstanceID string
	Status     string
}

// reconcilableStatuses are the states a job can be stuck in with a dead instance — the
// same set FindOrphanedJobCandidates scans, and the only ones MarkJobOrphaned retires
// from.
var reconcilableStatuses = []db.JobStatus{
	db.JobStatusRunning,
	db.JobStatusClaiming,
	db.JobStatusLaunched,
	db.JobStatusRequeued,
}

// ReconcileJob retires a single job whose instance is gone, the targeted form of the
// orphaned-jobs sweep. It refuses while the instance still exists: a job record is the
// only thing tying a live runner to its work, and marking it orphaned would hide that
// work rather than clean it up. An EC2 error counts as alive (see instanceStillExists),
// so a transient outage never retires a live job.
func ReconcileJob(ctx context.Context, scanAPI OrphanScanAPI, ec2Client OrphanEC2API, jobsTable string, jobID int64) (ReconcileResult, error) {
	res := ReconcileResult{JobID: jobID}
	if jobsTable == "" {
		return res, fmt.Errorf("jobs table not configured")
	}

	job, err := GetRequeueableJob(ctx, scanAPI, jobsTable, jobID)
	if err != nil {
		return res, fmt.Errorf("read job %d: %w", jobID, err)
	}
	if job == nil {
		res.Outcome = ReconcileNotFound
		return res, nil
	}

	res.InstanceID = job.InstanceID
	res.Status = job.Status
	if !statusIn(job.Status, reconcilableStatuses) {
		res.Outcome = ReconcileWrongStatus
		return res, nil
	}
	// A running/launched record with no instance_id cannot be verified against EC2 at
	// all, so retiring it would be a guess. FindOrphanedJobCandidates drops the same
	// shape for the same reason; only claiming (where instance creation itself failed)
	// is orphanable without an instance.
	if job.InstanceID == "" {
		if job.Status != string(db.JobStatusClaiming) {
			res.Outcome = ReconcileNoInstance
			return res, nil
		}
	} else if instanceStillExists(ctx, ec2Client, job.InstanceID) {
		res.Outcome = ReconcileInstanceAlive
		return res, nil
	}

	marked, err := MarkJobOrphaned(ctx, scanAPI, jobsTable, jobID, job.Status)
	if err != nil {
		return res, fmt.Errorf("mark job %d orphaned: %w", jobID, err)
	}
	if !marked {
		res.Outcome = ReconcileLostRace
		return res, nil
	}
	res.Outcome = ReconcileOrphaned
	return res, nil
}

// MarkJobOrphaned updates a job's status to "orphaned" with a completed_at timestamp,
// under a conditional write pinned to observedStatus — the status the caller read when it
// decided the job was orphaned. Candidates are selected by a scan and written back later,
// and a requeued record is re-claimable by design (see db.evaluateClaim), so a stale
// candidate can go live in between; pinning the write to what was observed is what stops
// the sweep from retiring a job that has since been claimed.
//
// The write also REMOVEs pool. A cold-start job records no pool, and records
// written before #227 stored that as an empty string rather than omitting the
// attribute — which is unrepresentable in the pool-status GSI (pool is its hash
// key), so DynamoDB rejects any update touching status, the index's range key.
// The sweep could therefore never retire those records, and because it is
// completed_at that makes a record collectable, the 7-day GC could not either.
// REMOVE on an absent attribute is a no-op, so this is safe for healthy records.
//
// It reports whether THIS call performed the write. False means the record moved on
// first, which a caller counting retirements must not present as its own success. An
// observedStatus outside reconcilableStatuses is refused outright: the caller read a
// settled record, and orphaning it would overwrite a real outcome.
func MarkJobOrphaned(ctx context.Context, dynamoClient OrphanScanAPI, jobsTableName string, jobID int64, observedStatus string) (bool, error) {
	if !statusIn(observedStatus, reconcilableStatuses) {
		return false, fmt.Errorf("refusing to orphan job %d observed in status %q", jobID, observedStatus)
	}

	now := time.Now().Format(time.RFC3339)

	_, err := dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(jobsTableName),
		Key: map[string]types.AttributeValue{
			"job_id": &types.AttributeValueMemberN{Value: strconv.FormatInt(jobID, 10)},
		},
		UpdateExpression: aws.String("SET #status = :orphaned, completed_at = :now REMOVE #pool"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
			"#pool":   "pool",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":orphaned": &types.AttributeValueMemberS{Value: string(db.JobStatusOrphaned)},
			":now":      &types.AttributeValueMemberS{Value: now},
			":observed": &types.AttributeValueMemberS{Value: observedStatus},
		},
		ConditionExpression: aws.String("#status = :observed"),
	})

	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return false, nil
		}
		return false, fmt.Errorf("failed to update job: %w", err)
	}
	return true, nil
}
