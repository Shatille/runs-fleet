# Queue Processors Implementation Plan

## Executive Summary
Implement two missing queue processors: termination queue (handles instance lifecycle notifications) and housekeeping queue (background cleanup tasks). These processors are documented in the architecture but not implemented, causing orphaned resources and incomplete job state tracking.

## Context

**Current State:**
- Termination queue: Documented in README.md:56, architecture expects it, but no processor exists
- Housekeeping queue: Documented in README.md:55, mentioned in data flow, but no processor exists
- Events queue processor: ✅ Implemented at `pkg/events/handler.go`
- Main/pool queue processors: ✅ Implemented in `cmd/server/main.go`

**Why This Change:**
- Agent sends termination notifications (Phase 6) but nothing consumes them
- DynamoDB job records never get marked complete
- SSM parameters accumulate indefinitely
- Orphaned instances don't get detected or cleaned up
- S3 cache lifecycle relies on bucket policies, not active management

**Problems It Solves:**
- Completes job lifecycle tracking (queued → running → completed/failed)
- Prevents DynamoDB table bloat
- Detects and terminates zombie instances
- Cleans up abandoned SSM parameters
- Provides visibility into job completion status

## Implementation Phases

### Phase 1: Termination Queue Processor
**Goal:** Process instance termination notifications from agents

**Tasks:**
1. Create `pkg/termination/handler.go` - Queue processor for termination events
   - Similar structure to `pkg/events/handler.go` (reference implementation)
   - Implement `Handler` struct with `Run(ctx)` method
   - Use 10-message batch receive with 20s long polling
   - Process messages concurrently (max 5 workers)

2. Define message format and parsing
   - Create `TerminationMessage` struct:
     ```go
     type TerminationMessage struct {
         InstanceID      string    `json:"instance_id"`
         JobID           string    `json:"job_id"`
         Status          string    `json:"status"` // success, failure, timeout, interrupted
         ExitCode        int       `json:"exit_code"`
         DurationSeconds int       `json:"duration_seconds"`
         StartedAt       time.Time `json:"started_at"`
         CompletedAt     time.Time `json:"completed_at"`
         Error           string    `json:"error,omitempty"`
     }
     ```
   - Validate required fields (instance_id, job_id, status)
   - Handle malformed JSON gracefully

3. Update DynamoDB job state
   - Add methods to `pkg/db/dynamo.go`:
     - `MarkJobComplete(ctx, jobID, status string, exitCode, duration int) error`
     - `UpdateJobMetrics(ctx, jobID string, startedAt, completedAt time.Time) error`
   - Update workflow-jobs table with completion status
   - Preserve job records for 7 days (housekeeping will clean up)

4. Clean up SSM parameters
   - Delete `/runs-fleet/runners/{instance-id}/config` parameter
   - Handle "parameter not found" errors gracefully (may already be cleaned)
   - Log SSM deletion failures but don't fail message processing

5. Publish CloudWatch metrics
   - Add methods to `pkg/metrics/cloudwatch.go`:
     - `PublishJobDuration(ctx context.Context, duration int) error`
     - `PublishJobSuccess(ctx context.Context) error`
     - `PublishJobFailure(ctx context.Context) error`
   - Metrics namespace: `RunsFleet`
   - Dimensions: `Environment=production`, `Status={status}`

6. Integration with server
   - Update `cmd/server/main.go` to start termination handler
   - Add `RUNS_FLEET_TERMINATION_QUEUE_URL` to config
   - Start goroutine: `go terminationHandler.Run(ctx)`
   - Ensure graceful shutdown on SIGTERM

**Risks:**
- **Message loss:** Use DLQ after 3 retry attempts
- **Duplicate processing:** DynamoDB updates should be idempotent
- **High throughput:** Batch processing handles up to 100 jobs/second

**Files Created:**
- `pkg/termination/handler.go`
- `pkg/termination/handler_test.go`

**Files Modified:**
- `cmd/server/main.go` - Add termination handler startup
- `pkg/db/dynamo.go` - Add job completion methods
- `pkg/metrics/cloudwatch.go` - Add job metrics methods
- `pkg/config/config.go` - Add TerminationQueueURL field

### Phase 2: Housekeeping Queue Processor
**Goal:** Background cleanup tasks triggered by scheduled EventBridge rules

**Tasks:**
1. Create `pkg/housekeeping/handler.go` - Queue processor for cleanup tasks
   - Similar structure to termination handler
   - Support multiple cleanup task types (discriminated union)
   - Process messages sequentially (order matters for some tasks)

2. Define housekeeping message types
   - Create `HousekeepingMessage` struct:
     ```go
     type HousekeepingMessage struct {
         TaskType  string          `json:"task_type"` // orphaned_instances, stale_ssm, old_jobs, pool_audit
         Timestamp time.Time       `json:"timestamp"`
         Params    json.RawMessage `json:"params,omitempty"`
     }
     ```
   - Task types:
     - `orphaned_instances`: Detect instances running > max_runtime
     - `stale_ssm`: Clean up SSM parameters > 24 hours old
     - `old_jobs`: Archive/delete DynamoDB job records > 7 days
     - `pool_audit`: Report on pool utilization and costs

