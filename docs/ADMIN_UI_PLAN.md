# Admin UI Enhancement Plan

This document outlines the plan to enhance the runs-fleet admin UI from basic pool CRUD to a full operational dashboard.

## Context

- **Current state**: Pool configuration management only (list, create, edit, delete)
- **Target state**: Full operational visibility and control
- **Auth**: Keycloak gatekeeper proxy handles authentication; admin UI trusts authenticated requests

## Architecture Changes

### Authentication Simplification

Remove Bearer token authentication from admin handlers. Keycloak gatekeeper proxy will:
- Authenticate users via OIDC
- Forward authenticated requests with user identity headers
- Admin UI backend trusts `X-Auth-Request-User` and `X-Auth-Request-Groups` headers

**Files to modify:**
- `pkg/admin/auth.go` - Replace token validation with header extraction
- `pkg/admin/ui/lib/api.ts` - Remove token management
- `pkg/admin/ui/components/auth-wrapper.tsx` - Remove login form

### Audit Logging

Audit logging is mandatory and non-configurable. All admin actions are logged with:
- User identity (from Keycloak headers)
- Action performed
- Target resource
- Timestamp
- Result (success/failure)
- Client IP

**Current state**: Already implemented in `handler.go:auditLog()`, needs user identity from headers.

---

## Feature Additions

### Phase 1: Visibility (Read-only dashboards)

#### 1.1 Jobs Dashboard

**Purpose**: View running and historical jobs.

**API endpoints:**
```
GET /api/jobs                    - List jobs with filters
GET /api/jobs/{job_id}           - Job details
GET /api/jobs/stats              - Aggregate statistics
```

**Query parameters:**
- `status` - running, completed, failed, terminating, requeued
- `pool` - Filter by pool name
- `since` - ISO8601 timestamp
- `limit`, `offset` - Pagination

**Data source**: `runs-fleet-jobs` DynamoDB table

**UI components:**
- Jobs table with sortable columns
- Status filters (tabs or dropdown)
- Job detail modal/page
- Real-time updates via polling (30s interval)

**Backend changes:**
- New `ListJobs` handler in `pkg/admin/handler.go`
- Add GSI on `(status, created_at)` for efficient queries (optional, scan acceptable at current scale)

---

#### 1.2 Instances Dashboard

**Purpose**: View EC2 instances managed by runs-fleet.

**API endpoints:**
```
GET /api/instances               - List instances with filters
GET /api/instances/{instance_id} - Instance details
```

**Query parameters:**
- `pool` - Filter by pool name
- `state` - running, stopped, pending, terminating
- `busy` - true/false (has active job)

**Data source**: EC2 DescribeInstances API with `runs-fleet:managed=true` tag

**UI components:**
- Instances table grouped by pool
- State badges (running/stopped/busy)
- Instance type, launch time, uptime
- Link to AWS console

**Backend changes:**
- New `ListInstances` handler using EC2 API
- Cross-reference with jobs table for busy status

---

#### 1.3 Pool Status Enhancement

**Purpose**: Show actual vs desired state for each pool.

**API changes:**
- Extend `GET /api/pools` response to include:
  ```json
  {
    "current_running": 3,
    "current_stopped": 5,
    "busy_instances": 2,
    "last_reconcile": "2024-01-15T10:30:00Z"
  }
  ```

**Data source**:
- `current_running`/`current_stopped` from pools DynamoDB table
- `busy_instances` from jobs table scan
- `last_reconcile` - add to pool reconciler

**UI changes:**
- Add columns for current counts
- Visual diff indicator when actual != desired
- Last reconcile timestamp

---

#### 1.4 Queue Dashboard

**Purpose**: Monitor SQS queue health.

**API endpoints:**
```
GET /api/queues                  - All queue metrics
GET /api/queues/{queue_name}     - Single queue details
```

**Response:**
```json
{
  "queues": [
    {
      "name": "main",
      "url": "https://sqs...",
      "messages_visible": 5,
      "messages_in_flight": 2,
      "messages_delayed": 0,
      "dlq_messages": 0
    }
  ]
}
```

**Data source**: SQS GetQueueAttributes API

**UI components:**
- Queue cards with message counts
- DLQ warning indicator
- Link to AWS console

---

#### 1.5 Circuit Breaker Status

**Purpose**: View instance type availability.

**API endpoints:**
```
GET /api/circuit-breaker         - All circuit states
```

**Response:**
```json
{
  "circuits": [
    {
      "instance_type": "c7g.xlarge",
      "state": "open",
      "failure_count": 5,
      "last_failure": "2024-01-15T10:00:00Z",
      "reset_at": "2024-01-15T10:30:00Z"
    }
  ]
}
```

**Data source**: `runs-fleet-circuit-state` DynamoDB table

