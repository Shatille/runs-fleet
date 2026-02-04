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

// NewCloudWatchPublisher creates a CloudWatch metrics publisher.
func NewCloudWatchPublisher(cfg aws.Config) *CloudWatchPublisher {
	return NewCloudWatchPublisherWithNamespace(cfg, "RunsFleet")
}

// NewCloudWatchPublisherWithNamespace creates a CloudWatch metrics publisher with custom namespace.
func NewCloudWatchPublisherWithNamespace(cfg aws.Config, namespace string) *CloudWatchPublisher {
	return &CloudWatchPublisher{
		client:    cloudwatch.NewFromConfig(cfg),
		namespace: namespace,
	}
}

// Close implements Publisher.Close. CloudWatch client doesn't require cleanup.
func (p *CloudWatchPublisher) Close() error {
	return nil
}

// PublishQueueDepth publishes queue depth metric.
func (p *CloudWatchPublisher) PublishQueueDepth(ctx context.Context, depth float64) error {
	return p.putGaugeMetric(ctx, "QueueDepth", depth, types.StandardUnitCount)
}

// PublishFleetSizeIncrement publishes fleet size increment metric.
func (p *CloudWatchPublisher) PublishFleetSizeIncrement(ctx context.Context) error {
	return p.putMetric(ctx, "FleetSizeIncrement", 1, types.StandardUnitCount)
}

// PublishFleetSizeDecrement publishes fleet size decrement metric.
func (p *CloudWatchPublisher) PublishFleetSizeDecrement(ctx context.Context) error {
	return p.putMetric(ctx, "FleetSizeDecrement", 1, types.StandardUnitCount)
}

// PublishJobDuration publishes job duration metric.
func (p *CloudWatchPublisher) PublishJobDuration(ctx context.Context, durationSeconds int) error {
	return p.putMetric(ctx, "JobDuration", float64(durationSeconds), types.StandardUnitSeconds)
}

// PublishJobSuccess publishes job success metric.
func (p *CloudWatchPublisher) PublishJobSuccess(ctx context.Context) error {
	return p.putMetric(ctx, "JobSuccess", 1, types.StandardUnitCount)
}

// PublishJobFailure publishes job failure metric.
func (p *CloudWatchPublisher) PublishJobFailure(ctx context.Context) error {
	return p.putMetric(ctx, "JobFailure", 1, types.StandardUnitCount)
}

// PublishJobQueued publishes job queued metric.
func (p *CloudWatchPublisher) PublishJobQueued(ctx context.Context) error {
	return p.putMetric(ctx, "JobQueued", 1, types.StandardUnitCount)
}

// PublishSpotInterruption publishes spot interruption metric.
func (p *CloudWatchPublisher) PublishSpotInterruption(ctx context.Context) error {
	return p.putMetric(ctx, "SpotInterruptions", 1, types.StandardUnitCount)
}

// PublishMessageDeletionFailure publishes message deletion failure metric.
func (p *CloudWatchPublisher) PublishMessageDeletionFailure(ctx context.Context) error {
	return p.putMetric(ctx, "MessageDeletionFailures", 1, types.StandardUnitCount)
}

// PublishCacheHit publishes cache hit metric.
func (p *CloudWatchPublisher) PublishCacheHit(ctx context.Context) error {
	return p.putMetric(ctx, "CacheHits", 1, types.StandardUnitCount)
}

// PublishCacheMiss publishes cache miss metric.
func (p *CloudWatchPublisher) PublishCacheMiss(ctx context.Context) error {
	return p.putMetric(ctx, "CacheMisses", 1, types.StandardUnitCount)
}

// PublishOrphanedInstancesTerminated publishes orphaned instances terminated metric.
func (p *CloudWatchPublisher) PublishOrphanedInstancesTerminated(ctx context.Context, count int) error {
	return p.putMetric(ctx, "OrphanedInstancesTerminated", float64(count), types.StandardUnitCount)
}

// PublishSSMParametersDeleted publishes SSM parameters deleted metric.
func (p *CloudWatchPublisher) PublishSSMParametersDeleted(ctx context.Context, count int) error {
	return p.putMetric(ctx, "SSMParametersDeleted", float64(count), types.StandardUnitCount)
}

// PublishJobRecordsArchived publishes job records archived metric.
func (p *CloudWatchPublisher) PublishJobRecordsArchived(ctx context.Context, count int) error {
	return p.putMetric(ctx, "JobRecordsArchived", float64(count), types.StandardUnitCount)
}

