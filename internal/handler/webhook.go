// Package handler provides HTTP request handlers for the runs-fleet server.
package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	gh "github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/google/go-github/v57/github"
)

var webhookLog = logging.WithComponent(logging.LogTypeWebhook, "handler")

const (
	maxJobRetries    = 2
	runnerNamePrefix = "runs-fleet-"
)

// HandleWorkflowJobQueued processes queued workflow_job events.
func HandleWorkflowJobQueued(ctx context.Context, event *github.WorkflowJobEvent, q queue.Queue, dbc *db.Client, m metrics.Publisher) (*queue.JobMessage, error) {
	jobConfig, err := gh.ParseLabels(event.GetWorkflowJob().Labels)
	if err != nil {
		webhookLog.Debug("skipping job no labels",
			slog.Int64(logging.KeyJobID, event.GetWorkflowJob().GetID()),
			slog.String("error", err.Error()))
		return nil, nil
	}

	runID, err := strconv.ParseInt(jobConfig.RunID, 10, 64)
	if err != nil {
		webhookLog.Warn("invalid run_id in label",
			slog.String("run_id", jobConfig.RunID),
			slog.String("error", err.Error()))
		return nil, nil
	}

	if jobConfig.Pool != "" && dbc != nil {
		if err := EnsureEphemeralPool(ctx, dbc, jobConfig); err != nil {
			webhookLog.Warn("ephemeral pool ensure failed",
				slog.String(logging.KeyPoolName, jobConfig.Pool),
				slog.String("error", err.Error()))
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
		webhookLog.Error("job enqueue failed",
			slog.Int64(logging.KeyJobID, msg.JobID),
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("failed to enqueue job: %w", err)
	}

	if err := m.PublishJobQueued(ctx); err != nil {
		webhookLog.Error("job queued metric failed", slog.String("error", err.Error()))
	}
	if err := m.PublishQueueDepth(ctx, 1); err != nil {
		webhookLog.Error("queue depth metric failed", slog.String("error", err.Error()))
	}

	webhookLog.Info("job enqueued",
		slog.Int64(logging.KeyJobID, msg.JobID),
		slog.Int64(logging.KeyRunID, runID),
		slog.String(logging.KeyRepo, msg.Repo),
		slog.String("arch", jobConfig.Arch),
		slog.String(logging.KeyPoolName, jobConfig.Pool))
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
		return dbc.TouchPoolActivity(ctx, jobConfig.Pool)
	}

	config := &db.PoolConfig{
		PoolName:           jobConfig.Pool,
		Ephemeral:          true,
		DesiredRunning:     0,
		DesiredStopped:     1,
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
			_ = dbc.TouchPoolActivity(ctx, jobConfig.Pool)
			return nil
		}
		return fmt.Errorf("failed to create ephemeral pool: %w", err)
	}

	webhookLog.Info("ephemeral pool created",
		slog.String(logging.KeyPoolName, jobConfig.Pool),
		slog.String("arch", jobConfig.Arch),
		slog.Int("cpu_min", jobConfig.CPUMin),
		slog.Int("cpu_max", jobConfig.CPUMax))
	return nil
}

// HandleJobFailure processes workflow_job completed events with failure conclusion.
func HandleJobFailure(ctx context.Context, event *github.WorkflowJobEvent, q queue.Queue, dbc *db.Client, m metrics.Publisher) (bool, error) {
	job := event.GetWorkflowJob()
	runnerName := job.GetRunnerName()

	if runnerName == "" {
		return false, nil
	}

	if len(runnerName) <= len(runnerNamePrefix) || runnerName[:len(runnerNamePrefix)] != runnerNamePrefix {
		return false, nil
	}
	instanceID := runnerName[len(runnerNamePrefix):]

	_, err := gh.ParseLabels(job.Labels)
	if err != nil {
		return false, nil
	}

	if dbc == nil || !dbc.HasJobsTable() {
		return false, nil
	}

	jobInfo, err := dbc.GetJobByInstance(ctx, instanceID)
	if err != nil {
		return false, fmt.Errorf("failed to get job for instance %s: %w", instanceID, err)
	}

	if jobInfo == nil {
		webhookLog.Warn("no job record for requeue", slog.String(logging.KeyInstanceID, instanceID))
		return false, nil
	}
	if jobInfo.RetryCount >= maxJobRetries {
		webhookLog.Warn("max retries exceeded",
			slog.Int64(logging.KeyJobID, jobInfo.JobID),
			slog.Int("max_retries", maxJobRetries))
		return false, nil
	}

	if jobInfo.RunID == 0 || jobInfo.Repo == "" {
		return false, nil
	}

	marked, err := dbc.MarkJobRequeued(ctx, instanceID)
	if err != nil {
		return false, fmt.Errorf("failed to mark job requeued: %w", err)
	}
	if !marked {
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

	webhookLog.Info("job requeued",
		slog.Int64(logging.KeyJobID, jobInfo.JobID),
		slog.String(logging.KeyInstanceID, instanceID),
		slog.Int("retry_count", requeueMsg.RetryCount))

	if err := m.PublishJobQueued(ctx); err != nil {
		webhookLog.Error("job queued metric failed", slog.String("error", err.Error()))
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
