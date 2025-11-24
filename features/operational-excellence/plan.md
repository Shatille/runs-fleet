# Operational Excellence Implementation Plan

## Executive Summary
Add production-grade operational tooling: circuit breaker for spot interruptions, CloudWatch dashboard and alarms, daily cost reporting, and SSH debugging access. These features transform runs-fleet from a functional prototype into a production-ready platform with proper observability and cost control.

## Context

**Current State:**
- Spot interruptions handled but can churn indefinitely (no circuit breaker)
- CloudWatch metrics published but no dashboard or alarms
- No cost visibility or reporting
- No way to debug misbehaving instances
- Production readiness: ~30%

**Why This Change:**
- Spot churn can drive costs 3-5x higher than stable spot allocation
- Incidents take >30 minutes to detect without alarms
- Cost overruns discovered only at month-end (AWS bill shock)
- Debugging requires terminating and recreating instances

**Problems It Solves:**
- Prevents runaway costs from spot interruption loops
- Enables <5 minute mean time to detect (MTTD) for incidents
- Provides daily cost visibility and trending
- Allows debugging live instances without losing state

## Implementation Phases

### Phase 1: Spot Interruption Circuit Breaker
**Goal:** Prevent cost spikes from repeated spot interruptions

**Tasks:**
1. Create `pkg/circuit/breaker.go` - Circuit breaker state machine
   - Track interruptions per instance type in 15/30 minute window
   - Threshold: 2-5 interruptions → open circuit
   - Circuit open → pause spot requests, force on-demand
   - Auto-reset after cooldown period (15-30 minutes)
   - Thread-safe implementation (sync.RWMutex)

2. Define circuit breaker state in DynamoDB
   - Table: `runs-fleet-circuit-state`
   - PK: instance_type (e.g., "t4g.medium")
   - Attributes:
     - state (string): "closed", "open", "half-open"
     - interruption_count (number)
     - first_interruption_at (string, ISO 8601)
     - last_interruption_at (string, ISO 8601)
     - opened_at (string, ISO 8601)
     - auto_reset_at (string, ISO 8601)
   - TTL: auto_reset_at + 1 hour (DynamoDB cleanup)

3. Implement circuit breaker logic
   - `RecordInterruption(ctx, instanceType string) error` - Increment count, check threshold
   - `CheckCircuit(ctx, instanceType string) (CircuitState, error)` - Query current state
   - `ResetCircuit(ctx, instanceType string) error` - Manual reset (emergency bypass)
   - Background goroutine: Check auto_reset_at every 60 seconds, close circuits

4. Integrate with fleet manager
   - Before creating fleet: `circuit.CheckCircuit(ctx, instanceType)`
   - If circuit open: Force `spot=false` in launch spec
   - Log circuit state changes to CloudWatch
   - Publish metric: `CircuitBreakerOpen` (gauge, 0 or 1 per instance type)

5. Integrate with events handler
   - When spot interruption received: `circuit.RecordInterruption(ctx, instanceType)`
   - Publish metric: `CircuitBreakerTriggered` (count)

**Risks:**
- **False positives:** AZ-wide spot interruptions can trigger circuit unnecessarily
- **State synchronization:** Multiple orchestrator instances need consistent state (DynamoDB)
- **Manual intervention:** Need escape hatch to force-close circuit

**Files Created:**
- `pkg/circuit/breaker.go`
- `pkg/circuit/breaker_test.go`

**Files Modified:**
- `pkg/fleet/fleet.go` - Check circuit before creating spot fleet
- `pkg/events/handler.go` - Record interruptions
- `cmd/server/main.go` - Initialize circuit breaker, start reset goroutine

### Phase 2: Forced On-Demand Retry
**Goal:** Ensure interrupted jobs complete on second attempt

**Tasks:**
1. Add retry metadata to job messages
   - Update `pkg/queue/JobMessage` struct:
     - Add `RetryCount int` field
     - Add `ForceOnDemand bool` field
   - Update `pkg/github/webhook.go` label parser to check for `retry=` label

2. Update re-queue logic in events handler
   - When re-queuing interrupted job: Set `RetryCount++` and `ForceOnDemand=true`
   - If `retry=false` label present: Don't re-queue (fail job)
   - If `retry=when-interrupted` (default): Re-queue with on-demand

