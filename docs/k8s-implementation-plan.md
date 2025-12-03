# Kubernetes Backend Implementation Plan

## Goal
Add Kubernetes as an alternative compute backend. **Server runs in one mode** - either EC2 or K8s per deployment, not mixed.

---

## Deployment Modes

```bash
# EC2 mode (current behavior)
RUNS_FLEET_MODE=ec2

# K8s mode
RUNS_FLEET_MODE=k8s
```

| Aspect | EC2 Mode | K8s Mode |
|--------|----------|----------|
| Compute | EC2 Fleet API | K8s Jobs + Karpenter |
| State storage | DynamoDB | ConfigMaps |
| Runner config | SSM Parameter Store | K8s Secrets |
| Warm pools | Stopped EC2 instances | Placeholder pods |
| Metrics | CloudWatch | CloudWatch (unified) |
| Queue | SQS FIFO | Valkey Streams |

**Workflow labels unchanged** - same `runs-on` format works for both modes:
```yaml
runs-on: "runs-fleet=${{ github.run_id }}/runner=4cpu-linux-arm64/pool=default"
```

**Migration path**: Deploy K8s-mode instance alongside EC2-mode, route test repos via separate webhook endpoint, then cut over.

---

## Architecture Changes

### 1. Provider Interface (`pkg/provider/`)

```go
// pkg/provider/interface.go

type Provider interface {
    // CreateRunner provisions compute for a job, returns runner identifiers
    CreateRunner(ctx context.Context, spec *RunnerSpec) (*RunnerResult, error)

    // TerminateRunner removes compute resources
    TerminateRunner(ctx context.Context, runnerID string) error

    // DescribeRunner returns current state
    DescribeRunner(ctx context.Context, runnerID string) (*RunnerState, error)

    // Name returns provider identifier ("ec2" or "k8s")
    Name() string
}

type PoolProvider interface {
    // ListPoolRunners returns all runners in a pool
    ListPoolRunners(ctx context.Context, poolName string) ([]PoolRunner, error)

    // StartRunners activates stopped/scaled-down runners
    StartRunners(ctx context.Context, runnerIDs []string) error

    // StopRunners pauses runners without terminating
    StopRunners(ctx context.Context, runnerIDs []string) error

    // TerminateRunners removes runners from pool
    TerminateRunners(ctx context.Context, runnerIDs []string) error
}

type ConfigStore interface {
    StoreRunnerConfig(ctx context.Context, runnerID string, cfg *RunnerConfig) error
    GetRunnerConfig(ctx context.Context, runnerID string) (*RunnerConfig, error)
    DeleteRunnerConfig(ctx context.Context, runnerID string) error
}

// StateStore replaces DynamoDB in K8s mode
type StateStore interface {
    // Jobs table equivalent
    SaveJob(ctx context.Context, job *Job) error
    GetJob(ctx context.Context, jobID string) (*Job, error)
    UpdateJobStatus(ctx context.Context, jobID string, status JobStatus) error
    ListPendingJobs(ctx context.Context) ([]*Job, error)

    // Locks table equivalent (leader election)
    AcquireLock(ctx context.Context, lockName string, holder string, ttl time.Duration) (bool, error)
    RenewLock(ctx context.Context, lockName string, holder string, ttl time.Duration) error
    ReleaseLock(ctx context.Context, lockName string, holder string) error

    // Pools table equivalent
    SavePoolConfig(ctx context.Context, pool *PoolConfig) error
    GetPoolConfig(ctx context.Context, poolName string) (*PoolConfig, error)
    ListPoolConfigs(ctx context.Context) ([]*PoolConfig, error)
}

type RunnerSpec struct {
    RunID         string
    JobID         string
    Repo          string
    Labels        []string
    InstanceType  string            // EC2: instance type, K8s: resource class
    Spot          bool              // EC2: spot, K8s: preemptible nodes
    Private       bool              // EC2: private subnet, K8s: namespace/network policy
    Pool          string
    Arch          string            // arm64 or amd64

    // EC2-specific (ignored by K8s)
    SubnetID      string

    // K8s-specific (ignored by EC2)
    Namespace     string
    NodeSelector  map[string]string
}

type RunnerResult struct {
    RunnerIDs     []string          // Instance IDs or Pod names
    ProviderData  map[string]string // Provider-specific metadata
}

type RunnerState struct {
    RunnerID      string
    State         string // "pending", "running", "stopped", "terminated"
    StartTime     time.Time
    ProviderData  map[string]string
}

type PoolRunner struct {
    RunnerID     string
    State        string
    Pool         string
    InstanceType string
    IdleSince    time.Time
}
```

