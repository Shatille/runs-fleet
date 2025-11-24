# Queue Processors - Task Checklist

**Last Updated:** 2025-11-25

## Phase 1: Termination Queue Processor

### Handler Implementation
- [ ] Create `pkg/termination/handler.go` with package documentation
- [ ] Define `Handler` struct with queueClient, dbClient, metricsClient fields
- [ ] Implement `NewHandler(q QueueAPI, db DBAPI, m MetricsAPI) *Handler` constructor
- [ ] Define `TerminationMessage` struct matching JSON format from agent
- [ ] Implement `Run(ctx context.Context)` loop using `pkg/events/handler.go:94-165` pattern
- [ ] Implement `processMessage(ctx context.Context, msg types.Message)` handler
- [ ] Implement `processTermination(ctx context.Context, tm *TerminationMessage) error` business logic
- [ ] Add retry with backoff for transient failures (copy from events/handler.go:211-238)
- [ ] Handle graceful shutdown (wait for in-flight messages)

### DynamoDB Job Completion
- [ ] Add `MarkJobComplete(ctx, jobID, status string, exitCode, duration int) error` to `pkg/db/dynamo.go`
- [ ] Use conditional update: `ConditionExpression` to check current status = "running"
- [ ] Handle `ConditionalCheckFailedException` as success (idempotency)
- [ ] Add `UpdateJobMetrics(ctx, jobID string, startedAt, completedAt time.Time) error` method
- [ ] Add error wrapping with context: `fmt.Errorf("failed to mark job complete: %w", err)`

### SSM Parameter Cleanup
- [ ] Add `DeleteRunnerParameter(ctx context.Context, instanceID string) error` to handler
- [ ] Use SSM client from `pkg/config` to delete parameter
- [ ] Parameter path: `/runs-fleet/runners/{instance-id}/config`
- [ ] Handle `ParameterNotFound` error gracefully (already deleted, log and continue)
- [ ] Add retry logic for transient SSM failures

### CloudWatch Metrics
- [ ] Add `PublishJobDuration(ctx context.Context, duration int) error` to `pkg/metrics/cloudwatch.go`
- [ ] Add `PublishJobSuccess(ctx context.Context) error` method
- [ ] Add `PublishJobFailure(ctx context.Context) error` method
- [ ] Use namespace `RunsFleet` and appropriate dimensions
- [ ] Follow existing pattern from `PublishSpotInterruption` method

### Server Integration
- [ ] Add `TerminationQueueURL string` field to `pkg/config/config.go` Config struct
- [ ] Read from `RUNS_FLEET_TERMINATION_QUEUE_URL` environment variable in `pkg/config/config.go:Load()`
- [ ] Validate termination queue URL is set (fatal if missing)
- [ ] Create termination handler in `cmd/server/main.go` after line 60
- [ ] Start termination handler goroutine after line 118: `go terminationHandler.Run(ctx)`
- [ ] Verify graceful shutdown (context cancellation propagates)

### Testing
- [ ] Create `pkg/termination/handler_test.go`
- [ ] Test: processTermination with valid message
- [ ] Test: processTermination with invalid JSON
- [ ] Test: processTermination with missing fields
- [ ] Test: DynamoDB update failure (retry logic)
- [ ] Test: SSM parameter already deleted (no error)
- [ ] Test: Idempotency (process same message twice)
- [ ] Test: Graceful shutdown (in-flight messages complete)
- [ ] Integration test: Send message to real SQS, verify DynamoDB updated

## Phase 2: Housekeeping Queue Processor

### Handler Implementation
- [ ] Create `pkg/housekeeping/handler.go` with package documentation
- [ ] Define `Handler` struct with queueClient, ec2Client, ssmClient, dbClient, metricsClient
- [ ] Implement `NewHandler(...)` constructor
- [ ] Define `HousekeepingMessage` struct with task_type, timestamp, params fields
- [ ] Implement `Run(ctx context.Context)` loop (similar to termination handler)
- [ ] Implement `processMessage(ctx context.Context, msg types.Message)` handler
- [ ] Implement `dispatchTask(ctx context.Context, hm *HousekeepingMessage) error` dispatcher
- [ ] Route task_type to appropriate cleanup task implementation

### Cleanup Tasks Interface
- [ ] Create `pkg/housekeeping/tasks.go` with package documentation
- [ ] Define `CleanupTask` interface:
  ```go
  type CleanupTask interface {
      Name() string
      Execute(ctx context.Context) error
  }
  ```
- [ ] Create task registry pattern for dispatching tasks

### Orphaned Instances Task
- [ ] Implement `OrphanedInstancesTask` struct in `tasks.go`
- [ ] Implement `Execute(ctx context.Context) error` method:
  - [ ] Query EC2 for instances with tag `runs-fleet:managed=true` and state=running
  - [ ] Filter instances with launch_time < now() - max_runtime - 10 min buffer
  - [ ] For each instance: check DynamoDB for active job record
  - [ ] Terminate instances without active job records
  - [ ] Handle EC2 API pagination (DescribeInstances returns 1000 max)
  - [ ] Log terminated instance IDs
  - [ ] Publish metric: `OrphanedInstancesTerminated` (count)
