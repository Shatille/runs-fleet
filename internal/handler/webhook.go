// Package handler provides HTTP request handlers for the runs-fleet server.
package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	gh "github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/google/go-github/v57/github"
)

const (
	maxJobRetries    = 2
	runnerNamePrefix = "runs-fleet-"
)

// HandleWorkflowJobQueued processes queued workflow_job events.
func HandleWorkflowJobQueued(ctx context.Context, event *github.WorkflowJobEvent, q queue.Queue, dbc *db.Client, m metrics.Publisher) (*queue.JobMessage, error) {
	log.Printf("Received workflow_job queued: %s", event.GetWorkflowJob().GetName())

	jobConfig, err := gh.ParseLabels(event.GetWorkflowJob().Labels)
	if err != nil {
		log.Printf("Skipping job (no runs-fleet labels): %v", err)
		return nil, nil
	}

	runID, err := strconv.ParseInt(jobConfig.RunID, 10, 64)
	if err != nil {
		log.Printf("Invalid run_id in label %q: %v", jobConfig.RunID, err)
		return nil, nil
	}

	if jobConfig.Pool != "" && dbc != nil {
		if err := EnsureEphemeralPool(ctx, dbc, jobConfig); err != nil {
			log.Printf("Failed to ensure ephemeral pool %s: %v", jobConfig.Pool, err)
		}
	}

	msg := &queue.JobMessage{
		JobID:         event.GetWorkflowJob().GetID(),
		RunID:         runID,
		Repo:          event.GetRepo().GetFullName(),
		InstanceType:  jobConfig.InstanceType,
		Pool:          jobConfig.Pool,
		Spot:          jobConfig.Spot,
		OriginalLabel: jobConfig.OriginalLabel,
		Region:        jobConfig.Region,
		Environment:   jobConfig.Environment,
		OS:            jobConfig.OS,
		Arch:          jobConfig.Arch,
		InstanceTypes: jobConfig.InstanceTypes,
		StorageGiB:    jobConfig.StorageGiB,
		PublicIP:      jobConfig.PublicIP,
	}

	if err := q.SendMessage(ctx, msg); err != nil {
		log.Printf("Failed to enqueue job: %v", err)
		return nil, fmt.Errorf("failed to enqueue job: %w", err)
	}

	if err := m.PublishJobQueued(ctx); err != nil {
		log.Printf("Failed to publish job queued metric: %v", err)
	}
	if err := m.PublishQueueDepth(ctx, 1); err != nil {
		log.Printf("Failed to publish queue depth metric: %v", err)
	}
	if len(jobConfig.InstanceTypes) > 1 {
		log.Printf("Enqueued job for run %s (os=%s, arch=%s, region=%s, env=%s, instanceTypes=%d)",
			jobConfig.RunID, jobConfig.OS, jobConfig.Arch, jobConfig.Region, jobConfig.Environment, len(jobConfig.InstanceTypes))
	} else {
		log.Printf("Enqueued job for run %s (os=%s, arch=%s, region=%s, env=%s)",
			jobConfig.RunID, jobConfig.OS, jobConfig.Arch, jobConfig.Region, jobConfig.Environment)
	}
	return msg, nil
}

// EnsureEphemeralPool creates or updates an ephemeral pool for the given job config.
func EnsureEphemeralPool(ctx context.Context, dbc *db.Client, jobConfig *gh.JobConfig) error {
	poolConfig, err := dbc.GetPoolConfig(ctx, jobConfig.Pool)
	if err != nil {
		return fmt.Errorf("failed to get pool config: %w", err)
	}

	if poolConfig != nil {
		if !poolConfig.Ephemeral {
			return nil
		}
		if err := dbc.TouchPoolActivity(ctx, jobConfig.Pool); err != nil {
			return fmt.Errorf("failed to update pool activity: %w", err)
		}
		log.Printf("Updated ephemeral pool %s activity", jobConfig.Pool)
		return nil
	}

	config := &db.PoolConfig{
		PoolName:           jobConfig.Pool,
		Ephemeral:          true,
		DesiredRunning:     1,
		DesiredStopped:     0,
		IdleTimeoutMinutes: 30,
		LastJobTime:        time.Now(),
		InstanceType:       jobConfig.InstanceType,
		Arch:               jobConfig.Arch,
		CPUMin:             jobConfig.CPUMin,
		CPUMax:             jobConfig.CPUMax,
		RAMMin:             jobConfig.RAMMin,
		RAMMax:             jobConfig.RAMMax,
		Families:           jobConfig.Families,
	}

	if err := dbc.CreateEphemeralPool(ctx, config); err != nil {
		if errors.Is(err, db.ErrPoolAlreadyExists) {
			if touchErr := dbc.TouchPoolActivity(ctx, jobConfig.Pool); touchErr != nil {
				log.Printf("Pool %s created by concurrent job, failed to update activity: %v", jobConfig.Pool, touchErr)
			} else {
				log.Printf("Pool %s created by concurrent job, updated activity", jobConfig.Pool)
			}
			return nil
		}
		return fmt.Errorf("failed to create ephemeral pool: %w", err)
	}

	log.Printf("Created ephemeral pool %s (arch=%s, cpu=%d-%d, ram=%.1f-%.1f)",
		jobConfig.Pool, jobConfig.Arch, jobConfig.CPUMin, jobConfig.CPUMax, jobConfig.RAMMin, jobConfig.RAMMax)
	return nil
}

