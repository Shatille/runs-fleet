package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// CloudWatchAPI provides CloudWatch operations.
type CloudWatchAPI interface {
	PutMetricData(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

// CloudWatchPublisher publishes metrics to AWS CloudWatch.
type CloudWatchPublisher struct {
	client    CloudWatchAPI
	namespace string
}

// Ensure CloudWatchPublisher implements Publisher.
var _ Publisher = (*CloudWatchPublisher)(nil)

// cloudWatchNamespace is the fixed CloudWatch namespace. It is intentionally
// not configurable: a stable namespace prevents metric collisions across
// deployments that publish to the same AWS account and region.
const cloudWatchNamespace = "RunsFleet"

// NewCloudWatchPublisher creates a CloudWatch metrics publisher. The namespace
// is fixed at cloudWatchNamespace and cannot be overridden.
func NewCloudWatchPublisher(cfg aws.Config) *CloudWatchPublisher {
	return &CloudWatchPublisher{
		client:    cloudwatch.NewFromConfig(cfg),
		namespace: cloudWatchNamespace,
	}
}

// Close implements Publisher.Close. CloudWatch client doesn't require cleanup.
func (p *CloudWatchPublisher) Close() error {
	return nil
}

// --- Job lifecycle ---

// PublishJobEnqueued publishes the jobs enqueued counter.
func (p *CloudWatchPublisher) PublishJobEnqueued(ctx context.Context, pool, arch, capacity, repo string) error {
	return p.putCounter(ctx, "JobsEnqueued", dims(
		"Pool", pool, "Arch", arch, "Capacity", capacity, "Repo", repo,
	))
}

// PublishJobAssigned publishes the jobs assigned counter.
func (p *CloudWatchPublisher) PublishJobAssigned(ctx context.Context, pool, source, repo string) error {
	return p.putCounter(ctx, "JobsAssigned", dims(
		"Pool", pool, "Source", source, "Repo", repo,
	))
}

// PublishRunnerConfirmed publishes the runner confirmed counter.
func (p *CloudWatchPublisher) PublishRunnerConfirmed(ctx context.Context, pool string) error {
	return p.putCounter(ctx, "RunnerConfirmed", dims("Pool", pool))
}

// PublishJobCompleted publishes the jobs completed counter.
func (p *CloudWatchPublisher) PublishJobCompleted(ctx context.Context, pool, result, repo string) error {
	return p.putCounter(ctx, "JobsCompleted", dims(
		"Pool", pool, "Result", result, "Repo", repo,
	))
}

// PublishJobRequeued publishes the jobs requeued counter.
func (p *CloudWatchPublisher) PublishJobRequeued(ctx context.Context, reason string) error {
	return p.putCounter(ctx, "JobsRequeued", dims("Reason", reason))
}

// PublishJobDeduplicated publishes the jobs deduplicated counter (dual-path dedup
// discards; not a fulfillment failure).
func (p *CloudWatchPublisher) PublishJobDeduplicated(ctx context.Context, path string) error {
	return p.putCounter(ctx, "JobsDeduplicated", dims("Path", path))
}

// PublishJobWaitSeconds publishes the job wait latency as a StatisticSet.
func (p *CloudWatchPublisher) PublishJobWaitSeconds(ctx context.Context, pool, source string, seconds float64) error {
	return p.putStatistic(ctx, "JobWaitSeconds", seconds, types.StandardUnitSeconds, dims(
		"Pool", pool, "Source", source,
	))
}

// PublishJobStartupSeconds publishes the end-to-end startup latency as a StatisticSet.
func (p *CloudWatchPublisher) PublishJobStartupSeconds(ctx context.Context, pool, source string, seconds float64) error {
	return p.putStatistic(ctx, "JobStartupSeconds", seconds, types.StandardUnitSeconds, dims(
		"Pool", pool, "Source", source,
	))
}

// PublishAgentBootstrapSeconds publishes an agent bootstrap segment as a StatisticSet.
func (p *CloudWatchPublisher) PublishAgentBootstrapSeconds(ctx context.Context, pool, phase string, seconds float64) error {
	return p.putStatistic(ctx, "AgentBootstrapSeconds", seconds, types.StandardUnitSeconds, dims(
		"Pool", pool, "Phase", phase,
	))
}

// PublishJobExecutionSeconds publishes the job execution latency as a StatisticSet.
func (p *CloudWatchPublisher) PublishJobExecutionSeconds(ctx context.Context, pool, result string, seconds float64) error {
	return p.putStatistic(ctx, "JobExecutionSeconds", seconds, types.StandardUnitSeconds, dims(
		"Pool", pool, "Result", result,
	))
}

// --- Fleet / provisioning ---

// PublishInstanceProvisionSeconds publishes the provision latency as a StatisticSet.
func (p *CloudWatchPublisher) PublishInstanceProvisionSeconds(ctx context.Context, source, family string, seconds float64) error {
	return p.putStatistic(ctx, "InstanceProvisionSeconds", seconds, types.StandardUnitSeconds, dims(
		"Source", source, "Family", family,
	))
}

// PublishFleetCreate publishes the fleet create counter.
func (p *CloudWatchPublisher) PublishFleetCreate(ctx context.Context, capacity, result string) error {
	return p.putCounter(ctx, "FleetCreate", dims("Capacity", capacity, "Result", result))
}

// PublishFleetCreateSeconds publishes the fleet create latency as a StatisticSet.
func (p *CloudWatchPublisher) PublishFleetCreateSeconds(ctx context.Context, capacity string, seconds float64) error {
	return p.putStatistic(ctx, "FleetCreateSeconds", seconds, types.StandardUnitSeconds, dims(
		"Capacity", capacity,
	))
}

// PublishInstances publishes the instances gauge.
func (p *CloudWatchPublisher) PublishInstances(ctx context.Context, state, capacity, pool string, n int) error {
	return p.putGauge(ctx, "Instances", float64(n), types.StandardUnitCount, dims(
		"State", state, "Capacity", capacity, "Pool", pool,
	))
}

// PublishSpotInterruption publishes the spot interruption counter.
func (p *CloudWatchPublisher) PublishSpotInterruption(ctx context.Context, family string) error {
	return p.putCounter(ctx, "SpotInterruptions", dims("Family", family))
}

// PublishCircuitBreakerTrip publishes the circuit breaker trip counter.
func (p *CloudWatchPublisher) PublishCircuitBreakerTrip(ctx context.Context, instanceType string) error {
	return p.putCounter(ctx, "CircuitBreakerTrip", dims("InstanceType", instanceType))
}

// PublishCircuitBreakerOpen publishes the circuit breaker open gauge (0/1).
func (p *CloudWatchPublisher) PublishCircuitBreakerOpen(ctx context.Context, instanceType string, open bool) error {
	return p.putGauge(ctx, "CircuitBreakerOpen", boolToFloat(open), types.StandardUnitCount, dims(
		"InstanceType", instanceType,
	))
}

// --- Pools ---

// PublishPoolInstances publishes the pool instances gauge.
func (p *CloudWatchPublisher) PublishPoolInstances(ctx context.Context, pool, state string, n int) error {
	return p.putGauge(ctx, "PoolInstances", float64(n), types.StandardUnitCount, dims(
		"PoolName", pool, "State", state,
	))
}

// PublishPoolDesired publishes the desired pool instances gauge.
func (p *CloudWatchPublisher) PublishPoolDesired(ctx context.Context, pool, kind string, n int) error {
	return p.putGauge(ctx, "PoolDesired", float64(n), types.StandardUnitCount, dims(
		"PoolName", pool, "Kind", kind,
	))
}

// PublishPoolAction publishes the pool action counter.
func (p *CloudWatchPublisher) PublishPoolAction(ctx context.Context, pool, action, reason string) error {
	return p.putCounter(ctx, "PoolActions", dims("PoolName", pool, "Action", action, "Reason", reason))
}

// PublishPoolReconcileSeconds publishes the pool reconcile latency as a StatisticSet.
func (p *CloudWatchPublisher) PublishPoolReconcileSeconds(ctx context.Context, seconds float64) error {
	return p.putStatistic(ctx, "PoolReconcileSeconds", seconds, types.StandardUnitSeconds, nil)
}

// --- Internals ---

// PublishMessageProcessingSeconds is intentionally a no-op on CloudWatch. Message
// processing latency is high-frequency (one sample per queue message); CloudWatch's
// un-batched PutMetricData would issue a synchronous API request per message. The
// histogram is carried by the Prometheus and Datadog backends instead.
func (*CloudWatchPublisher) PublishMessageProcessingSeconds(context.Context, string, string, float64) error {
	return nil
}

// PublishLockWaitSeconds is intentionally a no-op on CloudWatch. Lock wait time is
// high-frequency; the histogram is carried by Prometheus and Datadog instead.
func (*CloudWatchPublisher) PublishLockWaitSeconds(context.Context, string, float64) error {
	return nil
}

// PublishWorkerInflight publishes the worker in-flight gauge.
func (p *CloudWatchPublisher) PublishWorkerInflight(ctx context.Context, queue string, n int) error {
	return p.putGauge(ctx, "WorkerInflight", float64(n), types.StandardUnitCount, dims("Queue", queue))
}

// PublishQueueDepth publishes the queue depth gauge.
func (p *CloudWatchPublisher) PublishQueueDepth(ctx context.Context, queue string, depth float64) error {
	return p.putGauge(ctx, "QueueDepth", depth, types.StandardUnitCount, dims("Queue", queue))
}

// PublishQueueReceive publishes the queue receive counter.
func (p *CloudWatchPublisher) PublishQueueReceive(ctx context.Context, queue, result string) error {
	return p.putCounter(ctx, "QueueReceive", dims("Queue", queue, "Result", result))
}

// PublishAWSCallDuration is intentionally a no-op on the CloudWatch backend. AWS
// call latency is high-frequency (one sample per SDK call), and CloudWatch's
// un-batched PutMetricData would issue a synchronous API request per call —
// roughly doubling the orchestrator's AWS API volume and adding a round-trip to
// every instrumented call. The latency histogram is carried by the Prometheus
// and Datadog backends instead; CloudWatch retains only the low-frequency
// failure counter.
func (*CloudWatchPublisher) PublishAWSCallDuration(context.Context, string, string, float64) error {
	return nil
}

// PublishAWSCallFailure publishes an AWS SDK call failure counter.
func (p *CloudWatchPublisher) PublishAWSCallFailure(ctx context.Context, service, operation, result string) error {
	return p.putCounter(ctx, "AWSCallFailures", dims(
		"Service", service, "Operation", operation, "Result", result,
	))
}

// --- Cache / housekeeping / misc ---

// PublishCacheRequest publishes the cache request counter.
func (p *CloudWatchPublisher) PublishCacheRequest(ctx context.Context, result string) error {
	return p.putCounter(ctx, "CacheRequests", dims("Result", result))
}

// PublishCacheOperation publishes the cache operation counter.
func (p *CloudWatchPublisher) PublishCacheOperation(ctx context.Context, operation string) error {
	return p.putCounter(ctx, "CacheOperations", dims("Operation", operation))
}

// PublishCacheBytesStored publishes total bytes written to the cache.
func (p *CloudWatchPublisher) PublishCacheBytesStored(ctx context.Context, bytes int64) error {
	return p.putCounterValueUnit(ctx, "CacheBytesStored", float64(bytes), types.StandardUnitBytes, nil)
}

// PublishCacheError publishes the cache error counter.
func (p *CloudWatchPublisher) PublishCacheError(ctx context.Context, operation string) error {
	return p.putCounter(ctx, "CacheErrors", dims("Operation", operation))
}

// PublishCacheAuthRejected publishes the cache auth-rejection counter.
func (p *CloudWatchPublisher) PublishCacheAuthRejected(ctx context.Context, reason string) error {
	return p.putCounter(ctx, "CacheAuthRejected", dims("Reason", reason))
}

// PublishHousekeepingAction publishes the housekeeping action counter.
func (p *CloudWatchPublisher) PublishHousekeepingAction(ctx context.Context, action string, count int) error {
	return p.putCounterValue(ctx, "HousekeepingActions", float64(count), dims("Action", action))
}

// PublishSchedulingFailure publishes the scheduling failure counter.
func (p *CloudWatchPublisher) PublishSchedulingFailure(ctx context.Context, taskType string) error {
	return p.putCounter(ctx, "SchedulingFailure", dims("TaskType", taskType))
}

// PublishMessageDeletionFailure publishes the message deletion failure counter.
func (p *CloudWatchPublisher) PublishMessageDeletionFailure(ctx context.Context, queue string) error {
	return p.putCounter(ctx, "MessageDeletionFailures", dims("Queue", queue))
}

// PublishServiceCheck is a no-op for CloudWatch (Datadog-specific feature).
func (p *CloudWatchPublisher) PublishServiceCheck(_ context.Context, _ string, _ int, _ string) error { //nolint:revive
	return nil
}

// PublishEvent is a no-op for CloudWatch (Datadog-specific feature).
func (p *CloudWatchPublisher) PublishEvent(_ context.Context, _, _, _ string, _ []string) error { //nolint:revive
	return nil
}

// --- Cost ---

// PublishInstanceHours publishes the instance hours counter.
func (p *CloudWatchPublisher) PublishInstanceHours(ctx context.Context, capacity, family string, hours float64) error {
	return p.putCounterValue(ctx, "InstanceHours", hours, dims("Capacity", capacity, "Family", family))
}

// PublishEstimatedCost publishes the estimated cost gauge.
func (p *CloudWatchPublisher) PublishEstimatedCost(ctx context.Context, usd float64) error {
	return p.putGauge(ctx, "EstimatedCost", usd, types.StandardUnitNone, nil)
}

// PublishRunnerExecutionSeconds records billable runner seconds by arch, vCPU,
// spot/on-demand, and result (a counter; sum -> per-(arch,vCPU) minutes).
func (p *CloudWatchPublisher) PublishRunnerExecutionSeconds(ctx context.Context, arch string, vcpu int, spot bool, result string, seconds float64) error {
	return p.putCounterValue(ctx, "RunnerExecutionSeconds", seconds, dims(
		"Arch", arch, "Vcpu", vcpuLabel(vcpu), "Spot", spotLabel(spot), "Result", result,
	))
}

// PublishRunnerToolCacheMiss counts an on-demand tool download (not pre-baked) by
// tool, version (major.minor), and arch.
func (p *CloudWatchPublisher) PublishRunnerToolCacheMiss(ctx context.Context, tool, version, arch string) error {
	return p.putCounter(ctx, "RunnerToolCacheMiss", dims("Tool", tool, "Version", version, "Arch", arch))
}

// PublishRunnerCacheInterception counts a job by cache-interceptor outcome (status).
func (p *CloudWatchPublisher) PublishRunnerCacheInterception(ctx context.Context, status string) error {
	return p.putCounter(ctx, "RunnerCacheInterception", dims("Status", status))
}

// --- helpers ---

// dims builds a CloudWatch dimension list from name/value pairs. Empty values are
// omitted so optional labels (e.g. an absent pool) do not create distinct series.
func dims(pairs ...string) []types.Dimension {
	out := make([]types.Dimension, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			continue
		}
		out = append(out, types.Dimension{
			Name:  aws.String(pairs[i]),
			Value: aws.String(pairs[i+1]),
		})
	}
	return out
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (p *CloudWatchPublisher) putCounter(ctx context.Context, name string, dimensions []types.Dimension) error {
	return p.putCounterValue(ctx, name, 1, dimensions)
}

func (p *CloudWatchPublisher) putCounterValue(ctx context.Context, name string, value float64, dimensions []types.Dimension) error {
	return p.putCounterValueUnit(ctx, name, value, types.StandardUnitCount, dimensions)
}

func (p *CloudWatchPublisher) putCounterValueUnit(ctx context.Context, name string, value float64, unit types.StandardUnit, dimensions []types.Dimension) error {
	_, err := p.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(p.namespace),
		MetricData: []types.MetricDatum{
			{
				MetricName: aws.String(name),
				Value:      aws.Float64(value),
				Unit:       unit,
				Timestamp:  aws.Time(time.Now()),
				Dimensions: dimensions,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to publish metric %s: %w", name, err)
	}
	return nil
}

func (p *CloudWatchPublisher) putGauge(ctx context.Context, name string, value float64, unit types.StandardUnit, dimensions []types.Dimension) error {
	_, err := p.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(p.namespace),
		MetricData: []types.MetricDatum{
			{
				MetricName: aws.String(name),
				StatisticValues: &types.StatisticSet{
					SampleCount: aws.Float64(1),
					Sum:         aws.Float64(value),
					Minimum:     aws.Float64(value),
					Maximum:     aws.Float64(value),
				},
				Unit:       unit,
				Timestamp:  aws.Time(time.Now()),
				Dimensions: dimensions,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to publish metric %s: %w", name, err)
	}
	return nil
}

// putStatistic emits a low-frequency latency observation as a single-sample
// StatisticSet. High-frequency latency metrics are no-oped above to avoid
// per-event PutMetricData.
func (p *CloudWatchPublisher) putStatistic(ctx context.Context, name string, value float64, unit types.StandardUnit, dimensions []types.Dimension) error {
	return p.putGauge(ctx, name, value, unit, dimensions)
}
