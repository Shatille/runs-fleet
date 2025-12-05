# Vault Support for Runner Configuration

## Overview

Add HashiCorp Vault as an alternative to AWS SSM Parameter Store for passing runner configuration (including JIT tokens) from the orchestrator to ephemeral runners.

## Motivation

- **Multi-cloud**: Organizations using GCP/Azure or hybrid clouds may not have SSM
- **Existing infrastructure**: Many enterprises already run Vault for secrets management
- **Compliance**: Some environments require all secrets flow through Vault
- **Reduced AWS coupling**: SSM is the only AWS service runners directly depend on

## Current Architecture (SSM)

```
┌──────────────┐     PutParameter      ┌─────────────┐
│ Orchestrator │ ────────────────────► │     SSM     │
└──────────────┘                       │  Parameter  │
                                       │    Store    │
┌──────────────┐     GetParameter      │             │
│   Runner     │ ◄──────────────────── │             │
│   (EC2)      │                       └─────────────┘
└──────────────┘
```

**Data stored**: JSON blob containing:
```json
{
  "jit_token": "AXXXXXXX...",
  "repository": "owner/repo",
  "labels": ["runs-fleet=123", "runner=2cpu-linux-arm64"],
  "runner_group": "default"
}
```

## Proposed Architecture (Vault)

```
┌──────────────┐     Write KV          ┌─────────────┐
│ Orchestrator │ ────────────────────► │    Vault    │
└──────────────┘    (AppRole auth)     │   KV v2     │
                                       │             │
┌──────────────┐     Read KV           │             │
│   Runner     │ ◄──────────────────── │             │
│   (EC2)      │   (AWS IAM auth)      └─────────────┘
└──────────────┘
```

## Implementation Plan

### Phase 1: Secret Store Abstraction

**Goal**: Create an interface for secret storage that both SSM and Vault implement.

#### 1.1 Define Interface

```go
// pkg/secrets/store.go

package secrets

import "context"

// RunnerConfig contains configuration passed to runners.
type RunnerConfig struct {
    JITToken    string   `json:"jit_token"`
    Repository  string   `json:"repository"`
    Labels      []string `json:"labels"`
    RunnerGroup string   `json:"runner_group,omitempty"`
}

// Store defines operations for storing and retrieving runner configuration.
type Store interface {
    // Put stores runner configuration and returns a reference path/key.
    Put(ctx context.Context, runnerID string, config *RunnerConfig) (string, error)

    // Get retrieves runner configuration by reference path/key.
    Get(ctx context.Context, ref string) (*RunnerConfig, error)

    // Delete removes runner configuration.
    Delete(ctx context.Context, ref string) error
}
```

#### 1.2 SSM Implementation

```go
// pkg/secrets/ssm.go

package secrets

import (
    "context"
    "encoding/json"
    "fmt"

    "github.com/aws/aws-sdk-go-v2/service/ssm"
)

type SSMStore struct {
    client *ssm.Client
    prefix string // e.g., "/runs-fleet/runners"
}

func NewSSMStore(client *ssm.Client, prefix string) *SSMStore {
    return &SSMStore{client: client, prefix: prefix}
}

func (s *SSMStore) Put(ctx context.Context, runnerID string, config *RunnerConfig) (string, error) {
    path := fmt.Sprintf("%s/%s", s.prefix, runnerID)
    data, err := json.Marshal(config)
    if err != nil {
        return "", fmt.Errorf("failed to marshal config: %w", err)
    }

    _, err = s.client.PutParameter(ctx, &ssm.PutParameterInput{
        Name:      &path,
        Value:     aws.String(string(data)),
        Type:      types.ParameterTypeSecureString,
        Overwrite: aws.Bool(true),
    })
    if err != nil {
        return "", fmt.Errorf("failed to put parameter: %w", err)
    }

    return path, nil
}

func (s *SSMStore) Get(ctx context.Context, ref string) (*RunnerConfig, error) {
    // ... GetParameter implementation
}

func (s *SSMStore) Delete(ctx context.Context, ref string) error {
    // ... DeleteParameter implementation
}
```

#### 1.3 Vault Implementation

