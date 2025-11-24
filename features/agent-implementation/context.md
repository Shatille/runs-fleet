# Agent Implementation - Context & Key Decisions

**Last Updated:** 2025-11-25

## Key Files

### Current Agent
- `cmd/agent/main.go:1-82` - Current placeholder implementation
  - Lines 33-35: Placeholder logs that need replacement
  - Lines 52-81: `terminateInstance()` function - working, will be moved to pkg
  - Signal handling works (lines 18-19, 42-47)

### Related Infrastructure
- `pkg/config/config.go` - AWS SDK clients (EC2, SSM, CloudWatch, S3)
- `pkg/queue/sqs.go` - SQS message sending (will use for telemetry)
- `internal/logging/` - Not present, need to create or use stdlib log

### To Be Created
- `pkg/agent/downloader.go` - GitHub runner binary download
- `pkg/agent/cache.go` - S3 caching for downloaded runners
- `pkg/agent/registration.go` - GitHub runner registration
- `pkg/agent/executor.go` - Job execution and monitoring
- `pkg/agent/safety.go` - Safety timeouts and resource monitoring
- `pkg/agent/telemetry.go` - Send job status messages
- `pkg/agent/cleanup.go` - Cleanup runner directory
- `pkg/agent/termination.go` - Refactored self-termination

### Testing Files
- `pkg/agent/downloader_test.go`
- `pkg/agent/registration_test.go`
- `pkg/agent/executor_test.go`
- `pkg/agent/safety_test.go`
- `pkg/agent/telemetry_test.go`

## Architectural Decisions

### 1. Ephemeral Runners Only
**Decision:** Use GitHub's `--ephemeral` flag during registration
**Rationale:** Prevents runner reuse, aligns with security model (no state accumulation)
**Impact:** Each instance handles exactly one job, then self-terminates

### 2. JIT Token Registration
**Decision:** Use Just-In-Time registration tokens from GitHub API
**Rationale:** More secure than PAT tokens, automatically expire after 1 hour
**Impact:** Orchestrator must generate JIT token and store in SSM before instance launch

### 3. S3 Binary Caching
**Decision:** Cache downloaded runner binaries in S3 config bucket
**Rationale:** Reduces GitHub API load, faster instance boot (no download needed)
**Impact:** First boot downloads from GitHub, subsequent boots use S3 cache

### 4. Termination Queue Pattern
**Decision:** Agent sends termination notification to SQS before self-terminating
**Rationale:** Orchestrator needs to update DynamoDB state, clean up resources
**Impact:** Requires termination queue processor (Phase 7)

### 5. CloudWatch Streaming Logs
**Decision:** Stream runner output to CloudWatch in real-time
**Rationale:** Debugging and audit trail (GitHub logs may not persist)
**Impact:** Increases CloudWatch costs slightly (~$0.50/GB ingested)

### 6. Safety Timeouts
**Decision:** Enforce maximum runtime (default 360 minutes) at agent level
**Rationale:** Prevents runaway jobs from driving up costs
**Impact:** Jobs exceeding timeout are forcibly terminated

### 7. Graceful Shutdown on SIGTERM
**Decision:** Forward SIGTERM to runner process, wait 90 seconds for cleanup
**Rationale:** GitHub runners expect grace period for job cleanup
**Impact:** Spot interruptions handled gracefully (job re-queued by orchestrator)

## Integration Points

### SSM Parameter Store (Input)
- **Path:** `/runs-fleet/runners/{instance-id}/config`
- **Format:** JSON
  ```json
  {
    "org": "Shavakan",
    "repo": "my-repo",
    "jit_token": "JITC_...",
    "labels": ["runs-fleet=12345", "runner=2cpu-linux-arm64"],
    "runner_group": "Default",
    "job_id": "67890"
  }
  ```
- **Created by:** Orchestrator fleet manager before launching instance
- **Lifecycle:** Agent reads on boot, orchestrator/housekeeping deletes after termination

### Termination Queue (Output)
- **Queue:** `RUNS_FLEET_TERMINATION_QUEUE_URL` environment variable
- **Message Format:**
  ```json
  {
    "instance_id": "i-0123456789abcdef0",
    "job_id": "67890",
    "status": "success|failure|timeout|interrupted",
    "exit_code": 0,
    "duration_seconds": 123,
    "started_at": "2025-11-25T10:00:00Z",
    "completed_at": "2025-11-25T10:02:03Z",
    "error": "optional error message"
  }
  ```
