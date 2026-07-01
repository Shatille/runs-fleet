package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

// ErrJobAlreadyClaimed is returned when attempting to claim a job that is already being processed.
var ErrJobAlreadyClaimed = errors.New("job already claimed")

// ErrJobClaimExhausted is returned when a job's claim has been re-claimed
// claimMaxAttempts times without ever reaching the running state. The job is
// almost certainly unprovisionable; the caller must mark it terminal and stop
// redelivering rather than let the lease re-claim forever.
var ErrJobClaimExhausted = errors.New("job claim attempts exhausted")

// claimStaleThreshold is the age past which a record stuck in the claiming
// state is treated as a dead lease and may be re-claimed. A worker that dies
// between ClaimJob and SaveJob (SIGTERM, OOM, wedged AWS call) would otherwise
// leak a permanent claiming stub that poisons every redelivery.
//
// It sits above MessageProcessTimeout (90s) so a live worker mid-provision is
// never preempted, and below the main queue's SQS visibility timeout (120s) so
// the first redelivery of the message finds the lease already expired and
// re-claims it, instead of seeing a fresh claim and dropping the message.
const claimStaleThreshold = 100 * time.Second

// claimMaxAttempts caps how many times a single job may be (re-)claimed before
// it is declared terminally unprovisionable. The first claim is attempt 1; each
// stale re-claim increments the count. Kept in line with the worker's
// maxJobRetries on-demand fallback budget so a job fails hard rather than loops.
const claimMaxAttempts = 3

// jobRecord represents a job stored in DynamoDB.
// Primary key is instance_id (one job per instance, ephemeral runners).
type jobRecord struct {
	InstanceID     string `dynamodbav:"instance_id,omitempty"`
	JobID          int64  `dynamodbav:"job_id"`
	RunID          int64  `dynamodbav:"run_id"`
	Repo           string `dynamodbav:"repo"`
	InstanceType   string `dynamodbav:"instance_type"`
	Pool           string `dynamodbav:"pool,omitempty"`
	Spot           bool   `dynamodbav:"spot"`
	RetryCount     int    `dynamodbav:"retry_count"`
	WarmPoolHit    bool   `dynamodbav:"warm_pool_hit"`
	// omitempty so an (unexpected) empty status is dropped rather than written as
	// S:"" — the pool-status GSI is keyed on status, and DynamoDB rejects a
	// PutItem with an empty-string index-key attribute. Mirrors instance_id (#276)
	// and pool (#227). All writers set a non-empty status today; this guards the
	// GSI against a future one that doesn't.
	Status         string `dynamodbav:"status,omitempty"`
	CreatedAt      string `dynamodbav:"created_at"`
	SpotRequestID  string `dynamodbav:"spot_request_id,omitempty"`
	PersistentSpot bool   `dynamodbav:"persistent_spot,omitempty"`
	TraceID        string `dynamodbav:"trace_id,omitempty"`
}

// JobRecord contains job information for storage.
type JobRecord struct {
	JobID          int64
	RunID          int64
	Repo           string
	InstanceID     string
	InstanceType   string
	Pool           string
	Spot           bool
	RetryCount     int
	WarmPoolHit    bool
	SpotRequestID  string
	PersistentSpot bool
	Traceparent    string
}

// JobHistoryEntry represents a job with timing information for auto-scaling calculations.
type JobHistoryEntry struct {
	JobID       int64
	Pool        string
	Status      JobStatus
	CreatedAt   time.Time
	CompletedAt time.Time // Zero value if still running
}

// SaveJob creates or updates a job record in DynamoDB.
// The job is stored with status "launched" (instance up, runner not yet confirmed)
// and can be queried by instance_id via the GSI.
func (c *Client) SaveJob(ctx context.Context, job *JobRecord) error {
	if job == nil {
		return fmt.Errorf("job record cannot be nil")
	}
	if job.JobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}
	if job.InstanceID == "" {
		return fmt.Errorf("instance ID cannot be empty")
	}

	if c.jobsTable == "" {
		return fmt.Errorf("jobs table not configured")
	}

	record := jobRecord{
		InstanceID:     job.InstanceID,
		JobID:          job.JobID,
		RunID:          job.RunID,
		Repo:           job.Repo,
		InstanceType:   job.InstanceType,
		Pool:           job.Pool,
		Spot:           job.Spot,
		RetryCount:     job.RetryCount,
		WarmPoolHit:    job.WarmPoolHit,
		Status:         string(JobStatusLaunched),
		CreatedAt:      time.Now().Format(time.RFC3339),
		SpotRequestID:  job.SpotRequestID,
		PersistentSpot: job.PersistentSpot,
		TraceID:        extractTraceID(job.Traceparent),
	}

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("failed to marshal job record: %w", err)
	}

	_, err = c.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.jobsTable),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("failed to save job record: %w", err)
	}

	return nil
}

