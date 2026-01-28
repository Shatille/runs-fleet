// Package worker provides queue message processing for the runs-fleet server.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/Shavakan/runs-fleet/internal/handler"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
)

const (
	maxDeleteRetries      = 3
	maxFleetCreateRetries = 3
	maxJobRetries         = 2
)

var (
	// RetryDelay is the delay between retry attempts for queue operations.
	RetryDelay = 1 * time.Second
	// FleetRetryBaseDelay is the base delay for exponential backoff in fleet creation.
	FleetRetryBaseDelay = 2 * time.Second
)

// EC2WorkerDeps holds dependencies for the EC2 worker.
type EC2WorkerDeps struct {
	Queue       queue.Queue
	Fleet       *fleet.Manager
	Pool        *pools.Manager
	Metrics     metrics.Publisher
	Runner      *runner.Manager
	DB          *db.Client
	Config      *config.Config
	SubnetIndex *uint64
}

// RunEC2Worker starts the EC2 queue processing loop.
func RunEC2Worker(ctx context.Context, deps EC2WorkerDeps) {
	RunWorkerLoop(ctx, "EC2", deps.Queue, func(ctx context.Context, msg queue.Message) {
		processEC2Message(ctx, deps, msg)
	})
}

//nolint:gocyclo // Core message processing with warm pool + cold start paths
func processEC2Message(ctx context.Context, deps EC2WorkerDeps, msg queue.Message) {
	startTime := time.Now()
	jobProcessed := false
	fleetCreated := false
	poisonMessage := false
	alreadyClaimed := false

	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
		defer cleanupCancel()

		if jobProcessed {
			if err := deleteMessageWithRetry(cleanupCtx, deps.Queue, msg.Handle); err != nil {
				log.Printf("Failed to delete message after %d attempts: %v", maxDeleteRetries, err)
			}
			if deps.Metrics != nil {
				if err := deps.Metrics.PublishQueueDepth(cleanupCtx, -1); err != nil {
					log.Printf("Failed to publish queue depth metric: %v", err)
				}
				// Only publish fleet size increment for cold starts, not warm pool hits
				if fleetCreated {
					if metricErr := deps.Metrics.PublishFleetSizeIncrement(cleanupCtx); metricErr != nil {
						log.Printf("Failed to publish fleet size increment metric: %v", metricErr)
					}
				}
			}
		}

		if alreadyClaimed {
			if err := deleteMessageWithRetry(cleanupCtx, deps.Queue, msg.Handle); err != nil {
				log.Printf("Failed to delete already-claimed message: %v", err)
			}
			if deps.Metrics != nil {
				if err := deps.Metrics.PublishQueueDepth(cleanupCtx, -1); err != nil {
					log.Printf("Failed to publish queue depth metric: %v", err)
				}
			}
		}

		if poisonMessage {
			if deps.Metrics != nil {
				if err := deps.Metrics.PublishQueueDepth(cleanupCtx, -1); err != nil {
					log.Printf("Failed to publish queue depth metric: %v", err)
				}
			}
		}

		if deps.Metrics != nil {
			if err := deps.Metrics.PublishJobDuration(cleanupCtx, int(time.Since(startTime).Seconds())); err != nil {
				log.Printf("Failed to publish job duration metric: %v", err)
			}
		}
	}()

	var job queue.JobMessage
	if err := json.Unmarshal([]byte(msg.Body), &job); err != nil {
		log.Printf("Failed to unmarshal message: %v", err)
		poisonMessage = true

		if err := deleteMessageWithRetry(ctx, deps.Queue, msg.Handle); err != nil {
			log.Printf("Failed to delete poison message after %d attempts: %v", maxDeleteRetries, err)
		}

		if deps.Metrics != nil {
			metricCtx, metricCancel := context.WithTimeout(ctx, config.ShortTimeout)
			defer metricCancel()
			if metricErr := deps.Metrics.PublishMessageDeletionFailure(metricCtx); metricErr != nil {
				log.Printf("Failed to publish poison message metric: %v", metricErr)
			}
		}
		return
	}

	log.Printf("Processing job for run %d (retry=%d, forceOnDemand=%v, os=%s, region=%s, env=%s)",
		job.RunID, job.RetryCount, job.ForceOnDemand, job.OS, job.Region, job.Environment)

	if deps.DB != nil && deps.DB.HasJobsTable() {
		if err := deps.DB.ClaimJob(ctx, job.JobID); err != nil {
			if errors.Is(err, db.ErrJobAlreadyClaimed) {
				log.Printf("Job %d already claimed, skipping (duplicate from queue)", job.JobID)
				alreadyClaimed = true
				return
			}
			log.Printf("Job %d claim failed: %v, will retry via visibility timeout", job.JobID, err)
			if deps.Metrics != nil {
				if metricErr := deps.Metrics.PublishJobClaimFailure(ctx); metricErr != nil {
					log.Printf("Failed to publish job claim failure metric: %v", metricErr)
				}
			}
			return
		}
	}

	if deps.Fleet == nil {
		log.Printf("ERROR: Fleet manager is nil, cannot process job %d - this indicates a configuration error", job.JobID)
		return
	}

	// Try warm pool assignment first
	if job.Pool != "" {
		assigner := &WarmPoolAssigner{
			Pool:   deps.Pool,
			Runner: deps.Runner,
			DB:     deps.DB,
		}
		result, err := assigner.TryAssignToWarmPool(ctx, &job)
		if err != nil {
			log.Printf("Warm pool assignment error for job %d: %v", job.JobID, err)
		} else if result.Assigned {
			jobProcessed = true // Reuse flag for message deletion
			if deps.Metrics != nil {
				if metricErr := deps.Metrics.PublishWarmPoolHit(ctx); metricErr != nil {
					log.Printf("Failed to publish warm pool hit metric: %v", metricErr)
				}
			}
			log.Printf("Assigned job %d to warm pool instance %s", job.JobID, result.InstanceID)
			return
		}
	}

	// Cold start: create new fleet
	spec := &fleet.LaunchSpec{
		RunID:         job.RunID,
		InstanceType:  job.InstanceType,
		InstanceTypes: job.InstanceTypes,
		SubnetID:      SelectSubnet(deps.Config, deps.SubnetIndex, job.PublicIP),
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

	instanceIDs, err := CreateFleetWithRetry(ctx, deps.Fleet, spec)
	if err != nil {
		log.Printf("Failed to create fleet: %v", err)
		if deps.DB != nil && deps.DB.HasJobsTable() {
			if claimErr := deps.DB.DeleteJobClaim(ctx, job.JobID); claimErr != nil {
				log.Printf("Failed to delete job claim for %d: %v", job.JobID, claimErr)
			}
		}

		if job.Spot && !job.ForceOnDemand && job.RetryCount < maxJobRetries {
			handleOnDemandFallback(ctx, deps, &job, msg.Handle)
		}
		return
	}

	jobProcessed = true
	fleetCreated = true

	SaveJobRecords(ctx, deps.DB, &job, instanceIDs)
	PrepareRunners(ctx, deps.Runner, &job, instanceIDs)

	if deps.Pool != nil {
		for _, instanceID := range instanceIDs {
			deps.Pool.MarkInstanceBusy(instanceID)
		}
	}

	log.Printf("Successfully launched %d instance(s) for run %d", len(instanceIDs), job.RunID)
}

