package provider

import (
	"context"
	"testing"
	"time"
)

const stateRunning = "running"

// Mock implementations to verify interfaces can be implemented

type mockProvider struct {
	name string
}

func (m *mockProvider) CreateRunner(_ context.Context, spec *RunnerSpec) (*RunnerResult, error) {
	return &RunnerResult{
		RunnerIDs: []string{"runner-" + spec.RunID},
	}, nil
}

func (m *mockProvider) TerminateRunner(_ context.Context, _ string) error {
	return nil
}

func (m *mockProvider) DescribeRunner(_ context.Context, runnerID string) (*RunnerState, error) {
	return &RunnerState{
		RunnerID: runnerID,
		State:    stateRunning,
	}, nil
}

func (m *mockProvider) Name() string {
	return m.name
}

type mockPoolProvider struct{}

func (m *mockPoolProvider) ListPoolRunners(_ context.Context, _ string) ([]PoolRunner, error) {
	return []PoolRunner{}, nil
}

func (m *mockPoolProvider) StartRunners(_ context.Context, _ []string) error {
	return nil
}

func (m *mockPoolProvider) StopRunners(_ context.Context, _ []string) error {
	return nil
}

func (m *mockPoolProvider) TerminateRunners(_ context.Context, _ []string) error {
	return nil
}

func (m *mockPoolProvider) MarkRunnerBusy(_ string) {}

func (m *mockPoolProvider) MarkRunnerIdle(_ string) {}

type mockConfigStore struct{}

func (m *mockConfigStore) StoreRunnerConfig(_ context.Context, _ *StoreConfigRequest) error {
	return nil
}

func (m *mockConfigStore) DeleteRunnerConfig(_ context.Context, _ string) error {
	return nil
}

type mockStateStore struct{}

func (m *mockStateStore) SaveJob(_ context.Context, _ *Job) error {
	return nil
}

func (m *mockStateStore) GetJob(_ context.Context, _ string) (*Job, error) {
	return &Job{}, nil
}

func (m *mockStateStore) MarkJobComplete(_ context.Context, _, _ string, _, _ int) error {
	return nil
}

func (m *mockStateStore) MarkJobTerminating(_ context.Context, _ string) error {
	return nil
}

func (m *mockStateStore) UpdateJobMetrics(_ context.Context, _ string, _, _ time.Time) error {
	return nil
}

func (m *mockStateStore) GetPoolConfig(_ context.Context, _ string) (*PoolConfig, error) {
	return &PoolConfig{}, nil
}

func (m *mockStateStore) SavePoolConfig(_ context.Context, _ *PoolConfig) error {
	return nil
}

func (m *mockStateStore) ListPools(_ context.Context) ([]string, error) {
	return []string{}, nil
}

func (m *mockStateStore) UpdatePoolState(_ context.Context, _ string, _, _ int) error {
	return nil
}

type mockCoordinator struct {
	leader bool
}

func (m *mockCoordinator) IsLeader() bool {
	return m.leader
}

func (m *mockCoordinator) Start(_ context.Context) error {
	return nil
}

func (m *mockCoordinator) Stop() error {
	return nil
}

// Tests

func TestProviderInterface(t *testing.T) {
	var p Provider = &mockProvider{name: "test-provider"}

	ctx := context.Background()
	spec := &RunnerSpec{
		RunID: "test-run",
	}

	result, err := p.CreateRunner(ctx, spec)
	if err != nil {
		t.Errorf("CreateRunner() error = %v", err)
	}
	if len(result.RunnerIDs) != 1 || result.RunnerIDs[0] != "runner-test-run" {
		t.Errorf("CreateRunner() = %v, want [runner-test-run]", result.RunnerIDs)
	}

	if termErr := p.TerminateRunner(ctx, "runner-1"); termErr != nil {
		t.Errorf("TerminateRunner() error = %v", termErr)
	}

	state, descErr := p.DescribeRunner(ctx, "runner-1")
	if descErr != nil {
		t.Errorf("DescribeRunner() error = %v", descErr)
	}
	if state.State != stateRunning {
		t.Errorf("DescribeRunner() state = %v, want %s", state.State, stateRunning)
	}

	if p.Name() != "test-provider" {
		t.Errorf("Name() = %v, want test-provider", p.Name())
	}
}

