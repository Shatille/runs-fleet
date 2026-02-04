package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ErrJobAlreadyClaimed is returned when attempting to claim a job that is already being processed.
var ErrJobAlreadyClaimed = errors.New("job already claimed")

// jobRecord represents a job stored in DynamoDB.
// Primary key is instance_id (one job per instance, ephemeral runners).
type jobRecord struct {
	InstanceID   string `dynamodbav:"instance_id"`
	JobID        int64  `dynamodbav:"job_id"`
	RunID        int64  `dynamodbav:"run_id"`
	Repo         string `dynamodbav:"repo"`
	InstanceType string `dynamodbav:"instance_type"`
	Pool         string `dynamodbav:"pool"`
	Spot         bool   `dynamodbav:"spot"`
	RetryCount   int    `dynamodbav:"retry_count"`
	WarmPoolHit  bool   `dynamodbav:"warm_pool_hit"`
	Status       string `dynamodbav:"status"`
	CreatedAt    string `dynamodbav:"created_at"`
}

// JobRecord contains job information for storage.
type JobRecord struct {
	JobID        int64
	RunID        int64
	Repo         string
	InstanceID   string
	InstanceType string
	Pool         string
	Spot         bool
	RetryCount   int
	WarmPoolHit  bool
}

// JobHistoryEntry represents a job with timing information for auto-scaling calculations.
type JobHistoryEntry struct {
	JobID       int64
	Pool        string
	CreatedAt   time.Time
	CompletedAt time.Time // Zero value if still running
}

// SaveJob creates or updates a job record in DynamoDB.
// The job is stored with status "running" and can be queried by instance_id via the GSI.
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
		InstanceID:   job.InstanceID,
		JobID:        job.JobID,
		RunID:        job.RunID,
		Repo:         job.Repo,
		InstanceType: job.InstanceType,
		Pool:         job.Pool,
		Spot:         job.Spot,
		RetryCount:   job.RetryCount,
		WarmPoolHit:  job.WarmPoolHit,
		Status:       "running",
		CreatedAt:    time.Now().Format(time.RFC3339),
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

// ClaimJob atomically claims a job for processing using conditional write.
// Returns ErrJobAlreadyClaimed if the job is already being processed.
// Uses job_id as primary key to track claims.
func (c *Client) ClaimJob(ctx context.Context, jobID int64) error {
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}

	if c.jobsTable == "" {
		return fmt.Errorf("jobs table not configured")
	}

	record := jobRecord{
		JobID:     jobID,
		Status:    "claiming",
		CreatedAt: time.Now().Format(time.RFC3339),
	}

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("failed to marshal claim record: %w", err)
	}

	_, err = c.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(c.jobsTable),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(job_id)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrJobAlreadyClaimed
		}
		return fmt.Errorf("failed to claim job: %w", err)
	}

	return nil
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

// MarkJobComplete marks a job as complete in DynamoDB with exit status.
// Uses job_id as primary key (Number type in DynamoDB).
func (c *Client) MarkJobComplete(ctx context.Context, jobID int64, status string, exitCode, duration int) error {
	if jobID == 0 {
		return fmt.Errorf("job ID cannot be zero")
	}
	if status == "" {
		return fmt.Errorf("status cannot be empty")
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": jobID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
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
		return fmt.Errorf("failed to marshal values: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(c.jobsTable),
		Key:                       key,
		UpdateExpression:          aws.String(update),
		ExpressionAttributeNames:  exprNames,
		ExpressionAttributeValues: exprValues,
	})
	if err != nil {
		return fmt.Errorf("failed to update job: %w", err)
	}

	return nil
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
			":status": &types.AttributeValueMemberS{Value: "terminating"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to mark job as terminating: %w", err)
	}

	return nil
}