**UI components:**
- Circuit breaker table
- State badges (closed/open/half-open)
- Manual reset button (Phase 2)

---

#### 1.6 Metrics Dashboard

**Purpose**: Key operational metrics at a glance.

**API endpoints:**
```
GET /api/metrics/summary         - Aggregated metrics
```

**Response:**
```json
{
  "jobs_24h": {
    "total": 150,
    "completed": 140,
    "failed": 5,
    "in_progress": 5
  },
  "warm_pool_hit_rate": 0.85,
  "avg_startup_time_seconds": 45,
  "spot_interruption_rate": 0.02,
  "cost_mtd_usd": 52.30
}
```

**Data source**:
- Jobs table aggregation
- CloudWatch metrics (if enabled)
- Cost reporter

**UI components:**
- Metric cards with sparklines
- Trend indicators (up/down arrows)

---

#### 1.7 Cost Dashboard

**Purpose**: View cost data and trends.

**API endpoints:**
```
GET /api/cost/summary            - Current period summary
GET /api/cost/daily              - Daily breakdown
GET /api/cost/by-pool            - Per-pool breakdown
```

**Data source**: `pkg/cost/` reporter output

**UI components:**
- Cost summary cards
- Daily cost chart
- Per-pool breakdown table

---

#### 1.8 Audit Log Viewer

**Purpose**: View admin action history.

**API endpoints:**
```
GET /api/audit-logs              - List audit entries
```

**Query parameters:**
- `user` - Filter by user
- `action` - Filter by action type
- `since`, `until` - Time range
- `limit`, `offset` - Pagination

**Implementation options:**
1. CloudWatch Logs Insights query (if logs go to CloudWatch)
2. Dedicated DynamoDB table for audit entries
3. S3 + Athena for long-term storage

**Recommendation**: Option 2 (DynamoDB) for simplicity, with TTL for 90-day retention.

**UI components:**
- Audit log table with filters
- User, action, target, result, timestamp columns
- Expandable rows for details

---

### Phase 2: Actions (Write operations)

#### 2.1 Manual Instance Termination

**Purpose**: Terminate stuck or orphaned instances.

**API endpoints:**
```
DELETE /api/instances/{instance_id}
```

**Safety checks:**
- Confirm instance is runs-fleet managed
- Warn if instance has active job
- Require confirmation in UI

---

#### 2.2 Circuit Breaker Reset

**Purpose**: Manually reset tripped circuit breakers.

**API endpoints:**
```
POST /api/circuit-breaker/{instance_type}/reset
```

---

#### 2.3 Force Pool Reconciliation

**Purpose**: Trigger immediate pool reconciliation.

**API endpoints:**
```
POST /api/pools/{name}/reconcile
```

**Implementation**: Send message to pool queue or invoke reconciler directly.

---

#### 2.4 DLQ Redrive

**Purpose**: Move messages from DLQ back to main queue.

**API endpoints:**
```
POST /api/queues/{queue_name}/redrive
```

**Implementation**: SQS StartMessageMoveTask API

---

#### 2.5 Housekeeping Trigger

**Purpose**: Manually trigger housekeeping tasks.

**API endpoints:**
```
POST /api/housekeeping/run
```

**Request body:**
```json
{
  "tasks": ["orphaned_instances", "stale_ssm", "old_jobs"]
}
```

---

### Phase 3: Advanced Features

#### 3.1 Real-time Updates

Replace polling with Server-Sent Events (SSE) for:
- Job status changes
- Instance state changes
- Queue depth updates

**API endpoints:**
```
GET /api/events                  - SSE stream
```

---

#### 3.2 Spot Interruption History

**Purpose**: Track spot interruption patterns for capacity planning.

**API endpoints:**
```
GET /api/spot-interruptions      - Interruption history
```

**Data source**: EventBridge events stored in DynamoDB

---

#### 3.3 Cache Metrics

**Purpose**: S3 cache hit/miss rates.

**API endpoints:**
```
GET /api/cache/stats             - Cache statistics
```

---

## Implementation Order

| Priority | Feature | Effort | Value |
|----------|---------|--------|-------|
| 1 | Auth simplification (Keycloak) | S | High |
| 2 | Jobs dashboard | M | High |
| 3 | Pool status enhancement | S | High |
| 4 | Instances dashboard | M | High |
| 5 | Queue dashboard | S | Medium |
| 6 | Circuit breaker status | S | Medium |
| 7 | Audit log viewer + DynamoDB storage | M | Medium |
| 8 | Metrics summary | M | Medium |
| 9 | Cost dashboard | M | Medium |
| 10 | Manual instance termination | S | Medium |
| 11 | Force reconciliation | S | Low |
| 12 | Circuit breaker reset | S | Low |
| 13 | DLQ redrive | S | Low |
| 14 | Housekeeping trigger | S | Low |
| 15 | SSE real-time updates | L | Low |

