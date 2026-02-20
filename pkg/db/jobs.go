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
	InstanceID     string `dynamodbav:"instance_id"`
	JobID          int64  `dynamodbav:"job_id"`
	RunID          int64  `dynamodbav:"run_id"`
	Repo           string `dynamodbav:"repo"`
	InstanceType   string `dynamodbav:"instance_type"`
	Pool           string `dynamodbav:"pool"`
	Spot           bool   `dynamodbav:"spot"`
	RetryCount     int    `dynamodbav:"retry_count"`
	WarmPoolHit    bool   `dynamodbav:"warm_pool_hit"`
	Status         string `dynamodbav:"status"`
	CreatedAt      string `dynamodbav:"created_at"`
	SpotRequestID  string `dynamodbav:"spot_request_id,omitempty"`
	PersistentSpot bool   `dynamodbav:"persistent_spot,omitempty"`
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
		InstanceID:     job.InstanceID,
		JobID:          job.JobID,
		RunID:          job.RunID,
		Repo:           job.Repo,
		InstanceType:   job.InstanceType,
		Pool:           job.Pool,
		Spot:           job.Spot,
		RetryCount:     job.RetryCount,
		WarmPoolHit:    job.WarmPoolHit,
		Status:         "running",
		CreatedAt:      time.Now().Format(time.RFC3339),
		SpotRequestID:  job.SpotRequestID,
		PersistentSpot: job.PersistentSpot,
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

// GetPoolP90Concurrency calculates the 90th percentile of concurrent jobs
// for a pool within the specified time window (in hours).
// This is more cost-effective than peak for scaling stopped instances,
// as it tolerates occasional bursts falling back to cold-start.
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
		events = append(events, event{time: job.CreatedAt, delta: 1})

		endTime := job.CompletedAt
		if endTime.IsZero() {
			endTime = now
		}
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
// Performance note: Uses Scan. Acceptable for current scale (~100 jobs/day, ~10 concurrent).
// For high-volume deployments (>1000 concurrent jobs), add GSI on (pool, status).
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
// Returns true if the job was successfully marked (was in "running" state).
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

// MarkJobRequeued atomically marks a job as "requeued" if its current status is "running".
// Deprecated: Uses instance_id as key which doesn't match the job_id partition key.
// Use MarkJobRequeuedByJobID instead.
func (c *Client) MarkJobRequeued(ctx context.Context, instanceID string) (bool, error) {
	if instanceID == "" {
		return false, fmt.Errorf("instance ID cannot be empty")
	}

	key, err := attributevalue.MarshalMap(map[string]string{
		"instance_id": instanceID,
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

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to scan jobs: %w", err)
	}

	var entries []AdminJobEntry
	for _, item := range output.Items {
		entry := parseJobItem(item)
		entries = append(entries, entry)
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

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to scan jobs: %w", err)
	}

	stats := &AdminJobStats{}
	for _, item := range output.Items {
		stats.Total++

		if status, ok := item["status"]; ok {
			if s, ok := status.(*types.AttributeValueMemberS); ok {
				switch s.Value {
				case "completed", "success":
					stats.Completed++
				case "failed", "error":
					stats.Failed++
				case "running", "claiming":
					stats.Running++
				case "requeued":
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

	return stats, nil
}

// SaveSpotRequestID stores the spot request ID for an instance.
// Used to track persistent spot requests that can be cancelled on termination.
func (c *Client) SaveSpotRequestID(ctx context.Context, instanceID, spotRequestID string, persistent bool) error {
	if instanceID == "" {
		return fmt.Errorf("instance ID cannot be empty")
	}
	if spotRequestID == "" {
		return fmt.Errorf("spot request ID cannot be empty")
	}

	if c.jobsTable == "" {
		return fmt.Errorf("jobs table not configured")
	}

	// Update existing job record with spot request ID
	// Uses Scan since instance_id is not the primary key
	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("instance_id = :instance_id"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":instance_id": &types.AttributeValueMemberS{Value: instanceID},
		},
		ProjectionExpression: aws.String("job_id"),
		Limit:                aws.Int32(1),
	}

	output, err := c.dynamoClient.Scan(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to find job for instance: %w", err)
	}

	if len(output.Items) == 0 {
		return fmt.Errorf("no job found for instance %s", instanceID)
	}

	var jobID int64
	if v, ok := output.Items[0]["job_id"].(*types.AttributeValueMemberN); ok {
		if _, parseErr := fmt.Sscanf(v.Value, "%d", &jobID); parseErr != nil {
			return fmt.Errorf("failed to parse job ID: %w", parseErr)
		}
	}

	key, err := attributevalue.MarshalMap(map[string]int64{
		"job_id": jobID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	update := "SET spot_request_id = :spot_req_id, persistent_spot = :persistent"
	exprValues, err := attributevalue.MarshalMap(map[string]interface{}{
		":spot_req_id": spotRequestID,
		":persistent":  persistent,
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
		return fmt.Errorf("failed to save spot request ID: %w", err)
	}

	return nil
}

// SpotRequestInfo contains spot request information for an instance.
type SpotRequestInfo struct {
	InstanceID    string
	SpotRequestID string
	Persistent    bool
}

// GetSpotRequestIDs retrieves spot request IDs for multiple instances.
// Returns a map of instance ID to SpotRequestInfo.
// Only returns entries for instances that have persistent spot requests.
//
// Performance note: Uses single Scan with filter. For high-volume deployments,
// add GSI on (instance_id, persistent_spot) for O(1) lookups.
func (c *Client) GetSpotRequestIDs(ctx context.Context, instanceIDs []string) (map[string]SpotRequestInfo, error) {
	if len(instanceIDs) == 0 {
		return make(map[string]SpotRequestInfo), nil
	}

	if c.jobsTable == "" {
		return nil, fmt.Errorf("jobs table not configured")
	}

	result := make(map[string]SpotRequestInfo)

	// Build instance ID set for fast lookup
	instanceSet := make(map[string]struct{}, len(instanceIDs))
	for _, id := range instanceIDs {
		instanceSet[id] = struct{}{}
	}

	// Single Scan with persistent_spot filter, then filter by instance IDs in memory
	// More efficient than N separate scans for small/medium tables (<10k items)
	input := &dynamodb.ScanInput{
		TableName:        aws.String(c.jobsTable),
		FilterExpression: aws.String("persistent_spot = :persistent AND attribute_exists(spot_request_id)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":persistent": &types.AttributeValueMemberBOOL{Value: true},
		},
		ProjectionExpression: aws.String("instance_id, spot_request_id, persistent_spot"),
	}

	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := c.dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to scan for spot requests: %w", err)
		}

		for _, item := range output.Items {
			var instanceID string
			if v, ok := item["instance_id"].(*types.AttributeValueMemberS); ok {
				instanceID = v.Value
			}

			// Skip if not in our target set
			if _, ok := instanceSet[instanceID]; !ok {
				continue
			}

			info := SpotRequestInfo{
				InstanceID: instanceID,
			}
			if v, ok := item["spot_request_id"].(*types.AttributeValueMemberS); ok {
				info.SpotRequestID = v.Value
			}
			if v, ok := item["persistent_spot"].(*types.AttributeValueMemberBOOL); ok {
				info.Persistent = v.Value
			}

			if info.SpotRequestID != "" {
				result[instanceID] = info
			}
		}

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	return result, nil
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

