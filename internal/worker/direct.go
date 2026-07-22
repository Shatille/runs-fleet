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

	// Queue is the durable main queue. On fleet-creation failure the direct path
	// hands the job back here (a fresh send, since it holds no SQS receipt) so the
	// ec2-worker retries it with its on-demand fallback. Nil disables recovery,
	// in which case a failed direct attempt falls back to the always-enqueued SQS
	// copy via redelivery.
	Queue queue.Queue

	// WarmPoolAssigner allows injection of a custom assigner for testing.
	// If nil, creates a default WarmPoolAssigner from Pool, Runner, DB.
	WarmPoolAssigner WarmPoolAssignerInterface

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
		directLog.Error(ctx, "fleet manager nil")
		return false
	}

	if p.DB != nil && p.DB.HasJobsTable() {
		if err := p.DB.ClaimJob(ctx, job.JobID, job.RunID, job.Repo); err != nil {
			if errors.Is(err, db.ErrJobAlreadyClaimed) {
				return false
			}
			if errors.Is(err, db.ErrJobClaimExhausted) {
				// Lease exhausted. Defer to the always-enqueued SQS copy, whose
				// worker marks the job terminal and drops the message. Keeping the
				// terminal transition on the queue path avoids duplicating it here.
				directLog.Error(ctx, "job claim attempts exhausted; deferring terminal handling to queue")
				return false
			}
			directLog.Warn(ctx, "job claim failed, deferring to queue",
				slog.String("error", err.Error()))
			if p.Metrics != nil {
				if err := p.Metrics.PublishSchedulingFailure(ctx, schedulingFailureJobClaim); err != nil {
					directLog.Error(ctx, "job claim failure metric failed", slog.String("error", err.Error()))
				}
			}
			return false
		}
	}

	if job.Pool != "" {
		var assigner WarmPoolAssignerInterface
		if p.WarmPoolAssigner != nil {
			assigner = p.WarmPoolAssigner
		} else {
			assigner = &WarmPoolAssigner{
				Pool:   p.Pool,
				Runner: p.Runner,
				DB:     p.DB,
			}
		}
		result, err := assigner.TryAssignToWarmPool(ctx, job)
		if err != nil {
			// Non-terminal: a cold-start fleet creation follows below. Reserve ERROR
			// for that terminal path so warm-pool capacity blips don't page.
			directLog.Warn(ctx, "warm pool assignment failed; falling back to cold start",
				slog.String(logging.KeyPoolName, job.Pool),
				slog.String("error", err.Error()))
		} else if result.Assigned {
			if p.Metrics != nil {
				_ = p.Metrics.PublishJobAssigned(ctx, job.Pool, sourceWarmPool, job.Repo)
			}
			directLog.Info(ctx, "job assigned to warm pool",
				slog.String(logging.KeyInstanceID, result.InstanceID))
			return true
		}
	}

	spec := &fleet.LaunchSpec{
		RunID:         job.RunID,
		InstanceType:  job.InstanceType,
		InstanceTypes: job.InstanceTypes,
		SubnetID:      SelectSubnet(p.Config, p.SubnetIndex),
		SubnetIDs:     p.Config.SubnetIDs,
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
		// When the job can fall back to on-demand, this is a transient, self-healing
		// condition (WARN); reserve ERROR for the terminal give-up in recoverFleetFailure.
		if onDemandFallbackEligible(job) {
			directLog.Warn(ctx, "fleet creation failed; retrying on-demand",
				slog.String("error", err.Error()))
		} else {
			directLog.Error(ctx, "fleet creation failed; no fallback available",
				slog.String("error", err.Error()))
		}
		p.recoverFleetFailure(ctx, job)
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
				directLog.Error(ctx, "job record save failed",
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
				directLog.Error(cleanupCtx, "job claim delete failed after prep failure",
					slog.String("error", err.Error()))
			}
			cancel()
		}
		directLog.Error(ctx, "runner preparation failed; deferring job to queue",
			slog.Int("failed_instances", len(failed)))
		return false
	}

	if p.Pool != nil {
		for _, instanceID := range instanceIDs {
			p.Pool.MarkInstanceBusy(instanceID)
		}
	}

	if p.Metrics != nil {
		if err := p.Metrics.PublishJobAssigned(ctx, job.Pool, sourceColdStart, job.Repo); err != nil {
			directLog.Error(ctx, "job assigned metric failed", slog.String("error", err.Error()))
		}
	}

	directLog.Info(ctx, "instances launched",
		slog.Int(logging.KeyCount, len(instanceIDs)))
	return true
}