// ClaimJob atomically claims a job for processing as a self-expiring lease.
//
// The claim persists the job's identity (runID, repo) so the record is never a
// detail-less stub: ExecuteStaleJobs can query GitHub for it, and a requeue can
// reconstruct the job. A claim is re-claimable when the prior record is in the
// requeued/terminating states, or when it is a stale claiming lease (a worker
// died between ClaimJob and SaveJob, leaving a claiming record older than
// claimStaleThreshold).
//
// Concurrency is compare-and-swap on the observed created_at: the existing
// record is read, then the write is conditioned on attribute_not_exists(job_id)
// OR created_at = :observed. Two workers re-claiming the same stale lease pin
// the same observed created_at, so exactly one PutItem succeeds and the loser
// gets ErrJobAlreadyClaimed — no double-claim.
//
// Re-claim is bounded: each stale re-claim increments retry_count, and once it
// reaches claimMaxAttempts the job is declared terminally unprovisionable and
// ErrJobClaimExhausted is returned instead of re-claiming forever.
//
// Returns ErrJobAlreadyClaimed if the job is already claimed by a live worker
// (fresh lease) or if a concurrent re-claim won the race.
func (c *Client) ClaimJob(ctx context.Context, jobID, runID int64, repo string) error {
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}

	if c.jobsTable == "" {
		return fmt.Errorf("jobs table not configured")
	}

	existing, err := c.getClaimRecord(ctx, jobID)
	if err != nil {
		return err
	}

	attempts, reclaim, err := c.evaluateClaim(existing)
	if err != nil {
		return err
	}

	return c.writeClaim(ctx, jobID, runID, repo, attempts, reclaim)
}

// claimState holds the fields of an existing job record relevant to re-claiming.
type claimState struct {
	status     string
	createdAt  string
	retryCount int
}

func (c *Client) getClaimRecord(ctx context.Context, jobID int64) (*claimState, error) {
	key, err := attributevalue.MarshalMap(map[string]int64{"job_id": jobID})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal key: %w", err)
	}

	output, err := c.dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.jobsTable),
		Key:       key,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to read existing claim: %w", err)
	}
	if output.Item == nil {
		return nil, nil
	}

	var record jobRecord
	if err := attributevalue.UnmarshalMap(output.Item, &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal existing claim: %w", err)
	}

	return &claimState{
		status:     record.Status,
		createdAt:  record.CreatedAt,
		retryCount: record.RetryCount,
	}, nil
}

// evaluateClaim decides whether a (possibly nil) existing record may be claimed.
// It returns the attempt count to persist on the new claim, the created_at to
// pin in the conditional write ("" when no record was observed), or an error
// (ErrJobAlreadyClaimed / ErrJobClaimExhausted) when the claim must be rejected.
func (c *Client) evaluateClaim(existing *claimState) (attempts int, observedCreatedAt string, err error) {
	if existing == nil {
		// Brand-new job: first claim, no record to compare-and-swap against.
		return 1, "", nil
	}

	switch JobStatus(existing.status) {
	case JobStatusRequeued, JobStatusTerminating:
		// Legitimate redelivery (spot interruption, requeue). Re-claim, preserving
		// the existing attempt count: these are not unprovisionable retries.
		return existing.retryCount, existing.createdAt, nil
	case JobStatusClaiming:
		if !c.claimIsStale(existing.createdAt) {
			// A live worker holds a fresh lease; do not preempt it.
			return 0, "", ErrJobAlreadyClaimed
		}
		if existing.retryCount >= claimMaxAttempts {
			return 0, "", ErrJobClaimExhausted
		}
		return existing.retryCount + 1, existing.createdAt, nil
	default:
		// running, completed, failed, error, orphaned, etc.: not re-claimable.
		return 0, "", ErrJobAlreadyClaimed
	}
}

// claimIsStale reports whether a claiming record's created_at is older than the
// lease threshold. An unparseable timestamp is treated as stale so a corrupt
// lease can never strand a job permanently.
func (c *Client) claimIsStale(createdAt string) bool {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return true
	}
	return time.Since(t) >= claimStaleThreshold
}

func (c *Client) writeClaim(ctx context.Context, jobID, runID int64, repo string, attempts int, observedCreatedAt string) error {
	record := jobRecord{
		JobID:      jobID,
		RunID:      runID,
		Repo:       repo,
		RetryCount: attempts,
		Status:     string(JobStatusClaiming),
		CreatedAt:  time.Now().Format(time.RFC3339),
	}

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("failed to marshal claim record: %w", err)
	}

	input := &dynamodb.PutItemInput{
		TableName: aws.String(c.jobsTable),
		Item:      item,
	}

	if observedCreatedAt == "" {
		// No record was observed: only claim if one still does not exist. Guards
		// against a concurrent claimer that wrote between our read and this write.
		input.ConditionExpression = aws.String("attribute_not_exists(job_id)")
	} else {
		// Compare-and-swap: claim only if the record still carries the created_at
		// we read. A concurrent re-claimer that already overwrote it changes
		// created_at, so its competitor's condition fails.
		input.ConditionExpression = aws.String("attribute_not_exists(job_id) OR created_at = :observed_created_at")
		input.ExpressionAttributeValues = map[string]types.AttributeValue{
			":observed_created_at": &types.AttributeValueMemberS{Value: observedCreatedAt},
		}
	}

	if _, err := c.dynamoClient.PutItem(ctx, input); err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrJobAlreadyClaimed
		}
		return fmt.Errorf("failed to claim job: %w", err)
	}

	return nil
}

