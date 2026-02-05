package metrics

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const defaultPrometheusNamespace = "runs_fleet"

// PrometheusPublisher publishes metrics to Prometheus via /metrics endpoint.
// All Publisher interface methods are documented on the Publisher interface.
type PrometheusPublisher struct {
	registry *prometheus.Registry

	queueDepth                  prometheus.Gauge
	fleetSizeIncrement          prometheus.Counter
	fleetSizeDecrement          prometheus.Counter
	jobDuration                 prometheus.Histogram
	jobSuccess                  prometheus.Counter
	jobFailure                  prometheus.Counter
	jobQueued                   prometheus.Counter
	spotInterruptions           prometheus.Counter
	messageDeletionFailures     prometheus.Counter
	cacheHits                   prometheus.Counter
	cacheMisses                 prometheus.Counter
	orphanedInstancesTerminated prometheus.Counter
	ssmParametersDeleted        prometheus.Counter
	jobRecordsArchived          prometheus.Counter
	orphanedJobsCleanedUp       prometheus.Counter
	poolUtilization             *prometheus.GaugeVec
	poolRunningJobs             *prometheus.GaugeVec
	schedulingFailure           *prometheus.CounterVec
	circuitBreakerTriggered     *prometheus.CounterVec
	jobClaimFailures            prometheus.Counter
	warmPoolHits                prometheus.Counter
	fleetSize                   prometheus.Gauge
}

// Ensure PrometheusPublisher implements Publisher.
var _ Publisher = (*PrometheusPublisher)(nil)

// PrometheusConfig holds configuration for the Prometheus publisher.
type PrometheusConfig struct {
	Namespace string
}

// NewPrometheusPublisher creates a Prometheus metrics publisher.
func NewPrometheusPublisher(cfg PrometheusConfig) *PrometheusPublisher {
	if cfg.Namespace == "" {
		cfg.Namespace = defaultPrometheusNamespace
	}

	registry := prometheus.NewRegistry()

	p := &PrometheusPublisher{
		registry: registry,

		queueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: cfg.Namespace,
			Name:      "queue_depth",
			Help:      "Current depth of the job queue",
		}),
		fleetSizeIncrement: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "fleet_size_increment_total",
			Help:      "Total number of fleet size increments",
		}),
		fleetSizeDecrement: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "fleet_size_decrement_total",
			Help:      "Total number of fleet size decrements",
		}),
		jobDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: cfg.Namespace,
			Name:      "job_duration_seconds",
			Help:      "Duration of jobs in seconds",
			Buckets:   []float64{30, 60, 120, 300, 600, 900, 1800, 3600},
		}),
		jobSuccess: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "job_success_total",
			Help:      "Total number of successful jobs",
		}),
		jobFailure: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "job_failure_total",
			Help:      "Total number of failed jobs",
		}),
		jobQueued: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "job_queued_total",
			Help:      "Total number of jobs queued",
		}),
		spotInterruptions: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "spot_interruptions_total",
			Help:      "Total number of spot instance interruptions",
		}),
		messageDeletionFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "message_deletion_failures_total",
			Help:      "Total number of message deletion failures",
		}),
		cacheHits: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "cache_hits_total",
			Help:      "Total number of cache hits",
		}),
		cacheMisses: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "cache_misses_total",
			Help:      "Total number of cache misses",
		}),
		orphanedInstancesTerminated: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "orphaned_instances_terminated_total",
			Help:      "Total number of orphaned instances terminated",
		}),
		ssmParametersDeleted: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "ssm_parameters_deleted_total",
			Help:      "Total number of SSM parameters deleted",
		}),
		jobRecordsArchived: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "job_records_archived_total",
			Help:      "Total number of job records archived",
		}),
		orphanedJobsCleanedUp: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "orphaned_jobs_cleaned_up_total",
			Help:      "Total number of orphaned job records cleaned up",
		}),
		poolUtilization: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: cfg.Namespace,
			Name:      "pool_utilization_percent",
			Help:      "Pool utilization percentage",
		}, []string{"pool_name"}),
		poolRunningJobs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: cfg.Namespace,
			Name:      "pool_running_jobs",
			Help:      "Number of jobs with status=running per pool",
		}, []string{"pool_name"}),
		schedulingFailure: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "scheduling_failure_total",
			Help:      "Total number of scheduling failures",
		}, []string{"task_type"}),
		circuitBreakerTriggered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "circuit_breaker_triggered_total",
			Help:      "Total number of circuit breaker triggers",
		}, []string{"instance_type"}),
		jobClaimFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "job_claim_failures_total",
			Help:      "Total number of job claim failures that proceeded anyway",
		}),
		warmPoolHits: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: cfg.Namespace,
			Name:      "warm_pool_hits_total",
			Help:      "Total number of jobs assigned to warm pool instances",
		}),
		fleetSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: cfg.Namespace,
			Name:      "fleet_size",
			Help:      "Current absolute fleet size",
		}),
	}

	registry.MustRegister(
		p.queueDepth,
		p.fleetSizeIncrement,
		p.fleetSizeDecrement,
		p.jobDuration,
		p.jobSuccess,
		p.jobFailure,
		p.jobQueued,
		p.spotInterruptions,
		p.messageDeletionFailures,
		p.cacheHits,
		p.cacheMisses,
		p.orphanedInstancesTerminated,
		p.ssmParametersDeleted,
		p.jobRecordsArchived,
		p.orphanedJobsCleanedUp,
		p.poolUtilization,
		p.poolRunningJobs,
		p.schedulingFailure,
		p.circuitBreakerTriggered,
		p.jobClaimFailures,
		p.warmPoolHits,
		p.fleetSize,
	)

	return p
}