// HandleJobFailure processes workflow_job completed events with failure conclusion.
func HandleJobFailure(ctx context.Context, event *github.WorkflowJobEvent, q queue.Queue, dbc *db.Client, m metrics.Publisher) (bool, error) {
	job := event.GetWorkflowJob()
	runnerName := job.GetRunnerName()

	if runnerName == "" {
		log.Printf("Job %d failed with no runner assigned, skipping re-queue", job.GetID())
		return false, nil
	}

	if len(runnerName) <= len(runnerNamePrefix) || runnerName[:len(runnerNamePrefix)] != runnerNamePrefix {
		log.Printf("Job %d failed on non-runs-fleet runner %q, skipping", job.GetID(), runnerName)
		return false, nil
	}
	instanceID := runnerName[len(runnerNamePrefix):]

	_, err := gh.ParseLabels(job.Labels)
	if err != nil {
		log.Printf("Job %d not a runs-fleet job, skipping: %v", job.GetID(), err)
		return false, nil
	}

	if dbc == nil || !dbc.HasJobsTable() {
		log.Printf("Jobs table not configured, cannot check job state for instance %s", instanceID)
		return false, nil
	}

	jobInfo, err := dbc.GetJobByInstance(ctx, instanceID)
	if err != nil {
		return false, fmt.Errorf("failed to get job for instance %s: %w", instanceID, err)
	}

	if jobInfo == nil {
		log.Printf("No job record for instance %s, skipping re-queue", instanceID)
		return false, nil
	}

	if jobInfo.RetryCount >= maxJobRetries {
		log.Printf("Job %d exceeded max retries (%d), not re-queuing", jobInfo.JobID, maxJobRetries)
		return false, nil
	}

	if jobInfo.RunID == 0 || jobInfo.Repo == "" {
		log.Printf("Invalid job data for instance %s (RunID=%d, Repo=%q), skipping re-queue",
			instanceID, jobInfo.RunID, jobInfo.Repo)
		return false, nil
	}

	marked, err := dbc.MarkJobRequeued(ctx, instanceID)
	if err != nil {
		return false, fmt.Errorf("failed to mark job requeued: %w", err)
	}
	if !marked {
		log.Printf("Job %d for instance %s already handled (not in running state), skipping", jobInfo.JobID, instanceID)
		return false, nil
	}

	requeueMsg := &queue.JobMessage{
		JobID:         jobInfo.JobID,
		RunID:         jobInfo.RunID,
		Repo:          jobInfo.Repo,
		InstanceType:  jobInfo.InstanceType,
		Pool:          jobInfo.Pool,
		Spot:          false,
		RetryCount:    jobInfo.RetryCount + 1,
		ForceOnDemand: true,
	}

	if err := q.SendMessage(ctx, requeueMsg); err != nil {
		return false, fmt.Errorf("failed to re-queue job %d: %w", jobInfo.JobID, err)
	}

	log.Printf("Re-queued failed job %d from instance %s (RetryCount=%d, ForceOnDemand=true)",
		jobInfo.JobID, instanceID, requeueMsg.RetryCount)

	if err := m.PublishJobQueued(ctx); err != nil {
		log.Printf("Failed to publish job queued metric: %v", err)
	}

	return true, nil
}

// BuildRunnerLabel returns the runs-fleet label for GitHub runner registration.
func BuildRunnerLabel(job *queue.JobMessage) string {
	if job.OriginalLabel != "" {
		return job.OriginalLabel
	}
	label := fmt.Sprintf("runs-fleet=%d", job.RunID)
	if job.Pool != "" {
		label += fmt.Sprintf("/pool=%s", job.Pool)
	}
	if !job.Spot {
		label += "/spot=false"
	}
	return label
}