3. Update fleet manager to honor ForceOnDemand
   - Check `job.ForceOnDemand` before spot allocation
   - If true: Skip spot entirely, create on-demand fleet
   - Log retry attempts and spot->on-demand transitions

**Risks:**
- **Cost increase:** On-demand is 3x more expensive (but ensures job completion)
- **Cascading failures:** If on-demand unavailable, job fails permanently

**Files Modified:**
- `pkg/queue/sqs.go` - Update JobMessage struct
- `pkg/github/webhook.go` - Parse retry label
- `pkg/events/handler.go` - Set ForceOnDemand on re-queue
- `pkg/fleet/fleet.go` - Check ForceOnDemand flag

### Phase 3: CloudWatch Dashboard
**Goal:** Single-pane-of-glass visibility into system health

**Tasks:**
1. Create Terraform dashboard definition
   - File: `terraform/cloudwatch_dashboard.tf` (in separate Terraform repo)
   - Dashboard name: `RunsFleet-Production`
   - 12 widgets in 3 rows (4 columns)

2. Define dashboard widgets:
   **Row 1: Queue Health**
   - Widget 1: Main queue depth (ApproximateNumberOfMessages)
   - Widget 2: Pool queue depth
   - Widget 3: Events queue depth
   - Widget 4: Termination/housekeeping queue depth

   **Row 2: Fleet Metrics**
   - Widget 5: Fleet size (running instances, line chart)
   - Widget 6: Spot interruption rate (count per hour)
   - Widget 7: Job duration (p50, p95, p99)
   - Widget 8: Job success rate (percentage)

   **Row 3: Cost & Performance**
   - Widget 9: Pool utilization (hot vs idle)
   - Widget 10: Cache hit rate
   - Widget 11: Circuit breaker status (gauge per instance type)
   - Widget 12: API errors (EC2, DynamoDB, GitHub)

3. Add missing metrics to codebase
   - `JobSuccessRate` - Calculated metric: successes / (successes + failures)
   - `CacheHitRate` - Add to `pkg/cache/handler.go`
   - `APIErrors` - Add to fleet, db, github packages

**Risks:**
- **Dashboard sprawl:** Too many widgets → hard to read
- **Missing metrics:** Need to backfill metrics not yet published

**Files Created:**
- `terraform/cloudwatch_dashboard.tf` (in shavakan-terraform repo)

**Files Modified:**
- `pkg/cache/handler.go` - Add cache hit/miss metrics
- `pkg/fleet/fleet.go` - Add API error metrics
- `pkg/db/dynamo.go` - Add API error metrics

### Phase 4: CloudWatch Alarms
**Goal:** Proactive alerting for operational issues

**Tasks:**
1. Create alarm definitions in Terraform
   - File: `terraform/cloudwatch_alarms.tf`

2. Define critical alarms:
   - **Queue age >5 minutes:** Jobs backing up, fleet can't keep up
     - Metric: `ApproximateAgeOfOldestMessage` on main queue
     - Threshold: >300 seconds for 2 datapoints
     - Action: SNS topic (email/Slack)

   - **Fleet errors >5 in 5 minutes:** EC2 API failures
     - Metric: `APIErrors` with dimension `Service=EC2`
     - Threshold: >5 for 1 datapoint (5 min period)
     - Action: SNS topic

   - **Spot interruption rate >20%:** High interruption rate suggests capacity issues
     - Metric: `SpotInterruptions` / `FleetSizeIncrement`
     - Threshold: >0.20 for 3 datapoints (15 min)
     - Action: SNS topic + trigger circuit breaker check

   - **Pool exhaustion:** No available instances in pool
     - Metric: `PoolUtilization` per pool
     - Threshold: >0.95 for 2 datapoints
     - Action: SNS topic (may need to increase pool size)

   - **DLQ depth >10 messages:** Messages failing repeatedly
     - Metric: `ApproximateNumberOfMessages` on DLQ
     - Threshold: >10 for 1 datapoint
     - Action: SNS topic (requires manual intervention)

