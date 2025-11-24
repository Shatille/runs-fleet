# Operational Excellence - Context & Key Decisions

**Last Updated:** 2025-11-25

## Key Files

### Reference Implementations
- `pkg/events/handler.go` - Concurrency and error handling patterns
- `pkg/metrics/cloudwatch.go` - Metrics publishing
- `pkg/db/dynamo.go` - DynamoDB operations with conditional updates

### To Be Created
- `pkg/circuit/breaker.go` - Circuit breaker state machine
- `pkg/circuit/breaker_test.go`
- `pkg/cost/reporter.go` - Cost calculation and reporting
- `pkg/cost/reporter_test.go`

### To Be Modified
- `pkg/fleet/fleet.go` - Check circuit before spot requests
- `pkg/events/handler.go` - Record interruptions in circuit
- `pkg/queue/sqs.go` - Add RetryCount, ForceOnDemand to JobMessage
- `pkg/cache/handler.go` - Add cache hit/miss metrics
- `cmd/server/main.go` - Initialize circuit breaker
- Terraform files (separate repo): dashboard, alarms, SSH access

## Architectural Decisions

### 1. Circuit Breaker in DynamoDB
**Decision:** Store circuit state in DynamoDB instead of in-memory
**Rationale:** Multiple orchestrator instances need shared state
**Impact:** Consistent behavior across container restarts, requires DB read per fleet creation

### 2. Time-Window Based Circuit
**Decision:** Track interruptions in sliding 15/30 minute window
**Rationale:** Recent interruptions more relevant than historical count
**Impact:** Need to store first_interruption_at and prune old counts

### 3. Auto-Reset After Cooldown
**Decision:** Automatically close circuit after 15-30 minutes
**Rationale:** Spot capacity issues are usually transient
**Impact:** Background goroutine checks auto_reset_at every 60s

### 4. Instance-Type Granularity
**Decision:** Circuit breaker operates per instance type, not globally
**Rationale:** t4g.medium interruptions shouldn't block c7g.xlarge availability
**Impact:** Separate circuit state per instance type in DynamoDB

### 5. Cost Report Storage
**Decision:** Store reports in S3 and send via SNS, not just logs
**Rationale:** Historical cost tracking, email delivery, potential analytics
**Impact:** S3 bucket for reports, lifecycle policy for retention

### 6. Session Manager Preferred Over SSH
**Decision:** Document both SSH and Session Manager, recommend Session Manager
**Rationale:** No key management, automatic audit logging, no inbound ports
**Impact:** Simpler security model, CloudWatch Logs integration

## Integration Points

### Circuit Breaker State (DynamoDB)
- **Table:** `runs-fleet-circuit-state`
- **Schema:**
  ```
  PK: instance_type (string, e.g., "t4g.medium")
  Attributes:
    - state: "closed|open|half-open"
    - interruption_count: number
    - first_interruption_at: ISO 8601 timestamp
    - last_interruption_at: ISO 8601 timestamp
    - opened_at: ISO 8601 timestamp
    - auto_reset_at: ISO 8601 timestamp
  TTL: auto_reset_at + 3600 (1 hour after reset)
  ```

### CloudWatch Dashboard
- **Dashboard Name:** `RunsFleet-Production`
- **Widgets:** 12 widgets in 3x4 grid
- **Metrics:** All from namespace `RunsFleet`

### SNS Topic for Alerts
- **Topic:** `runs-fleet-alerts`
- **Subscriptions:** Email (from Terraform variable)
- **Publishers:** CloudWatch Alarms

### Cost Reports S3 Bucket
- **Bucket:** `runs-fleet-reports` (or existing bucket with prefix)
- **Path:** `cost/{YYYY}/{MM}/{DD}.md`
- **Lifecycle:** 90-day retention

### SSH Access
- **Security Group:** Add port 22 rule with source CIDR restriction
- **Key Pair:** EC2 KeyPair (existing or created via Terraform)
- **Alternative:** Systems Manager Session Manager (no SSH key needed)

## Important Patterns

