# Advanced Features - Context & Key Decisions

**Last Updated:** 2025-11-25

## Key Files

### Pool Reconciliation (Incomplete)
- `pkg/pools/manager.go:59` - Stubbed reconcile() function needs implementation
- `pkg/db/dynamo.go` - Pool config storage

### To Be Created
- Pool scheduling logic in pools package
- Config watcher for GitOps
- Multi-region routing
- Windows agent binary
- OpenTelemetry instrumentation
- Environment isolation

## Architectural Decisions

### 1. Pool Scheduling in DynamoDB
**Decision:** Store schedule as JSON array in pool config
**Rationale:** Flexible schedule definition, no schema changes
**Impact:** Parse and evaluate schedule on each reconciliation

### 2. GitOps via GitHub Webhooks
**Decision:** Watch config repo for push events, apply on change
**Rationale:** Familiar workflow for developers, versioned config
**Impact:** Requires webhook endpoint and config validation

### 3. Multi-Region Active-Active
**Decision:** Deploy orchestrator per region with shared state
**Rationale:** Low latency, regional failover
**Impact:** DynamoDB Global Tables, S3 CRR

### 4. Windows Agent Separate Binary
**Decision:** Compile separate agent for Windows (not cross-platform)
**Rationale:** Different system calls, file paths, termination mechanisms
**Impact:** Maintain two agent codebases

### 5. OpenTelemetry Optional
**Decision:** OTLP exporter configurable via env var
**Rationale:** Not all deployments need tracing (cost/complexity)
**Impact:** Graceful degradation when tracing disabled

### 6. Environment as Label
**Decision:** Use `env=` label for environment routing, not separate deployments
**Rationale:** Single codebase, simpler operations
**Impact:** Need environment-aware resource routing

## Next Steps

**Pool Scheduling:** Implement reconcile() logic first (highest impact)
**GitOps:** Define YAML schema and validation
**Multi-Region:** Test with secondary region
**Windows:** Build AMI and test agent
**OpenTelemetry:** Add SDK and instrument key paths
**Environments:** Add label parsing and routing
