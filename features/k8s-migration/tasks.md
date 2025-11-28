# K8s Migration Tasks

**Updated**: 2025-11-29

## Phase 1: Design & Planning

- [x] Write initial design document (`docs/design-k8s-migration.md`)
- [x] Define component mapping (EC2 → K8s)
- [x] Draft Karpenter NodePool specifications
- [x] Design warm pool strategy (placeholder pods)
- [x] Sketch Job creator implementation
- [x] Review and finalize design decisions
- [x] Decide: Docker-in-Docker vs Sysbox vs host socket → **DinD**
- [x] Decide: Single cluster vs multi-region → **Single cluster**
- [x] Decide: Windows runner strategy → **No Windows support**

## Phase 2: Infrastructure Setup

- [ ] Create EKS cluster (or identify existing)
- [ ] Install Karpenter v1.0+
- [ ] Configure IRSA for runs-fleet service accounts
- [ ] Create `deploy/karpenter/nodepools.yaml`
- [ ] Create `deploy/karpenter/ec2nodeclasses.yaml`
- [ ] Create `runs-fleet` namespace with resource quotas
- [ ] Set up PriorityClasses (runner, placeholder)
- [ ] Configure NetworkPolicy for runner egress

## Phase 3: Core Implementation

- [ ] Create `pkg/k8s/client.go` - K8s client initialization
- [ ] Create `pkg/k8s/jobs/specs.go` - Runner class definitions
- [ ] Create `pkg/k8s/jobs/creator.go` - Job/Secret creation
- [ ] Create `pkg/k8s/jobs/watcher.go` - Job status monitoring
- [ ] Create `pkg/k8s/warmpool/controller.go` - Placeholder management
- [ ] Create `pkg/k8s/cleanup/cleanup.go` - Orphaned resource cleanup
- [ ] Update `pkg/config/` - Add K8s client config
- [ ] Update `cmd/server/main.go` - Wire K8s components

## Phase 4: Integration

- [ ] Add feature flag: `RUNS_FLEET_USE_K8S=true`
- [ ] Update queue processor to route to K8s job creator
- [ ] Adapt metrics for K8s (pod scheduling latency, warm hits)
- [ ] Update housekeeping for K8s resource cleanup
- [ ] Remove/disable EC2-specific code paths when K8s enabled

## Phase 5: Testing & Validation

- [ ] Unit tests for `pkg/k8s/jobs/`
- [ ] Unit tests for `pkg/k8s/warmpool/`
- [ ] Integration test: cold start path
- [ ] Integration test: warm path (placeholder preemption)
- [ ] Integration test: spot interruption recovery
- [ ] Load test: concurrent job creation
- [ ] Compare metrics: EC2 vs K8s (latency, success rate)

## Phase 6: Migration

- [ ] Deploy K8s orchestrator to staging
- [ ] Route test repositories to K8s path
- [ ] Monitor for 1 week, tune NodePool settings
- [ ] Gradual rollout: 10% → 50% → 100% traffic
- [ ] Decommission EC2 orchestrator
- [ ] Archive legacy packages (pkg/fleet, pkg/pools, pkg/termination, pkg/events)

## Packages to Create

| Package | Purpose |
|---------|---------|
| `pkg/k8s/client.go` | K8s client initialization, config |
| `pkg/k8s/jobs/specs.go` | Runner class → resource requirements |
| `pkg/k8s/jobs/creator.go` | Create K8s Jobs and JIT Secrets |
| `pkg/k8s/jobs/watcher.go` | Watch Job status, handle failures |
| `pkg/k8s/warmpool/controller.go` | Manage placeholder pod replicas |
| `pkg/k8s/cleanup/cleanup.go` | Delete orphaned Jobs/Secrets/Pods |

## Packages to Remove (after migration)

| Package | Replacement |
|---------|-------------|
| `pkg/fleet/` | `pkg/k8s/jobs/` |
| `pkg/pools/` | `pkg/k8s/warmpool/` + Karpenter |
| `pkg/termination/` | Karpenter node lifecycle |
| `pkg/events/` | Karpenter spot handling |
| `pkg/coordinator/` | K8s Lease API |

## Deploy Artifacts to Create

```
deploy/
└── karpenter/
    ├── nodepools.yaml           # NodePool per runner class
    ├── ec2nodeclasses.yaml      # EC2NodeClass for disk/AMI config
    ├── priorityclasses.yaml     # Runner and placeholder priorities
    └── namespace.yaml           # runs-fleet namespace + quotas
```
