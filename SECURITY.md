# Security Considerations

## Critical: Cache API Authentication (MUST FIX BEFORE PRODUCTION)

**Status:** ⚠️ NOT IMPLEMENTED
**Priority:** CRITICAL
**Tracked in:** Phase 4 - Production Hardening

### Issue

Cache endpoints (`/_apis/artifactcache/*` and `/_artifacts/*`) currently have **zero authentication**. This allows:

1. **Cache poisoning** - Attackers can upload malicious artifacts
2. **Data exfiltration** - Anyone can download cached artifacts
3. **Resource exhaustion** - Unlimited uploads to S3
4. **Cost attacks** - Abuse S3 storage/bandwidth

### Required Implementation

Add authentication middleware before exposing cache endpoints. Options:

#### Option 1: GitHub JIT Tokens (Recommended)
- Validate `Authorization: Bearer <token>` header
- Verify token with GitHub API
- Ensure token scope matches repository/org

#### Option 2: Shared Secret
- Generate per-runner secrets in SSM
- Validate `X-Cache-Token` header
- Simpler but less secure than JIT tokens

#### Option 3: Network Isolation
- Deploy cache endpoints on private subnet
- Only accessible from runner instances
- Requires VPC peering/PrivateLink

### Implementation Checklist

- [ ] Add authentication middleware to cache handler
- [ ] Validate tokens against GitHub API or shared secret store
- [ ] Add rate limiting per repository/org
- [ ] Log authentication failures with request metadata
- [ ] Add metrics for auth success/failure rates
- [ ] Update README with authentication configuration
- [ ] Add integration tests with auth enabled

### Temporary Mitigation

Until authentication is implemented:

1. **Do NOT expose cache endpoints publicly**
2. Deploy server in private subnet with Security Group restrictions
3. Use VPC endpoints for GitHub webhook traffic only
4. Monitor S3 bucket for unexpected objects
5. Set aggressive S3 lifecycle policies (7-day retention)

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
