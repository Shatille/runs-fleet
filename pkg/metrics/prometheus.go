package metrics

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// prometheusNamespace is the fixed metric name prefix for the Prometheus
// backend. It is intentionally not configurable: a stable prefix prevents
// metric-name collisions across deployments that share a Prometheus instance.
const prometheusNamespace = "runs_fleet"

// Histogram bucket sets. Job/provision/fleet latencies range up to ~120s;
// lock-wait and message-processing latencies are sub-second to ~30s.
var (
	latencyBucketsLong  = []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 90, 120}
	latencyBucketsShort = []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30}
)

// PrometheusPublisher publishes metrics to Prometheus via /metrics endpoint.
// All Publisher interface methods are documented on the Publisher interface.
type PrometheusPublisher struct {
	registry *prometheus.Registry

	// Job lifecycle
	jobsEnqueued          *prometheus.CounterVec
	jobsAssigned          *prometheus.CounterVec
	runnerConfirmed       *prometheus.CounterVec
	jobsCompleted         *prometheus.CounterVec
	jobsRequeued          *prometheus.CounterVec
	jobsDeduplicated      *prometheus.CounterVec
	jobWaitSeconds        *prometheus.HistogramVec
	jobStartupSeconds     *prometheus.HistogramVec
	agentBootstrapSeconds *prometheus.HistogramVec
	jobExecutionSeconds   *prometheus.HistogramVec

	// Fleet / provisioning
	instanceProvisionSeconds *prometheus.HistogramVec
	fleetCreate              *prometheus.CounterVec
	fleetCreateSeconds       *prometheus.HistogramVec
	instances                *prometheus.GaugeVec
	spotInterruptions        *prometheus.CounterVec
	circuitBreakerTrip       *prometheus.CounterVec
	circuitBreakerOpen       *prometheus.GaugeVec

	// Pools
	poolInstances        *prometheus.GaugeVec
	poolDesired          *prometheus.GaugeVec
	poolActions          *prometheus.CounterVec
	poolReconcileSeconds prometheus.Histogram

	// Internals
	messageProcessingSeconds *prometheus.HistogramVec
	lockWaitSeconds          *prometheus.HistogramVec
	workerInflight           *prometheus.GaugeVec
	queueDepth               *prometheus.GaugeVec
	queueReceive             *prometheus.CounterVec
	awsCallDuration          *prometheus.HistogramVec
	awsCallFailures          *prometheus.CounterVec

	// Cache / housekeeping / misc
	cacheRequests           *prometheus.CounterVec
	cacheOperations         *prometheus.CounterVec
	cacheBytesStored        prometheus.Counter
	cacheErrors             *prometheus.CounterVec
	cacheAuthRejected       *prometheus.CounterVec
	housekeepingActions     *prometheus.CounterVec
	schedulingFailure       *prometheus.CounterVec
	messageDeletionFailures *prometheus.CounterVec

	// Cost
	instanceHours                *prometheus.CounterVec
	estimatedCost                prometheus.Gauge
	runnerExecutionSeconds       *prometheus.CounterVec
	runnerToolCacheMiss          *prometheus.CounterVec
	runnerCacheInterception      *prometheus.CounterVec
	runnerBuildCacheInterception *prometheus.CounterVec
}

// Ensure PrometheusPublisher implements Publisher.
var _ Publisher = (*PrometheusPublisher)(nil)

// PrometheusConfig holds configuration for the Prometheus publisher.
type PrometheusConfig struct{}