// Handler returns an HTTP handler for the /metrics endpoint.
func (p *PrometheusPublisher) Handler() http.Handler {
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{})
}

// Registry returns the Prometheus registry for custom integrations.
func (p *PrometheusPublisher) Registry() *prometheus.Registry {
	return p.registry
}

// Close implements Publisher.Close. Prometheus registry doesn't require cleanup.
func (p *PrometheusPublisher) Close() error {
	return nil
}

// Publisher interface implementation below.
// All methods are documented on the Publisher interface.

func (p *PrometheusPublisher) PublishQueueDepth(_ context.Context, depth float64) error { //nolint:revive
	p.queueDepth.Set(depth)
	return nil
}

func (p *PrometheusPublisher) PublishFleetSizeIncrement(_ context.Context) error { //nolint:revive
	p.fleetSizeIncrement.Inc()
	return nil
}

func (p *PrometheusPublisher) PublishFleetSizeDecrement(_ context.Context) error { //nolint:revive
	p.fleetSizeDecrement.Inc()
	return nil
}

func (p *PrometheusPublisher) PublishJobDuration(_ context.Context, durationSeconds int) error { //nolint:revive
	p.jobDuration.Observe(float64(durationSeconds))
	return nil
}

func (p *PrometheusPublisher) PublishJobSuccess(_ context.Context) error { //nolint:revive
	p.jobSuccess.Inc()
	return nil
}

func (p *PrometheusPublisher) PublishJobFailure(_ context.Context) error { //nolint:revive
	p.jobFailure.Inc()
	return nil
}

func (p *PrometheusPublisher) PublishJobQueued(_ context.Context) error { //nolint:revive
	p.jobQueued.Inc()
	return nil
}

func (p *PrometheusPublisher) PublishSpotInterruption(_ context.Context) error { //nolint:revive
	p.spotInterruptions.Inc()
	return nil
}

func (p *PrometheusPublisher) PublishMessageDeletionFailure(_ context.Context) error { //nolint:revive
	p.messageDeletionFailures.Inc()
	return nil
}

func (p *PrometheusPublisher) PublishCacheHit(_ context.Context) error { //nolint:revive
	p.cacheHits.Inc()
	return nil
}

func (p *PrometheusPublisher) PublishCacheMiss(_ context.Context) error { //nolint:revive
	p.cacheMisses.Inc()
	return nil
}

func (p *PrometheusPublisher) PublishOrphanedInstancesTerminated(_ context.Context, count int) error { //nolint:revive
	p.orphanedInstancesTerminated.Add(float64(count))
	return nil
}

func (p *PrometheusPublisher) PublishSSMParametersDeleted(_ context.Context, count int) error { //nolint:revive
	p.ssmParametersDeleted.Add(float64(count))
	return nil
}

func (p *PrometheusPublisher) PublishJobRecordsArchived(_ context.Context, count int) error { //nolint:revive
	p.jobRecordsArchived.Add(float64(count))
	return nil
}

func (p *PrometheusPublisher) PublishOrphanedJobsCleanedUp(_ context.Context, count int) error { //nolint:revive
	p.orphanedJobsCleanedUp.Add(float64(count))
	return nil
}

func (p *PrometheusPublisher) PublishPoolUtilization(_ context.Context, poolName string, utilization float64) error { //nolint:revive
	p.poolUtilization.WithLabelValues(poolName).Set(utilization)
	return nil
}

func (p *PrometheusPublisher) PublishPoolRunningJobs(_ context.Context, poolName string, count int) error { //nolint:revive
	p.poolRunningJobs.WithLabelValues(poolName).Set(float64(count))
	return nil
}

func (p *PrometheusPublisher) PublishSchedulingFailure(_ context.Context, taskType string) error { //nolint:revive
	p.schedulingFailure.WithLabelValues(taskType).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishCircuitBreakerTriggered(_ context.Context, instanceType string) error { //nolint:revive
	p.circuitBreakerTriggered.WithLabelValues(instanceType).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishJobClaimFailure(_ context.Context) error { //nolint:revive
	p.jobClaimFailures.Inc()
	return nil
}

func (p *PrometheusPublisher) PublishWarmPoolHit(_ context.Context) error { //nolint:revive
	p.warmPoolHits.Inc()
	return nil
}

func (p *PrometheusPublisher) PublishFleetSize(_ context.Context, size int) error { //nolint:revive
	p.fleetSize.Set(float64(size))
	return nil
}

// PublishServiceCheck is a no-op for Prometheus (Datadog-specific feature).
func (p *PrometheusPublisher) PublishServiceCheck(_ context.Context, _ string, _ int, _ string) error { //nolint:revive
	return nil
}

// PublishEvent is a no-op for Prometheus (Datadog-specific feature).
func (p *PrometheusPublisher) PublishEvent(_ context.Context, _, _, _ string, _ []string) error { //nolint:revive
	return nil
}