- [ ] Add retry logic for EC2 API throttling
- [ ] Test with mocked EC2 client

### Stale SSM Parameters Task
- [ ] Implement `StaleSSMTask` struct in `tasks.go`
- [ ] Implement `Execute(ctx context.Context) error` method:
  - [ ] List SSM parameters with prefix `/runs-fleet/runners/`
  - [ ] Handle SSM API pagination (100 parameters per call)
  - [ ] Parse instance ID from parameter name
  - [ ] Query EC2 to check if instance still exists
  - [ ] Delete parameters for non-existent instances
  - [ ] Batch delete operations (10 per batch for performance)
  - [ ] Log deleted parameter paths
  - [ ] Publish metric: `SSMParametersDeleted` (count)
- [ ] Handle SSM API rate limiting
- [ ] Test with mocked SSM and EC2 clients

### Old Jobs Archival Task
- [ ] Implement `OldJobsTask` struct in `tasks.go`
- [ ] Add DynamoDB GSI on `completed_at` field (if not exists, add to Terraform)
- [ ] Implement `Execute(ctx context.Context) error` method:
  - [ ] Query DynamoDB using GSI: `completed_at < now() - 7 days`
  - [ ] Handle DynamoDB pagination (1MB per query)
  - [ ] Optional: Archive to S3 before deletion (JSON Lines format)
  - [ ] Batch delete job records (25 per batch)
  - [ ] Log count of archived/deleted records
  - [ ] Publish metric: `JobRecordsArchived` (count)
- [ ] Add `QueryOldJobs(ctx, cutoff time.Time) ([]JobRecord, error)` to `pkg/db/dynamo.go`
- [ ] Add `BatchDeleteJobs(ctx, jobIDs []string) error` to `pkg/db/dynamo.go`
- [ ] Test with mocked DynamoDB client

### Pool Audit Task
- [ ] Implement `PoolAuditTask` struct in `tasks.go`
- [ ] Implement `Execute(ctx context.Context) error` method:
  - [ ] Query DynamoDB pools table for all pool configurations
  - [ ] For each pool:
    - [ ] Count running instances (query EC2 with pool tag)
    - [ ] Count stopped instances (state=stopped, pool tag)
    - [ ] Count idle instances (running but not assigned to job)
    - [ ] Calculate idle cost: stopped instances * EBS GB * $0.10/GB-month / 730 hours
    - [ ] Calculate compute cost: running instances * hourly rate * uptime hours
    - [ ] Calculate utilization: assigned instances / (running + stopped)
  - [ ] Generate markdown report with costs per pool
  - [ ] Log report summary
  - [ ] Publish metrics: `PoolIdleCost`, `PoolComputeCost`, `PoolUtilization` per pool
- [ ] Add instance pricing lookup (hardcoded map or AWS Pricing API)
- [ ] Test with mocked EC2 and DynamoDB clients

### Server Integration
- [ ] Add `HousekeepingQueueURL string` field to `pkg/config/config.go` Config struct
- [ ] Read from `RUNS_FLEET_HOUSEKEEPING_QUEUE_URL` environment variable
- [ ] Validate housekeeping queue URL is set
- [ ] Create housekeeping handler in `cmd/server/main.go` after termination handler
- [ ] Start housekeeping handler goroutine: `go housekeepingHandler.Run(ctx)`
- [ ] Verify graceful shutdown

### Testing
- [ ] Create `pkg/housekeeping/handler_test.go`
- [ ] Create `pkg/housekeeping/tasks_test.go`
- [ ] Test: Each cleanup task independently with mocked clients
- [ ] Test: Task dispatcher routes correctly
- [ ] Test: Housekeeping message parsing (valid/invalid JSON)
- [ ] Test: Long-running task respects context timeout
- [ ] Test: Error handling for each task type
- [ ] Integration test: Trigger each task via SQS message

## Infrastructure Updates

### Terraform Changes
- [ ] Create termination queue in Terraform (if not exists)
  - [ ] Queue name: `runs-fleet-termination.fifo`
  - [ ] FIFO queue with content-based deduplication
  - [ ] Visibility timeout: 120 seconds
  - [ ] Dead letter queue after 3 retries
  - [ ] Retention: 14 days
