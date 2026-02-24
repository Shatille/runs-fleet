# Configuration

All configuration is via environment variables. See `.envrc.example` for a template.

## Core (Required)

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_GITHUB_APP_ID` | GitHub App ID |
| `RUNS_FLEET_GITHUB_APP_PRIVATE_KEY` | GitHub App private key (PEM format) |
| `RUNS_FLEET_GITHUB_WEBHOOK_SECRET` | Webhook HMAC secret |
| `AWS_REGION` | AWS region (default: `ap-northeast-1`) |
| `RUNS_FLEET_MODE` | Backend mode: `ec2` or `k8s` (default: `ec2`) |

## Queues (SQS - EC2 mode)

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_QUEUE_URL` | Main job queue URL (required for EC2) |
| `RUNS_FLEET_QUEUE_DLQ_URL` | Dead letter queue URL |
| `RUNS_FLEET_POOL_QUEUE_URL` | Warm pool batch queue URL |
| `RUNS_FLEET_EVENTS_QUEUE_URL` | EventBridge events queue (spot interruptions) |
| `RUNS_FLEET_TERMINATION_QUEUE_URL` | Instance termination notifications |
| `RUNS_FLEET_HOUSEKEEPING_QUEUE_URL` | Cleanup task scheduling |

## DynamoDB

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_JOBS_TABLE` | | Job state tracking |
| `RUNS_FLEET_POOLS_TABLE` | | Pool configurations (includes per-pool reconciliation locks) |
| `RUNS_FLEET_CIRCUIT_BREAKER_TABLE` | `runs-fleet-circuit-state` | Circuit breaker state |

## S3 & SNS

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_CACHE_BUCKET` | GitHub Actions cache artifacts |
| `RUNS_FLEET_COST_REPORT_BUCKET` | Cost report storage |
| `RUNS_FLEET_COST_REPORT_SNS_TOPIC` | Cost report notifications |

## EC2 Backend

Required when `RUNS_FLEET_MODE=ec2`.

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_VPC_ID` | | VPC ID (required) |
| `RUNS_FLEET_PUBLIC_SUBNET_IDS` | | Comma-separated subnet IDs (required) |
| `RUNS_FLEET_PRIVATE_SUBNET_IDS` | | Private subnets (optional) |
| `RUNS_FLEET_SECURITY_GROUP_ID` | | Security group ID (required) |
| `RUNS_FLEET_INSTANCE_PROFILE_ARN` | | IAM instance profile ARN (required) |
| `RUNS_FLEET_RUNNER_IMAGE` | | ECR image URL for runners (required) |
| `RUNS_FLEET_KEY_NAME` | | EC2 key pair name (optional) |
| `RUNS_FLEET_SPOT_ENABLED` | `true` | Enable spot instances (cold-start only; warm pool always uses on-demand) |
| `RUNS_FLEET_MAX_RUNTIME_MINUTES` | `360` | Max job runtime (1-1440) |
| `RUNS_FLEET_LAUNCH_TEMPLATE_NAME` | `runs-fleet-runner` | EC2 launch template |
| `RUNS_FLEET_TAGS` | | Custom EC2 tags (JSON object) |

## Kubernetes Backend

Required when `RUNS_FLEET_MODE=k8s`.

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_KUBE_NAMESPACE` | | Runner namespace (required) |
| `RUNS_FLEET_KUBE_RUNNER_IMAGE` | | Runner container image (required) |
| `RUNS_FLEET_KUBE_CONFIG` | | Kubeconfig path (empty = in-cluster) |
| `RUNS_FLEET_KUBE_SERVICE_ACCOUNT` | `runs-fleet-runner` | Runner ServiceAccount |
| `RUNS_FLEET_KUBE_NODE_SELECTOR` | | Node selector (`key=value,...`) |
| `RUNS_FLEET_KUBE_TOLERATIONS` | | Tolerations (JSON array) |
| `RUNS_FLEET_KUBE_IDLE_TIMEOUT_MINUTES` | `10` | Pod idle timeout |
| `RUNS_FLEET_KUBE_RELEASE_NAME` | `runs-fleet` | Helm release name |
| `RUNS_FLEET_KUBE_STORAGE_CLASS` | | PVC storage class |
| `RUNS_FLEET_KUBE_RESOURCE_LABELS` | | Additional pod labels (`key=value,...`) |

