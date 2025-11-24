# Agent Implementation Plan

## Executive Summary
Implement the GitHub Actions runner execution capability in the agent binary. Currently a placeholder that logs but doesn't execute jobs. This is the critical blocker preventing the system from functioning end-to-end.

## Context

**Current State:**
- Agent binary exists at `cmd/agent/main.go` with placeholder implementation (lines 33-35)
- Logs "Downloading...", "Registering...", "Executing..." but does nothing
- Self-termination logic exists but never reached by real job completion
- SSM Parameter Store integration designed but not used

**Why This Change:**
- System cannot execute actual GitHub Actions workflows
- Infrastructure orchestrates EC2 instances but they sit idle
- Blocks all validation and testing of the platform
- 60% → 85% functionality gap

**Problems It Solves:**
- Enables end-to-end workflow execution
- Validates architecture with real workloads
- Allows cost and performance measurement
- Unblocks integration testing

## Implementation Phases

### Phase 1: Runner Download & Verification
**Goal:** Reliably download and verify GitHub Actions runner binaries

**Tasks:**
1. Create `pkg/agent/downloader.go` - Fetch runner from GitHub Releases API
   - Query `https://api.github.com/repos/actions/runner/releases/latest`
   - Parse JSON for Linux x64/arm64 tarball URLs
   - Download to `/opt/actions-runner/`
   - Verify SHA256 checksums from API response

2. Add caching logic in `pkg/agent/cache.go`
   - Check S3 config bucket for cached runner binary
   - Upload successful downloads to S3 for faster subsequent boots
   - Key format: `runners/{version}/{arch}/actions-runner-{version}.tar.gz`

3. Extract and prepare runner directory
   - Decompress tarball to `/opt/actions-runner/`
   - Set execute permissions on `run.sh` and `config.sh`
   - Create working directory `/opt/actions-runner/_work/`

**Risks:**
- **Network failures:** Retry with exponential backoff (3 attempts)
- **Checksum mismatch:** Fail fast, log error, terminate instance
- **Disk space:** Verify 1GB free before download

**Files Modified:**
- `cmd/agent/main.go` - Replace lines 33-35 with downloader calls
- New: `pkg/agent/downloader.go`
- New: `pkg/agent/cache.go`

### Phase 2: Runner Registration
**Goal:** Register ephemeral runner with GitHub using JIT tokens

**Tasks:**
1. Fetch runner config from SSM Parameter Store
   - Read parameter path from `RUNS_FLEET_SSM_PARAMETER` env var
   - Parse JSON containing: `{org, repo, jit_token, labels, runner_group}`
   - Handle SSM errors gracefully (retry 3x with backoff)
   - Reference: `pkg/config/config.go` for SSM client setup

2. Create `pkg/agent/registration.go` - Register runner
   - Execute `./config.sh --unattended --ephemeral --url https://github.com/{org}/{repo} --token {jit_token} --labels {labels}`
   - Capture stdout/stderr to CloudWatch Logs
   - Verify registration success (exit code 0)
   - Handle registration failures (invalid token, org/repo not found)

3. Set runner environment
   - Configure `ACTIONS_RUNNER_HOOK_JOB_STARTED` to notify orchestrator (future)
   - Set `ACTIONS_RUNNER_HOOK_JOB_COMPLETED` for cleanup trigger
   - Pass through S3 cache endpoint via `ACTIONS_CACHE_URL`

**Risks:**
- **JIT token expiration:** Tokens expire in 1 hour, must register quickly after instance boot
- **GitHub API rate limits:** Circuit breaker in orchestrator (not agent concern)
- **Invalid org/repo:** Fail fast, log error, send termination notification

**Files Modified:**
- `cmd/agent/main.go` - Call registration before job execution
- New: `pkg/agent/registration.go`

**Integration Points:**
- SSM Parameter Store: `runs-fleet/runners/{instance-id}/config`
- CloudWatch Logs: `/runs-fleet/agent/{instance-id}`

### Phase 3: Job Execution & Monitoring
**Goal:** Execute workflow job and monitor progress

**Tasks:**
1. Create `pkg/agent/executor.go` - Run GitHub Actions job
   - Execute `./run.sh` in `/opt/actions-runner/`
   - Stream stdout/stderr to CloudWatch Logs in real-time
   - Monitor process for completion
   - Capture exit code (0 = success, non-zero = failure)