### 2. EC2 Provider (`pkg/provider/ec2/`)

Refactor existing `pkg/fleet/` code:

```
pkg/provider/ec2/
├── provider.go      # Implements Provider interface (wraps fleet.Manager)
├── pool.go          # Implements PoolProvider (wraps pools.Manager EC2 ops)
├── config.go        # SSM-based ConfigStore implementation
└── types.go         # EC2-specific config structs
```

**Key changes:**
- `fleet.Manager` becomes internal, wrapped by `ec2.Provider`
- `pools.Manager` EC2 operations extracted to `ec2.PoolProvider`
- SSM operations moved from `pkg/runner/` to `pkg/provider/ec2/config.go`

### 3. K8s Provider (`pkg/provider/k8s/`)

```
pkg/provider/k8s/
├── provider.go      # Creates Jobs/Pods for runners
├── pool.go          # Manages Deployments with replica scaling
├── config.go        # ConfigMap/Secret-based ConfigStore
├── client.go        # k8s client initialization
└── types.go         # K8s-specific config structs
```

**K8s resource model:**

| runs-fleet concept | EC2 | Kubernetes |
|-------------------|-----|------------|
| Runner instance | EC2 instance | Pod (in Job) |
| Warm pool | Running instances | Deployment with HPA min replicas |
| Stopped pool | Stopped instances | Deployment scaled to 0 |
| Spot | Spot Fleet | Preemptible/Spot node pool |
| Private network | Private subnet + SG | Namespace + NetworkPolicy |
| Config delivery | SSM Parameter Store | ConfigMap + Secret |
| Self-termination | EC2 API | Pod completion (Job) |

**Pod template:**
```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: runs-fleet-runner-{run-id}-{job-id}
  namespace: runs-fleet
  labels:
    runs-fleet.io/run-id: "12345"
    runs-fleet.io/pool: "default"
spec:
  ttlSecondsAfterFinished: 300
  backoffLimit: 0
  template:
    spec:
      serviceAccountName: runs-fleet-runner
      nodeSelector:
        kubernetes.io/arch: arm64
        node.kubernetes.io/instance-type: c7g.xlarge  # or resource class
      tolerations:
        - key: "runs-fleet.io/preemptible"
          operator: "Exists"
      containers:
        - name: runner
          image: runs-fleet-agent:latest
          env:
            - name: RUNS_FLEET_RUNNER_ID
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: RUNS_FLEET_CONFIG_SOURCE
              value: "configmap"
          resources:
            requests:
              cpu: "4"
              memory: "8Gi"
          volumeMounts:
            - name: runner-config
              mountPath: /etc/runs-fleet
      volumes:
        - name: runner-config
          configMap:
            name: runs-fleet-runner-{run-id}-{job-id}
      restartPolicy: Never
```

### 4. Agent Changes (`cmd/agent/`, `pkg/agent/`)

Current agent flow (EC2):
1. Fetch config from SSM
2. Register with GitHub
3. Run job
4. Send telemetry to SQS
5. Self-terminate via EC2 API

**K8s flow:**
1. Read config from mounted ConfigMap/Secret
2. Register with GitHub
3. Run job
4. Send telemetry to Valkey
5. Exit cleanly (pod terminates naturally)

**Implementation approach - Interface abstractions:**

```go
// pkg/agent/config.go - Config fetching abstraction

// ConfigFetcher defines the interface for fetching runner configuration.
type ConfigFetcher interface {
    FetchConfig(ctx context.Context, source string) (*RunnerConfig, error)
}

// SSM implementation (existing Registrar.FetchConfig)
// File implementation (K8s ConfigMap/Secret)
type FileConfigFetcher struct { ... }

// K8s config paths
type K8sConfigPaths struct {
    ConfigDir string // /etc/runs-fleet/config (ConfigMap mount)
    SecretDir string // /etc/runs-fleet/secrets (Secret mount)
}

// FetchK8sConfig assembles RunnerConfig from mounted volumes
func FetchK8sConfig(ctx context.Context, paths K8sConfigPaths, logger Logger) (*RunnerConfig, error)

// IsK8sEnvironment detects if running in Kubernetes
func IsK8sEnvironment() bool {
    return os.Getenv("KUBERNETES_SERVICE_HOST") != "" ||
           fileExists("/var/run/secrets/kubernetes.io/serviceaccount/token")
}
```