// FailExhaustedClaim marks an exhausted claim terminal (status "error").
//
// The write is conditioned on the record still being in the claiming state, so
// a slow prior worker whose SaveJob lands (status "running") concurrently is
// not clobbered — matching the guarded transition used by stale-job recovery. A
// ConditionalCheckFailedException means the job advanced under us and is left
// untouched; it is treated as success.
func (c *Client) FailExhaustedClaim(ctx context.Context, jobID int64) error {
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}
	if c.jobsTable == "" {
		return fmt.Errorf("jobs table not configured")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{"job_id": jobID})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(c.jobsTable),
		Key:                 key,
		UpdateExpression:    aws.String("SET #status = :error, completed_at = :completed_at"),
		ConditionExpression: aws.String("#status = :claiming"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":error":        &types.AttributeValueMemberS{Value: string(JobStatusError)},
			":claiming":     &types.AttributeValueMemberS{Value: string(JobStatusClaiming)},
			":completed_at": &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil
		}
		return fmt.Errorf("failed to mark claim exhausted: %w", err)
	}

	return nil
}

// MarkJobStarted transitions a launched job to "running" once the agent reports
// the runner has registered and begun executing (the SendJobStarted signal). It
// returns the updated job record (via ReturnValueAllNew, like MarkJobComplete) so
// the caller can emit a confirmation metric without a second read.
//
// The write is conditioned on the record still being in the launched state, so a
// late "started" message that arrives after the job already completed or was
// recovered by the watchdog is a no-op: it returns (nil, nil) rather than
// resurrecting a terminal record. A non-nil return means the transition applied.
func (c *Client) MarkJobStarted(ctx context.Context, jobID int64, startedAt time.Time) (*events.JobInfo, error) {
	if jobID == 0 {
		return nil, fmt.Errorf("job ID cannot be zero")
	}
	if c.jobsTable == "" {
		return nil, fmt.Errorf("jobs table not configured")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{"job_id": jobID})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal key: %w", err)
	}

	output, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(c.jobsTable),
		Key:                 key,
		UpdateExpression:    aws.String("SET #status = :running, started_at = :started_at"),
		ConditionExpression: aws.String("#status = :launched"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":running":    &types.AttributeValueMemberS{Value: string(JobStatusRunning)},
			":launched":   &types.AttributeValueMemberS{Value: string(JobStatusLaunched)},
			":started_at": &types.AttributeValueMemberS{Value: startedAt.Format(time.RFC3339)},
		},
		ReturnValues: types.ReturnValueAllNew,
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to mark job started: %w", err)
	}

	return unmarshalJobInfo(output.Attributes)
}

// DeleteJobClaim removes a job claim record. Used for cleanup on processing failure.
func (c *Client) DeleteJobClaim(ctx context.Context, jobID int64) error {
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}

	if c.jobsTable == "" {
		return fmt.Errorf("jobs table not configured")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": jobID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	_, err = c.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.jobsTable),
		Key:       key,
	})
	if err != nil {
		return fmt.Errorf("failed to delete job claim: %w", err)
	}

	return nil
}

// MarkJobComplete marks a job as complete in DynamoDB with exit status and
// returns the updated job record. Uses job_id as primary key (Number type in
// DynamoDB). ReturnValueAllNew makes the write echo back the full post-update
// item, so callers can read identity fields (run_id, repo) off the record
// without a second GetItem.
func (c *Client) MarkJobComplete(ctx context.Context, jobID int64, status string, exitCode, duration int) (*events.JobInfo, error) {
	if jobID == 0 {
		return nil, fmt.Errorf("job ID cannot be zero")
	}
	if status == "" {
		return nil, fmt.Errorf("status cannot be empty")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": jobID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal key: %w", err)
	}

	update := "SET #status = :status, exit_code = :exit_code, duration_seconds = :duration, completed_at = :completed_at"
	exprNames := map[string]string{
		"#status": "status",
	}
	exprValues, err := attributevalue.MarshalMap(map[string]interface{}{
		":status":       status,
		":exit_code":    exitCode,
		":duration":     duration,
		":completed_at": time.Now().Format(time.RFC3339),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal values: %w", err)
	}

	output, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(c.jobsTable),
		Key:                       key,
		UpdateExpression:          aws.String(update),
		ExpressionAttributeNames:  exprNames,
		ExpressionAttributeValues: exprValues,
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update job: %w", err)
	}

	return unmarshalJobInfo(output.Attributes)
}

