package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Shavakan/runs-fleet/internal/handler"
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
}

// ProcessJobDirect processes a job immediately without SQS.
// Returns true if instance was assigned (warm pool or new fleet), false if job was already claimed or processing failed.
func (p *DirectProcessor) ProcessJobDirect(ctx context.Context, job *queue.JobMessage) bool {
	if p.Fleet == nil {
		directLog.Error("fleet manager nil", slog.Int64(logging.KeyJobID, job.JobID))
		return false
	}

	if p.DB != nil && p.DB.HasJobsTable() {
		if err := p.DB.ClaimJob(ctx, job.JobID); err != nil {
			if errors.Is(err, db.ErrJobAlreadyClaimed) {
				return false
			}
			directLog.Warn("job claim failed, deferring to queue",
				slog.Int64(logging.KeyJobID, job.JobID),
				slog.String("error", err.Error()))
			if p.Metrics != nil {
				if err := p.Metrics.PublishJobClaimFailure(ctx); err != nil {
					directLog.Error("job claim failure metric failed", slog.String("error", err.Error()))
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
			directLog.Error("warm pool assignment failed",
				slog.Int64(logging.KeyJobID, job.JobID),
				slog.String("error", err.Error()))
		} else if result.Assigned {
			if p.Metrics != nil {
				_ = p.Metrics.PublishWarmPoolHit(ctx)
			}
			directLog.Info("job assigned to warm pool",
				slog.Int64(logging.KeyJobID, job.JobID),
				slog.String(logging.KeyInstanceID, result.InstanceID))
			return true
		}
	}

	spec := &fleet.LaunchSpec{
		RunID:         job.RunID,
		InstanceType:  job.InstanceType,
		InstanceTypes: job.InstanceTypes,
		SubnetID:      SelectSubnet(p.Config, p.SubnetIndex, job.PublicIP),
		Spot:          job.Spot,
		Pool:          job.Pool,
		Repo:          job.Repo,
		ForceOnDemand: job.ForceOnDemand,
		RetryCount:    job.RetryCount,
		Region:        job.Region,
		Environment:   job.Environment,
		OS:            job.OS,
		Arch:          job.Arch,
		StorageGiB:    job.StorageGiB,
	}

	instanceIDs, err := CreateFleetWithRetry(ctx, p.Fleet, spec)
	if err != nil {
		directLog.Error("fleet creation failed",
			slog.Int64(logging.KeyJobID, job.JobID),
			slog.String("error", err.Error()))
		if p.DB != nil && p.DB.HasJobsTable() {
			if err := p.DB.DeleteJobClaim(ctx, job.JobID); err != nil {
				directLog.Error("job claim delete failed",
					slog.Int64(logging.KeyJobID, job.JobID),
					slog.String("error", err.Error()))
			}
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
			}
			if err := p.DB.SaveJob(ctx, jobRecord); err != nil {
				directLog.Error("job record save failed",
					slog.String(logging.KeyInstanceID, instanceID),
					slog.String("error", err.Error()))
			}
		}
	}

	if p.Runner != nil {
		for _, instanceID := range instanceIDs {
			label := handler.BuildRunnerLabel(job)
			prepareReq := runner.PrepareRunnerRequest{
				InstanceID: instanceID,
				JobID:      fmt.Sprintf("%d", job.JobID),
				RunID:      fmt.Sprintf("%d", job.RunID),
				Repo:       job.Repo,
				Labels:     []string{label},
				Pool:       job.Pool,
				Arch:       job.Arch,
			}
			if err := p.Runner.PrepareRunner(ctx, prepareReq); err != nil {
				directLog.Error("runner config preparation failed",
					slog.String(logging.KeyInstanceID, instanceID),
					slog.String("error", err.Error()))
			}
		}
	}

	for _, instanceID := range instanceIDs {
		p.Pool.MarkInstanceBusy(instanceID)
	}

	if p.Metrics != nil {
		if err := p.Metrics.PublishFleetSizeIncrement(ctx); err != nil {
			directLog.Error("fleet size increment metric failed", slog.String("error", err.Error()))
		}
	}

	directLog.Info("instances launched",
		slog.Int64(logging.KeyRunID, job.RunID),
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
			processor.ProcessJobDirect(directCtx, jobMsg)
		}()
	default:
		// At capacity, job will be processed via queue
	}
}
