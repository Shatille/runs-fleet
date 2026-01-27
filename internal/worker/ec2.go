// Package worker provides queue message processing for the runs-fleet server.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

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

var ec2Log = logging.WithComponent(logging.LogTypeQueue, "ec2-worker")

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
			_ = deleteMessageWithRetry(cleanupCtx, deps.Queue, msg.Handle)
			if deps.Metrics != nil {
				_ = deps.Metrics.PublishQueueDepth(cleanupCtx, -1)
				if fleetCreated {
					_ = deps.Metrics.PublishFleetSizeIncrement(cleanupCtx)
				}
			}
		}

		if alreadyClaimed {
			_ = deleteMessageWithRetry(cleanupCtx, deps.Queue, msg.Handle)
			if deps.Metrics != nil {
				_ = deps.Metrics.PublishQueueDepth(cleanupCtx, -1)
			}
		}

		if poisonMessage && deps.Metrics != nil {
			_ = deps.Metrics.PublishQueueDepth(cleanupCtx, -1)
		}

		if deps.Metrics != nil {
			_ = deps.Metrics.PublishJobDuration(cleanupCtx, int(time.Since(startTime).Seconds()))
		}
	}()

	var job queue.JobMessage
	if err := json.Unmarshal([]byte(msg.Body), &job); err != nil {
		ec2Log.Warn("poison message", slog.String("error", err.Error()))
		poisonMessage = true
		_ = deleteMessageWithRetry(ctx, deps.Queue, msg.Handle)
		if deps.Metrics != nil {
			metricCtx, metricCancel := context.WithTimeout(ctx, config.ShortTimeout)
			defer metricCancel()
			_ = deps.Metrics.PublishMessageDeletionFailure(metricCtx)
		}
		return
	}

	if deps.DB != nil && deps.DB.HasJobsTable() {
		if err := deps.DB.ClaimJob(ctx, job.JobID); err != nil {
			if errors.Is(err, db.ErrJobAlreadyClaimed) {
				alreadyClaimed = true
				return
			}
			if deps.Metrics != nil {
				_ = deps.Metrics.PublishJobClaimFailure(ctx)
			}
			return
		}
	}

	if deps.Fleet == nil {
		ec2Log.Error("fleet manager nil", slog.Int64(logging.KeyJobID, job.JobID))
		return
	}

	if job.Pool != "" {
		assigner := &WarmPoolAssigner{
			Pool:   deps.Pool,
			Runner: deps.Runner,
			DB:     deps.DB,
		}
		result, err := assigner.TryAssignToWarmPool(ctx, &job)
		if err == nil && result.Assigned {
			jobProcessed = true
			if deps.Metrics != nil {
				_ = deps.Metrics.PublishWarmPoolHit(ctx)
			}
			ec2Log.Info("job assigned to warm pool",
				slog.Int64(logging.KeyJobID, job.JobID),
				slog.String(logging.KeyInstanceID, result.InstanceID))
			return
		}
	}

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
		ec2Log.Error("fleet creation failed",
			slog.Int64(logging.KeyRunID, job.RunID),
			slog.String("error", err.Error()))
		if deps.DB != nil && deps.DB.HasJobsTable() {
			_ = deps.DB.DeleteJobClaim(ctx, job.JobID)
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

	ec2Log.Info("instances launched",
		slog.Int64(logging.KeyRunID, job.RunID),
		slog.Int(logging.KeyCount, len(instanceIDs)))
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
			time.Sleep(backoff)
		}

		instanceIDs, err = f.CreateFleet(ctx, spec)
		if err == nil {
			return instanceIDs, nil
		}
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
			ec2Log.Error("job record save failed",
				slog.String(logging.KeyInstanceID, instanceID),
				slog.String("error", saveErr.Error()))
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
			ec2Log.Error("runner config preparation failed",
				slog.String(logging.KeyInstanceID, instanceID),
				slog.String("error", err.Error()))
		}
	}
}

func handleOnDemandFallback(ctx context.Context, deps EC2WorkerDeps, job *queue.JobMessage, msgHandle string) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
	if delErr := deleteMessageWithRetry(cleanupCtx, deps.Queue, msgHandle); delErr != nil {
		cleanupCancel()
		ec2Log.Error("message delete failed, skipping fallback",
			slog.Int64(logging.KeyJobID, job.JobID),
			slog.String("error", delErr.Error()))
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
		ec2Log.Error("job lost - fallback requeue failed",
			slog.Int64(logging.KeyJobID, job.JobID),
			slog.String("error", sendErr.Error()))
	} else {
		ec2Log.Info("job requeued with on-demand fallback",
			slog.Int64(logging.KeyJobID, job.JobID),
			slog.Int("retry_count", fallbackJob.RetryCount))
		if deps.Metrics != nil {
			_ = deps.Metrics.PublishJobQueued(ctx)
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