// UpdateJobMetrics updates job timing metrics in DynamoDB.
// Uses job_id as primary key (Number type in DynamoDB).
func (c *Client) UpdateJobMetrics(ctx context.Context, jobID int64, startedAt, completedAt time.Time) error {
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": jobID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	update := "SET started_at = :started_at, completed_at = :completed_at"
	exprValues, err := attributevalue.MarshalMap(map[string]string{
		":started_at":   startedAt.Format(time.RFC3339),
		":completed_at": completedAt.Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("failed to marshal values: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(c.jobsTable),
		Key:                       key,
		UpdateExpression:          aws.String(update),
		ExpressionAttributeValues: exprValues,
	})
	if err != nil {
		return fmt.Errorf("failed to update job metrics: %w", err)
	}

	return nil
}

// MarkInstanceTerminating marks jobs on an instance as terminating in DynamoDB.
// Updates any running jobs for this instance to status "terminating".
func (c *Client) MarkInstanceTerminating(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("instance ID cannot be empty")
	}

	if c.jobsTable == "" {
		return fmt.Errorf("jobs table not configured")
	}

	job, err := c.GetJobByInstance(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get job for instance: %w", err)
	}
	if job == nil {
		return nil
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": job.JobID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(c.jobsTable),
		Key:              key,
		UpdateExpression: aws.String("SET #status = :status"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":status": &types.AttributeValueMemberS{Value: string(JobStatusTerminating)},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to mark job as terminating: %w", err)
	}

	return nil
}

// GetJobByInstance retrieves job information for a given instance ID from DynamoDB.
//
// When jobsInstanceIDGSI is configured, uses Query on the GSI for O(1) lookup.
// Falls back to Scan if the GSI is not configured or if the Query fails.
func (c *Client) GetJobByInstance(ctx context.Context, instanceID string) (*events.JobInfo, error) {
	if instanceID == "" {
		return nil, fmt.Errorf("instance ID cannot be empty")
	}

	if c.jobsTable == "" {
		return nil, fmt.Errorf("jobs table not configured")
	}

	if c.jobsInstanceIDGSI != "" {
		info, err := c.getJobByInstanceViaGSI(ctx, instanceID)
		if err == nil {
			return info, nil
		}
		if !isGSIValidationError(err) {
			return nil, err
		}
		dbLog.Warn(ctx, "GSI query failed, falling back to scan",
			"gsi", c.jobsInstanceIDGSI,
			"error", err,
		)
	}

	return c.getJobByInstanceViaScan(ctx, instanceID)
}

func (c *Client) getJobByInstanceViaGSI(ctx context.Context, instanceID string) (*events.JobInfo, error) {
	input := &dynamodb.QueryInput{
		TableName:              aws.String(c.jobsTable),
		IndexName:              aws.String(c.jobsInstanceIDGSI),
		KeyConditionExpression: aws.String("instance_id = :instance_id"),
		FilterExpression:       aws.String("#status IN (:running, :launched)"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":instance_id": &types.AttributeValueMemberS{Value: instanceID},
			":running":     &types.AttributeValueMemberS{Value: string(JobStatusRunning)},
			":launched":    &types.AttributeValueMemberS{Value: string(JobStatusLaunched)},
		},
		Limit: aws.Int32(1),
	}

	output, err := c.dynamoClient.Query(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to query GSI: %w", err)
	}

	if len(output.Items) == 0 {
		return nil, nil
	}

	return unmarshalJobInfo(output.Items[0])
}

func (c *Client) getJobByInstanceViaScan(ctx context.Context, instanceID string) (*events.JobInfo, error) {
	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("instance_id = :instance_id AND #status IN (:running, :launched)"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":instance_id": &types.AttributeValueMemberS{Value: instanceID},
			":running":     &types.AttributeValueMemberS{Value: string(JobStatusRunning)},
			":launched":    &types.AttributeValueMemberS{Value: string(JobStatusLaunched)},
		},
		Limit: aws.Int32(1),
	}

	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := c.dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to scan for job by instance: %w", err)
		}

		if len(output.Items) > 0 {
			return unmarshalJobInfo(output.Items[0])
		}

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			return nil, nil
		}
	}
}

func unmarshalJobInfo(item map[string]types.AttributeValue) (*events.JobInfo, error) {
	var record jobRecord
	if err := attributevalue.UnmarshalMap(item, &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal job record: %w", err)
	}

	return &events.JobInfo{
		JobID:        record.JobID,
		RunID:        record.RunID,
		Repo:         record.Repo,
		InstanceType: record.InstanceType,
		Pool:         record.Pool,
		Spot:         record.Spot,
		RetryCount:   record.RetryCount,
	}, nil
}

// poolCreatedAtIndexName is the GSI on the jobs table keyed by (pool, created_at)
// used by QueryPoolJobHistory. Provisioned in Terraform.
const poolCreatedAtIndexName = "pool-created-at-index"

// QueryPoolJobHistory retrieves recent jobs for a pool within the specified time window.
//
// Uses Query on the (pool, created_at) GSI for an efficient range scan. Falls back to
// a filtered table Scan if the GSI is not yet provisioned (DynamoDB returns
// ValidationException), so a code deploy can land before the matching infra change.
//
// The Scan fallback paginates across all pages. The GSI Query path reads a single
// page (DynamoDB's ~1 MB limit), sufficient for current scale (~100 jobs/day) and a
// 1-hour window; if a single pool ever sustains tens of thousands of jobs in an hour,
// p90 concurrency would be computed from a truncated dataset and read under the true value.
func (c *Client) QueryPoolJobHistory(ctx context.Context, poolName string, since time.Time) ([]JobHistoryEntry, error) {
	if poolName == "" {
		return nil, fmt.Errorf("pool name cannot be empty")
	}

	if c.jobsTable == "" {
		return nil, fmt.Errorf("jobs table not configured")
	}

	entries, err := c.queryPoolJobHistoryViaGSI(ctx, poolName, since)
	if err == nil {
		return entries, nil
	}
	if !isGSIValidationError(err) {
		return nil, err
	}
	dbLog.Warn(ctx, "GSI query failed, falling back to scan",
		"gsi", poolCreatedAtIndexName,
		"error", err,
	)

	return c.queryPoolJobHistoryViaScan(ctx, poolName, since)
}