- [ ] Create housekeeping queue in Terraform (if not exists)
  - [ ] Queue name: `runs-fleet-housekeeping`
  - [ ] Standard queue (order doesn't matter)
  - [ ] Visibility timeout: 300 seconds (5 min for long tasks)
  - [ ] Dead letter queue after 3 retries
  - [ ] Retention: 14 days
- [ ] Add EventBridge rules for housekeeping schedule:
  - [ ] Rule: orphaned_instances (every 15 minutes)
  - [ ] Rule: stale_ssm (every 1 hour)
  - [ ] Rule: old_jobs (daily at 2am)
  - [ ] Rule: pool_audit (daily at 9am)
  - [ ] Target: housekeeping queue
- [ ] Add DynamoDB GSI on workflow-jobs table:
  - [ ] GSI name: `completed_at-index`
  - [ ] Partition key: status (string)
  - [ ] Sort key: completed_at (string, ISO 8601)
  - [ ] Projection: ALL attributes
- [ ] Update Fargate task definition environment variables:
  - [ ] Add `RUNS_FLEET_TERMINATION_QUEUE_URL`
  - [ ] Add `RUNS_FLEET_HOUSEKEEPING_QUEUE_URL`
- [ ] Update IAM task role permissions:
  - [ ] `sqs:ReceiveMessage` on termination queue
  - [ ] `sqs:DeleteMessage` on termination queue
  - [ ] `sqs:ReceiveMessage` on housekeeping queue
  - [ ] `sqs:DeleteMessage` on housekeeping queue
  - [ ] `dynamodb:UpdateItem` on workflow-jobs table
  - [ ] `dynamodb:Query` on workflow-jobs table with GSI
  - [ ] `dynamodb:BatchWriteItem` on workflow-jobs table
  - [ ] `ssm:DeleteParameter` on `/runs-fleet/runners/*`
  - [ ] `ssm:ListParameters` (for stale_ssm task)
  - [ ] `ec2:DescribeInstances` (for orphaned instances task)
  - [ ] `ec2:TerminateInstances` (for orphaned instances task)

### Environment Variables
- [ ] Add to `.env.example` or documentation:
  ```
  RUNS_FLEET_TERMINATION_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/.../runs-fleet-termination.fifo
  RUNS_FLEET_HOUSEKEEPING_QUEUE_URL=https://sqs.ap-northeast-1.amazonaws.com/.../runs-fleet-housekeeping
  ```

## Validation Checklist

### Termination Queue
- [ ] Agent sends termination message successfully (Phase 6 required)
- [ ] Termination handler receives and processes message
- [ ] DynamoDB job record updated with status="completed"
- [ ] SSM parameter deleted
- [ ] CloudWatch metrics published (JobDuration, JobSuccess)
- [ ] SQS message deleted (queue depth returns to zero)
- [ ] Duplicate message processed idempotently (no errors)
- [ ] DLQ remains empty (no failed messages)

### Housekeeping Queue
- [ ] Orphaned instances task:
  - [ ] Detects instance running > max_runtime
  - [ ] Does NOT terminate instance with active job
  - [ ] Terminates instance without job record
  - [ ] Publishes metric: OrphanedInstancesTerminated
- [ ] Stale SSM task:
  - [ ] Detects parameter for terminated instance
  - [ ] Deletes stale parameter
  - [ ] Does NOT delete parameter for running instance
  - [ ] Publishes metric: SSMParametersDeleted
- [ ] Old jobs task:
  - [ ] Queries jobs completed > 7 days ago
  - [ ] Deletes old job records
  - [ ] Does NOT delete recent jobs
  - [ ] Publishes metric: JobRecordsArchived
- [ ] Pool audit task:
  - [ ] Calculates costs per pool correctly (±10%)
  - [ ] Reports utilization percentage
  - [ ] Publishes metrics: PoolIdleCost, PoolComputeCost, PoolUtilization
- [ ] EventBridge rules trigger correctly on schedule
- [ ] All tasks complete within 5-minute timeout
- [ ] DLQ remains empty

### End-to-End Flow
- [ ] Run complete workflow: webhook → fleet → agent → termination → housekeeping
- [ ] Verify no orphaned resources after 24 hours
- [ ] Verify DynamoDB table size stays stable (<100MB)
- [ ] Verify SSM parameter count stays low (<10)
- [ ] Verify CloudWatch metrics appear correctly
- [ ] Verify graceful shutdown works (deploy new version)

## Documentation
- [ ] Update README.md queue processor status (mark as ✅ Complete)
- [ ] Update architecture diagram to show termination flow
- [ ] Document housekeeping task schedule and purpose
- [ ] Add troubleshooting section for queue processing errors
- [ ] Document DLQ monitoring and manual intervention process

## Post-Completion
- [ ] Monitor termination queue depth (should be near-zero at steady state)
- [ ] Monitor DLQ depth (should remain at zero)
- [ ] Check CloudWatch logs for processing errors
- [ ] Verify job completion rates in CloudWatch dashboard
- [ ] Check housekeeping logs daily for cleanup statistics
- [ ] Mark Phase 7 as ✅ Complete in README.md
- [ ] Proceed to Phase 8: Operational Excellence
