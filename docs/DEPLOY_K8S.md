# Kubernetes Deployment Guide

Deploy runs-fleet on Kubernetes with Karpenter for node provisioning.

## Prerequisites

- Kubernetes 1.28+
- Helm 3.x
- Karpenter 1.0+ installed on EKS
- GitHub App configured

## 1. Create GitHub App

1. Go to GitHub Settings → Developer settings → GitHub Apps → New GitHub App
2. Configure:
   - **Webhook URL**: `https://<your-domain>/webhook`
   - **Webhook secret**: Generate with `openssl rand -hex 32`
   - **Permissions**:
     - Repository: Actions (read), Administration (read/write), Metadata (read)
     - Organization: Self-hosted runners (read/write)
   - **Events**: Workflow job
3. Generate and download private key
4. Install app on target repositories/organization

## 2. Create values.yaml

```yaml
github:
  appId: "123456"
  privateKey: |
    -----BEGIN RSA PRIVATE KEY-----
    <your-private-key>
    -----END RSA PRIVATE KEY-----
  webhookSecret: "<your-webhook-secret>"

orchestrator:
  image:
    repository: <your-ecr>/runs-fleet
    tag: latest

runner:
  image:
    repository: <your-ecr>/runs-fleet-runner
    tag: latest

ingress:
  enabled: true
  className: nginx
  host: runs-fleet.example.com
  tls:
    enabled: true
    secretName: runs-fleet-tls

karpenter:
  enabled: true
  ec2NodeClass:
    instanceProfile: KarpenterNodeInstanceProfile-<cluster-name>
    subnetSelectorTerms:
      - tags:
          karpenter.sh/discovery: <cluster-name>
    securityGroupSelectorTerms:
      - tags:
          karpenter.sh/discovery: <cluster-name>
```

## 3. Deploy

```bash
# Add namespace
kubectl create namespace runs-fleet

# Install chart
helm install runs-fleet ./deploy/helm/runs-fleet \
  -n runs-fleet \
  -f values.yaml

# Verify
kubectl get pods -n runs-fleet
kubectl get nodepool
```

## 4. Configure DNS

Point your domain to the ingress:

```bash
kubectl get ingress -n runs-fleet
# Note the ADDRESS, create DNS A/CNAME record
```

## 5. Test

Add workflow to a repository:

```yaml
name: Test runs-fleet
on: push
jobs:
  test:
    runs-on: "runs-fleet=${{ github.run_id }}/cpu=2/arch=arm64"
    steps:
      - run: echo "Hello from runs-fleet"
```

## Warm Pools (Optional)

Enable placeholder pods for faster job pickup (~5s vs ~90s):

```yaml
warmPool:
  enabled: true
  placeholders:
    arm64:
      replicas: 2
```

## Troubleshooting

```bash
# Check orchestrator logs
kubectl logs -n runs-fleet -l app.kubernetes.io/component=orchestrator -f

# Check Valkey
kubectl exec -n runs-fleet -it valkey-0 -- valkey-cli XLEN runs-fleet:jobs

# Check Karpenter provisioning
kubectl logs -n karpenter -l app.kubernetes.io/name=karpenter -f

# List runner pods
kubectl get pods -n runs-fleet -l runs-fleet.io/managed=true
```