// SelectSubnet returns the next subnet ID in round-robin fashion.
// Prioritizes private subnets by default (to avoid public IPv4 costs).
// Uses public subnets only when explicitly requested via publicIP=true label,
// or when no private subnets are configured.
// Returns empty string if publicIP is requested but no public subnets are available.
func SelectSubnet(cfg *config.Config, subnetIndex *uint64, publicIP bool) string {
	var subnets []string

	if publicIP {
		if len(cfg.PublicSubnetIDs) > 0 {
			subnets = cfg.PublicSubnetIDs
		} else {
			return ""
		}
	} else if len(cfg.PrivateSubnetIDs) > 0 {
		subnets = cfg.PrivateSubnetIDs
	} else {
		subnets = cfg.PublicSubnetIDs
	}

	if len(subnets) == 0 {
		return ""
	}

	idx := atomic.AddUint64(subnetIndex, 1) - 1
	return subnets[idx%uint64(len(subnets))]
}

// CreateFleetWithRetry attempts to create a fleet with exponential backoff.
func CreateFleetWithRetry(ctx context.Context, f *fleet.Manager, spec *fleet.LaunchSpec) ([]string, error) {
	var instanceIDs []string
	var err error
	for attempt := 0; attempt < maxFleetCreateRetries; attempt++ {
		if attempt > 0 {
			backoff := FleetRetryBaseDelay * time.Duration(1<<uint(attempt-1))
			log.Printf("Retrying fleet creation (attempt %d/%d) after %v", attempt+1, maxFleetCreateRetries, backoff)
			time.Sleep(backoff)
		}

		instanceIDs, err = f.CreateFleet(ctx, spec)
		if err == nil {
			return instanceIDs, nil
		}

		log.Printf("Fleet creation attempt %d/%d failed: %v", attempt+1, maxFleetCreateRetries, err)
	}
	return nil, fmt.Errorf("failed after %d attempts: %w", maxFleetCreateRetries, err)
}

