# Queue Processors - Context & Key Decisions

**Last Updated:** 2025-11-25

## Key Files

### Reference Implementation
- `pkg/events/handler.go:1-311` - **BEST REFERENCE** for queue processor pattern
  - Shows proper message batching (line 115: receive 10 messages)
  - Concurrent processing with semaphore (lines 99-100, 143-144)
  - Graceful shutdown handling (lines 104-105, 158-162)
  - Retry with backoff (lines 211-238)
  - Message deletion pattern (lines 173-182)

### Existing Infrastructure
- `pkg/queue/sqs.go` - SQS client wrapper with send/receive/delete methods
- `pkg/db/dynamo.go` - DynamoDB client for pool and lock operations
- `pkg/metrics/cloudwatch.go` - CloudWatch metrics publisher
- `pkg/config/config.go` - Configuration and AWS client initialization

### To Be Created
- `pkg/termination/handler.go` - Termination queue processor
- `pkg/termination/handler_test.go` - Unit tests
- `pkg/housekeeping/handler.go` - Housekeeping queue processor
- `pkg/housekeeping/tasks.go` - Individual cleanup task implementations
- `pkg/housekeeping/handler_test.go` - Unit tests
- `pkg/housekeeping/tasks_test.go` - Task unit tests

### To Be Modified
- `cmd/server/main.go` - Add handler initialization and goroutine startup
- `pkg/db/dynamo.go` - Add job completion and query methods
- `pkg/metrics/cloudwatch.go` - Add job duration/success/failure metrics
- `pkg/config/config.go` - Add TerminationQueueURL and HousekeepingQueueURL fields

## Architectural Decisions

### 1. Follow Events Handler Pattern
**Decision:** Replicate the structure and patterns from `pkg/events/handler.go`
**Rationale:** Proven implementation with proper error handling, concurrency, and shutdown
**Impact:** Faster development, consistent codebase patterns, less risk of bugs

### 2. Idempotent Message Processing
**Decision:** Make all database updates idempotent (can apply multiple times safely)
**Rationale:** SQS delivers messages at-least-once, duplicates possible
**Impact:** Use DynamoDB conditional updates or check-before-update patterns

### 3. Concurrent Processing with Semaphore
**Decision:** Process multiple messages concurrently but limit to 5 workers
**Rationale:** Balance throughput vs resource usage (DynamoDB, SSM, EC2 API limits)
**Impact:** Higher throughput than sequential, controlled resource consumption

### 4. Graceful Shutdown
**Decision:** Wait for in-flight messages to complete on SIGTERM (with timeout)
**Rationale:** Prevents message loss and incomplete state updates during deployment
**Impact:** Orchestrator must support graceful shutdown (already does via context cancellation)

### 5. Dead Letter Queue After 3 Retries
**Decision:** Move messages to DLQ after 3 failed processing attempts
**Rationale:** Prevents poison messages from blocking queue, allows manual investigation
**Impact:** Monitor DLQ depth as operational metric (should stay at zero)

### 6. Separate SSM Cleanup from Agent
**Decision:** Don't rely on agent to delete its own SSM parameter
**Rationale:** Agent may crash before cleanup, network issues, etc.
**Impact:** Housekeeping provides safety net for orphaned parameters

### 7. 7-Day Job Record Retention
**Decision:** Keep completed job records for 7 days before archival/deletion
**Rationale:** Sufficient for debugging recent issues, prevents table bloat
**Impact:** DynamoDB table stays under 100MB with typical workload

## Integration Points

### Termination Queue (Input)
- **Queue URL:** `RUNS_FLEET_TERMINATION_QUEUE_URL` environment variable
- **Message Source:** Agent binary sends on job completion (`pkg/agent/telemetry.go`)
- **Message Format:**
  ```json
  {
    "instance_id": "i-0123456789abcdef0",
    "job_id": "67890",
    "status": "success|failure|timeout|interrupted",
    "exit_code": 0,
    "duration_seconds": 123,
    "started_at": "2025-11-25T10:00:00Z",
    "completed_at": "2025-11-25T10:02:03Z",
    "error": "optional error message"
  }
  ```
- **Throughput:** ~100 messages/hour typical, burst to 1000/hour possible

### Housekeeping Queue (Input)
- **Queue URL:** `RUNS_FLEET_HOUSEKEEPING_QUEUE_URL` environment variable
- **Message Source:** EventBridge scheduled rules
- **Message Format:**
  ```json
  {
    "task_type": "orphaned_instances|stale_ssm|old_jobs|pool_audit",
    "timestamp": "2025-11-25T10:00:00Z",
    "params": {}
  }
  ```