// recoverFleetFailure hands a job back to the durable queue after fleet creation
// fails on the direct path, instead of dropping it. Symmetric with the
// ec2-worker's handleOnDemandFallback: because the direct path holds no SQS
// receipt handle (it is webhook-triggered, not SQS-received), it can only SEND a
// fresh message, and only when the always-enqueued SQS copy may already be gone
// (the ec2-worker deletes it as already-claimed once direct wins the claim).
//
// Eligibility mirrors the ec2-worker: a spot job under the retry cap is requeued
// as on-demand (BuildOnDemandFallbackJob bumps RetryCount so the FIFO dedup id
// changes). When the job is no longer eligible (retry cap reached, or already
// on-demand and still failing) it is not silently dropped: the claim is marked
// terminal (status "error") so it surfaces in the admin UI and stale-job
// recovery instead of hanging until cancellation.
func (p *DirectProcessor) recoverFleetFailure(ctx context.Context, job *queue.JobMessage) {
	eligible := onDemandFallbackEligible(job)
	if eligible && p.Queue != nil {
		// Release the claim first so the requeued message can re-claim the job;
		// a leaked claim would make the ec2-worker treat the requeue as
		// already-claimed and drop it. Use a fresh context: the job ctx may
		// already be expired on a wedged connection.
		p.releaseClaim(job, "job claim delete failed before requeue")

		cleanupCtx, cancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
		defer cancel()
		cleanupCtx = logging.ContextWithJob(cleanupCtx, job.JobID, job.RunID, job.Repo)

		fallbackJob := BuildOnDemandFallbackJob(job)
		if sendErr := sendMessageWithRetry(cleanupCtx, p.Queue, fallbackJob); sendErr != nil {
			// Send failed: the SQS copy (if any survived) handles redelivery, and
			// the released claim lets it re-claim. Leave the claim released.
			directLog.Error(cleanupCtx, "direct fleet-failure requeue failed; deferring to SQS redelivery",
				slog.String("error", sendErr.Error()))
			return
		}
		directLog.Info(cleanupCtx, "job requeued after direct fleet failure",
			slog.Int("retry_count", fallbackJob.RetryCount))
		if p.Metrics != nil {
			if err := p.Metrics.PublishJobRequeued(cleanupCtx, requeueReasonDirectFleetFailure); err != nil {
				directLog.Error(cleanupCtx, "job requeued metric failed", slog.String("error", err.Error()))
			}
		}
		return
	}

	// Not eligible for requeue (cap reached, already on-demand, or no queue
	// available for recovery). Mark the claim terminal rather than dropping the
	// job. The claim is still in the "claiming" state here (not released), so the
	// guarded terminal write succeeds.
	p.failTerminal(ctx, job)
}

// releaseClaim deletes the job claim on a fresh context so a strained job ctx
// (canceled when the HTTP handler returned, or wedged on a dead connection) does
// not prevent cleanup. A missing jobs table is a no-op.
func (p *DirectProcessor) releaseClaim(job *queue.JobMessage, failMsg string) {
	if p.DB == nil || !p.DB.HasJobsTable() {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
	defer cancel()
	cleanupCtx = logging.ContextWithJob(cleanupCtx, job.JobID, job.RunID, job.Repo)
	if err := p.DB.DeleteJobClaim(cleanupCtx, job.JobID); err != nil {
		directLog.Error(cleanupCtx, failMsg, slog.String("error", err.Error()))
	}
}

// failTerminal marks a job's claim terminal (status "error") when it cannot be
// recovered, so the drop is visible instead of leaving the GitHub job hung. The
// write is guarded on the record still being in the claiming state, so a slow
// concurrent processor that lands SaveJob is not clobbered.
func (p *DirectProcessor) failTerminal(ctx context.Context, job *queue.JobMessage) {
	directLog.Error(ctx, "direct fleet failure not recoverable; marking job terminal",
		slog.Int64(logging.KeyJobID, job.JobID))
	if p.DB == nil || !p.DB.HasJobsTable() {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
	defer cancel()
	cleanupCtx = logging.ContextWithJob(cleanupCtx, job.JobID, job.RunID, job.Repo)
	if err := p.DB.FailExhaustedClaim(cleanupCtx, job.JobID); err != nil {
		directLog.Error(cleanupCtx, "mark job terminal failed after direct fleet failure",
			slog.String("error", err.Error()))
	}
	if p.Metrics != nil {
		// failTerminal is reached only from recoverFleetFailure, i.e. a fleet-create
		// give-up with no eligible fallback. Tag it accordingly so the failure SLA
		// reason matches the ec2-worker's equivalent give-up.
		if err := p.Metrics.PublishSchedulingFailure(cleanupCtx, schedulingFailureFleetCreate); err != nil {
			directLog.Error(cleanupCtx, "scheduling failure metric failed", slog.String("error", err.Error()))
		}
	}
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
					// Parent ctx is intentionally discarded here (it may be canceled
					// when the HTTP handler returns), so stash job identity on a fresh
					// context for the panic record.
					panicCtx := logging.ContextWithJob(context.Background(), jobMsg.JobID, jobMsg.RunID, jobMsg.Repo)
					directLog.Error(panicCtx, "panic in direct processing",
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