```go
// pkg/agent/termination.go - Terminator interface

type Terminator interface {
    TerminateInstance(ctx context.Context, instanceID string, status JobStatus) error
}

// EC2Terminator - existing implementation
// Calls EC2 TerminateInstances API after sending SQS telemetry

// K8sTerminator - new implementation
// Sends Valkey telemetry, then returns (pod exits naturally)
type K8sTerminator struct {
    telemetry TelemetryClient
    logger    Logger
}

func (t *K8sTerminator) TerminateInstance(ctx context.Context, podName string, status JobStatus) error {
    if t.telemetry != nil {
        t.telemetry.SendJobCompleted(ctx, status)
    }
    // Pod will terminate when main() returns
    return nil
}
```

```go
// pkg/agent/telemetry.go - Telemetry interface

type TelemetryClient interface {
    SendJobStarted(ctx context.Context, status JobStatus) error
    SendJobCompleted(ctx context.Context, status JobStatus) error
}

// SQSTelemetry - existing implementation for EC2 mode
// ValkeyTelemetry - new implementation for K8s mode
type ValkeyTelemetry struct {
    client *redis.Client
    stream string // "runs-fleet:termination"
    logger Logger
}
```

**K8s ConfigMap/Secret structure:**

ConfigMap (`runs-fleet-runner-{run-id}`):
```yaml
data:
  repo: "owner/repo"
  labels: '["self-hosted","linux","arm64"]'
  runner_group: "default"
  job_id: "12345"
```

Secret (`runs-fleet-runner-{run-id}-secrets`):
```yaml
data:
  jit_token: <base64-encoded>
  cache_token: <base64-encoded>
```

**Agent main.go wiring:**

```go
func main() {
    if agent.IsK8sEnvironment() {
        // K8s mode
        config, err := agent.FetchK8sConfig(ctx, agent.DefaultK8sConfigPaths(), logger)

        var telemetry agent.TelemetryClient
        if valkeyAddr := os.Getenv("RUNS_FLEET_VALKEY_ADDR"); valkeyAddr != "" {
            telemetry = agent.NewValkeyTelemetry(valkeyAddr, "runs-fleet:termination", logger)
        }

        terminator = agent.NewK8sTerminator(telemetry, logger)
    } else {
        // EC2 mode (existing)
        config, err = registrar.FetchConfig(ctx, ssmParameterPath)
        telemetry = agent.NewSQSTelemetry(awsCfg, terminationQueueURL, logger)
        terminator = agent.NewEC2Terminator(awsCfg, telemetry, logger)
    }
    // ... rest of agent flow unchanged
}
```

**K8s Provider Pod Spec updates:**

```go
// pkg/provider/k8s/provider.go - Add volume mounts for config

func (p *Provider) buildPodSpec(name string, spec *provider.RunnerSpec) *corev1.Pod {
    // ... existing code ...

    pod.Spec.Volumes = []corev1.Volume{
        {
            Name: "config",
            VolumeSource: corev1.VolumeSource{
                ConfigMap: &corev1.ConfigMapVolumeSource{
                    LocalObjectReference: corev1.LocalObjectReference{
                        Name: configMapName(spec.RunID),
                    },
                },
            },
        },
        {
            Name: "secrets",
            VolumeSource: corev1.VolumeSource{
                Secret: &corev1.SecretVolumeSource{
                    SecretName: secretName(spec.RunID),
                },
            },
        },
    }

    pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
        {Name: "config", MountPath: "/etc/runs-fleet/config", ReadOnly: true},
        {Name: "secrets", MountPath: "/etc/runs-fleet/secrets", ReadOnly: true},
    }

    // Add Valkey address for telemetry
    pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
        corev1.EnvVar{Name: "RUNS_FLEET_VALKEY_ADDR", Value: p.config.ValkeyAddr},
    )
}

// CreateRunner must also create ConfigMap and Secret before creating Pod
func (p *Provider) CreateRunner(ctx context.Context, spec *provider.RunnerSpec) (*provider.RunnerResult, error) {
    // 1. Create ConfigMap with runner config
    if err := p.createRunnerConfigMap(ctx, spec); err != nil {
        return nil, err
    }

    // 2. Create Secret with JIT token
    if err := p.createRunnerSecret(ctx, spec); err != nil {
        p.deleteConfigMap(ctx, spec.RunID) // cleanup
        return nil, err
    }

    // 3. Create Pod (existing)
    // ...
}
```

