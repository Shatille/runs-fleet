package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/DataDog/datadog-go/v5/statsd"
)

// datadogNamespace is the fixed metric name prefix for the Datadog backend. It
// is intentionally not configurable: a stable prefix prevents metric-name
// collisions across deployments that report to the same Datadog account.
const datadogNamespace = "runs_fleet"

// ServiceCheckStatus represents Datadog service check status values.
const (
	ServiceCheckOK       = 0
	ServiceCheckWarning  = 1
	ServiceCheckCritical = 2
	ServiceCheckUnknown  = 3
)

// DatadogPublisher publishes metrics to Datadog via DogStatsD.
// All Publisher interface methods are documented on the Publisher interface.
type DatadogPublisher struct {
	client     *statsd.Client
	namespace  string
	tags       []string
	sampleRate float64
}

// Ensure DatadogPublisher implements Publisher.
var _ Publisher = (*DatadogPublisher)(nil)

// DatadogConfig holds configuration for the Datadog publisher.
type DatadogConfig struct {
	// Address is the DogStatsD address (default: "127.0.0.1:8125")
	Address string
	// Tags are global tags applied to all metrics
	Tags []string
	// SampleRate for high-frequency metrics (default: 1.0 = 100%)
	// Values < 1.0 enable sampling to reduce network traffic
	SampleRate float64

	// Client tuning options (0 = use library default)
	// BufferPoolSize configures buffer pool size (0 = library default of 2048)
	BufferPoolSize int
	// BufferFlushInterval configures flush interval (0 = library default of 100ms)
	BufferFlushInterval time.Duration
	// WorkersCount configures parallel workers (0 = library default of 1)
	WorkersCount int
	// MaxMessagesPerPayload limits messages per UDP payload (0 = unlimited)
	MaxMessagesPerPayload int
}

// NewDatadogPublisher creates a Datadog metrics publisher using DogStatsD.
func NewDatadogPublisher(cfg DatadogConfig) (*DatadogPublisher, error) {
	if cfg.Address == "" {
		cfg.Address = "127.0.0.1:8125"
	}
	if cfg.SampleRate <= 0 || cfg.SampleRate > 1 {
		cfg.SampleRate = 1.0
	}

	opts := []statsd.Option{
		statsd.WithNamespace(datadogNamespace + "."),
		statsd.WithTags(cfg.Tags),
	}

	if cfg.BufferPoolSize > 0 {
		opts = append(opts, statsd.WithBufferPoolSize(cfg.BufferPoolSize))
	}
	if cfg.BufferFlushInterval > 0 {
		opts = append(opts, statsd.WithBufferFlushInterval(cfg.BufferFlushInterval))
	}
	if cfg.WorkersCount > 0 {
		opts = append(opts, statsd.WithWorkersCount(cfg.WorkersCount))
	}
	if cfg.MaxMessagesPerPayload > 0 {
		opts = append(opts, statsd.WithMaxMessagesPerPayload(cfg.MaxMessagesPerPayload))
	}

	client, err := statsd.New(cfg.Address, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create DogStatsD client: %w", err)
	}

	return &DatadogPublisher{
		client:     client,
		namespace:  datadogNamespace,
		tags:       cfg.Tags,
		sampleRate: cfg.SampleRate,
	}, nil
}

// Close closes the DogStatsD client connection.
func (p *DatadogPublisher) Close() error {
	return p.client.Close()
}

// ddTag formats a key/value pair as a DogStatsD tag, dropping empty values so
// optional labels do not create distinct series.
func ddTag(out []string, key, value string) []string {
	if value == "" {
		return out
	}
	return append(out, key+":"+value)
}

// Publisher interface implementation below.
// All methods are documented on the Publisher interface.

func (p *DatadogPublisher) PublishJobEnqueued(_ context.Context, pool, arch, capacity, repo string) error { //nolint:revive
	tags := ddTag(ddTag(ddTag(ddTag(nil, "pool", pool), "arch", arch), "capacity", capacity), "repo", repo)
	return p.client.Incr("jobs_enqueued", tags, 1)
}

func (p *DatadogPublisher) PublishJobAssigned(_ context.Context, pool, source, repo string) error { //nolint:revive
	tags := ddTag(ddTag(ddTag(nil, "pool", pool), "source", source), "repo", repo)
	return p.client.Incr("jobs_assigned", tags, 1)
}

func (p *DatadogPublisher) PublishJobCompleted(_ context.Context, pool, result, repo string) error { //nolint:revive
	tags := ddTag(ddTag(ddTag(nil, "pool", pool), "result", result), "repo", repo)
	return p.client.Incr("jobs_completed", tags, 1)
}

func (p *DatadogPublisher) PublishJobRequeued(_ context.Context, reason string) error { //nolint:revive
	return p.client.Incr("jobs_requeued", ddTag(nil, "reason", reason), 1)
}