- **Consumer:** Termination queue processor (Phase 7)

### CloudWatch Logs (Output)
- **Log Group:** `RUNS_FLEET_LOG_GROUP` environment variable (e.g., `/runs-fleet/agent`)
- **Log Stream:** `{instance-id}/{job-id}`
- **Content:** Runner stdout/stderr, agent lifecycle events

### S3 Config Bucket (Input/Output)
- **Bucket:** `RUNS_FLEET_CONFIG_BUCKET` environment variable
- **Cache Path:** `runners/{version}/{arch}/actions-runner-{version}.tar.gz`
- **Used for:** Caching downloaded GitHub runner binaries

### GitHub API (External)
- **Releases API:** `https://api.github.com/repos/actions/runner/releases/latest`
- **Rate Limit:** 60 req/hour unauthenticated, 5000/hour with token (not needed for releases)
- **Used for:** Downloading runner binaries

### EC2 API (Output)
- **API Call:** `TerminateInstances` with own instance ID
- **Purpose:** Self-termination after job completion
- **IAM Permission:** Instance profile must have `ec2:TerminateInstances` for own ID

## Important Patterns

### Error Handling
Follow existing pattern from `pkg/fleet/fleet.go`:
```go
if err != nil {
    return fmt.Errorf("failed to {action}: %w", err)
}
```

### AWS SDK Usage
Initialize clients using `pkg/config/config.go` pattern:
```go
cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
if err != nil {
    return fmt.Errorf("failed to load AWS config: %w", err)
}
client := ec2.NewFromConfig(cfg)
```

### Context Propagation
Always pass `context.Context` as first parameter:
```go
func DownloadRunner(ctx context.Context, version, arch string) error {
    // Use ctx for HTTP requests, AWS API calls
}
```

### Testing Pattern
Follow `pkg/queue/sqs_test.go` structure:
- Use table-driven tests
- Mock AWS SDK calls using interfaces
- Test error paths explicitly

### Logging
Use stdlib `log` package (matches `cmd/agent/main.go`):
```go
log.Printf("Downloading runner version %s for %s", version, arch)
```

## Data Flow

```
1. Instance boots with user data script
2. User data downloads agent binary from S3
3. Agent starts, reads environment variables
4. Agent fetches config from SSM Parameter Store
   ↓
5. Agent downloads/caches GitHub runner binary
6. Agent registers ephemeral runner with GitHub
7. GitHub assigns workflow job to runner
   ↓
8. Agent executes runner process (./run.sh)
9. Agent monitors job progress, streams logs
10. Job completes (success/failure/timeout)
   ↓
11. Agent sends termination notification to queue
12. Agent cleans up runner directory
13. Agent terminates EC2 instance via API
```

## Next Steps

**To resume work on this feature:**

1. **Start with Phase 1:** Create `pkg/agent/downloader.go`
   - Implement `DownloadRunner(ctx, version, arch string) (string, error)`
   - Return path to extracted runner directory
   - Add unit tests with mocked HTTP responses

2. **Then Phase 1 continued:** Create `pkg/agent/cache.go`
   - Implement `CheckCache(ctx, version, arch string) (bool, error)`
   - Implement `CacheRunner(ctx, version, arch, localPath string) error`
   - Use S3 client from `pkg/config`

3. **Integration point:** Update `cmd/agent/main.go` line 33
   - Replace placeholder log with `downloader.DownloadRunner()` call
   - Handle errors (log and terminate instance)

**Reference existing code:**
- SSM client usage: `pkg/config/config.go:48-52`
- S3 operations: `pkg/cache/s3.go` (pre-signed URLs, uploads)
- Error patterns: `pkg/fleet/fleet.go:200-210`
- Testing: `pkg/queue/sqs_test.go`

**Environment variables needed:**
- `RUNS_FLEET_INSTANCE_ID` (already present)
- `RUNS_FLEET_SSM_PARAMETER` (already present)
- `RUNS_FLEET_LOG_GROUP` (already present)
- `RUNS_FLEET_MAX_RUNTIME` (already present)
- `RUNS_FLEET_CONFIG_BUCKET` (add to launch template)
- `RUNS_FLEET_TERMINATION_QUEUE_URL` (add to launch template)
- `RUNS_FLEET_S3_CACHE_ENDPOINT` (already present for GitHub Actions cache)