// NewPrometheusPublisher creates a Prometheus metrics publisher. The metric
// name prefix is fixed at prometheusNamespace and cannot be overridden.
func NewPrometheusPublisher(_ PrometheusConfig) *PrometheusPublisher {
	ns := prometheusNamespace

	registry := prometheus.NewRegistry()

	p := &PrometheusPublisher{
		registry: registry,

		jobsEnqueued: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "jobs_enqueued_total",
			Help: "Total number of jobs enqueued",
		}, []string{"pool", "arch", "capacity", "repo"}),
		jobsAssigned: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "jobs_assigned_total",
			Help: "Jobs for which we delivered a runner (success side of the fulfillment SLA; failure side is scheduling_failure_total)",
		}, []string{"pool", "source", "repo"}),
		runnerConfirmed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "runner_confirmed_total",
			Help: "Launched instances whose runner registered and began executing; a flatline vs jobs_assigned flags fleet-wide registration failure",
		}, []string{"pool"}),
		jobsCompleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "jobs_completed_total",
			Help: "Jobs finished, by our runner's operational result (served, interrupted, error, timeout); never the client workflow's pass/fail",
		}, []string{"pool", "result", "repo"}),
		jobsRequeued: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "jobs_requeued_total",
			Help: "Total number of jobs requeued",
		}, []string{"reason"}),
		jobsDeduplicated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "jobs_deduplicated_total",
			Help: "Job messages discarded as already-claimed by a concurrent processor (dual-path dedup); NOT a fulfillment failure",
		}, []string{"path"}),
		jobWaitSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "job_wait_seconds",
			Help: "Time from job enqueue to assignment in seconds", Buckets: latencyBucketsLong,
		}, []string{"pool", "source"}),
		jobStartupSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "job_startup_seconds",
			Help: "End-to-end job acquisition latency (GitHub created to started) in seconds", Buckets: latencyBucketsLong,
		}, []string{"pool", "source"}),
		agentBootstrapSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "agent_bootstrap_seconds",
			Help: "Agent bootstrap segment latency in seconds, by phase", Buckets: latencyBucketsLong,
		}, []string{"pool", "phase"}),
		jobExecutionSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "job_execution_seconds",
			Help: "Job execution duration in seconds", Buckets: latencyBucketsLong,
		}, []string{"pool", "result"}),

		instanceProvisionSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "instance_provision_seconds",
			Help: "Time to provision an instance in seconds", Buckets: latencyBucketsLong,
		}, []string{"source", "family"}),
		fleetCreate: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "fleet_create_total",
			Help: "Total number of fleet creation attempts",
		}, []string{"capacity", "result"}),
		fleetCreateSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "fleet_create_seconds",
			Help: "EC2 CreateFleet latency in seconds", Buckets: latencyBucketsLong,
		}, []string{"capacity"}),
		instances: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Name: "instances",
			Help: "Current instance count by state, capacity, and pool",
		}, []string{"state", "capacity", "pool"}),
		spotInterruptions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "spot_interruptions_total",
			Help: "Total number of spot instance interruptions",
		}, []string{"family"}),
		circuitBreakerTrip: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "circuit_breaker_trip_total",
			Help: "Total number of circuit breaker trips",
		}, []string{"instance_type"}),
		circuitBreakerOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Name: "circuit_breaker_open",
			Help: "Circuit breaker open state (1 open, 0 closed)",
		}, []string{"instance_type"}),

		poolInstances: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Name: "pool_instances",
			Help: "Pool instance count by state",
		}, []string{"pool", "state"}),
		poolDesired: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Name: "pool_desired",
			Help: "Desired pool instance count by kind",
		}, []string{"pool", "kind"}),
		poolActions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "pool_actions_total",
			Help: "Total number of pool scaling actions",
		}, []string{"pool", "action", "reason"}),
		poolReconcileSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: ns, Name: "pool_reconcile_seconds",
			Help: "Pool reconcile loop latency in seconds", Buckets: latencyBucketsLong,
		}),

		messageProcessingSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "message_processing_seconds",
			Help: "Queue message processing latency in seconds", Buckets: latencyBucketsShort,
		}, []string{"queue", "result"}),
		lockWaitSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "lock_wait_seconds",
			Help: "Lock acquisition wait time in seconds", Buckets: latencyBucketsShort,
		}, []string{"lock"}),
		workerInflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Name: "worker_inflight",
			Help: "Number of in-flight worker messages by queue",
		}, []string{"queue"}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Name: "queue_depth",
			Help: "Current queue depth by queue",
		}, []string{"queue"}),
		queueReceive: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "queue_receive_total",
			Help: "Total number of queue receive results",
		}, []string{"queue", "result"}),
		awsCallDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "aws_call_duration_seconds",
			Help:    "Latency of AWS SDK calls in seconds, by service and operation",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30},
		}, []string{"service", "operation"}),
		awsCallFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "aws_call_failures_total",
			Help: "Total number of failed AWS SDK calls, by service, operation, and result",
		}, []string{"service", "operation", "result"}),

		cacheRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "cache_requests_total",
			Help: "Total number of cache requests by result",
		}, []string{"result"}),
		cacheOperations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "cache_operations_total",
			Help: "Total cache operations by operation",
		}, []string{"operation"}),
		cacheBytesStored: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "cache_bytes_stored_total",
			Help: "Total bytes written to the cache",
		}),
		cacheErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "cache_errors_total",
			Help: "Total cache server errors by operation",
		}, []string{"operation"}),
		cacheAuthRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "cache_auth_rejected_total",
			Help: "Total rejected cache auth attempts by reason",
		}, []string{"reason"}),
		housekeepingActions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "housekeeping_actions_total",
			Help: "Total number of housekeeping actions by type",
		}, []string{"action"}),
		schedulingFailure: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "scheduling_failure_total",
			Help: "Requests for which we gave up assigning a runner (failure side of the fulfillment SLA; success side is jobs_assigned_total)",
		}, []string{"task_type"}),
		messageDeletionFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "message_deletion_failures_total",
			Help: "Total number of message deletion failures by queue",
		}, []string{"queue"}),

		instanceHours: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "instance_hours_total",
			Help: "Total instance hours consumed by capacity and family",
		}, []string{"capacity", "family"}),
		estimatedCost: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Name: "estimated_cost_usd",
			Help: "Estimated cost in USD",
		}),
		runnerExecutionSeconds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "runner_execution_seconds_total",
			Help: "Total billable runner execution seconds by arch, vCPU, spot, and result",
		}, []string{"arch", "vcpu", "spot", "result"}),
		runnerToolCacheMiss: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "runner_tool_cache_miss_total",
			Help: "On-demand tool downloads not pre-baked into the AMI, by tool, version (major.minor), and arch",
		}, []string{"tool", "version", "arch"}),
		runnerCacheInterception: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "runner_cache_interception_total",
			Help: "Jobs by on-host cache interceptor outcome (engaged, failed, disabled)",
		}, []string{"status"}),
		runnerBuildCacheInterception: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "runner_build_cache_interception_total",
			Help: "Jobs by buildx layer-cache shim outcome (engaged, skipped, failed, disabled)",
		}, []string{"status"}),
	}

	registry.MustRegister(
		p.jobsEnqueued, p.jobsAssigned, p.runnerConfirmed, p.jobsCompleted, p.jobsRequeued,
		p.jobsDeduplicated, p.jobWaitSeconds, p.jobStartupSeconds, p.agentBootstrapSeconds, p.jobExecutionSeconds,
		p.instanceProvisionSeconds, p.fleetCreate, p.fleetCreateSeconds, p.instances,
		p.spotInterruptions, p.circuitBreakerTrip, p.circuitBreakerOpen,
		p.poolInstances, p.poolDesired, p.poolActions, p.poolReconcileSeconds,
		p.messageProcessingSeconds, p.lockWaitSeconds, p.workerInflight,
		p.queueDepth, p.queueReceive, p.awsCallDuration, p.awsCallFailures,
		p.cacheRequests, p.cacheOperations, p.cacheBytesStored, p.cacheErrors, p.cacheAuthRejected,
		p.housekeepingActions, p.schedulingFailure, p.messageDeletionFailures,
		p.instanceHours, p.estimatedCost, p.runnerExecutionSeconds, p.runnerToolCacheMiss,
		p.runnerCacheInterception, p.runnerBuildCacheInterception,
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

func (p *PrometheusPublisher) PublishJobEnqueued(_ context.Context, pool, arch, capacity, repo string) error { //nolint:revive
	p.jobsEnqueued.WithLabelValues(pool, arch, capacity, repo).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishJobAssigned(_ context.Context, pool, source, repo string) error { //nolint:revive
	p.jobsAssigned.WithLabelValues(pool, source, repo).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishJobCompleted(_ context.Context, pool, result, repo string) error { //nolint:revive
	p.jobsCompleted.WithLabelValues(pool, result, repo).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishJobRequeued(_ context.Context, reason string) error { //nolint:revive
	p.jobsRequeued.WithLabelValues(reason).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishJobDeduplicated(_ context.Context, path string) error { //nolint:revive
	p.jobsDeduplicated.WithLabelValues(path).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishRunnerConfirmed(_ context.Context, pool string) error { //nolint:revive
	p.runnerConfirmed.WithLabelValues(pool).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishJobWaitSeconds(_ context.Context, pool, source string, seconds float64) error { //nolint:revive
	p.jobWaitSeconds.WithLabelValues(pool, source).Observe(seconds)
	return nil
}

func (p *PrometheusPublisher) PublishJobStartupSeconds(_ context.Context, pool, source string, seconds float64) error { //nolint:revive
	p.jobStartupSeconds.WithLabelValues(pool, source).Observe(seconds)
	return nil
}

func (p *PrometheusPublisher) PublishAgentBootstrapSeconds(_ context.Context, pool, phase string, seconds float64) error { //nolint:revive
	p.agentBootstrapSeconds.WithLabelValues(pool, phase).Observe(seconds)
	return nil
}

func (p *PrometheusPublisher) PublishJobExecutionSeconds(_ context.Context, pool, result string, seconds float64) error { //nolint:revive
	p.jobExecutionSeconds.WithLabelValues(pool, result).Observe(seconds)
	return nil
}

func (p *PrometheusPublisher) PublishInstanceProvisionSeconds(_ context.Context, source, family string, seconds float64) error { //nolint:revive
	p.instanceProvisionSeconds.WithLabelValues(source, family).Observe(seconds)
	return nil
}

func (p *PrometheusPublisher) PublishFleetCreate(_ context.Context, capacity, result string) error { //nolint:revive
	p.fleetCreate.WithLabelValues(capacity, result).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishFleetCreateSeconds(_ context.Context, capacity string, seconds float64) error { //nolint:revive
	p.fleetCreateSeconds.WithLabelValues(capacity).Observe(seconds)
	return nil
}

func (p *PrometheusPublisher) PublishInstances(_ context.Context, state, capacity, pool string, n int) error { //nolint:revive
	p.instances.WithLabelValues(state, capacity, pool).Set(float64(n))
	return nil
}

func (p *PrometheusPublisher) PublishSpotInterruption(_ context.Context, family string) error { //nolint:revive
	p.spotInterruptions.WithLabelValues(family).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishCircuitBreakerTrip(_ context.Context, instanceType string) error { //nolint:revive
	p.circuitBreakerTrip.WithLabelValues(instanceType).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishCircuitBreakerOpen(_ context.Context, instanceType string, open bool) error { //nolint:revive
	p.circuitBreakerOpen.WithLabelValues(instanceType).Set(boolToFloat(open))
	return nil
}

func (p *PrometheusPublisher) PublishPoolInstances(_ context.Context, pool, state string, n int) error { //nolint:revive
	p.poolInstances.WithLabelValues(pool, state).Set(float64(n))
	return nil
}

func (p *PrometheusPublisher) PublishPoolDesired(_ context.Context, pool, kind string, n int) error { //nolint:revive
	p.poolDesired.WithLabelValues(pool, kind).Set(float64(n))
	return nil
}

func (p *PrometheusPublisher) PublishPoolAction(_ context.Context, pool, action, reason string) error { //nolint:revive
	p.poolActions.WithLabelValues(pool, action, reason).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishPoolReconcileSeconds(_ context.Context, seconds float64) error { //nolint:revive
	p.poolReconcileSeconds.Observe(seconds)
	return nil
}

func (p *PrometheusPublisher) PublishMessageProcessingSeconds(_ context.Context, queue, result string, seconds float64) error { //nolint:revive
	p.messageProcessingSeconds.WithLabelValues(queue, result).Observe(seconds)
	return nil
}

func (p *PrometheusPublisher) PublishLockWaitSeconds(_ context.Context, lock string, seconds float64) error { //nolint:revive
	p.lockWaitSeconds.WithLabelValues(lock).Observe(seconds)
	return nil
}

func (p *PrometheusPublisher) PublishWorkerInflight(_ context.Context, queue string, n int) error { //nolint:revive
	p.workerInflight.WithLabelValues(queue).Set(float64(n))
	return nil
}

func (p *PrometheusPublisher) PublishQueueDepth(_ context.Context, queue string, depth float64) error { //nolint:revive
	p.queueDepth.WithLabelValues(queue).Set(depth)
	return nil
}

func (p *PrometheusPublisher) PublishQueueReceive(_ context.Context, queue, result string) error { //nolint:revive
	p.queueReceive.WithLabelValues(queue, result).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishAWSCallDuration(_ context.Context, service, operation string, durationSeconds float64) error { //nolint:revive
	p.awsCallDuration.WithLabelValues(service, operation).Observe(durationSeconds)
	return nil
}

func (p *PrometheusPublisher) PublishAWSCallFailure(_ context.Context, service, operation, result string) error { //nolint:revive
	p.awsCallFailures.WithLabelValues(service, operation, result).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishCacheRequest(_ context.Context, result string) error { //nolint:revive
	p.cacheRequests.WithLabelValues(result).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishCacheOperation(_ context.Context, operation string) error { //nolint:revive
	p.cacheOperations.WithLabelValues(operation).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishCacheBytesStored(_ context.Context, bytes int64) error { //nolint:revive
	p.cacheBytesStored.Add(float64(bytes))
	return nil
}

func (p *PrometheusPublisher) PublishCacheError(_ context.Context, operation string) error { //nolint:revive
	p.cacheErrors.WithLabelValues(operation).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishCacheAuthRejected(_ context.Context, reason string) error { //nolint:revive
	p.cacheAuthRejected.WithLabelValues(reason).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishHousekeepingAction(_ context.Context, action string, count int) error { //nolint:revive
	p.housekeepingActions.WithLabelValues(action).Add(float64(count))
	return nil
}

func (p *PrometheusPublisher) PublishSchedulingFailure(_ context.Context, taskType string) error { //nolint:revive
	p.schedulingFailure.WithLabelValues(taskType).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishMessageDeletionFailure(_ context.Context, queue string) error { //nolint:revive
	p.messageDeletionFailures.WithLabelValues(queue).Inc()
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

func (p *PrometheusPublisher) PublishInstanceHours(_ context.Context, capacity, family string, hours float64) error { //nolint:revive
	p.instanceHours.WithLabelValues(capacity, family).Add(hours)
	return nil
}

func (p *PrometheusPublisher) PublishRunnerExecutionSeconds(_ context.Context, arch string, vcpu int, spot bool, result string, seconds float64) error { //nolint:revive
	p.runnerExecutionSeconds.WithLabelValues(arch, vcpuLabel(vcpu), spotLabel(spot), result).Add(seconds)
	return nil
}

func (p *PrometheusPublisher) PublishRunnerToolCacheMiss(_ context.Context, tool, version, arch string) error { //nolint:revive
	p.runnerToolCacheMiss.WithLabelValues(tool, version, arch).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishRunnerCacheInterception(_ context.Context, status string) error { //nolint:revive
	p.runnerCacheInterception.WithLabelValues(status).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishRunnerBuildCacheInterception(_ context.Context, status string) error { //nolint:revive
	p.runnerBuildCacheInterception.WithLabelValues(status).Inc()
	return nil
}

func (p *PrometheusPublisher) PublishEstimatedCost(_ context.Context, usd float64) error { //nolint:revive
	p.estimatedCost.Set(usd)
	return nil
}
