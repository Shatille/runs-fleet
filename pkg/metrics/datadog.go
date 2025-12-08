package metrics

import (
	"context"
	"fmt"

	"github.com/DataDog/datadog-go/v5/statsd"
)

const defaultDatadogNamespace = "runs_fleet"

// DatadogPublisher publishes metrics to Datadog via DogStatsD.
// All Publisher interface methods are documented on the Publisher interface.
type DatadogPublisher struct {
	client    *statsd.Client
	namespace string
	tags      []string
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
}

// NewDatadogPublisher creates a Datadog metrics publisher using DogStatsD.
func NewDatadogPublisher(cfg DatadogConfig) (*DatadogPublisher, error) {
	if cfg.Address == "" {
		cfg.Address = "127.0.0.1:8125"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = defaultDatadogNamespace
	}

	client, err := statsd.New(cfg.Address,
		statsd.WithNamespace(cfg.Namespace+"."),
		statsd.WithTags(cfg.Tags),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create DogStatsD client: %w", err)
	}

	return &DatadogPublisher{
		client:    client,
		namespace: cfg.Namespace,
		tags:      cfg.Tags,
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

func (p *DatadogPublisher) PublishJobDuration(_ context.Context, durationSeconds int) error { //nolint:revive
	return p.client.Histogram("job_duration_seconds", float64(durationSeconds), nil, 1)
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

func (p *DatadogPublisher) PublishCacheHit(_ context.Context) error { //nolint:revive
	return p.client.Incr("cache_hits", nil, 1)
}

func (p *DatadogPublisher) PublishCacheMiss(_ context.Context) error { //nolint:revive
	return p.client.Incr("cache_misses", nil, 1)
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

func (p *DatadogPublisher) PublishPoolUtilization(_ context.Context, poolName string, utilization float64) error { //nolint:revive
	return p.client.Gauge("pool_utilization_percent", utilization, []string{"pool_name:" + poolName}, 1)
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
