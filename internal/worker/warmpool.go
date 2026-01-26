package worker

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/Shavakan/runs-fleet/internal/handler"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
)

// WarmPoolAssigner handles assignment of jobs to warm pool instances.
type WarmPoolAssigner struct {
	Pool   *pools.Manager
	Runner *runner.Manager
	DB     *db.Client
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

	// Atomically claim and start a stopped instance to prevent race conditions
	// Uses DynamoDB distributed locking to prevent multiple orchestrators from claiming the same instance
	instance, err := w.Pool.ClaimAndStartPoolInstance(ctx, job.Pool, job.JobID)
	if err != nil {
		if errors.Is(err, pools.ErrNoAvailableInstance) {
			log.Printf("No warm pool instance available for pool %s, falling back to cold start", job.Pool)
			return &WarmPoolResult{Assigned: false}, nil
		}
		return nil, fmt.Errorf("failed to claim pool instance: %w", err)
	}

	log.Printf("Claimed and started warm pool instance %s for job %d", instance.InstanceID, job.JobID)

	// Prepare runner config for this instance (updates SSM)
	// Instance is starting but agent won't read SSM until fully booted (~30s)
	label := handler.BuildRunnerLabel(job)
	prepareReq := runner.PrepareRunnerRequest{
		InstanceID: instance.InstanceID,
		JobID:      fmt.Sprintf("%d", job.JobID),
		RunID:      fmt.Sprintf("%d", job.RunID),
		Repo:       job.Repo,
		Labels:     []string{label},
	}

	if err := w.Runner.PrepareRunner(ctx, prepareReq); err != nil {
		log.Printf("Failed to prepare runner config for warm pool instance %s: %v", instance.InstanceID, err)
		// Instance already started - stop it since it has no valid config
		// Retry stop to ensure instance doesn't run with stale SSM config
		stopped := false
		for attempt := 0; attempt < 3; attempt++ {
			if ctx.Err() != nil {
				log.Printf("Context canceled during stop retry for instance %s", instance.InstanceID)
				break
			}
			if stopErr := w.Pool.StopPoolInstance(ctx, instance.InstanceID); stopErr != nil {
				log.Printf("Failed to stop instance after SSM prep failure (attempt %d/3): %v", attempt+1, stopErr)
				continue
			}
			stopped = true
			break
		}
		if !stopped {
			log.Printf("CRITICAL: Instance %s may be running with invalid config - manual cleanup required", instance.InstanceID)
		}
		// Release the distributed claim since assignment failed
		if w.DB != nil {
			if releaseErr := w.DB.ReleaseInstanceClaim(ctx, instance.InstanceID, job.JobID); releaseErr != nil {
				log.Printf("Failed to release instance claim after SSM prep failure: %v", releaseErr)
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
			log.Printf("Failed to save job record for warm pool assignment: %v", err)
			// Release claim first to allow another orchestrator to retry with this instance
			if releaseErr := w.DB.ReleaseInstanceClaim(ctx, instance.InstanceID, job.JobID); releaseErr != nil {
				log.Printf("Failed to release instance claim after DB save failure: %v", releaseErr)
			}
			// Job record is required for tracking - stop instance and fall back to cold start
			stopped := false
			for attempt := 0; attempt < 3; attempt++ {
				if ctx.Err() != nil {
					break
				}
				if stopErr := w.Pool.StopPoolInstance(ctx, instance.InstanceID); stopErr != nil {
					log.Printf("Failed to stop instance after DB save failure (attempt %d/3): %v", attempt+1, stopErr)
					continue
				}
				stopped = true
				break
			}
			if !stopped {
				log.Printf("CRITICAL: Instance %s may be running without job record - manual cleanup required", instance.InstanceID)
			}
			return &WarmPoolResult{Assigned: false}, nil
		}
	}

	log.Printf("Successfully assigned job %d to warm pool instance %s", job.JobID, instance.InstanceID)
	return &WarmPoolResult{
		Assigned:   true,
		InstanceID: instance.InstanceID,
	}, nil
}