**Effort**: S = Small (1-2 days), M = Medium (3-5 days), L = Large (1+ week)

---

## UI Layout

```
┌─────────────────────────────────────────────────────────────┐
│  runs-fleet Admin                              [User] [Logout] │
├─────────┬───────────────────────────────────────────────────┤
│         │                                                     │
│ Dashboard│  ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────┐  │
│ Jobs     │  │ Jobs 24h│ │ Warm Hit│ │ Cost MTD│ │ Queued  │  │
│ Instances│  │   150   │ │   85%   │ │  $52.30 │ │    3    │  │
│ Pools    │  └─────────┘ └─────────┘ └─────────┘ └─────────┘  │
│ Queues   │                                                    │
│ Circuit  │  Recent Jobs                                       │
│ Cost     │  ┌─────────────────────────────────────────────┐  │
│ Audit    │  │ ID    │ Repo      │ Pool   │ Status │ Time  │  │
│          │  │ 12345 │ org/repo  │ default│ ✓ done │ 5m ago│  │
│          │  │ 12344 │ org/repo2 │ arm64  │ ⟳ run  │ 2m ago│  │
│          │  └─────────────────────────────────────────────┘  │
│          │                                                    │
└─────────┴───────────────────────────────────────────────────┘
```

---

## File Structure

```
pkg/admin/
├── auth.go              # Keycloak header extraction
├── handler.go           # Pool CRUD (existing)
├── handler_jobs.go      # Jobs endpoints
├── handler_instances.go # Instances endpoints
├── handler_queues.go    # Queue endpoints
├── handler_circuit.go   # Circuit breaker endpoints
├── handler_metrics.go   # Metrics endpoints
├── handler_cost.go      # Cost endpoints
├── handler_audit.go     # Audit log endpoints
├── handler_actions.go   # Write operations (Phase 2)
└── ui/
    ├── app/
    │   ├── page.tsx           # Dashboard (new home)
    │   ├── jobs/
    │   │   └── page.tsx       # Jobs list
    │   ├── instances/
    │   │   └── page.tsx       # Instances list
    │   ├── pools/
    │   │   ├── page.tsx       # Pools list (move from root)
    │   │   ├── new/
    │   │   └── edit/
    │   ├── queues/
    │   │   └── page.tsx       # Queue status
    │   ├── circuit/
    │   │   └── page.tsx       # Circuit breaker status
    │   ├── cost/
    │   │   └── page.tsx       # Cost dashboard
    │   └── audit/
    │       └── page.tsx       # Audit logs
    ├── components/
    │   ├── sidebar.tsx        # Navigation sidebar
    │   ├── metric-card.tsx    # Dashboard metric cards
    │   ├── jobs-table.tsx
    │   ├── instances-table.tsx
    │   ├── queue-card.tsx
    │   └── ...
    └── lib/
        ├── api.ts             # API client (simplified)
        └── types.ts           # TypeScript types
```

---

## Database Changes

### New: Audit Log Table

```
Table: runs-fleet-audit
Primary Key: timestamp (S) + id (S) (composite)
GSI: user-index (user, timestamp)

Attributes:
- id: ULID
- timestamp: ISO8601
- user: string (from Keycloak)
- action: string (pool.create, pool.update, instance.terminate, etc.)
- target: string (pool name, instance ID, etc.)
- result: string (success, denied, error)
- details: map (request body, error message, etc.)
- client_ip: string
- ttl: number (90-day expiry)
```

### Pools Table: Add Fields

```
- last_reconcile_at: ISO8601 timestamp
- last_reconcile_result: string (success, error)
```

---

## Security Considerations

1. **Trust boundary**: Keycloak gatekeeper is the trust boundary. Backend trusts forwarded headers.
2. **Header validation**: Verify required headers present; reject requests without identity.
3. **Audit logging**: All write operations logged with user identity.
4. **Rate limiting**: Consider adding rate limits for expensive operations (instance list, job queries).
5. **RBAC future**: Headers can include groups for future role-based access.

---

## Testing Strategy

1. **Unit tests**: Handler logic with mocked AWS clients
2. **Integration tests**: API endpoints with test DynamoDB/SQS
3. **E2E tests**: Playwright tests for critical UI flows
4. **Load tests**: Ensure dashboard queries don't impact production

---

## Rollout Plan

1. Deploy auth changes with feature flag (accept both token and Keycloak headers)
2. Enable Keycloak gatekeeper in staging
3. Verify auth flow works
4. Deploy Phase 1 features incrementally
5. Remove legacy token auth
6. Deploy Phase 2 features
