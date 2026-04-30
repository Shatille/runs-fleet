---
topic: Compute Providers (Backend Abstraction)
last_compiled: 2026-04-30
sources_count: 3
---

# Compute Providers (Backend Abstraction)

## Purpose [coverage: medium -- 3 sources]

`pkg/provider` defines a backend-agnostic interface for provisioning runner
compute. Orchestrator code calls `Provider.CreateRunner` / `TerminateRunner` /
`DescribeRunner` and receives back opaque `RunnerID`s without knowing whether
the underlying resource is an EC2 instance or a Kubernetes Pod. Backend
selection happens at startup via `RUNS_FLEET_MODE=ec2|k8s` and can be
overridden per-job with the `backend=ec2|k8s` label.

The two backends differ substantially in lifecycle (EC2 has a stoppable state,
K8s does not), in queueing (SQS FIFO vs Valkey), and in resource granularity
(VM with EBS vs Pod with PVC). The interface deliberately leans toward EC2's
shape (e.g., `InstanceType`, `Spot`, `SubnetID` fields on `RunnerSpec`) but
also exposes K8s-native fields (`CPUCores`, `MemoryGiB`, `StorageGiB`) used by
Karpenter to provision nodes.

## Architecture [coverage: medium -- 2 sources]

The package layout is asymmetric:

- `pkg/provider/interface.go` — defines `Provider`, `PoolProvider`,
  `StateStore`, and `Coordinator` interfaces plus shared structs
  (`RunnerSpec`, `RunnerResult`, `RunnerState`, `Job`, `PoolConfig`).
- `pkg/provider/k8s/` — concrete implementation for Kubernetes
  (`provider.go`, `pool.go`).
- **No `pkg/provider/ec2/` directory.** The EC2 path is implemented directly
  in `pkg/fleet`. The `Provider` interface is used by call sites that need
  backend abstraction; EC2-specific code paths still call `pkg/fleet`
  helpers directly.

The K8s implementation is a thin facade over `client-go`. `Provider` holds a
`kubernetes.Interface` clientset and a pointer to `*config.Config`. It can be
constructed with in-cluster config or a kubeconfig path
(`NewProvider`), or with an injected fake clientset for tests
(`NewProviderWithClient`).

## Talks To [coverage: medium -- 2 sources]

- **Kubernetes API** via `k8s.io/client-go/kubernetes` — Pods, ConfigMaps,
  Secrets, PersistentVolumeClaims in `config.KubeNamespace`.
- **`pkg/config`** — reads `KubeNamespace`, `KubeRunnerImage`, `KubeDindImage`,
  `KubeNodeSelector`, `KubeTolerations`, `KubeStorageClass`,
  `KubeServiceAccount`, `KubeDockerGroupGID`, `KubeDaemonJSONConfigMap`,
  `KubeRegistryMirror`, `KubeIdleTimeoutMinutes`, `ValkeyAddr`,
  `ValkeyPassword`.
- **`pkg/queue`** — Valkey on K8s, SQS on EC2 (different concurrency
  semantics; see Key Decisions).
- **`pkg/fleet`** — the de facto EC2 implementation; not wrapped behind the
  `Provider` interface today.
- **Karpenter** — Pods carry `karpenter.sh/do-not-evict` and
  `karpenter.sh/do-not-disrupt` annotations to prevent node-level disruption
  during job execution. Resource requests on the Pod drive Karpenter's node
  provisioning decisions.

## API Surface [coverage: medium -- 1 source]

From `pkg/provider/interface.go`:

```go
type Provider interface {
    CreateRunner(ctx context.Context, spec *RunnerSpec) (*RunnerResult, error)
    TerminateRunner(ctx context.Context, runnerID string) error
    DescribeRunner(ctx context.Context, runnerID string) (*RunnerState, error)
    Name() string // "ec2" or "k8s"
}

type PoolProvider interface {
    ListPoolRunners(ctx context.Context, poolName string) ([]PoolRunner, error)
    StartRunners(ctx context.Context, runnerIDs []string) error
    StopRunners(ctx context.Context, runnerIDs []string) error
    TerminateRunners(ctx context.Context, runnerIDs []string) error
    MarkRunnerBusy(runnerID string)
    MarkRunnerIdle(runnerID string)
}

type StateStore interface {
    SaveJob / GetJob / MarkJobComplete / MarkJobTerminating / UpdateJobMetrics
    GetPoolConfig / SavePoolConfig / ListPools / UpdatePoolState
}

type Coordinator interface {
    IsLeader() bool
    Start(ctx context.Context) error
    Stop() error
}
```

`RunnerSpec` carries both EC2-shaped fields (`InstanceType`, `InstanceTypes`
for spot diversification, `Spot`, `ForceOnDemand`, `SubnetID`) and K8s-shaped
fields (`CPUCores`, `MemoryGiB`, `StorageGiB`). `JITToken`, `CacheToken`,
`RunnerGroup` are used by both backends to register the runner with GitHub.

## Data [coverage: medium -- 2 sources]

The K8s provider creates four resources per job, all in
`config.KubeNamespace`:

- **Pod** named `runner-<jobID>` (DNS-1123 sanitized, truncated to 63 chars
  via `sanitizePodName`).
- **ConfigMap** `<podName>-config` — non-sensitive runner config (`repo`,
  `labels` JSON, `runner_group`, `job_id`).
