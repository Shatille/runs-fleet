package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/internal/handler"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	"github.com/Shavakan/runs-fleet/pkg/provider/k8s"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
)

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
	log.Println("Starting K8s worker loop...")
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)
	var activeWork sync.WaitGroup

	defer func() {
		log.Println("Waiting for in-flight K8s work to complete...")
		activeWork.Wait()
		log.Println("K8s worker shutdown complete")
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			timeout := 25 * time.Second
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline)
				if remaining < timeout {
					timeout = remaining
				}
			}
			recvCtx, cancel := context.WithTimeout(ctx, timeout)
			messages, err := deps.Queue.ReceiveMessages(recvCtx, 10, 20)
			cancel()
			if err != nil {
				log.Printf("Failed to receive messages: %v", err)
				continue
			}

			if len(messages) == 0 {
				continue
			}

			for _, msg := range messages {
				msg := msg
				activeWork.Add(1)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("panic in processK8sMessage: %v", r)
						}
					}()
					defer activeWork.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					processCtx, processCancel := context.WithTimeout(ctx, config.MessageProcessTimeout)
					defer processCancel()
					processK8sMessage(processCtx, deps, msg)
				}()
			}
		}
	}
}

func processK8sMessage(ctx context.Context, deps K8sWorkerDeps, msg queue.Message) {
	startTime := time.Now()
	podCreated := false
	poisonMessage := false

	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
		defer cleanupCancel()

		if podCreated {
			if err := deleteMessageWithRetry(cleanupCtx, deps.Queue, msg.Handle); err != nil {
				log.Printf("Failed to delete message after %d attempts: %v", maxDeleteRetries, err)
			}
			if err := deps.Metrics.PublishQueueDepth(cleanupCtx, -1); err != nil {
				log.Printf("Failed to publish queue depth metric: %v", err)
			}
		}

		if poisonMessage {
			if err := deps.Metrics.PublishQueueDepth(cleanupCtx, -1); err != nil {
				log.Printf("Failed to publish queue depth metric: %v", err)
			}
		}

		if err := deps.Metrics.PublishJobDuration(cleanupCtx, int(time.Since(startTime).Seconds())); err != nil {
			log.Printf("Failed to publish job duration metric: %v", err)
		}
	}()

	if msg.Body == "" {
		log.Printf("Received message with empty body")
		poisonMessage = true
		return
	}

	var job queue.JobMessage
	if err := json.Unmarshal([]byte(msg.Body), &job); err != nil {
		log.Printf("Failed to unmarshal message: %v", err)
		poisonMessage = true

		if err := deleteMessageWithRetry(ctx, deps.Queue, msg.Handle); err != nil {
			log.Printf("Failed to delete poison message after %d attempts: %v", maxDeleteRetries, err)
		}

		metricCtx, metricCancel := context.WithTimeout(ctx, config.ShortTimeout)
		defer metricCancel()
		if metricErr := deps.Metrics.PublishMessageDeletionFailure(metricCtx); metricErr != nil {
			log.Printf("Failed to publish poison message metric: %v", metricErr)
		}
		return
	}

	log.Printf("Processing K8s job for run %d (os=%s, arch=%s, pool=%s)",
		job.RunID, job.OS, job.Arch, job.Pool)

	var jitToken string
	if deps.Runner == nil {
		log.Printf("Runner manager not initialized for K8s job %d", job.RunID)
		poisonMessage = true
		if metricErr := deps.Metrics.PublishSchedulingFailure(ctx, "k8s-runner-manager-missing"); metricErr != nil {
			log.Printf("Failed to publish runner manager missing metric: %v", metricErr)
		}
		return
	}
	regResult, err := deps.Runner.GetRegistrationToken(ctx, job.Repo)
	if err != nil {
		log.Printf("Failed to get registration token for K8s job %d: %v", job.RunID, err)
		poisonMessage = true
		if metricErr := deps.Metrics.PublishSchedulingFailure(ctx, "k8s-registration-token"); metricErr != nil {
			log.Printf("Failed to publish registration token failure metric: %v", metricErr)
		}
		return
	}
	jitToken = regResult.Token

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
		log.Printf("Failed to create K8s runner: %v", err)
		if metricErr := deps.Metrics.PublishSchedulingFailure(ctx, "k8s-pod-creation"); metricErr != nil {
			log.Printf("Failed to publish pod creation failure metric: %v", metricErr)
		}
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
				log.Printf("ERROR: Failed to save job record after retries for pod %s: %v", runnerID, saveErr)
				if metricErr := deps.Metrics.PublishSchedulingFailure(ctx, "k8s-job-record-save"); metricErr != nil {
					log.Printf("Failed to publish job record save failure metric: %v", metricErr)
				}
			}
		}
	}

	for _, runnerID := range result.RunnerIDs {
		deps.PoolProvider.MarkRunnerBusy(runnerID)
	}

	log.Printf("Successfully created %d K8s pod(s) for run %d", len(result.RunnerIDs), job.RunID)
}

// CreateK8sRunnerWithRetry attempts to create a K8s runner with exponential backoff.
func CreateK8sRunnerWithRetry(ctx context.Context, p *k8s.Provider, spec *provider.RunnerSpec) (*provider.RunnerResult, error) {
	var result *provider.RunnerResult
	var err error
	for attempt := 0; attempt < maxFleetCreateRetries; attempt++ {
		if attempt > 0 {
			backoff := FleetRetryBaseDelay * time.Duration(1<<uint(attempt-1))
			log.Printf("Retrying K8s pod creation (attempt %d/%d) after %v", attempt+1, maxFleetCreateRetries, backoff)
			time.Sleep(backoff)
		}

		result, err = p.CreateRunner(ctx, spec)
		if err == nil {
			return result, nil
		}

		log.Printf("K8s pod creation attempt %d/%d failed: %v", attempt+1, maxFleetCreateRetries, err)
	}
	return nil, fmt.Errorf("failed after %d attempts: %w", maxFleetCreateRetries, err)
}
