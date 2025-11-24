# Operational Excellence - Task Checklist

**Last Updated:** 2025-11-25

## Phase 1: Circuit Breaker

- [ ] Create DynamoDB table `runs-fleet-circuit-state` in Terraform
- [ ] Create `pkg/circuit/breaker.go` with CircuitBreaker struct
- [ ] Implement `RecordInterruption(ctx, instanceType) error`
- [ ] Implement `CheckCircuit(ctx, instanceType) (CircuitState, error)`
- [ ] Implement `ResetCircuit(ctx, instanceType) error`
- [ ] Add background goroutine for auto-reset checking
- [ ] Integrate with `pkg/fleet/fleet.go` (check before spot)
- [ ] Integrate with `pkg/events/handler.go` (record interruptions)
- [ ] Add metrics: CircuitBreakerTriggered, CircuitBreakerOpen
- [ ] Unit tests with mocked DynamoDB
- [ ] Integration test: Trigger 3 interruptions, verify circuit opens

## Phase 2: Forced On-Demand Retry

- [ ] Add `RetryCount int` to `pkg/queue/JobMessage`
- [ ] Add `ForceOnDemand bool` to `pkg/queue/JobMessage`
- [ ] Update `pkg/github/webhook.go` to parse `retry=` label
- [ ] Update `pkg/events/handler.go` re-queue logic
- [ ] Update `pkg/fleet/fleet.go` to check ForceOnDemand flag
- [ ] Test: Spot interruption → re-queue with ForceOnDemand=true

## Phase 3: CloudWatch Dashboard

- [ ] Create `terraform/cloudwatch_dashboard.tf`
- [ ] Define 12 dashboard widgets (queue, fleet, cost/perf)
- [ ] Add `CacheHitRate` metric to `pkg/cache/handler.go`
- [ ] Add `APIErrors` metric to fleet, db, github packages
- [ ] Deploy dashboard via Terraform
- [ ] Verify all metrics appear correctly

## Phase 4: CloudWatch Alarms

- [ ] Create `terraform/cloudwatch_alarms.tf`
- [ ] Create SNS topic `runs-fleet-alerts`
- [ ] Define 5 critical alarms (queue age, errors, interruptions, pool, DLQ)
- [ ] Configure SNS email subscription (Terraform variable)
- [ ] Test each alarm trigger condition manually
- [ ] Verify SNS notifications received

## Phase 5: Daily Cost Reporting

- [ ] Create `pkg/cost/reporter.go`
- [ ] Implement `CalculateDailyCosts(ctx, date) (*CostBreakdown, error)`
- [ ] Implement cost calculation for each service (EC2, S3, etc.)
- [ ] Add pricing map or integrate AWS Pricing API
- [ ] Generate markdown report
- [ ] Add CostReportTask to `pkg/housekeeping/tasks.go`
- [ ] Create EventBridge rule for daily 9am trigger
- [ ] Store reports in S3: `runs-fleet-reports/cost/{YYYY}/{MM}/{DD}.md`
- [ ] Publish to SNS topic
- [ ] Test report accuracy (±20%)

## Phase 6: SSH Debugging Access

- [ ] Add SSH security group rule in Terraform (port 22)
- [ ] Add Terraform variables: `ssh_key_name`, `ssh_allowed_cidr`
- [ ] Update launch templates with key_name
- [ ] Add SSM policy to IAM instance profile
- [ ] Document SSH access in README.md
- [ ] Document Session Manager access in README.md
- [ ] Test SSH access from allowed CIDR
- [ ] Test Session Manager access

## Validation

- [ ] Circuit breaker triggers on 3 interruptions
- [ ] Circuit auto-resets after cooldown
- [ ] Forced retry uses on-demand pricing
- [ ] Dashboard shows all metrics
- [ ] Alarms trigger correctly
- [ ] Cost report calculations accurate
- [ ] SSH/Session Manager access works
