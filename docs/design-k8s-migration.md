# runs-fleet Kubernetes Migration Design

## Overview

This document describes a Kubernetes-native architecture for runs-fleet, replacing direct EC2 Fleet API calls with Kubernetes Jobs and Karpenter for node provisioning. The goal is faster warm-path job pickup while maintaining the responsive webhook-driven architecture.

## Motivation

**Current limitations:**
- Stopped EC2 instance warm pool takes ~30s to start
- Direct EC2 management requires custom spot interruption handling
- Tag-based instance discovery adds operational complexity

**Kubernetes benefits:**
- Warm path reduced to ~3-5s (pod scheduling vs EC2 state transition)
- Native spot interruption handling via Karpenter
- Standard K8s observability and tooling

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                         GitHub                                       │
│                           │                                          │
│                    webhook (workflow_job)                            │
└───────────────────────────┬─────────────────────────────────────────┘
                            ▼
┌─────────────────────────────────────────────────────────────────────┐
│                    API Gateway → SQS FIFO                            │
│                    (unchanged from current)                          │
└───────────────────────────┬─────────────────────────────────────────┘
                            ▼
┌─────────────────────────────────────────────────────────────────────┐
│                      Orchestrator Pod                                │
│                  (Deployment in K8s cluster)                         │
│                                                                      │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                  │
│  │ Queue       │  │ Job         │  │ Pool        │                  │
│  │ Processor   │  │ Creator     │  │ Controller  │                  │
│  └──────┬──────┘  └──────┬──────┘  └──────┬──────┘                  │
└─────────┼────────────────┼────────────────┼─────────────────────────┘
          │                │                │
          │                ▼                │
          │    ┌───────────────────┐        │
          │    │   K8s API Server  │◄───────┘
          │    └─────────┬─────────┘
          │              │
          │              ▼
          │    ┌───────────────────┐
          │    │    Karpenter      │
          │    │  (NodePool CRDs)  │
          │    └─────────┬─────────┘
          │              │
          │              ▼
          │    ┌───────────────────┐
          │    │   EC2 Fleet API   │
          │    │  (spot/on-demand) │
          │    └─────────┬─────────┘
          │              │
          ▼              ▼
┌─────────────────────────────────────────────────────────────────────┐
│                        Worker Nodes                                  │
│                                                                      │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐      │
│  │  Runner Pod     │  │  Runner Pod     │  │  Placeholder    │      │
│  │  (Job workload) │  │  (Job workload) │  │  Pod (warm)     │      │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘      │
└─────────────────────────────────────────────────────────────────────┘
```

## Component Mapping

| Current (EC2) | Kubernetes | Notes |
|---------------|------------|-------|
| `pkg/fleet/` | `pkg/k8s/jobs/` | Create K8s Jobs instead of EC2 Fleets |
| `pkg/pools/` | `pkg/k8s/warmpool/` | Manage placeholder pods + NodePool config |
| `pkg/runner/` | ConfigMap/Secret | Runner config as K8s resources, not SSM |
| `pkg/termination/` | (removed) | Karpenter handles node lifecycle |
| `pkg/events/` | (removed) | Karpenter handles spot interruptions |
| `pkg/housekeeping/` | `pkg/k8s/cleanup/` | Clean orphaned Jobs/Pods |
| EC2 tags | K8s labels | Same metadata, different format |
| DynamoDB (jobs) | K8s Job status | Native job tracking |
| DynamoDB (locks) | K8s Lease | Native leader election |

## Karpenter Configuration

### NodePool Definitions

One NodePool per runner specification:

```yaml
# nodepool-arm64-small.yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: runners-arm64-small
  labels:
    runs-fleet/runner-class: 2cpu-linux-arm64
spec:
  template:
    metadata:
      labels:
        runs-fleet/managed: "true"
        runs-fleet/runner-class: 2cpu-linux-arm64
    spec:
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot", "on-demand"]
        - key: kubernetes.io/arch
          operator: In
          values: ["arm64"]
        - key: node.kubernetes.io/instance-type
          operator: In
          values: ["t4g.medium", "t4g.large", "c7g.medium"]
      taints:
        - key: runs-fleet/runner
          value: "true"
          effect: NoSchedule
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: runners-arm64

  limits:
    cpu: 100  # max 100 vCPUs in this pool

  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 60m
    budgets:
      - nodes: "0"  # never disrupt nodes with running pods