func (c *Client) queryPoolJobHistoryViaGSI(ctx context.Context, poolName string, since time.Time) ([]JobHistoryEntry, error) {
	sinceStr := since.Format(time.RFC3339)
	input := &dynamodb.QueryInput{
		TableName:              aws.String(c.jobsTable),
		IndexName:              aws.String(poolCreatedAtIndexName),
		KeyConditionExpression: aws.String("#pool = :pool AND #created_at >= :since"),
		FilterExpression:       aws.String("#status <> :orphaned"),
		ExpressionAttributeNames: map[string]string{
			"#pool":       "pool",
			"#created_at": "created_at",
			"#status":     "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pool":     &types.AttributeValueMemberS{Value: poolName},
			":since":    &types.AttributeValueMemberS{Value: sinceStr},
			":orphaned": &types.AttributeValueMemberS{Value: string(JobStatusOrphaned)},
		},
	}

	output, err := c.dynamoClient.Query(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to query GSI: %w", err)
	}

	return parseJobHistoryItems(output.Items), nil
}

func (c *Client) queryPoolJobHistoryViaScan(ctx context.Context, poolName string, since time.Time) ([]JobHistoryEntry, error) {
	sinceStr := since.Format(time.RFC3339)
	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("#pool = :pool AND created_at >= :since AND #status <> :orphaned"),
		ExpressionAttributeNames: map[string]string{
			"#pool":   "pool",
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pool":     &types.AttributeValueMemberS{Value: poolName},
			":since":    &types.AttributeValueMemberS{Value: sinceStr},
			":orphaned": &types.AttributeValueMemberS{Value: string(JobStatusOrphaned)},
		},
	}

	var entries []JobHistoryEntry
	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := c.dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to scan jobs: %w", err)
		}

		entries = append(entries, parseJobHistoryItems(output.Items)...)

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	return entries, nil
}

func parseJobHistoryItems(items []map[string]types.AttributeValue) []JobHistoryEntry {
	var entries []JobHistoryEntry
	for _, item := range items {
		var record struct {
			JobID       int64  `dynamodbav:"job_id"`
			Pool        string `dynamodbav:"pool"`
			Status      string `dynamodbav:"status"`
			CreatedAt   string `dynamodbav:"created_at"`
			CompletedAt string `dynamodbav:"completed_at"`
		}
		if err := attributevalue.UnmarshalMap(item, &record); err != nil {
			continue
		}

		entry := JobHistoryEntry{
			JobID:  record.JobID,
			Pool:   record.Pool,
			Status: JobStatus(record.Status),
		}

		if record.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, record.CreatedAt); err == nil {
				entry.CreatedAt = t
			}
		}
		if record.CompletedAt != "" {
			if t, err := time.Parse(time.RFC3339, record.CompletedAt); err == nil {
				entry.CompletedAt = t
			}
		}

		entries = append(entries, entry)
	}

	return entries
}

// maxConcurrencyRuntime bounds how long a job without a recorded completion may
// be counted as occupying an instance. It mirrors housekeeping's orphanedJobThreshold
// (2h): the configured job ceiling is RUNS_FLEET_MAX_RUNTIME_MINUTES (default 360, but
// realistic jobs run minutes), plus headroom for spot-interruption re-queue delays.
// An active record older than this is treated as abandoned (its completion message was
// lost or its instance is gone) rather than in-flight, so it cannot inflate concurrency
// across the whole window.
const maxConcurrencyRuntime = 2 * time.Hour

// occupiesInstance reports whether a job that has not recorded a completion is still
// genuinely running on an instance. Only active statuses (launched, running, terminating)
// created within maxConcurrencyRuntime count; claim stubs, terminal-without-completion
// records, and abandoned active records do not occupy capacity. launched counts because
// the instance is up and consuming capacity even before its runner confirms.
func occupiesInstance(status JobStatus, createdAt, now time.Time) bool {
	switch status {
	case JobStatusLaunched, JobStatusRunning, JobStatusTerminating:
		return now.Sub(createdAt) < maxConcurrencyRuntime
	default:
		return false
	}
}

