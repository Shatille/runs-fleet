# Security Considerations

## Cache API Authentication

**Status:** ✅ IMPLEMENTED
**Implementation:** HMAC-based token authentication

### Overview

Cache endpoints (`/_apis/artifactcache/*` and `/_artifacts/*`) are protected by HMAC-based token authentication when `RUNS_FLEET_CACHE_SECRET` is configured.

### How It Works

1. **Token Generation**: When creating a runner config, the server generates an HMAC-SHA256 token:
   ```
   token = HMAC-SHA256(secret, job_id + ":" + instance_id)
   ```

2. **Token Distribution**: The token is included in the runner config stored in SSM, alongside the JIT token.

3. **Token Validation**: Cache requests must include the token via:
   - `X-Cache-Token` header (preferred), OR
   - `Authorization: Bearer <token>` header

4. **Server Validation**: The cache handler validates tokens against registered active jobs.

### Configuration

Enable cache authentication by setting:
```bash
RUNS_FLEET_CACHE_SECRET=<your-secret-key>
```

**Important:** Generate a strong random secret (32+ characters recommended):
```bash
openssl rand -hex 32
```

### Security Properties

- **Unforgeable**: Tokens cannot be generated without knowledge of the server secret
- **Job-scoped**: Each token is tied to a specific job/instance combination
- **Ephemeral**: Tokens are valid only while the job is active
- **Zero AWS Cost**: Validation is pure computation (no API calls)
- **Stateless**: Works across server restarts (token store rebuilt on job registration)

### Backwards Compatibility

If `RUNS_FLEET_CACHE_SECRET` is not set:
- Authentication is **disabled** (warning logged at startup)
- All cache requests are allowed (legacy behavior)
- A warning is logged: `WARNING: Cache authentication disabled`

### Remaining Recommendations

While authentication is now implemented, consider these additional hardening measures:

1. **Network Isolation**: Deploy in private subnet for defense-in-depth
2. **Rate Limiting**: Add rate limiting per token (not yet implemented)
3. **Audit Logging**: Log all cache operations to CloudWatch (partial)
4. **S3 Lifecycle**: Set aggressive retention policies (7-day recommended)

## Other Security Items

### Medium Priority

1. **Request size validation** - ✅ DONE (MaxBytesReader set to 1MB)
2. **Path traversal protection** - ✅ DONE (validateKey checks for `..`, `/`, `\`)
3. **Information disclosure** - ✅ DONE (generic error messages)
4. **Cache size validation** - ⚠️ PARTIAL (decoded but not enforced)

### Future Enhancements

- Add HTTPS/TLS termination at ALB
- Implement request signing (similar to S3)
- Add audit logging to CloudWatch Logs
- Enable S3 bucket versioning for rollback
- Add CloudFront for cache distribution (reduce S3 costs)
