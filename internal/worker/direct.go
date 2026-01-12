package worker

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/Shavakan/runs-fleet/internal/handler"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
)

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
// Returns true if fleet was created, false if job was already claimed or processing failed.
func (p *DirectProcessor) ProcessJobDirect(ctx context.Context, job *queue.JobMessage) bool {
	log.Printf("Direct processing job for run %d", job.RunID)

	if p.Fleet == nil {
		log.Printf("Direct processing: Fleet manager is nil for job %d", job.JobID)
		return false
	}

	if p.DB != nil && p.DB.HasJobsTable() {
		if err := p.DB.ClaimJob(ctx, job.JobID); err != nil {
			if errors.Is(err, db.ErrJobAlreadyClaimed) {
				log.Printf("Job %d already claimed (direct path)", job.JobID)
				return false
			}
			log.Printf("Job %d claim failed (direct path): %v, deferring to queue", job.JobID, err)
			if p.Metrics != nil {
				if metricErr := p.Metrics.PublishJobClaimFailure(ctx); metricErr != nil {
					log.Printf("Failed to publish job claim failure metric: %v", metricErr)
				}
			}
			return false
		}
	}

	spec := &fleet.LaunchSpec{
		RunID:         job.RunID,
		InstanceType:  job.InstanceType,
		InstanceTypes: job.InstanceTypes,
		SubnetID:      SelectSubnet(p.Config, p.SubnetIndex),
		Spot:          job.Spot,
		Pool:          job.Pool,
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
		log.Printf("Direct processing: failed to create fleet for job %d: %v", job.JobID, err)
		if p.DB != nil && p.DB.HasJobsTable() {
			if claimErr := p.DB.DeleteJobClaim(ctx, job.JobID); claimErr != nil {
				log.Printf("Direct processing: failed to delete job claim for %d: %v", job.JobID, claimErr)
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
				log.Printf("Direct processing: failed to save job record: %v", err)
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
			}
			if err := p.Runner.PrepareRunner(ctx, prepareReq); err != nil {
				log.Printf("Direct processing: failed to prepare runner: %v", err)
			}
		}
	}

	for _, instanceID := range instanceIDs {
		p.Pool.MarkInstanceBusy(instanceID)
	}

	if err := p.Metrics.PublishFleetSizeIncrement(ctx); err != nil {
		log.Printf("Direct processing: failed to publish metric: %v", err)
	}

	log.Printf("Direct processing: launched %d instance(s) for run %d", len(instanceIDs), job.RunID)
	return true
}

// TryDirectProcessing attempts to process a job directly if capacity is available.
func TryDirectProcessing(ctx context.Context, processor *DirectProcessor, sem chan struct{}, jobMsg *queue.JobMessage) {
	if processor == nil || sem == nil {
		return
	}
	select {
	case sem <- struct{}{}:
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Panic in direct processing for job %d: %v", jobMsg.JobID, r)
				}
			}()
			defer func() { <-sem }()
			directCtx, cancel := context.WithTimeout(ctx, config.MessageProcessTimeout)
			defer cancel()
			processor.ProcessJobDirect(directCtx, jobMsg)
		}()
	default:
		log.Printf("Direct processing at capacity, job %d will be processed via queue", jobMsg.JobID)
	}
}
