# Hybrid Mode: K8s Orchestrator + EC2 Runners

## Overview

Enable deploying the runs-fleet orchestrator on Kubernetes while using EC2 Fleet for runner provisioning. This combines K8s-native deployment benefits (GitOps, secrets management, observability) with EC2's fast boot times and native Docker performance.

## Motivation

The current K8s mode uses Karpenter + runner pods, which has drawbacks:
- **Slow cold start**: Karpenter node provisioning + kubelet bootstrap adds 30-60s
- **DinD overhead**: Nested Docker is ~30% slower than native
- **ARC parity**: No differentiation from GitHub's official solution
- **Spot inefficiency**: Karpenter's spot handling is less precise than EC2 Fleet API

EC2 mode already solves these but requires ECS Fargate for orchestrator hosting.

## Target Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     Kubernetes Cluster                       │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  runs-fleet namespace                                │    │
│  │  ┌──────────────┐  ┌──────────────┐  ┌───────────┐  │    │
│  │  │ Orchestrator │  │ Orchestrator │  │  Valkey   │  │    │
│  │  │   Pod (1)    │  │   Pod (2)    │  │ (optional)│  │    │
│  │  └──────┬───────┘  └──────┬───────┘  └───────────┘  │    │
│  │         │                 │                          │    │
│  │         └────────┬────────┘                          │    │
│  └──────────────────┼───────────────────────────────────┘    │
└─────────────────────┼────────────────────────────────────────┘
                      │
                      │ AWS APIs
                      ▼
┌─────────────────────────────────────────────────────────────┐
│                        AWS                                   │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────────────┐ │
│  │   SQS   │  │ DynamoDB│  │   SSM   │  │   EC2 Fleet     │ │
│  │  FIFO   │  │ (state) │  │(config) │  │   (runners)     │ │
│  └─────────┘  └─────────┘  └─────────┘  └─────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

## Implementation Plan

### Phase 1: Helm Chart Refactor

**Goal**: Support `mode: ec2` in Helm chart to deploy orchestrator for EC2 backend.

#### 1.1 Update values.yaml

```yaml
# Mode selection
mode: ec2  # ec2 | k8s

# AWS configuration (required for ec2 mode)
aws:
  region: ap-northeast-1

  # VPC configuration
  vpcId: ""
  publicSubnetIds: []
  privateSubnetIds: []
  securityGroupId: ""
  instanceProfileArn: ""

  # SQS queues
  queueUrl: ""
  queueDlqUrl: ""
  poolQueueUrl: ""
  eventsQueueUrl: ""
  terminationQueueUrl: ""
  housekeepingQueueUrl: ""

  # DynamoDB tables
  locksTable: "runs-fleet-locks"
  jobsTable: "runs-fleet-jobs"
  poolsTable: "runs-fleet-pools"
  circuitBreakerTable: "runs-fleet-circuit-state"

  # S3 buckets
  cacheBucket: ""
  configBucket: ""

  # EC2 runner configuration
  runnerImage: ""  # ECR URL
  launchTemplateName: "runs-fleet-runner"
  spotEnabled: true

# Orchestrator configuration (unchanged)
orchestrator:
  image:
    repository: runs-fleet
    tag: latest
  replicas: 2
  # ...

# GitHub configuration (unchanged)
github:
  appId: ""
  privateKey: ""
  webhookSecret: ""
  existingSecret: ""

# Valkey - optional for ec2 mode, required for k8s mode
valkey:
  enabled: false  # Set to false for ec2 mode
```

#### 1.2 Update deployment.yaml

Add conditional environment variables based on mode:

```yaml
env:
  - name: RUNS_FLEET_MODE
    value: {{ .Values.mode | quote }}

  {{- if eq .Values.mode "ec2" }}
  # EC2 mode configuration
  - name: AWS_REGION
    value: {{ .Values.aws.region | quote }}
  - name: RUNS_FLEET_VPC_ID
    value: {{ .Values.aws.vpcId | quote }}
  - name: RUNS_FLEET_PUBLIC_SUBNET_IDS
    value: {{ .Values.aws.publicSubnetIds | join "," | quote }}
  - name: RUNS_FLEET_PRIVATE_SUBNET_IDS
    value: {{ .Values.aws.privateSubnetIds | join "," | quote }}
  - name: RUNS_FLEET_SECURITY_GROUP_ID
    value: {{ .Values.aws.securityGroupId | quote }}
  - name: RUNS_FLEET_INSTANCE_PROFILE_ARN
    value: {{ .Values.aws.instanceProfileArn | quote }}
  - name: RUNS_FLEET_QUEUE_URL
    value: {{ .Values.aws.queueUrl | quote }}
  # ... (all SQS, DynamoDB, S3 env vars)
  - name: RUNS_FLEET_RUNNER_IMAGE
    value: {{ .Values.aws.runnerImage | quote }}
  - name: RUNS_FLEET_SPOT_ENABLED
    value: {{ .Values.aws.spotEnabled | quote }}
  {{- end }}

  {{- if eq .Values.mode "k8s" }}
  # K8s mode configuration (existing)
  - name: RUNS_FLEET_KUBE_NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
  # ... (existing k8s config)
  {{- end }}
```