// SaveJobRecords saves job records to DynamoDB.
func SaveJobRecords(ctx context.Context, dbc *db.Client, job *queue.JobMessage, instanceIDs []string) {
	if dbc == nil || !dbc.HasJobsTable() {
		return
	}
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
		var saveErr error
		for attempt := 0; attempt < 3; attempt++ {
			if saveErr = dbc.SaveJob(ctx, jobRecord); saveErr == nil {
				break
			}
			if attempt < 2 {
				time.Sleep(time.Duration(1<<attempt) * 100 * time.Millisecond)
			}
		}
		if saveErr != nil {
			log.Printf("ERROR: Failed to save job record after retries for instance %s: %v (spot interruption tracking disabled)", instanceID, saveErr)
		}
	}
}

// PrepareRunners prepares runner configurations in SSM.
func PrepareRunners(ctx context.Context, rm *runner.Manager, job *queue.JobMessage, instanceIDs []string) {
	if rm == nil {
		return
	}
	for _, instanceID := range instanceIDs {
		label := handler.BuildRunnerLabel(job)
		prepareReq := runner.PrepareRunnerRequest{
			InstanceID: instanceID,
			JobID:      fmt.Sprintf("%d", job.JobID),
			RunID:      fmt.Sprintf("%d", job.RunID),
			Repo:       job.Repo,
			Labels:     []string{label},
		}
		if err := rm.PrepareRunner(ctx, prepareReq); err != nil {
			log.Printf("Failed to prepare runner config for instance %s: %v", instanceID, err)
		}
	}
}

func handleOnDemandFallback(ctx context.Context, deps EC2WorkerDeps, job *queue.JobMessage, msgHandle string) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
	if delErr := deleteMessageWithRetry(cleanupCtx, deps.Queue, msgHandle); delErr != nil {
		cleanupCancel()
		log.Printf("Failed to delete original message, skipping fallback to prevent duplicates: %v", delErr)
		return
	}
	cleanupCancel()

	fallbackJob := &queue.JobMessage{
		JobID:         job.JobID,
		RunID:         job.RunID,
		Repo:          job.Repo,
		InstanceType:  job.InstanceType,
		InstanceTypes: job.InstanceTypes,
		Pool:          job.Pool,
		Spot:          false,
		OriginalLabel: job.OriginalLabel,
		RetryCount:    job.RetryCount + 1,
		ForceOnDemand: true,
		Region:        job.Region,
		Environment:   job.Environment,
		OS:            job.OS,
		Arch:          job.Arch,
		StorageGiB:    job.StorageGiB,
		PublicIP:      job.PublicIP,
	}
	if sendErr := sendMessageWithRetry(ctx, deps.Queue, fallbackJob); sendErr != nil {
		log.Printf("CRITICAL: Job %d lost - message deleted but fallback failed after retries: %v", job.JobID, sendErr)
	} else {
		log.Printf("Re-queued job %d with on-demand fallback (RetryCount=%d)", job.JobID, fallbackJob.RetryCount)
		if deps.Metrics != nil {
			if metricErr := deps.Metrics.PublishJobQueued(ctx); metricErr != nil {
				log.Printf("Failed to publish job queued metric: %v", metricErr)
			}
		}
	}
}

func deleteMessageWithRetry(ctx context.Context, q queue.Queue, receiptHandle string) error {
	err := q.DeleteMessage(ctx, receiptHandle)
	for attempts := 1; attempts < maxDeleteRetries && err != nil; attempts++ {
		time.Sleep(RetryDelay)
		err = q.DeleteMessage(ctx, receiptHandle)
	}
	return err
}

func sendMessageWithRetry(ctx context.Context, q queue.Queue, job *queue.JobMessage) error {
	err := q.SendMessage(ctx, job)
	for attempts := 1; attempts < maxDeleteRetries && err != nil; attempts++ {
		time.Sleep(RetryDelay)
		err = q.SendMessage(ctx, job)
	}
	return err
}