func (p *DatadogPublisher) PublishJobDeduplicated(_ context.Context, path string) error { //nolint:revive
	return p.client.Incr("jobs_deduplicated", ddTag(nil, "path", path), 1)
}

func (p *DatadogPublisher) PublishRunnerConfirmed(_ context.Context, pool string) error { //nolint:revive
	return p.client.Incr("runner_confirmed", ddTag(nil, "pool", pool), 1)
}

func (p *DatadogPublisher) PublishJobWaitSeconds(_ context.Context, pool, source string, seconds float64) error { //nolint:revive
	return p.client.Distribution("job_wait_seconds", seconds, ddTag(ddTag(nil, "pool", pool), "source", source), 1)
}

func (p *DatadogPublisher) PublishJobExecutionSeconds(_ context.Context, pool, result string, seconds float64) error { //nolint:revive
	return p.client.Distribution("job_execution_seconds", seconds, ddTag(ddTag(nil, "pool", pool), "result", result), 1)
}

func (p *DatadogPublisher) PublishInstanceProvisionSeconds(_ context.Context, source, family string, seconds float64) error { //nolint:revive
	return p.client.Distribution("instance_provision_seconds", seconds, ddTag(ddTag(nil, "source", source), "family", family), 1)
}

func (p *DatadogPublisher) PublishFleetCreate(_ context.Context, capacity, result string) error { //nolint:revive
	return p.client.Incr("fleet_create", ddTag(ddTag(nil, "capacity", capacity), "result", result), 1)
}

func (p *DatadogPublisher) PublishFleetCreateSeconds(_ context.Context, capacity string, seconds float64) error { //nolint:revive
	return p.client.Distribution("fleet_create_seconds", seconds, ddTag(nil, "capacity", capacity), 1)
}

func (p *DatadogPublisher) PublishInstances(_ context.Context, state, capacity, pool string, n int) error { //nolint:revive
	tags := ddTag(ddTag(ddTag(nil, "state", state), "capacity", capacity), "pool", pool)
	return p.client.Gauge("instances", float64(n), tags, 1)
}

func (p *DatadogPublisher) PublishSpotInterruption(_ context.Context, family string) error { //nolint:revive
	return p.client.Incr("spot_interruptions", ddTag(nil, "family", family), 1)
}

func (p *DatadogPublisher) PublishCircuitBreakerTrip(_ context.Context, instanceType string) error { //nolint:revive
	return p.client.Incr("circuit_breaker_trip", ddTag(nil, "instance_type", instanceType), 1)
}

func (p *DatadogPublisher) PublishCircuitBreakerOpen(_ context.Context, instanceType string, open bool) error { //nolint:revive
	return p.client.Gauge("circuit_breaker_open", boolToFloat(open), ddTag(nil, "instance_type", instanceType), 1)
}

func (p *DatadogPublisher) PublishPoolInstances(_ context.Context, pool, state string, n int) error { //nolint:revive
	return p.client.Gauge("pool_instances", float64(n), ddTag(ddTag(nil, "pool", pool), "state", state), 1)
}

func (p *DatadogPublisher) PublishPoolDesired(_ context.Context, pool, kind string, n int) error { //nolint:revive
	return p.client.Gauge("pool_desired", float64(n), ddTag(ddTag(nil, "pool", pool), "kind", kind), 1)
}

func (p *DatadogPublisher) PublishPoolAction(_ context.Context, pool, action, reason string) error { //nolint:revive
	tags := ddTag(ddTag(ddTag(nil, "pool", pool), "action", action), "reason", reason)
	return p.client.Incr("pool_actions", tags, 1)
}

func (p *DatadogPublisher) PublishPoolReconcileSeconds(_ context.Context, seconds float64) error { //nolint:revive
	return p.client.Distribution("pool_reconcile_seconds", seconds, nil, 1)
}

func (p *DatadogPublisher) PublishMessageProcessingSeconds(_ context.Context, queue, result string, seconds float64) error { //nolint:revive
	return p.client.Distribution("message_processing_seconds", seconds, ddTag(ddTag(nil, "queue", queue), "result", result), p.sampleRate)
}

func (p *DatadogPublisher) PublishLockWaitSeconds(_ context.Context, lock string, seconds float64) error { //nolint:revive
	return p.client.Distribution("lock_wait_seconds", seconds, ddTag(nil, "lock", lock), p.sampleRate)
}

func (p *DatadogPublisher) PublishWorkerInflight(_ context.Context, queue string, n int) error { //nolint:revive
	return p.client.Gauge("worker_inflight", float64(n), ddTag(nil, "queue", queue), 1)
}

func (p *DatadogPublisher) PublishQueueDepth(_ context.Context, queue string, depth float64) error { //nolint:revive
	return p.client.Gauge("queue_depth", depth, ddTag(nil, "queue", queue), 1)
}

