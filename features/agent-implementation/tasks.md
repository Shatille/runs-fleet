# Agent Implementation - Task Checklist

**Last Updated:** 2025-11-25

## Phase 1: Runner Download & Verification

### Core Download Logic
- [ ] Create `pkg/agent/downloader.go` with package documentation
- [ ] Implement `DownloadRunner(ctx context.Context, version, arch string) (string, error)` function
- [ ] Add GitHub Releases API client (query `/repos/actions/runner/releases/latest`)
- [ ] Parse JSON response for tarball URL matching architecture
- [ ] Download tarball to `/opt/actions-runner/` directory
- [ ] Verify SHA256 checksum from API response
- [ ] Extract tarball and set executable permissions

### S3 Caching
- [ ] Create `pkg/agent/cache.go` with package documentation
- [ ] Implement `CheckCache(ctx context.Context, version, arch string) (bool, string, error)` function
- [ ] Implement `DownloadFromCache(ctx context.Context, version, arch, destPath string) error` function
- [ ] Implement `UploadToCache(ctx context.Context, version, arch, sourcePath string) error` function
- [ ] Use S3 client from `pkg/config` with proper error handling
- [ ] Add cache key format: `runners/{version}/{arch}/actions-runner-{version}.tar.gz`

### Integration
- [ ] Update `cmd/agent/main.go:33` to call downloader
- [ ] Add retry logic with exponential backoff (3 attempts)
- [ ] Handle errors (log and terminate instance on failure)
- [ ] Add environment variable `RUNS_FLEET_CONFIG_BUCKET` to launch template

## Phase 2: Runner Registration

### SSM Parameter Fetch
- [ ] Create `pkg/agent/registration.go` with package documentation
- [ ] Implement `FetchConfig(ctx context.Context, parameterPath string) (*RunnerConfig, error)` function
- [ ] Define `RunnerConfig` struct matching SSM JSON format
- [ ] Use SSM client from `pkg/config` with retry logic
- [ ] Parse JSON and validate required fields (org, repo, jit_token, labels)

### GitHub Registration
- [ ] Implement `RegisterRunner(ctx context.Context, config *RunnerConfig, runnerPath string) error` function
- [ ] Execute `./config.sh --unattended --ephemeral --url {url} --token {token} --labels {labels}`
- [ ] Use `os/exec` package with proper context cancellation
- [ ] Capture stdout/stderr and stream to CloudWatch Logs
- [ ] Verify registration success (exit code 0)
- [ ] Handle registration failures with clear error messages

### Environment Setup
- [ ] Set `ACTIONS_CACHE_URL` environment variable for runner process
- [ ] Configure runner working directory `/opt/actions-runner/_work/`
- [ ] Set proper file permissions on runner directory

### Integration
- [ ] Update `cmd/agent/main.go:34` to call registration
- [ ] Handle JIT token expiration gracefully (< 1 hour from parameter creation)
- [ ] Add CloudWatch log stream creation for runner output

## Phase 3: Job Execution & Monitoring

### Executor Core
- [ ] Create `pkg/agent/executor.go` with package documentation
- [ ] Implement `ExecuteJob(ctx context.Context, runnerPath string) (int, error)` function
- [ ] Execute `./run.sh` in runner directory using `os/exec`
- [ ] Stream stdout/stderr to CloudWatch Logs in real-time
- [ ] Monitor process for completion
- [ ] Capture and return exit code