### Kubernetes DinD (Docker-in-Docker)

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_KUBE_DIND_IMAGE` | `docker:dind` | DinD sidecar image |
| `RUNS_FLEET_KUBE_DAEMON_JSON_CONFIGMAP` | | ConfigMap for daemon.json |
| `RUNS_FLEET_KUBE_DOCKER_WAIT_SECONDS` | `120` | Docker daemon startup wait (10-300) |
| `RUNS_FLEET_KUBE_DOCKER_GROUP_GID` | `123` | Docker socket GID (1-65535) |
| `RUNS_FLEET_KUBE_REGISTRY_MIRROR` | | Registry mirror URL |

### Valkey Queue (K8s mode)

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_VALKEY_ADDR` | `valkey:6379` | Valkey/Redis address (required for K8s) |
| `RUNS_FLEET_VALKEY_PASSWORD` | | Valkey password |
| `RUNS_FLEET_VALKEY_DB` | `0` | Database number |

## Cache

| Variable | Description |
|----------|-------------|
| `RUNS_FLEET_CACHE_SECRET` | HMAC secret for cache auth |
| `RUNS_FLEET_CACHE_URL` | Cache service URL (passed to runners) |

## Metrics

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_METRICS_NAMESPACE` | `RunsFleet` | Metric namespace/prefix |
| `RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED` | `true` | Enable CloudWatch metrics |
| `RUNS_FLEET_METRICS_PROMETHEUS_ENABLED` | `false` | Enable Prometheus `/metrics` endpoint |
| `RUNS_FLEET_METRICS_PROMETHEUS_PATH` | `/metrics` | Prometheus endpoint path |
| `RUNS_FLEET_METRICS_DATADOG_ENABLED` | `false` | Enable Datadog DogStatsD |
| `RUNS_FLEET_METRICS_DATADOG_ADDR` | `127.0.0.1:8125` | DogStatsD address |
| `RUNS_FLEET_METRICS_DATADOG_TAGS` | | Global tags (comma-separated) |
| `RUNS_FLEET_METRICS_DATADOG_SAMPLE_RATE` | `1.0` | DogStatsD sample rate (0.0-1.0) |
| `RUNS_FLEET_METRICS_DATADOG_BUFFER_POOL_SIZE` | `0` | DogStatsD buffer pool size |
| `RUNS_FLEET_METRICS_DATADOG_WORKERS_COUNT` | `0` | DogStatsD worker count |
| `RUNS_FLEET_METRICS_DATADOG_MAX_MSGS_PER_PAYLOAD` | `0` | DogStatsD max messages per payload |

## Secrets Backend

Runner configuration secrets can be stored in SSM Parameter Store (default) or HashiCorp Vault.

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_SECRETS_BACKEND` | `ssm` | Backend: `ssm` or `vault` |
| `RUNS_FLEET_SECRETS_PATH_PREFIX` | `/runs-fleet/runners` | SSM parameter path prefix |

### Vault Configuration

Required when `RUNS_FLEET_SECRETS_BACKEND=vault`.

| Variable | Default | Description |
|----------|---------|-------------|
| `VAULT_ADDR` | | Vault server address (required) |
| `VAULT_NAMESPACE` | | Vault namespace (enterprise) |
| `VAULT_KV_MOUNT` | `secret` | KV secrets engine mount path |
| `VAULT_KV_VERSION` | auto-detect | KV version (1 or 2) |
| `VAULT_BASE_PATH` | `runs-fleet/runners` | Base path for secrets |
| `VAULT_AUTH_METHOD` | `aws` | Auth: `aws`, `kubernetes`, `approle`, `token` |

### Vault Auth Methods

| Auth | Variables |
|------|-----------|
| `aws` | `VAULT_AWS_ROLE` (default: `runs-fleet`), `VAULT_AWS_REGION` |
| `kubernetes` | `VAULT_K8S_ROLE` (required), `VAULT_K8S_JWT_PATH`, `VAULT_K8S_AUTH_MOUNT` (default: `kubernetes`) |
| `approle` | `VAULT_APP_ROLE_ID`, `VAULT_APP_SECRET_ID` (both required) |
| `token` | `VAULT_TOKEN` (required) |

## Logging & Admin

| Variable | Default | Description |
|----------|---------|-------------|
| `RUNS_FLEET_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `RUNS_FLEET_LOG_FORMAT` | `json` | Log format: `json` or `text` |
| `RUNS_FLEET_ADMIN_SECRET` | | Admin UI authentication secret |