// GetPoolP90Concurrency calculates the 90th percentile of concurrent jobs
// for a pool within the specified time window (in hours).
// This is more cost-effective than peak for scaling stopped instances,
// as it tolerates occasional bursts falling back to cold-start.
//
// Only jobs that actually occupied an instance contribute an interval:
//   - completed_at set: counted over [created_at, completed_at] (it ran and finished).
//   - completed_at empty + active status (running/terminating) created within
//     maxConcurrencyRuntime: counted over [created_at, now] (genuinely in-flight).
//   - everything else (claiming stubs, terminal-without-completion, abandoned active
//     records): excluded. Counting these inflated p90 and over-provisioned stopped
//     instances.
//
// Returns 0 if no jobs found or on error.
func (c *Client) GetPoolP90Concurrency(ctx context.Context, poolName string, windowHours int) (int, error) {
	if windowHours <= 0 {
		windowHours = 1
	}

	since := time.Now().Add(-time.Duration(windowHours) * time.Hour)
	jobs, err := c.QueryPoolJobHistory(ctx, poolName, since)
	if err != nil {
		return 0, err
	}

	if len(jobs) == 0 {
		return 0, nil
	}

	type event struct {
		time  time.Time
		delta int // +1 for start, -1 for end
	}

	var events []event
	now := time.Now()

	for _, job := range jobs {
		if job.CreatedAt.IsZero() {
			continue
		}

		endTime := job.CompletedAt
		if endTime.IsZero() {
			if !occupiesInstance(job.Status, job.CreatedAt, now) {
				continue
			}
			endTime = now
		}

		events = append(events, event{time: job.CreatedAt, delta: 1})
		events = append(events, event{time: endTime, delta: -1})
	}

	if len(events) == 0 {
		return 0, nil
	}

	// Sort events by time, ends before starts at same timestamp to avoid negative concurrency
	sort.Slice(events, func(i, j int) bool {
		if events[i].time.Equal(events[j].time) {
			return events[i].delta < events[j].delta // -1 (end) before +1 (start)
		}
		return events[i].time.Before(events[j].time)
	})

	// Sample concurrency at 1-minute intervals over the window
	sampleInterval := time.Minute
	windowStart := since
	windowEnd := now

	var samples []int
	var eventIdx int
	var current int

	for t := windowStart; t.Before(windowEnd); t = t.Add(sampleInterval) {
		// Advance through events strictly before time t
		for eventIdx < len(events) && events[eventIdx].time.Before(t) {
			current += events[eventIdx].delta
			eventIdx++
		}
		samples = append(samples, current)
	}

	if len(samples) == 0 {
		return 0, nil
	}

	// Sort samples to find P90 (90th percentile)
	sort.Ints(samples)
	// P90 index: for N samples, index = floor(0.9 * (N-1)) gives the 90th percentile
	// e.g., 10 samples: floor(0.9 * 9) = 8 (9th value, 0-indexed)
	// e.g., 60 samples: floor(0.9 * 59) = 53 (54th value, 0-indexed)
	p90Index := int(0.9 * float64(len(samples)-1))

	return samples[p90Index], nil
}

// GetPoolBusyInstanceIDs returns instance IDs that have running jobs in the pool.
// Used to identify which instances should not be stopped during reconciliation.
//
// When jobsPoolStatusGSI is configured, uses Query on the GSI for O(1) lookup.
// Falls back to Scan if the GSI is not configured or if the Query fails.
func (c *Client) GetPoolBusyInstanceIDs(ctx context.Context, poolName string) ([]string, error) {
	if poolName == "" {
		return nil, fmt.Errorf("pool name cannot be empty")
	}

	if c.jobsTable == "" {
		return nil, nil
	}

	if c.jobsPoolStatusGSI != "" {
		ids, err := c.getPoolBusyInstanceIDsViaGSI(ctx, poolName)
		if err == nil {
			return ids, nil
		}
		if !isGSIValidationError(err) {
			return nil, err
		}
		dbLog.Warn(ctx, "GSI query failed, falling back to scan",
			"gsi", c.jobsPoolStatusGSI,
			"error", err,
		)
	}

	return c.getPoolBusyInstanceIDsViaScan(ctx, poolName)
}

func (c *Client) getPoolBusyInstanceIDsViaGSI(ctx context.Context, poolName string) ([]string, error) {
	// The pool-status GSI is keyed on status (sort key), so one Query can only
	// match a single status. Both launched (instance up, runner unconfirmed) and
	// running (runner executing) occupy the instance and must not be stopped, so
	// query each and union the instance IDs.
	var ids []string
	seen := make(map[string]struct{})
	for _, status := range []JobStatus{JobStatusLaunched, JobStatusRunning} {
		input := &dynamodb.QueryInput{
			TableName:              aws.String(c.jobsTable),
			IndexName:              aws.String(c.jobsPoolStatusGSI),
			KeyConditionExpression: aws.String("#pool = :pool AND #status = :status"),
			ExpressionAttributeNames: map[string]string{
				"#pool":   "pool",
				"#status": "status",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pool":   &types.AttributeValueMemberS{Value: poolName},
				":status": &types.AttributeValueMemberS{Value: string(status)},
			},
			ProjectionExpression: aws.String("instance_id"),
		}

		output, err := c.dynamoClient.Query(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to query GSI: %w", err)
		}
		for _, id := range extractInstanceIDs(output.Items) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}

	return ids, nil
}

func (c *Client) getPoolBusyInstanceIDsViaScan(ctx context.Context, poolName string) ([]string, error) {
	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("#pool = :pool AND #status IN (:running, :launched)"),
		ExpressionAttributeNames: map[string]string{
			"#pool":   "pool",
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pool":     &types.AttributeValueMemberS{Value: poolName},
			":running":  &types.AttributeValueMemberS{Value: string(JobStatusRunning)},
			":launched": &types.AttributeValueMemberS{Value: string(JobStatusLaunched)},
		},
		ProjectionExpression: aws.String("instance_id"),
	}

	var ids []string
	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := c.dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to scan jobs: %w", err)
		}

		ids = append(ids, extractInstanceIDs(output.Items)...)

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	return ids, nil
}

func extractInstanceIDs(items []map[string]types.AttributeValue) []string {
	var ids []string
	for _, item := range items {
		if v, ok := item["instance_id"]; ok {
			if s, ok := v.(*types.AttributeValueMemberS); ok && s.Value != "" {
				ids = append(ids, s.Value)
			}
		}
	}
	return ids
}

func isGSIValidationError(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "ValidationException"
	}
	return false
}