### Graceful Shutdown
- [ ] Implement `HandleShutdown(ctx context.Context, runnerPID int) error` function
- [ ] Listen for SIGTERM signal (spot interruption warning)
- [ ] Forward SIGTERM to runner process
- [ ] Wait 90 seconds for graceful cleanup (GitHub's grace period)
- [ ] Force kill runner process after timeout
- [ ] Log shutdown reason and state to CloudWatch

### Safety Mechanisms
- [ ] Create `pkg/agent/safety.go` with package documentation
- [ ] Implement `EnforceMaxRuntime(ctx context.Context, duration time.Duration) <-chan struct{}` function
- [ ] Implement `MonitorDiskSpace(ctx context.Context, threshold int64) error` function
- [ ] Implement `MonitorMemory(ctx context.Context, threshold int64) error` function
- [ ] Check disk space every 30 seconds, fail if < 500MB free
- [ ] Check memory via `/proc/meminfo`, warn if < 200MB available
- [ ] Start timer on job start, send signal on timeout

### CloudWatch Logging
- [ ] Implement `StreamLogs(ctx context.Context, reader io.Reader, logGroup, logStream string) error` function
- [ ] Use CloudWatch Logs client from `pkg/config`
- [ ] Create log stream if not exists
- [ ] Batch log events (5 seconds or 1MB, whichever comes first)
- [ ] Handle rate limiting and backoff

### Integration
- [ ] Update `cmd/agent/main.go:35` to call executor
- [ ] Integrate safety mechanisms (max runtime, disk, memory monitoring)
- [ ] Handle executor errors and log to CloudWatch
- [ ] Ensure context cancellation propagates to all goroutines

## Phase 4: Telemetry & Cleanup

### Telemetry Messaging
- [ ] Create `pkg/agent/telemetry.go` with package documentation
- [ ] Define `JobStatus` struct for termination messages
- [ ] Implement `SendJobStarted(ctx context.Context, metadata JobStatus) error` function
- [ ] Implement `SendJobCompleted(ctx context.Context, metadata JobStatus) error` function
- [ ] Use SQS client from `pkg/queue` package
- [ ] Add environment variable `RUNS_FLEET_TERMINATION_QUEUE_URL` to launch template
- [ ] Include all metadata: instance_id, job_id, status, exit_code, duration, timestamps

### Cleanup Logic
- [ ] Create `pkg/agent/cleanup.go` with package documentation
- [ ] Implement `CleanupRunner(ctx context.Context, runnerPath string) error` function
- [ ] Remove runner directory `/opt/actions-runner/`
- [ ] Clear working directory `/opt/actions-runner/_work/`
- [ ] Flush CloudWatch Logs buffer with timeout
- [ ] Log cleanup completion

### Self-Termination
- [ ] Create `pkg/agent/termination.go` with package documentation
- [ ] Move `terminateInstance()` from `cmd/agent/main.go:52-81` to this file
- [ ] Rename to `TerminateInstance(ctx context.Context, instanceID string) error`
- [ ] Add pre-termination notification sending
- [ ] Wait for notification to be queued (with 10 second timeout)
- [ ] Then call EC2 TerminateInstances API
- [ ] Log all steps for debugging

### Crash Recovery
- [ ] Add `defer recover()` panic handler in `cmd/agent/main.go:main()`
- [ ] On panic: log stack trace to CloudWatch
- [ ] Send failure notification with panic details
- [ ] Terminate instance even on panic
- [ ] Ensure no code paths leave instance running after agent crash

### Integration
- [ ] Update `cmd/agent/main.go:38-49` to integrate telemetry
- [ ] Send job started notification before execution
- [ ] Send job completed notification after execution
- [ ] Call cleanup before termination
- [ ] Replace old termination logic with new `pkg/agent/termination.go` version

## Testing

### Unit Tests
- [ ] `pkg/agent/downloader_test.go` - Test with mocked HTTP responses
- [ ] `pkg/agent/cache_test.go` - Test with mocked S3 client
- [ ] `pkg/agent/registration_test.go` - Test with mocked SSM and exec
- [ ] `pkg/agent/executor_test.go` - Test with mocked runner process
- [ ] `pkg/agent/safety_test.go` - Test timeouts and resource monitoring
- [ ] `pkg/agent/telemetry_test.go` - Test with mocked SQS client
- [ ] `pkg/agent/cleanup_test.go` - Test file removal logic
- [ ] `pkg/agent/termination_test.go` - Test with mocked EC2 client

### Integration Tests
- [ ] Create `test/integration/agent_test.go`
- [ ] Test: Download runner binary successfully
- [ ] Test: Register runner with GitHub (requires test repo/token)
- [ ] Test: Execute simple workflow (echo "hello")
- [ ] Test: SIGTERM triggers graceful shutdown
- [ ] Test: Max runtime timeout terminates job
- [ ] Test: Disk space monitoring triggers early termination
- [ ] Test: Agent self-terminates after job completion
- [ ] Test: CloudWatch logs appear correctly
- [ ] Test: Termination notification sent to queue

### Local Testing (Docker)
- [ ] Create `Dockerfile.agent-test` for local testing
- [ ] Create `docker-compose.yml` with LocalStack (mock AWS services)
- [ ] Test agent with mocked SSM, SQS, CloudWatch, EC2
- [ ] Verify all workflows without AWS credentials

### Live Testing
- [ ] Deploy agent binary to S3 config bucket
- [ ] Update launch template with new agent binary URL
- [ ] Create test GitHub repository with simple workflow
- [ ] Trigger workflow and verify end-to-end execution
- [ ] Monitor CloudWatch logs for errors
- [ ] Verify instance self-terminates
- [ ] Check termination queue for notification message

## Documentation
- [ ] Update README.md with agent implementation status
- [ ] Document environment variables in README.md
- [ ] Update architecture diagram to show agent flow
- [ ] Add troubleshooting section for common agent errors
- [ ] Document runner binary caching behavior

## Infrastructure Updates
- [ ] Add `RUNS_FLEET_CONFIG_BUCKET` to Terraform variables
- [ ] Add `RUNS_FLEET_TERMINATION_QUEUE_URL` to Terraform variables
- [ ] Create termination queue in Terraform (if not exists)
- [ ] Update launch template user data to pass new env vars
- [ ] Update IAM instance profile permissions:
  - [ ] `ssm:GetParameter` on `/runs-fleet/runners/*`
  - [ ] `s3:GetObject` on `${config_bucket}/runners/*`
  - [ ] `s3:PutObject` on `${config_bucket}/runners/*`
  - [ ] `logs:CreateLogStream` on `/runs-fleet/agent/*`
  - [ ] `logs:PutLogEvents` on `/runs-fleet/agent/*`
  - [ ] `sqs:SendMessage` on termination queue
  - [ ] `ec2:TerminateInstances` on own instance only

## Validation Checklist
- [ ] Agent downloads runner binary without errors
- [ ] Agent registers with GitHub (runner appears in settings)
- [ ] Simple workflow completes successfully
- [ ] Complex workflow (multi-step, with cache) completes
- [ ] Agent self-terminates after job completion
- [ ] Instance state becomes "terminated" in EC2 console
- [ ] CloudWatch logs contain complete runner output
- [ ] Termination notification appears in SQS queue
- [ ] SIGTERM signal triggers graceful shutdown
- [ ] Max runtime timeout forces termination (test with 1-minute timeout)
- [ ] Disk space monitoring works (test by filling disk)
- [ ] Memory monitoring doesn't false-positive
- [ ] Panic recovery terminates instance correctly
- [ ] No zombie instances remain after 24h operation

## Post-Completion
- [ ] Mark Phase 6 as ✅ Complete in README.md
- [ ] Update roadmap with actual timeline
- [ ] Document lessons learned in this context.md
- [ ] Proceed to Phase 7: Queue Processors