- **Throughput:** ~100 messages/day (scheduled tasks)

### DynamoDB Workflow Jobs Table (Read/Write)
- **Table Name:** `RUNS_FLEET_JOBS_TABLE` environment variable
- **Schema:**
  ```
  PK: job_id (string)
  Attributes:
    - instance_id (string)
    - status (string): queued, running, completed, failed, timeout, interrupted
    - exit_code (number)
    - duration_seconds (number)
    - started_at (string, ISO 8601)
    - completed_at (string, ISO 8601)
    - error (string, optional)
  GSI: completed_at-index (for old job queries)
  ```
- **Operations:**
  - `MarkJobComplete`: Update status, exit_code, completed_at
  - `QueryOldJobs`: GSI query for completed_at < cutoff
  - `BatchDeleteJobs`: Batch write delete requests

### SSM Parameter Store (Delete)
- **Parameter Path:** `/runs-fleet/runners/{instance-id}/config`
- **Operations:**
  - `DeleteParameter`: Remove parameter by path
  - `ListParameters`: Get all parameters under prefix (for stale_ssm task)
- **Pagination:** 100 parameters per API call

### EC2 API (Read/Terminate)
- **Operations:**
  - `DescribeInstances`: Query instances with tag `runs-fleet:managed=true`
  - `TerminateInstances`: Terminate orphaned instances
- **Filters:**
  - Tag: `runs-fleet:managed=true`
  - State: running, pending
  - Launch time < now() - max_runtime - 10 minutes

### CloudWatch Metrics (Write)
- **Namespace:** `RunsFleet`
- **Metrics:**
  - `JobDuration` (seconds) - Dimension: Status
  - `JobSuccess` (count) - No dimensions
  - `JobFailure` (count) - Dimension: Error
  - `OrphanedInstancesTerminated` (count)
  - `SSMParametersDeleted` (count)
  - `JobRecordsArchived` (count)
  - `PoolIdleCost` (USD) - Dimension: PoolName
  - `PoolComputeCost` (USD) - Dimension: PoolName
  - `PoolUtilization` (percent) - Dimension: PoolName

## Important Patterns

### Message Processing Loop (from events/handler.go)
```go
func (h *Handler) Run(ctx context.Context) {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    const maxConcurrency = 5
    sem := make(chan struct{}, maxConcurrency)

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            messages, err := h.queueClient.ReceiveMessages(ctx, 10, 20)
            if err != nil {
                log.Printf("failed to receive messages: %v", err)
                continue
            }

            var wg sync.WaitGroup
            for _, msg := range messages {
                msg := msg // capture loop variable
                wg.Add(1)
                go func() {
                    defer wg.Done()
                    sem <- struct{}{}        // acquire
                    defer func() { <-sem }() // release
                    h.processMessage(ctx, msg)
                }()
            }

            wg.Wait() // wait for batch to complete
        }
    }
}
```

### Idempotent DynamoDB Update
```go
func (c *Client) MarkJobComplete(ctx context.Context, jobID, status string, exitCode int) error {
    // Use UpdateItem with condition: status must be "running"
    // This prevents marking already-completed jobs (idempotency)
    input := &dynamodb.UpdateItemInput{
        TableName: aws.String(c.tableName),
        Key: map[string]types.AttributeValue{
            "job_id": &types.AttributeValueMemberS{Value: jobID},
        },
        UpdateExpression: aws.String("SET #status = :status, exit_code = :code, completed_at = :time"),
        ConditionExpression: aws.String("#status = :running OR attribute_not_exists(#status)"),
        ExpressionAttributeNames: map[string]string{
            "#status": "status",
        },
        ExpressionAttributeValues: map[string]types.AttributeValue{
            ":status":  &types.AttributeValueMemberS{Value: status},
            ":code":    &types.AttributeValueMemberN{Value: strconv.Itoa(exitCode)},
            ":time":    &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
            ":running": &types.AttributeValueMemberS{Value: "running"},
        },
    }
    _, err := c.client.UpdateItem(ctx, input)
    if err != nil {
        // ConditionalCheckFailedException means already completed - this is OK
        return fmt.Errorf("failed to mark job complete: %w", err)
    }
    return nil
}
```

