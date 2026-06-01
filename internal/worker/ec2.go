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
	"github.com/Shavakan/runs-fleet/pkg/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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

// WarmPoolAssignerInterface defines warm pool assignment operations for testing.
type WarmPoolAssignerInterface interface {
	TryAssignToWarmPool(ctx context.Context, job *queue.JobMessage) (*WarmPoolResult, error)
}

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

	// WarmPoolAssigner allows injection of custom assigner for testing.
	// If nil, creates default WarmPoolAssigner from Pool, Runner, DB.
	WarmPoolAssigner WarmPoolAssignerInterface

	// CreateFleetFn allows injection of a custom fleet creator for testing.
	// If nil, defaults to CreateFleetWithRetry against the embedded Fleet manager.
	CreateFleetFn func(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)

	// PrepareRunnersFn allows injection of a custom runner preparer for testing.
	// If nil, defaults to PrepareRunners against the embedded Runner manager.
	PrepareRunnersFn func(ctx context.Context, job *queue.JobMessage, instanceIDs []string) []string
}

func (d EC2WorkerDeps) createFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error) {
	if d.CreateFleetFn != nil {
		return d.CreateFleetFn(ctx, spec)
	}
	return CreateFleetWithRetry(ctx, d.Fleet, spec)
}

func (d EC2WorkerDeps) prepareRunners(ctx context.Context, job *queue.JobMessage, instanceIDs []string) []string {
	if d.PrepareRunnersFn != nil {
		return d.PrepareRunnersFn(ctx, job, instanceIDs)
	}
	if d.Runner == nil {
		return nil
	}
	return PrepareRunners(ctx, d.Runner, job, instanceIDs)
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
				ec2Log.Error(cleanupCtx, "message delete failed", slog.String("error", err.Error()))
			}
			if deps.Metrics != nil {
				if err := deps.Metrics.PublishQueueDepth(cleanupCtx, -1); err != nil {
					ec2Log.Error(cleanupCtx, "queue depth metric failed", slog.String("error", err.Error()))
				}
				if fleetCreated {
					if err := deps.Metrics.PublishFleetSizeIncrement(cleanupCtx); err != nil {
						ec2Log.Error(cleanupCtx, "fleet size increment metric failed", slog.String("error", err.Error()))
					}
				}
			}
		}

		if alreadyClaimed {
			if err := deleteMessageWithRetry(cleanupCtx, deps.Queue, msg.Handle); err != nil {
				ec2Log.Error(cleanupCtx, "already claimed message delete failed", slog.String("error", err.Error()))
			}
			if deps.Metrics != nil {
				if err := deps.Metrics.PublishQueueDepth(cleanupCtx, -1); err != nil {
					ec2Log.Error(cleanupCtx, "queue depth metric failed", slog.String("error", err.Error()))
				}
			}
		}

		if poisonMessage && deps.Metrics != nil {
			if err := deps.Metrics.PublishQueueDepth(cleanupCtx, -1); err != nil {
				ec2Log.Error(cleanupCtx, "queue depth metric failed", slog.String("error", err.Error()))
			}
		}

		if deps.Metrics != nil {
			_ = deps.Metrics.PublishJobDuration(cleanupCtx, int(time.Since(startTime).Seconds()))
		}
	}()

	var job queue.JobMessage
	if err := json.Unmarshal([]byte(msg.Body), &job); err != nil {
		ec2Log.Warn(ctx, "poison message", slog.String("error", err.Error()))
		poisonMessage = true
		_ = deleteMessageWithRetry(ctx, deps.Queue, msg.Handle)
		if deps.Metrics != nil {
			metricCtx, metricCancel := context.WithTimeout(ctx, config.ShortTimeout)
			defer metricCancel()
			_ = deps.Metrics.PublishMessageDeletionFailure(metricCtx)
		}
		return
	}

	if tp := job.Traceparent; tp != "" {
		ctx = tracing.ExtractTraceContext(tp)
	}
	// Stash task identity so all downstream logs (including deep AWS calls)
	// carry job_id/run_id/repo without restating them at each site.
	ctx = logging.ContextWithJob(ctx, job.JobID, job.RunID, job.Repo)
	ctx, span := tracing.Tracer().Start(ctx, "worker.process_job",
		trace.WithAttributes(
			attribute.Int64("job.id", job.JobID),
			attribute.String("job.pool", job.Pool),
			attribute.String("job.instance_type", job.InstanceType),
			attribute.Bool("job.spot", job.Spot),
		))
	defer span.End()

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

	if job.Pool != "" {
		var assigner WarmPoolAssignerInterface
		if deps.WarmPoolAssigner != nil {
			assigner = deps.WarmPoolAssigner
		} else {
			assigner = &WarmPoolAssigner{
				Pool:   deps.Pool,
				Runner: deps.Runner,
				DB:     deps.DB,
			}
		}
		result, err := assigner.TryAssignToWarmPool(ctx, &job)
		if err != nil {
			ec2Log.Error(ctx, "warm pool assignment failed",
				slog.String(logging.KeyPoolName, job.Pool),
				slog.String("error", err.Error()))
			// Fall through to cold start
		} else if result.Assigned {
			jobProcessed = true
			if deps.Metrics != nil {
				_ = deps.Metrics.PublishWarmPoolHit(ctx)
			}
			ec2Log.Info(ctx, "job assigned to warm pool",
				slog.String(logging.KeyInstanceID, result.InstanceID))
			return
		}
	}

	if deps.Fleet == nil {
		ec2Log.Error(ctx, "fleet manager nil")
		return
	}

	spec := &fleet.LaunchSpec{
		RunID:         job.RunID,
		InstanceType:  job.InstanceType,
		InstanceTypes: job.InstanceTypes,
		SubnetID:      SelectSubnet(deps.Config, deps.SubnetIndex),
		Spot:          job.Spot,
		Pool:          job.Pool,
		Repo:          job.Repo,
		ForceOnDemand: job.ForceOnDemand,
		RetryCount:    job.RetryCount,
		Arch:          job.Arch,
		StorageGiB:    job.StorageGiB,
		Conditions:    BuildRunnerConditions(&job),
	}

	instanceIDs, err := deps.createFleet(ctx, spec)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		ec2Log.Error(ctx, "fleet creation failed",
			slog.String("error", err.Error()))
		if deps.DB != nil && deps.DB.HasJobsTable() {
			// Release the claim on a fresh context; the job ctx may already be
			// expired on a wedged connection, and a leaked claim would block the
			// on-demand fallback requeue below from re-claiming the job.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
			cleanupCtx = logging.ContextWithJob(cleanupCtx, job.JobID, job.RunID, job.Repo)
			if err := deps.DB.DeleteJobClaim(cleanupCtx, job.JobID); err != nil {
				ec2Log.Error(cleanupCtx, "job claim delete failed",
					slog.String("error", err.Error()))
			}
			cancel()
		}

		if job.Spot && !job.ForceOnDemand && job.RetryCount < maxJobRetries {
			handleOnDemandFallback(ctx, deps, &job, msg.Handle)
		}
		return
	}

	jobProcessed = true
	fleetCreated = true

	SaveJobRecords(ctx, deps.DB, &job, instanceIDs)

	if failed := deps.prepareRunners(ctx, &job, instanceIDs); len(failed) > 0 {
		// Runner config could not be written (e.g. SSM throttled). The launched
		// instances would boot without config and never register, so do not mark
		// the job done: release the claim and leave the SQS message for redelivery
		// so the job is retried on a fresh instance. The config-less instances are
		// reaped by housekeeping.
		span.SetStatus(codes.Error, "runner preparation failed")
		jobProcessed = false
		fleetCreated = false
		if deps.DB != nil && deps.DB.HasJobsTable() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
			cleanupCtx = logging.ContextWithJob(cleanupCtx, job.JobID, job.RunID, job.Repo)
			if err := deps.DB.DeleteJobClaim(cleanupCtx, job.JobID); err != nil {
				ec2Log.Error(cleanupCtx, "job claim delete failed after prep failure",
					slog.String("error", err.Error()))
			}
			cancel()
		}
		ec2Log.Error(ctx, "runner preparation failed; job left for redelivery",
			slog.Int("failed_instances", len(failed)))
		return
	}

	if deps.Pool != nil {
		for _, instanceID := range instanceIDs {
			deps.Pool.MarkInstanceBusy(instanceID)
		}
	}

	ec2Log.Info(ctx, "instances launched",
		slog.Int(logging.KeyCount, len(instanceIDs)))
}