3. Create SNS topic for alerts
   - Topic name: `runs-fleet-alerts`
   - Subscriptions: Email (configurable via Terraform variable)
   - Optional: Slack webhook via Lambda function

**Risks:**
- **Alert fatigue:** Too many false positives → ignored alarms
- **Insufficient coverage:** Missing critical failure modes

**Files Created:**
- `terraform/cloudwatch_alarms.tf`
- `terraform/sns_topics.tf`

### Phase 5: Daily Cost Reporting
**Goal:** Visibility into AWS spend and cost optimization opportunities

**Tasks:**
1. Create `pkg/cost/reporter.go` - Cost calculation and reporting
   - Query CloudWatch metrics for previous 24 hours
   - Calculate EC2 costs:
     - Instance hours by type and pricing (spot vs on-demand)
     - Use AWS Pricing API or hardcoded rates
     - Formula: hours * rate * instance_count
   - Calculate S3 costs:
     - Storage GB-months (from S3 bucket metrics)
     - Request costs (PutObject, GetObject counts)
   - Calculate supporting service costs (estimates):
     - Fargate: Fixed $36/month (1 vCPU, 2 GB)
     - SQS: Requests * $0.40 per million
     - DynamoDB: PAY_PER_REQUEST pricing
     - CloudWatch: Logs ingested * $0.50/GB

2. Generate markdown report
   - Report structure:
     ```markdown
     # runs-fleet Daily Cost Report
     **Date:** 2025-11-25
     **24-Hour Total:** $2.34

     ## EC2 Compute
     - Spot instances: $1.20 (45 instance-hours)
       - t4g.medium: $0.60 (30h @ $0.02/h)
       - c7g.xlarge: $0.60 (15h @ $0.04/h)
     - On-demand: $0.40 (5 instance-hours, fallback from spot)
     - **Spot savings:** 75% ($0.80 saved)

     ## Storage & Data
     - S3 cache: $0.30 (storage + requests)
     - EBS volumes: $0.10 (stopped pool instances)

     ## Supporting Services
     - Fargate orchestrator: $1.20 (1/30 of monthly cost)
     - SQS: $0.05 (12,000 requests)
     - DynamoDB: $0.08 (15,000 read/write units)
     - CloudWatch: $0.01 (logs + metrics)

     ## Cost Trends
     - 7-day average: $2.10/day
     - 30-day average: $2.05/day
     - Month-to-date: $51.25

     ## Recommendations
     - Pool "default" idle 60% of time → consider stopped pool
     - Instance type "c7g.xlarge" used only 2x → remove from fleet
     ```

3. Implement cost tracking
   - Add `GetCostMetrics(ctx, startTime, endTime time.Time) (*CostBreakdown, error)` method
   - Query CloudWatch for: FleetSize, InstanceHours, PoolUtilization
   - Query S3 for: BucketSizeBytes, NumberOfObjects
   - Calculate costs using pricing data

4. Schedule via EventBridge
   - Rule: `runs-fleet-daily-cost-report`
   - Schedule: `cron(0 9 * * ? *)` (9am daily, configurable timezone)
   - Target: Housekeeping queue with message: `{task_type: "cost_report"}`
   - Handler: New task in `pkg/housekeeping/tasks.go`

5. Delivery mechanisms
   - Publish to SNS topic (email subscribers receive report)
   - Store in S3 bucket: `runs-fleet-reports/cost/{YYYY}/{MM}/{DD}.md`
   - Optional: Post to Slack webhook (env var `RUNS_FLEET_SLACK_WEBHOOK_URL`)

**Risks:**
- **Pricing data stale:** AWS pricing changes, hardcoded rates outdated
- **Cost estimation errors:** ±20% accuracy acceptable for daily reports
- **Report spam:** Daily emails may be too frequent (consider weekly digest)

**Files Created:**
- `pkg/cost/reporter.go`
- `pkg/cost/reporter_test.go`

**Files Modified:**
- `pkg/housekeeping/tasks.go` - Add CostReportTask
- `terraform/eventbridge_rules.tf` - Add cost report schedule

### Phase 6: SSH Debugging Access
**Goal:** Debug live instances without terminating them

