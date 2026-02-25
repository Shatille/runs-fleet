package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Shavakan/runs-fleet/internal/handler"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
)

var warmPoolLog = logging.WithComponent(logging.LogTypePool, "warmpool-assigner")

// PoolManager defines warm pool operations for instance assignment.
type PoolManager interface {
	ClaimAndStartPoolInstance(ctx context.Context, poolName string, jobID int64, repo string, spec *fleet.FlexibleSpec) (*pools.AvailableInstance, error)
	StopPoolInstance(ctx context.Context, instanceID string) error
}

// RunnerPreparer defines runner configuration operations.
type RunnerPreparer interface {
	PrepareRunner(ctx context.Context, req runner.PrepareRunnerRequest) error
}

// JobDBClient defines database operations for job records.
type JobDBClient interface {
	HasJobsTable() bool
	SaveJob(ctx context.Context, job *db.JobRecord) error
	ReleaseInstanceClaim(ctx context.Context, instanceID string, jobID int64) error
}

// WarmPoolAssigner handles assignment of jobs to warm pool instances.
type WarmPoolAssigner struct {
	Pool   PoolManager
	Runner RunnerPreparer
	DB     JobDBClient
}

// WarmPoolResult contains the result of a warm pool assignment attempt.
type WarmPoolResult struct {
	Assigned   bool
	InstanceID string
}

// TryAssignToWarmPool attempts to assign a job to an available warm pool instance.
// Returns (result, nil) if assignment succeeded or no instance available.
// Returns (nil, error) only on unexpected failures.
func (w *WarmPoolAssigner) TryAssignToWarmPool(ctx context.Context, job *queue.JobMessage) (*WarmPoolResult, error) {
	if job.Pool == "" {
		return &WarmPoolResult{Assigned: false}, nil
	}

	if w.Pool == nil || w.Runner == nil {
		return &WarmPoolResult{Assigned: false}, nil
	}

	// Build FlexibleSpec from job message for multi-spec pool matching
	// If no spec fields are set, spec is nil (legacy: any instance matches)
	var spec *fleet.FlexibleSpec
	if job.CPUMin > 0 || job.CPUMax > 0 || job.RAMMin > 0 || job.RAMMax > 0 || len(job.Families) > 0 || job.Gen > 0 || job.Arch != "" {
		spec = &fleet.FlexibleSpec{
			CPUMin:   job.CPUMin,
			CPUMax:   job.CPUMax,
			RAMMin:   job.RAMMin,
			RAMMax:   job.RAMMax,
			Arch:     job.Arch,
			Families: job.Families,
			Gen:      job.Gen,
		}
	}

	// Atomically claim and start a stopped instance to prevent race conditions
	// Uses DynamoDB distributed locking to prevent multiple orchestrators from claiming the same instance
	instance, err := w.Pool.ClaimAndStartPoolInstance(ctx, job.Pool, job.JobID, job.Repo, spec)
	if err != nil {
		if errors.Is(err, pools.ErrNoAvailableInstance) {
			return &WarmPoolResult{Assigned: false}, nil
		}
		return nil, fmt.Errorf("failed to claim pool instance: %w", err)
	}

	warmPoolLog.Info("instance claimed",
		slog.String(logging.KeyInstanceID, instance.InstanceID),
		slog.Int64(logging.KeyJobID, job.JobID))

	// Prepare runner config for this instance (updates SSM)
	// Instance is starting but agent won't read SSM until fully booted (~30s)
	label := handler.BuildRunnerLabel(job)
	prepareReq := runner.PrepareRunnerRequest{
		InstanceID: instance.InstanceID,
		JobID:      fmt.Sprintf("%d", job.JobID),
		RunID:      fmt.Sprintf("%d", job.RunID),
		Repo:       job.Repo,
		Labels:     []string{label},
		Pool:       job.Pool,
		Conditions: BuildRunnerConditions(job),
	}

	if err := w.Runner.PrepareRunner(ctx, prepareReq); err != nil {
		warmPoolLog.Error("runner config preparation failed",
			slog.String(logging.KeyInstanceID, instance.InstanceID),
			slog.String("error", err.Error()))
		// Instance already started - stop it since it has no valid config
		// Retry stop to ensure instance doesn't run with stale SSM config
		stopped := false
		for attempt := 0; attempt < 3; attempt++ {
			if ctx.Err() != nil {
				break
			}
			if stopErr := w.Pool.StopPoolInstance(ctx, instance.InstanceID); stopErr != nil {
				warmPoolLog.Error("instance stop failed after SSM prep failure",
					slog.String(logging.KeyInstanceID, instance.InstanceID),
					slog.Int("attempt", attempt+1),
					slog.String("error", stopErr.Error()))
				continue
			}
			stopped = true
			break
		}
		if !stopped {
			warmPoolLog.Error("instance may have invalid config - manual cleanup required",
				slog.String(logging.KeyInstanceID, instance.InstanceID))
		}
		// Release the distributed claim since assignment failed
		if w.DB != nil {
			if releaseErr := w.DB.ReleaseInstanceClaim(ctx, instance.InstanceID, job.JobID); releaseErr != nil {
				warmPoolLog.Error("instance claim release failed",
					slog.String(logging.KeyInstanceID, instance.InstanceID),
					slog.String("error", releaseErr.Error()))
			}
		}
		return &WarmPoolResult{Assigned: false}, nil
	}

	// Save job record - required for job tracking and spot interruption handling
	if w.DB != nil && w.DB.HasJobsTable() {
		jobRecord := &db.JobRecord{
			JobID:        job.JobID,
			RunID:        job.RunID,
			Repo:         job.Repo,
			InstanceID:   instance.InstanceID,
			InstanceType: instance.InstanceType,
			Pool:         job.Pool,
			Spot:         job.Spot,
			RetryCount:   job.RetryCount,
			WarmPoolHit:  true,
		}
		if err := w.DB.SaveJob(ctx, jobRecord); err != nil {
			warmPoolLog.Error("job record save failed",
				slog.String(logging.KeyInstanceID, instance.InstanceID),
				slog.String("error", err.Error()))
			// Release claim first to allow another orchestrator to retry with this instance
			if releaseErr := w.DB.ReleaseInstanceClaim(ctx, instance.InstanceID, job.JobID); releaseErr != nil {
				warmPoolLog.Error("instance claim release failed",
					slog.String(logging.KeyInstanceID, instance.InstanceID),
					slog.String("error", releaseErr.Error()))
			}
			// Job record is required for tracking - stop instance and fall back to cold start
			stopped := false
			for attempt := 0; attempt < 3; attempt++ {
				if ctx.Err() != nil {
					break
				}
				if stopErr := w.Pool.StopPoolInstance(ctx, instance.InstanceID); stopErr != nil {
					warmPoolLog.Error("instance stop failed after DB save failure",
						slog.String(logging.KeyInstanceID, instance.InstanceID),
						slog.Int("attempt", attempt+1),
						slog.String("error", stopErr.Error()))
					continue
				}
				stopped = true
				break
			}
			if !stopped {
				warmPoolLog.Error("instance may be running without job record - manual cleanup required",
					slog.String(logging.KeyInstanceID, instance.InstanceID))
			}
			return &WarmPoolResult{Assigned: false}, nil
		}
	}

	warmPoolLog.Info("job assigned to warm pool",
		slog.Int64(logging.KeyJobID, job.JobID),
		slog.String(logging.KeyInstanceID, instance.InstanceID))
	return &WarmPoolResult{
		Assigned:   true,
		InstanceID: instance.InstanceID,
	}, nil
}
