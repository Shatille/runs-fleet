// Package provider defines interfaces for compute backends (EC2, K8s).
package provider

import (
	"context"
	"time"
)

// Provider manages compute resources for runner instances.
type Provider interface {
	// CreateRunner provisions compute for a job, returns runner identifiers.
	CreateRunner(ctx context.Context, spec *RunnerSpec) (*RunnerResult, error)

	// TerminateRunner removes compute resources.
	TerminateRunner(ctx context.Context, runnerID string) error

	// DescribeRunner returns current state of a runner.
	DescribeRunner(ctx context.Context, runnerID string) (*RunnerState, error)

	// Name returns provider identifier ("ec2" or "k8s").
	Name() string
}

// PoolProvider manages warm pool operations.
type PoolProvider interface {
	// ListPoolRunners returns all runners in a pool.
	ListPoolRunners(ctx context.Context, poolName string) ([]PoolRunner, error)

	// StartRunners activates stopped/scaled-down runners.
	StartRunners(ctx context.Context, runnerIDs []string) error

	// StopRunners pauses runners without terminating.
	StopRunners(ctx context.Context, runnerIDs []string) error

	// TerminateRunners removes runners from pool.
	TerminateRunners(ctx context.Context, runnerIDs []string) error

	// MarkRunnerBusy marks a runner as busy (has an assigned job).
	MarkRunnerBusy(runnerID string)

	// MarkRunnerIdle marks a runner as idle (no assigned job).
	MarkRunnerIdle(runnerID string)
}

// ConfigStore manages runner configuration delivery.
type ConfigStore interface {
	// StoreRunnerConfig saves config for agent retrieval.
	StoreRunnerConfig(ctx context.Context, req *StoreConfigRequest) error

	// DeleteRunnerConfig cleans up config after job.
	DeleteRunnerConfig(ctx context.Context, runnerID string) error
}

// StateStore manages job and pool state persistence.
type StateStore interface {
	// Job operations
	SaveJob(ctx context.Context, job *Job) error
	GetJob(ctx context.Context, runnerID string) (*Job, error)
	MarkJobComplete(ctx context.Context, runnerID, status string, exitCode, duration int) error
	MarkJobTerminating(ctx context.Context, runnerID string) error
	UpdateJobMetrics(ctx context.Context, runnerID string, startedAt, completedAt time.Time) error

	// Pool operations
	GetPoolConfig(ctx context.Context, poolName string) (*PoolConfig, error)
	SavePoolConfig(ctx context.Context, config *PoolConfig) error
	ListPools(ctx context.Context) ([]string, error)
	UpdatePoolState(ctx context.Context, poolName string, running, stopped int) error
}

// Coordinator provides distributed coordination for multi-instance deployments.
type Coordinator interface {
	// IsLeader returns true if this instance is the leader.
	IsLeader() bool

	// Start begins leader election.
	Start(ctx context.Context) error

	// Stop ends leader election.
	Stop() error
}

// RunnerSpec defines parameters for creating a runner.
type RunnerSpec struct {
	RunID         string
	JobID         string
	Repo          string
	Labels        []string
	InstanceType  string   // Primary instance type (EC2-specific)
	InstanceTypes []string // Multiple types for spot diversification (EC2-specific)
	Spot          bool
	Private       bool
	Pool          string
	OS            string // linux, windows
	Arch          string // arm64, x64
	Environment   string
	Region        string
	ForceOnDemand bool
	RetryCount    int
	SubnetID      string // Network isolation (EC2: subnet, K8s: namespace)

	// K8s-native resource requests (used by Karpenter to provision nodes)
	CPUCores   int     // CPU cores requested (e.g., 4)
	MemoryGiB  float64 // Memory in GiB requested (e.g., 8)
	StorageGiB int     // Ephemeral storage in GiB (e.g., 50)
}

// RunnerResult contains the result of creating a runner.
type RunnerResult struct {
	RunnerIDs    []string          // Instance IDs or Pod names
	ProviderData map[string]string // Provider-specific metadata
}

// RunnerState represents the current state of a runner.
type RunnerState struct {
	RunnerID     string
	State        string // pending, running, stopped, terminated
	InstanceType string
	LaunchTime   time.Time
	ProviderData map[string]string
}

// PoolRunner represents a runner in a warm pool.
type PoolRunner struct {
	RunnerID     string
	State        string // pending, running, stopped
	InstanceType string
	LaunchTime   time.Time
	IdleSince    time.Time
}

// StoreConfigRequest contains parameters for storing runner config.
type StoreConfigRequest struct {
	RunnerID   string
	JobID      string
	RunID      string
	Repo       string
	Labels     []string
	JITToken   string
	CacheToken string
	CacheURL   string
}

// Job represents a workflow job record.
type Job struct {
	JobID        string
	RunID        string
	Repo         string
	InstanceID   string
	InstanceType string
	Pool         string
	Private      bool
	Spot         bool
	RunnerSpec   string
	RetryCount   int
	Status       string
	CreatedAt    time.Time
	CompletedAt  time.Time
}

// PoolConfig represents pool configuration.
type PoolConfig struct {
	PoolName           string
	InstanceType       string
	DesiredRunning     int
	DesiredStopped     int
	IdleTimeoutMinutes int
	Schedules          []PoolSchedule
	Environment        string
	Region             string
}

// PoolSchedule defines time-based pool sizing.
type PoolSchedule struct {
	Name           string
	StartHour      int
	EndHour        int
	DaysOfWeek     []int
	DesiredRunning int
	DesiredStopped int
}
