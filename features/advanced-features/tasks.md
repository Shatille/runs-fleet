# Advanced Features - Task Checklist

**Last Updated:** 2025-11-25

## Phase 1: Pool Scheduling

- [x] Complete `pkg/pools/manager.go:59` reconcile() implementation
- [x] Add schedule schema to DynamoDB pool config
- [x] Query EC2 for pool instances (by tag)
- [x] Compare actual vs desired state (running/stopped counts)
- [x] Create/stop/terminate to match schedule
- [x] Implement idle timeout tracking
- [x] Test schedule transitions (business hours → night)

## Phase 2: GitOps Configuration

- [x] Define YAML schema for `.github-private/.github/runs-fleet.yml`
- [x] Create config watcher webhook endpoint
- [x] Implement config validation (schema, runner specs)
- [x] Apply config to DynamoDB (pools, runner specs)
- [x] Add GitHub API client for fetching config
- [x] Document configuration options

## Phase 3: Multi-Region Support

- [x] Add `region=` label parsing
- [x] Update JobConfig and JobMessage with region field
- [x] Add region tag to fleet instances
- [ ] Create regional queues or route via tags (infrastructure)
- [ ] Configure DynamoDB Global Tables (infrastructure)
- [ ] Enable S3 Cross-Region Replication (infrastructure)
- [ ] Test failover between regions

## Phase 4: Windows Support

- [x] Add Windows runner spec mappings (instance types)
- [x] Compile Windows agent binary (GOOS=windows) in Makefile
- [x] Create PowerShell bootstrap script
- [x] Add Windows instance types to fleet manager
- [x] Select Windows launch template based on OS
- [ ] Build Windows Server 2022 AMI (Packer) - separate task
- [ ] Test Windows workflow execution

## Phase 5: OpenTelemetry Integration

- [x] Add OpenTelemetry Go SDK dependency
- [x] Configure OTLP exporter (env var)
- [x] Create tracing package with instrumentation helpers
- [x] Add trace context to SQS messages
- [x] Propagate trace context in SQS messages
- [ ] Instrument HTTP handlers (optional, for full tracing)
- [ ] Instrument queue processing (optional, for full tracing)

## Phase 6: Per-Stack Environments

- [x] Add `env=` label parsing
- [x] Update JobConfig and JobMessage with environment field
- [x] Add environment tag to fleet instances
- [x] Route to environment-specific resources
- [x] Tag all resources with Environment for cost tracking
- [ ] Configure budget alerts per environment (infrastructure)
- [ ] Test isolation between dev/prod

## Summary

All code changes for Sprint 4 have been completed:
- Pool scheduling with time-based sizing ✅
- GitOps configuration with YAML schema ✅
- Multi-region label support ✅
- Windows support (code and bootstrap script) ✅
- OpenTelemetry integration ✅
- Per-stack environment isolation ✅

Remaining items are infrastructure/deployment tasks (AMI builds, DynamoDB Global Tables, etc.)
