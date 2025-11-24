# Advanced Features Implementation Plan

## Executive Summary
Implement advanced features for competitive parity with runs-on: pool scheduling, GitOps configuration, multi-region support, Windows runners, OpenTelemetry tracing, and per-stack environments. These features are nice-to-have improvements that expand platform capabilities.

## Implementation Phases

### Phase 1: Pool Scheduling (Priority: High)
**Goal:** Time-based pool sizing to reduce idle costs

**Tasks:**
- Complete stubbed `reconcile()` in `pkg/pools/manager.go:59`
- Add schedule definitions to DynamoDB pool config
- Evaluate current schedule on each reconciliation (60s)
- Implement smooth transitions between schedules
- Track idle timeout and terminate/stop instances

**Timeline:** 5 days

### Phase 2: GitOps Configuration (Priority: Medium)
**Goal:** Config-as-code for runner specs and pool settings

**Tasks:**
- Define YAML schema for `.github-private/.github/runs-fleet.yml`
- Create config watcher for GitHub webhook (push events)
- Validate config schema and apply to DynamoDB
- Add pre-commit hook for syntax checking
- Document configuration options

**Timeline:** 4 days

### Phase 3: Multi-Region Support (Priority: Low)
**Goal:** Deploy runners across multiple AWS regions

**Tasks:**
- Add `region=` label support for routing
- Deploy orchestrator per region or single multi-region
- Configure DynamoDB Global Tables for cross-region state
- Enable S3 Cross-Region Replication for cache
- Implement region failover logic

**Timeline:** 6 days

### Phase 4: Windows Support (Priority: Medium)
**Goal:** Support Windows Server runners

**Tasks:**
- Build custom Windows Server 2022 AMI
- Add Windows instance types (m6i, c6i)
- Compile Windows agent binary
- Create PowerShell bootstrap script
- Add `runner=*-windows-x64` label support

**Timeline:** 7 days

### Phase 5: OpenTelemetry Integration (Priority: Low)
**Goal:** Distributed tracing for debugging

**Tasks:**
- Add OpenTelemetry Go SDK
- Configure OTLP exporter (optional env var)
- Instrument HTTP handlers, queue processing, EC2 calls
- Propagate trace context through SQS message attributes

**Timeline:** 3 days

### Phase 6: Per-Stack Environments (Priority: Low)
**Goal:** Isolate dev/staging/prod infrastructure

**Tasks:**
- Add `env=dev/staging/prod` label support
- Route to environment-specific resources (VPCs, tables, buckets)
- Tag resources with Environment for cost tracking
- Configure budget alerts per environment

**Timeline:** 3 days

## Total Timeline: 28 days (4 weeks)
## Priority Order: 1 → 4 → 2 → 3 → 5 → 6