- **Secret** `<podName>-secrets` — `jit_token`, `cache_token`.
- **PersistentVolumeClaim** `<podName>-workspace` — `ReadWriteOnce`,
  `spec.StorageGiB` (default 30Gi), provisioned via `KubeStorageClass`
  (typically EBS CSI).

Default labels applied to all resources (merged with `config.KubeResourceLabels`,
defaults take precedence):

- `app=runs-fleet-runner`
- `runs-fleet.io/run-id`, `runs-fleet.io/job-id`, `runs-fleet.io/pool`
- `runs-fleet.io/arch`, `runs-fleet.io/os`, `runs-fleet.io/instance-type`
- `runs-fleet.io/spot=true` when `spec.Spot`

Pods include a DinD sidecar container (privileged) for Docker-in-Docker
support, an init container that copies runner externals into a shared
`emptyDir`, and (optionally) a `daemon-json` ConfigMap mount and registry
mirror flag. Resource requests/limits are derived from `spec.CPUCores` and
`spec.MemoryGiB` (defaults: 2 vCPU / 4 GiB) and applied identically to
runner and DinD containers to prevent starvation.

## Key Decisions [coverage: medium -- 3 sources]

- **Interface boundary at the orchestrator level, not below.** The `Provider`
  abstraction sits above pool managers and queue processors, not inside them.
  EC2 code in `pkg/fleet` is not wrapped behind the interface — only K8s
  has a dedicated `pkg/provider/<backend>/` subdir. This is the asymmetry
  to keep in mind when adding a new backend.
- **DinD sidecar for Docker support on K8s.** Each Pod runs a privileged
  `dind` container with `dockerd` listening on a shared `/var/run/docker.sock`
  emptyDir volume; the runner container talks to it via `DOCKER_HOST=
  unix:///var/run/docker.sock`. EC2 instances run Docker on the host
  directly.
- **K8s uses Valkey queue, EC2 uses SQS FIFO.** The two queues have
  different ordering and concurrency semantics — SQS FIFO guarantees
  per-message-group ordering with a single in-flight consumer per group;
  Valkey lists/streams have looser semantics. Code that assumes one
  backend's behavior is unlikely to port cleanly.
- **Spot scheduling on K8s via tolerations, not API mode.** `buildTolerations`
  emits tolerations for GKE preemptible/spot, EKS spot, AKS spot, GKE
  provisioning, generic `runs-fleet.io/preemptible`, and `karpenter.sh/
  disruption`. There is no equivalent of EC2's `spot-and-on-demand` fleet
  fallback — node scheduling is the cluster's problem.
- **No "stopped" state on K8s.** `PoolProvider.StartRunners` is a no-op,
  and `StopRunners` is implemented as `deletePods`. Warm-pool size is
  managed by creating new pods; idle pods past `KubeIdleTimeoutMinutes`
  (default 10) are terminated by `PoolProvider.ReconcileLoop`.
- **Karpenter-aware Pod annotations.** `karpenter.sh/do-not-evict` and
  `karpenter.sh/do-not-disrupt` are set on every runner Pod to prevent
  Karpenter consolidation from killing in-flight jobs.

## Gotchas [coverage: medium -- 3 sources]

- **Asymmetric package layout.** New contributors looking for "where is the
  EC2 provider" will not find one — `pkg/provider/ec2/` does not exist.
  EC2 lives in `pkg/fleet`.
- **Lifecycle mismatch.** EC2 instances support stop/start (used by warm
  pools to keep instances cheap); K8s pods do not. `PoolProvider.
  StartRunners` returns `nil` without doing anything on the K8s backend.
  Code that assumes "stopped" is a meaningful state will silently behave
  differently across backends.
- **Metadata service differs.** EC2 uses IMDSv2 (token-protected), K8s pods
  rely on the downward API and projected service account tokens. Agent
  code that reads `169.254.169.254` works on EC2 only.
- **No distributed locks on K8s.** Per-pool reconcile locks in DynamoDB
  exist for the EC2 backend; the K8s pool reconciler relies on K8s API
  idempotency (Delete is safe to retry, Create returns `AlreadyExists`).
  The local `podIdle` map in `PoolProvider` is per-instance and is not
  coordinated across replicas.
- **Pod name collision risk.** `sanitizePodName` truncates to 63 characters
  to satisfy DNS-1123. For very large `jobID` values combined with the
  `runner-` prefix this is fine, but custom prefixes risk collisions.
- **JITToken required.** `createRunnerSecret` returns an error if
  `spec.JITToken` is empty — failing fast before Pod creation. Callers
  that forget to populate it get an early failure rather than a Pod that
  never registers.
- **Cleanup on partial failure.** `CreateRunner` uses a `cleanup(includePod)`
  closure to delete the ConfigMap, Secret, and PVC if any later step
  fails. `TerminateRunner` collects errors across Pod, PVC, ConfigMap,
  Secret deletes and returns a combined error — partial cleanup is
  reported, not silently swallowed.
- **Resource limits == requests.** The K8s provider sets `Limits` equal to
  `Requests` for both CPU and memory. This means BestEffort/Burstable QoS
  is not used — runners are Guaranteed QoS. Don't expect bursting beyond
  the requested CPU.

## Sources [coverage: high]

- [pkg/provider/interface.go](../../pkg/provider/interface.go)
- [pkg/provider/k8s/provider.go](../../pkg/provider/k8s/provider.go)
- [pkg/provider/k8s/pool.go](../../pkg/provider/k8s/pool.go)