**Tasks:**
1. Update security group in Terraform
   - Add inbound rule: Port 22 (SSH), source CIDR from variable
   - Variable: `ssh_allowed_cidr` (e.g., "203.0.113.0/24" or "0.0.0.0/0" for dev)
   - Apply to both public and private instance security groups

2. Configure SSH key in launch template
   - Terraform variable: `ssh_key_name` (EC2 KeyPair name)
   - Pass to launch template `key_name` field
   - Document SSH access in README.md

3. Optional: AWS Systems Manager Session Manager
   - Alternative to SSH (no key management needed)
   - Requires SSM agent on instances (pre-installed on Amazon Linux 2)
   - Update IAM instance profile: Add `AmazonSSMManagedInstanceCore` policy
   - Access via: `aws ssm start-session --target i-0123456789abcdef0`
   - Benefits: No inbound port 22, audit logging, no key distribution

4. Document debugging procedures
   - How to SSH to instance: `ssh -i key.pem ec2-user@<public-ip>`
   - How to use Session Manager: AWS CLI commands
   - Common debugging tasks:
     - Check runner logs: `sudo journalctl -u actions-runner`
     - Check disk space: `df -h`
     - Check agent logs: `tail -f /var/log/runs-fleet-agent.log`
     - Check for zombie processes: `ps aux | grep runner`
   - Security note: Sessions are logged to CloudWatch

**Risks:**
- **Security exposure:** Open SSH port increases attack surface (mitigate with CIDR restriction)
- **Key management:** Distributing SSH keys to team members
- **Session logging:** Need to audit who accessed what when

**Files Modified:**
- `terraform/security_groups.tf` - Add SSH rule
- `terraform/launch_templates.tf` - Add key_name
- `terraform/variables.tf` - Add ssh_key_name and ssh_allowed_cidr
- `terraform/iam_roles.tf` - Add SSM policy for Session Manager
- README.md - Document SSH and Session Manager access

## Testing Strategy

### Circuit Breaker Testing
- Unit test: Record 5 interruptions, verify circuit opens
- Unit test: Auto-reset after cooldown period
- Integration test: Trigger real spot interruptions (terminate 3 instances quickly)
- Integration test: Verify on-demand fallback when circuit open
- Load test: 100 concurrent interruptions (check DynamoDB contention)

### Dashboard & Alarms Testing
- Verify all metrics appear in dashboard
- Trigger each alarm condition manually (e.g., fill queue, stop instances)
- Verify SNS notifications received
- Verify alarm auto-resolves when condition clears

### Cost Reporting Testing
- Run cost report manually via housekeeping queue
- Verify report calculation accuracy (±20%)
- Verify S3 storage and SNS delivery
- Test with zero activity (should report minimal costs)

### SSH Access Testing
- SSH to running instance with key
- Use Session Manager to access instance
- Verify CloudWatch session logs
- Test from restricted CIDR (should succeed) and outside CIDR (should fail)

## Success Metrics
- **Circuit breaker:** Spot churn events reduced by >80%
- **MTTD:** Critical incidents detected in <5 minutes (alarm latency)
- **Cost visibility:** Daily cost variance <10% (report accuracy)
- **Debugging:** Mean time to resolution (MTTR) reduced by 50% (SSH access)

## Dependencies
- Phase 6 (Agent) and Phase 7 (Queue Processors) must be complete for full observability
- Terraform infrastructure repository access for dashboard/alarms
- AWS Pricing API access for cost calculations (or hardcoded pricing)
- SNS topic for alerting (email/Slack subscriptions)

## Timeline
- **Phase 1 (Circuit Breaker):** 3 days
- **Phase 2 (Forced Retry):** 2 days
- **Phase 3 (Dashboard):** 3 days
- **Phase 4 (Alarms):** 2 days
- **Phase 5 (Cost Reporting):** 4 days
- **Phase 6 (SSH Access):** 1 day
- **Testing:** 2 days

**Total: 17 days (2-3 weeks)**

## Next Steps After Completion
1. Monitor circuit breaker triggers (should be rare in steady state)
2. Tune alarm thresholds based on false positive rate
3. Review cost reports weekly, identify optimization opportunities
4. Use SSH/Session Manager to debug production issues
5. Proceed to Phase 9 (Advanced Features)
