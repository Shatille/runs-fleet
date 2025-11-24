# Implementation Order

**Recommended sequence for implementing phases 6-9 based on dependencies and impact.**

---

## Critical Path (Must Complete First)

### 1. Phase 6: Agent Implementation
**Directory:** `features/agent-implementation/`
**Duration:** 14 days (2-3 weeks)
**Priority:** P0 - BLOCKING

**Why First:**
- System cannot execute real GitHub Actions workflows without this
- Blocks all end-to-end testing and validation
- Required before termination queue can be tested
- 60% → 75% functionality gap

**Implementation Order Within Phase:**
1. Phase 1: Runner download & verification (3 days)
2. Phase 2: Runner registration (2 days)
3. Phase 3: Job execution & monitoring (4 days)
4. Phase 4: Telemetry & cleanup (2 days)
5. Testing: Integration tests with real workflows (3 days)

**Blockers:** None
**Blocks:** Everything else

---

### 2. Phase 7.1: Termination Queue Processor
**Directory:** `features/queue-processors/` (Phase 1)
**Duration:** 3 days
**Priority:** P0 - CRITICAL

**Why Second:**
- Agent sends termination messages (Phase 6) but nothing consumes them
- DynamoDB job records never get marked complete without this
- Required for accurate job lifecycle tracking
- Unblocks observability metrics (job duration, success rate)

**Dependency:** Phase 6 must be complete (agent sends termination messages)
**Blocks:** Cost reporting (needs job completion data)

---

### 3. Phase 8.1: Circuit Breaker
**Directory:** `features/operational-excellence/` (Phase 1)
**Duration:** 3 days
**Priority:** P1 - HIGH

**Why Third:**
- Prevents runaway costs from spot interruption loops
- Should be deployed BEFORE heavy production usage
- Can save 50-70% vs uncontrolled spot churn
- No dependencies on other features

**Dependency:** Events queue processor (already implemented)
**Blocks:** None, but enables safe production use

---

### 4. Phase 8.2: Forced On-Demand Retry
**Directory:** `features/operational-excellence/` (Phase 2)
**Duration:** 2 days
**Priority:** P1 - HIGH

**Why Fourth:**
- Complements circuit breaker (ensures job completion)
- Simple change to existing re-queue logic
- High reliability improvement for low effort

**Dependency:** Events queue processor (already implemented)
**Blocks:** None

---

## Observability & Operations (Before Production)

### 5. Phase 8.3: CloudWatch Dashboard
**Directory:** `features/operational-excellence/` (Phase 3)
**Duration:** 3 days
**Priority:** P1 - HIGH

**Why Fifth:**
- Visibility into system health and performance
- Required for detecting incidents quickly
- Informs optimization decisions
- Can develop concurrently with alarms

**Dependency:** Termination queue (for job completion metrics)
**Blocks:** None, enables informed operations

---

### 6. Phase 8.4: CloudWatch Alarms
**Directory:** `features/operational-excellence/` (Phase 4)
**Duration:** 2 days
**Priority:** P1 - HIGH

**Why Sixth:**
- Proactive incident detection (<5 min MTTD)
- Should be deployed before production usage
- Depends on dashboard metrics being defined

**Dependency:** Dashboard metrics defined
**Blocks:** None, enables proactive operations

---

### 7. Phase 7.2: Housekeeping Queue Processor
**Directory:** `features/queue-processors/` (Phase 2)
**Duration:** 5 days
**Priority:** P1 - MEDIUM

**Why Seventh:**
- Background cleanup prevents resource accumulation
- Not blocking for initial production use (manual cleanup possible)
- Can be deployed after initial production validation
- Includes pool audit task (useful for cost reporting)

**Dependency:** None (independent cleanup tasks)
**Blocks:** Cost reporting pool audit data

---

## Cost Control & Debugging (Production Hardening)

### 8. Phase 8.5: Daily Cost Reporting
**Directory:** `features/operational-excellence/` (Phase 5)
**Duration:** 4 days
**Priority:** P2 - MEDIUM

**Why Eighth:**
- Cost visibility after production usage begins
- Requires job completion data (termination queue)
- Requires pool audit data (housekeeping queue)
- Can validate cost model estimates

**Dependency:** Termination queue, housekeeping queue (pool audit)
**Blocks:** None

---

### 9. Phase 8.6: SSH Debugging Access
**Directory:** `features/operational-excellence/` (Phase 6)
**Duration:** 1 day
**Priority:** P2 - LOW

**Why Ninth:**
- Debugging tool, not required for operation
- Quick win, can be added anytime
- Useful once production issues arise

**Dependency:** None
**Blocks:** None

---

## Advanced Features (Competitive Parity)

### 10. Phase 9.1: Pool Scheduling
**Directory:** `features/advanced-features/` (Phase 1)
**Duration:** 5 days
**Priority:** P2 - MEDIUM

**Why Tenth:**
- Completes Phase 2 warm pools (reconciliation was stubbed)
- Reduces idle costs (stopped pools during off-hours)
- Highest impact among advanced features
- Should be implemented before other advanced features

**Dependency:** Pool reconciliation infrastructure (exists but stubbed)
**Blocks:** None