2. Implement graceful shutdown handling
   - Listen for SIGTERM (spot interruption warning)
   - On signal: Send SIGTERM to runner process
   - Runner has 90 seconds to clean up (GitHub's grace period)
   - Force kill after timeout
   - Log shutdown reason to CloudWatch

3. Add safety mechanisms in `pkg/agent/safety.go`
   - Maximum runtime enforcement (read from `RUNS_FLEET_MAX_RUNTIME` env var, default 360 min)
   - Start timer on job start, terminate instance on timeout
   - Disk space monitoring (check every 30s, fail if <500MB free)
   - Memory pressure detection (read `/proc/meminfo`, warn if <200MB available)

4. Stream logs to CloudWatch
   - Use existing CloudWatch client from `pkg/config`
   - Log group: `RUNS_FLEET_LOG_GROUP` env var
   - Log stream: `{instance-id}/{job-id}`
   - Batch logs every 5 seconds or 1MB

**Risks:**
- **Job hangs:** Max runtime timeout protects against infinite loops
- **Disk fills up:** Monitoring and early termination prevents cascading failures
- **Memory exhaustion:** OOM killer may terminate agent - log clearly for debugging
- **Network partition:** CloudWatch logs may be lost - consider local buffer

**Files Modified:**
- `cmd/agent/main.go` - Replace lines 38-40 with executor
- New: `pkg/agent/executor.go`
- New: `pkg/agent/safety.go`

### Phase 4: Telemetry & Cleanup
**Goal:** Report job status and clean up resources

**Tasks:**
1. Create `pkg/agent/telemetry.go` - Send job events
   - Job started: Send message to termination queue with metadata
   - Job completed: Send message with exit code, duration, logs URL
   - Job failed: Send message with error details
   - Message format: `{instance_id, job_id, status, exit_code, duration_seconds, started_at, completed_at}`

2. Implement cleanup in `pkg/agent/cleanup.go`
   - Remove runner directory `/opt/actions-runner/`
   - Clear working directory `/opt/actions-runner/_work/`
   - Remove SSM parameter (optional - housekeeping will handle)
   - Flush CloudWatch logs buffer

3. Update self-termination logic in `cmd/agent/main.go:52-81`
   - Move to `pkg/agent/termination.go`
   - Send termination notification before calling EC2 API
   - Wait for notification to be queued (with timeout)
   - Then terminate instance via EC2 API
   - Log all steps for debugging

4. Add crash recovery
   - Defer panic handler in main()
   - On panic: Log stack trace, send failure notification, terminate
   - Ensure instance never stays running after agent crash

**Risks:**
- **Termination notification fails:** Instance terminates anyway (housekeeping will clean up orphaned state)
- **CloudWatch flush fails:** Logs may be incomplete (non-critical, job status in DynamoDB)
- **Cleanup fails:** Next instance gets fresh filesystem (ephemeral instances)

**Files Modified:**
- `cmd/agent/main.go` - Integrate telemetry and cleanup
- New: `pkg/agent/telemetry.go`
- New: `pkg/agent/cleanup.go`
- New: `pkg/agent/termination.go`

## Testing Strategy

### Unit Tests
- `pkg/agent/downloader_test.go` - Mock GitHub API responses
- `pkg/agent/registration_test.go` - Mock SSM and config.sh execution
- `pkg/agent/executor_test.go` - Mock run.sh with various exit codes
- `pkg/agent/safety_test.go` - Test timeout and resource monitoring
- `pkg/agent/telemetry_test.go` - Mock SQS message sending

### Integration Tests
- **Local Docker test:** Build agent, run in container with mock SSM/CloudWatch
- **Real workflow test:** Deploy to AWS, trigger actual GitHub workflow
- **Spot interruption test:** Send SIGTERM to agent mid-job, verify graceful shutdown
- **Timeout test:** Run job exceeding max runtime, verify forced termination
- **Disk space test:** Fill disk during job, verify early termination

### Validation Checklist
- [ ] Agent downloads runner binary successfully
- [ ] Agent registers with GitHub and appears in runners list
- [ ] Simple workflow (echo "hello") completes successfully
- [ ] Agent self-terminates after job completion
- [ ] Logs appear in CloudWatch
- [ ] Termination notification appears in queue
- [ ] SIGTERM triggers graceful shutdown
- [ ] Max runtime timeout works
- [ ] Disk space monitoring works

## Success Metrics
- **Functionality:** End-to-end workflow execution success rate >95%
- **Performance:** Job start time <60s from instance launch
- **Reliability:** Agent crash rate <1%
- **Cost:** No zombie instances (all terminate within max_runtime + 5 min)

## Dependencies
- Termination queue must exist in infrastructure (currently documented but not deployed)
- SSM Parameter Store pattern must be implemented in orchestrator (exists but verify)
- CloudWatch Log Group must exist (verify in Terraform)

## Timeline
- **Phase 1:** 3 days (downloader + cache logic)
- **Phase 2:** 2 days (registration integration)
- **Phase 3:** 4 days (execution + safety mechanisms)
- **Phase 4:** 2 days (telemetry + cleanup)
- **Testing:** 3 days (integration tests with real workflows)

**Total: 14 days (2-3 weeks with buffer)**

## Next Steps After Completion
1. Deploy agent binary to S3 config bucket
2. Update launch template user data to download agent
3. Run integration tests with real GitHub workflows
4. Monitor CloudWatch for errors
5. Proceed to Phase 7 (Queue Processors) to handle termination notifications