---
# nodepool-arm64-large.yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: runners-arm64-large
  labels:
    runs-fleet/runner-class: 8cpu-linux-arm64
spec:
  template:
    metadata:
      labels:
        runs-fleet/managed: "true"
        runs-fleet/runner-class: 8cpu-linux-arm64
    spec:
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot", "on-demand"]
        - key: kubernetes.io/arch
          operator: In
          values: ["arm64"]
        - key: node.kubernetes.io/instance-type
          operator: In
          values: ["c7g.2xlarge", "m7g.2xlarge", "r7g.2xlarge"]
      taints:
        - key: runs-fleet/runner
          value: "true"
          effect: NoSchedule
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: runners-arm64

  limits:
    cpu: 200

  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 30m
    budgets:
      - nodes: "0"

---
# nodepool-amd64.yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: runners-amd64
  labels:
    runs-fleet/runner-class: 4cpu-linux-amd64
spec:
  template:
    metadata:
      labels:
        runs-fleet/managed: "true"
        runs-fleet/runner-class: 4cpu-linux-amd64
    spec:
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot", "on-demand"]
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
        - key: node.kubernetes.io/instance-type
          operator: In
          values: ["c6i.xlarge", "c6a.xlarge", "m6i.xlarge"]
      taints:
        - key: runs-fleet/runner
          value: "true"
          effect: NoSchedule
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: runners-amd64

  limits:
    cpu: 100

  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 60m
    budgets:
      - nodes: "0"
```

### EC2NodeClass

```yaml
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: runners-arm64
spec:
  role: runs-fleet-node-role

  amiSelectorTerms:
    - alias: al2023@latest

  subnetSelectorTerms:
    - tags:
        runs-fleet/subnet: "true"

  securityGroupSelectorTerms:
    - tags:
        runs-fleet/security-group: "true"

  blockDeviceMappings:
    - deviceName: /dev/xvda
      ebs:
        volumeSize: 50Gi
        volumeType: gp3
        encrypted: true
        deleteOnTermination: true

  metadataOptions:
    httpEndpoint: enabled
    httpProtocolIPv6: disabled
    httpPutResponseHopLimit: 1
    httpTokens: required  # IMDSv2

  tags:
    runs-fleet/managed: "true"

---
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: runners-arm64-large-disk
spec:
  role: runs-fleet-node-role

  amiSelectorTerms:
    - alias: al2023@latest

  subnetSelectorTerms:
    - tags:
        runs-fleet/subnet: "true"

  securityGroupSelectorTerms:
    - tags:
        runs-fleet/security-group: "true"

  blockDeviceMappings:
    - deviceName: /dev/xvda
      ebs:
        volumeSize: 200Gi
        volumeType: gp3
        encrypted: true
        deleteOnTermination: true

  metadataOptions:
    httpEndpoint: enabled
    httpProtocolIPv6: disabled
    httpPutResponseHopLimit: 1
    httpTokens: required

  tags:
    runs-fleet/managed: "true"
```

## Runner Pod Specification

### Job Template

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: runner-${RUN_ID}-${JOB_ID}
  namespace: runs-fleet
  labels:
    runs-fleet/managed: "true"
    runs-fleet/run-id: "${RUN_ID}"
    runs-fleet/job-id: "${JOB_ID}"
    runs-fleet/runner-class: "${RUNNER_CLASS}"
    runs-fleet/repo: "${REPO_HASH}"
spec:
  ttlSecondsAfterFinished: 300
  backoffLimit: 0  # no retries, re-queue via orchestrator
  activeDeadlineSeconds: 21600  # 6 hour max
  template:
    metadata:
      labels:
        runs-fleet/managed: "true"
        runs-fleet/run-id: "${RUN_ID}"
        runs-fleet/runner-class: "${RUNNER_CLASS}"
    spec:
      restartPolicy: Never
      serviceAccountName: runs-fleet-runner

      nodeSelector:
        runs-fleet/runner-class: "${RUNNER_CLASS}"

      tolerations:
        - key: runs-fleet/runner
          operator: Equal
          value: "true"
          effect: NoSchedule

      containers:
        - name: runner
          image: ghcr.io/actions/actions-runner:latest
          resources:
            requests:
              cpu: "${CPU_REQUEST}"
              memory: "${MEMORY_REQUEST}"
            limits:
              cpu: "${CPU_LIMIT}"
              memory: "${MEMORY_LIMIT}"
          env:
            - name: RUNNER_NAME
              value: "runs-fleet-${RUN_ID}-${JOB_ID}"
            - name: RUNNER_WORKDIR
              value: "/runner/_work"
            - name: RUNNER_EPHEMERAL
              value: "true"
            - name: RUNNER_JITCONFIG
              valueFrom:
                secretKeyRef:
                  name: runner-jit-${RUN_ID}-${JOB_ID}
                  key: jit-config
          volumeMounts:
            - name: work
              mountPath: /runner/_work
            - name: dind-certs
              mountPath: /certs/client
              readOnly: true

        # Docker-in-Docker sidecar
        - name: dind
          image: docker:27-dind
          securityContext:
            privileged: true
          env:
            - name: DOCKER_TLS_CERTDIR
              value: /certs
          volumeMounts:
            - name: work
              mountPath: /runner/_work
            - name: dind-certs
              mountPath: /certs
            - name: dind-storage
              mountPath: /var/lib/docker

      volumes:
        - name: work
          emptyDir:
            sizeLimit: 40Gi
        - name: dind-certs
          emptyDir: {}
        - name: dind-storage
          emptyDir:
            sizeLimit: 50Gi
```