#### 1.3 Conditional Templates

Gate K8s-runner-specific templates:

```yaml
# karpenter.yaml
{{- if and .Values.karpenter.enabled (eq .Values.mode "k8s") }}
# ... Karpenter resources
{{- end }}

# warmpool.yaml
{{- if and .Values.warmPool.enabled (eq .Values.mode "k8s") }}
# ... Placeholder pods
{{- end }}
```

#### 1.4 RBAC Updates

EC2 mode needs minimal RBAC (no pod creation):

```yaml
# rbac.yaml
{{- if eq .Values.mode "ec2" }}
# Minimal RBAC - only needs secrets access if using K8s secrets
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]
{{- else }}
# Full K8s mode RBAC - pod creation, configmaps, etc.
rules:
  - apiGroups: [""]
    resources: ["pods", "secrets", "configmaps"]
    verbs: ["create", "get", "list", "watch", "delete"]
  # ...
{{- end }}
```

### Phase 2: IAM for K8s Service Account

**Goal**: Enable orchestrator pods to call AWS APIs via IRSA (IAM Roles for Service Accounts).

#### 2.1 Service Account Annotation

```yaml
# values.yaml
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: "arn:aws:iam::123456789012:role/runs-fleet-orchestrator"
```

#### 2.2 IAM Policy

Same policy as ECS task role:
- EC2: CreateFleet, TerminateInstances, DescribeInstances, etc.
- SQS: SendMessage, ReceiveMessage, DeleteMessage
- DynamoDB: GetItem, PutItem, DeleteItem, Query
- SSM: PutParameter, DeleteParameter
- S3: GetObject, PutObject (for cache/config buckets)

### Phase 3: Documentation

- Update `docs/DEPLOY_K8S.md` with hybrid mode instructions
- Add example values files for common scenarios
- Document IAM role setup for EKS IRSA

## Files Changed

| File | Change |
|------|--------|
| `deploy/helm/runs-fleet/values.yaml` | Add `mode`, `aws` sections |
| `deploy/helm/runs-fleet/templates/deployment.yaml` | Conditional env vars |
| `deploy/helm/runs-fleet/templates/rbac.yaml` | Mode-specific RBAC |
| `deploy/helm/runs-fleet/templates/karpenter.yaml` | Gate behind `mode: k8s` |
| `deploy/helm/runs-fleet/templates/warmpool.yaml` | Gate behind `mode: k8s` |
| `deploy/helm/runs-fleet/templates/_helpers.tpl` | Add mode helper functions |
| `docs/DEPLOY_K8S.md` | Update with hybrid mode docs |

## No Code Changes Required

The `cmd/server` binary already supports both modes via `RUNS_FLEET_MODE` environment variable. This is purely a deployment/configuration change.

## Migration Path

For existing K8s mode users who want to switch to hybrid:

1. Deploy EC2 infrastructure (Terraform)
2. Update Helm values: `mode: ec2`, add `aws` config
3. `helm upgrade` - orchestrator restarts with EC2 backend
4. Existing K8s runner pods finish jobs, no new pods created
5. Remove Karpenter NodePool/EC2NodeClass if desired

## Testing

1. Deploy to test EKS cluster with `mode: ec2`
2. Verify orchestrator connects to SQS, DynamoDB
3. Trigger workflow, verify EC2 instance launches
4. Verify runner completes job and terminates
5. Test multi-replica leader election

## Risks

| Risk | Mitigation |
|------|------------|
| IRSA misconfiguration | Provide Terraform module for IAM role |
| Mixed mode confusion | Clear documentation, validation in Helm |
| Valkey still deployed | Make valkey.enabled default to false for ec2 mode |