// GetJobByInstance retrieves job information for a given instance ID from DynamoDB.
// Uses Scan with filter since job_id is the primary key, not instance_id.
func (c *Client) GetJobByInstance(ctx context.Context, instanceID string) (*events.JobInfo, error) {
	if instanceID == "" {
		return nil, fmt.Errorf("instance ID cannot be empty")
	}

	if c.jobsTable == "" {
		return nil, fmt.Errorf("jobs table not configured")
	}

	// Scan for running job with this instance_id (job_id is the primary key).
	// TODO: Add GSI on instance_id for O(1) lookup instead of O(n) scan.
	// Current approach is acceptable given small table size (ephemeral jobs, <1000 items)
	// and low call frequency (spot interruption handling only).
	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("instance_id = :instance_id AND #status = :status"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":instance_id": &types.AttributeValueMemberS{Value: instanceID},
			":status":      &types.AttributeValueMemberS{Value: "running"},
		},
		Limit: aws.Int32(1),
	}

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to scan for job by instance: %w", err)
	}

	if len(output.Items) == 0 {
		return nil, nil
	}

	var record jobRecord
	if err := attributevalue.UnmarshalMap(output.Items[0], &record); err != nil {
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

// QueryPoolJobHistory retrieves recent jobs for a pool within the specified time window.
//
// Performance note: Uses Scan with filter. Acceptable for current scale (~100 jobs/day).
// For high-volume deployments, add GSI on (pool, created_at) for efficient queries.
func (c *Client) QueryPoolJobHistory(ctx context.Context, poolName string, since time.Time) ([]JobHistoryEntry, error) {
	if poolName == "" {
		return nil, fmt.Errorf("pool name cannot be empty")
	}

	if c.jobsTable == "" {
		return nil, fmt.Errorf("jobs table not configured")
	}

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
			":orphaned": &types.AttributeValueMemberS{Value: "orphaned"},
		},
	}

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to scan jobs: %w", err)
	}

	var entries []JobHistoryEntry
	for _, item := range output.Items {
		var record struct {
			JobID       int64  `dynamodbav:"job_id"`
			Pool        string `dynamodbav:"pool"`
			CreatedAt   string `dynamodbav:"created_at"`
			CompletedAt string `dynamodbav:"completed_at"`
		}
		if err := attributevalue.UnmarshalMap(item, &record); err != nil {
			continue
		}

		entry := JobHistoryEntry{
			JobID: record.JobID,
			Pool:  record.Pool,
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

	return entries, nil
}

// GetPoolPeakConcurrency calculates the maximum number of concurrent jobs
// for a pool within the specified time window (in hours).
// Returns 0 if no jobs found or on error.
func (c *Client) GetPoolPeakConcurrency(ctx context.Context, poolName string, windowHours int) (int, error) {
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
		events = append(events, event{time: job.CreatedAt, delta: 1})

		endTime := job.CompletedAt
		if endTime.IsZero() {
			endTime = now
		}
		events = append(events, event{time: endTime, delta: -1})
	}

	sort.Slice(events, func(i, j int) bool {
		if events[i].time.Equal(events[j].time) {
			return events[i].delta > events[j].delta
		}
		return events[i].time.Before(events[j].time)
	})

	var current, peak int
	for _, e := range events {
		current += e.delta
		if current > peak {
			peak = current
		}
	}

	return peak, nil
}

// GetPoolRunningJobCount returns the number of jobs currently running in a pool.
// Uses Scan with filter on pool and status fields.
//
// Performance note: Scan is acceptable for current scale (~100 jobs/day, ~10 concurrent).
// For high-volume deployments (>1000 concurrent jobs), add GSI on (pool, status) in
// Terraform and switch to Query. See: github.com/Shatille/runs-fleet/issues/TBD
func (c *Client) GetPoolRunningJobCount(ctx context.Context, poolName string) (int, error) {
	if poolName == "" {
		return 0, fmt.Errorf("pool name cannot be empty")
	}

	if c.jobsTable == "" {
		return 0, nil
	}

	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("#pool = :pool AND #status = :status"),
		ExpressionAttributeNames: map[string]string{
			"#pool":   "pool",
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pool":   &types.AttributeValueMemberS{Value: poolName},
			":status": &types.AttributeValueMemberS{Value: "running"},
		},
		Select: types.SelectCount,
	}

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return 0, fmt.Errorf("failed to scan jobs: %w", err)
	}

	return int(output.Count), nil
}

// GetPoolBusyInstanceIDs returns instance IDs that have running jobs in the pool.
// Used to identify which instances should not be stopped during reconciliation.
//
// Performance note: Uses Scan (same as GetPoolRunningJobCount). Acceptable for
// current scale; requires GSI for high-volume deployments.
func (c *Client) GetPoolBusyInstanceIDs(ctx context.Context, poolName string) ([]string, error) {
	if poolName == "" {
		return nil, fmt.Errorf("pool name cannot be empty")
	}

	if c.jobsTable == "" {
		return nil, nil
	}

	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("#pool = :pool AND #status = :status"),
		ExpressionAttributeNames: map[string]string{
			"#pool":   "pool",
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pool":   &types.AttributeValueMemberS{Value: poolName},
			":status": &types.AttributeValueMemberS{Value: "running"},
		},
		ProjectionExpression: aws.String("instance_id"),
	}

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to scan jobs: %w", err)
	}

	var instanceIDs []string
	for _, item := range output.Items {
		if v, ok := item["instance_id"]; ok {
			if s, ok := v.(*types.AttributeValueMemberS); ok && s.Value != "" {
				instanceIDs = append(instanceIDs, s.Value)
			}
		}
	}

	return instanceIDs, nil
}

// MarkJobRequeued atomically marks a job as "requeued" if its current status is "running".
// Returns true if the job was successfully marked (was in "running" state).
// Returns false if the job was already in another state (terminating, requeued, etc.).
// This prevents duplicate re-queuing from concurrent webhook handlers.
func (c *Client) MarkJobRequeued(ctx context.Context, instanceID string) (bool, error) {
	if instanceID == "" {
		return false, fmt.Errorf("instance ID cannot be empty")
	}

	if c.jobsTable == "" {
		return false, fmt.Errorf("jobs table not configured")
	}

	key, err := attributevalue.MarshalMap(map[string]string{
		"instance_id": instanceID,
	})
	if err != nil {
		return false, fmt.Errorf("failed to marshal key: %w", err)
	}

	update := "SET #status = :new_status, requeued_at = :requeued_at"
	condition := "#status = :current_status"
	exprNames := map[string]string{
		"#status": "status",
	}
	exprValues, err := attributevalue.MarshalMap(map[string]interface{}{
		":new_status":     "requeued",
		":current_status": "running",
		":requeued_at":    time.Now().Format(time.RFC3339),
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