### Circuit Breaker State Machine
```go
type CircuitState string

const (
    StateClosed   CircuitState = "closed"   // Normal operation
    StateOpen     CircuitState = "open"     // Spot blocked
    StateHalfOpen CircuitState = "half-open" // Testing recovery
)

type CircuitBreaker struct {
    tableName string
    client    *dynamodb.Client
    mu        sync.RWMutex
    cache     map[string]*CircuitInfo // In-memory cache (TTL 60s)
}

func (cb *CircuitBreaker) RecordInterruption(ctx context.Context, instanceType string) error {
    // 1. Increment interruption_count
    // 2. Set first_interruption_at if not exists
    // 3. Update last_interruption_at
    // 4. If count >= threshold: Open circuit, set auto_reset_at
    // 5. Publish metric: CircuitBreakerTriggered
}

func (cb *CircuitBreaker) CheckCircuit(ctx context.Context, instanceType string) (CircuitState, error) {
    // 1. Check in-memory cache (60s TTL)
    // 2. If miss: Query DynamoDB
    // 3. Check if auto_reset_at passed → close circuit
    // 4. Return current state
}
```

### Cost Calculation
```go
type CostBreakdown struct {
    EC2Spot        float64
    EC2OnDemand    float64
    S3Storage      float64
    S3Requests     float64
    Fargate        float64
    SQS            float64
    DynamoDB       float64
    CloudWatch     float64
    Total          float64
    SpotSavings    float64
    SpotSavingsPct float64
}

func CalculateDailyCosts(ctx context.Context, date time.Time) (*CostBreakdown, error) {
    // 1. Query CloudWatch metrics for 24h window
    // 2. Get EC2 instance-hours by type and pricing
    // 3. Calculate costs using pricing map or Pricing API
    // 4. Query S3 metrics for storage and requests
    // 5. Estimate supporting services (fixed + variable)
    // 6. Calculate spot savings vs on-demand
    // 7. Return breakdown
}
```

### Dashboard Widget Definition (Terraform)
```hcl
resource "aws_cloudwatch_dashboard" "runs_fleet" {
  dashboard_name = "RunsFleet-Production"
  dashboard_body = jsonencode({
    widgets = [
      {
        type = "metric"
        properties = {
          metrics = [
            ["RunsFleet", "QueueDepth", { stat = "Average" }]
          ]
          period = 300
          region = var.aws_region
          title  = "Main Queue Depth"
        }
      },
      # ... 11 more widgets
    ]
  })
}
```

## Data Flow

### Circuit Breaker Flow
```
1. Spot interruption occurs (EventBridge)
2. Events handler processes interruption
   ↓
3. Call circuit.RecordInterruption(instanceType)
4. DynamoDB: Increment count, check threshold
5. If threshold exceeded:
   a. Update state = "open"
   b. Set auto_reset_at = now() + 30 minutes
   c. Publish metric: CircuitBreakerTriggered
   ↓
6. Next job for same instance type:
   a. Fleet manager calls circuit.CheckCircuit(instanceType)
   b. Circuit returns "open"
   c. Fleet manager forces spot=false
   d. Create on-demand fleet instead
   ↓
7. After 30 minutes:
   a. Background goroutine checks auto_reset_at
   b. Update state = "closed"
   c. Publish metric: CircuitBreakerReset
   d. Next job can use spot again
```

### Cost Report Flow
```
1. EventBridge rule triggers at 9am daily
2. Message sent to housekeeping queue
   ↓
3. Housekeeping handler dispatches to CostReportTask
4. Task queries CloudWatch for previous 24h metrics
5. Calculate costs per service:
   a. EC2: instance-hours * pricing
   b. S3: storage + requests
   c. Supporting services (estimates)
   ↓
6. Generate markdown report
7. Store report in S3: cost/{YYYY}/{MM}/{DD}.md
8. Publish to SNS topic (email delivery)
9. Optional: POST to Slack webhook
```

## Next Steps

**To resume work on this feature:**

1. **Start with circuit breaker** (highest impact)
   - Create `pkg/circuit/breaker.go` with state machine
   - Add DynamoDB table in Terraform
   - Integrate with fleet manager and events handler

2. **Then forced retry** (complements circuit breaker)
   - Update JobMessage struct with RetryCount, ForceOnDemand
   - Update events handler re-queue logic
   - Test with simulated spot interruption

3. **Dashboard and alarms** (visibility)
   - Create Terraform definitions
   - Add missing metrics (cache hit rate, API errors)
   - Deploy and verify widgets appear

4. **Cost reporting** (last, requires other metrics)
   - Implement cost calculator
   - Add to housekeeping tasks
   - Test report generation and delivery

5. **SSH access** (quick win)
   - Update security group Terraform
   - Document SSH and Session Manager procedures

**Terraform Infrastructure Location:**
- Repository: `shavakan-terraform/terraform/runs-fleet/`
- New files: `circuit_breaker_table.tf`, `cloudwatch_dashboard.tf`, `cloudwatch_alarms.tf`, `security_groups.tf` (update)
