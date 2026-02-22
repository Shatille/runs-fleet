package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/DataDog/datadog-go/v5/statsd"
)

const defaultDatadogNamespace = "runs_fleet"

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
	// Namespace is the metric namespace prefix (default: "runs_fleet")
	Namespace string
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
	if cfg.Namespace == "" {
		cfg.Namespace = defaultDatadogNamespace
	}
	if cfg.SampleRate <= 0 || cfg.SampleRate > 1 {
		cfg.SampleRate = 1.0
	}

	opts := []statsd.Option{
		statsd.WithNamespace(cfg.Namespace + "."),
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
		namespace:  cfg.Namespace,
		tags:       cfg.Tags,
		sampleRate: cfg.SampleRate,
	}, nil
}

// Close closes the DogStatsD client connection.
func (p *DatadogPublisher) Close() error {
	return p.client.Close()
}

// Publisher interface implementation below.
// All methods are documented on the Publisher interface.

func (p *DatadogPublisher) PublishQueueDepth(_ context.Context, depth float64) error { //nolint:revive
	return p.client.Gauge("queue_depth", depth, nil, 1)
}

func (p *DatadogPublisher) PublishFleetSizeIncrement(_ context.Context) error { //nolint:revive
	return p.client.Incr("fleet_size_increment", nil, 1)
}

func (p *DatadogPublisher) PublishFleetSizeDecrement(_ context.Context) error { //nolint:revive
	return p.client.Incr("fleet_size_decrement", nil, 1)
}

func (p *DatadogPublisher) PublishFleetSize(_ context.Context, size int) error { //nolint:revive
	return p.client.Gauge("fleet_size", float64(size), nil, 1)
}

func (p *DatadogPublisher) PublishJobDuration(_ context.Context, durationSeconds int) error { //nolint:revive
	// Use Distribution for global percentile aggregation across all hosts
	return p.client.Distribution("job_duration_seconds", float64(durationSeconds), nil, 1)
}

func (p *DatadogPublisher) PublishJobSuccess(_ context.Context) error { //nolint:revive
	return p.client.Incr("job_success", nil, 1)
}

func (p *DatadogPublisher) PublishJobFailure(_ context.Context) error { //nolint:revive
	return p.client.Incr("job_failure", nil, 1)
}

func (p *DatadogPublisher) PublishJobQueued(_ context.Context) error { //nolint:revive
	return p.client.Incr("job_queued", nil, 1)
}

func (p *DatadogPublisher) PublishSpotInterruption(_ context.Context) error { //nolint:revive
	return p.client.Incr("spot_interruptions", nil, 1)
}

func (p *DatadogPublisher) PublishMessageDeletionFailure(_ context.Context) error { //nolint:revive
	return p.client.Incr("message_deletion_failures", nil, 1)
}

// PublishCacheHit uses sample rate for high-frequency metrics.
func (p *DatadogPublisher) PublishCacheHit(_ context.Context) error { //nolint:revive
	return p.client.Incr("cache_hits", nil, p.sampleRate)
}

// PublishCacheMiss uses sample rate for high-frequency metrics.
func (p *DatadogPublisher) PublishCacheMiss(_ context.Context) error { //nolint:revive
	return p.client.Incr("cache_misses", nil, p.sampleRate)
}

func (p *DatadogPublisher) PublishOrphanedInstancesTerminated(_ context.Context, count int) error { //nolint:revive
	return p.client.Count("orphaned_instances_terminated", int64(count), nil, 1)
}

func (p *DatadogPublisher) PublishSSMParametersDeleted(_ context.Context, count int) error { //nolint:revive
	return p.client.Count("ssm_parameters_deleted", int64(count), nil, 1)
}

func (p *DatadogPublisher) PublishJobRecordsArchived(_ context.Context, count int) error { //nolint:revive
	return p.client.Count("job_records_archived", int64(count), nil, 1)
}

func (p *DatadogPublisher) PublishOrphanedJobsCleanedUp(_ context.Context, count int) error { //nolint:revive
	return p.client.Count("orphaned_jobs_cleaned_up", int64(count), nil, 1)
}

func (p *DatadogPublisher) PublishStaleJobsReconciled(_ context.Context, count int) error { //nolint:revive
	return p.client.Count("stale_jobs_reconciled", int64(count), nil, 1)
}

func (p *DatadogPublisher) PublishPoolUtilization(_ context.Context, poolName string, utilization float64) error { //nolint:revive
	return p.client.Gauge("pool_utilization_percent", utilization, []string{"pool_name:" + poolName}, 1)
}

func (p *DatadogPublisher) PublishPoolRunningJobs(_ context.Context, poolName string, count int) error { //nolint:revive
	return p.client.Gauge("pool_running_jobs", float64(count), []string{"pool_name:" + poolName}, 1)
}

func (p *DatadogPublisher) PublishSchedulingFailure(_ context.Context, taskType string) error { //nolint:revive
	return p.client.Incr("scheduling_failure", []string{"task_type:" + taskType}, 1)
}

func (p *DatadogPublisher) PublishCircuitBreakerTriggered(_ context.Context, instanceType string) error { //nolint:revive
	return p.client.Incr("circuit_breaker_triggered", []string{"instance_type:" + instanceType}, 1)
}

func (p *DatadogPublisher) PublishJobClaimFailure(_ context.Context) error { //nolint:revive
	return p.client.Incr("job_claim_failures", nil, 1)
}

func (p *DatadogPublisher) PublishWarmPoolHit(_ context.Context) error { //nolint:revive
	return p.client.Incr("warm_pool_hits", nil, 1)
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
