package worker

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
)

var directLog = logging.WithComponent(logging.LogTypeQueue, "direct")

// DirectProcessor handles immediate job processing from webhooks.
type DirectProcessor struct {
	Fleet       *fleet.Manager
	Pool        *pools.Manager
	Metrics     metrics.Publisher
	Runner      *runner.Manager
	DB          *db.Client
	Config      *config.Config
	SubnetIndex *uint64

	// CreateFleetFn allows injection of a custom fleet creator for testing.
	// If nil, defaults to CreateFleetWithRetry against the embedded Fleet manager.
	CreateFleetFn func(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)

	// PrepareRunnersFn allows injection of a custom runner preparer for testing.
	// If nil, defaults to PrepareRunners against the embedded Runner manager.
	PrepareRunnersFn func(ctx context.Context, job *queue.JobMessage, instanceIDs []string) []string
}

func (p *DirectProcessor) createFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error) {
	if p.CreateFleetFn != nil {
		return p.CreateFleetFn(ctx, spec)
	}
	return CreateFleetWithRetry(ctx, p.Fleet, spec)
}

func (p *DirectProcessor) prepareRunners(ctx context.Context, job *queue.JobMessage, instanceIDs []string) []string {
	if p.PrepareRunnersFn != nil {
		return p.PrepareRunnersFn(ctx, job, instanceIDs)
	}
	if p.Runner == nil {
		return nil
	}
	return PrepareRunners(ctx, p.Runner, job, instanceIDs)
}