### 5. Config Changes (`pkg/config/`)

```go
type Config struct {
    // Existing fields...

    // Provider selection
    DefaultBackend string // "ec2" or "k8s" (default: "ec2")

    // EC2-specific (required when using EC2)
    VPCID              string
    PublicSubnetIDs    []string
    PrivateSubnetIDs   []string
    SecurityGroupID    string
    InstanceProfileARN string

    // Kubernetes-specific (required when using K8s)
    KubeConfig         string // Path to kubeconfig (empty = in-cluster)
    KubeNamespace      string // Default namespace for runners
    KubeServiceAccount string // SA for runner pods
    KubeNodeSelector   map[string]string
}

func (c *Config) Validate() error {
    // Validate based on enabled backends
    if c.DefaultBackend == "ec2" || c.hasEC2Pools() {
        if err := c.validateEC2Config(); err != nil {
            return err
        }
    }
    if c.DefaultBackend == "k8s" || c.hasK8sPools() {
        if err := c.validateK8sConfig(); err != nil {
            return err
        }
    }
    return nil
}
```

### 6. Queue Abstraction (`pkg/queue/`)

Abstract queue interface to support SQS (EC2 mode) and Valkey Streams (K8s mode):

```go
// pkg/queue/interface.go

type Queue interface {
    // SendMessage publishes a job to the queue
    SendMessage(ctx context.Context, job *JobMessage) error

    // ReceiveMessages retrieves messages with long polling
    ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]Message, error)

    // DeleteMessage acknowledges successful processing
    DeleteMessage(ctx context.Context, handle string) error
}

// Message wraps queue-specific message types
type Message struct {
    ID            string
    Body          string
    Handle        string // ReceiptHandle (SQS) or message ID (Valkey)
    Attributes    map[string]string
}
```

**SQS Implementation** (existing, refactored):
```go
// pkg/queue/sqs.go
type SQSClient struct {
    client   SQSAPI
    queueURL string
}

func (c *SQSClient) SendMessage(ctx context.Context, job *JobMessage) error {
    // Existing FIFO logic with MessageGroupId, deduplication
}
```

**Valkey Implementation** (new):
```go
// pkg/queue/valkey.go
type ValkeyClient struct {
    client     *redis.Client
    stream     string // e.g., "runs-fleet:jobs"
    group      string // consumer group for exactly-once
    consumerID string
}

func (c *ValkeyClient) SendMessage(ctx context.Context, job *JobMessage) error {
    return c.client.XAdd(ctx, &redis.XAddArgs{
        Stream: c.stream,
        Values: map[string]interface{}{
            "data":     marshal(job),
            "group_id": job.RunID, // Preserve FIFO ordering per run
        },
    }).Err()
}

func (c *ValkeyClient) ReceiveMessages(ctx context.Context, max int32, wait int32) ([]Message, error) {
    streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
        Group:    c.group,
        Consumer: c.consumerID,
        Streams:  []string{c.stream, ">"},
        Count:    int64(max),
        Block:    time.Duration(wait) * time.Second,
    }).Result()
    // Convert XMessage to Message...
}

func (c *ValkeyClient) DeleteMessage(ctx context.Context, handle string) error {
    return c.client.XAck(ctx, c.stream, c.group, handle).Err()
}
```