3. Implement cleanup tasks
   - **Orphaned instances:**
     - Query EC2 for instances with tag `runs-fleet:managed=true`
     - Filter by launch time > `max_runtime` + 10 minutes buffer
     - Check if instance has active job in DynamoDB (may be legitimate long job)
     - Terminate instances with no active job record
     - Publish metric: `OrphanedInstancesTerminated`

   - **Stale SSM parameters:**
     - List parameters under `/runs-fleet/runners/` prefix
     - Parse instance ID from parameter name
     - Query EC2 to check if instance still exists
     - Delete parameters for non-existent instances
     - Handle pagination (100 parameters per API call)
     - Publish metric: `SSMParametersDeleted`

   - **Old job records:**
     - Query DynamoDB workflow-jobs table with filter: `completed_at < now() - 7 days`
     - Use GSI on completed_at for efficient queries
     - Batch delete job records (25 per batch)
     - Optional: Archive to S3 before deletion (for analytics)
     - Publish metric: `JobRecordsArchived`

   - **Pool audit:**
     - Query DynamoDB pools table for all pool configurations
     - For each pool: count running, stopped, idle instances
     - Calculate idle costs: stopped instances * EBS GB * $0.10/GB-month
     - Calculate compute costs: running instances * hourly rate * hours
     - Generate report and publish to SNS topic (future: email/Slack)
     - Publish metrics: `PoolIdleCost`, `PoolComputeCost`, `PoolUtilization`

4. EventBridge schedule configuration
   - Create scheduled rules in Terraform:
     - Every 15 minutes: `orphaned_instances` check
     - Every 1 hour: `stale_ssm` cleanup
     - Daily at 2am: `old_jobs` archival
     - Daily at 9am: `pool_audit` report
   - Rules send JSON messages to housekeeping queue

5. Integration with server
   - Update `cmd/server/main.go` to start housekeeping handler
   - Add `RUNS_FLEET_HOUSEKEEPING_QUEUE_URL` to config
   - Start goroutine: `go housekeepingHandler.Run(ctx)`
   - Ensure graceful shutdown on SIGTERM

**Risks:**
- **Long-running tasks:** Use context timeouts (5 min per task)
- **EC2 pagination:** Handle large fleets (>100 instances)
- **DynamoDB costs:** Batch operations and use PAY_PER_REQUEST billing
- **False positives:** Don't terminate instances with active jobs

**Files Created:**
- `pkg/housekeeping/handler.go`
- `pkg/housekeeping/tasks.go` - Individual cleanup task implementations
- `pkg/housekeeping/handler_test.go`
- `pkg/housekeeping/tasks_test.go`

**Files Modified:**
- `cmd/server/main.go` - Add housekeeping handler startup
- `pkg/config/config.go` - Add HousekeepingQueueURL field

## Testing Strategy

### Unit Tests
- `pkg/termination/handler_test.go` - Mock SQS, DynamoDB, SSM clients
- `pkg/housekeeping/tasks_test.go` - Test each cleanup task independently
- Test message parsing with valid/invalid JSON
- Test idempotency (processing same message twice)
- Test error handling (AWS API failures, timeouts)

### Integration Tests
- **Termination flow:**
  - Send mock termination message to queue
  - Verify DynamoDB job record updated
  - Verify SSM parameter deleted
  - Verify CloudWatch metrics published
  - Verify message deleted from queue

- **Housekeeping flow:**
  - Create orphaned instance (no job record)
  - Trigger orphaned_instances cleanup
  - Verify instance terminated
  - Create stale SSM parameter
  - Trigger stale_ssm cleanup
  - Verify parameter deleted

### Local Testing
- Use LocalStack for SQS, DynamoDB, SSM, EC2 mocks
- Create `docker-compose.yml` for local testing environment
- Test all task types end-to-end

### Validation Checklist
- [ ] Termination messages processed successfully
- [ ] DynamoDB job records marked complete
- [ ] SSM parameters cleaned up
- [ ] CloudWatch metrics appear in console
- [ ] Orphaned instances detected and terminated
- [ ] Stale SSM parameters deleted
- [ ] Old job records archived/deleted
- [ ] Pool audit report generated
- [ ] No message loss (DLQ remains empty)
- [ ] Graceful shutdown works (in-flight messages complete)

## Success Metrics
- **Termination processor:**
  - Message processing latency <500ms (p99)
  - Zero message loss (DLQ depth = 0)
  - Job completion rate = 100% (all jobs eventually marked complete)

- **Housekeeping processor:**
  - Orphaned instances <1% of total fleet
  - SSM parameter count <10 per day
  - DynamoDB table size <100MB (with 7-day retention)
  - Pool audit reports accurate (±5% cost estimate)

## Dependencies
- Phase 6 (Agent Implementation) must be complete for termination messages
- Terraform infrastructure must deploy both queues with DLQs
- EventBridge rules must be configured for housekeeping schedule
- DynamoDB GSI on completed_at for efficient old job queries

## Timeline
- **Phase 1 (Termination):** 3 days (handler + DB updates + metrics)
- **Phase 2 (Housekeeping):** 5 days (4 cleanup tasks + testing)
- **Testing:** 2 days (integration tests + validation)

**Total: 10 days (1-2 weeks)**

## Next Steps After Completion
1. Monitor termination queue depth (should be near-zero)
2. Monitor DLQ depth (should remain zero)
3. Verify job completion rates in CloudWatch dashboard
4. Check housekeeping logs for cleanup statistics
5. Proceed to Phase 8 (Operational Excellence)