### Runner Class Mapping

```go
// pkg/k8s/jobs/specs.go

type RunnerClassSpec struct {
    NodeSelector map[string]string
    CPURequest   string
    CPULimit     string
    MemoryRequest string
    MemoryLimit   string
    NodeClass     string  // EC2NodeClass reference for disk size
}

var RunnerClasses = map[string]RunnerClassSpec{
    "2cpu-linux-arm64": {
        NodeSelector:  map[string]string{"runs-fleet/runner-class": "2cpu-linux-arm64"},
        CPURequest:    "1800m",
        CPULimit:      "2000m",
        MemoryRequest: "3500Mi",
        MemoryLimit:   "4000Mi",
        NodeClass:     "runners-arm64",
    },
    "4cpu-linux-arm64": {
        NodeSelector:  map[string]string{"runs-fleet/runner-class": "4cpu-linux-arm64"},
        CPURequest:    "3600m",
        CPULimit:      "4000m",
        MemoryRequest: "7Gi",
        MemoryLimit:   "8Gi",
        NodeClass:     "runners-arm64",
    },
    "8cpu-linux-arm64": {
        NodeSelector:  map[string]string{"runs-fleet/runner-class": "8cpu-linux-arm64"},
        CPURequest:    "7200m",
        CPULimit:      "8000m",
        MemoryRequest: "14Gi",
        MemoryLimit:   "16Gi",
        NodeClass:     "runners-arm64",
    },
    "4cpu-linux-amd64": {
        NodeSelector:  map[string]string{"runs-fleet/runner-class": "4cpu-linux-amd64"},
        CPURequest:    "3600m",
        CPULimit:      "4000m",
        MemoryRequest: "7Gi",
        MemoryLimit:   "8Gi",
        NodeClass:     "runners-amd64",
    },
    // Large disk variants
    "8cpu-linux-arm64/large-disk": {
        NodeSelector:  map[string]string{"runs-fleet/runner-class": "8cpu-linux-arm64-large-disk"},
        CPURequest:    "7200m",
        CPULimit:      "8000m",
        MemoryRequest: "14Gi",
        MemoryLimit:   "16Gi",
        NodeClass:     "runners-arm64-large-disk",
    },
}
```

## Warm Pool Strategy

### Option 1: Consolidation Delay (Simple)

Keep nodes running after pods complete:

```yaml
# In NodePool spec
disruption:
  consolidationPolicy: WhenEmpty
  consolidateAfter: 60m  # node stays warm for 60 minutes
```

**Pros:** Simple, no extra components
**Cons:** Pay for idle node time

### Option 2: Placeholder Pods (Cost-Optimized)

Use low-priority pods to hold capacity:

```yaml
# PriorityClass for placeholders
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: runs-fleet-placeholder
value: -100
preemptionPolicy: Never
globalDefault: false
description: "Placeholder pods for warm pool - preempted by real jobs"

---
# PriorityClass for real jobs
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: runs-fleet-runner
value: 100
preemptionPolicy: PreemptLowerPriority
globalDefault: false
description: "Runner pods - preempt placeholders"

---
# Placeholder Deployment
apiVersion: apps/v1
kind: Deployment
metadata:
  name: warm-pool-arm64-small
  namespace: runs-fleet
spec:
  replicas: 2  # maintain 2 warm nodes
  selector:
    matchLabels:
      runs-fleet/placeholder: "true"
      runs-fleet/runner-class: 2cpu-linux-arm64
  template:
    metadata:
      labels:
        runs-fleet/placeholder: "true"
        runs-fleet/runner-class: 2cpu-linux-arm64
    spec:
      priorityClassName: runs-fleet-placeholder
      terminationGracePeriodSeconds: 0

      nodeSelector:
        runs-fleet/runner-class: 2cpu-linux-arm64

      tolerations:
        - key: runs-fleet/runner
          operator: Equal
          value: "true"
          effect: NoSchedule

      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          resources:
            requests:
              cpu: "1800m"
              memory: "3500Mi"
```

**Flow:**
1. Placeholder pods keep nodes warm (minimal resource usage)
2. Real job comes in with higher priority
3. K8s preempts placeholder, schedules real job (~3-5s)
4. Real job completes
5. Placeholder reschedules onto same node
6. Node stays warm for next job

**Pros:** Only pay for placeholder pod overhead (negligible)
**Cons:** Slightly more complex, placeholder rescheduling adds ~5s

### Warm Pool Controller

```go
// pkg/k8s/warmpool/controller.go

type WarmPoolConfig struct {
    RunnerClass string
    MinReplicas int
    MaxReplicas int
    ScaleUpThreshold   int  // pending jobs to trigger scale up
    ScaleDownDelay     time.Duration
}

type Controller struct {
    kubeClient    kubernetes.Interface
    poolConfigs   map[string]WarmPoolConfig
    metricsClient metrics.Client
}

// Reconcile adjusts placeholder replicas based on demand patterns
func (c *Controller) Reconcile(ctx context.Context) error {
    for class, config := range c.poolConfigs {
        // Get recent job arrival rate
        rate := c.metricsClient.GetJobArrivalRate(class, 15*time.Minute)

        // Calculate desired placeholders
        desired := c.calculateDesiredReplicas(rate, config)

        // Update deployment replicas
        if err := c.updatePlaceholderReplicas(ctx, class, desired); err != nil {
            return err
        }
    }
    return nil
}
```

## Job Creator Implementation

