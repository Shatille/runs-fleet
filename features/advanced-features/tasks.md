# Advanced Features - Task Checklist

**Last Updated:** 2025-11-25

## Phase 1: Pool Scheduling

- [ ] Complete `pkg/pools/manager.go:59` reconcile() implementation
- [ ] Add schedule schema to DynamoDB pool config
- [ ] Query EC2 for pool instances (by tag)
- [ ] Compare actual vs desired state (running/stopped counts)
- [ ] Create/stop/terminate to match schedule
- [ ] Implement idle timeout tracking
- [ ] Test schedule transitions (business hours → night)

## Phase 2: GitOps Configuration

- [ ] Define YAML schema for `.github-private/.github/runs-fleet.yml`
- [ ] Create config watcher webhook endpoint
- [ ] Implement config validation (schema, runner specs)
- [ ] Apply config to DynamoDB (pools, runner specs)
- [ ] Add pre-commit hook for syntax checking
- [ ] Document configuration options

## Phase 3: Multi-Region Support

- [ ] Add `region=` label parsing
- [ ] Create regional queues or route via tags
- [ ] Configure DynamoDB Global Tables
- [ ] Enable S3 Cross-Region Replication
- [ ] Test failover between regions

## Phase 4: Windows Support

- [ ] Build Windows Server 2022 AMI (Packer)
- [ ] Compile Windows agent binary (GOOS=windows)
- [ ] Create PowerShell bootstrap script
- [ ] Add Windows instance types to fleet
- [ ] Test Windows workflow execution

## Phase 5: OpenTelemetry Integration

- [ ] Add OpenTelemetry Go SDK dependency
- [ ] Configure OTLP exporter (env var)
- [ ] Instrument HTTP handlers
- [ ] Instrument queue processing
- [ ] Propagate trace context in SQS messages

## Phase 6: Per-Stack Environments

- [ ] Add `env=` label parsing
- [ ] Route to environment-specific resources
- [ ] Tag all resources with Environment
- [ ] Configure budget alerts per environment
- [ ] Test isolation between dev/prod