---

### 11. Phase 9.4: Windows Support
**Directory:** `features/advanced-features/` (Phase 4)
**Duration:** 7 days
**Priority:** P2 - MEDIUM

**Why Eleventh:**
- Expands platform capabilities (Linux-only is limiting)
- Requires separate agent binary and AMI
- Moderate complexity, medium demand

**Dependency:** Agent implementation (Phase 6)
**Blocks:** None

---

### 12. Phase 9.2: GitOps Configuration
**Directory:** `features/advanced-features/` (Phase 2)
**Duration:** 4 days
**Priority:** P3 - LOW

**Why Twelfth:**
- Developer experience improvement
- Current env-var config works, this is convenience
- Requires additional webhook endpoint and validation

**Dependency:** Pool scheduling (to make config valuable)
**Blocks:** None

---

### 13. Phase 9.3: Multi-Region Support
**Directory:** `features/advanced-features/` (Phase 3)
**Duration:** 6 days
**Priority:** P3 - LOW

**Why Thirteenth:**
- Availability improvement but single-region sufficient initially
- Complex infrastructure changes (Global Tables, CRR)
- Low demand until scale increases

**Dependency:** All core features complete
**Blocks:** None

---

### 14. Phase 9.5: OpenTelemetry Integration
**Directory:** `features/advanced-features/` (Phase 5)
**Duration:** 3 days
**Priority:** P3 - LOW

**Why Fourteenth:**
- Debugging enhancement (CloudWatch Logs sufficient initially)
- Optional feature, not all deployments need it
- Low complexity, can add anytime

**Dependency:** None
**Blocks:** None

---

### 15. Phase 9.6: Per-Stack Environments
**Directory:** `features/advanced-features/` (Phase 6)
**Duration:** 3 days
**Priority:** P3 - LOW

**Why Last:**
- Infrastructure isolation for dev/staging/prod
- Can be handled with separate AWS accounts initially
- Lowest impact, most complex operationally

**Dependency:** None
**Blocks:** None

---

## Sprint Breakdown

### Sprint 1: Critical Path (18 days)
1. Agent Implementation (14 days)
2. Termination Queue Processor (3 days)
3. Circuit Breaker (3 days) - overlap last 2 days

**Outcome:** System can execute real workflows safely

---

### Sprint 2: Observability (10 days)
4. Forced On-Demand Retry (2 days)
5. CloudWatch Dashboard (3 days)
6. CloudWatch Alarms (2 days)
7. Housekeeping Queue Processor (5 days) - overlap

**Outcome:** Production-ready with full observability

---

### Sprint 3: Polish (5 days)
8. Daily Cost Reporting (4 days)
9. SSH Debugging Access (1 day)

**Outcome:** Complete operational tooling

---

### Sprint 4: Advanced Features (25 days)
10. Pool Scheduling (5 days)
11. Windows Support (7 days)
12. GitOps Configuration (4 days)
13. Multi-Region Support (6 days)
14. OpenTelemetry Integration (3 days)
15. Per-Stack Environments (3 days)

**Outcome:** Competitive parity with runs-on

---

## Parallel Work Opportunities

**Can be developed concurrently:**
- Circuit Breaker + Dashboard (independent)
- Alarms + Housekeeping Queue (independent)
- Cost Reporting + SSH Access (independent)
- Pool Scheduling + Windows Support (independent)
- GitOps + Multi-Region (independent)

**Must be sequential:**
- Agent → Termination Queue (message dependency)
- Termination Queue → Cost Reporting (data dependency)
- Dashboard → Alarms (metric definitions)
- Pool Scheduling → GitOps (configuration schema)

---

## Deployment Gates

**Gate 1: End-to-End Validation**
- Complete: Agent Implementation, Termination Queue
- Validate: Run real GitHub workflow successfully
- Validate: Job lifecycle tracked in DynamoDB
- Validate: Instance self-terminates correctly

**Gate 2: Production Readiness**
- Complete: Circuit Breaker, Dashboard, Alarms
- Validate: Spot interruption handling works
- Validate: Incidents detected in <5 minutes
- Validate: No zombie instances after 24h

**Gate 3: Cost Optimization**
- Complete: Housekeeping Queue, Cost Reporting
- Validate: Daily costs match estimates (±20%)
- Validate: Resource cleanup working
- Validate: Pool utilization visible

**Gate 4: Feature Parity**
- Complete: Pool Scheduling, Windows Support
- Validate: Idle costs reduced by >50%
- Validate: Windows workflows execute
- Validate: GitOps config working (if implemented)

---

## Timeline Summary

**Critical Path (Sprint 1):** 18 days → System functional
**Observability (Sprint 2):** 10 days → Production-ready
**Polish (Sprint 3):** 5 days → Fully operational
**Advanced (Sprint 4):** 25 days → Competitive parity

**Total Sequential:** 58 days (~12 weeks)
**Total with Parallelization:** ~40 days (~8 weeks)

---

## Decision Points

**After Sprint 1:** Deploy to production? (requires Sprint 2 for safety)
**After Sprint 2:** Sufficient for production use
**After Sprint 3:** Full operational maturity
**Sprint 4:** Optional based on business requirements