// GetJobByJobID retrieves job information by job ID (partition key lookup, O(1)).
func (c *Client) GetJobByJobID(ctx context.Context, jobID int64) (*events.JobInfo, error) {
	if jobID == 0 {
		return nil, fmt.Errorf("job ID cannot be zero")
	}

	if c.jobsTable == "" {
		return nil, fmt.Errorf("jobs table not configured")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": jobID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal key: %w", err)
	}

	output, err := c.dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.jobsTable),
		Key:       key,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get job: %w", err)
	}

	if output.Item == nil {
		return nil, nil
	}

	var record jobRecord
	if err := attributevalue.UnmarshalMap(output.Item, &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal job record: %w", err)
	}

	return &events.JobInfo{
		JobID:        record.JobID,
		RunID:        record.RunID,
		Repo:         record.Repo,
		InstanceType: record.InstanceType,
		Pool:         record.Pool,
		Spot:         record.Spot,
		RetryCount:   record.RetryCount,
	}, nil
}

// MarkJobRequeuedByJobID atomically marks a job as "requeued" using the job_id partition key.
// Returns true if the job was successfully marked (was in "running" or "launched" state).
// Returns false if the job was already in another state (terminating, requeued, etc.).
func (c *Client) MarkJobRequeuedByJobID(ctx context.Context, jobID int64) (bool, error) {
	if jobID == 0 {
		return false, fmt.Errorf("job ID cannot be zero")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": jobID,
	})
	if err != nil {
		return false, fmt.Errorf("failed to marshal key: %w", err)
	}

	return c.markJobRequeued(ctx, key)
}

func (c *Client) markJobRequeued(ctx context.Context, key map[string]types.AttributeValue) (bool, error) {
	if c.jobsTable == "" {
		return false, fmt.Errorf("jobs table not configured")
	}

	update := "SET #status = :new_status, requeued_at = :requeued_at"
	// Requeue an active job whether or not its runner confirmed: a spot
	// interruption can hit a launched (not-yet-confirmed) instance, and the
	// unconfirmed-runner watchdog requeues launched jobs whose runner never came up.
	condition := "#status IN (:running, :launched)"
	exprNames := map[string]string{
		"#status": "status",
	}
	exprValues, err := attributevalue.MarshalMap(map[string]interface{}{
		":new_status":  string(JobStatusRequeued),
		":running":     string(JobStatusRunning),
		":launched":    string(JobStatusLaunched),
		":requeued_at": time.Now().Format(time.RFC3339),
	})
	if err != nil {
		return false, fmt.Errorf("failed to marshal values: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(c.jobsTable),
		Key:                       key,
		UpdateExpression:          aws.String(update),
		ConditionExpression:       aws.String(condition),
		ExpressionAttributeNames:  exprNames,
		ExpressionAttributeValues: exprValues,
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return false, nil
		}
		return false, fmt.Errorf("failed to mark job requeued: %w", err)
	}

	return true, nil
}

// AdminJobEntry represents a job for admin API responses.
type AdminJobEntry struct {
	JobID           int64
	RunID           int64
	Repo            string
	InstanceID      string
	InstanceType    string
	Pool            string
	Spot            bool
	WarmPoolHit     bool
	RetryCount      int
	Status          string
	ExitCode        int
	DurationSeconds int
	SpotRequestID   string
	TraceID         string
	CreatedAt       time.Time
	StartedAt       time.Time
	CompletedAt     time.Time
}

// AdminJobFilter specifies filtering options for job queries.
type AdminJobFilter struct {
	Status string
	Pool   string
	Since  time.Time
	Limit  int
	Offset int
}

// AdminJobStats contains aggregate job statistics.
type AdminJobStats struct {
	Total       int
	Completed   int
	Failed      int
	Running     int
	Requeued    int
	WarmPoolHit int
}

// ListJobsForAdmin retrieves jobs with filtering for admin API.
func (c *Client) ListJobsForAdmin(ctx context.Context, filter AdminJobFilter) ([]AdminJobEntry, int, error) {
	if c.jobsTable == "" {
		return nil, 0, fmt.Errorf("jobs table not configured")
	}

	// Build filter expression
	filterParts := []string{}
	exprNames := map[string]string{}
	exprValues := map[string]types.AttributeValue{}

	if filter.Status != "" {
		filterParts = append(filterParts, "#status = :status")
		exprNames["#status"] = "status"
		exprValues[":status"] = &types.AttributeValueMemberS{Value: filter.Status}
	}

	if filter.Pool != "" {
		filterParts = append(filterParts, "#pool = :pool")
		exprNames["#pool"] = "pool"
		exprValues[":pool"] = &types.AttributeValueMemberS{Value: filter.Pool}
	}

	if !filter.Since.IsZero() {
		filterParts = append(filterParts, "created_at >= :since")
		exprValues[":since"] = &types.AttributeValueMemberS{Value: filter.Since.Format(time.RFC3339)}
	}

	input := &dynamodb.ScanInput{
		TableName: aws.String(c.jobsTable),
	}

	if len(filterParts) > 0 {
		filterExpr := filterParts[0]
		for i := 1; i < len(filterParts); i++ {
			filterExpr += " AND " + filterParts[i]
		}
		input.FilterExpression = aws.String(filterExpr)
	}

	if len(exprNames) > 0 {
		input.ExpressionAttributeNames = exprNames
	}
	if len(exprValues) > 0 {
		input.ExpressionAttributeValues = exprValues
	}

	var entries []AdminJobEntry
	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := c.dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to scan jobs: %w", err)
		}

		for _, item := range output.Items {
			entries = append(entries, parseJobItem(item))
		}

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	// Sort by created_at descending
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})

	total := len(entries)

	// Apply offset and limit
	if filter.Offset > 0 {
		if filter.Offset >= len(entries) {
			entries = nil
		} else {
			entries = entries[filter.Offset:]
		}
	}

	if filter.Limit > 0 && filter.Limit < len(entries) {
		entries = entries[:filter.Limit]
	}

	return entries, total, nil
}