```go
// pkg/secrets/vault.go

package secrets

import (
    "context"
    "fmt"

    vault "github.com/hashicorp/vault/api"
)

type VaultStore struct {
    client     *vault.Client
    mountPath  string // e.g., "secret"
    secretPath string // e.g., "runs-fleet/runners"
}

type VaultConfig struct {
    Address    string
    MountPath  string
    SecretPath string

    // Auth configuration
    AuthMethod string // "kubernetes", "approle", "aws"

    // Kubernetes auth
    K8sRole    string
    K8sJWTPath string // default: /var/run/secrets/kubernetes.io/serviceaccount/token

    // AppRole auth
    RoleID     string
    SecretID   string

    // AWS IAM auth
    AWSRole    string
    AWSRegion  string
}

func NewVaultStore(cfg *VaultConfig) (*VaultStore, error) {
    client, err := vault.NewClient(&vault.Config{
        Address: cfg.Address,
    })
    if err != nil {
        return nil, fmt.Errorf("failed to create vault client: %w", err)
    }

    // Authenticate based on method
    switch cfg.AuthMethod {
    case "kubernetes":
        if err := authenticateK8s(client, cfg); err != nil {
            return nil, err
        }
    case "approle":
        if err := authenticateAppRole(client, cfg); err != nil {
            return nil, err
        }
    case "aws":
        if err := authenticateAWS(client, cfg); err != nil {
            return nil, err
        }
    default:
        return nil, fmt.Errorf("unsupported auth method: %s", cfg.AuthMethod)
    }

    return &VaultStore{
        client:     client,
        mountPath:  cfg.MountPath,
        secretPath: cfg.SecretPath,
    }, nil
}

func (v *VaultStore) Put(ctx context.Context, runnerID string, config *RunnerConfig) (string, error) {
    path := fmt.Sprintf("%s/%s", v.secretPath, runnerID)

    data := map[string]interface{}{
        "jit_token":    config.JITToken,
        "repository":   config.Repository,
        "labels":       config.Labels,
        "runner_group": config.RunnerGroup,
    }

    _, err := v.client.KVv2(v.mountPath).Put(ctx, path, data)
    if err != nil {
        return "", fmt.Errorf("failed to write secret: %w", err)
    }

    // Return full path for runner to retrieve
    return fmt.Sprintf("%s/data/%s", v.mountPath, path), nil
}

func (v *VaultStore) Get(ctx context.Context, ref string) (*RunnerConfig, error) {
    secret, err := v.client.KVv2(v.mountPath).Get(ctx, ref)
    if err != nil {
        return nil, fmt.Errorf("failed to read secret: %w", err)
    }

    // Parse secret data into RunnerConfig
    // ...
}

func (v *VaultStore) Delete(ctx context.Context, ref string) error {
    return v.client.KVv2(v.mountPath).DeleteMetadata(ctx, ref)
}
```

### Phase 2: Orchestrator Integration

#### 2.1 Configuration

Add to `pkg/config/config.go`:

```go
// Secret store configuration
SecretBackend string // "ssm" (default) | "vault"

// Vault configuration (only when SecretBackend == "vault")
VaultAddress    string
VaultMountPath  string
VaultSecretPath string
VaultAuthMethod string // "kubernetes" | "approle" | "aws"
VaultRole       string
VaultRoleID     string // For AppRole
VaultSecretID   string // For AppRole
```

Environment variables:
```bash
RUNS_FLEET_SECRET_BACKEND=vault
RUNS_FLEET_VAULT_ADDR=https://vault.example.com
RUNS_FLEET_VAULT_MOUNT_PATH=secret
RUNS_FLEET_VAULT_SECRET_PATH=runs-fleet/runners
RUNS_FLEET_VAULT_AUTH_METHOD=kubernetes
RUNS_FLEET_VAULT_ROLE=runs-fleet-orchestrator
```

#### 2.2 Update Runner Manager

Modify `pkg/runner/manager.go` to use the `secrets.Store` interface:

```go
type Manager struct {
    secretStore secrets.Store
    // ... other fields
}

func (m *Manager) PrepareRunner(ctx context.Context, job *Job) error {
    config := &secrets.RunnerConfig{
        JITToken:    job.JITToken,
        Repository:  job.Repository,
        Labels:      job.Labels,
        RunnerGroup: job.RunnerGroup,
    }

    ref, err := m.secretStore.Put(ctx, job.RunnerID, config)
    if err != nil {
        return fmt.Errorf("failed to store runner config: %w", err)
    }

    // Pass ref to EC2 instance via user-data
    job.ConfigRef = ref
    // ...
}
```

### Phase 3: Agent Integration

#### 3.1 Add Vault Config Fetcher

```go
// pkg/agent/config_vault.go

package agent

import (
    "context"
    "fmt"

    vault "github.com/hashicorp/vault/api"
)

type VaultConfigFetcher struct {
    client    *vault.Client
    mountPath string
}

func NewVaultConfigFetcher(address, authMethod, role, region string) (*VaultConfigFetcher, error) {
    client, err := vault.NewClient(&vault.Config{Address: address})
    if err != nil {
        return nil, err
    }

    // For runners, AWS IAM auth is recommended
    // Runner proves identity via EC2 instance metadata
    if authMethod == "aws" {
        if err := authenticateAWSIAM(client, role, region); err != nil {
            return nil, fmt.Errorf("vault aws auth failed: %w", err)
        }
    }

    return &VaultConfigFetcher{client: client}, nil
}

func (v *VaultConfigFetcher) FetchConfig(ctx context.Context, ref string) (*RunnerConfig, error) {
    secret, err := v.client.Logical().ReadWithContext(ctx, ref)
    if err != nil {
        return nil, fmt.Errorf("failed to read vault secret: %w", err)
    }

    // Parse and return config
    // ...
}
```

#### 3.2 Update Agent Main

```go
// cmd/agent/main.go

func loadConfig(ctx context.Context) (*RunnerConfig, error) {
    backend := os.Getenv("RUNS_FLEET_SECRET_BACKEND")

    switch backend {
    case "vault":
        fetcher, err := agent.NewVaultConfigFetcher(
            os.Getenv("RUNS_FLEET_VAULT_ADDR"),
            os.Getenv("RUNS_FLEET_VAULT_AUTH_METHOD"),
            os.Getenv("RUNS_FLEET_VAULT_ROLE"),
            os.Getenv("AWS_REGION"),
        )
        if err != nil {
            return nil, err
        }
        return fetcher.FetchConfig(ctx, os.Getenv("RUNS_FLEET_CONFIG_REF"))

    case "ssm", "":
        // Existing SSM logic (default)
        return fetchConfigFromSSM(ctx, os.Getenv("RUNS_FLEET_SSM_PARAMETER"))

    default:
        return nil, fmt.Errorf("unknown secret backend: %s", backend)
    }
}
```

### Phase 4: EC2 User-Data Updates

Pass secret backend info to runner via user-data/launch template:

```bash
#!/bin/bash
export RUNS_FLEET_SECRET_BACKEND="${SECRET_BACKEND}"
export RUNS_FLEET_CONFIG_REF="${CONFIG_REF}"

# Vault-specific (only when backend=vault)
export RUNS_FLEET_VAULT_ADDR="${VAULT_ADDR}"
export RUNS_FLEET_VAULT_AUTH_METHOD="aws"
export RUNS_FLEET_VAULT_ROLE="${VAULT_RUNNER_ROLE}"

/opt/runs-fleet/agent
```

### Phase 5: Vault Auth Configuration

#### 5.1 Orchestrator Auth (Kubernetes)

For K8s-deployed orchestrator, use Kubernetes auth:

```bash
# Enable Kubernetes auth in Vault
vault auth enable kubernetes

# Configure Kubernetes auth
vault write auth/kubernetes/config \
    kubernetes_host="https://kubernetes.default.svc" \
    kubernetes_ca_cert=@/var/run/secrets/kubernetes.io/serviceaccount/ca.crt

# Create policy for orchestrator
vault policy write runs-fleet-orchestrator - <<EOF
path "secret/data/runs-fleet/runners/*" {
  capabilities = ["create", "read", "update", "delete"]
}
EOF

# Create role for orchestrator
vault write auth/kubernetes/role/runs-fleet-orchestrator \
    bound_service_account_names=runs-fleet-orchestrator \
    bound_service_account_namespaces=runs-fleet \
    policies=runs-fleet-orchestrator \
    ttl=1h
```

