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
	"github.com/Shavakan/runs-fleet/pkg/events"
	gh "github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/tracing"
	"github.com/google/go-github/v57/github"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var webhookLog = logging.WithComponent(logging.LogTypeWebhook, "handler")

const (
	maxJobRetries    = 2
	runnerNamePrefix = "runs-fleet-"
)

// PoolDBClient defines database operations for pool management.
type PoolDBClient interface {
	GetPoolConfig(ctx context.Context, poolName string) (*db.PoolConfig, error)
	CreateEphemeralPool(ctx context.Context, config *db.PoolConfig) error
	TouchPoolActivity(ctx context.Context, poolName string) error
}

// HandleWorkflowJobQueued processes queued workflow_job events. It performs only
// the durable work (label parse, ephemeral pool ensure, SQS enqueue) so the
// webhook can be acked before best-effort observability runs; see
// PublishJobQueuedMetrics for the deferred metrics.
func HandleWorkflowJobQueued(ctx context.Context, event *github.WorkflowJobEvent, q queue.Queue, dbc *db.Client, resolver *gh.AliasResolver) (*queue.JobMessage, error) {
	ctx, span := tracing.Tracer().Start(ctx, "webhook.process",
		trace.WithAttributes(
			attribute.String("github.event_type", "workflow_job"),
			attribute.String("github.repo", event.GetRepo().GetFullName()),
			attribute.Int64("github.run_id", event.GetWorkflowJob().GetRunID()),
		))
	defer span.End()

	job := event.GetWorkflowJob()
	if job == nil {
		webhookLog.Debug(ctx, "skipping event with no workflow_job")
		return nil, nil
	}

	jobConfig, err := gh.ParseLabelsWithAliases(job.Labels, resolver)
	if err != nil {
		webhookLog.Debug(ctx, "skipping job no labels",
			slog.Int64(logging.KeyJobID, job.GetID()),
			slog.String("error", err.Error()))
		return nil, nil
	}

	// run_id is sourced from the webhook payload, not the label. The label's
	// run_id (legacy runs-fleet=<run-id> form) is optional and ignored here.
	runID := job.GetRunID()

	if jobConfig.Pool != "" && dbc != nil {
		if err := EnsureEphemeralPool(ctx, dbc, jobConfig); err != nil {
			webhookLog.Warn(ctx, "ephemeral pool ensure failed",
				slog.String(logging.KeyPoolName, jobConfig.Pool),
				slog.String("error", err.Error()))
		}
	}

	msg := &queue.JobMessage{
		JobID:         job.GetID(),
		RunID:         runID,
		Repo:          event.GetRepo().GetFullName(),
		InstanceType:  jobConfig.InstanceType,
		Pool:          jobConfig.Pool,
		Spot:          jobConfig.Spot,
		OriginalLabel: jobConfig.OriginalLabel,
		Arch:          jobConfig.Arch,
		InstanceTypes: jobConfig.InstanceTypes,
		StorageGiB:    jobConfig.StorageGiB,
		Traceparent:   tracing.InjectTraceContext(ctx),
		// Flexible spec for multi-spec pool matching
		CPUMin:   jobConfig.CPUMin,
		CPUMax:   jobConfig.CPUMax,
		RAMMin:   jobConfig.RAMMin,
		RAMMax:   jobConfig.RAMMax,
		Families: jobConfig.Families,
		Gen:      jobConfig.Gen,
	}

	// Stash task identity so all downstream logs (enqueue, metrics, deep AWS
	// calls) inherit job_id/run_id/repo without restating them at each site.
	ctx = logging.ContextWithJob(ctx, msg.JobID, msg.RunID, msg.Repo)

	if err := q.SendMessage(ctx, msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		webhookLog.Error(ctx, "job enqueue failed",
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("failed to enqueue job: %w", err)
	}

	webhookLog.Info(ctx, "job enqueued",
		slog.String("arch", jobConfig.Arch),
		slog.String(logging.KeyPoolName, jobConfig.Pool),
		slog.Int("cpu_min", jobConfig.CPUMin),
		slog.Int("cpu_max", jobConfig.CPUMax),
		slog.Float64("ram_min", jobConfig.RAMMin),
		slog.Float64("ram_max", jobConfig.RAMMax),
		slog.Any("families", jobConfig.Families),
		slog.Int("gen", jobConfig.Gen),
		slog.Int("disk", jobConfig.StorageGiB),
		slog.Bool("spot", jobConfig.Spot),
		slog.String(logging.KeyAliasLabel, jobConfig.AliasLabel),
		slog.String("original_label", jobConfig.OriginalLabel))
	return msg, nil
}

// PublishJobQueuedMetrics emits the best-effort enqueue metrics for a queued
// job. These observability calls run after the webhook is acked so they never
// count against GitHub's delivery budget; failures are logged, not returned.
func PublishJobQueuedMetrics(ctx context.Context, m metrics.Publisher, job *queue.JobMessage) {
	if m == nil {
		return
	}
	pool, arch, capacity, repo := "", "", "", ""
	if job != nil {
		pool, arch, repo = job.Pool, job.Arch, job.Repo
		capacity = CapacityLabel(job.CPUMin)
	}
	if err := m.PublishJobEnqueued(ctx, pool, arch, capacity, repo); err != nil {
		webhookLog.Error(ctx, "job enqueued metric failed", slog.String("error", err.Error()))
	}
	if err := m.PublishQueueDepth(ctx, "main", 1); err != nil {
		webhookLog.Error(ctx, "queue depth metric failed", slog.String("error", err.Error()))
	}
}

// CapacityLabel maps a requested vCPU count to a low-cardinality capacity label.
// An unset count (0) yields an empty label so it is omitted from the metric.
func CapacityLabel(cpu int) string {
	if cpu <= 0 {
		return ""
	}
	return strconv.Itoa(cpu)
}

// EnsureEphemeralPool creates or updates an ephemeral pool for the given job config.
func EnsureEphemeralPool(ctx context.Context, dbc PoolDBClient, jobConfig *gh.JobConfig) error {
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

	// InstanceType is intentionally NOT set: pinning jobConfig.InstanceType (the
	// smallest resolved match) would lock a flexible-label pool to one type. The pool
	// carries the flexible spec instead; resolvePoolInstanceTypes re-resolves and
	// price-ranks per launch (it uses InstanceType only as an error fallback).
	config := &db.PoolConfig{
		PoolName:           jobConfig.Pool,
		Ephemeral:          true,
		DesiredRunning:     0,
		DesiredStopped:     1,
		IdleTimeoutMinutes: 30,
		LastJobTime:        time.Now(),
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

	webhookLog.Info(ctx, "ephemeral pool created",
		slog.String(logging.KeyPoolName, jobConfig.Pool),
		slog.String("arch", jobConfig.Arch),
		slog.Int("cpu_min", jobConfig.CPUMin),
		slog.Int("cpu_max", jobConfig.CPUMax))
	return nil
}

// HandleJobFailure processes workflow_job completed events with failure conclusion.
func HandleJobFailure(ctx context.Context, event *github.WorkflowJobEvent, q queue.Queue, dbc *db.Client, resolver *gh.AliasResolver) (bool, error) {
	job := event.GetWorkflowJob()
	runnerName := job.GetRunnerName()

	if runnerName == "" {
		return false, nil
	}

	if len(runnerName) <= len(runnerNamePrefix) || runnerName[:len(runnerNamePrefix)] != runnerNamePrefix {
		return false, nil
	}

	_, err := gh.ParseLabelsWithAliases(job.Labels, resolver)
	if err != nil {
		return false, nil
	}

	if dbc == nil || !dbc.HasJobsTable() {
		return false, nil
	}

	ghJobID := job.GetID()
	jobInfo, err := dbc.GetJobByJobID(ctx, ghJobID)
	if err != nil {
		return false, fmt.Errorf("failed to get job %d: %w", ghJobID, err)
	}

	if jobInfo == nil {
		webhookLog.Warn(ctx, "no job record for requeue", slog.Int64(logging.KeyJobID, ghJobID))
		return false, nil
	}
	if jobInfo.RetryCount >= maxJobRetries {
		webhookLog.Warn(ctx, "max retries exceeded",
			slog.Int64(logging.KeyJobID, jobInfo.JobID),
			slog.Int("max_retries", maxJobRetries))
		return false, nil
	}

	if jobInfo.RunID == 0 || jobInfo.Repo == "" {
		return false, nil
	}

	marked, err := dbc.MarkJobRequeuedByJobID(ctx, ghJobID)
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

	ctx = logging.ContextWithJob(ctx, jobInfo.JobID, jobInfo.RunID, jobInfo.Repo)

	if err := q.SendMessage(ctx, requeueMsg); err != nil {
		return false, fmt.Errorf("failed to re-queue job %d: %w", jobInfo.JobID, err)
	}

	webhookLog.Info(ctx, "job requeued",
		slog.Int("retry_count", requeueMsg.RetryCount))

	return true, nil
}

// StartupObservation captures the end-to-end acquisition latency for a single
// workflow_job in_progress event, ready to publish once the source is resolved.
type StartupObservation struct {
	JobID   int64
	Pool    string
	Seconds float64
}

// JobStartupDB resolves the warm-pool disposition of a job for the startup
// metric. It is satisfied by *db.Client.
type JobStartupDB interface {
	HasJobsTable() bool
	GetJobByJobID(ctx context.Context, jobID int64) (*events.JobInfo, error)
}

// HandleWorkflowJobInProgress derives the startup observation from an in_progress
// workflow_job event. It returns nil unless the job ran on one of our runners
// (runner name carries the runs-fleet- prefix) AND carries parseable runs-fleet
// labels — the same two-sided proof HandleJobFailure uses, since in_progress
// fires for every runner in the repo, including foreign ones. The duration is the
// GitHub-clock created-to-started span; nil when either timestamp is zero or the
// span is non-positive.
func HandleWorkflowJobInProgress(event *github.WorkflowJobEvent, resolver *gh.AliasResolver) *StartupObservation {
	job := event.GetWorkflowJob()
	if job == nil {
		return nil
	}

	runnerName := job.GetRunnerName()
	if len(runnerName) <= len(runnerNamePrefix) || runnerName[:len(runnerNamePrefix)] != runnerNamePrefix {
		return nil
	}

	jobConfig, err := gh.ParseLabelsWithAliases(job.Labels, resolver)
	if err != nil {
		return nil
	}

	created := job.GetCreatedAt().Time
	started := job.GetStartedAt().Time
	if created.IsZero() || started.IsZero() {
		return nil
	}
	seconds := started.Sub(created).Seconds()
	if seconds <= 0 {
		return nil
	}

	return &StartupObservation{
		JobID:   job.GetID(),
		Pool:    jobConfig.Pool,
		Seconds: seconds,
	}
}

// PublishJobStartupMetrics publishes the end-to-end startup latency for an
// observation. It resolves the source (warm_pool or cold_start) via one job
// lookup; any miss (nil db, no jobs table, error, or no record) publishes with an
// empty source rather than dropping the observation. Publish errors are logged,
// not returned — this runs post-ack.
func PublishJobStartupMetrics(ctx context.Context, m metrics.Publisher, dbc JobStartupDB, obs *StartupObservation) {
	if m == nil || obs == nil {
		return
	}

	source := ""
	if dbc != nil && dbc.HasJobsTable() {
		info, err := dbc.GetJobByJobID(ctx, obs.JobID)
		switch {
		case err != nil:
			webhookLog.Warn(ctx, "startup source lookup failed",
				slog.Int64(logging.KeyJobID, obs.JobID),
				slog.String("error", err.Error()))
		case info != nil:
			if info.WarmPoolHit {
				source = "warm_pool"
			} else {
				source = "cold_start"
			}
		}
	}

	if err := m.PublishJobStartupSeconds(ctx, obs.Pool, source, obs.Seconds); err != nil {
		webhookLog.Error(ctx, "job startup metric failed",
			slog.Int64(logging.KeyJobID, obs.JobID),
			slog.String("error", err.Error()))
	}
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