// PublishOrphanedJobsCleanedUp publishes orphaned jobs cleaned up metric.
func (p *CloudWatchPublisher) PublishOrphanedJobsCleanedUp(ctx context.Context, count int) error {
	return p.putMetric(ctx, "OrphanedJobsCleanedUp", float64(count), types.StandardUnitCount)
}

// PublishPoolUtilization publishes pool utilization metric with pool name dimension.
func (p *CloudWatchPublisher) PublishPoolUtilization(ctx context.Context, poolName string, utilization float64) error {
	_, err := p.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(p.namespace),
		MetricData: []types.MetricDatum{
			{
				MetricName: aws.String("PoolUtilization"),
				Value:      aws.Float64(utilization),
				Unit:       types.StandardUnitPercent,
				Timestamp:  aws.Time(time.Now()),
				Dimensions: []types.Dimension{
					{
						Name:  aws.String("PoolName"),
						Value: aws.String(poolName),
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to publish pool utilization metric for %s: %w", poolName, err)
	}
	return nil
}

// PublishSchedulingFailure publishes scheduling failure metric with task type dimension.
func (p *CloudWatchPublisher) PublishSchedulingFailure(ctx context.Context, taskType string) error {
	_, err := p.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(p.namespace),
		MetricData: []types.MetricDatum{
			{
				MetricName: aws.String("SchedulingFailure"),
				Value:      aws.Float64(1),
				Unit:       types.StandardUnitCount,
				Timestamp:  aws.Time(time.Now()),
				Dimensions: []types.Dimension{
					{
						Name:  aws.String("TaskType"),
						Value: aws.String(taskType),
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to publish scheduling failure metric for %s: %w", taskType, err)
	}
	return nil
}

// PublishCircuitBreakerTriggered publishes circuit breaker triggered metric.
func (p *CloudWatchPublisher) PublishCircuitBreakerTriggered(ctx context.Context, instanceType string) error {
	_, err := p.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(p.namespace),
		MetricData: []types.MetricDatum{
			{
				MetricName: aws.String("CircuitBreakerTriggered"),
				Value:      aws.Float64(1),
				Unit:       types.StandardUnitCount,
				Timestamp:  aws.Time(time.Now()),
				Dimensions: []types.Dimension{
					{
						Name:  aws.String("InstanceType"),
						Value: aws.String(instanceType),
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to publish circuit breaker triggered metric for %s: %w", instanceType, err)
	}
	return nil
}

// PublishJobClaimFailure publishes job claim failure metric.
func (p *CloudWatchPublisher) PublishJobClaimFailure(ctx context.Context) error {
	return p.putMetric(ctx, "JobClaimFailures", 1, types.StandardUnitCount)
}

// PublishWarmPoolHit publishes warm pool hit metric.
func (p *CloudWatchPublisher) PublishWarmPoolHit(ctx context.Context) error {
	return p.putMetric(ctx, "WarmPoolHits", 1, types.StandardUnitCount)
}

// PublishFleetSize publishes current absolute fleet size.
func (p *CloudWatchPublisher) PublishFleetSize(ctx context.Context, size int) error {
	return p.putGaugeMetric(ctx, "FleetSize", float64(size), types.StandardUnitCount)
}

// PublishServiceCheck is a no-op for CloudWatch (Datadog-specific feature).
func (p *CloudWatchPublisher) PublishServiceCheck(_ context.Context, _ string, _ int, _ string) error { //nolint:revive
	return nil
}

// PublishEvent is a no-op for CloudWatch (Datadog-specific feature).
func (p *CloudWatchPublisher) PublishEvent(_ context.Context, _, _, _ string, _ []string) error { //nolint:revive
	return nil
}

func (p *CloudWatchPublisher) putMetric(ctx context.Context, name string, value float64, unit types.StandardUnit) error {
	_, err := p.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(p.namespace),
		MetricData: []types.MetricDatum{
			{
				MetricName: aws.String(name),
				Value:      aws.Float64(value),
				Unit:       unit,
				Timestamp:  aws.Time(time.Now()),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to publish metric %s: %w", name, err)
	}
	return nil
}

func (p *CloudWatchPublisher) putGaugeMetric(ctx context.Context, name string, value float64, unit types.StandardUnit) error {
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
				Unit:      unit,
				Timestamp: aws.Time(time.Now()),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to publish metric %s: %w", name, err)
	}
	return nil
}