#### 5.2 Runner Auth (AWS IAM)

For EC2 runners, use AWS IAM auth (no secrets needed on instance):

```bash
# Enable AWS auth in Vault
vault auth enable aws

# Configure AWS auth
vault write auth/aws/config/client \
    secret_key=$AWS_SECRET_KEY \
    access_key=$AWS_ACCESS_KEY

# Create policy for runners
vault policy write runs-fleet-runner - <<EOF
path "secret/data/runs-fleet/runners/*" {
  capabilities = ["read"]
}
EOF

# Create role for runners (bound to instance profile)
vault write auth/aws/role/runs-fleet-runner \
    auth_type=iam \
    bound_iam_principal_arn="arn:aws:iam::123456789012:role/runs-fleet-runner" \
    policies=runs-fleet-runner \
    ttl=15m
```

## Configuration Reference

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `RUNS_FLEET_SECRET_BACKEND` | Secret store backend | `ssm` |
| `RUNS_FLEET_VAULT_ADDR` | Vault server address | - |
| `RUNS_FLEET_VAULT_MOUNT_PATH` | KV secrets engine mount | `secret` |
| `RUNS_FLEET_VAULT_SECRET_PATH` | Path prefix for runner configs | `runs-fleet/runners` |
| `RUNS_FLEET_VAULT_AUTH_METHOD` | Auth method | `kubernetes` (orchestrator), `aws` (runner) |
| `RUNS_FLEET_VAULT_ROLE` | Vault role name | - |

### Helm Values

```yaml
secrets:
  backend: ssm  # ssm | vault

  # SSM configuration (default)
  ssm:
    parameterPrefix: /runs-fleet/runners

  # Vault configuration
  vault:
    address: https://vault.example.com
    mountPath: secret
    secretPath: runs-fleet/runners

    # Orchestrator auth (Kubernetes)
    orchestratorAuth:
      method: kubernetes
      role: runs-fleet-orchestrator

    # Runner auth (AWS IAM)
    runnerAuth:
      method: aws
      role: runs-fleet-runner
```

## Files Changed

| File | Change |
|------|--------|
| `pkg/secrets/store.go` | New - interface definition |
| `pkg/secrets/ssm.go` | New - SSM implementation |
| `pkg/secrets/vault.go` | New - Vault implementation |
| `pkg/secrets/vault_auth.go` | New - Vault auth helpers |
| `pkg/config/config.go` | Add Vault config fields |
| `pkg/runner/manager.go` | Use secrets.Store interface |
| `pkg/agent/config_vault.go` | New - Vault config fetcher |
| `cmd/agent/main.go` | Backend selection logic |
| `cmd/server/main.go` | Initialize secret store |
| `deploy/helm/runs-fleet/values.yaml` | Add secrets config section |
| `deploy/helm/runs-fleet/templates/deployment.yaml` | Add Vault env vars |
| `go.mod` | Add `github.com/hashicorp/vault/api` |

## Dependencies

```
github.com/hashicorp/vault/api v1.12.0
```

## Testing

1. **Unit tests**: Mock Vault client, test Store interface implementations
2. **Integration tests**: Local Vault dev server, test full flow
3. **E2E tests**: Deploy with Vault backend, run workflow

## Rollout

1. Deploy Vault with required auth methods
2. Update orchestrator with `RUNS_FLEET_SECRET_BACKEND=vault`
3. New runners use Vault, existing SSM runners continue working
4. Gradually migrate, then disable SSM backend

## Risks

| Risk | Mitigation |
|------|------------|
| Vault unavailability | Vault HA cluster, circuit breaker |
| Auth token expiry | Auto-renewal, short-lived tokens |
| Network latency | Vault in same region, connection pooling |
| Complex setup | Detailed docs, Terraform modules |