**Queue selection in main.go:**
```go
var mainQueue, poolQueue, eventsQueue queue.Queue

switch cfg.Mode {
case "ec2":
    mainQueue = queue.NewSQSClient(awsCfg, cfg.QueueURL)
    poolQueue = queue.NewSQSClient(awsCfg, cfg.PoolQueueURL)
    eventsQueue = queue.NewSQSClient(awsCfg, cfg.EventsQueueURL)
case "k8s":
    valkeyClient := queue.NewValkeyClient(cfg.ValkeyURL)
    mainQueue = valkeyClient.Stream("runs-fleet:jobs")
    poolQueue = valkeyClient.Stream("runs-fleet:pool")
    eventsQueue = valkeyClient.Stream("runs-fleet:events")
}
```

**Valkey K8s deployment:**
```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: valkey
  namespace: runs-fleet
spec:
  serviceName: valkey
  replicas: 1
  selector:
    matchLabels:
      app: valkey
  template:
    metadata:
      labels:
        app: valkey
    spec:
      containers:
        - name: valkey
          image: valkey/valkey:8
          ports:
            - containerPort: 6379
          args: ["--appendonly", "yes"]
          volumeMounts:
            - name: data
              mountPath: /data
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: 1Gi
---
apiVersion: v1
kind: Service
metadata:
  name: valkey
  namespace: runs-fleet
spec:
  selector:
    app: valkey
  ports:
    - port: 6379
```

### 7. Label Parsing Changes (`pkg/github/webhook.go`)

```go
type JobConfig struct {
    // Existing fields...

    Backend string // "ec2", "k8s", or "" (use default)
}

// In ParseLabels:
case "backend":
    if value == "ec2" || value == "k8s" {
        cfg.Backend = value
    }
```

### 7. Server Orchestration (`cmd/server/main.go`)

```go
func main() {
    cfg := config.Load()

    // Initialize providers based on config
    providers := make(map[string]provider.Provider)

    if cfg.DefaultBackend == "ec2" || cfg.hasEC2Config() {
        ec2Provider, err := ec2.NewProvider(cfg)
        if err != nil {
            log.Fatal(err)
        }
        providers["ec2"] = ec2Provider
    }

    if cfg.DefaultBackend == "k8s" || cfg.hasK8sConfig() {
        k8sProvider, err := k8s.NewProvider(cfg)
        if err != nil {
            log.Fatal(err)
        }
        providers["k8s"] = k8sProvider
    }

    // Router selects provider per job
    router := provider.NewRouter(providers, cfg.DefaultBackend)

    // In processJob:
    backend := router.SelectBackend(job)
    prov := providers[backend]
    result, err := prov.CreateRunner(ctx, spec)
}
```

---

## K8s Pool Strategies

### Option 1: Deployment Scaling (Recommended for warm pools)
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: runs-fleet-pool-default
spec:
  replicas: 3  # Hot pool
  selector:
    matchLabels:
      runs-fleet.io/pool: default
```
- Scale to 0 = "stopped"
- Scale up = "start instances"
- HPA for auto-scaling based on queue depth

### Option 2: Job-per-Runner (Cold start)
Each job creates a new Job resource, no pool reuse.

### Option 3: StatefulSet with PVC (Persistent cache)
For pools that benefit from local cache persistence.

---

## Migration Path

### Phase 1: Interface Abstraction
1. Create `pkg/provider/interface.go`
2. Wrap existing EC2 code in `pkg/provider/ec2/`
3. No behavioral changes - pure refactor
4. All tests must pass

### Phase 2: Label & Config Support
1. Add `backend=` label parsing
2. Add `RUNS_FLEET_DEFAULT_BACKEND` config
3. Add K8s config fields (kubeconfig, namespace, etc.)
4. Config validation per backend

### Phase 3: Kubernetes Provider
1. Implement `k8s.Provider` (Job creation)
2. Implement `k8s.ConfigStore` (ConfigMap-based)
3. Agent K8s detection and config loading
4. Basic cold-start flow working

### Phase 4: K8s Pools
1. Implement `k8s.PoolProvider` (Deployment scaling)
2. Pool queue routing for K8s
3. Idle detection and scale-down

### Phase 5: Production Hardening
1. Preemptible node support (spot equivalent)
2. NetworkPolicy for private jobs
3. Resource quotas and limits
4. Monitoring and metrics

---

## Files to Create

```
pkg/provider/
├── interface.go          # Provider, PoolProvider, ConfigStore
├── router.go             # Backend selection logic
├── types.go              # Shared types (RunnerSpec, RunnerState, etc.)
├── ec2/
│   ├── provider.go
│   ├── pool.go
│   ├── config.go
│   └── types.go
└── k8s/
    ├── provider.go
    ├── pool.go
    ├── config.go
    ├── client.go
    └── types.go
