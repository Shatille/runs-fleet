package ec2

import (
	"context"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	"github.com/aws/aws-sdk-go-v2/aws"
)

// StateStore implements provider.StateStore using DynamoDB.
type StateStore struct {
	client *db.Client
}

// NewStateStore creates a DynamoDB-backed state store.
func NewStateStore(awsCfg aws.Config, poolsTable, jobsTable string) *StateStore {
	return &StateStore{
		client: db.NewClient(awsCfg, poolsTable, jobsTable),
	}
}

// NewStateStoreFromClient creates a state store from an existing db.Client.
func NewStateStoreFromClient(client *db.Client) *StateStore {
	return &StateStore{client: client}
}

// Client returns the underlying db.Client for advanced operations.
func (s *StateStore) Client() *db.Client {
	return s.client
}

// SaveJob saves a job record.
func (s *StateStore) SaveJob(ctx context.Context, job *provider.Job) error {
	record := &db.JobRecord{
		JobID:        job.JobID,
		RunID:        job.RunID,
		Repo:         job.Repo,
		InstanceID:   job.InstanceID,
		InstanceType: job.InstanceType,
		Pool:         job.Pool,
		Spot:         job.Spot,
		RetryCount:   job.RetryCount,
	}
	return s.client.SaveJob(ctx, record)
}

// GetJob retrieves a job by runner ID.
// Note: Only returns jobs with status "running" (db.GetJobByInstance filters non-running jobs).
func (s *StateStore) GetJob(ctx context.Context, runnerID string) (*provider.Job, error) {
	info, err := s.client.GetJobByInstance(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil
	}

	return &provider.Job{
		JobID:        info.JobID,
		RunID:        info.RunID,
		Repo:         info.Repo,
		InstanceID:   runnerID,
		InstanceType: info.InstanceType,
		Pool:         info.Pool,
		Spot:         info.Spot,
		RetryCount:   info.RetryCount,
		Status:       "running", // db.GetJobByInstance only returns running jobs
	}, nil
}

// MarkJobComplete marks a job as complete.
func (s *StateStore) MarkJobComplete(ctx context.Context, jobID int64, status string, exitCode, duration int) error {
	return s.client.MarkJobComplete(ctx, jobID, status, exitCode, duration)
}

// MarkJobTerminating marks a job as terminating.
func (s *StateStore) MarkJobTerminating(ctx context.Context, instanceID string) error {
	return s.client.MarkInstanceTerminating(ctx, instanceID)
}

// GetPoolConfig retrieves pool configuration.
func (s *StateStore) GetPoolConfig(ctx context.Context, poolName string) (*provider.PoolConfig, error) {
	cfg, err := s.client.GetPoolConfig(ctx, poolName)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}

	return convertPoolConfig(cfg), nil
}

// SavePoolConfig saves pool configuration.
func (s *StateStore) SavePoolConfig(ctx context.Context, config *provider.PoolConfig) error {
	dbConfig := &db.PoolConfig{
		PoolName:           config.PoolName,
		InstanceType:       config.InstanceType,
		DesiredRunning:     config.DesiredRunning,
		DesiredStopped:     config.DesiredStopped,
		IdleTimeoutMinutes: config.IdleTimeoutMinutes,
		Environment:        config.Environment,
		Region:             config.Region,
	}

	for _, s := range config.Schedules {
		dbConfig.Schedules = append(dbConfig.Schedules, db.PoolSchedule{
			Name:           s.Name,
			StartHour:      s.StartHour,
			EndHour:        s.EndHour,
			DaysOfWeek:     s.DaysOfWeek,
			DesiredRunning: s.DesiredRunning,
			DesiredStopped: s.DesiredStopped,
		})
	}

	return s.client.SavePoolConfig(ctx, dbConfig)
}

// ListPools returns all pool names.
func (s *StateStore) ListPools(ctx context.Context) ([]string, error) {
	return s.client.ListPools(ctx)
}

// UpdatePoolState updates the current state of a pool.
func (s *StateStore) UpdatePoolState(ctx context.Context, poolName string, running, stopped int) error {
	return s.client.UpdatePoolState(ctx, poolName, running, stopped)
}

// UpdateJobMetrics updates job timing metrics.
func (s *StateStore) UpdateJobMetrics(ctx context.Context, jobID int64, startedAt, completedAt time.Time) error {
	return s.client.UpdateJobMetrics(ctx, jobID, startedAt, completedAt)
}

// convertPoolConfig converts db.PoolConfig to provider.PoolConfig.
func convertPoolConfig(cfg *db.PoolConfig) *provider.PoolConfig {
	result := &provider.PoolConfig{
		PoolName:           cfg.PoolName,
		InstanceType:       cfg.InstanceType,
		DesiredRunning:     cfg.DesiredRunning,
		DesiredStopped:     cfg.DesiredStopped,
		IdleTimeoutMinutes: cfg.IdleTimeoutMinutes,
		Environment:        cfg.Environment,
		Region:             cfg.Region,
	}

	for _, s := range cfg.Schedules {
		result.Schedules = append(result.Schedules, provider.PoolSchedule{
			Name:           s.Name,
			StartHour:      s.StartHour,
			EndHour:        s.EndHour,
			DaysOfWeek:     s.DaysOfWeek,
			DesiredRunning: s.DesiredRunning,
			DesiredStopped: s.DesiredStopped,
		})
	}

	return result
}

// Ensure StateStore implements provider.StateStore.
var _ provider.StateStore = (*StateStore)(nil)