// SelectSubnet returns the next private subnet ID in round-robin fashion.
// Returns empty string if no private subnets are configured.
func SelectSubnet(cfg *config.Config, subnetIndex *uint64) string {
	if len(cfg.SubnetIDs) == 0 {
		return ""
	}
	idx := atomic.AddUint64(subnetIndex, 1) - 1
	return cfg.SubnetIDs[idx%uint64(len(cfg.SubnetIDs))]
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
			Traceparent:  job.Traceparent,
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
			ec2Log.Error(ctx, "job record save failed",
				slog.String(logging.KeyInstanceID, instanceID),
				slog.String("error", saveErr.Error()))
		}
	}
}

// PrepareRunners writes runner configurations (to SSM) for each instance and
// returns the IDs of instances whose preparation failed. A non-empty result is
// fatal for the job: a launched instance with no config never registers, so the
// caller must fail the job rather than mark it done.
func PrepareRunners(ctx context.Context, preparer RunnerPreparer, job *queue.JobMessage, instanceIDs []string) []string {
	if preparer == nil {
		return nil
	}
	var failed []string
	for _, instanceID := range instanceIDs {
		// Stash instance_id so the runner manager's config logs (fetching token,
		// storing config, config stored) carry it on the direct/cold-start path,
		// matching the warm-pool path which stashes it before PrepareRunner.
		instanceCtx := logging.ContextWith(ctx, slog.String(logging.KeyInstanceID, instanceID))
		label := handler.BuildRunnerLabel(job)
		prepareReq := runner.PrepareRunnerRequest{
			InstanceID: instanceID,
			JobID:      fmt.Sprintf("%d", job.JobID),
			RunID:      fmt.Sprintf("%d", job.RunID),
			Repo:       job.Repo,
			Labels:     []string{label},
			Pool:       job.Pool,
			Conditions: BuildRunnerConditions(job),
		}
		if err := preparer.PrepareRunner(instanceCtx, prepareReq); err != nil {
			ec2Log.Error(instanceCtx, "runner config preparation failed",
				slog.String("error", err.Error()))
			failed = append(failed, instanceID)
		}
	}
	return failed
}