```

## Files to Modify

| File | Change |
|------|--------|
| `pkg/github/webhook.go` | Add `Backend` field, parse `backend=` label |
| `pkg/config/config.go` | Add K8s config fields, backend selection |
| `cmd/server/main.go` | Provider initialization and routing |
| `cmd/agent/main.go` | Provider detection, config source selection |
| `pkg/agent/registration.go` | ConfigStore interface usage |
| `pkg/agent/termination.go` | Provider-aware termination |
| `pkg/fleet/fleet.go` | Extract to provider interface |
| `pkg/pools/manager.go` | Use PoolProvider interface |

---

## Open Questions

1. **K8s cluster management**: Will runs-fleet provision K8s clusters, or use existing?
   - Recommendation: Use existing clusters, runs-fleet creates workloads only

2. **Node pool strategy**: Dedicated node pools for runners, or shared?
   - Recommendation: Dedicated with taints/tolerations for isolation

3. **Image registry**: Where will runner images be stored?
   - Recommendation: ECR (already have AWS), or configurable

4. **Secrets management**: K8s Secrets vs external (Vault, AWS Secrets Manager)?
   - Recommendation: K8s Secrets for runner config, external for GitHub app creds

5. **Multi-cluster support**: Single cluster or federated?
   - Recommendation: Start single, add cluster selection label later

---

## Implementation Progress

### Phase 1: Interface Abstraction ✅ COMPLETE
| Item | Status | Notes |
|------|--------|-------|
| `pkg/provider/interface.go` | ✅ Done | Provider, PoolProvider, RunnerSpec, RunnerState |
| `pkg/provider/types.go` | ✅ Done | Shared types |
| EC2 provider wrap | ⏭️ Skipped | Using existing fleet code directly; K8s-only focus |

### Phase 2: Label & Config Support ✅ COMPLETE
| Item | Status | Notes |
|------|--------|-------|
| K8s config fields in `pkg/config/` | ✅ Done | KubeConfig, KubeNamespace, KubeServiceAccount, KubeNodeSelector, KubeRunnerImage, ValkeyAddr |
| `IsK8sBackend()` / `IsEC2Backend()` | ✅ Done | Mode detection helpers |
| Config validation | ✅ Done | Validates required fields per mode |

### Phase 3: Kubernetes Provider ✅ COMPLETE
| Item | Status | Notes |
|------|--------|-------|
| `pkg/provider/k8s/provider.go` | ✅ Done | CreateRunner (Pod), TerminateRunner, DescribeRunner |
| Pod spec with security context | ✅ Done | Non-root, seccomp, drop capabilities |
| Node selector for arch | ✅ Done | arm64/amd64 selection |
| Spot/preemptible tolerations | ✅ Done | GKE, EKS, AKS, Karpenter tolerations |
| NetworkPolicy for private jobs | ✅ Done | Blocks internal CIDRs, allows DNS/HTTPS |
| `pkg/provider/k8s/pool.go` | ✅ Done | K8s PoolProvider implementation |

### Phase 4: Queue Abstraction ✅ COMPLETE
| Item | Status | Notes |
|------|--------|-------|
| `pkg/queue/interface.go` | ✅ Done | Queue interface, Message, JobMessage types |
| `pkg/queue/sqs.go` | ✅ Done | Refactored to implement Queue interface |
| `pkg/queue/valkey.go` | ✅ Done | Valkey Streams implementation with consumer groups |
| Server main.go integration | ✅ Done | `initJobQueue()` selects SQS or Valkey based on backend |
| Valkey K8s manifests | ✅ Done | StatefulSet + Service in docs |

### Phase 5: Agent K8s Support ✅ COMPLETE
| Item | Status | Notes |
|------|--------|-------|
| `pkg/agent/config.go` | ✅ Done | FileConfigFetcher, FetchK8sConfig, IsK8sEnvironment |
| `pkg/agent/termination.go` interface | ✅ Done | InstanceTerminator interface, EC2Terminator renamed |
| `pkg/agent/termination_k8s.go` | ✅ Done | K8sTerminator (sends telemetry, clean exit) |
| `pkg/agent/telemetry.go` interface | ✅ Done | TelemetryClient interface, status constants, DetermineCompletionStatus helper |
| `pkg/agent/telemetry_valkey.go` | ✅ Done | ValkeyTelemetry using Redis Streams |
| `cmd/agent/main.go` wiring | ✅ Done | IsK8sEnvironment detection, initK8sMode/initEC2Mode split |
| K8s provider ConfigMap/Secret creation | ✅ Done | createRunnerConfigMap, createRunnerSecret with JITToken validation |
| K8s provider volume mounts in Pod | ✅ Done | /etc/runs-fleet/config and /etc/runs-fleet/secrets |
| K8s provider cleanup on failure | ✅ Done | Atomic cleanup with error logging |

### Phase 6: Runner Docker Image ✅ COMPLETE
| Item | Status | Notes |
|------|--------|-------|
| `docker/runner/Dockerfile` | ✅ Done | Ubuntu 22.04 + GitHub Actions runner + runs-fleet agent |
| `docker/runner/entrypoint.sh` | ✅ Done | Entrypoint script for pod startup |
| Multi-arch build (arm64/amd64) | ✅ Done | `.github/workflows/build-runner.yml` with buildx |
| Makefile targets | ✅ Done | docker-build-runner, docker-build-runner-local, docker-push-runner |
| Push to registry | ✅ Done | ECR via GitHub Actions workflow |

### Phase 7: Helm Chart ✅ COMPLETE
| Item | Status | Notes |
|------|--------|-------|
| `deploy/helm/runs-fleet/Chart.yaml` | ✅ Done | Helm chart metadata |
| `deploy/helm/runs-fleet/values.yaml` | ✅ Done | Configurable values with defaults |
| `deploy/helm/runs-fleet/templates/_helpers.tpl` | ✅ Done | Template helper functions |
| `deploy/helm/runs-fleet/templates/rbac.yaml` | ✅ Done | Orchestrator ServiceAccount, Role, RoleBinding |
| `deploy/helm/runs-fleet/templates/runner-rbac.yaml` | ✅ Done | Runner ServiceAccount |
| `deploy/helm/runs-fleet/templates/deployment.yaml` | ✅ Done | Orchestrator Deployment with env config |
| `deploy/helm/runs-fleet/templates/service.yaml` | ✅ Done | Orchestrator ClusterIP Service |
| `deploy/helm/runs-fleet/templates/secrets.yaml` | ✅ Done | GitHub App credentials Secret |
| `deploy/helm/runs-fleet/templates/valkey.yaml` | ✅ Done | Valkey StatefulSet (conditional) |
| `deploy/helm/runs-fleet/templates/ingress.yaml` | ✅ Done | Webhook Ingress (conditional) |
| `deploy/helm/runs-fleet/templates/NOTES.txt` | ✅ Done | Post-install instructions |
| `deploy/helm/runs-fleet/templates/istio-*.yaml` | ✅ Done | Istio VirtualService, DestinationRule, PeerAuthentication (conditional) |

### Phase 8: Karpenter Setup 🔴 NOT STARTED
| Item | Status | Notes |
|------|--------|-------|
| NodePool for runners | 🔴 TODO | Spot-first, arm64/amd64 |
| EC2NodeClass | 🔴 TODO | AMI, security groups, subnet selection |
| Documentation | 🔴 TODO | Setup guide for EKS + Karpenter |

---

## Summary

| Phase | Status | Completion |
|-------|--------|------------|
| 1. Interface Abstraction | ✅ Complete | 100% |
| 2. Label & Config Support | ✅ Complete | 100% |
| 3. Kubernetes Provider | ✅ Complete | 100% |
| 4. Queue Abstraction | ✅ Complete | 100% |
| 5. Agent K8s Support | ✅ Complete | 100% |
| 6. Runner Docker Image | ✅ Complete | 100% |
| 7. Helm Chart | ✅ Complete | 100% |
| 8. Karpenter Setup | 🔴 Not Started | 0% |

**Overall Progress: ~87.5%**

**Next Steps:**
1. Karpenter Setup (Phase 8) - NodePool, EC2NodeClass, documentation
