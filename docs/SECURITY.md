# Security Considerations

## Cache API Authentication

**Status:** ✅ IMPLEMENTED
**Implementation:** HMAC-based token authentication

### Overview

Cache endpoints (`/_apis/artifactcache/*` and `/_artifacts/*`) are protected by HMAC-based token authentication when `RUNS_FLEET_CACHE_SECRET` is configured.

### How It Works

1. **Token Generation**: When creating a runner config, generate a self-contained HMAC token:
   ```
   payload = base64url(job_id + ":" + instance_id)
   signature = HMAC-SHA256(secret, job_id + ":" + instance_id)
   token = payload + "." + hex(signature)
   ```

2. **Token Distribution**: The token is included in the runner config stored in SSM (EC2) or ConfigMap (K8s).

3. **Token Validation**: Cache requests must include the token via:
   - `X-Cache-Token` header (preferred), OR
   - `Authorization: Bearer <token>` header

4. **Server Validation**: The cache handler:
   - Parses the token to extract the payload
   - Recomputes the HMAC signature
   - Compares signatures (no state/lookup required)

### Configuration

Enable cache authentication by setting:
```bash
RUNS_FLEET_CACHE_SECRET=<your-secret-key>
```

Generate a strong random secret (32+ characters recommended):
```bash
openssl rand -hex 32
```

### Security Properties

- **Unforgeable**: Tokens cannot be generated without knowledge of the server secret
- **Job-scoped**: Each token is tied to a specific job/instance combination
- **Ephemeral**: Tokens are valid only while the job is active
- **Stateless**: Server needs no token storage - validation is pure HMAC recomputation
- **Multi-instance Safe**: Works across multiple server instances without shared state

### Backwards Compatibility

If `RUNS_FLEET_CACHE_SECRET` is not set:
- Authentication is **disabled** (warning logged at startup)
- All cache requests are allowed (legacy behavior)

## TLS Configuration

### Kubernetes (Helm)

TLS is supported via Ingress or Istio Gateway:

```yaml
# Ingress TLS
ingress:
  enabled: true
  tls:
    enabled: true
    secretName: runs-fleet-tls

# Istio Gateway TLS
istio:
  gateway:
    tls:
      enabled: true
      credentialName: runs-fleet-tls
```

### ECS/Fargate

Configure TLS termination at the ALB via Terraform (infrastructure concern).

## Kubernetes Security

When deploying on Kubernetes:

1. **RBAC**: ServiceAccount with minimal permissions (pods, configmaps in namespace only)
2. **Network Policies**: Restrict egress to required endpoints (GitHub API, container registry)
3. **Pod Security**: Non-root containers, read-only root filesystem where possible
4. **Secrets**: Use Kubernetes Secrets or external secret managers for sensitive config

## Remaining Hardening

| Item | Status |
|------|--------|
| Cache size validation | ⚠️ PARTIAL (decoded but not enforced) |
| Rate limiting per token | Not implemented |
| Structured audit logging | Partial (log.Printf) |
| S3 bucket versioning | Infrastructure concern |
| CloudFront cache distribution | Not implemented |