```go
// pkg/k8s/jobs/creator.go

package jobs

import (
    "context"
    "fmt"

    batchv1 "k8s.io/api/batch/v1"
    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
)

type Creator struct {
    kubeClient kubernetes.Interface
    namespace  string
    runnerImage string
}

type JobSpec struct {
    RunID       string
    JobID       string
    RunnerClass string
    JITConfig   string
    Repository  string
    Labels      map[string]string
}

func (c *Creator) CreateRunnerJob(ctx context.Context, spec *JobSpec) (*batchv1.Job, error) {
    classSpec, ok := RunnerClasses[spec.RunnerClass]
    if !ok {
        return nil, fmt.Errorf("unknown runner class: %s", spec.RunnerClass)
    }

    jobName := fmt.Sprintf("runner-%s-%s", spec.RunID, spec.JobID)
    secretName := fmt.Sprintf("runner-jit-%s-%s", spec.RunID, spec.JobID)

    // Create JIT config secret
    secret := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name:      secretName,
            Namespace: c.namespace,
            Labels: map[string]string{
                "runs-fleet/managed": "true",
                "runs-fleet/run-id":  spec.RunID,
            },
        },
        Data: map[string][]byte{
            "jit-config": []byte(spec.JITConfig),
        },
    }

    if _, err := c.kubeClient.CoreV1().Secrets(c.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
        return nil, fmt.Errorf("failed to create JIT secret: %w", err)
    }

    // Create Job
    job := &batchv1.Job{
        ObjectMeta: metav1.ObjectMeta{
            Name:      jobName,
            Namespace: c.namespace,
            Labels: map[string]string{
                "runs-fleet/managed":      "true",
                "runs-fleet/run-id":       spec.RunID,
                "runs-fleet/job-id":       spec.JobID,
                "runs-fleet/runner-class": spec.RunnerClass,
            },
        },
        Spec: batchv1.JobSpec{
            TTLSecondsAfterFinished: ptr(int32(300)),
            BackoffLimit:            ptr(int32(0)),
            ActiveDeadlineSeconds:   ptr(int64(21600)),
            Template: corev1.PodTemplateSpec{
                ObjectMeta: metav1.ObjectMeta{
                    Labels: map[string]string{
                        "runs-fleet/managed":      "true",
                        "runs-fleet/run-id":       spec.RunID,
                        "runs-fleet/runner-class": spec.RunnerClass,
                    },
                },
                Spec: corev1.PodSpec{
                    RestartPolicy:      corev1.RestartPolicyNever,
                    ServiceAccountName: "runs-fleet-runner",
                    PriorityClassName:  "runs-fleet-runner",
                    NodeSelector:       classSpec.NodeSelector,
                    Tolerations: []corev1.Toleration{
                        {
                            Key:      "runs-fleet/runner",
                            Operator: corev1.TolerationOpEqual,
                            Value:    "true",
                            Effect:   corev1.TaintEffectNoSchedule,
                        },
                    },
                    Containers: []corev1.Container{
                        {
                            Name:  "runner",
                            Image: c.runnerImage,
                            Resources: corev1.ResourceRequirements{
                                Requests: corev1.ResourceList{
                                    corev1.ResourceCPU:    resource.MustParse(classSpec.CPURequest),
                                    corev1.ResourceMemory: resource.MustParse(classSpec.MemoryRequest),
                                },
                                Limits: corev1.ResourceList{
                                    corev1.ResourceCPU:    resource.MustParse(classSpec.CPULimit),
                                    corev1.ResourceMemory: resource.MustParse(classSpec.MemoryLimit),
                                },
                            },
                            Env: []corev1.EnvVar{
                                {Name: "RUNNER_NAME", Value: jobName},
                                {Name: "RUNNER_EPHEMERAL", Value: "true"},
                                {
                                    Name: "RUNNER_JITCONFIG",
                                    ValueFrom: &corev1.EnvVarSource{
                                        SecretKeyRef: &corev1.SecretKeySelector{
                                            LocalObjectReference: corev1.LocalObjectReference{
                                                Name: secretName,
                                            },
                                            Key: "jit-config",
                                        },
                                    },
                                },
                            },
                        },
                    },
                },
            },
        },
    }

    return c.kubeClient.BatchV1().Jobs(c.namespace).Create(ctx, job, metav1.CreateOptions{})
}

func ptr[T any](v T) *T {
    return &v
}
```

## Migration Path

### Phase 1: Parallel Deployment

Run K8s version alongside EC2 version:

1. Deploy orchestrator to EKS cluster
2. Configure separate webhook endpoint
3. Route test repositories to K8s orchestrator
4. Compare metrics (latency, success rate, cost)

### Phase 2: Gradual Migration

1. Add routing logic based on repository/org
2. Migrate low-risk repositories first
3. Monitor and tune Karpenter NodePools
4. Adjust warm pool sizing based on traffic patterns

### Phase 3: Full Cutover

1. Route all traffic to K8s orchestrator
2. Decommission EC2 orchestrator
3. Remove EC2-specific code paths
4. Archive legacy pkg/fleet, pkg/pools, pkg/termination

## What Gets Removed

| Package | Reason |
|---------|--------|
| `pkg/fleet/` | Replaced by K8s Job creation |
| `pkg/pools/` | Replaced by Karpenter + placeholder pods |
| `pkg/termination/` | Karpenter handles node lifecycle |
| `pkg/events/` | Karpenter handles spot interruptions |
| `pkg/coordinator/` | Use K8s Lease for leader election |
| `pkg/runner/manager.go` (SSM parts) | Config via K8s Secrets |

## What Stays (with modifications)

| Package | Changes |
|---------|---------|
| `pkg/github/` | Unchanged - webhook parsing, JIT tokens |
| `pkg/queue/` | Unchanged - SQS processing |
| `pkg/cache/` | Unchanged - S3 cache protocol |
| `pkg/config/` | Add K8s client initialization |
| `pkg/metrics/` | Add K8s-native metrics (or keep CloudWatch) |
| `pkg/db/` | May remove if using K8s Job status tracking |
| `pkg/housekeeping/` | Adapt for K8s resource cleanup |

## Cost Comparison

### Current EC2 Architecture (100 jobs/day @ 10 min avg)