func TestPoolProviderInterface(t *testing.T) {
	var pp PoolProvider = &mockPoolProvider{}

	ctx := context.Background()

	runners, err := pp.ListPoolRunners(ctx, "default")
	if err != nil {
		t.Errorf("ListPoolRunners() error = %v", err)
	}
	if runners == nil {
		t.Error("ListPoolRunners() returned nil")
	}

	if startErr := pp.StartRunners(ctx, []string{"r1", "r2"}); startErr != nil {
		t.Errorf("StartRunners() error = %v", startErr)
	}

	if stopErr := pp.StopRunners(ctx, []string{"r1"}); stopErr != nil {
		t.Errorf("StopRunners() error = %v", stopErr)
	}

	if termErr := pp.TerminateRunners(ctx, []string{"r1"}); termErr != nil {
		t.Errorf("TerminateRunners() error = %v", termErr)
	}

	// These should not panic
	pp.MarkRunnerBusy("r1")
	pp.MarkRunnerIdle("r1")
}

func TestConfigStoreInterface(t *testing.T) {
	var cs ConfigStore = &mockConfigStore{}

	ctx := context.Background()

	req := &StoreConfigRequest{
		RunnerID: "runner-1",
		JobID:    "job-1",
	}
	if err := cs.StoreRunnerConfig(ctx, req); err != nil {
		t.Errorf("StoreRunnerConfig() error = %v", err)
	}

	if err := cs.DeleteRunnerConfig(ctx, "runner-1"); err != nil {
		t.Errorf("DeleteRunnerConfig() error = %v", err)
	}
}

func TestStateStoreInterface(t *testing.T) {
	var ss StateStore = &mockStateStore{}

	ctx := context.Background()
	now := time.Now()

	if saveErr := ss.SaveJob(ctx, &Job{}); saveErr != nil {
		t.Errorf("SaveJob() error = %v", saveErr)
	}

	job, getErr := ss.GetJob(ctx, "runner-1")
	if getErr != nil {
		t.Errorf("GetJob() error = %v", getErr)
	}
	if job == nil {
		t.Error("GetJob() returned nil")
	}

	if completeErr := ss.MarkJobComplete(ctx, "runner-1", "success", 0, 100); completeErr != nil {
		t.Errorf("MarkJobComplete() error = %v", completeErr)
	}

	if termErr := ss.MarkJobTerminating(ctx, "runner-1"); termErr != nil {
		t.Errorf("MarkJobTerminating() error = %v", termErr)
	}

	if metricsErr := ss.UpdateJobMetrics(ctx, "runner-1", now, now.Add(time.Hour)); metricsErr != nil {
		t.Errorf("UpdateJobMetrics() error = %v", metricsErr)
	}

	cfg, cfgErr := ss.GetPoolConfig(ctx, "default")
	if cfgErr != nil {
		t.Errorf("GetPoolConfig() error = %v", cfgErr)
	}
	if cfg == nil {
		t.Error("GetPoolConfig() returned nil")
	}

	if savePoolErr := ss.SavePoolConfig(ctx, &PoolConfig{}); savePoolErr != nil {
		t.Errorf("SavePoolConfig() error = %v", savePoolErr)
	}

	pools, listErr := ss.ListPools(ctx)
	if listErr != nil {
		t.Errorf("ListPools() error = %v", listErr)
	}
	if pools == nil {
		t.Error("ListPools() returned nil")
	}

	if updateErr := ss.UpdatePoolState(ctx, "default", 5, 3); updateErr != nil {
		t.Errorf("UpdatePoolState() error = %v", updateErr)
	}
}