### Cleanup Task Implementation Pattern
```go
type CleanupTask interface {
    Name() string
    Execute(ctx context.Context) error
}

type OrphanedInstancesTask struct {
    ec2Client   EC2API
    dbClient    DBAPI
    maxRuntime  time.Duration
}

func (t *OrphanedInstancesTask) Execute(ctx context.Context) error {
    // 1. Query EC2 for old instances
    // 2. Check DynamoDB for active job records
    // 3. Terminate instances without active jobs
    // 4. Publish metrics
    return nil
}
```

### Error Handling (from events/handler.go:211-238)
```go
func (h *Handler) retryWithBackoff(ctx context.Context, operation func(context.Context) error) error {
    backoffs := []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}
    var lastErr error

    for attempt := 0; attempt < 3; attempt++ {
        if attempt > 0 {
            select {
            case <-time.After(backoffs[attempt-1]):
            case <-ctx.Done():
                return ctx.Err()
            }
        }

        err := operation(ctx)
        if err == nil {
            return nil
        }
        lastErr = err
    }
    return lastErr
}
```

## Data Flow

### Termination Queue Flow
```
1. Agent completes job execution
2. Agent sends termination message to SQS
   ↓
3. Termination handler receives message (batch of 10)
4. For each message concurrently (max 5):
   a. Update DynamoDB job record (status, exit_code, timestamps)
   b. Delete SSM parameter /runs-fleet/runners/{instance-id}/config
   c. Publish CloudWatch metrics (duration, success/failure)
   d. Delete SQS message (acknowledge)
   ↓
5. Job lifecycle complete
```

### Housekeeping Queue Flow
```
1. EventBridge scheduled rule triggers
2. Rule sends housekeeping message to SQS
   ↓
3. Housekeeping handler receives message
4. Dispatch to appropriate cleanup task:

   orphaned_instances:
   a. Query EC2 for instances > max_runtime
   b. Check DynamoDB for active job records
   c. Terminate instances without active jobs
   d. Publish metric: OrphanedInstancesTerminated

   stale_ssm:
   a. List SSM parameters under /runs-fleet/runners/
   b. Parse instance ID from each parameter
   c. Check if instance still exists in EC2
   d. Delete parameters for non-existent instances
   e. Publish metric: SSMParametersDeleted

   old_jobs:
   a. Query DynamoDB GSI: completed_at < now() - 7 days
   b. Batch delete job records (25 per batch)
   c. Publish metric: JobRecordsArchived

   pool_audit:
   a. Query DynamoDB for all pool configs
   b. For each pool: count running, stopped, idle instances
   c. Calculate idle and compute costs
   d. Generate report (log + publish metrics)
   e. Publish metrics: PoolIdleCost, PoolComputeCost, PoolUtilization
   ↓
5. Delete SQS message (acknowledge)
```

## Next Steps

**To resume work on this feature:**

1. **Start with termination handler** (simpler, blocks agent testing)
   - Create `pkg/termination/handler.go` using `pkg/events/handler.go` as template
   - Copy structure: Handler struct, NewHandler, Run loop, processMessage
   - Adapt message type to TerminationMessage
   - Implement processTermination logic

2. **Add DynamoDB methods** for job completion
   - Update `pkg/db/dynamo.go` with MarkJobComplete method
   - Use conditional updates for idempotency
   - Add QueryOldJobs method for housekeeping

3. **Add CloudWatch metrics** for job tracking
   - Update `pkg/metrics/cloudwatch.go` with job duration/success/failure metrics
   - Follow existing pattern for metric publishing

4. **Integrate with server**
   - Update `cmd/server/main.go:60` to add termination handler
   - Add config field in `pkg/config/config.go`
   - Start goroutine after other handlers (line 118)

5. **Test with real agent** (requires Phase 6 complete)
   - Send test message to termination queue
   - Verify DynamoDB update
   - Verify SSM deletion
   - Verify metrics in CloudWatch

6. **Then housekeeping handler**
   - Create `pkg/housekeeping/handler.go` similar to termination
   - Create `pkg/housekeeping/tasks.go` with CleanupTask interface
   - Implement 4 cleanup tasks
   - Add EventBridge rules in Terraform

**Reference existing code:**
- Queue processing: `pkg/events/handler.go` (complete reference implementation)
- DynamoDB operations: `pkg/db/dynamo.go` (UpdatePoolState pattern)
- CloudWatch metrics: `pkg/metrics/cloudwatch.go` (PublishSpotInterruption pattern)
- Error handling: `pkg/events/handler.go:211-238` (retry with backoff)
- Testing: `pkg/queue/sqs_test.go` (mocking pattern)
