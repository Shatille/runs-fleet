package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Shavakan/runs-fleet/internal/handler"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	"github.com/Shavakan/runs-fleet/pkg/provider/k8s"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
)

var k8sLog = logging.WithComponent(logging.LogTypeQueue, "k8s-worker")

// K8sWorkerDeps holds dependencies for the K8s worker.
type K8sWorkerDeps struct {
	Queue        queue.Queue
	Provider     *k8s.Provider
	PoolProvider *k8s.PoolProvider
	Metrics      metrics.Publisher
	Runner       *runner.Manager
	DB           *db.Client
	Config       *config.Config
}

// RunK8sWorker starts the K8s queue processing loop.
func RunK8sWorker(ctx context.Context, deps K8sWorkerDeps) {
	RunWorkerLoop(ctx, "K8s", deps.Queue, func(ctx context.Context, msg queue.Message) {
		processK8sMessage(ctx, deps, msg)
	})
}

func processK8sMessage(ctx context.Context, deps K8sWorkerDeps, msg queue.Message) {
	startTime := time.Now()
	podCreated := false
	poisonMessage := false

	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
		defer cleanupCancel()

		if podCreated {
			_ = deleteMessageWithRetry(cleanupCtx, deps.Queue, msg.Handle)
			if err := deps.Metrics.PublishQueueDepth(cleanupCtx, -1); err != nil {
				k8sLog.Error("queue depth metric failed", slog.String("error", err.Error()))
			}
		}

		if poisonMessage {
			if err := deps.Metrics.PublishQueueDepth(cleanupCtx, -1); err != nil {
				k8sLog.Error("queue depth metric failed", slog.String("error", err.Error()))
			}
		}

		if err := deps.Metrics.PublishJobDuration(cleanupCtx, int(time.Since(startTime).Seconds())); err != nil {
			k8sLog.Error("job duration metric failed", slog.String("error", err.Error()))
		}
	}()

	if msg.Body == "" {
		k8sLog.Warn("poison message", slog.String("reason", "empty body"))
		poisonMessage = true
		return
	}

	var job queue.JobMessage
	if err := json.Unmarshal([]byte(msg.Body), &job); err != nil {
		k8sLog.Warn("poison message", slog.String("error", err.Error()))
		poisonMessage = true
		_ = deleteMessageWithRetry(ctx, deps.Queue, msg.Handle)
		metricCtx, metricCancel := context.WithTimeout(ctx, config.ShortTimeout)
		defer metricCancel()
		_ = deps.Metrics.PublishMessageDeletionFailure(metricCtx)
		return
	}

	if deps.Runner == nil {
		k8sLog.Error("runner manager nil", slog.Int64(logging.KeyRunID, job.RunID))
		poisonMessage = true
		_ = deps.Metrics.PublishSchedulingFailure(ctx, "k8s-runner-manager-missing")
		return
	}
	regResult, err := deps.Runner.GetRegistrationToken(ctx, job.Repo)
	if err != nil {
		k8sLog.Error("registration token failed",
			slog.Int64(logging.KeyRunID, job.RunID),
			slog.String("error", err.Error()))
		poisonMessage = true
		_ = deps.Metrics.PublishSchedulingFailure(ctx, "k8s-registration-token")
		return
	}
	jitToken := regResult.Token

	spec := &provider.RunnerSpec{
		RunID:        job.RunID,
		JobID:        job.JobID,
		Repo:         job.Repo,
		Labels:       []string{handler.BuildRunnerLabel(&job)},
		Pool:         job.Pool,
		OS:           job.OS,
		Arch:         job.Arch,
		InstanceType: job.InstanceType,
		Spot:         job.Spot,
		Environment:  job.Environment,
		RetryCount:   job.RetryCount,
		StorageGiB:   job.StorageGiB,
		JITToken:     jitToken,
	}

	result, err := CreateK8sRunnerWithRetry(ctx, deps.Provider, spec)
	if err != nil {
		k8sLog.Error("pod creation failed",
			slog.Int64(logging.KeyRunID, job.RunID),
			slog.String("error", err.Error()))
		_ = deps.Metrics.PublishSchedulingFailure(ctx, "k8s-pod-creation")
		return
	}

	podCreated = true

	if deps.DB != nil && deps.DB.HasJobsTable() {
		for _, runnerID := range result.RunnerIDs {
			jobRecord := &db.JobRecord{
				JobID:        job.JobID,
				RunID:        job.RunID,
				Repo:         job.Repo,
				InstanceID:   runnerID,
				InstanceType: job.InstanceType,
				Pool:         job.Pool,
				Spot:         job.Spot,
				RetryCount:   job.RetryCount,
			}
			var saveErr error
			for attempt := 0; attempt < 3; attempt++ {
				if saveErr = deps.DB.SaveJob(ctx, jobRecord); saveErr == nil {
					break
				}
				if attempt < 2 {
					time.Sleep(time.Duration(1<<attempt) * 100 * time.Millisecond)
				}
			}
			if saveErr != nil {
				k8sLog.Error("job record save failed",
					slog.String("pod_id", runnerID),
					slog.String("error", saveErr.Error()))
				_ = deps.Metrics.PublishSchedulingFailure(ctx, "k8s-job-record-save")
			}
		}
	}

	for _, runnerID := range result.RunnerIDs {
		deps.PoolProvider.MarkRunnerBusy(runnerID)
	}

	k8sLog.Info("pods launched",
		slog.Int64(logging.KeyRunID, job.RunID),
		slog.Int(logging.KeyCount, len(result.RunnerIDs)))
}

// CreateK8sRunnerWithRetry attempts to create a K8s runner with exponential backoff.
func CreateK8sRunnerWithRetry(ctx context.Context, p *k8s.Provider, spec *provider.RunnerSpec) (*provider.RunnerResult, error) {
	var result *provider.RunnerResult
	var err error
	for attempt := 0; attempt < maxFleetCreateRetries; attempt++ {
		if attempt > 0 {
			backoff := FleetRetryBaseDelay * time.Duration(1<<uint(attempt-1))
			time.Sleep(backoff)
		}

		result, err = p.CreateRunner(ctx, spec)
		if err == nil {
			return result, nil
		}
	}
	return nil, fmt.Errorf("failed after %d attempts: %w", maxFleetCreateRetries, err)
}