// GetJobForAdmin retrieves a single job by ID for admin API.
func (c *Client) GetJobForAdmin(ctx context.Context, jobID int64) (*AdminJobEntry, error) {
	if c.jobsTable == "" {
		return nil, fmt.Errorf("jobs table not configured")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": jobID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal key: %w", err)
	}

	output, err := c.dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.jobsTable),
		Key:       key,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get job: %w", err)
	}

	if output.Item == nil {
		return nil, nil
	}

	entry := parseJobItem(output.Item)
	return &entry, nil
}

// GetJobStatsForAdmin retrieves aggregate job statistics for admin API.
func (c *Client) GetJobStatsForAdmin(ctx context.Context, since time.Time) (*AdminJobStats, error) {
	if c.jobsTable == "" {
		return nil, fmt.Errorf("jobs table not configured")
	}

	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("created_at >= :since"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":since": &types.AttributeValueMemberS{Value: since.Format(time.RFC3339)},
		},
	}

	stats := &AdminJobStats{}
	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := c.dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to scan jobs: %w", err)
		}

		for _, item := range output.Items {
			stats.Total++

			if status, ok := item["status"]; ok {
				if s, ok := status.(*types.AttributeValueMemberS); ok {
					switch s.Value {
					case string(JobStatusCompleted), string(JobStatusSuccess):
						stats.Completed++
					case string(JobStatusFailed), string(JobStatusError):
						stats.Failed++
					case string(JobStatusLaunched), string(JobStatusRunning), string(JobStatusClaiming):
						stats.Running++
					case string(JobStatusRequeued):
						stats.Requeued++
					}
				}
			}

			if warmHit, ok := item["warm_pool_hit"]; ok {
				if b, ok := warmHit.(*types.AttributeValueMemberBOOL); ok && b.Value {
					stats.WarmPoolHit++
				}
			}
		}

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	return stats, nil
}

func parseJobItem(item map[string]types.AttributeValue) AdminJobEntry {
	return AdminJobEntry{
		JobID:           getInt64Attr(item, "job_id"),
		RunID:           getInt64Attr(item, "run_id"),
		Repo:            getStringAttr(item, "repo"),
		InstanceID:      getStringAttr(item, "instance_id"),
		InstanceType:    getStringAttr(item, "instance_type"),
		Pool:            getStringAttr(item, "pool"),
		Spot:            getBoolAttr(item, "spot"),
		WarmPoolHit:     getBoolAttr(item, "warm_pool_hit"),
		RetryCount:      getIntAttr(item, "retry_count"),
		Status:          getStringAttr(item, "status"),
		ExitCode:        getIntAttr(item, "exit_code"),
		DurationSeconds: getIntAttr(item, "duration_seconds"),
		SpotRequestID:   getStringAttr(item, "spot_request_id"),
		TraceID:         getStringAttr(item, "trace_id"),
		CreatedAt:       getTimeAttr(item, "created_at"),
		StartedAt:       getTimeAttr(item, "started_at"),
		CompletedAt:     getTimeAttr(item, "completed_at"),
	}
}

func getStringAttr(item map[string]types.AttributeValue, key string) string {
	if v, ok := item[key]; ok {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			return s.Value
		}
	}
	return ""
}

func getInt64Attr(item map[string]types.AttributeValue, key string) int64 {
	if v, ok := item[key]; ok {
		if n, ok := v.(*types.AttributeValueMemberN); ok {
			val, _ := parseInt64(n.Value)
			return val
		}
	}
	return 0
}

func getIntAttr(item map[string]types.AttributeValue, key string) int {
	if v, ok := item[key]; ok {
		if n, ok := v.(*types.AttributeValueMemberN); ok {
			val, _ := parseInt(n.Value)
			return val
		}
	}
	return 0
}

func getBoolAttr(item map[string]types.AttributeValue, key string) bool {
	if v, ok := item[key]; ok {
		if b, ok := v.(*types.AttributeValueMemberBOOL); ok {
			return b.Value
		}
	}
	return false
}

func getTimeAttr(item map[string]types.AttributeValue, key string) time.Time {
	if v, ok := item[key]; ok {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			t, _ := time.Parse(time.RFC3339, s.Value)
			return t
		}
	}
	return time.Time{}
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// extractTraceID extracts the trace-id from a W3C traceparent header.
// Format: version-trace_id-parent_id-trace_flags (e.g., "00-abc...def-0123456789abcdef-01").
// Returns empty string if the traceparent is invalid or empty.
func extractTraceID(traceparent string) string {
	if traceparent == "" {
		return ""
	}
	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 {
		return ""
	}
	traceID := parts[1]
	if len(traceID) != 32 {
		return ""
	}
	return traceID
}