| Component | Monthly Cost |
|-----------|--------------|
| EC2 spot instances | $15-20 |
| Fargate orchestrator | $36 |
| Warm pool (stopped EBS) | $5 |
| DynamoDB | $1 |
| S3 cache | $2-5 |
| **Total** | **~$60-65** |

### Kubernetes Architecture (same workload)

| Component | Monthly Cost |
|-----------|--------------|
| EKS control plane | $73 |
| EC2 spot nodes (via Karpenter) | $15-20 |
| Orchestrator pod | $0 (runs on system nodes) |
| Warm nodes (idle time) | $10-20 |
| S3 cache | $2-5 |
| **Total** | **~$100-120** |

### Break-even Analysis

K8s version becomes cost-effective when:
- Already running EKS for other workloads (control plane amortized)
- Job volume >500/day (faster warm path reduces total compute time)
- Team velocity gains from K8s tooling justify premium

## Performance Comparison

| Metric | EC2 (current) | Kubernetes |
|--------|---------------|------------|
| Cold start (no warm capacity) | ~2 min | ~2 min |
| Warm start | ~30s | ~3-5s |
| Spot interruption recovery | ~3 min (re-queue + new instance) | ~90s (Karpenter replacement) |
| Job pickup latency | ~500ms | ~500ms |
| Max concurrent jobs | Limited by EC2 API rate | Limited by NodePool CPU limits |

## Security Considerations

### Pod Security

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: runs-fleet-runner
  namespace: runs-fleet
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT:role/runs-fleet-runner-role

---
apiVersion: policy/v1
kind: PodSecurityPolicy  # or Pod Security Standards
metadata:
  name: runs-fleet-runner
spec:
  privileged: false
  runAsUser:
    rule: MustRunAsNonRoot
  volumes:
    - emptyDir
    - secret
    - configMap
  hostNetwork: false
  hostIPC: false
  hostPID: false
```

### Network Isolation

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: runner-egress
  namespace: runs-fleet
spec:
  podSelector:
    matchLabels:
      runs-fleet/managed: "true"
  policyTypes:
    - Egress
  egress:
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
      ports:
        - protocol: TCP
          port: 443
```

## Observability

### Metrics

```yaml
# ServiceMonitor for Prometheus
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: runs-fleet-orchestrator
spec:
  selector:
    matchLabels:
      app: runs-fleet-orchestrator
  endpoints:
    - port: metrics
      interval: 15s
```

Key metrics to track:
- `runs_fleet_job_duration_seconds` - Job execution time
- `runs_fleet_pod_scheduling_seconds` - Time from Job creation to Pod running
- `runs_fleet_node_provisioning_seconds` - Karpenter node creation time
- `runs_fleet_warm_pool_hits_total` - Jobs scheduled on existing nodes
- `runs_fleet_warm_pool_misses_total` - Jobs requiring new node

### Logging

```go
// Structured logging with K8s context
logger.Info("job created",
    "run_id", spec.RunID,
    "job_id", spec.JobID,
    "runner_class", spec.RunnerClass,
    "namespace", c.namespace,
)
```

## Open Questions

1. ~~**Docker-in-Docker vs Sysbox**: Should runner pods use DinD, sysbox, or host Docker socket?~~ **Resolved: DinD**
2. ~~**Multi-cluster**: Single EKS cluster or regional clusters with federation?~~ **Resolved: Single cluster**
3. **Cache locality**: How to ensure S3 cache proximity to runner nodes?
4. ~~**Windows support**: Karpenter Windows support is limited - keep EC2 path for Windows?~~ **Resolved: No Windows support**

## Decision Log

| Decision | Rationale | Date |
|----------|-----------|------|
| Use Karpenter over Cluster Autoscaler | Faster provisioning, better spot handling | 2025-11-29 |
| Placeholder pods over consolidation delay | Lower idle costs | 2025-11-29 |
| Keep SQS over K8s native queuing | Proven reliability, existing integration | 2025-11-29 |
| Single namespace isolation | Simpler RBAC, resource quotas | 2025-11-29 |
| Docker-in-Docker for container builds | Avoids host socket security concerns, portable across nodes | 2025-11-29 |
| Single EKS cluster | Simpler operations, sufficient for current scale, avoids federation complexity | 2025-11-29 |
| No Windows runner support | Focus on Linux workloads, Karpenter Windows support immature | 2025-11-29 |