// ProcessJobDirect processes a job immediately without SQS.
// Returns true if instance was assigned (warm pool or new fleet), false if job was already claimed or processing failed.
func (p *DirectProcessor) ProcessJobDirect(ctx context.Context, job *queue.JobMessage) bool {
	if p.Fleet == nil {
		directLog.ErrorContext(ctx, "fleet manager nil")
		return false
	}

	if p.DB != nil && p.DB.HasJobsTable() {
		if err := p.DB.ClaimJob(ctx, job.JobID); err != nil {
			if errors.Is(err, db.ErrJobAlreadyClaimed) {
				return false
			}
			directLog.WarnContext(ctx, "job claim failed, deferring to queue",
				slog.String("error", err.Error()))
			if p.Metrics != nil {
				if err := p.Metrics.PublishJobClaimFailure(ctx); err != nil {
					directLog.ErrorContext(ctx, "job claim failure metric failed", slog.String("error", err.Error()))
				}
			}
			return false
		}
	}

	if job.Pool != "" {
		assigner := &WarmPoolAssigner{
			Pool:   p.Pool,
			Runner: p.Runner,
			DB:     p.DB,
		}
		result, err := assigner.TryAssignToWarmPool(ctx, job)
		if err != nil {
			directLog.ErrorContext(ctx, "warm pool assignment failed",
				slog.String("error", err.Error()))
		} else if result.Assigned {
			if p.Metrics != nil {
				_ = p.Metrics.PublishWarmPoolHit(ctx)
			}
			directLog.InfoContext(ctx, "job assigned to warm pool",
				slog.String(logging.KeyInstanceID, result.InstanceID))
			return true
		}
	}

	spec := &fleet.LaunchSpec{
		RunID:         job.RunID,
		InstanceType:  job.InstanceType,
		InstanceTypes: job.InstanceTypes,
		SubnetID:      SelectSubnet(p.Config, p.SubnetIndex),
		Spot:          job.Spot,
		Pool:          job.Pool,
		Repo:          job.Repo,
		ForceOnDemand: job.ForceOnDemand,
		RetryCount:    job.RetryCount,
		Arch:          job.Arch,
		StorageGiB:    job.StorageGiB,
		Conditions:    BuildRunnerConditions(job),
	}

	instanceIDs, err := p.createFleet(ctx, spec)
	if err != nil {
		directLog.ErrorContext(ctx, "fleet creation failed",
			slog.String("error", err.Error()))
		if p.DB != nil && p.DB.HasJobsTable() {
			// Release the claim on a fresh context; the job ctx may already be expired.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
			cleanupCtx = logging.ContextWithJob(cleanupCtx, job.JobID, job.RunID, job.Repo)
			if err := p.DB.DeleteJobClaim(cleanupCtx, job.JobID); err != nil {
				directLog.ErrorContext(cleanupCtx, "job claim delete failed",
					slog.String("error", err.Error()))
			}
			cancel()
		}
		return false
	}

	if p.DB != nil && p.DB.HasJobsTable() {
		for _, instanceID := range instanceIDs {
			jobRecord := &db.JobRecord{
				JobID:        job.JobID,
				RunID:        job.RunID,
				Repo:         job.Repo,
				InstanceID:   instanceID,
				InstanceType: job.InstanceType,
				Pool:         job.Pool,
				Spot:         job.Spot,
				RetryCount:   job.RetryCount,
				Traceparent:  job.Traceparent,
			}
			if err := p.DB.SaveJob(ctx, jobRecord); err != nil {
				directLog.ErrorContext(ctx, "job record save failed",
					slog.String(logging.KeyInstanceID, instanceID),
					slog.String("error", err.Error()))
			}
		}
	}

	if failed := p.prepareRunners(ctx, job, instanceIDs); len(failed) > 0 {
		// Config could not be written (e.g. SSM throttled); fail the direct path
		// so the always-enqueued SQS copy of this job is retried by the worker.
		// Release the claim so the worker can re-claim. The config-less instances
		// are reaped by housekeeping.
		if p.DB != nil && p.DB.HasJobsTable() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
			cleanupCtx = logging.ContextWithJob(cleanupCtx, job.JobID, job.RunID, job.Repo)
			if err := p.DB.DeleteJobClaim(cleanupCtx, job.JobID); err != nil {
				directLog.ErrorContext(cleanupCtx, "job claim delete failed after prep failure",
					slog.String("error", err.Error()))
			}
			cancel()
		}
		directLog.ErrorContext(ctx, "runner preparation failed; deferring job to queue",
			slog.Int("failed_instances", len(failed)))
		return false
	}

	if p.Pool != nil {
		for _, instanceID := range instanceIDs {
			p.Pool.MarkInstanceBusy(instanceID)
		}
	}

	if p.Metrics != nil {
		if err := p.Metrics.PublishFleetSizeIncrement(ctx); err != nil {
			directLog.ErrorContext(ctx, "fleet size increment metric failed", slog.String("error", err.Error()))
		}
	}

	directLog.InfoContext(ctx, "instances launched",
		slog.Int(logging.KeyCount, len(instanceIDs)))
	return true
}

// TryDirectProcessing attempts to process a job directly if capacity is available.
// Note: ctx is ignored - we use context.Background() since the goroutine must outlive the HTTP request.
func TryDirectProcessing(_ context.Context, processor *DirectProcessor, sem chan struct{}, jobMsg *queue.JobMessage) {
	if processor == nil || sem == nil {
		return
	}
	select {
	case sem <- struct{}{}:
		go func() {
			defer func() {
				if r := recover(); r != nil {
					directLog.Error("panic in direct processing",
						slog.Int64(logging.KeyJobID, jobMsg.JobID),
						slog.Any("panic", r))
				}
			}()
			defer func() { <-sem }()
			// Use Background context - parent ctx may be canceled when HTTP handler returns
			directCtx, cancel := context.WithTimeout(context.Background(), config.MessageProcessTimeout)
			defer cancel()
			// Stash task identity so downstream logs (including deep AWS calls)
			// carry job_id/run_id/repo without restating them at each site.
			directCtx = logging.ContextWithJob(directCtx, jobMsg.JobID, jobMsg.RunID, jobMsg.Repo)
			processor.ProcessJobDirect(directCtx, jobMsg)
		}()
	default:
		// At capacity, job will be processed via queue
	}
}