func (p *DatadogPublisher) PublishQueueReceive(_ context.Context, queue, result string) error { //nolint:revive
	return p.client.Incr("queue_receive", ddTag(ddTag(nil, "queue", queue), "result", result), 1)
}

// PublishAWSCallDuration publishes AWS SDK call latency as a Distribution for
// global percentile aggregation, tagged by service and operation.
func (p *DatadogPublisher) PublishAWSCallDuration(_ context.Context, service, operation string, durationSeconds float64) error { //nolint:revive
	return p.client.Distribution("aws_call_duration_seconds", durationSeconds,
		[]string{"service:" + service, "operation:" + operation}, 1)
}

// PublishAWSCallFailure increments the AWS SDK call failure counter, tagged by
// service, operation, and result.
func (p *DatadogPublisher) PublishAWSCallFailure(_ context.Context, service, operation, result string) error { //nolint:revive
	return p.client.Incr("aws_call_failures",
		[]string{"service:" + service, "operation:" + operation, "result:" + result}, 1)
}

// PublishCacheRequest uses sample rate for high-frequency cache traffic.
func (p *DatadogPublisher) PublishCacheRequest(_ context.Context, result string) error { //nolint:revive
	return p.client.Incr("cache_requests", ddTag(nil, "result", result), p.sampleRate)
}

func (p *DatadogPublisher) PublishCacheOperation(_ context.Context, operation string) error { //nolint:revive
	return p.client.Incr("cache_operations", ddTag(nil, "operation", operation), p.sampleRate)
}

func (p *DatadogPublisher) PublishCacheBytesStored(_ context.Context, bytes int64) error { //nolint:revive
	return p.client.Count("cache_bytes_stored", bytes, nil, 1)
}

func (p *DatadogPublisher) PublishCacheError(_ context.Context, operation string) error { //nolint:revive
	return p.client.Incr("cache_errors", ddTag(nil, "operation", operation), 1)
}

func (p *DatadogPublisher) PublishCacheAuthRejected(_ context.Context, reason string) error { //nolint:revive
	return p.client.Incr("cache_auth_rejected", ddTag(nil, "reason", reason), 1)
}

func (p *DatadogPublisher) PublishHousekeepingAction(_ context.Context, action string, count int) error { //nolint:revive
	return p.client.Count("housekeeping_actions", int64(count), ddTag(nil, "action", action), 1)
}

func (p *DatadogPublisher) PublishSchedulingFailure(_ context.Context, taskType string) error { //nolint:revive
	return p.client.Incr("scheduling_failure", ddTag(nil, "task_type", taskType), 1)
}

func (p *DatadogPublisher) PublishMessageDeletionFailure(_ context.Context, queue string) error { //nolint:revive
	return p.client.Incr("message_deletion_failures", ddTag(nil, "queue", queue), 1)
}

// PublishServiceCheck publishes a Datadog service check.
func (p *DatadogPublisher) PublishServiceCheck(_ context.Context, name string, status int, message string) error { //nolint:revive
	var ddStatus statsd.ServiceCheckStatus
	switch status {
	case ServiceCheckOK:
		ddStatus = statsd.Ok
	case ServiceCheckWarning:
		ddStatus = statsd.Warn
	case ServiceCheckCritical:
		ddStatus = statsd.Critical
	default:
		ddStatus = statsd.Unknown
	}

	return p.client.ServiceCheck(&statsd.ServiceCheck{
		Name:    p.namespace + "." + name,
		Status:  ddStatus,
		Message: message,
		Tags:    p.tags,
	})
}

// PublishEvent publishes a Datadog event.
func (p *DatadogPublisher) PublishEvent(_ context.Context, title, text, alertType string, tags []string) error { //nolint:revive
	var ddAlertType statsd.EventAlertType
	switch alertType {
	case "warning":
		ddAlertType = statsd.Warning
	case "error":
		ddAlertType = statsd.Error
	case "success":
		ddAlertType = statsd.Success
	default:
		ddAlertType = statsd.Info
	}

	allTags := make([]string, 0, len(p.tags)+len(tags))
	allTags = append(allTags, p.tags...)
	allTags = append(allTags, tags...)

	return p.client.Event(&statsd.Event{
		Title:     title,
		Text:      text,
		AlertType: ddAlertType,
		Tags:      allTags,
	})
}

func (p *DatadogPublisher) PublishInstanceHours(_ context.Context, capacity, family string, hours float64) error { //nolint:revive
	return p.client.Count("instance_hours", int64(hours), ddTag(ddTag(nil, "capacity", capacity), "family", family), 1)
}

func (p *DatadogPublisher) PublishEstimatedCost(_ context.Context, usd float64) error { //nolint:revive
	return p.client.Gauge("estimated_cost_usd", usd, nil, 1)
}