func TestCoordinatorInterface(t *testing.T) {
	var c Coordinator = &mockCoordinator{leader: true}

	ctx := context.Background()

	if err := c.Start(ctx); err != nil {
		t.Errorf("Start() error = %v", err)
	}

	if !c.IsLeader() {
		t.Error("IsLeader() = false, want true")
	}

	if err := c.Stop(); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

func TestRunnerSpec_AllFields(t *testing.T) {
	spec := RunnerSpec{
		RunID:         "run-123",
		JobID:         "job-456",
		Repo:          "owner/repo",
		Labels:        []string{"self-hosted", "linux"},
		InstanceType:  "t4g.medium",
		InstanceTypes: []string{"t4g.medium", "t4g.large"},
		Spot:          true,
		Private:       false,
		Pool:          "default",
		OS:            "linux",
		Arch:          "arm64",
		Environment:   "production",
		Region:        "us-east-1",
		ForceOnDemand: false,
		RetryCount:    1,
		SubnetID:      "subnet-123",
		CPUCores:      4,
		MemoryGiB:     8.0,
		StorageGiB:    50,
		JITToken:      "jit-token-xyz",
		CacheToken:    "cache-token-abc",
		RunnerGroup:   "default-group",
	}

	if spec.RunID != "run-123" {
		t.Errorf("RunnerSpec.RunID = %q, want %q", spec.RunID, "run-123")
	}
	if spec.JobID != "job-456" {
		t.Errorf("RunnerSpec.JobID = %q, want %q", spec.JobID, "job-456")
	}
	if len(spec.Labels) != 2 {
		t.Errorf("RunnerSpec.Labels length = %d, want 2", len(spec.Labels))
	}
	if spec.CPUCores != 4 {
		t.Errorf("RunnerSpec.CPUCores = %d, want 4", spec.CPUCores)
	}
	if spec.MemoryGiB != 8.0 {
		t.Errorf("RunnerSpec.MemoryGiB = %f, want 8.0", spec.MemoryGiB)
	}
}

func TestRunnerResult_Fields(t *testing.T) {
	result := RunnerResult{
		RunnerIDs: []string{"runner-1", "runner-2"},
		ProviderData: map[string]string{
			"instance_type": "t4g.medium",
			"spot":          "true",
		},
	}

	if len(result.RunnerIDs) != 2 {
		t.Errorf("RunnerResult.RunnerIDs length = %d, want 2", len(result.RunnerIDs))
	}
	if result.ProviderData["instance_type"] != "t4g.medium" {
		t.Errorf("RunnerResult.ProviderData[instance_type] = %q, want t4g.medium",
			result.ProviderData["instance_type"])
	}
}

func TestRunnerState_Fields(t *testing.T) {
	now := time.Now()
	state := RunnerState{
		RunnerID:     "runner-1",
		State:        stateRunning,
		InstanceType: "c7g.xlarge",
		LaunchTime:   now,
		ProviderData: map[string]string{
			"az": "us-east-1a",
		},
	}

	if state.RunnerID != "runner-1" {
		t.Errorf("RunnerState.RunnerID = %q, want runner-1", state.RunnerID)
	}
	if state.State != stateRunning {
		t.Errorf("RunnerState.State = %q, want %s", state.State, stateRunning)
	}
	if !state.LaunchTime.Equal(now) {
		t.Errorf("RunnerState.LaunchTime = %v, want %v", state.LaunchTime, now)
	}
}

func TestPoolRunner_Fields(t *testing.T) {
	now := time.Now()
	runner := PoolRunner{
		RunnerID:     "pool-runner-1",
		State:        stateRunning,
		InstanceType: "t4g.medium",
		LaunchTime:   now,
		IdleSince:    now.Add(-10 * time.Minute),
	}

	if runner.RunnerID != "pool-runner-1" {
		t.Errorf("PoolRunner.RunnerID = %q, want pool-runner-1", runner.RunnerID)
	}
	if runner.State != stateRunning {
		t.Errorf("PoolRunner.State = %q, want %s", runner.State, stateRunning)
	}
	if runner.IdleSince.After(runner.LaunchTime) {
		t.Error("PoolRunner.IdleSince should be before LaunchTime")
	}
}

func TestStoreConfigRequest_Fields(t *testing.T) {
	req := StoreConfigRequest{
		RunnerID:   "runner-1",
		JobID:      "job-1",
		RunID:      "run-1",
		Repo:       "owner/repo",
		Labels:     []string{"self-hosted"},
		JITToken:   "token",
		CacheToken: "cache",
		CacheURL:   "https://cache.example.com",
	}

	if req.RunnerID != "runner-1" {
		t.Errorf("StoreConfigRequest.RunnerID = %q, want runner-1", req.RunnerID)
	}
	if len(req.Labels) != 1 {
		t.Errorf("StoreConfigRequest.Labels length = %d, want 1", len(req.Labels))
	}
	if req.CacheURL != "https://cache.example.com" {
		t.Errorf("StoreConfigRequest.CacheURL = %q, want https://cache.example.com", req.CacheURL)
	}
}

func TestJob_Fields(t *testing.T) {
	now := time.Now()
	job := Job{
		JobID:        "job-123",
		RunID:        "run-456",
		Repo:         "owner/repo",
		InstanceID:   "i-12345",
		InstanceType: "t4g.medium",
		Pool:         "default",
		Private:      true,
		Spot:         true,
		RunnerSpec:   "2cpu-linux-arm64",
		RetryCount:   0,
		Status:       stateRunning,
		CreatedAt:    now,
		CompletedAt:  now.Add(time.Hour),
	}

	if job.JobID != "job-123" {
		t.Errorf("Job.JobID = %q, want job-123", job.JobID)
	}
	if job.InstanceID != "i-12345" {
		t.Errorf("Job.InstanceID = %q, want i-12345", job.InstanceID)
	}
	if !job.Private {
		t.Error("Job.Private = false, want true")
	}
	if !job.Spot {
		t.Error("Job.Spot = false, want true")
	}
	if job.Status != stateRunning {
		t.Errorf("Job.Status = %q, want %s", job.Status, stateRunning)
	}
}

func TestPoolConfig_Fields(t *testing.T) {
	config := PoolConfig{
		PoolName:           "default",
		InstanceType:       "t4g.medium",
		DesiredRunning:     5,
		DesiredStopped:     10,
		IdleTimeoutMinutes: 30,
		Schedules: []PoolSchedule{
			{
				Name:           "weekday-peak",
				StartHour:      9,
				EndHour:        18,
				DaysOfWeek:     []int{1, 2, 3, 4, 5},
				DesiredRunning: 10,
				DesiredStopped: 5,
			},
		},
		Environment: "production",
		Region:      "us-east-1",
	}

	if config.PoolName != "default" {
		t.Errorf("PoolConfig.PoolName = %q, want default", config.PoolName)
	}
	if config.DesiredRunning != 5 {
		t.Errorf("PoolConfig.DesiredRunning = %d, want 5", config.DesiredRunning)
	}
	if config.IdleTimeoutMinutes != 30 {
		t.Errorf("PoolConfig.IdleTimeoutMinutes = %d, want 30", config.IdleTimeoutMinutes)
	}
	if len(config.Schedules) != 1 {
		t.Errorf("PoolConfig.Schedules length = %d, want 1", len(config.Schedules))
	}
}

func TestPoolSchedule_Fields(t *testing.T) {
	schedule := PoolSchedule{
		Name:           "weekend",
		StartHour:      10,
		EndHour:        16,
		DaysOfWeek:     []int{0, 6},
		DesiredRunning: 2,
		DesiredStopped: 0,
	}

	if schedule.Name != "weekend" {
		t.Errorf("PoolSchedule.Name = %q, want weekend", schedule.Name)
	}
	if schedule.StartHour != 10 {
		t.Errorf("PoolSchedule.StartHour = %d, want 10", schedule.StartHour)
	}
	if schedule.EndHour != 16 {
		t.Errorf("PoolSchedule.EndHour = %d, want 16", schedule.EndHour)
	}
	if len(schedule.DaysOfWeek) != 2 {
		t.Errorf("PoolSchedule.DaysOfWeek length = %d, want 2", len(schedule.DaysOfWeek))
	}
}
