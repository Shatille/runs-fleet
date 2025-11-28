# K8s Migration Feature Context

**Created**: 2025-11-29
**Updated**: 2025-11-29
**Status**: Design Phase

## Overview

Migration of runs-fleet orchestration layer from direct EC2 Fleet API to Kubernetes with Karpenter for node provisioning. Replaces custom EC2 management with K8s-native job scheduling while maintaining the fast webhook-driven architecture.

## Motivation

- **Warm path latency**: Current stopped EC2 instances take ~30s to start; K8s pod scheduling achieves ~3-5s
- **Spot handling**: Karpenter provides native spot interruption handling, eliminating custom EventBridge integration
- **Operational simplicity**: K8s-native tooling for observability, debugging, and scaling

## Architecture Summary

```
GitHub Webhook → SQS FIFO → Orchestrator Pod → K8s Job/Pod
                                                    ↓
                                              Karpenter
                                                    ↓
                                              EC2 Spot Nodes
```

### Key Design Decisions

| Decision | Rationale |
|----------|-----------|
| Keep SQS queue | Proven reliability, sub-second job pickup, existing webhook integration |
| Karpenter over Cluster Autoscaler | Faster provisioning (~60s vs ~3min), better spot support |
| Placeholder pods for warm pool | Lower idle cost than consolidation delay, ~5s scheduling |
| One NodePool per runner class | Clean separation, independent scaling limits |
| Docker-in-Docker (DinD) | Avoids host socket security concerns, portable across nodes |
| Single EKS cluster | Simpler operations, sufficient for current scale |
| No Windows support | Focus on Linux workloads, Karpenter Windows support immature |

## Component Mapping

| Current (EC2) | Kubernetes |
|---------------|------------|
| `pkg/fleet/` | `pkg/k8s/jobs/` |
| `pkg/pools/` | `pkg/k8s/warmpool/` + Karpenter NodePools |
| `pkg/runner/` (SSM) | K8s Secrets |
| `pkg/termination/` | Removed (Karpenter handles) |
| `pkg/events/` | Removed (Karpenter handles) |
| `pkg/coordinator/` | K8s Lease API |
| EC2 tags | K8s labels |
| DynamoDB jobs table | K8s Job status |

## Key Files

### Design Documentation
- `docs/design-k8s-migration.md` - Full design document with implementation details

### To Be Created
- `pkg/k8s/jobs/creator.go` - K8s Job creation (replaces pkg/fleet)
- `pkg/k8s/jobs/specs.go` - Runner class specifications
- `pkg/k8s/warmpool/controller.go` - Placeholder pod management
- `deploy/karpenter/` - NodePool and EC2NodeClass manifests

## Dependencies

### External
- EKS cluster (control plane: ~$73/month)
- Karpenter v1.0+
- IAM roles for service accounts (IRSA)

### Internal (Unchanged)
- `pkg/github/` - Webhook parsing, JIT token generation
- `pkg/queue/` - SQS message processing
- `pkg/cache/` - S3 cache protocol

## Performance Targets

| Metric | Current | Target |
|--------|---------|--------|
| Cold start | ~2 min | ~2 min (same) |
| Warm start | ~30s | ~3-5s |
| Spot recovery | ~3 min | ~90s |

## Cost Analysis

K8s architecture adds ~$40-60/month overhead (EKS control plane + warm node idle time) compared to current EC2 architecture. Break-even when:
- EKS already exists for other workloads
- Job volume >500/day
- Team velocity gains from K8s tooling

## Resolved Questions

1. ~~Docker-in-Docker vs Sysbox vs host Docker socket?~~ → **DinD** (security, portability)
2. ~~Single EKS cluster or regional clusters?~~ → **Single cluster** (simplicity)
3. ~~Keep EC2 path for Windows runners?~~ → **No Windows support** (Linux focus)

## Open Questions

1. S3 cache locality - how to ensure low-latency cache access from runner pods?

## Recent Progress

### Completed
- Initial design document created (`docs/design-k8s-migration.md`)
- Architecture comparison (EC2 vs K8s)
- Karpenter NodePool specifications drafted
- Job creator implementation sketched
- Warm pool strategy defined (placeholder pods)

### In Progress
- Design review and refinement

### Next Steps
1. Review design document for completeness
2. Decide on open questions (DinD, multi-cluster, Windows)
3. Set up EKS cluster with Karpenter for proof-of-concept
4. Implement `pkg/k8s/jobs/creator.go`
5. Create Karpenter manifests in `deploy/karpenter/`

### Issues/Blockers
- None currently