func handleOnDemandFallback(_ context.Context, deps EC2WorkerDeps, job *queue.JobMessage, msgHandle string) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
	defer cleanupCancel()
	cleanupCtx = logging.ContextWithJob(cleanupCtx, job.JobID, job.RunID, job.Repo)

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
		Arch:          job.Arch,
		StorageGiB:    job.StorageGiB,
	}

	// Send before delete so a failed requeue leaves the original for redelivery.
	if sendErr := sendMessageWithRetry(cleanupCtx, deps.Queue, fallbackJob); sendErr != nil {
		ec2Log.Error(cleanupCtx, "on-demand fallback requeue failed, leaving original message for redelivery",
			slog.String("error", sendErr.Error()))
		return
	}

	ec2Log.Info(cleanupCtx, "job requeued with on-demand fallback",
		slog.Int("retry_count", fallbackJob.RetryCount))
	if deps.Metrics != nil {
		if err := deps.Metrics.PublishJobQueued(cleanupCtx); err != nil {
			ec2Log.Error(cleanupCtx, "job queued metric failed", slog.String("error", err.Error()))
		}
	}

	// Delete only after a successful send. A failed delete is non-fatal: any
	// redelivered duplicate is deduped by ClaimJob's job-id claim.
	if delErr := deleteMessageWithRetry(cleanupCtx, deps.Queue, msgHandle); delErr != nil {
		ec2Log.Error(cleanupCtx, "original message delete failed after fallback requeue (duplicate possible, deduped by claim)",
			slog.String("error", delErr.Error()))
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
